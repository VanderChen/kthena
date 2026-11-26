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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Reasons of the CoordinatedRoleRolloutBlocked condition while it is false.
const (
	ReasonCoordinationProgressAvailable = "ProgressAvailable"
	ReasonCoordinationRolloutComplete   = "RolloutComplete"
)

// coordinatedReplica is what a coordinated rollout reads of one Role instance.
type coordinatedReplica struct {
	ordinal int
	// comparison is the template of the instance compared with the target. An
	// instance whose in-place update to the target has started is at the target.
	comparison workload.Comparison
	// ready: the instance is established and every member is ready. An instance
	// that is being updated in place is not.
	ready bool
	// deleting: the instance is being drained.
	deleting bool
}

// stableRolePopulation returns the ordinals a rollout of the Role counts: the
// range of the smaller of the previous and the desired replica count, with
// the identities that are kept above it in the place of its holes.
//
// Missing replacements reserve low holes. Temporary surge never participates
// in stable progress. Observed formal expansion members join the population;
// uncreated expansion slots do not reserve a template replacement start.
func stableRolePopulation(desired, previousDesired int, replicas []coordinatedReplica, terminating map[int]workload.Comparison, temporary sets.Set[int]) *roleStablePopulation {
	ordinals := sets.New[int]()
	for _, replica := range replicas {
		if replica.ordinal >= 0 && !temporary.Has(replica.ordinal) {
			ordinals.Insert(replica.ordinal)
		}
	}
	for ordinal := range terminating {
		if ordinal >= 0 && !temporary.Has(ordinal) {
			ordinals.Insert(ordinal)
		}
	}
	ordered := sets.List(ordinals)
	if len(ordered) > desired {
		ordered = ordered[:desired]
		ordinals = sets.New(ordered...)
	}
	population := &roleStablePopulation{end: min(previousDesired, desired), holes: sets.New[int]()}
	retainedOutsideDesired := 0
	for _, ordinal := range ordered {
		if ordinal >= population.end {
			population.extra = append(population.extra, ordinal)
		}
		if ordinal >= desired {
			retainedOutsideDesired++
		}
	}
	for ordinal := population.end - 1; population.holes.Len() < retainedOutsideDesired && ordinal >= 0; ordinal-- {
		if !ordinals.Has(ordinal) {
			population.holes.Insert(ordinal)
		}
	}
	return population
}

