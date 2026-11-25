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

// Package grouplevel computes the group-level rollout plan of a ModelServing:
// which ServingGroup objects to create, delete, promote or retarget in one
// reconcile round, and whether the baseline revision may advance
// (docs/proposal/modelserving-servinggroup-cr.md, "Group level: the
// ModelServing controller").
//
// The plan is a pure function of the resolved spec and of what the controller
// reads of every ServingGroup. A round takes these steps, in this order, on a
// working copy that every chosen write is applied to before the next choice:
//
//	S1′  recover: an object for an ordinal that carries ownerless member Pods
//	G0   promote: a surge group below N becomes a regular group
//	G1   delete a Failed group once it is Drained
//	G2   scale down: delete regular groups above N
//	G2′  memberless groups: (a) delete a regular one outside the range,
//	     (b) set the revision of any other to the one of its ordinal
//	G4   fill: create regular groups at the lowest free ordinals below N
//	G3a  create surge groups while groups remain to be replaced
//	G3b  reclaim surge groups that are not needed any more
//	G3c  replace: delete replaceable groups within the two budgets, or
//	     update them in place
//	G6   advance the baseline when the rollout is complete
//
// Writing the desired spec down to every group (G5) is not part of the plan.
//
// The package is a port of the reviewed reference model of the design. The
// vectors in testdata are exported from that model; see testdata/README.md.
package grouplevel

import "k8s.io/apimachinery/pkg/types"

// Mode is the rollout strategy of the ModelServing.
type Mode int

const (
	// ModeServingGroup is ServingGroupRollingUpdate: groups are replaced as a whole.
	ModeServingGroup Mode = iota
	// ModeRole is RoleRollingUpdate: the rollout happens inside every group and
	// the group level only recovers, scales and fills (S1′, G0, G1, G2, G2′(a), G4, G6).
	ModeRole
)

// Spec is the resolved group-level input of one ModelServing.
type Spec struct {
	// Replicas is N, the number of regular groups.
	Replicas int
	// MaxUnavailable is U, resolved (percentages already rounded down by the caller).
	MaxUnavailable int
	// MaxSurge is S, resolved (rounded up).
	MaxSurge int
	// Partition is P, resolved (rounded up). ModeRole: not used.
	Partition int
	// Target is T, the revision of the latest template.
	Target string
	// Baseline is B; "" when it cannot be resolved.
	Baseline string
	Mode     Mode
	// RolePartitioned (ModeRole only): some Role still has a partition; G6 must not advance the baseline.
	RolePartitioned bool
	// InPlace: the effective update method of a ServingGroupRollingUpdate is in place
	// (the caller sets it only when updateMethod is InPlace and U > 0).
	InPlace bool
}

// Group is what the plan reads of one ServingGroup object owned by the ModelServing.
type Group struct {
	// Ordinal is parsed from the name; it is unique among the groups of one ModelServing.
	Ordinal int
	UID     types.UID
	// Revision is the revision the group counts as, resolved by the caller:
	// spec.revision, or status.revision under RoleRollingUpdate.
	Revision string
	// RevisionUnknown: the revision cannot be compared with the target because a
	// template cannot be resolved. The group is not of the target revision and
	// is not replaced on account of its revision either: nothing is deleted on
	// the strength of a history that cannot be read.
	RevisionUnknown bool
	// Surge: the group carries the surge annotation.
	Surge bool
	// Terminating: deletionTimestamp is set.
	Terminating bool
	// Failed: the Failed condition is true.
	Failed bool
	// Drained: the Drained condition is true (only meaningful with Failed).
	Drained bool
	// Observed: status.observedGeneration >= metadata.generation.
	Observed bool
	// Fresh: status.observedPods equals the digest of the Pod cache.
	Fresh bool
	// Ready is the Ready condition as last written.
	Ready bool
	// PodsReady is taken from the Pod cache: the group has member Pods, all ready,
	// none terminating. Only the orders of G2 and G3b read it.
	PodsReady bool
	// Memberless: status.pods is 0 and the Pod cache shows no member Pod.
	Memberless bool
	// Empty: every Role has zero replicas (spec).
	Empty bool
	// RecreateRequired: the RecreateRequired condition is true.
	RecreateRequired bool
	// DeletionCost is the sum of the pod-deletion-cost of the member Pods.
	DeletionCost int64
}

