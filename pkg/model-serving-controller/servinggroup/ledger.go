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
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Member is a member Pod of a group together with what the rules know about it.
type Member struct {
	Pod        *corev1.Pod
	Membership Membership
	// State is the state of a Pod the group owns. It is empty for a Pod that is
	// still to be adopted.
	State MemberState
	// RetryAt, when not zero, is when the fault of a failing member starts to count.
	RetryAt time.Time
	// Ordinal is the position of the Pod in its Role instance: 0 for the entry
	// Pod, 1 and up for the workers.
	Ordinal int
}

// Generation returns the generation of the Role instance the Pod was created for.
func (m Member) Generation() int64 {
	return InstanceGeneration(m.Pod)
}

// Instance is one Role instance of a group as observed: its ledger entry and
// its member Pods.
type Instance struct {
	Role  string
	Index int
	// Entry is the ledger entry of the instance, nil when the ledger has none.
	Entry *workloadv1alpha1.RoleInstanceStatus
	// Members are the member Pods of the instance, sorted by name.
	Members []Member
}

// Phase returns the phase of the instance. An instance without a ledger entry
// is being created: the zero value of the ledger never causes a deletion.
func (i Instance) Phase() workloadv1alpha1.RoleInstancePhase {
	if i.Entry == nil || i.Entry.Phase == "" {
		return workloadv1alpha1.RoleInstanceCreating
	}
	return i.Entry.Phase
}

// Generation returns the current generation of the instance. While the
// instance is draining it is the generation being drained, and Pods of a later
// generation are not part of it. Otherwise it is never below the generation of
// a Pod the group owns, whatever the ledger says.
func (i Instance) Generation() int64 {
	generation := int64(0)
	if i.Entry != nil {
		generation = i.Entry.Generation
	}
	if i.Phase() == workloadv1alpha1.RoleInstanceDraining {
		return generation
	}
	return max(generation, i.maxOwnedGeneration())
}

func (i Instance) maxOwnedGeneration() int64 {
	generation := int64(0)
	for _, member := range i.Members {
		if member.Membership == Owned {
			generation = max(generation, member.Generation())
		}
	}
	return generation
}

// Observe sorts the member Pods of a group into Role instances and joins them
// with the ledger. Instances are returned sorted by Role and index. A member
// whose labels and name do not place it in a Role instance is returned in
// strays; that can only be a Pod the group owns whose labels were changed.
func Observe(members []*corev1.Pod, ledger []workloadv1alpha1.RoleInstanceStatus, groupName string, groupUID types.UID, parent Parent, ctx MemberContext) (instances []Instance, strays []*corev1.Pod) {
	type key struct {
		role  string
		index int
	}
	byKey := map[key]*Instance{}
	instance := func(role string, index int) *Instance {
		k := key{role, index}
		if byKey[k] == nil {
			byKey[k] = &Instance{Role: role, Index: index}
		}
		return byKey[k]
	}
	for i := range ledger {
		instance(ledger[i].Role, int(ledger[i].Index)).Entry = &ledger[i]
	}
	for _, pod := range members {
		membership := MembershipOf(pod, groupName, groupUID, parent)
		if membership == NotMember {
			continue
		}
		role, roleID := pod.Labels[workloadv1alpha1.RoleLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey]
		index, okIndex := ordinalAfter(roleID, role+"-")
		ordinal, okOrdinal := ordinalAfter(pod.Name, groupName+"-"+roleID+"-")
		if role == "" || !okIndex || !okOrdinal {
			strays = append(strays, pod)
			continue
		}
		member := Member{Pod: pod, Membership: membership, Ordinal: ordinal}
		if membership == Owned {
			member.State, member.RetryAt = ClassifyMember(pod, ctx)
		}
		inst := instance(role, index)
		inst.Members = append(inst.Members, member)
	}
	instances = make([]Instance, 0, len(byKey))
	for _, inst := range byKey {
		sort.Slice(inst.Members, func(a, b int) bool { return inst.Members[a].Pod.Name < inst.Members[b].Pod.Name })
		instances = append(instances, *inst)
	}
	sort.Slice(instances, func(a, b int) bool {
		if instances[a].Role != instances[b].Role {
			return instances[a].Role < instances[b].Role
		}
		return instances[a].Index < instances[b].Index
	})
	return instances, strays
}

