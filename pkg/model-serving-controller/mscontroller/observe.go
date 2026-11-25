/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mscontroller

import (
	"fmt"
	"sort"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// groupState is what a round knows about one ServingGroup of the ModelServing.
type groupState struct {
	object  *workloadv1alpha1.ServingGroup
	ordinal int
	// members are the member Pods of the group in the Pod cache.
	members []*corev1.Pod
	// revision is the revision the group runs: spec.revision, or status.revision
	// under RoleRollingUpdate.
	revision string
	// comparison is that revision compared with the target.
	comparison workload.Comparison
	// unit is the group as the plan sees it.
	unit grouplevel.Group
	// fallback, when not empty, says why the group is recreated although the
	// ModelServing is updated in place.
	fallback string
}

func (g groupState) condition(conditionType workloadv1alpha1.ServingGroupConditionType) bool {
	return meta.IsStatusConditionTrue(g.object.Status.Conditions, string(conditionType))
}

// observed are the ServingGroups a round is decided on: the ones the
// ModelServing controls, and the ordinals whose name another object holds.
// They come from the cache, or from the API server.
type observed struct {
	own     []*workloadv1alpha1.ServingGroup
	foreign []int
}

// split sorts the ServingGroups that have a name of a group of ms into the
// ones ms controls and the ordinals someone else holds.
func split(ms *workloadv1alpha1.ModelServing, named []*workloadv1alpha1.ServingGroup) observed {
	var out observed
	for _, group := range named {
		prefix, ordinal, ok := objcache.OrdinalName(group.Name)
		if !ok || prefix != ms.Name || group.Namespace != ms.Namespace {
			continue
		}
		if owner := metav1.GetControllerOfNoCopy(group); owner != nil && owner.UID == ms.UID {
			out.own = append(out.own, group)
		} else {
			out.foreign = append(out.foreign, ordinal)
		}
	}
	sort.Slice(out.own, func(a, b int) bool { return out.own[a].Name < out.own[b].Name })
	sort.Ints(out.foreign)
	return out
}

// rolloutSpec resolves the rollout settings of ms into the input of the plan.
func rolloutSpec(ms *workloadv1alpha1.ModelServing, target, baseline string) (grouplevel.Spec, error) {
	replicas := 1
	if ms.Spec.Replicas != nil {
		replicas = int(*ms.Spec.Replicas)
	}
	spec := grouplevel.Spec{Replicas: replicas, Target: target, Baseline: baseline, Mode: grouplevel.ModeServingGroup}
	strategy := ms.Spec.RolloutStrategy
	if strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate {
		spec.Mode = grouplevel.ModeRole
		for _, role := range ms.Spec.Template.Roles {
			partition, err := scaled(role.Partition, int(workload.RoleReplicas(role)), true)
			if err != nil {
				return spec, fmt.Errorf("partition of Role %s: %w", role.Name, err)
			}
			spec.RolePartitioned = spec.RolePartitioned || partition > 0
		}
		// Under RoleRollingUpdate the group level only recovers, scales and
		// fills: a group is never deleted to be replaced, so no budget applies.
		spec.MaxUnavailable = replicas
		return spec, nil
	}

	var err error
	if spec.MaxUnavailable, err = utils.GetMaxUnavailable(withReplicas(ms, replicas)); err != nil {
		return spec, fmt.Errorf("maxUnavailable: %w", err)
	}
	if spec.MaxSurge, err = utils.GetMaxSurge(withReplicas(ms, replicas)); err != nil {
		return spec, fmt.Errorf("maxSurge: %w", err)
	}
	if strategy != nil && strategy.RollingUpdateConfiguration != nil {
		if spec.Partition, err = scaled(strategy.RollingUpdateConfiguration.Partition, replicas, true); err != nil {
			return spec, fmt.Errorf("partition: %w", err)
		}
	}
	return spec, nil
}

// withReplicas returns ms with spec.replicas set, for helpers that read it
// without a default.
func withReplicas(ms *workloadv1alpha1.ModelServing, replicas int) *workloadv1alpha1.ModelServing {
	if ms.Spec.Replicas != nil {
		return ms
	}
	copied := ms.DeepCopy()
	value := int32(replicas)
	copied.Spec.Replicas = &value
	return copied
}

func scaled(value *intstr.IntOrString, total int, roundUp bool) (int, error) {
	if value == nil {
		return 0, nil
	}
	return intstr.GetScaledValueFromIntOrPercent(value, total, roundUp)
}

// state describes one group the ModelServing controls.
func (c *Controller) state(ms *workloadv1alpha1.ModelServing, spec grouplevel.Spec, history *workload.History, group *workloadv1alpha1.ServingGroup, ordinal int) (groupState, bool) {
	parent := servinggroup.ParentOf(ms)
	g := groupState{object: group, ordinal: ordinal}
	g.members = servinggroup.Members(c.caches.PodsOfGroup(group), group.Name, group.UID, parent)

	g.revision = group.Spec.Revision
	if spec.Mode == grouplevel.ModeRole {
		g.revision = group.Status.Revision
	}
	g.comparison = history.Compare(g.revision, spec.Target)

	unit := grouplevel.Group{
		Ordinal:          ordinal,
		UID:              group.UID,
		Revision:         g.revision,
		Surge:            group.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup,
		Terminating:      group.DeletionTimestamp != nil,
		Failed:           g.condition(workloadv1alpha1.ServingGroupConditionFailed),
		Observed:         group.Status.ObservedGeneration >= group.Generation,
		Fresh:            group.Status.ObservedPods == servinggroup.Digest(g.members),
		Ready:            g.condition(workloadv1alpha1.ServingGroupConditionReady),
		Memberless:       group.Status.Pods == 0 && len(g.members) == 0,
		Empty:            servinggroup.Empty(group),
		RecreateRequired: g.condition(workloadv1alpha1.ServingGroupConditionRecreateRequired),
		PodsReady:        len(g.members) > 0,
	}
	// A revision that renders the same Pods as the target is the target, under
	// whatever identity it was recorded. One that cannot be compared is neither
	// the target nor something to replace.
	switch g.comparison {
	case workload.Equivalent:
		unit.Revision = spec.Target
	case workload.Unknown:
		unit.RevisionUnknown = true
	}
	// What the group says about itself counts only when it says it about the
	// Pods this controller sees.
	unit.Drained = g.condition(workloadv1alpha1.ServingGroupConditionDrained) && unit.Fresh
	awaiting := false
	for _, pod := range g.members {
		unit.PodsReady = unit.PodsReady && pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod)
		if cost, err := strconv.ParseInt(pod.Annotations[corev1.PodDeletionCost], 10, 64); err == nil {
			unit.DeletionCost += cost
		}
		awaiting = awaiting || servinggroup.MembershipOf(pod, group.Name, group.UID, parent).Adoptable()
	}
	g.unit = unit
	return g, awaiting
}

