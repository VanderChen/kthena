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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// errTargetUnknown reports that the revision of the template of a
// ModelServing could not be built or recorded: the round has no target.
var errTargetUnknown = errors.New("the target revision is unknown")

// ReasonRevisionUnresolved is the reason of the event that says that the
// revision of the template of a ModelServing cannot be recorded.
const ReasonRevisionUnresolved = "RevisionUnresolved"

// withoutTarget ends a round whose plan failed. When it failed because the
// round has no target, what needs no target is carried out, a decrease of
// replica counts, and the failure is returned together with the error of
// doing so. Any other failure is returned as it is.
//
// The result is never nil: the round is due again, with a growing delay,
// until the revision can be recorded.
func (c *Controller) withoutTarget(ctx context.Context, ms *workloadv1alpha1.ModelServing, failure error) error {
	if !errors.Is(failure, errTargetUnknown) {
		return failure
	}
	c.reportUnresolved(ms, failure)
	return errors.Join(failure, c.scaleIn(ctx, ms))
}

// reportUnresolved records, on the ModelServing, that the revision of its
// template cannot be recorded. It is recorded once for a generation, not in
// every round that finds it so. What was reported is remembered in the
// process and nowhere else: a new process reports it again.
func (c *Controller) reportUnresolved(ms *workloadv1alpha1.ModelServing, failure error) {
	c.mu.Lock()
	reported, known := c.unresolved[ms.UID]
	// A cache that is behind shows a generation that was reported before.
	again := known && reported >= ms.Generation
	if !again {
		c.unresolved[ms.UID] = ms.Generation
	}
	c.mu.Unlock()
	if again {
		return
	}
	c.recorder.Eventf(ms, corev1.EventTypeWarning, ReasonRevisionUnresolved,
		"The revision of the template cannot be recorded; until it can, nothing is created or replaced and only decreases of replica counts are carried out: %v", failure)
}

// scaleInPlan is the decision of a round without a target. The zero value
// decides nothing.
type scaleInPlan struct {
	// round is the round as it was planned with the stand-in for the target.
	round *round
	// deletions are the writes of that plan that are issued: the ones that
	// take the number of groups down.
	deletions []grouplevel.Action
	// lowered are the groups that stay and whose Role counts go down, as they
	// are to be written.
	lowered []*workloadv1alpha1.ServingGroup
	// failures say why something that was asked for is not decided.
	failures []error
}

func (p scaleInPlan) writes() bool {
	return len(p.deletions) > 0 || len(p.lowered) > 0
}

// scaleIn is the round of a ModelServing that has no target: the revision of
// its template could not be built or recorded. Nothing may be created from a
// revision that is not recorded, and nothing may be replaced for it. A
// decrease of a replica count needs no target, so that much is carried out:
// groups above the declared number are deleted, and the groups that stay are
// told the lower count of a Role. The status is not written, for the
// generation was not observed in full.
func (c *Controller) scaleIn(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	plan := c.planScaleIn(ms, split(ms, c.caches.GroupsNamedAfter(ms.Namespace, ms.Name)))
	if !plan.writes() {
		return errors.Join(plan.failures...)
	}
	// As in any round, nothing is deleted or changed on the word of the
	// cache: the ModelServing and its groups are read from the API server and
	// the round is decided again on what was read.
	direct, groups, ok, err := c.readDirect(ctx, ms)
	if err != nil || !ok {
		return err
	}
	plan = c.planScaleIn(direct, groups)

	failures := plan.failures
	for _, action := range plan.deletions {
		if err := c.deleteGroup(ctx, plan.round, action); err != nil {
			// The deletions after it were chosen on the assumption that this
			// one succeeded.
			failures = append(failures, fmt.Errorf("%s ServingGroup %s: %w", action.Kind, utils.GenerateServingGroupName(direct.Name, action.Ordinal), err))
			break
		}
	}
	for _, group := range plan.lowered {
		// The write carries the resourceVersion that was read.
		if _, err := c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).Update(ctx, group, metav1.UpdateOptions{}); err != nil {
			failures = append(failures, fmt.Errorf("lower the Role counts of ServingGroup %s: %w", group.Name, err))
		}
	}
	return errors.Join(failures...)
}

// planScaleIn decides the round of a ModelServing without a target. It
// writes nothing: no revision is recorded, and what is recorded with one, the
// replica counts of the Roles of a coordinated rollout, is left as it is.
//
// The round is planned like any other, on the spec of the ModelServing as it
// is now, with the last target that was recorded standing in for the one that
// is missing. Every group was created from that target, or from the baseline
// of the rollout to it, or from a target before it. Of that plan only what
// takes replica counts down is kept.
func (c *Controller) planScaleIn(ms *workloadv1alpha1.ModelServing, groups observed) scaleInPlan {
	// Whether workloads of an earlier version are taken over, and when that
	// is done, is decided by a round that has a target. Until one has said
	// that it is done, or while workloads are handed back, nothing is scaled.
	if ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] != workloadv1alpha1.ServingGroupAdoptionDone {
		return scaleInPlan{}
	}
	// A ModelServing that never had a target, or whose last target cannot be
	// read, has no stand-in. Its groups are left to a round that has a target.
	standIn := ms.Status.UpdateRevision
	history := workload.NewHistory(ms, c.caches.ControllerRevision)
	if _, err := history.At(standIn); err != nil {
		return scaleInPlan{}
	}
	r := &round{ms: ms, target: standIn, history: history}
	r.baseline = baselineRevision(ms, groups.own, standIn)
	spec, err := rolloutSpec(ms, standIn, r.baseline)
	if err == nil && spec.MaxSurge < 0 {
		err = errors.New("maxSurge: must not be negative")
	}
	if err != nil {
		// Settings that cannot be resolved give no bound to scale in to.
		return scaleInPlan{failures: []error{err}}
	}
	r.view = c.observe(ms, spec, history, groups)

	plan := scaleInPlan{round: r, deletions: scaleInOnly(r.view, grouplevel.Plan(r.view.input))}
	plan.lowered, plan.failures = c.lowerRoles(r, plan.deletions)
	return plan
}

