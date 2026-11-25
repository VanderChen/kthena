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
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// ErrNotSettled reports that a round ended while the status of a group does
// not tell yet what the plan needs: it was not computed from the current spec
// or from the Pods the cache shows. Nothing is wrong; the round is due again
// shortly, with a growing delay, in case no event announces the change.
var ErrNotSettled = errors.New("a group has not reported on its current spec and Pods yet")

// Reasons of events.
const (
	ReasonOrdinalOccupied = "OrdinalOccupied"
	ReasonGroupCreated    = "ServingGroupCreated"
	ReasonGroupDeleted    = "ServingGroupDeleted"
)

// round is the decision of one round.
type round struct {
	ms         *workloadv1alpha1.ModelServing
	target     string
	baseline   string
	collisions *int32
	history    *workload.History
	view       *view
	actions    []grouplevel.Action
	// baselineReplicas holds, by Role name, the replica counts recorded with
	// the baseline revision; nil unless the Roles are rolled together.
	baselineReplicas map[string]int32
	// gated reports that workloads of an earlier version are still being
	// taken over: group objects are created for them and nothing else is rolled.
	gated bool
	// direct reports that the round was planned on a ModelServing read from
	// the API server. upToDate remembers whether the cached one is what the
	// API server has (see isCurrent).
	direct   bool
	upToDate *bool
}

// writes reports whether the round has a write to a group to issue.
func (r *round) writes() bool {
	for _, action := range r.actions {
		if action.Kind != grouplevel.ActionWait && action.Kind != grouplevel.ActionAdvanceBaseline {
			return true
		}
	}
	return false
}

// Reconcile runs one round for a ModelServing. The returned duration, when
// positive, is when the next round is due even if nothing changes.
func (c *Controller) Reconcile(ctx context.Context, namespace, name string) (time.Duration, error) {
	ms, err := c.caches.ModelServing(namespace, name)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if ms.DeletionTimestamp != nil {
		// The garbage collector deletes the groups.
		c.forget(ms.UID)
		return 0, nil
	}
	if c.rollingBack() {
		return c.rollBack(ctx, ms)
	}
	requeue, err := c.syncPlugins(ctx, ms)
	if err != nil {
		return 0, err
	}

	r, err := c.plan(ctx, ms, split(ms, c.caches.GroupsNamedAfter(namespace, name)))
	if err != nil {
		return 0, err
	}
	if r.writes() {
		// Before a group is created, deleted or changed, the ModelServing and
		// its groups are read from the API server and the round is planned again
		// on what was read.
		direct, groups, ok, err := c.readDirect(ctx, ms)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
		if r, err = c.plan(ctx, direct, groups); err != nil {
			return 0, err
		}
		r.direct = true
	}

	failure := c.execute(ctx, r)
	status, blockedAfter, err := c.writeStatus(ctx, r)
	if err != nil {
		return 0, errors.Join(failure, err)
	}
	if failure != nil {
		return 0, failure
	}
	c.cleanupRevisions(ctx, r, status)
	if blockedAfter > 0 && (requeue == 0 || blockedAfter < requeue) {
		requeue = blockedAfter
	}
	for _, g := range r.view.groups {
		if !g.unit.Terminating && !(g.unit.Observed && g.unit.Fresh) {
			return requeue, ErrNotSettled
		}
	}
	return requeue, nil
}

// syncPlugins calls the OnModelServingSync hooks. A hook that fails without
// asking to continue stops the round before anything is written.
func (c *Controller) syncPlugins(ctx context.Context, ms *workloadv1alpha1.ModelServing) (time.Duration, error) {
	if len(ms.Spec.Plugins) == 0 {
		return 0, nil
	}
	chain, err := plugins.NewChain(c.registry, ms.Spec.Plugins)
	if err != nil {
		return 0, fmt.Errorf("build the plugins: %w", err)
	}
	result := chain.OnModelServingSync(ctx, &plugins.HookRequest{ModelServing: ms, KubeClient: c.kube, ServiceLister: c.services})
	if result.Err != nil {
		if !result.Continue {
			return 0, fmt.Errorf("plugins of ModelServing %s: %w", ms.Name, result.Err)
		}
		klog.ErrorS(result.Err, "A plugin failed; the round continues", "modelServing", klog.KObj(ms))
	}
	return max(result.RequeueAfter, 0), nil
}