// resolveRoleRolloutState sums up the instances of one coordinated Role of a
// group into what calculateRoleRolloutLimits reads. terminating maps the
// ordinals that have a terminating Pod to how that Pod compares with the
// target: old capacity counts until its Pods are gone.
func resolveRoleRolloutState(
	roleName string,
	desired, previousDesired int,
	replicas []coordinatedReplica,
	partition int,
	templateChanged bool,
	terminatingReplicas map[int]workload.Comparison,
	stablePopulation *roleStablePopulation,
) coordinatedRoleState {
	if stablePopulation == nil {
		stablePopulation = &roleStablePopulation{end: min(previousDesired, desired)}
	}
	totalToUpdate := stablePopulation.countFrom(partition)
	protectedOld := stablePopulation.countFrom(0) > totalToUpdate
	stableTargetReady := 0
	hasTargetWork := false
	hasTargetReady := false
	hasCommittedReplacement := false
	remainingOldToUpdate := 0
	hasOldVersion := false
	versionReady, outsideEligibleStarted, versionOld, formalObserved, protectedObserved := 0, 0, 0, 0, 0
	tailReady := len(terminatingReplicas) == 0
	for _, replica := range replicas {
		ordinal := replica.ordinal
		if ordinal < 0 {
			continue
		}
		inStableRange := ordinal >= partition && stablePopulation.contains(ordinal)
		formal := stablePopulation.contains(ordinal) || (ordinal >= previousDesired && ordinal < desired)
		if formal {
			formalObserved++
		}
		if stablePopulation.contains(ordinal) && ordinal < partition {
			protectedObserved++
		}
		_, terminating := terminatingReplicas[ordinal]
		if inStableRange && (terminating || replica.deleting) {
			hasCommittedReplacement = true
		}
		running := replica.ready && !replica.deleting
		oldVersion := replica.comparison != workload.Equivalent
		if replica.comparison == workload.Unknown || !running || terminating {
			tailReady = false
		}
		if oldVersion {
			hasOldVersion = true
			if formal {
				versionOld++
			} else {
				// An old temporary/excess instance still carries an old path.
				tailReady = false
			}
			if !replica.deleting && !terminating && inStableRange {
				remainingOldToUpdate++
			}
			continue
		}
		if formal && !inStableRange {
			outsideEligibleStarted++
		}
		if replica.deleting || terminating || (ordinal >= desired && !stablePopulation.contains(ordinal)) {
			continue
		}
		hasTargetWork = true
		if running {
			hasTargetReady = true
			if formal {
				versionReady++
			}
			if inStableRange {
				stableTargetReady++
			}
		}
	}

	// Keep terminating old capacity in the dependency state until all its Pods
	// vanish.
	for ordinal, comparison := range terminatingReplicas {
		if ordinal < 0 {
			continue
		}
		if comparison != workload.Equivalent {
			hasOldVersion = true
		}
	}
	// A user partition preserves old-version stable slots. Keep the old-version
	// request path present even if a protected Role is temporarily absent while
	// its old template is being recovered.
	if templateChanged && protectedOld && protectedObserved < stablePopulation.countFrom(0)-totalToUpdate {
		hasOldVersion = true
	}

	startedCount := totalToUpdate - min(totalToUpdate, remainingOldToUpdate)
	readyCount := min(startedCount, stableTargetReady)
	targetState := targetNotStarted
	if hasTargetReady {
		targetState = targetReady
	} else if hasTargetWork || hasCommittedReplacement {
		targetState = targetStarted
	}

	return coordinatedRoleState{
		stablePopulation: stablePopulation,
		roleName:         roleName,
		userPartition:    partition,
		totalToUpdate:    totalToUpdate,
		startedCount:     startedCount,
		readyCount:       readyCount,
		targetState:      targetState,
		hasOldVersion:    hasOldVersion,
		inProgress:       templateChanged && totalToUpdate > 0 && readyCount < totalToUpdate,
		versionTotal:     desired,
		versionReady:     min(desired, versionReady),
		versionStarted:   min(desired, startedCount+outsideEligibleStarted),
		versionOld:       versionOld,
		versionChanged:   templateChanged,
		tailReady:        tailReady && formalObserved == desired && versionReady+versionOld == desired,
	}
}

// constrainRoleDeletion applies the limits of a coordinated Role to the old
// instances the rollout may replace. outdated holds their ordinals, highest
// first; it returns the prefix that may be replaced and how many of it.
func (l roleRolloutLimits) constrainRoleDeletion(outdated []int) (eligible []int, allowed int) {
	updateableOldCount := 0
	for _, ordinal := range outdated {
		if l.containsStable(ordinal) {
			updateableOldCount++
		}
	}
	for _, ordinal := range outdated {
		if ordinal < l.effectivePartition || !l.containsStable(ordinal) {
			// A blocked high ordinal must never be filtered away to expose a
			// lower candidate.
			break
		}
		eligible = append(eligible, ordinal)
	}
	allowed = l.remainingDeletions
	if l.retainOldReplica {
		allowed = min(allowed, max(updateableOldCount-1, 0))
	}
	return eligible, allowed
}

// Coordination is what a coordinated Role rollout says about one group in a
// round.
type Coordination struct {
	// Reason and Message say what keeps the rollout from advancing; both are
	// empty when it can advance or is complete.
	Reason, Message string
	// InProgress: a coordinated Role still has instances to bring to the target.
	InProgress bool
	// Deferred holds the Roles that may not start an instance of the target
	// revision yet: they wait for the Roles they depend on.
	Deferred sets.Set[string]
}

// Condition returns the CoordinatedRoleRolloutBlocked condition of the group.
// Its type, observed generation and transition time are set by ComputeStatus.
func (c *Coordination) Condition() *metav1.Condition {
	if c == nil {
		return nil
	}
	switch {
	case c.Reason != "":
		return &metav1.Condition{Status: metav1.ConditionTrue, Reason: c.Reason, Message: c.Message}
	case c.InProgress:
		return &metav1.Condition{Status: metav1.ConditionFalse, Reason: ReasonCoordinationProgressAvailable, Message: "Coordinated Role rollout can make progress"}
	}
	return &metav1.Condition{Status: metav1.ConditionFalse, Reason: ReasonCoordinationRolloutComplete, Message: "Coordinated Role rollout is complete"}
}

