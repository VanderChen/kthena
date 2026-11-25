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
	"math/bits"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// This file holds the rules of a coordinated Role rollout
// (rolloutStrategy.roleCoordination): from what is known of every coordinated
// Role of one group they compute how far each Role may advance in this round.
// They are the rules the ModelServing controller applied before groups were
// objects, moved here unchanged; coordination_state.go computes their input.

// coordinatedRoleState is a per-ServingGroup snapshot used to calculate a
// temporary effective partition for a coordinated Role rollout. The user
// partition remains the rollout target; effective partitions only limit how
// far the current reconcile may advance.
type coordinatedRoleState struct {
	roleName         string
	stablePopulation *roleStablePopulation

	userPartition int
	totalToUpdate int
	startedCount  int
	readyCount    int
	targetState   targetVersionState
	hasOldVersion bool
	inProgress    bool
	// Version capacity is independent of the partition-limited work ledger.
	versionTotal   int
	versionReady   int
	versionStarted int
	versionOld     int
	versionChanged bool
	tailReady      bool
}

type targetVersionState uint8

const (
	targetNotStarted targetVersionState = iota
	targetStarted
	targetReady
)

type coordinatedRoleBlocker struct {
	reason  string
	message string
}

// roleRolloutLimits is the small execution contract consumed by the existing
// Role creation and deletion paths.
type roleRolloutLimits struct {
	stablePopulation   *roleStablePopulation
	rolloutEnd         int
	effectivePartition int
	remainingDeletions int
	allowTargetStart   bool
	retainOldReplica   bool
}

func (l roleRolloutLimits) containsStable(ordinal int) bool {
	if l.stablePopulation != nil {
		return l.stablePopulation.contains(ordinal)
	}
	return ordinal >= 0 && ordinal < l.rolloutEnd
}

// roleRolloutGroupPolicy contains the Role limits derived from one immutable
// ServingGroup snapshot.
type roleRolloutGroupPolicy struct {
	roles      map[string]roleRolloutLimits
	blocker    *coordinatedRoleBlocker
	inProgress bool
}

// Reasons of the CoordinatedRoleRolloutBlocked condition.
const (
	coordinatedRoleDependencyNotReady          = "DependencyNotReady"
	coordinatedRoleMaxSkewLimitReached         = "MaxSkewLimitReached"
	coordinatedRoleOldVersionDependencyPresent = "OldVersionDependencyPresent"
	coordinatedRoleIncompatiblePartitions      = "IncompatiblePartitions"
)

func parseMaxSkewPercent(value *intstr.IntOrString) (int, error) {
	if value == nil || value.Type != intstr.String || !strings.HasSuffix(value.StrVal, "%") {
		return 0, fmt.Errorf("maxSkew must be a percentage string")
	}
	percent, err := strconv.Atoi(strings.TrimSuffix(value.StrVal, "%"))
	if err != nil || percent <= 0 || percent > 100 {
		return 0, fmt.Errorf("maxSkew must be greater than 0%% and no greater than 100%%")
	}
	return percent, nil
}

