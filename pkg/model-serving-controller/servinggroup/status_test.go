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

package servinggroup

import (
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

const (
	condReady    = string(workloadv1alpha1.ServingGroupConditionReady)
	condFailed   = string(workloadv1alpha1.ServingGroupConditionFailed)
	condDrained  = string(workloadv1alpha1.ServingGroupConditionDrained)
	condRecreate = string(workloadv1alpha1.ServingGroupConditionRecreateRequired)
	condBlocked  = string(workloadv1alpha1.ServingGroupConditionDrainBlocked)
	condError    = string(workloadv1alpha1.ServingGroupConditionReconcileFailed)
	condCoord    = string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked)
)

// statusGroup returns a group that declares one prefill instance of an entry
// and two workers, with the given conditions already in its status.
func statusGroup(conditions ...metav1.Condition) *workloadv1alpha1.ServingGroup {
	return &workloadv1alpha1.ServingGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: testGroup, UID: testGroupUID, Generation: 4},
		Spec: workloadv1alpha1.ServingGroupSpec{
			Revision: "rev-a", ExpectedPods: 3,
			Roles: []workloadv1alpha1.ServingGroupRole{{Name: "prefill", Replicas: 1}},
		},
		Status: workloadv1alpha1.ServingGroupStatus{Conditions: conditions},
	}
}

func trueCondition(conditionType, reason string) metav1.Condition {
	return metav1.Condition{
		Type: conditionType, Status: metav1.ConditionTrue, Reason: reason, Message: "earlier",
		ObservedGeneration: 2, LastTransitionTime: metav1.NewTime(time.Unix(1700000500, 0)),
	}
}

// statusOf computes the status of a group from Pods the way a round does.
func statusOf(group *workloadv1alpha1.ServingGroup, ledger []workloadv1alpha1.RoleInstanceStatus, pods []*corev1.Pod, mutate ...func(*StatusInput)) workloadv1alpha1.ServingGroupStatus {
	ctx := MemberContext{Now: testNow, Recovery: Recovery{Enabled: true, Grace: time.Minute}, Faults: NewFaultTracker()}
	instances, _ := Observe(pods, group.Status.RoleInstances, group.Name, group.UID, Parent{Name: testMS, UID: testMSUID, AdoptionDone: true}, ctx)
	in := StatusInput{
		Group: group, Now: testNow, Members: pods, Instances: instances, Ledger: ledger,
		Wants: map[InstanceKey]*Want{{Role: "prefill", Index: 0}: {Workers: 2, LayoutKnown: true}},
	}
	for _, m := range mutate {
		m(&in)
	}
	return ComputeStatus(in)
}

func threePods(mutate ...func(*corev1.Pod)) []*corev1.Pod {
	return []*corev1.Pod{memberPod("prefill", 0, 0), memberPod("prefill", 0, 1, mutate...), memberPod("prefill", 0, 2)}
}

func ledgerOf(phase workloadv1alpha1.RoleInstancePhase, generation int64) []workloadv1alpha1.RoleInstanceStatus {
	return []workloadv1alpha1.RoleInstanceStatus{*entry(phase, generation)}
}

func conditionOf(t *testing.T, status workloadv1alpha1.ServingGroupStatus, conditionType string) (value, reason string) {
	t.Helper()
	condition := meta.FindStatusCondition(status.Conditions, conditionType)
	if condition == nil {
		return "", ""
	}
	if condition.ObservedGeneration != 4 {
		t.Errorf("condition %s was observed at generation %d, want 4", conditionType, condition.ObservedGeneration)
	}
	return string(condition.Status), condition.Reason
}