// Want says that the Role plan of this round wants an instance to exist.
type Want struct {
	// Workers is the number of worker Pods of the instance besides its entry Pod,
	// taken from the template the instance runs.
	Workers int32
	// LayoutKnown is false when that template cannot be resolved. Nothing is then
	// created for the instance and it is not established.
	LayoutKnown bool
	// Revision is the revision the members of the instance are created from.
	Revision string
	// Surge: the instance is temporary capacity of a Role rolling update. Members
	// created for it carry the Role surge mark.
	Surge bool
}

// Scope is what the transition of an instance depends on besides the instance.
type Scope struct {
	// GroupTerminating: the group has a deletionTimestamp.
	GroupTerminating bool
	// GroupFailed: the Failed condition of the group is true.
	GroupFailed bool
	// Policy is the recovery policy that applies now.
	Policy workloadv1alpha1.RecoveryPolicy
}

// LedgerChange is a change of one ledger entry.
type LedgerChange struct {
	// Remove drops the entry.
	Remove     bool
	Phase      workloadv1alpha1.RoleInstancePhase
	Generation int64
}

// Reasons a group fails as a whole.
const (
	// FailMemberLost: a member of an established instance is missing or terminating.
	FailMemberLost = "MemberLost"
	// FailMemberFaulty: a member has a fault that counts.
	FailMemberFaulty = "MemberFaulty"
	// FailAllPodsLost: the group has no Pod left.
	FailAllPodsLost = "AllPodsLost"
)

// Step is what one round does about one Role instance. The fields are in the
// order they take effect: adopt; call the hook; write the ledger or fail the
// group; delete; create. A later part only happens when the earlier ones
// succeeded.
type Step struct {
	// Rule names the transition that applied, for logs and tests.
	Rule string
	// Adopt lists member Pods the group has to become the owner of. Nothing else
	// is done about the instance in that round.
	Adopt []*corev1.Pod
	// RoleDelete asks for the OnRoleDelete hook. Ledger is written only when the
	// hook succeeded.
	RoleDelete bool
	// Ledger is the change of the ledger entry of the instance, nil for none.
	Ledger *LedgerChange
	// FailGroup, when not empty, is the reason to set the Failed condition of the
	// group with.
	FailGroup string
	// Delete lists the Pods to delete, after Ledger or FailGroup was written.
	Delete []*corev1.Pod
	// Create lists the ordinals of the members to create, with Generation.
	Create     []int
	Generation int64
	// Waiting, when not empty, says what the instance waits for.
	Waiting string
	// Confirm reports that the step rests on Pods being absent, terminating or
	// faulty. Such evidence is confirmed by reading the API directly before the
	// step is taken.
	Confirm bool
}