// readDirect reads the ModelServing and the ServingGroups that take the names
// of its groups from the API server. ok is false when the ModelServing is
// gone, was replaced or is being deleted.
func (c *Controller) readDirect(ctx context.Context, cached *workloadv1alpha1.ModelServing) (*workloadv1alpha1.ModelServing, observed, bool, error) {
	ms, err := c.kthena.WorkloadV1alpha1().ModelServings(cached.Namespace).Get(ctx, cached.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, observed{}, false, nil
	}
	if err != nil {
		return nil, observed{}, false, err
	}
	if ms.UID != cached.UID || ms.DeletionTimestamp != nil {
		return nil, observed{}, false, nil
	}
	// The groups of the ModelServing carry its name in a label. An object that
	// holds the name of a group without that label is known from the cache alone.
	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	list, err := c.kthena.WorkloadV1alpha1().ServingGroups(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, observed{}, false, err
	}
	named := make([]*workloadv1alpha1.ServingGroup, 0, len(list.Items))
	listed := map[string]bool{}
	for i := range list.Items {
		named = append(named, &list.Items[i])
		listed[list.Items[i].Name] = true
	}
	for _, group := range c.caches.GroupsNamedAfter(ms.Namespace, ms.Name) {
		if listed[group.Name] {
			continue
		}
		if owner := metav1.GetControllerOfNoCopy(group); owner == nil || owner.UID != ms.UID {
			named = append(named, group)
			continue
		}
		// A group of the ModelServing that the list does not have was deleted,
		// or lost its label. Left out, its ordinal would look free, the group
		// would be created again and again, and every other write of the round
		// would be dropped with the failed creation.
		direct, err := c.kthena.WorkloadV1alpha1().ServingGroups(ms.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, observed{}, false, err
		}
		named = append(named, direct)
	}
	return ms, split(ms, named), true, nil
}

// plan decides a round: the revisions, the view of the groups and the writes.
func (c *Controller) plan(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups observed) (*round, error) {
	target, collisions, recorded, err := c.targetRevision(ctx, ms)
	if err != nil {
		return nil, err
	}
	r := &round{ms: ms, target: target, collisions: collisions}
	r.baseline = baselineRevision(ms, groups.own, target)
	if ms.Status.CurrentRevision == "" && len(groups.own) == 0 {
		// Nothing records the baseline. Pods of an earlier version may tell.
		r.baseline = c.legacyBaseline(ms, target)
	}
	if recorded, r.baselineReplicas, err = c.coordinatedReplicas(ctx, ms, target, r.baseline, recorded); err != nil {
		return nil, err
	}
	// The ControllerRevision that was just recorded is read before the cache shows it.
	r.history = workload.NewHistory(ms, func(namespace, name string) (*appsv1.ControllerRevision, error) {
		if namespace == recorded.Namespace && name == recorded.Name {
			return recorded, nil
		}
		return c.caches.ControllerRevision(namespace, name)
	})
	spec, err := rolloutSpec(ms, target, r.baseline)
	if err != nil {
		return nil, err
	}
	r.view = c.observe(ms, spec, r.history, groups)
	r.actions = grouplevel.Plan(r.view.input)

	if r.gated, err = c.adoption(ctx, r); err != nil {
		return nil, err
	}
	if r.gated {
		// Only the objects of the groups that exist as Pods are created.
		kept := r.actions[:0:0]
		for _, action := range r.actions {
			if action.Kind == grouplevel.ActionCreate && action.Reason == grouplevel.ReasonRecover {
				kept = append(kept, action)
			}
		}
		r.actions = kept
	}
	return r, nil
}

// execute issues the writes of a round in the order the plan gives them, and
// then hands the settings of the ModelServing down to the groups. A write
// that fails ends the writes of the plan: each of them was computed on the
// assumption that the ones before it succeeded. The settings are handed down
// all the same.
func (c *Controller) execute(ctx context.Context, r *round) error {
	touched := map[int]bool{}
	var failure error
	for _, action := range r.actions {
		var err error
		switch action.Kind {
		case grouplevel.ActionCreate:
			err = c.createGroup(ctx, r, action)
		case grouplevel.ActionDelete:
			err = c.deleteGroup(ctx, r, action)
		case grouplevel.ActionPromote:
			err = c.promoteGroup(ctx, r, action)
		case grouplevel.ActionRetarget:
			err = c.retargetGroup(ctx, r, action)
		default:
			continue
		}
		touched[action.Ordinal] = true
		if err != nil {
			failure = fmt.Errorf("%s ServingGroup %s: %w", action.Kind, utils.GenerateServingGroupName(r.ms.Name, action.Ordinal), err)
			break
		}
	}
	if r.gated {
		return failure
	}
	return errors.Join(failure, c.handDown(ctx, r, touched))
}