func TestComputeStatusReady(t *testing.T) {
	revision := func(rev string) func(*corev1.Pod) {
		return func(p *corev1.Pod) { p.Labels[workloadv1alpha1.RevisionLabelKey] = rev }
	}
	noReplicas := func(in *StatusInput) {
		in.Group.Spec.Roles[0].Replicas = 0
		in.Group.Spec.ExpectedPods = 0
		in.Wants = nil
	}
	unresolved := func(in *StatusInput) { in.LayoutUnknown, in.Wants = true, nil }

	tests := []struct {
		name   string
		ledger []workloadv1alpha1.RoleInstanceStatus
		pods   []*corev1.Pod
		mutate func(*StatusInput)

		ready, reason string
		readyPods     int32
		revision      string
		referenced    []string
	}{
		{name: "a group that has nothing yet", ready: "False", reason: ReasonInstanceCountMismatch},
		{name: "established and every member ready",
			ledger: ledgerOf(established, 0), pods: threePods(),
			ready: "True", reason: ReasonAllInstancesReady, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "every member ready but not established: the ledger decides",
			ledger: ledgerOf(creating, 0), pods: threePods(),
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "draining",
			ledger: ledgerOf(draining, 0), pods: threePods(),
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "a member is not ready",
			ledger: ledgerOf(established, 0), pods: threePods(notReady),
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 2, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "a member is missing",
			ledger: ledgerOf(established, 0), pods: threePods()[:2],
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 2, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "a member is terminating: its revision is referenced but not the group's",
			ledger: ledgerOf(established, 0), pods: threePods(terminating, revision("rev-0")),
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 2, revision: "rev-a", referenced: []string{"rev-0", "rev-a"}},
		{name: "members of two revisions: the group has no single revision",
			ledger: ledgerOf(established, 0), pods: threePods(revision("rev-b")),
			ready: "True", reason: ReasonAllInstancesReady, readyPods: 3, referenced: []string{"rev-a", "rev-b"}},
		{name: "an unfinished in-place update references its target",
			ledger: ledgerOf(established, 0), pods: threePods(inPlace),
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 2, revision: "rev-a", referenced: []string{"rev-a", "rev-b"}},
		{name: "an instance the Role plan does not want",
			ledger: ledgerOf(established, 0), pods: threePods(), mutate: func(in *StatusInput) { in.Wants = nil },
			ready: "False", reason: ReasonInstancesNotReady, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "more instances than declared, as during a Role rolling update with surge",
			ledger: append(ledgerOf(established, 0), workloadv1alpha1.RoleInstanceStatus{Role: "prefill", Index: 1, Phase: creating}),
			pods:   threePods(),
			ready:  "False", reason: ReasonInstanceCountMismatch, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "an instance of a Role the group does not declare",
			ledger: ledgerOf(established, 0), pods: append(threePods(), memberPod("decode", 0, 0)),
			ready: "False", reason: ReasonInstanceCountMismatch, readyPods: 4, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "no replicas and no instance: ready",
			mutate: noReplicas, ready: "True", reason: ReasonAllInstancesReady},
		{name: "no replicas but an instance is left",
			ledger: ledgerOf(draining, 0), pods: threePods(), mutate: noReplicas,
			ready: "False", reason: ReasonInstanceCountMismatch, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "template unresolved: enough Pods and all ready",
			ledger: ledgerOf(established, 0), pods: threePods(), mutate: unresolved,
			ready: "True", reason: ReasonTemplateUnresolved, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "template unresolved: a Pod is not ready",
			ledger: ledgerOf(established, 0), pods: threePods(notReady), mutate: unresolved,
			ready: "False", reason: ReasonTemplateUnresolved, readyPods: 2, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "template unresolved: fewer Pods than expected",
			ledger: ledgerOf(established, 0), pods: threePods()[:2], mutate: unresolved,
			ready: "False", reason: ReasonTemplateUnresolved, readyPods: 2, revision: "rev-a", referenced: []string{"rev-a"}},
		{name: "template of one instance unresolved",
			ledger: ledgerOf(established, 0), pods: threePods(),
			mutate: func(in *StatusInput) { in.Wants[InstanceKey{Role: "prefill", Index: 0}] = &Want{} },
			ready:  "True", reason: ReasonTemplateUnresolved, readyPods: 3, revision: "rev-a", referenced: []string{"rev-a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*StatusInput)
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			group := statusGroup()
			status := statusOf(group, tt.ledger, tt.pods, mutate...)

			if value, reason := conditionOf(t, status, condReady); value != tt.ready || reason != tt.reason {
				t.Errorf("Ready = %s/%s, want %s/%s", value, reason, tt.ready, tt.reason)
			}
			if status.ReadyPods != tt.readyPods {
				t.Errorf("readyPods = %d, want %d", status.ReadyPods, tt.readyPods)
			}
			if status.Revision != tt.revision {
				t.Errorf("revision = %q, want %q", status.Revision, tt.revision)
			}
			if !reflect.DeepEqual(status.ReferencedRevisions, tt.referenced) {
				t.Errorf("referencedRevisions = %v, want %v", status.ReferencedRevisions, tt.referenced)
			}
			if status.ObservedGeneration != 4 || status.ObservedPods != Digest(tt.pods) {
				t.Errorf("observed generation %d and Pods %s, want 4 and %s", status.ObservedGeneration, status.ObservedPods, Digest(tt.pods))
			}
			if status.LabelSelector != "modelserving.volcano.sh/group-name="+testGroup {
				t.Errorf("labelSelector = %q", status.LabelSelector)
			}
			if !reflect.DeepEqual(status.RoleInstances, tt.ledger) {
				t.Errorf("roleInstances = %v, want the ledger that was passed in", status.RoleInstances)
			}
			for _, absent := range []string{condFailed, condDrained, condRecreate, condBlocked, condError, condCoord} {
				if meta.FindStatusCondition(status.Conditions, absent) != nil {
					t.Errorf("condition %s is set", absent)
				}
			}
		})
	}
}

