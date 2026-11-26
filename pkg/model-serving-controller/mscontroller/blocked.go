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
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// blockedDeadline is how long a round may wait for the same thing before the
// ModelServing is reported as blocked. Waiting for a group to disappear, to
// become ready or to report is what every rollout and every scale-in does;
// it is worth a condition only when it lasts.
const blockedDeadline = 5 * time.Minute

// blockage says what a ModelServing that has work to do and issued no write
// in a round is waiting for.
type blockage struct {
	reason  string
	message string
	// since is when the wait began. The zero time means that it is reported at once.
	since time.Time
}

// blockedOn returns what the round waits for, or nil when it does not wait:
// the ModelServing is available, or the round made progress by a write.
//
// The reasons are tried in a fixed order and the first that applies is
// reported: a group that hangs in deletion hides what comes after it, and is
// what has to be looked at first anyway.
func (c *Controller) blockedOn(r *round, status workloadv1alpha1.ModelServingStatus, now time.Time) *blockage {
	// Since when a group has not reported is kept up in every round, also in
	// one that waits for nothing: a group that has reported in between must
	// not be found, the next time it is behind, with the moment of an earlier
	// time, and be reported as overdue at once.
	type unreported struct {
		since time.Time
		what  string
	}
	behind := map[types.UID]unreported{}
	seen := map[types.UID]bool{}
	for i := range r.view.groups {
		g := &r.view.groups[i]
		_, awaiting := r.view.input.Orphans[g.ordinal]
		if g.unit.Terminating || g.unit.Observed && g.unit.Fresh && !awaiting {
			continue
		}
		what := "has not reported on its current spec"
		switch {
		case awaiting:
			what = "has member Pods it has not adopted"
		case g.unit.Observed:
			what = "has not reported on the Pods the controller sees"
		}
		seen[g.object.UID] = true
		behind[g.object.UID] = unreported{since: c.unsettledSince(g.object.UID, now), what: what}
	}
	c.forgetSettled(r.ms.UID, seen)

	if meta.IsStatusConditionTrue(status.Conditions, string(workloadv1alpha1.ModelServingAvailable)) || r.gated {
		return nil
	}
	var baselineUnknown, occupied []int
	for _, action := range r.actions {
		switch {
		case action.Kind != grouplevel.ActionWait && action.Kind != grouplevel.ActionAdvanceBaseline:
			return nil
		case action.Reason == grouplevel.ReasonBaselineUnknown:
			baselineUnknown = append(baselineUnknown, action.Ordinal)
		case action.Reason == grouplevel.ReasonOrdinalOccupied:
			occupied = append(occupied, action.Ordinal)
		}
	}
	name := func(ordinal int) string { return utils.GenerateServingGroupName(r.ms.Name, ordinal) }
	if len(baselineUnknown) > 0 {
		return &blockage{reason: workloadv1alpha1.RolloutBlockedBaselineUnknown, message: fmt.Sprintf(
			"%s has to be created from the baseline revision, which cannot be told: status.currentRevision is empty and no ServingGroup carries a copy", name(baselineUnknown[0]))}
	}
	if len(occupied) > 0 {
		return &blockage{reason: workloadv1alpha1.RolloutBlockedOrdinalOccupied, message: fmt.Sprintf(
			"the name %s is held by an object that this ModelServing does not control", name(occupied[0]))}
	}

	conditionSince := func(g *groupState, conditionType workloadv1alpha1.ServingGroupConditionType) time.Time {
		if condition := meta.FindStatusCondition(g.object.Status.Conditions, string(conditionType)); condition != nil {
			return condition.LastTransitionTime.Time
		}
		return g.object.CreationTimestamp.Time
	}
	// earliest keeps, of the groups a reason applies to, the one that has
	// waited longest. A reason is only tried when none before it applied.
	var found *blockage
	earliest := func(reason string, since time.Time, format string, args ...any) {
		if found == nil || since.Before(found.since) {
			found = &blockage{reason: reason, message: fmt.Sprintf(format, args...), since: since}
		}
	}

	for i := range r.view.groups {
		g := &r.view.groups[i]
		switch {
		case g.unit.Terminating:
			earliest(workloadv1alpha1.RolloutBlockedWaitingForDeletion, g.object.DeletionTimestamp.Time,
				"ServingGroup %s is being deleted and holds its ordinal until its Pods are gone", g.object.Name)
		case g.unit.Failed && !g.unit.Drained:
			earliest(workloadv1alpha1.RolloutBlockedWaitingForDeletion, conditionSince(g, workloadv1alpha1.ServingGroupConditionFailed),
				"ServingGroup %s has failed and still has members with a fault or terminating Pods", g.object.Name)
		}
	}
	if found != nil {
		return found
	}
	for i := range r.view.groups {
		g := &r.view.groups[i]
		if late, ok := behind[g.object.UID]; ok {
			earliest(workloadv1alpha1.RolloutBlockedGroupNotObserved, late.since, "ServingGroup %s %s", g.object.Name, late.what)
		}
	}
	if found != nil {
		return found
	}
	// A regular group first. A surge group that is not ready may have waited
	// longer, but when a regular place is not ready either, that place is
	// what the surge group stands in for and what the rollout waits for.
	for _, surge := range []bool{false, true} {
		for i := range r.view.groups {
			g := &r.view.groups[i]
			if g.unit.Surge == surge && g.unit.Revision == r.view.spec.Target && !g.unit.Ready && !g.unit.Empty {
				// When the group says why its rounds fail, that is the reason
				// to pass on: Pods that cannot be created are not found by
				// looking at the Pods.
				why := ""
				if g.condition(workloadv1alpha1.ServingGroupConditionReconcileFailed) {
					why = ": " + meta.FindStatusCondition(g.object.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionReconcileFailed)).Message
				}
				earliest(workloadv1alpha1.RolloutBlockedTargetNotReady, conditionSince(g, workloadv1alpha1.ServingGroupConditionReady),
					"ServingGroup %s of the target revision is not ready%s", g.object.Name, why)
			}
		}
		if found != nil {
			return found
		}
	}
	for i := range r.view.groups {
		g := &r.view.groups[i]
		if !g.unit.Ready && !g.unit.Empty {
			earliest(workloadv1alpha1.RolloutBlockedBudgetExhausted, conditionSince(g, workloadv1alpha1.ServingGroupConditionReady),
				"ServingGroup %s is not ready and no further ServingGroup may become unavailable", g.object.Name)
		}
	}
	return found
}