func (c *Controller) createGroup(ctx context.Context, r *round, action grouplevel.Action) error {
	ms := r.ms
	group := &workloadv1alpha1.ServingGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace,
			Name:      utils.GenerateServingGroupName(ms.Name, action.Ordinal),
			Labels:    map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion:         workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:               workloadv1alpha1.ModelServingKind.Kind,
				Name:               ms.Name,
				UID:                ms.UID,
				Controller:         ptr.To(true),
				BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: workload.GroupSpec(ms, r.settings(action.Revision)),
	}
	if action.Surge {
		// The mark is set when the object is created and at no other time.
		group.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
	}
	_, err := c.kthena.WorkloadV1alpha1().ServingGroups(ms.Namespace).Create(ctx, group, metav1.CreateOptions{})
	if err == nil {
		c.recorder.Eventf(ms, corev1.EventTypeNormal, ReasonGroupCreated, "created ServingGroup %s at revision %s (%s)", group.Name, action.Revision, action.Reason)
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return err
	}
	// The name is taken. Only a direct read can tell by whom.
	existing, getErr := c.kthena.WorkloadV1alpha1().ServingGroups(ms.Namespace).Get(ctx, group.Name, metav1.GetOptions{})
	if getErr != nil {
		return getErr
	}
	if owner := metav1.GetControllerOfNoCopy(existing); owner != nil && owner.UID == ms.UID {
		return fmt.Errorf("the group exists already; the round is planned again on it")
	}
	c.recorder.Eventf(ms, corev1.EventTypeWarning, ReasonOrdinalOccupied, "the name %s is held by an object this ModelServing does not control", group.Name)
	return fmt.Errorf("the name is held by an object this ModelServing does not control")
}

// current returns the group an action is addressed to, as the round read it.
func (r *round) current(action grouplevel.Action) (*workloadv1alpha1.ServingGroup, error) {
	g := r.view.group(action.Ordinal)
	if g == nil || g.object.UID != action.UID {
		return nil, fmt.Errorf("the group the plan refers to is not the one that was read")
	}
	return g.object, nil
}

// deleteGroup deletes the group that was read, in the foreground: the object
// stays, and holds its ordinal, until its members are gone.
func (c *Controller) deleteGroup(ctx context.Context, r *round, action grouplevel.Action) error {
	group, err := r.current(action)
	if err != nil {
		return err
	}
	err = c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).Delete(ctx, group.Name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &group.UID, ResourceVersion: &group.ResourceVersion},
		PropagationPolicy: ptr.To(metav1.DeletePropagationForeground),
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err == nil {
		c.recorder.Eventf(r.ms, corev1.EventTypeNormal, ReasonGroupDeleted, "deleted ServingGroup %s (%s)", group.Name, action.Reason)
	}
	return err
}

// promoteGroup removes the surge mark: the group takes a regular place.
func (c *Controller) promoteGroup(ctx context.Context, r *round, action grouplevel.Action) error {
	group, err := r.current(action)
	if err != nil {
		return err
	}
	updated := group.DeepCopy()
	delete(updated.Annotations, workloadv1alpha1.SurgeAnnotationKey)
	_, err = c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

// retargetGroup changes the revision of a group: of one without a member, so
// that it is created from the revision its ordinal has now, or of one that is
// updated in place. The write carries the resourceVersion that was read. For
// a group without a member that makes it exclude the status write with which
// the group commits to creating its first Pod: whichever comes first, the
// other is rejected and decided again.
func (c *Controller) retargetGroup(ctx context.Context, r *round, action grouplevel.Action) error {
	group, err := r.current(action)
	if err != nil {
		return err
	}
	updated := group.DeepCopy()
	updated.Spec = workload.GroupSpec(r.ms, r.settings(action.Revision))
	_, err = c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
	if apierrors.IsConflict(err) && action.Reason == grouplevel.ReasonMemberless {
		// The group got its first member, or another status, in between. That
		// is the exclusion at work, not a failure: the next round plans on it.
		klog.V(2).InfoS("The revision of a group without a member was not changed: the group changed", "servingGroup", klog.KObj(group))
		return nil
	}
	return err
}

// handDown brings the spec of every group in line with the ModelServing: the
// settings that are not part of a revision, the target and the baseline. The
// revision a group runs as a whole is not changed here.
//
// A spec is not replaced by one derived from an older generation of the
// ModelServing: a controller with a stale view does not undo what a newer one
// handed down.
func (c *Controller) handDown(ctx context.Context, r *round, touched map[int]bool) error {
	var failures []error
	for i := range r.view.groups {
		g := &r.view.groups[i]
		group := g.object
		if g.unit.Terminating || touched[g.ordinal] {
			continue
		}
		// A spec derived from a later generation was handed down by a
		// controller that knew more. When this round knows the ModelServing as
		// the API server has it, there is no later generation: the number was
		// put there by something else, and the group would never be written again.
		if group.Spec.SourceGeneration > r.ms.Generation && !c.isCurrent(ctx, r) {
			continue
		}
		revision, ok := r.revisionOf(g)
		if !ok {
			continue
		}
		desired := workload.GroupSpec(r.ms, r.settings(revision))
		// The generation it was derived from changes with every change of the
		// ModelServing. It is only written along with something else.
		desired.SourceGeneration = group.Spec.SourceGeneration
		if apiequality.Semantic.DeepEqual(desired, group.Spec) {
			continue
		}
		desired.SourceGeneration = r.ms.Generation
		updated := group.DeepCopy()
		updated.Spec = desired
		if _, err := c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
			failures = append(failures, fmt.Errorf("hand the settings down to ServingGroup %s: %w", group.Name, err))
		}
	}
	return errors.Join(failures...)
}

