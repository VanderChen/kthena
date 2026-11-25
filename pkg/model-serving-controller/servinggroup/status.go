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
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Reasons of the conditions of a ServingGroup.
const (
	ReasonAllInstancesReady     = "AllInstancesReady"
	ReasonInstancesNotReady     = "InstancesNotReady"
	ReasonInstanceCountMismatch = "InstanceCountMismatch"
	ReasonTemplateUnresolved    = "TemplateUnresolved"
	ReasonNoFaultyMember        = "NoFaultyMember"
	ReasonFaultyMembersRemain   = "FaultyMembersRemain"
	ReasonRoleDeleteHookFailed  = "RoleDeleteHookFailed"
)

// ConditionChange sets a condition that a round decided on.
type ConditionChange struct {
	Reason  string
	Message string
}

// StatusInput is what the status of a group is computed from: the group as it
// was read, the member Pods of this round, and what the round decided.
type StatusInput struct {
	Group *workloadv1alpha1.ServingGroup
	// Now is the transition time of a condition that changes its value.
	Now time.Time
	// Members are the member Pods the round worked with. ObservedPods is their digest.
	Members []*corev1.Pod
	// Instances are the Role instances observed from Members. Their ledger
	// entries are not read: Ledger is.
	Instances []Instance
	// Ledger is the ledger after the changes of this round.
	Ledger []workloadv1alpha1.RoleInstanceStatus
	// Wants holds the instances the Role plan of this round wants, with the
	// layout of the template each of them runs.
	Wants map[InstanceKey]*Want
	// LayoutUnknown reports that no Role plan could be made because the template
	// of the group cannot be resolved.
	LayoutUnknown bool
	// Revisions compares what a Role renders in two revisions. With it the
	// group also runs a revision that the labels of its Pods do not name (see
	// revisionOf); without it only the labels count.
	Revisions RoleComparer
	// Updating holds the instances whose in-place update is not complete: to be
	// patched, restarting, or waiting for the labels of their Pods to change.
	// Such an instance is not ready, whatever its Pods report.
	Updating map[InstanceKey]bool

	// FailurePending leaves the ReconcileFailed condition as the group carries
	// it: the status is written before the round acts, and whether the round
	// fails is not known yet. A round that fails the way the one before it did
	// then writes nothing, and one that removed the condition before acting
	// would write twice, every time.
	FailurePending bool

	// Fail, when not nil, sets the Failed condition. Failed is never cleared.
	Fail *ConditionChange
	// RecreateRequired, when not nil, sets the RecreateRequired condition. It is never cleared.
	RecreateRequired *ConditionChange
	// DrainBlocked, when not empty, says which hook keeps an instance from being drained.
	DrainBlocked string
	// Failure, when not nil, reports that the round ended with an error.
	Failure *ConditionChange
	// Coordination, when not nil, is the CoordinatedRoleRolloutBlocked condition of this round.
	Coordination *metav1.Condition

	// PendingCreates is the number of member Pods the round creates or will create.
	PendingCreates int
	// CreationCommitted reports that this status write is the one that precedes
	// the creation of the first Pod of the group.
	CreationCommitted bool
}