// calculateRoleRolloutLimits converts dependency and maxSkew constraints into
// per-Role limits without mutating the user-supplied partition.
func calculateRoleRolloutLimits(
	states []coordinatedRoleState,
	coordination *workloadv1alpha1.RoleCoordination,
) (roleRolloutGroupPolicy, error) {
	if coordination == nil {
		return roleRolloutGroupPolicy{}, nil
	}
	maxSkewPercent, err := parseMaxSkewPercent(coordination.MaxSkew)
	if err != nil {
		return roleRolloutGroupPolicy{}, err
	}

	stateByName := make(map[string]*coordinatedRoleState, len(states))
	for i := range states {
		stateByName[states[i].roleName] = &states[i]
	}
	dependencies := make(map[string][]string, len(coordination.Dependencies))
	dependents := make(map[string][]string, len(coordination.Dependencies))
	for _, dependency := range coordination.Dependencies {
		dependencies[dependency.Role] = append(dependencies[dependency.Role], dependency.DependsOn...)
		for _, dependencyRole := range dependency.DependsOn {
			dependents[dependencyRole] = append(dependents[dependencyRole], dependency.Role)
		}
	}

	baseline := slowestReadyProgress(states)
	progressingRoleCount := 0
	for i := range states {
		if states[i].versionChanged && states[i].versionTotal > 0 {
			progressingRoleCount++
		}
	}
	terminal := fullRoleRolloutTailReady(states)
	policy := roleRolloutGroupPolicy{
		roles: make(map[string]roleRolloutLimits, len(states)),
	}
	for i := range states {
		state := &states[i]
		policy.inProgress = policy.inProgress || state.inProgress
		var dependencyBlocker, retentionBlocker, skewBlocker *coordinatedRoleBlocker
		limits := roleRolloutLimits{
			stablePopulation:   state.stablePopulation,
			rolloutEnd:         state.userPartition + state.totalToUpdate,
			effectivePartition: state.userPartition,
			allowTargetStart:   true,
		}

		// Only rollout roots wait for the dependency closure on their first
		// target-version start. Internal Roles may start together, allowing the
		// target-version dependency chain to become Ready from the bottom up.
		if state.targetState == targetNotStarted && len(dependents[state.roleName]) == 0 {
			unready := unreadyDependencyClosure(state.roleName, stateByName, dependencies)
			if len(unready) > 0 {
				limits.allowTargetStart = false
				limits.effectivePartition = state.userPartition + state.totalToUpdate
				dependencyBlocker = &coordinatedRoleBlocker{
					reason:  coordinatedRoleDependencyNotReady,
					message: fmt.Sprintf("root Role %s is waiting for target-version RoleRunning capacity from Roles %s", state.roleName, strings.Join(unready, ", ")),
				}
			}
		}

		if state.inProgress && state.totalToUpdate > 0 && limits.allowTargetStart {
			allowedStarted := state.versionTotal
			if progressingRoleCount > 1 {
				allowedStarted = allowedStartedReplicas(*state, baseline, maxSkewPercent)
			}
			limits.remainingDeletions = max(allowedStarted-state.versionStarted, 0)
			if terminal && limits.remainingDeletions == 0 && state.versionOld == 1 &&
				len(oldDirectDependents(state.roleName, stateByName, dependents)) == 0 {
				for _, name := range dependencies[state.roleName] {
					if dependency := stateByName[name]; dependency != nil && dependency.hasOldVersion {
						limits.remainingDeletions = 1
						klog.V(2).InfoS("Allowing one full-rollout terminal rounding step", "role", state.roleName,
							"targetReady", state.versionReady, "total", state.versionTotal, "maxSkew", maxSkewPercent)
						break
					}
				}
			}
			allowedEligible := min(state.totalToUpdate, state.startedCount+limits.remainingDeletions)
			limits.effectivePartition = state.userPartition + state.totalToUpdate - allowedEligible
			if limits.remainingDeletions == 0 && state.startedCount < state.totalToUpdate {
				skewBlocker = &coordinatedRoleBlocker{
					reason: coordinatedRoleMaxSkewLimitReached,
					message: fmt.Sprintf("Role %s has started %d of %d formal target replicas and reached the current maxSkew allowance of %d",
						state.roleName, state.versionStarted, state.versionTotal, allowedStarted),
				}
			}
		}

		// Keep one old dependency while a direct old-version caller remains.
		// A user partition of at least one already provides this boundary.
		oldDependents := oldDirectDependents(state.roleName, stateByName, dependents)
		if state.hasOldVersion && state.userPartition == 0 && len(oldDependents) > 0 {
			limits.retainOldReplica = true
			retentionIsBinding := limits.effectivePartition == 0
			limits.effectivePartition = max(limits.effectivePartition, 1)
			if retentionIsBinding {
				retentionBlocker = &coordinatedRoleBlocker{
					reason: coordinatedRoleOldVersionDependencyPresent,
					message: fmt.Sprintf("Role %s is retaining an old replica while old dependent Roles %s still exist",
						state.roleName, strings.Join(oldDependents, ", ")),
				}
			}
		}
		// Convert the count boundary to an actual ordinal after skew and
		// dependencies. A sparse ordinal is not a replica count.
		if state.stablePopulation != nil {
			limits.rolloutEnd = state.stablePopulation.upperBound()
			offset := max(0, limits.effectivePartition-state.userPartition)
			limits.effectivePartition = state.stablePopulation.boundary(state.userPartition, offset)
		}
		policy.roles[state.roleName] = limits
		if policy.blocker == nil {
			for _, blocker := range []*coordinatedRoleBlocker{dependencyBlocker, retentionBlocker, skewBlocker} {
				if blocker != nil {
					policy.blocker = blocker
					break
				}
			}
		}
	}
	return policy, nil
}

