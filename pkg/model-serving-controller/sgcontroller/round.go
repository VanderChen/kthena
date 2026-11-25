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
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// observation is what one round is decided on: a group, the ModelServing that
// owns it and the Pods that may be members of the group. All three come from
// the cache, or all three from the API server.
type observation struct {
	group  *workloadv1alpha1.ServingGroup
	parent *workloadv1alpha1.ModelServing
	pods   []*corev1.Pod
}

// instanceStep is what a round does about one Role instance.
type instanceStep struct {
	key  servinggroup.InstanceKey
	inst servinggroup.Instance
	want *servinggroup.Want
	step servinggroup.Step
}

// round is the decision of one round. It is computed from an observation and
// from nothing else but the clock and the fault tracker, so a round that is
// decided again on a direct read of the same objects decides the same.
type round struct {
	observation
	history *workload.History
	members []*corev1.Pod
	// predecessors are the Pods of an earlier object of the group that still
	// exist. No member is created beside them.
	predecessors []*corev1.Pod
	instances    []servinggroup.Instance
	strays       []*corev1.Pod
	plan         servinggroup.RolePlan
	steps        []instanceStep
	// fail, when not nil, sets the Failed condition in this round.
	fail *servinggroup.ConditionChange
	// failure is an error of the decision itself. Nothing is created or deleted.
	// failureRetry, when positive, is when it is worth deciding again.
	failure      error
	failureRetry time.Duration
	// confirm reports that the round rests on Pods being absent, terminating
	// or faulty, and has to be decided again on a direct read before it acts.
	confirm bool
	// retryAt, when not zero, is when the fault of a member starts to count.
	retryAt time.Time

	// policy is the recovery policy that applies in this round.
	policy workloadv1alpha1.RecoveryPolicy
	// deadlines is what the progress deadline says about the in-place updates.
	deadlines servinggroup.InPlaceDeadlines
	// updates are the in-place updates this round makes progress on.
	updates []unitUpdate
	// updating holds the instances that are not ready because their in-place
	// update is not complete.
	updating map[servinggroup.InstanceKey]bool
	// recreate, when not nil, sets the RecreateRequired condition in this
	// round: the group cannot be updated in place.
	recreate *servinggroup.ConditionChange
	// fallbacks are the messages of the InPlaceUpdateFallback events of this round.
	fallbacks []string
}

// policyRank orders the recovery policies by the scope they recover.
var policyRank = map[workloadv1alpha1.RecoveryPolicy]int{
	workloadv1alpha1.NoneRestartPolicy:    0,
	workloadv1alpha1.RoleRecreate:         1,
	workloadv1alpha1.ServingGroupRecreate: 2,
}

// cautiousPolicy returns the policy with the smaller scope. The spec of a
// group follows the ModelServing with a delay; a change of the ModelServing
// that recovers less takes effect before it reaches the group.
func cautiousPolicy(group, parent workloadv1alpha1.RecoveryPolicy) workloadv1alpha1.RecoveryPolicy {
	rank := func(policy workloadv1alpha1.RecoveryPolicy) int {
		if r, ok := policyRank[policy]; ok {
			return r
		}
		// The API default.
		return policyRank[workloadv1alpha1.RoleRecreate]
	}
	if rank(parent) < rank(group) {
		return parent
	}
	return group
}

// roleMode reports whether the group is rolled Role by Role.
func roleMode(group *workloadv1alpha1.ServingGroup) bool {
	strategy := group.Spec.RolloutStrategy
	return strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate
}

// predecessors returns the Pods of an earlier object of the group: Pods under
// the label of the group that a ServingGroup of its name with another UID
// controls. The object was deleted without waiting for them. The garbage
// collector deletes them, or releases them to be adopted.
func predecessors(pods []*corev1.Pod, group *workloadv1alpha1.ServingGroup) []*corev1.Pod {
	var earlier []*corev1.Pod
	for _, pod := range pods {
		owner := metav1.GetControllerOfNoCopy(pod)
		if owner != nil && owner.Kind == workloadv1alpha1.ServingGroupKind.Kind && owner.Name == group.Name && owner.UID != group.UID {
			earlier = append(earlier, pod)
		}
	}
	return earlier
}