// unsettledSince returns when this process first saw that a group had not
// reported. That moment is on no object; a new process starts counting again,
// which delays the report and nothing else.
func (c *Controller) unsettledSince(group types.UID, now time.Time) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if since, ok := c.unsettled[group]; ok {
		return since
	}
	c.unsettled[group] = now
	return now
}

// forgetSettled drops the groups of a ModelServing that have reported, and
// the ones that are gone.
func (c *Controller) forgetSettled(modelServing types.UID, unsettled map[types.UID]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for group := range c.unsettledOf[modelServing] {
		if !unsettled[group] {
			delete(c.unsettled, group)
		}
	}
	if len(unsettled) == 0 {
		delete(c.unsettledOf, modelServing)
	} else {
		c.unsettledOf[modelServing] = unsettled
	}
}

// setBlocked writes the RolloutBlocked condition into status and returns when
// the round is due again to report a wait that has not lasted long enough yet.
func (c *Controller) setBlocked(r *round, status *workloadv1alpha1.ModelServingStatus, now time.Time) time.Duration {
	condition := metav1.Condition{
		Type: string(workloadv1alpha1.ModelServingRolloutBlocked), Status: metav1.ConditionFalse,
		Reason: "NotBlocked", Message: "the rollout is not waiting beyond its deadline",
		ObservedGeneration: r.ms.Generation, LastTransitionTime: metav1.NewTime(now),
	}
	requeue := time.Duration(0)
	if blocked := c.blockedOn(r, *status, now); blocked != nil {
		if waited := now.Sub(blocked.since); blocked.since.IsZero() || waited >= blockedDeadline {
			condition.Status, condition.Reason, condition.Message = metav1.ConditionTrue, blocked.reason, blocked.message
		} else {
			requeue = blockedDeadline - waited
		}
	}
	if condition.Status == metav1.ConditionFalse && meta.FindStatusCondition(status.Conditions, condition.Type) == nil {
		// A ModelServing that was never blocked does not carry the condition.
		return requeue
	}
	meta.SetStatusCondition(&status.Conditions, condition)
	return requeue
}