// roleCoordination returns the coordination settings of a group, or nil when
// its Roles are rolled independently.
func roleCoordination(group *workloadv1alpha1.ServingGroup) *workloadv1alpha1.RoleCoordination {
	strategy := group.Spec.RolloutStrategy
	if strategy == nil || strategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return nil
	}
	return strategy.RoleCoordination
}

// coordinatedRole is what the Role plan needs of the coordination of one Role
// of a group whose Roles are rolled together.
type coordinatedRole struct {
	// limits are the limits of a Role that takes part in the coordination; nil
	// for a Role that does not, or while there is nothing to coordinate. Such a
	// Role is still replaced in descending ordinal, like the others.
	limits *roleRolloutLimits
	// temporary holds the ordinals of marked surge instances beyond the
	// desired count.
	temporary sets.Set[int]
	// baselineReplicas is the replica count of the Role at the baseline
	// revision. A place at or above it is added capacity.
	baselineReplicas int
	// unknownOrdinal is the highest ordinal of a stable instance whose template
	// cannot be compared with the target, -1 when there is none. No instance at
	// or below it is replaced: it may be an old instance that has to go first.
	unknownOrdinal int
}

// coordinate computes the limits of the coordinated Roles of a group. specs
// holds the resolved rollout settings of every Role, instances the observed
// instances by Role.
//
// It returns nothing when the Roles of the group are rolled independently.
// Otherwise every Role gets an entry; one has limits when it takes part in the
// coordination and the baseline renders other Pods than the target. A revision
// or a baseline replica count that cannot be read is an error: nothing is
// rolled on a guess.
func coordinate(group *workloadv1alpha1.ServingGroup, specs map[string]grouplevel.Spec, instances map[string][]Instance, revisions Revisions) (map[string]*coordinatedRole, *Coordination, error) {
	coordination := roleCoordination(group)
	if coordination == nil {
		return nil, nil, nil
	}
	report := &Coordination{Deferred: sets.New[string]()}
	baseline, target := group.Spec.BaselineRevision, group.Spec.TargetRevision
	rolling := false
	switch revisions.Compare(baseline, target) {
	case workload.Different:
		rolling = true
	case workload.Unknown:
		return nil, nil, fmt.Errorf("coordinated Role rollout: the baseline revision %q cannot be compared with the target revision %q", baseline, target)
	}

	selected := sets.New(coordination.Roles...)
	var states []coordinatedRoleState
	roles := map[string]*coordinatedRole{}
	for _, role := range group.Spec.Roles {
		spec := specs[role.Name]
		participates := rolling && (selected.Len() == 0 || selected.Has(role.Name))
		previousDesired, templateChanged := 0, false
		if _, err := revisions.Role(baseline, role.Name); participates && err == nil {
			if role.BaselineReplicas == nil {
				return nil, nil, fmt.Errorf("coordinated Role rollout: the replica count of Role %s at the baseline revision %s is not known", role.Name, baseline)
			}
			previousDesired = int(*role.BaselineReplicas)
			switch revisions.CompareRole(baseline, target, role.Name) {
			case workload.Different:
				templateChanged = true
			case workload.Unknown:
				return nil, nil, fmt.Errorf("coordinated Role rollout: Role %s cannot be compared between revisions %s and %s", role.Name, baseline, target)
			}
		}

		coordinated := &coordinatedRole{temporary: sets.New[int](), unknownOrdinal: -1, baselineReplicas: previousDesired}
		var replicas []coordinatedReplica
		terminating := map[int]workload.Comparison{}
		for _, inst := range instances[role.Name] {
			replica, gone, temporary := observeReplica(inst, role.Name, target, spec, templateChanged, revisions)
			replicas = append(replicas, replica)
			if gone != nil {
				terminating[inst.Index] = *gone
			}
			if temporary && inst.Index >= int(role.Replicas) {
				coordinated.temporary.Insert(inst.Index)
			}
		}
		for _, replica := range replicas {
			if replica.comparison == workload.Unknown && !replica.deleting && !coordinated.temporary.Has(replica.ordinal) {
				coordinated.unknownOrdinal = max(coordinated.unknownOrdinal, replica.ordinal)
			}
		}
		roles[role.Name] = coordinated
		if participates {
			population := stableRolePopulation(int(role.Replicas), previousDesired, replicas, terminating, coordinated.temporary)
			states = append(states, resolveRoleRolloutState(role.Name, int(role.Replicas), previousDesired, replicas, spec.Partition, templateChanged, terminating, population))
		}
	}
	if !rolling {
		return roles, report, nil
	}

	policy, err := calculateRoleRolloutLimits(states, coordination)
	if err != nil {
		return nil, nil, fmt.Errorf("coordinated Role rollout: %w", err)
	}
	if _, conflict := utils.CoordinatedPartitionConflict(partitionView(group)); conflict != nil {
		// Preserve already-issued replacements, but do not select more old
		// stable instances for a legacy incompatible configuration.
		for name, limits := range policy.roles {
			limits.remainingDeletions = 0
			limits.effectivePartition = limits.rolloutEnd
			policy.roles[name] = limits
		}
		policy.blocker = &coordinatedRoleBlocker{reason: coordinatedRoleIncompatiblePartitions, message: conflict.Error()}
	}
	for name, limits := range policy.roles {
		roles[name].limits = &limits
		if !limits.allowTargetStart {
			report.Deferred.Insert(name)
		}
	}
	report.InProgress = policy.inProgress
	if policy.blocker != nil {
		report.Reason, report.Message = policy.blocker.reason, policy.blocker.message
	}
	return roles, report, nil
}