// decide computes the round for an observation.
func (c *Controller) decide(obs observation, now time.Time) *round {
	group, parent := obs.group, obs.parent
	r := &round{observation: obs}
	r.history = workload.NewHistory(workload.Base(parent, group), c.caches.ControllerRevision)

	recovery := servinggroup.RecoveryOf(group.Spec.RecoveryPolicy, group.Spec.RestartGracePeriodSeconds).
		Cautious(servinggroup.RecoveryOf(parent.Spec.RecoveryPolicy, parent.Spec.Template.RestartGracePeriodSeconds))
	identity := servinggroup.ParentOf(parent)
	r.members = servinggroup.Members(obs.pods, group.Name, group.UID, identity)
	r.predecessors = predecessors(obs.pods, group)
	// Holding back for a Pod of an earlier object rests on that Pod being
	// there. A deletion the cache never heard of would hold the group back for
	// good: the round is decided again on a direct read.
	r.confirm = len(r.predecessors) > 0
	// An in-place update that ran past its progress deadline is a fault of the
	// Pods that keep it from finishing.
	r.deadlines = servinggroup.CheckInPlaceDeadlines(group, r.members, recovery.Enabled, now)
	memberContext := servinggroup.MemberContext{Now: now, Recovery: recovery, Faults: c.faults, Overdue: r.deadlines.Overdue}
	r.instances, r.strays = servinggroup.Observe(r.members, group.Status.RoleInstances, group.Name, group.UID, identity, memberContext)
	for _, inst := range r.instances {
		for _, member := range inst.Members {
			if !member.RetryAt.IsZero() && (r.retryAt.IsZero() || member.RetryAt.Before(r.retryAt)) {
				r.retryAt = member.RetryAt
			}
		}
	}

	scope := servinggroup.Scope{
		GroupTerminating: group.DeletionTimestamp != nil,
		GroupFailed:      meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionFailed)),
		Policy:           cautiousPolicy(group.Spec.RecoveryPolicy, parent.Spec.RecoveryPolicy),
	}
	r.policy = scope.Policy

	// The plan is also what Ready is judged by, so it is made for a group that
	// has failed as well, although nothing of it is carried out there.
	plan, err := servinggroup.PlanRoles(group, r.instances, r.history)
	if err != nil {
		r.failure = err
		return r
	}
	r.plan = plan
	if scope.GroupTerminating {
		return r
	}
	// A group that is replaced as a whole and has lost every Pod has nothing
	// to recover Role by Role: it fails, and is drained in the same write.
	if !scope.GroupFailed && !roleMode(group) && !servinggroup.Empty(group) && servinggroup.AllPodsLost(r.instances, r.strays) {
		r.fail = &servinggroup.ConditionChange{Reason: servinggroup.FailAllPodsLost, Message: "the group has no Pod left"}
		r.confirm = true
		return r
	}

	observed := map[servinggroup.InstanceKey]bool{}
	for _, inst := range r.instances {
		observed[inst.Key()] = true
		r.steps = append(r.steps, instanceStep{key: inst.Key(), inst: inst, want: r.plan.Wants[inst.Key()]})
	}
	for key, want := range r.plan.Wants {
		if !observed[key] {
			r.steps = append(r.steps, instanceStep{key: key, inst: servinggroup.Instance{Role: key.Role, Index: key.Index}, want: want})
		}
	}
	sort.Slice(r.steps, func(a, b int) bool {
		if r.steps[a].key.Role != r.steps[b].key.Role {
			return r.steps[a].key.Role < r.steps[b].key.Role
		}
		return r.steps[a].key.Index < r.steps[b].key.Index
	})

	// While Roles are rolled together, whether an instance may be created
	// depends on what the other instances are: a creation rests on a direct
	// read then, like a deletion does.
	coordinating := r.plan.Coordination != nil && r.plan.Coordination.InProgress
	for i := range r.steps {
		s := &r.steps[i]
		s.step = servinggroup.Transition(s.inst, s.want, scope)
		// An adoption rests on a direct read as well: the cache may still show
		// a group that was deleted and created again under the same name, and
		// a Pod adopted by the object that is gone would be garbage collected.
		r.confirm = r.confirm || s.step.Confirm || len(s.step.Adopt) > 0 || coordinating && len(s.step.Create) > 0
		if s.step.FailGroup != "" && r.fail == nil {
			r.fail = &servinggroup.ConditionChange{
				Reason:  s.step.FailGroup,
				Message: fmt.Sprintf("Role instance %s-%d: %s", s.key.Role, s.key.Index, s.step.FailGroup),
			}
		}
	}
	if r.fail != nil {
		// The group is replaced as a whole. This round only records that; what is
		// deleted first is decided by the next one, on the Failed condition.
		for i := range r.steps {
			r.steps[i].step = servinggroup.Step{Rule: r.steps[i].step.Rule, Adopt: r.steps[i].step.Adopt}
		}
		return r
	}
	if !scope.GroupFailed {
		r.planInPlace(recovery.Enabled)
	}
	return r
}
