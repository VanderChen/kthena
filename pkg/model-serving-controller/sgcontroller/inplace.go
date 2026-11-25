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

package sgcontroller

import (
	"context"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Reasons of the events that report in-place updates. They are recorded on
// the ModelServing, where they were recorded before groups were objects.
const (
	ReasonInPlaceUpdateStarted   = "InPlaceUpdateStarted"
	ReasonInPlaceUpdateCompleted = "InPlaceUpdateCompleted"
	ReasonInPlaceUpdateFallback  = "InPlaceUpdateFallback"
	ReasonInPlaceUpdateTimeout   = "InPlaceUpdateTimeout"
)

// unitUpdate is the in-place update of one unit in a round: the whole group
// when it is replaced as a whole, one Role instance when it is rolled Role by
// Role.
type unitUpdate struct {
	// keys are the instances of the unit.
	keys []servinggroup.InstanceKey
	step servinggroup.InPlaceStep
}

// planInPlace decides the in-place updates of the round.
//
// A group that is replaced as a whole is updated to spec.revision, which the
// ModelServing controller sets within the rollout budget. A group that is
// rolled Role by Role updates the instances its Role plan names. Either way
// the progress of an update is read from the Pods: nothing about it is kept
// in the process.
func (r *round) planInPlace(recoveryEnabled bool) {
	group := r.group
	steps := map[servinggroup.InstanceKey]*instanceStep{}
	// leaving holds the Pods this round deletes, or commits to delete.
	leaving := sets.New[types.UID]()
	for i := range r.steps {
		s := &r.steps[i]
		steps[s.key] = s
		for _, pod := range s.step.Delete {
			leaving.Insert(pod.UID)
		}
		if s.inst.Phase() == workloadv1alpha1.RoleInstanceDraining || s.step.Ledger != nil && s.step.Ledger.Phase == workloadv1alpha1.RoleInstanceDraining {
			for _, member := range s.inst.Members {
				leaving.Insert(member.Pod.UID)
			}
		}
	}
	// An instance is settled when it is complete and this round does nothing
	// about it.
	settled := func(s *instanceStep) bool {
		step := s.step
		return s.inst.Settled(s.want) && step.Ledger == nil && step.FailGroup == "" && len(step.Delete)+len(step.Create)+len(step.Adopt) == 0
	}

	var units []servinggroup.InPlaceUnit
	var keys [][]servinggroup.InstanceKey
	if roleMode(group) {
		chosen := make([]servinggroup.InstanceKey, 0, len(r.plan.InPlace))
		for key := range r.plan.InPlace {
			chosen = append(chosen, key)
		}
		sort.Slice(chosen, func(a, b int) bool {
			if chosen[a].Role != chosen[b].Role {
				return chosen[a].Role < chosen[b].Role
			}
			return chosen[a].Index < chosen[b].Index
		})
		for _, key := range chosen {
			s := steps[key]
			if s == nil {
				continue
			}
			units = append(units, servinggroup.InPlaceUnit{
				Role: key.Role, Instances: []servinggroup.Instance{s.inst}, Target: r.plan.InPlace[key],
				Allowed: true, Settled: settled(s), RecoveryEnabled: recoveryEnabled,
			})
			keys = append(keys, []servinggroup.InstanceKey{key})
		}
	} else if group.Spec.Revision != "" && !meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionRecreateRequired)) {
		// A group that has to be recreated is left as it is until it is replaced.
		unit := servinggroup.InPlaceUnit{
			Target: group.Spec.Revision, Allowed: servinggroup.UpdatesInPlace(group),
			Settled: len(r.steps) > 0, RecoveryEnabled: recoveryEnabled,
		}
		var all []servinggroup.InstanceKey
		for i := range r.steps {
			s := &r.steps[i]
			if s.want == nil && s.inst.Entry == nil && len(s.inst.Members) == 0 {
				continue
			}
			unit.Instances = append(unit.Instances, s.inst)
			unit.Settled = unit.Settled && settled(s)
			all = append(all, s.key)
		}
		units = append(units, unit)
		keys = append(keys, all)
	}

	for i, unit := range units {
		step := servinggroup.PlanInPlace(unit, r.history)
		if !step.Active {
			continue
		}
		if step.Obstacle != "" {
			if unit.Role != "" {
				// The Role plan asks for the same answer before it chooses an
				// instance, so this cannot be; the instance is left alone.
				continue
			}
			// The ModelServing controller replaces the group. Until then it
			// counts by what its Pods report.
			r.recreate = &servinggroup.ConditionChange{Reason: servinggroup.ReasonInPlaceUpdateNotPossible, Message: step.Obstacle}
			r.confirm = true
			if step.Fallback {
				r.fallbacks = append(r.fallbacks, fmt.Sprintf("ServingGroup %s is recreated instead of updated in place: %s", group.Name, step.Obstacle))
			}
			continue
		}
		patches := step.Patch[:0:0]
		for _, patch := range step.Patch {
			if !leaving.Has(patch.Pod.UID) {
				patches = append(patches, patch)
			}
		}
		step.Patch = patches
		if step.Waiting != "" {
			klog.V(4).InfoS("An in-place update waits", "servingGroup", klog.KObj(group), "revision", step.Revision, "for", step.Waiting)
		}
		if r.updating == nil {
			r.updating = map[servinggroup.InstanceKey]bool{}
		}
		for _, key := range keys[i] {
			r.updating[key] = true
		}
		// A patch rests on the group still asking for the revision and on the
		// Pods being what the cache shows.
		r.confirm = r.confirm || len(step.Patch)+len(step.Complete) > 0
		r.updates = append(r.updates, unitUpdate{keys: keys[i], step: step})
	}

	// An instance that is drained because it cannot be updated in place.
	for key, reason := range r.plan.Fallback {
		if s := steps[key]; s != nil && s.step.Ledger != nil && s.step.Ledger.Phase == workloadv1alpha1.RoleInstanceDraining {
			r.fallbacks = append(r.fallbacks, fmt.Sprintf("Role %s/%s in ServingGroup %s is recreated instead of updated in place: %s",
				key.Role, utils.GenerateRoleID(key.Role, key.Index), group.Name, reason))
		}
	}
	sort.Strings(r.fallbacks)
}

