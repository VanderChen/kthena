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
	"cmp"
	"fmt"
	"slices"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Revisions resolves the revisions a group refers to. workload.History
// implements it.
type Revisions interface {
	// At returns the ModelServing as of a revision.
	At(revision string) (*workloadv1alpha1.ModelServing, error)
	// Compare tells whether a group that runs revision is at target.
	Compare(revision, target string) workload.Comparison
	Role(revision, roleName string) (*workloadv1alpha1.Role, error)
	CompareRole(revision, target, roleName string) workload.Comparison
}

// RolePlan is what one round wants of the Role instances of a group.
type RolePlan struct {
	// Wants holds the instances that are to exist, the ones to create included.
	// An observed instance that is not in it is drained.
	Wants map[InstanceKey]*Want
	// Promote lists the instances that lose the Role surge mark: temporary
	// capacity that takes a regular place.
	Promote []InstanceKey
	// InPlace maps an instance to the revision it is updated to in place: an
	// instance the rollout chose in this round, and one whose update is under
	// way. The second kind is carried on whatever the budget and the partition
	// say now, so that an instance never stays patched in part.
	InPlace map[InstanceKey]string
	// Fallback says, for an instance that is replaced in this round although
	// its Role is updated in place, why it cannot be.
	Fallback map[InstanceKey]string
	// Waiting says, per Role, why a place is not filled or an instance is not
	// replaced. It is reported, not acted on.
	Waiting []string
	// Coordination is what the coordinated Role rollout says about the group;
	// nil when its Roles are rolled independently.
	Coordination *Coordination
}

// PlanRoles decides which Role instances of a group are to exist, from which
// revision each is created, and which of them a Role rolling update replaces.
//
// The Role level follows the rules of the group level, with the instances of
// one Role in one group in the place of the groups of a ModelServing: scaling
// in and out, filling holes, the rollout budget, partition, surge and its
// reclaim are the same plan. Under RoleRollingUpdate an instance is replaced
// when the template of its Role differs from the target revision. Under
// ServingGroupRollingUpdate instances are not compared: the group is replaced
// as a whole by the ModelServing controller, and the Role level only scales
// and fills.
func PlanRoles(group *workloadv1alpha1.ServingGroup, instances []Instance, revisions Revisions) (RolePlan, error) {
	plan := RolePlan{Wants: map[InstanceKey]*Want{}, InPlace: map[InstanceKey]string{}, Fallback: map[InstanceKey]string{}}
	byRole := map[string][]Instance{}
	for _, inst := range instances {
		if inst.Entry != nil || len(inst.Members) > 0 {
			byRole[inst.Role] = append(byRole[inst.Role], inst)
		}
	}
	strategy := group.Spec.RolloutStrategy
	roleMode := strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate

	// A group that says it was created from the target is taken at its word
	// only while every Pod it has is of the target.
	born := roleMode && createdAtTarget(group) && !holdsAnotherRevision(group, instances, revisions)
	specs := map[string]grouplevel.Spec{}
	for _, role := range group.Spec.Roles {
		spec, err := roleSpec(group, role, roleMode, born)
		if err != nil {
			return RolePlan{}, err
		}
		specs[role.Name] = spec
	}
	// The Roles of a group that are rolled together limit each other: how far
	// one may advance depends on how far the others are.
	coordinated, coordination, err := coordinate(group, specs, byRole, revisions)
	if err != nil {
		return RolePlan{}, err
	}
	plan.Coordination = coordination

	for _, role := range group.Spec.Roles {
		p := rolePlanner{
			role: role.Name, spec: specs[role.Name], revisions: revisions, plan: &plan,
			updating: map[int]string{}, fallback: map[int]string{}, coordinated: coordinated[role.Name],
		}
		p.run(byRole[role.Name])
	}
	return plan, nil
}