func slowestReadyProgress(states []coordinatedRoleState) coordinatedRoleState {
	baseline := coordinatedRoleState{versionReady: 1, versionTotal: 1}
	found := false
	for _, state := range states {
		if !state.versionChanged || state.versionTotal == 0 {
			continue
		}
		if !found || int64(state.versionReady)*int64(baseline.versionTotal) < int64(baseline.versionReady)*int64(state.versionTotal) {
			baseline = state
			found = true
		}
	}
	return baseline
}

func allowedStartedReplicas(state, baseline coordinatedRoleState, maxSkewPercent int) int {
	if state.versionTotal <= 0 {
		return 0
	}
	if baseline.versionTotal <= 0 {
		return state.versionTotal
	}
	progress := uint64(baseline.versionReady)*100 + uint64(maxSkewPercent)*uint64(baseline.versionTotal)
	denominator := uint64(baseline.versionTotal) * 100
	if progress >= denominator {
		return state.versionTotal
	}
	// The product can exceed int64 for legal int32 replica counts.
	hi, lo := bits.Mul64(progress, uint64(state.versionTotal))
	allowed, remainder := bits.Div64(hi, lo, denominator)
	if remainder != 0 {
		allowed++
	}
	return int(allowed)
}

func fullRoleRolloutTailReady(states []coordinatedRoleState) bool {
	participants := 0
	for _, state := range states {
		if state.versionTotal == 0 {
			continue
		}
		// Unchanged templates do not constrain version progress, but their
		// unavailable or unknown capacity cannot justify a terminal exception.
		if !state.tailReady {
			return false
		}
		if !state.versionChanged {
			continue
		}
		participants++
		if state.userPartition != 0 || state.versionOld > 1 ||
			state.versionReady+state.versionOld != state.versionTotal || state.versionStarted != state.versionReady {
			return false
		}
	}
	return participants > 1
}

func oldDirectDependents(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependents map[string][]string,
) []string {
	var oldDependents []string
	for _, dependentRole := range dependents[roleName] {
		dependent := states[dependentRole]
		if dependent != nil && dependent.hasOldVersion {
			oldDependents = append(oldDependents, dependentRole)
		}
	}
	return oldDependents
}

func unreadyDependencyClosure(
	roleName string,
	states map[string]*coordinatedRoleState,
	dependencies map[string][]string,
) []string {
	visited := make(map[string]bool)
	reported := make(map[string]bool)
	var result []string
	var visit func(string)
	visit = func(current string) {
		if visited[current] {
			return
		}
		visited[current] = true
		for _, dependencyRole := range dependencies[current] {
			dependency := states[dependencyRole]
			if (dependency == nil || dependency.targetState != targetReady) && !reported[dependencyRole] {
				reported[dependencyRole] = true
				result = append(result, dependencyRole)
			}
			visit(dependencyRole)
		}
	}
	visit(roleName)
	return result
}

// A stable population is the ordinary range with sparse retained identities
// replacing holes. Store only deviations, not O(desiredReplicas) virtual slots.
type roleStablePopulation struct {
	end   int
	holes sets.Set[int]
	extra []int
}

func (p *roleStablePopulation) contains(ordinal int) bool {
	return (ordinal >= 0 && ordinal < p.end && !p.holes.Has(ordinal)) || slices.Contains(p.extra, ordinal)
}

func (p *roleStablePopulation) countFrom(partition int) int {
	count := max(0, p.end-partition)
	for ordinal := range p.holes {
		if ordinal >= partition {
			count--
		}
	}
	for _, ordinal := range p.extra {
		if ordinal >= partition {
			count++
		}
	}
	return count
}

func (p *roleStablePopulation) upperBound() int {
	if len(p.extra) > 0 {
		return p.extra[len(p.extra)-1] + 1
	}
	return p.end
}

func (p *roleStablePopulation) boundary(partition, offset int) int {
	if offset >= p.countFrom(partition) {
		return p.upperBound()
	}
	candidate := partition + offset
	baseCount := max(0, p.end-partition)
	for _, hole := range sets.List(p.holes) {
		if hole < partition {
			continue
		}
		baseCount--
		if hole <= candidate {
			candidate++
		}
	}
	if offset < baseCount {
		return candidate
	}
	offset -= baseCount
	for _, ordinal := range p.extra {
		if ordinal < partition {
			continue
		}
		if offset == 0 {
			return ordinal
		}
		offset--
	}
	return p.upperBound()
}