// scaleInOnly returns, of the writes of a plan, the ones a round without a
// target issues: deletions that take the number of groups down, in the order
// of the plan. These are the groups above the declared number, chosen as in
// any scale-in, and after them the groups that carry the surge mark.
//
// The plan was made for the stand-in. What it creates, promotes, retargets
// or replaces rests on that target and is dropped. So is the deletion of a
// group that failed, and of a group without members that is moved to a lower
// ordinal: the plan fills the place of either by a creation, and none is
// issued here.
//
// The surge of a rollout by recreation is kept up to its allowance, whether
// it exists yet or not: without the target nothing proves that it is not
// needed any more. Groups are therefore deleted only while more than replicas
// plus maxSurge of them are neither terminating nor failed. A ModelServing
// that is updated in place, or scaled to zero, goes down to replicas.
func scaleInOnly(v *view, actions []grouplevel.Action) []grouplevel.Action {
	bound := v.spec.Replicas
	if bound > 0 && !v.spec.InPlace {
		bound += v.spec.MaxSurge
	}
	surplus := -bound
	for _, g := range v.groups {
		if !g.unit.Terminating && !g.unit.Failed {
			surplus++
		}
	}
	var kept []grouplevel.Action
	for _, action := range actions {
		if len(kept) >= surplus {
			break
		}
		// Both are reasons of a deletion and of nothing else.
		if action.Reason == grouplevel.ReasonScaleDown || action.Reason == grouplevel.ReasonReclaimSurge {
			kept = append(kept, action)
		}
	}
	return kept
}

// lowerRoles returns the groups that carry a higher count for a Role than the
// template declares now, as they are to be written: with the count of such a
// Role lowered to the declared one. The group then removes the instances
// above it by its own rules. A count is never raised, a Role is neither added
// nor removed, and nothing else of the template is handed down: all of that
// belongs to the revision that is not recorded.
//
// A group below the partition is lowered like any other, as it is when the
// settings are handed down: the partition holds a group at its revision, and
// a count is no part of a revision.
//
// Some groups are left as they are. One that this round deletes or that is
// being deleted. One whose number of Pods cannot be told because its
// revision cannot be read; the round says so. And every group of a
// ModelServing whose Roles are rolled together: such a rollout measures a
// Role against the count recorded with a revision, which this round does not
// record.
func (c *Controller) lowerRoles(r *round, deletions []grouplevel.Action) (lowered []*workloadv1alpha1.ServingGroup, failures []error) {
	if roleCoordination(r.ms) != nil {
		return nil, nil
	}
	declared := map[string]int32{}
	for _, role := range r.ms.Spec.Template.Roles {
		declared[role.Name] = workload.RoleReplicas(role)
	}
	deleted := map[int]bool{}
	for _, action := range deletions {
		deleted[action.Ordinal] = true
	}
	for i := range r.view.groups {
		g := &r.view.groups[i]
		if g.unit.Terminating || deleted[g.ordinal] {
			continue
		}
		var updated *workloadv1alpha1.ServingGroup
		for j, role := range g.object.Spec.Roles {
			replicas, ok := declared[role.Name]
			if !ok || replicas >= role.Replicas {
				continue
			}
			if updated == nil {
				updated = g.object.DeepCopy()
			}
			updated.Spec.Roles[j].Replicas = replicas
		}
		if updated == nil {
			continue
		}
		// The number of Pods of the complete group follows the counts.
		expected, err := c.expectedPods(r.ms, updated)
		if err != nil {
			failures = append(failures, fmt.Errorf("lower the Role counts of ServingGroup %s: %w", updated.Name, err))
			continue
		}
		updated.Spec.ExpectedPods = expected
		// As when settings are handed down: the spec says which generation it
		// was derived from, so that a controller with a stale view does not
		// raise the counts again.
		updated.Spec.SourceGeneration = r.ms.Generation
		lowered = append(lowered, updated)
	}
	return lowered, failures
}

// expectedPods returns the number of Pods of group when it is complete, for
// the Role counts its spec carries. The layout is the one the group reports
// by: the revision it runs as a whole, or its target when its Roles are
// rolled one by one and it has no revision of its own.
func (c *Controller) expectedPods(ms *workloadv1alpha1.ModelServing, group *workloadv1alpha1.ServingGroup) (int32, error) {
	layout := group.Spec.Revision
	if layout == "" {
		layout = group.Spec.TargetRevision
	}
	at, err := workload.NewHistory(workload.Base(ms, group), c.caches.ControllerRevision).At(layout)
	if err != nil {
		return 0, err
	}
	return workload.ExpectedPods(at), nil
}