// ComputeStatus returns the status of a group. Apart from the ledger, the
// Failed and RecreateRequired conditions and the commitment recorded in Pods,
// everything in it is derived from the member Pods and the spec, so a status
// that was cleared is restored by the next round.
func ComputeStatus(in StatusInput) workloadv1alpha1.ServingGroupStatus {
	group := in.Group
	status := workloadv1alpha1.ServingGroupStatus{
		ObservedGeneration: group.Generation,
		ObservedPods:       Digest(in.Members),
		LabelSelector:      workloadv1alpha1.GroupNameLabelKey + "=" + group.Name,
		Conditions:         append([]metav1.Condition(nil), group.Status.Conditions...),
	}
	if len(in.Ledger) > 0 {
		status.RoleInstances = in.Ledger
	}
	instances := withLedger(in.Instances, in.Ledger)

	referenced := sets.New[string]()
	for _, pod := range in.Members {
		if revision := utils.ObjectRevision(pod); revision != "" {
			referenced.Insert(revision)
		}
		if state := inplace.StateOf(pod); state.InProgress() && state.Revision != "" {
			referenced.Insert(state.Revision)
		}
	}
	status.Revision = revisionOf(group, in.Members, in.Revisions)
	if referenced.Len() > 0 {
		status.ReferencedRevisions = sets.List(referenced)
	}

	failed := in.Fail != nil || meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionFailed))
	faultRemains := false
	for _, inst := range instances {
		draining := inst.Phase() == workloadv1alpha1.RoleInstanceDraining
		for _, member := range inst.Members {
			// A fault that is tolerated counts once the scope of the member is being removed.
			committed := failed || (draining && member.Generation() <= inst.Generation())
			switch {
			case member.State == MemberReady:
				status.ReadyPods++
			case member.State == MemberTerminating:
				status.TerminatingPods++
			case member.State == MemberFaulty, member.State == MemberFailing && committed:
				status.FaultyMembers++
			}
			faultRemains = faultRemains || member.State.HasFault()
		}
	}

	if existing := len(in.Members); existing > 0 || group.Status.Pods > 0 || in.CreationCommitted {
		status.Pods = int32(existing + in.PendingCreates)
	}

	set := func(conditionType workloadv1alpha1.ServingGroupConditionType, value bool, reason, message string) {
		condition := metav1.Condition{
			Type: string(conditionType), Status: metav1.ConditionFalse,
			Reason: reason, Message: message,
			ObservedGeneration: group.Generation, LastTransitionTime: metav1.NewTime(in.Now),
		}
		if value {
			condition.Status = metav1.ConditionTrue
		}
		meta.SetStatusCondition(&status.Conditions, condition)
	}
	// sticky sets a condition that is never cleared once it is true.
	sticky := func(conditionType workloadv1alpha1.ServingGroupConditionType, change *ConditionChange) {
		if current := meta.FindStatusCondition(status.Conditions, string(conditionType)); current != nil && current.Status == metav1.ConditionTrue {
			current.ObservedGeneration = group.Generation
		} else if change != nil {
			set(conditionType, true, change.Reason, change.Message)
		}
	}
	// transient sets a condition that only exists while it is true.
	transient := func(conditionType workloadv1alpha1.ServingGroupConditionType, change *ConditionChange) {
		if change != nil {
			set(conditionType, true, change.Reason, change.Message)
		} else {
			meta.RemoveStatusCondition(&status.Conditions, string(conditionType))
		}
	}

	ready, reason, message := readiness(group, instances, in.Wants, in.Updating, in.LayoutUnknown, status)
	set(workloadv1alpha1.ServingGroupConditionReady, ready, reason, message)

	sticky(workloadv1alpha1.ServingGroupConditionFailed, in.Fail)
	switch {
	case !failed:
		meta.RemoveStatusCondition(&status.Conditions, string(workloadv1alpha1.ServingGroupConditionDrained))
	case faultRemains || status.TerminatingPods > 0:
		set(workloadv1alpha1.ServingGroupConditionDrained, false, ReasonFaultyMembersRemain,
			"members with a fault or terminating Pods remain")
	default:
		set(workloadv1alpha1.ServingGroupConditionDrained, true, ReasonNoFaultyMember,
			"no member with a fault and no terminating Pod is left")
	}
	sticky(workloadv1alpha1.ServingGroupConditionRecreateRequired, in.RecreateRequired)

	var drainBlocked *ConditionChange
	if in.DrainBlocked != "" {
		drainBlocked = &ConditionChange{Reason: ReasonRoleDeleteHookFailed, Message: in.DrainBlocked}
	}
	transient(workloadv1alpha1.ServingGroupConditionDrainBlocked, drainBlocked)
	if !in.FailurePending {
		transient(workloadv1alpha1.ServingGroupConditionReconcileFailed, in.Failure)
	}

	if in.Coordination != nil {
		condition := *in.Coordination
		condition.Type = string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked)
		condition.ObservedGeneration = group.Generation
		condition.LastTransitionTime = metav1.NewTime(in.Now)
		meta.SetStatusCondition(&status.Conditions, condition)
	} else {
		meta.RemoveStatusCondition(&status.Conditions, string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked))
	}

	sort.Slice(status.Conditions, func(a, b int) bool { return status.Conditions[a].Type < status.Conditions[b].Type })
	return status
}

// RoleComparer compares what a Role renders in two revisions.
// workload.History implements it.
type RoleComparer interface {
	CompareRole(revision, target, roleName string) workload.Comparison
}

// revisionOf returns the revision that every Pod of the group that is not
// terminating runs, or "" when there is none.
//
// A Pod runs the revision of its label, and every revision in which its Role
// renders the same Pods. A rollout that replaces Role by Role leaves the Pods
// of an unchanged Role alone, their labels included: after it the labels of
// the group name several revisions although every Pod is what the target
// renders. The revisions the spec names are tried first, so that such a group
// reports the revision it was rolled to.
func revisionOf(group *workloadv1alpha1.ServingGroup, members []*corev1.Pod, revisions RoleComparer) string {
	type labelled struct{ revision, role string }
	var pods []labelled
	labels := sets.New[string]()
	for _, pod := range members {
		if revision := utils.ObjectRevision(pod); revision != "" && pod.DeletionTimestamp == nil {
			pods = append(pods, labelled{revision: revision, role: pod.Labels[workloadv1alpha1.RoleLabelKey]})
			labels.Insert(revision)
		}
	}
	switch {
	case labels.Len() == 1:
		return pods[0].revision
	case labels.Len() == 0 || revisions == nil:
		return ""
	}
	candidates := append([]string{group.Spec.TargetRevision, group.Spec.Revision, group.Spec.BaselineRevision}, sets.List(labels)...)
	for _, candidate := range candidates {
		runs := candidate != ""
		for i := 0; runs && i < len(pods); i++ {
			runs = pods[i].revision == candidate ||
				revisions.CompareRole(pods[i].revision, candidate, pods[i].role) == workload.Equivalent
		}
		if runs {
			return candidate
		}
	}
	return ""
}