// view is what a round is planned on.
type view struct {
	spec   grouplevel.Spec
	groups []groupState
	input  grouplevel.Input
	// inPlace reports that the update method that takes effect is InPlace. It
	// is handed down to the groups, which need it under either rollout strategy.
	inPlace bool
	// legacy reports that Pods exist which the ModelServing itself controls:
	// workloads of a version without group objects that are not taken over yet.
	legacy bool
}

func (v *view) group(ordinal int) *groupState {
	for i := range v.groups {
		if v.groups[i].ordinal == ordinal {
			return &v.groups[i]
		}
	}
	return nil
}

// observe builds the view of a round from the groups that were read and from
// the Pod cache.
func (c *Controller) observe(ms *workloadv1alpha1.ModelServing, spec grouplevel.Spec, history *workload.History, groups observed) *view {
	inPlace := c.updatesInPlace(ms, spec)
	// The plan itself updates in place only where it replaces whole groups.
	spec.InPlace = inPlace && spec.Mode == grouplevel.ModeServingGroup
	v := &view{spec: spec, inPlace: inPlace}
	v.input = grouplevel.Input{Spec: spec, Foreign: groups.foreign, Orphans: map[int]grouplevel.Orphans{}}

	taken := map[int]bool{}
	fallbacks := map[string]string{}
	for _, group := range groups.own {
		_, ordinal, _ := objcache.OrdinalName(group.Name)
		state, awaiting := c.state(ms, spec, history, group, ordinal)
		if reason := inPlaceFallback(spec, history, state, fallbacks); reason != "" {
			// Only for the plan of this round: the condition of the group is
			// not written from here, and a later target is judged anew.
			state.fallback, state.unit.RecreateRequired = reason, true
		}
		v.groups = append(v.groups, state)
		v.input.Groups = append(v.input.Groups, state.unit)
		taken[ordinal] = true
		if awaiting {
			// The group occupies its place and is no candidate until its Pods are adopted.
			v.input.Orphans[ordinal] = grouplevel.Orphans{}
		}
	}
	sort.Slice(v.groups, func(a, b int) bool { return v.groups[a].ordinal < v.groups[b].ordinal })

	// Member Pods without a group object: an ordinal that carries them gets its
	// object back before anything else is decided about it.
	parent := servinggroup.ParentOf(ms)
	type found struct {
		revisions map[string]bool
		// updating holds the revisions that Pods are being updated to in place.
		updating map[string]bool
		marked   bool
	}
	ownerless := map[int]*found{}
	for _, pod := range c.caches.UnadoptedPodsOf(ms.Namespace, ms.Name) {
		groupName := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		prefix, ordinal, ok := objcache.OrdinalName(groupName)
		if !ok || prefix != ms.Name {
			continue
		}
		membership := servinggroup.MembershipOf(pod, groupName, "", parent)
		if !membership.Adoptable() {
			continue
		}
		v.legacy = v.legacy || membership == servinggroup.Legacy
		if taken[ordinal] {
			continue
		}
		if ownerless[ordinal] == nil {
			ownerless[ordinal] = &found{revisions: map[string]bool{}, updating: map[string]bool{}}
		}
		ownerless[ordinal].revisions[utils.ObjectRevision(pod)] = true
		if state := inplace.StateOf(pod); state.InProgress() && state.Revision != "" {
			ownerless[ordinal].updating[state.Revision] = true
		}
		ownerless[ordinal].marked = ownerless[ordinal].marked ||
			pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup
	}
	for ordinal, pods := range ownerless {
		orphans := grouplevel.Orphans{Marked: pods.marked}
		// The Pods of a group that is being updated in place carry the label
		// of the revision they came from until every one of them runs the new
		// images. The group is at the revision they are updated to: with the
		// one of the labels it would look as if the update had been taken
		// back, and the group would be recreated.
		revisions := pods.revisions
		if len(pods.updating) > 0 {
			revisions = pods.updating
		}
		if len(revisions) == 1 {
			for revision := range revisions {
				orphans.Revision = revision
			}
		}
		v.input.Orphans[ordinal] = orphans
	}
	c.reportFallbacks(ms, v)
	return v
}