// Transition decides what to do about one Role instance. want is nil when the
// Role plan does not want the instance (any more).
//
// The decision depends on the instance, want and scope alone. In particular it
// does not depend on how the instance got into its state, so a process that
// takes over continues where the previous one stopped.
func Transition(inst Instance, want *Want, scope Scope) Step {
	if scope.GroupTerminating {
		return Step{Rule: "E0"}
	}
	var owned []Member
	var adopt []*corev1.Pod
	for _, member := range inst.Members {
		if member.Membership == Owned {
			owned = append(owned, member)
		} else {
			adopt = append(adopt, member.Pod)
		}
	}
	if scope.GroupFailed {
		// The group is replaced as a whole. Only the members with a fault are
		// deleted here; the others keep running until those are gone.
		faulty := withFault(owned)
		return Step{Rule: "E1", Delete: pods(faulty), Confirm: len(faulty) > 0}
	}
	if len(adopt) > 0 {
		return Step{Rule: "R2", Adopt: adopt}
	}

	generation := inst.Generation()
	if inst.Phase() == workloadv1alpha1.RoleInstanceDraining {
		var old []Member
		for _, member := range owned {
			if member.Generation() <= generation {
				old = append(old, member)
			}
		}
		if len(old) > 0 {
			return deleteInTwoSteps(Step{Rule: "E2"}, old)
		}
		// That the Pods are gone is what the next generation is created on:
		// it is read from the API server. A cache that does not show them yet
		// would end the drain before it began.
		step := Step{Rule: "E3", RoleDelete: true, Confirm: true, Ledger: &LedgerChange{Remove: true}}
		if want != nil {
			next := max(generation, inst.maxOwnedGeneration()) + 1
			step.Ledger = &LedgerChange{Phase: workloadv1alpha1.RoleInstanceCreating, Generation: next}
			if want.LayoutKnown {
				step.Create, _ = missingOrdinals(owned, want.Workers)
				step.Generation = next
			}
		}
		return step
	}

	if want == nil {
		if inst.Entry == nil && len(owned) == 0 {
			return Step{Rule: "E9"}
		}
		return drain("E4", owned, generation)
	}
	if inst.Entry != nil && inst.Entry.Generation < generation {
		// A Pod is of a later generation than the ledger records. Raise the
		// ledger first, so that a later Draining covers the Pod.
		return Step{Rule: "L5", Ledger: &LedgerChange{Phase: inst.Phase(), Generation: generation}}
	}

	for _, member := range owned {
		if member.State != MemberFaulty {
			continue
		}
		switch scope.Policy {
		case workloadv1alpha1.RoleRecreate:
			return drain("E5", owned, generation)
		case workloadv1alpha1.ServingGroupRecreate:
			return Step{Rule: "E5", FailGroup: FailMemberFaulty, Confirm: true}
		}
	}

	missing, terminating := []int(nil), false
	if want.LayoutKnown {
		missing, terminating = missingOrdinals(owned, want.Workers)
	}
	waiting := ""
	if terminating {
		waiting = "a terminating member"
	}

	if inst.Phase() == workloadv1alpha1.RoleInstanceEstablished {
		if len(missing) == 0 && !terminating {
			return Step{Rule: "E9"}
		}
		switch scope.Policy {
		case workloadv1alpha1.RoleRecreate:
			return drain("E6", owned, generation)
		case workloadv1alpha1.ServingGroupRecreate:
			return Step{Rule: "E6", FailGroup: FailMemberLost, Confirm: true}
		}
		return Step{Rule: "E6", Create: missing, Generation: generation, Waiting: waiting, Confirm: len(missing) > 0}
	}

	if !want.LayoutKnown {
		return Step{Rule: "E7", Waiting: "the template of the instance"}
	}
	if len(missing) > 0 || terminating {
		return Step{Rule: "E7", Create: missing, Generation: generation, Waiting: waiting}
	}
	return Step{Rule: "E8", Ledger: &LedgerChange{Phase: workloadv1alpha1.RoleInstanceEstablished, Generation: generation}}
}

// drain commits the removal of the generation of an instance and starts it.
func drain(rule string, owned []Member, generation int64) Step {
	step := Step{Rule: rule, Ledger: &LedgerChange{Phase: workloadv1alpha1.RoleInstanceDraining, Generation: generation}}
	return deleteInTwoSteps(step, owned)
}

// deleteInTwoSteps deletes the members with a fault first. The other members
// are deleted once no member with a fault and no terminating member is left,
// so that they keep running until the faulty ones have stopped.
func deleteInTwoSteps(step Step, members []Member) Step {
	step.Confirm = true
	if faulty := withFault(members); len(faulty) > 0 {
		step.Delete = pods(faulty)
		return step
	}
	var rest []Member
	for _, member := range members {
		if member.State == MemberTerminating {
			step.Waiting = "a terminating member"
			return step
		}
		rest = append(rest, member)
	}
	step.Delete = pods(rest)
	return step
}

func withFault(members []Member) []Member {
	var faulty []Member
	for _, member := range members {
		if member.State.HasFault() {
			faulty = append(faulty, member)
		}
	}
	return faulty
}

func pods(members []Member) []*corev1.Pod {
	var result []*corev1.Pod
	for _, member := range members {
		result = append(result, member.Pod)
	}
	return result
}