// handedDown reports whether the spec of a group is the one this round hands
// down. The generation the spec was derived from changes with every change of
// the ModelServing; it is only written along with something else, and is not
// compared.
func (r *round) handedDown(group *workloadv1alpha1.ServingGroup) bool {
	desired := workload.GroupSpec(r.ms, r.settings(group.Spec.Revision))
	desired.SourceGeneration = group.Spec.SourceGeneration
	return apiequality.Semantic.DeepEqual(desired, group.Spec)
}

// isCurrent reports whether the ModelServing of the round is the one the API
// server has. It is asked there at most once in a round.
func (c *Controller) isCurrent(ctx context.Context, r *round) bool {
	if r.direct {
		return true
	}
	if r.upToDate == nil {
		direct, err := c.kthena.WorkloadV1alpha1().ModelServings(r.ms.Namespace).Get(ctx, r.ms.Name, metav1.GetOptions{})
		upToDate := err == nil && direct.UID == r.ms.UID && direct.Generation == r.ms.Generation
		r.upToDate = &upToDate
	}
	return *r.upToDate
}

// revisionOf returns the revision a group runs as a whole, for its spec. It
// is the one the spec has. Under RoleRollingUpdate the spec has none; a group
// that comes from there, because the rollout strategy was changed, gets the
// revision its Pods run. ok is false while that cannot be told: the spec of
// the group is then left as it is, for an empty revision would be one that no
// rollout ever replaces.
func (r *round) revisionOf(g *groupState) (revision string, ok bool) {
	group := g.object
	if group.Spec.Revision != "" || r.view.spec.Mode != grouplevel.ModeServingGroup {
		return group.Spec.Revision, true
	}
	switch {
	case !g.unit.Observed && group.Status.ObservedGeneration > 0, !g.unit.Fresh:
		// What the group reports is not about the Pods of now.
		return "", false
	case g.unit.Memberless:
		// Created from the revision its ordinal has, like a new group.
		if g.ordinal < r.view.spec.Partition && r.baseline != "" {
			return r.baseline, true
		}
		return r.target, true
	case group.Status.Revision != "":
		return group.Status.Revision, true
	}
	// Its Roles were being rolled when the strategy changed, or are held by a
	// Role partition, and its Pods run more than one revision. It is taken for
	// the baseline, and moved or replaced within the budget like any group
	// that is not at the target; until then it leaves its Pods as they are.
	// Taken for the target it would bring all of them to it at once.
	if r.history.Compare(r.baseline, r.target) != workload.Different {
		return "", false
	}
	return r.baseline, true
}

// writeStatus writes the status of the ModelServing when it changed, and
// returns it. The baseline advances here, in the round that found the rollout
// complete. The duration, when positive, is when a wait of the round has
// lasted long enough to be reported as a blocked rollout.
func (c *Controller) writeStatus(ctx context.Context, r *round) (workloadv1alpha1.ModelServingStatus, time.Duration, error) {
	baseline := r.baseline
	for _, action := range r.actions {
		if action.Kind == grouplevel.ActionAdvanceBaseline {
			baseline = action.Revision
		}
	}
	if baseline == "" {
		// The baseline cannot be told. What the status says about it is kept.
		baseline = r.ms.Status.CurrentRevision
	}
	status := computeStatus(statusInput{ms: r.ms, view: r.view, baseline: baseline, collisions: r.collisions, adopting: r.gated, handedDown: r.handedDown})
	blockedAfter := c.setBlocked(r, &status, c.now())
	if apiequality.Semantic.DeepEqual(status, r.ms.Status) {
		return status, blockedAfter, nil
	}
	updated := r.ms.DeepCopy()
	updated.Status = status
	if _, err := c.kthena.WorkloadV1alpha1().ModelServings(updated.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return status, 0, fmt.Errorf("write the status of ModelServing %s: %w", updated.Name, err)
	}
	c.reportCoordination(r.ms, status)
	return status, blockedAfter, nil
}