// roleSpec resolves the settings of one Role of a group into the input of the
// plan. born reports that the group was created new from its target and has
// nothing of another revision.
func roleSpec(group *workloadv1alpha1.ServingGroup, role workloadv1alpha1.ServingGroupRole, roleMode, born bool) (grouplevel.Spec, error) {
	replicas := int(role.Replicas)
	spec := grouplevel.Spec{
		Replicas: replicas,
		// Without a rolling update of its own, a Role is not limited.
		MaxUnavailable: replicas,
		Target:         group.Spec.Revision,
		Baseline:       group.Spec.BaselineRevision,
		Mode:           grouplevel.ModeRole,
	}
	if !roleMode {
		return spec, nil
	}
	spec.Mode = grouplevel.ModeServingGroup
	spec.Target = group.Spec.TargetRevision
	if born {
		// No instance of the group ran the baseline: a place below the
		// partition is filled from the target as well.
		spec.Baseline = spec.Target
	}
	scaled := func(name string, value *intstr.IntOrString, roundUp bool, into *int) error {
		if value == nil {
			return nil
		}
		resolved, err := intstr.GetScaledValueFromIntOrPercent(value, replicas, roundUp)
		if err != nil {
			return fmt.Errorf("%s of Role %s: %w", name, role.Name, err)
		}
		*into = resolved
		return nil
	}
	if err := scaled("maxUnavailable", role.MaxUnavailable, false, &spec.MaxUnavailable); err != nil {
		return spec, err
	}
	if err := scaled("maxSurge", role.MaxSurge, true, &spec.MaxSurge); err != nil {
		return spec, err
	}
	if err := scaled("partition", role.Partition, true, &spec.Partition); err != nil {
		return spec, err
	}
	// With no unavailable budget an in-place update could never start. The
	// Role is then rolled by recreation, which can use its surge.
	spec.InPlace = UpdatesInPlace(group) && (role.MaxUnavailable == nil || spec.MaxUnavailable > 0)
	return spec, nil
}

// createdAtTarget reports whether the group says that it was born in the
// rollout that is under way: the ModelServing controller created it new from
// the revision its Roles are rolled to. Such a group never ran the baseline.
// The partition of a Role keeps the instances below it at the baseline in a
// group that ran it; here they are created from the target like the others,
// in the round that creates them and in any later one of the same rollout:
// after a crash between the ledger and the Pods, and when such an instance is
// lost.
//
// The ModelServing controller keeps the annotation in line with the target:
// in the write that tells the group another target it takes it away, or,
// while the group has built nothing, makes it name that target. So the
// revision it names is the target of the group. The two are compared all the
// same: an annotation that names another revision says nothing about the
// rollout of now.
func createdAtTarget(group *workloadv1alpha1.ServingGroup) bool {
	created := group.Annotations[workloadv1alpha1.CreatedRevisionAnnotationKey]
	return created != "" && created == group.Spec.TargetRevision
}

// holdsAnotherRevision reports whether the group has a Pod that is not of its
// target. A group that says it was created from the target and has such a
// Pod was not: it is not taken at its word, and its Roles are planned like
// those of any group, with the baseline below the partition.
//
// The ModelServing controller makes a group say the new target only while the
// group has built nothing, and it can be wrong about that: the round of the
// group that committed to its Pods goes on to create them from the target of
// before, whatever is written to the group meanwhile. Taken at its word, such
// a group would hold those Pods below the partition of their Role and create
// everything else from the new target. Where Roles are rolled together and
// every instance is below the partition, the Role that others depend on would
// then stay at the target of before, and the instances that wait for it would
// wait for capacity of the new target, which never comes.
//
// A Pod is of the target when it carries the target revision, or a revision
// whose template of its Role renders the same Pods: a Role that did not
// change between the two. A Pod of a revision that cannot be compared is not
// known to be of the target, and counts against the group as well. That
// cannot happen to a group that was created from the target, whose Pods
// carry the target itself and need no comparison. Were it the other way
// round, the group would be taken at its word again on the day the revision
// of its Pods leaves the history.
//
// A Pod that is terminating does not count: the group does not hold what is
// on its way out. When the last Pod of another revision goes, the group has
// only Pods of the target, and is what it says.
func holdsAnotherRevision(group *workloadv1alpha1.ServingGroup, instances []Instance, revisions Revisions) bool {
	target := group.Spec.TargetRevision
	for _, inst := range instances {
		for _, member := range inst.Members {
			if member.Pod.DeletionTimestamp != nil {
				continue
			}
			revision := utils.ObjectRevision(member.Pod)
			if revision != target && revisions.CompareRole(revision, target, inst.Role) != workload.Equivalent {
				return true
			}
		}
	}
	return false
}