// The ledger a round passes in is the one after its own changes. The entries
// the instances were observed with are those before.
func TestComputeStatusReadsTheLedgerOfThisRound(t *testing.T) {
	group := statusGroup()
	if value, _ := conditionOf(t, statusOf(group, ledgerOf(established, 0), threePods()), condReady); value != "True" {
		t.Errorf("Ready = %s in the round that establishes the instance, want True", value)
	}
	group.Status.RoleInstances = ledgerOf(established, 0)
	if value, _ := conditionOf(t, statusOf(group, ledgerOf(draining, 0), threePods()), condReady); value != "False" {
		t.Errorf("Ready = %s in the round that starts to drain the instance, want False", value)
	}
}

// roleClasses says, per Role, which revisions render the same Pods: the ones
// with the same class. A revision that is not listed renders Pods of its own.
type roleClasses map[string]map[string]string

func (c roleClasses) CompareRole(revision, target, role string) workload.Comparison {
	if revision == "unreadable" || target == "unreadable" {
		return workload.Unknown
	}
	class := func(revision string) string {
		if class, ok := c[role][revision]; ok {
			return class
		}
		return revision
	}
	if class(revision) == class(target) {
		return workload.Equivalent
	}
	return workload.Different
}

func TestComputeStatusRevision(t *testing.T) {
	// rev-b changes prefill and not decode; rev-c changes decode and not prefill.
	same := roleClasses{
		"decode":  {"rev-a": "first", "rev-b": "first"},
		"prefill": {"rev-b": "second", "rev-c": "second"},
	}
	at := func(revision string) func(*corev1.Pod) {
		return func(pod *corev1.Pod) { pod.Labels[workloadv1alpha1.RevisionLabelKey] = revision }
	}
	pods := func(prefill, decode string, more ...*corev1.Pod) []*corev1.Pod {
		return append([]*corev1.Pod{memberPod("prefill", 0, 0, at(prefill)), memberPod("decode", 0, 0, at(decode))}, more...)
	}
	tests := []struct {
		name             string
		target, baseline string
		pods             []*corev1.Pod
		revisions        RoleComparer
		want             string
	}{
		{name: "one label", pods: threePods(), want: "rev-a"},
		{name: "no Pod", target: "rev-b", revisions: same, want: ""},
		{name: "several labels and nothing to compare them with", target: "rev-b", pods: pods("rev-b", "rev-a"), want: ""},
		{name: "the Role the target does not change keeps its labels", target: "rev-b", pods: pods("rev-b", "rev-a"), revisions: same, want: "rev-b"},
		{name: "the changed Role is not rolled yet: the baseline", target: "rev-c", baseline: "rev-b", pods: pods("rev-b", "rev-a"), revisions: same, want: "rev-b"},
		{name: "the changed Role is rolled in part", target: "rev-c", baseline: "rev-b",
			pods: pods("rev-b", "rev-a", memberPod("decode", 1, 0, at("rev-c"))), revisions: same, want: ""},
		{name: "rolled twice, another Role each time", target: "rev-c", baseline: "rev-b", pods: pods("rev-b", "rev-c"), revisions: same, want: "rev-c"},
		{name: "a terminating Pod of the old revision does not count", target: "rev-b",
			pods: pods("rev-b", "rev-a", memberPod("prefill", 1, 0, at("rev-a"), terminating)), revisions: same, want: "rev-b"},
		{name: "the target cannot be read: what the labels agree on", target: "unreadable", pods: pods("rev-b", "rev-a"), revisions: same, want: "rev-b"},
		{name: "nothing can be read", target: "rev-b", pods: pods("rev-b", "unreadable"), revisions: same, want: ""},
	}
	for _, tt := range tests {
		group := statusGroup()
		group.Spec.Revision, group.Spec.TargetRevision, group.Spec.BaselineRevision = "", tt.target, tt.baseline
		status := statusOf(group, nil, tt.pods, func(in *StatusInput) { in.Revisions = tt.revisions })
		if status.Revision != tt.want {
			t.Errorf("%s: revision = %q, want %q", tt.name, status.Revision, tt.want)
		}
	}
}