// withLedger returns the instances with their entries taken from ledger.
func withLedger(instances []Instance, ledger []workloadv1alpha1.RoleInstanceStatus) []Instance {
	entries := map[InstanceKey]*workloadv1alpha1.RoleInstanceStatus{}
	for i := range ledger {
		entries[InstanceKey{Role: ledger[i].Role, Index: int(ledger[i].Index)}] = &ledger[i]
	}
	out := make([]Instance, 0, len(instances))
	for _, inst := range instances {
		inst.Entry = entries[inst.Key()]
		delete(entries, inst.Key())
		out = append(out, inst)
	}
	for i := range ledger {
		key := InstanceKey{Role: ledger[i].Role, Index: int(ledger[i].Index)}
		if entries[key] != nil {
			out = append(out, Instance{Role: key.Role, Index: key.Index, Entry: &ledger[i]})
		}
	}
	return out
}

// readiness tells whether the group is ready: every Role has its declared
// number of instances, the Role plan wants each of them, and each is
// established, complete and ready by the layout of the template it runs. A
// group whose Roles all have zero replicas is ready when it has no instance.
//
// When a template cannot be resolved the layout is unknown. The group is then
// ready when it has at least the expected number of Pods and all of them are
// ready, so that a group that serves is not reported as broken because its
// history cannot be read.
//
// An instance that is being updated in place is not ready from the round that
// decides to patch it until the labels of its Pods have changed. The Pods of
// the group may all report ready at both ends of that time: before the kubelet
// has seen the patch, and after the last container has restarted.
func readiness(group *workloadv1alpha1.ServingGroup, instances []Instance, wants map[InstanceKey]*Want, updating map[InstanceKey]bool, layoutUnknown bool, status workloadv1alpha1.ServingGroupStatus) (bool, string, string) {
	count := map[string]int32{}
	pods := int32(0)
	notReady := ""
	for _, inst := range instances {
		if inst.Entry == nil && len(inst.Members) == 0 {
			continue
		}
		if updating[inst.Key()] {
			return false, ReasonInstancesNotReady, fmt.Sprintf("instance %s-%d is being updated in place", inst.Role, inst.Index)
		}
		count[inst.Role]++
		pods += int32(len(inst.Members))
		want := wants[inst.Key()]
		if want != nil && !want.LayoutKnown {
			layoutUnknown = true
		}
		if notReady == "" && (want == nil || !inst.Ready(want.Workers)) {
			notReady = fmt.Sprintf("%s-%d", inst.Role, inst.Index)
		}
	}
	if layoutUnknown {
		if pods >= group.Spec.ExpectedPods && status.ReadyPods == pods {
			return true, ReasonTemplateUnresolved, "a template cannot be resolved; every Pod is ready"
		}
		return false, ReasonTemplateUnresolved, "a template cannot be resolved; Pods are missing or not ready"
	}
	declared := map[string]bool{}
	for _, role := range group.Spec.Roles {
		declared[role.Name] = true
		if count[role.Name] != role.Replicas {
			return false, ReasonInstanceCountMismatch, fmt.Sprintf("Role %s has %d of %d instances", role.Name, count[role.Name], role.Replicas)
		}
	}
	for _, inst := range instances {
		if count[inst.Role] > 0 && !declared[inst.Role] {
			return false, ReasonInstanceCountMismatch, fmt.Sprintf("Role %s has %d of 0 instances", inst.Role, count[inst.Role])
		}
	}
	if notReady != "" {
		return false, ReasonInstancesNotReady, fmt.Sprintf("instance %s is not ready", notReady)
	}
	return true, ReasonAllInstancesReady, "every Role instance is ready"
}

// Ready reports whether the instance is established and has a ready Pod at
// every ordinal of its layout.
func (i Instance) Ready(workers int32) bool {
	if i.Phase() != workloadv1alpha1.RoleInstanceEstablished {
		return false
	}
	ready := map[int]bool{}
	for _, member := range i.Members {
		if member.Membership == Owned && member.State == MemberReady {
			ready[member.Ordinal] = true
		}
	}
	for ordinal := 0; ordinal <= int(workers); ordinal++ {
		if !ready[ordinal] {
			return false
		}
	}
	return true
}

// Empty reports whether every Role of the group has zero replicas: the group
// is meant to have no member.
func Empty(group *workloadv1alpha1.ServingGroup) bool {
	for _, role := range group.Spec.Roles {
		if role.Replicas > 0 {
			return false
		}
	}
	return true
}