// observeReplica describes one instance to the coordination. gone, when not
// nil, says how the terminating Pods of the instance compare with the target.
// temporary reports that the instance carries the Role surge mark.
func observeReplica(inst Instance, role, target string, spec grouplevel.Spec, templateChanged bool, revisions Revisions) (replica coordinatedReplica, gone *workload.Comparison, temporary bool) {
	replica = coordinatedReplica{ordinal: inst.Index, deleting: inst.Phase() == workloadv1alpha1.RoleInstanceDraining}
	revision, compared := "", false
	for _, member := range inst.Members {
		pod := member.Pod
		temporary = temporary || pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole
		comparison := revisions.CompareRole(utils.ObjectRevision(pod), target, role)
		if pod.DeletionTimestamp != nil {
			// Every Pod must match before terminating capacity counts as target.
			if gone == nil || *gone == workload.Equivalent {
				gone = &comparison
			}
		}
		if revision == "" {
			revision = utils.ObjectRevision(pod)
		}
		switch {
		case !compared:
			replica.comparison, compared = comparison, true
		case comparison == workload.Different || replica.comparison == workload.Equivalent:
			replica.comparison = comparison
		}
	}
	switch {
	case !compared && !replica.deleting:
		// An instance without a Pod yet is created from the revision of its
		// place: the baseline below the partition, else the target. In a
		// group that was created at the target every place is one of the
		// target; the plan is told so by a baseline that is the target.
		replica.comparison = workload.Equivalent
		if templateChanged && inst.Index < spec.Partition && spec.Baseline != spec.Target {
			replica.comparison = workload.Different
		}
	case compared:
		if layout, err := revisions.Role(revision, role); err == nil {
			replica.ready = inst.Ready(layout.WorkerReplicas)
		}
	}
	if updating := unfinishedUpdate(inst.Members); updating != "" {
		// A patch reservation is a start, never Ready capacity.
		replica.ready = false
		if revisions.CompareRole(updating, target, role) == workload.Equivalent {
			replica.comparison = workload.Equivalent
		}
	}
	return replica, gone, temporary
}

// partitionView returns the settings of a group that
// utils.CoordinatedPartitionConflict reads, as a ModelServing.
func partitionView(group *workloadv1alpha1.ServingGroup) *workloadv1alpha1.ModelServing {
	view := &workloadv1alpha1.ModelServing{}
	view.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:             workloadv1alpha1.RoleRollingUpdate,
		RoleCoordination: roleCoordination(group),
	}
	for _, role := range group.Spec.Roles {
		replicas := role.Replicas
		view.Spec.Template.Roles = append(view.Spec.Template.Roles, workloadv1alpha1.Role{
			Name: role.Name, Replicas: &replicas,
			RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{Partition: role.Partition},
		})
	}
	return view
}