type rolePlanner struct {
	role      string
	spec      grouplevel.Spec
	revisions Revisions
	plan      *RolePlan
	// updating maps the index of an instance whose in-place update is under
	// way to the revision it is updated to.
	updating map[int]string
	// fallback maps the index of an instance that cannot be updated in place
	// to the reason.
	fallback map[int]string
	// coordinated holds the limits of a Role that is rolled together with
	// other Roles of the group; nil for a Role that is rolled on its own.
	coordinated *coordinatedRole
}

func (p *rolePlanner) run(instances []Instance) {
	units := make([]grouplevel.Group, 0, len(instances))
	settled := make([]grouplevel.Group, 0, len(instances))
	awaiting := map[int]grouplevel.Orphans{}
	own := map[int]string{}
	for _, inst := range instances {
		unit, revision, adoptable := p.unit(inst)
		units = append(units, unit)
		own[inst.Index] = revision
		if adoptable {
			awaiting[inst.Index] = grouplevel.Orphans{}
		}
		if !unit.Terminating {
			settled = append(settled, unit)
		}
	}

	removed := map[int]bool{}
	promoted := map[int]bool{}
	retargeted := map[int]string{}
	for _, action := range p.constrain(grouplevel.Plan(grouplevel.Input{Spec: p.spec, Groups: units, Orphans: awaiting}), units) {
		key := InstanceKey{Role: p.role, Index: action.Ordinal}
		switch action.Kind {
		case grouplevel.ActionCreate:
			p.want(key, action.Revision, action.Surge)
		case grouplevel.ActionDelete:
			removed[action.Ordinal] = true
			if reason, ok := p.fallback[action.Ordinal]; ok && (action.Reason == grouplevel.ReasonReplace || action.Reason == grouplevel.ReasonReplaceNotReady) {
				p.plan.Fallback[key] = reason
			}
		case grouplevel.ActionPromote:
			p.plan.Promote = append(p.plan.Promote, key)
			promoted[action.Ordinal] = true
		case grouplevel.ActionRetarget:
			if action.Reason == grouplevel.ReasonInPlace {
				p.plan.InPlace[key] = action.Revision
			} else {
				retargeted[action.Ordinal] = action.Revision
			}
		case grouplevel.ActionWait:
			p.plan.Waiting = append(p.plan.Waiting, fmt.Sprintf("Role %s, instance %d: %s", p.role, action.Ordinal, action.Reason))
		}
	}

	for index, revision := range p.updating {
		key := InstanceKey{Role: p.role, Index: index}
		if _, chosen := p.plan.InPlace[key]; !chosen && !removed[index] {
			p.plan.InPlace[key] = revision
		}
	}

	// An instance that is being drained is wanted when its place is filled
	// again once it is gone. The plan that tells is the one without it.
	refill := map[int]grouplevel.Action{}
	for _, action := range grouplevel.Plan(grouplevel.Input{Spec: p.spec, Groups: settled, Orphans: awaiting}) {
		if action.Kind == grouplevel.ActionCreate && p.mayStart(action.Revision) {
			refill[action.Ordinal] = action
		}
	}

	for _, unit := range units {
		key := InstanceKey{Role: p.role, Index: unit.Ordinal}
		switch {
		case removed[unit.Ordinal]:
		case unit.Terminating:
			if action, ok := refill[unit.Ordinal]; ok {
				p.want(key, action.Revision, action.Surge)
			}
		case unit.Memberless:
			revision, ok := retargeted[unit.Ordinal]
			if !ok && p.spec.Mode == grouplevel.ModeRole {
				revision = p.spec.Target
			}
			if p.mayStart(revision) {
				p.want(key, revision, unit.Surge && !promoted[unit.Ordinal])
			}
		default:
			p.want(key, own[unit.Ordinal], unit.Surge && !promoted[unit.Ordinal])
		}
	}
}

// mayStart tells whether an instance may be created from revision now. A
// coordinated Role that waits for the Roles it depends on starts no instance
// of the target revision; instances of the baseline are not held back.
func (p *rolePlanner) mayStart(revision string) bool {
	return p.coordinated == nil || p.coordinated.limits == nil || p.coordinated.limits.allowTargetStart || revision != p.spec.Target
}