// wakeAfter returns when the next round is due even if nothing changes: when
// the fault of a member starts to count, or when an in-place update passes
// its progress deadline. Zero means that no round is due.
func (r *round) wakeAfter(now time.Time) time.Duration {
	at := r.retryAt
	if next := r.deadlines.Next; !next.IsZero() && (at.IsZero() || next.Before(at)) {
		at = next
	}
	if at.IsZero() {
		return 0
	}
	return max(at.Sub(now), time.Second)
}

// reportInPlace records the events about in-place updates that follow from
// the status write of the round: a fallback to recreation that was committed,
// and an update that ran past its progress deadline.
func (c *Controller) reportInPlace(r *round) {
	for _, message := range r.fallbacks {
		c.recorder.Event(r.parent, corev1.EventTypeNormal, ReasonInPlaceUpdateFallback, message)
	}

	// The deadline is reported once for an update, not in every round that
	// finds it passed. What was reported is remembered in the process: a new
	// process reports again.
	started := r.deadlines.TimedOut
	c.readyMu.Lock()
	reported, known := c.timeouts[r.group.UID]
	switch {
	case started.IsZero():
		delete(c.timeouts, r.group.UID)
	case !known || !reported.Equal(started):
		c.timeouts[r.group.UID] = started
	}
	c.readyMu.Unlock()
	if !started.IsZero() && !(known && reported.Equal(started)) {
		c.recorder.Eventf(r.parent, corev1.EventTypeWarning, ReasonInPlaceUpdateTimeout,
			"In-place update of ServingGroup %s exceeded progress deadline %s, applying recovery policy %s",
			r.group.Name, servinggroup.ProgressDeadline(r.group), r.policy)
	}
}

// updateInPlace issues the patches of the in-place updates of a round: the
// images of the Pods that are to run another revision, and the labels of the
// Pods of a unit that runs its revision as a whole. Every patch applies only
// to the Pod as it was read.
func (c *Controller) updateInPlace(ctx context.Context, r *round, now time.Time) error {
	for _, update := range r.updates {
		step := update.step
		started := false
		for _, patch := range step.Patch {
			patched, err := inplace.Start(ctx, c.kube, patch.Pod, patch.Images, step.Revision, now)
			if err != nil {
				return fmt.Errorf("update Pod %s in place: %w", patch.Pod.Name, err)
			}
			if patched {
				started = true
				klog.V(2).InfoS("Pod is updated in place", "pod", klog.KObj(patch.Pod), "revision", step.Revision)
			}
		}
		if started {
			c.recorder.Eventf(r.parent, corev1.EventTypeNormal, ReasonInPlaceUpdateStarted,
				"Updating images in place in ServingGroup %s to revision %s", r.group.Name, step.Revision)
		}

		for _, pod := range step.Complete {
			hash, err := r.history.RoleHash(step.Revision, pod.Labels[workloadv1alpha1.RoleLabelKey])
			if err != nil {
				return fmt.Errorf("finish the in-place update of Pod %s: %w", pod.Name, err)
			}
			if err := inplace.Complete(ctx, c.kube, pod, step.Revision, hash); err != nil {
				return fmt.Errorf("finish the in-place update of Pod %s: %w", pod.Name, err)
			}
		}
		if len(step.Complete) > 0 {
			c.recorder.Eventf(r.parent, corev1.EventTypeNormal, ReasonInPlaceUpdateCompleted,
				"In-place update of ServingGroup %s to revision %s completed", r.group.Name, step.Revision)
		}
	}
	return nil
}