// Orphans describes the ownerless member Pods found on an ordinal. Only Pods
// that have the name of an expected member are passed in.
type Orphans struct {
	// Revision is their common revision; "" when it cannot be read.
	Revision string
	// Marked: at least one carries the copy of the group-level surge mark.
	Marked bool
}

// ActionKind is the kind of one write of the plan.
type ActionKind string

const (
	// ActionCreate creates a ServingGroup at Ordinal with Revision and, when Surge is set, the surge mark.
	ActionCreate ActionKind = "create"
	// ActionDelete deletes the group UID at Ordinal (foreground cascade).
	ActionDelete ActionKind = "delete"
	// ActionPromote removes the surge mark of the group UID at Ordinal.
	ActionPromote ActionKind = "promote"
	// ActionRetarget sets spec.revision of the group UID at Ordinal to Revision.
	ActionRetarget ActionKind = "retarget"
	// ActionAdvanceBaseline sets the baseline of the ModelServing to Revision.
	ActionAdvanceBaseline ActionKind = "baseline"
	// ActionWait writes nothing: it reports why the place at Ordinal is not filled or not replaced.
	ActionWait ActionKind = "wait"
)

// Reasons of an Action.
const (
	// Delete.
	ReasonFailed               = "failed"
	ReasonScaleDown            = "scale-down"
	ReasonEmptyOutOfRange      = "empty-out-of-range"
	ReasonMemberlessOutOfRange = "memberless-out-of-range"
	ReasonReplace              = "replace"
	ReasonReplaceNotReady      = "replace-notready"
	ReasonReclaimSurge         = "reclaim-surge"
	// Create.
	ReasonRecover = "recover"
	ReasonRegular = "regular"
	ReasonSurge   = "surge"
	// Wait. ReasonBaselineUnknown names the ordinal that needs the baseline,
	// ReasonOrdinalOccupied an ordinal below N that a foreign object takes.
	ReasonBaselineUnknown = "baseline-unknown"
	ReasonOrdinalOccupied = "ordinal-occupied"
	// Retarget.
	ReasonMemberless = "memberless"
	ReasonInPlace    = "inplace"
	// Baseline.
	ReasonComplete = "complete"
)

// Action is one write of a round.
type Action struct {
	Kind ActionKind
	// Ordinal is the group, or the place of a creation; -1 for ActionAdvanceBaseline.
	Ordinal int
	// UID is the object a promote, delete or retarget is addressed to.
	UID types.UID
	// Revision is set for create, retarget and baseline.
	Revision string
	// Surge: the created group carries the surge mark.
	Surge  bool
	Reason string
}

// Input is everything Plan reads.
type Input struct {
	Spec   Spec
	Groups []Group
	// Orphans maps an ordinal to the ownerless member Pods on it. A group
	// whose ordinal is listed awaits adoption.
	Orphans map[int]Orphans
	// Foreign lists the ordinals whose name is taken by an object that is not ours.
	Foreign []int
}

// Class is the class of a group; every group has exactly one.
type Class string

const (
	ClassTerminating Class = "terminating"
	ClassFailed      Class = "failed"
	ClassMemberless  Class = "memberless"
	ClassReplaceable Class = "replaceable"
	ClassProtected   Class = "protected"
	ClassTarget      Class = "target"
)

// View is the classification and the counts of one set of groups.
type View struct {
	// Classes maps an ordinal to the class of its group.
	Classes map[int]Class
	// Active counts every group; Live the ones that are neither terminating nor failed.
	Active, Live int
	// Ready counts the live groups that are confirmed Ready.
	Ready int
	// Regular counts the groups without the surge mark, terminating and failed
	// ones included; RegularLive the live ones that do not await adoption.
	Regular, RegularLive int
	// Surge counts the live groups with the surge mark.
	Surge int
	// NotReadyTarget counts the live groups of the target revision that are
	// neither confirmed Ready nor replaceable.
	NotReadyTarget int
	// Floor is max(0, N - U).
	Floor int
	// MaxScaleDown is max(0, Live - Floor - NotReadyTarget).
	MaxScaleDown int
	// MaxHealthyScaleDown is max(0, Ready - Floor).
	MaxHealthyScaleDown int
	// Work: a replaceable regular group exists. Whether its place can be
	// filled, which Plan asks before it rolls, is not looked at here.
	Work bool
	// ScalingDown: Regular > N.
	ScalingDown bool
}