// missingOrdinals returns the ordinals in [0, workers] at which the instance
// has no Pod, and whether a Pod at one of those ordinals is terminating. A
// terminating Pod still holds its name, so its ordinal is not missing.
func missingOrdinals(owned []Member, workers int32) (missing []int, terminating bool) {
	present := map[int]bool{}
	for _, member := range owned {
		if member.Ordinal > int(workers) {
			continue
		}
		present[member.Ordinal] = true
		if member.State == MemberTerminating {
			terminating = true
		}
	}
	for ordinal := 0; ordinal <= int(workers); ordinal++ {
		if !present[ordinal] {
			missing = append(missing, ordinal)
		}
	}
	return missing, terminating
}

// AllPodsLost reports that a group which had an established Role instance has
// no member Pod left: none it owns, none to adopt and none terminating. Under
// ServingGroupRollingUpdate such a group is replaced as a whole whatever the
// recovery policy is, because the revision of a whole group is chosen by its
// ordinal and the partition, which the ModelServing controller knows.
func AllPodsLost(instances []Instance, strays []*corev1.Pod) bool {
	if len(strays) > 0 {
		return false
	}
	established := false
	for _, inst := range instances {
		if len(inst.Members) > 0 {
			return false
		}
		if inst.Phase() == workloadv1alpha1.RoleInstanceEstablished {
			established = true
		}
	}
	return established
}

// BeingBorn reports whether nothing of a group was ever established, which
// is one half of a group that is still being born; the other half is that it
// has no Pod, and is for the caller to know. Only the ledger of group is
// read, so the answer belongs to the resourceVersion of group.
//
// An instance was established when the ledger has an entry for it that is
// not Creating at generation 0: it is established, or is being drained, or
// is being created again after a drain.
//
// The ledger is not a history, and in three cases it says less than that.
//
// A group can have Pods and no instance that is established. An instance is
// established in the round after its last Pod was created, and an instance
// of which one Pod is refused is not established at all. That is why the
// caller looks at the Pods as well.
//
// A group whose instances were all drained and are not wanted for now, its
// Roles having been scaled to nothing, has no entry left.
//
// An instance that was drained before it was ever complete, a Pod of it
// having failed first, reads as one that was established.
func BeingBorn(group *workloadv1alpha1.ServingGroup) bool {
	for _, entry := range group.Status.RoleInstances {
		creating := entry.Phase == workloadv1alpha1.RoleInstanceCreating || entry.Phase == ""
		if !creating || entry.Generation > 0 {
			return false
		}
	}
	return true
}

// ApplyLedger returns ledger with the changes applied, sorted by Role and
// index. Entries are identified by Role and index; ledger is not modified.
func ApplyLedger(ledger []workloadv1alpha1.RoleInstanceStatus, changes map[InstanceKey]LedgerChange) []workloadv1alpha1.RoleInstanceStatus {
	result := make([]workloadv1alpha1.RoleInstanceStatus, 0, len(ledger)+len(changes))
	seen := map[InstanceKey]bool{}
	for _, entry := range ledger {
		key := InstanceKey{Role: entry.Role, Index: int(entry.Index)}
		seen[key] = true
		change, changed := changes[key]
		switch {
		case !changed:
			result = append(result, entry)
		case !change.Remove:
			result = append(result, workloadv1alpha1.RoleInstanceStatus{Role: entry.Role, Index: entry.Index, Phase: change.Phase, Generation: change.Generation})
		}
	}
	for key, change := range changes {
		if !seen[key] && !change.Remove {
			result = append(result, workloadv1alpha1.RoleInstanceStatus{Role: key.Role, Index: int32(key.Index), Phase: change.Phase, Generation: change.Generation})
		}
	}
	sort.Slice(result, func(a, b int) bool {
		if result[a].Role != result[b].Role {
			return result[a].Role < result[b].Role
		}
		return result[a].Index < result[b].Index
	})
	return result
}

// InstanceKey identifies a Role instance within a group.
type InstanceKey struct {
	Role  string
	Index int
}

// Key returns the key of the instance.
func (i Instance) Key() InstanceKey {
	return InstanceKey{Role: i.Role, Index: i.Index}
}