// constrain applies the limits of a coordinated Role to the writes of its
// plan. The plan has chosen what the budget of the Role allows; coordination
// allows less. It drops the creation of target instances while the Role may
// not start, and of the replacements, by deletion or in place, all but a
// prefix of the old instances in descending ordinal: the instances at or
// above the effective partition, as many as the Role may still start. An
// instance that the plan did not choose ends the prefix, so a lower ordinal
// is never replaced past a higher one that has to wait.
//
// Temporary capacity, marked surge beyond the desired count, is not part of
// that order: an old temporary instance goes when the budget allows, unless
// it is the last old instance and a Role that depends on this one still runs
// the old revision.
func (p *rolePlanner) constrain(actions []grouplevel.Action, units []grouplevel.Group) []grouplevel.Action {
	c := p.coordinated
	if c == nil {
		return actions
	}
	// Reclaiming a marked instance of an old revision is a replacement too:
	// it takes old capacity away.
	replaces := func(action grouplevel.Action) bool {
		switch action.Kind {
		case grouplevel.ActionDelete:
			return action.Reason == grouplevel.ReasonReplace || action.Reason == grouplevel.ReasonReplaceNotReady || action.Reason == grouplevel.ReasonReclaimSurge
		case grouplevel.ActionRetarget:
			return action.Reason == grouplevel.ReasonInPlace
		}
		return false
	}
	// Capacity that this round adds from the target is started as well. The
	// limits do not count it before it exists, and its creation is not held
	// back by them: it takes from what the old instances may start. A hole
	// among the places the baseline had is counted by the limits already.
	planned, added := sets.New[int](), 0
	for _, action := range actions {
		switch {
		case replaces(action):
			planned.Insert(action.Ordinal)
		case action.Kind == grouplevel.ActionCreate && !action.Surge && action.Revision == p.spec.Target &&
			action.Ordinal >= c.baselineReplicas && p.mayStart(action.Revision):
			added++
		}
	}
	// The old instances the rollout may replace, highest ordinal first.
	var outdated []int
	for _, unit := range units {
		if !unit.Terminating && !unit.Memberless && !unit.RevisionUnknown && unit.Revision != p.spec.Target && unit.Ordinal >= p.spec.Partition {
			outdated = append(outdated, unit.Ordinal)
		}
	}
	slices.SortFunc(outdated, func(a, b int) int { return cmp.Compare(b, a) })

	keep := sets.New[int]()
	remainingOld := len(outdated)
	var stable []int
	for _, ordinal := range outdated {
		switch {
		case !c.temporary.Has(ordinal):
			stable = append(stable, ordinal)
		case c.limits != nil && c.limits.retainOldReplica && remainingOld <= 1:
		case planned.Has(ordinal):
			keep.Insert(ordinal)
			remainingOld--
		}
	}
	for i, ordinal := range stable {
		if ordinal <= c.unknownOrdinal {
			stable = stable[:i]
			break
		}
	}
	eligible, allowed := stable, len(stable)
	if c.limits != nil {
		eligible, allowed = c.limits.constrainRoleDeletion(stable)
		allowed -= added
	}
	for _, ordinal := range eligible {
		if allowed <= 0 || !planned.Has(ordinal) {
			break
		}
		keep.Insert(ordinal)
		allowed--
	}

	old := sets.New(outdated...)
	kept := actions[:0:0]
	for _, action := range actions {
		switch {
		case replaces(action) && old.Has(action.Ordinal) && !keep.Has(action.Ordinal):
		case action.Kind == grouplevel.ActionCreate && !p.mayStart(action.Revision):
		default:
			kept = append(kept, action)
		}
	}
	return kept
}

// want records that an instance is to exist, created from revision.
func (p *rolePlanner) want(key InstanceKey, revision string, surge bool) {
	want := &Want{Revision: revision, Surge: surge}
	if role, err := p.revisions.Role(revision, p.role); err == nil {
		want.Workers, want.LayoutKnown = role.WorkerReplicas, true
	}
	p.plan.Wants[key] = want
}