func TestComputeStatusPods(t *testing.T) {
	tests := []struct {
		name      string
		read      int32
		pods      []*corev1.Pod
		pending   int
		committed bool
		want      int32
	}{
		{name: "nothing decided yet", pending: 3, want: 0},
		{name: "the write that precedes the first creation", pending: 3, committed: true, want: 3},
		{name: "committed earlier, nothing created yet", read: 3, pending: 3, want: 3},
		{name: "some created", read: 3, pods: threePods()[:1], pending: 2, want: 3},
		{name: "terminating Pods count", pods: threePods(terminating), want: 3},
		{name: "status was cleared, the Pods are there", pods: threePods(), want: 3},
		{name: "everything was drained and is created again", read: 3, pending: 3, want: 3},
		{name: "nothing left and nothing to create", read: 3, want: 0},
	}
	for _, tt := range tests {
		group := statusGroup()
		group.Status.Pods = tt.read
		status := statusOf(group, nil, tt.pods, func(in *StatusInput) {
			in.PendingCreates, in.CreationCommitted = tt.pending, tt.committed
		})
		if status.Pods != tt.want {
			t.Errorf("%s: pods = %d, want %d", tt.name, status.Pods, tt.want)
		}
	}
}

func TestComputeStatusFailedAndDrained(t *testing.T) {
	fail := func(in *StatusInput) { in.Fail = &ConditionChange{Reason: FailMemberLost, Message: "now"} }
	wasFailed := trueCondition(condFailed, FailMemberFaulty)
	// restartedOnce has a fault that recovery tolerates for a grace period.
	tests := []struct {
		name       string
		conditions []metav1.Condition
		ledger     []workloadv1alpha1.RoleInstanceStatus
		pods       []*corev1.Pod
		mutate     func(*StatusInput)

		failed, failedReason   string
		drained, drainedReason string
		faulty, terminating    int32
	}{
		{name: "a tolerated fault is not counted",
			ledger: ledgerOf(established, 0), pods: threePods(restarted)},
		{name: "Drained means nothing without Failed and is removed",
			conditions: []metav1.Condition{trueCondition(condDrained, ReasonNoFaultyMember)},
			ledger:     ledgerOf(established, 0), pods: threePods()},
		{name: "failed in this round, a member has a fault",
			ledger: ledgerOf(established, 0), pods: threePods(restarted), mutate: fail,
			failed: "True", failedReason: FailMemberLost, drained: "False", drainedReason: ReasonFaultyMembersRemain, faulty: 1},
		{name: "failed in this round with no Pod left: drained in the same write",
			ledger: ledgerOf(established, 0), mutate: fail,
			failed: "True", failedReason: FailMemberLost, drained: "True", drainedReason: ReasonNoFaultyMember},
		{name: "failed earlier: stays failed with its first reason",
			conditions: []metav1.Condition{wasFailed}, ledger: ledgerOf(established, 0), pods: threePods(), mutate: fail,
			failed: "True", failedReason: FailMemberFaulty, drained: "True", drainedReason: ReasonNoFaultyMember},
		{name: "failed earlier and the round says nothing: stays failed",
			conditions: []metav1.Condition{wasFailed}, ledger: ledgerOf(established, 0), pods: threePods()[:2],
			failed: "True", failedReason: FailMemberFaulty, drained: "True", drainedReason: ReasonNoFaultyMember},
		{name: "failed: a terminating Pod keeps the group from being drained",
			conditions: []metav1.Condition{wasFailed}, ledger: ledgerOf(established, 0), pods: threePods(terminating),
			failed: "True", failedReason: FailMemberFaulty, drained: "False", drainedReason: ReasonFaultyMembersRemain, terminating: 1},
		{name: "failed and drained, then a member fails: no longer drained",
			conditions: []metav1.Condition{wasFailed, trueCondition(condDrained, ReasonNoFaultyMember)},
			ledger:     ledgerOf(established, 0), pods: threePods(failed),
			failed: "True", failedReason: FailMemberFaulty, drained: "False", drainedReason: ReasonFaultyMembersRemain, faulty: 1},
		{name: "a tolerated fault counts in an instance that is being drained",
			ledger: ledgerOf(draining, 0), pods: threePods(restarted), faulty: 1},
		{name: "but not in a Pod created after the drain was decided",
			ledger: ledgerOf(draining, 0), pods: threePods(restarted, func(p *corev1.Pod) {
				p.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = "1"
			})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*StatusInput)
			if tt.mutate != nil {
				mutate = append(mutate, tt.mutate)
			}
			status := statusOf(statusGroup(tt.conditions...), tt.ledger, tt.pods, mutate...)
			if value, reason := conditionOf(t, status, condFailed); value != tt.failed || reason != tt.failedReason {
				t.Errorf("Failed = %s/%s, want %s/%s", value, reason, tt.failed, tt.failedReason)
			}
			if value, reason := conditionOf(t, status, condDrained); value != tt.drained || reason != tt.drainedReason {
				t.Errorf("Drained = %s/%s, want %s/%s", value, reason, tt.drained, tt.drainedReason)
			}
			if status.FaultyMembers != tt.faulty || status.TerminatingPods != tt.terminating {
				t.Errorf("faultyMembers, terminatingPods = %d, %d, want %d, %d", status.FaultyMembers, status.TerminatingPods, tt.faulty, tt.terminating)
			}
		})
	}
}

func TestComputeStatusConditionLifecycle(t *testing.T) {
	earlier := metav1.NewTime(time.Unix(1700000500, 0))
	now := metav1.NewTime(testNow)
	group := statusGroup(
		metav1.Condition{Type: condReady, Status: metav1.ConditionTrue, Reason: ReasonAllInstancesReady, ObservedGeneration: 2, LastTransitionTime: earlier},
		trueCondition(condRecreate, "InPlaceUpdateNotPossible"),
		trueCondition(condBlocked, ReasonRoleDeleteHookFailed),
		trueCondition(condError, "Error"),
		trueCondition(condCoord, "WaitingForRoles"),
	)

	// Nothing changed but the generation: values and transition times stay.
	status := statusOf(group, ledgerOf(established, 0), threePods(), func(in *StatusInput) {
		in.DrainBlocked = "hook failed"
		in.Failure = &ConditionChange{Reason: "Error", Message: "again"}
		in.Coordination = &metav1.Condition{Status: metav1.ConditionTrue, Reason: "WaitingForRoles", Message: "again"}
		in.RecreateRequired = &ConditionChange{Reason: "AdoptedRevisionMismatch", Message: "now"}
	})
	var types []string
	for _, condition := range status.Conditions {
		types = append(types, condition.Type)
		if !condition.LastTransitionTime.Equal(&earlier) {
			t.Errorf("condition %s: transition time changed although its value did not", condition.Type)
		}
		if condition.ObservedGeneration != 4 {
			t.Errorf("condition %s: observed at generation %d, want 4", condition.Type, condition.ObservedGeneration)
		}
	}
	if want := []string{condCoord, condBlocked, condReady, condError, condRecreate}; !reflect.DeepEqual(types, want) {
		t.Errorf("conditions = %v, want %v", types, want)
	}
	if _, reason := conditionOf(t, status, condRecreate); reason != "InPlaceUpdateNotPossible" {
		t.Errorf("RecreateRequired changed its reason to %s", reason)
	}
	if message := meta.FindStatusCondition(status.Conditions, condError).Message; message != "again" {
		t.Errorf("ReconcileFailed message = %q, want the one of this round", message)
	}
	if len(group.Status.Conditions) != 5 || group.Status.Conditions[0].ObservedGeneration != 2 {
		t.Error("ComputeStatus modified the conditions of the group it was given")
	}

	// The next round has nothing to report: what only exists while true goes
	// away, RecreateRequired stays, Ready flips with a new transition time.
	group.Status = status
	status = statusOf(group, ledgerOf(established, 0), threePods(notReady))
	types = nil
	for _, condition := range status.Conditions {
		types = append(types, condition.Type)
	}
	if want := []string{condReady, condRecreate}; !reflect.DeepEqual(types, want) {
		t.Errorf("conditions = %v, want %v", types, want)
	}
	if ready := meta.FindStatusCondition(status.Conditions, condReady); ready.Status != metav1.ConditionFalse || !ready.LastTransitionTime.Equal(&now) {
		t.Errorf("Ready = %s since %v, want False since now", ready.Status, ready.LastTransitionTime)
	}

	// The rollout strategy becomes RoleRollingUpdate. Nothing replaces a
	// group as a whole then: the condition that asks for it goes away.
	group.Status = status
	group.Spec.RolloutStrategy = &workloadv1alpha1.ServingGroupRolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	status = statusOf(group, ledgerOf(established, 0), threePods())
	if meta.FindStatusCondition(status.Conditions, condRecreate) != nil {
		t.Errorf("RecreateRequired stays on a group that is rolled Role by Role")
	}
}

func TestEmpty(t *testing.T) {
	group := statusGroup()
	if Empty(group) {
		t.Error("a group with a Role of one replica is not empty")
	}
	group.Spec.Roles[0].Replicas = 0
	if !Empty(group) {
		t.Error("a group whose Roles have no replicas is empty")
	}
}