// unit describes one Role instance to the plan. revision is the revision its
// members were created from; adoptable reports that it has members the group
// still has to adopt.
func (p *rolePlanner) unit(inst Instance) (unit grouplevel.Group, revision string, adoptable bool) {
	unit = grouplevel.Group{
		Ordinal:     inst.Index,
		UID:         types.UID(utils.GenerateRoleID(inst.Role, inst.Index)),
		Terminating: inst.Phase() == workloadv1alpha1.RoleInstanceDraining,
		Observed:    true,
		Fresh:       true,
		Memberless:  true,
		PodsReady:   true,
	}
	for _, member := range inst.Members {
		if member.Membership != Owned {
			adoptable = true
			continue
		}
		unit.Memberless = false
		unit.HasPods = unit.HasPods || member.Pod.DeletionTimestamp == nil
		unit.Surge = unit.Surge || member.Pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole
		unit.PodsReady = unit.PodsReady && member.State == MemberReady
		unit.DeletionCost += deletionCost(member.Pod)

		podRevision := utils.ObjectRevision(member.Pod)
		if revision == "" {
			revision = podRevision
		}
		if p.spec.Mode != grouplevel.ModeServingGroup {
			unit.Revision = revision
			continue
		}
		// The instance is at the target only when every member is. A template
		// that renders the same Pods as the target is the target, under whatever
		// identity it was recorded.
		switch p.revisions.CompareRole(podRevision, p.spec.Target, p.role) {
		case workload.Equivalent:
			if unit.Revision == "" {
				unit.Revision = p.spec.Target
			}
		case workload.Different:
			if unit.Revision == "" || unit.Revision == p.spec.Target {
				unit.Revision = podRevision
			}
		default:
			unit.RevisionUnknown = true
			unit.Revision = podRevision
		}
	}
	unit.PodsReady = unit.PodsReady && !unit.Memberless
	if role, err := p.revisions.Role(revision, p.role); err == nil {
		unit.Ready = inst.Ready(role.WorkerReplicas)
	}
	if p.spec.Mode == grouplevel.ModeServingGroup && !unit.Terminating && !unit.Memberless {
		p.inPlace(inst, &unit)
	}
	return unit, revision, adoptable
}

// inPlace describes the in-place update of an instance to the plan.
//
// An instance whose update is under way counts as the revision it is being
// updated to and is not ready until the labels of its Pods have changed, so it
// keeps its share of the budget for as long as the update takes. An instance
// that cannot be updated in place asks to be recreated: one whose Pods cannot
// be patched to the target, one whose update cannot be carried on, and one
// whose Role went back to the template its Pods are labelled with after their
// images were patched for another revision.
func (p *rolePlanner) inPlace(inst Instance, unit *grouplevel.Group) {
	target := p.spec.Target
	check := func(revision string, allowed bool) (obstacle bool) {
		step := PlanInPlace(InPlaceUnit{Role: p.role, Instances: []Instance{inst}, Target: revision, Allowed: allowed}, p.revisions)
		if step.Obstacle == "" {
			return false
		}
		unit.RecreateRequired = true
		if step.Fallback {
			p.fallback[inst.Index] = step.Obstacle
		}
		return true
	}

	if updating := unfinishedUpdate(inst.Members); updating != "" {
		towards := p.revisions.CompareRole(updating, target, p.role)
		if towards == workload.Different && unit.Revision == target && !unit.RevisionUnknown {
			// By their labels the Pods are at the target: no rollout would
			// ever patch them back.
			unit.RecreateRequired = true
			return
		}
		if check(updating, p.spec.InPlace) {
			return
		}
		unit.Ready = false
		unit.Revision, unit.RevisionUnknown = updating, false
		switch towards {
		case workload.Equivalent:
			unit.Revision = target
		case workload.Unknown:
			unit.RevisionUnknown = true
		}
		p.updating[inst.Index] = updating
	}
	// Whether the instance can be patched is asked only where the plan would
	// replace it for its revision: a protected instance must not become
	// replaceable by asking.
	if p.spec.InPlace && unit.Revision != target && !unit.RevisionUnknown && inst.Index >= p.spec.Partition {
		check(target, true)
	}
}

func deletionCost(pod *corev1.Pod) int64 {
	cost, err := strconv.ParseInt(pod.Annotations[corev1.PodDeletionCost], 10, 64)
	if err != nil {
		return 0
	}
	return cost
}
