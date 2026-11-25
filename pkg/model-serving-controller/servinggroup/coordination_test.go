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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The tests in this file are the tests of the rules of a coordinated Role
// rollout as they were written for the controller that applied them before
// groups were objects. They moved here with the rules, unchanged.

func TestCalculateRoleRolloutLimitsProportionalLimits(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 100, 0, 0),
		newCoordinatedRoleStateForTest("b", 50, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 90, "b": 45, "c": 9}, effectivePartitionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsDependencyStartupAndCompletion(t *testing.T) {
	coordination := coordinationForTest("20%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "a", DependsOn: []string{"b"}},
		{Role: "b", DependsOn: []string{"c"}},
	}

	t.Run("internal Roles prestart while entry Role is fully partitioned", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 0),
			newCoordinatedRoleStateForTest("b", 10, 0, 0),
			newCoordinatedRoleStateForTest("c", 10, 0, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 10, "b": 8, "c": 8}, effectivePartitionsByRole(decision))
	})

	t.Run("ready dependency closure unlocks entry Role", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 0),
			newCoordinatedRoleStateForTest("b", 10, 2, 0),
			newCoordinatedRoleStateForTest("c", 10, 2, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 8, "b": 8, "c": 8}, effectivePartitionsByRole(decision))
	})

	t.Run("an admitted root is not startup-gated again", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 0, 1),
			newCoordinatedRoleStateForTest("b", 10, 0, 0),
			newCoordinatedRoleStateForTest("c", 10, 0, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 8, decision.roles["a"].effectivePartition)
	})

	t.Run("dependency order is not a permanent progress order", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 2, 0),
			newCoordinatedRoleStateForTest("b", 10, 2, 0),
			newCoordinatedRoleStateForTest("c", 10, 2, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"a": 6, "b": 6, "c": 6}, effectivePartitionsByRole(decision))
	})

	t.Run("dependency retains one eligible old replica until dependents complete", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 8, 0),
			newCoordinatedRoleStateForTest("b", 10, 9, 0),
			newCoordinatedRoleStateForTest("c", 10, 9, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 1, decision.roles["b"].effectivePartition)
		assert.Equal(t, 1, decision.roles["c"].effectivePartition)
	})

	t.Run("completion releases dependency partitions in order", func(t *testing.T) {
		states := []coordinatedRoleState{
			newCoordinatedRoleStateForTest("a", 10, 10, 0),
			newCoordinatedRoleStateForTest("b", 10, 9, 0),
			newCoordinatedRoleStateForTest("c", 10, 9, 0),
		}

		decision, err := calculateRoleRolloutLimits(states, coordination)
		require.NoError(t, err)
		assert.Equal(t, 0, decision.roles["b"].effectivePartition)
		assert.Equal(t, 1, decision.roles["c"].effectivePartition)
	})
}

func TestCalculateRoleRolloutLimitsInfersRootFromSelectedGraph(t *testing.T) {
	coordination := coordinationForTest("20%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "b", DependsOn: []string{"c"}},
	}
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("b", 10, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 10, decision.roles["b"].effectivePartition)
	assert.Equal(t, 8, decision.roles["c"].effectivePartition)
	require.NotNil(t, decision.blocker)
	assert.Equal(t, coordinatedRoleDependencyNotReady, decision.blocker.reason)
}

func TestCalculateRoleRolloutLimitsCountsInFlightReservations(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 10, 0, 1),
		newCoordinatedRoleStateForTest("b", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 9, "b": 9}, effectivePartitionsByRole(decision))
	assert.Equal(t, map[string]int{"a": 0, "b": 1}, remainingDeletionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsUsesUserPartitionAsCompletionFloor(t *testing.T) {
	coordination := coordinationForTest("100%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{
		{Role: "a", DependsOn: []string{"b"}},
	}
	states := []coordinatedRoleState{
		{roleName: "a", userPartition: 1, totalToUpdate: 3, startedCount: 2, readyCount: 2, targetState: targetReady, hasOldVersion: true, inProgress: true, versionChanged: true, versionTotal: 4, versionStarted: 2, versionReady: 2},
		{roleName: "b", userPartition: 2, totalToUpdate: 2, startedCount: 1, readyCount: 1, targetState: targetReady, hasOldVersion: true, inProgress: true, versionChanged: true, versionTotal: 4, versionStarted: 1, versionReady: 1},
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 2, decision.roles["b"].effectivePartition)

	states[0].readyCount = states[0].totalToUpdate
	states[0].startedCount = states[0].totalToUpdate
	states[0].hasOldVersion = false
	states[0].inProgress = false
	decision, err = calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 2, decision.roles["b"].effectivePartition)
}

func TestCalculateRoleRolloutLimitsDoesNotBypassMaxSkewAtCompletion(t *testing.T) {
	coordination := coordinationForTest("5%")
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 100, 95, 0),
		newCoordinatedRoleStateForTest("b", 10, 9, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordination)
	require.NoError(t, err)
	assert.Equal(t, 5, decision.roles["a"].effectivePartition)
	assert.Zero(t, decision.roles["b"].effectivePartition)
	assert.Equal(t, 0, decision.roles["a"].remainingDeletions)
	assert.Equal(t, 1, decision.roles["b"].remainingDeletions)
}

func TestCalculateRoleRolloutLimitsKeepsQuantizationLocal(t *testing.T) {
	states := []coordinatedRoleState{
		newCoordinatedRoleStateForTest("a", 2, 0, 0),
		newCoordinatedRoleStateForTest("b", 4, 0, 0),
		newCoordinatedRoleStateForTest("c", 10, 0, 0),
	}

	decision, err := calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"a": 1, "b": 3, "c": 9}, effectivePartitionsByRole(decision))
}

func TestCalculateRoleRolloutLimitsRejectsInvalidMaxSkew(t *testing.T) {
	integerSkew := intstr.FromInt(10)
	_, err := calculateRoleRolloutLimits(
		[]coordinatedRoleState{newCoordinatedRoleStateForTest("a", 10, 0, 0)},
		&workloadv1alpha1.RoleCoordination{MaxSkew: &integerSkew},
	)
	require.ErrorContains(t, err, "percentage string")
}

func newCoordinatedRoleStateForTest(roleName string, total, ready, inFlight int) coordinatedRoleState {
	started := min(total, ready+inFlight)
	targetState := targetNotStarted
	if ready > 0 {
		targetState = targetReady
	} else if started > 0 {
		targetState = targetStarted
	}
	return coordinatedRoleState{
		roleName:      roleName,
		totalToUpdate: total,
		startedCount:  started,
		readyCount:    ready,
		targetState:   targetState,
		hasOldVersion: ready < total,
		inProgress:    total > 0 && ready < total,
		versionTotal:  total, versionReady: ready, versionStarted: started,
		versionOld: total - started, versionChanged: total > 0, tailReady: ready == started,
	}
}

func coordinationForTest(maxSkew string) *workloadv1alpha1.RoleCoordination {
	value := intstr.FromString(maxSkew)
	return &workloadv1alpha1.RoleCoordination{MaxSkew: &value}
}

func effectivePartitionsByRole(decision roleRolloutGroupPolicy) map[string]int {
	partitions := make(map[string]int, len(decision.roles))
	for roleName, limits := range decision.roles {
		partitions[roleName] = limits.effectivePartition
	}
	return partitions
}

func remainingDeletionsByRole(decision roleRolloutGroupPolicy) map[string]int {
	remaining := make(map[string]int, len(decision.roles))
	for roleName, limits := range decision.roles {
		remaining[roleName] = limits.remainingDeletions
	}
	return remaining
}

func TestCoordinatedVersionRatioIncludesPartitionAndCompletedRoles(t *testing.T) {
	for _, p := range []int{2, 4} {
		t.Run(string(rune('0'+p)), func(t *testing.T) {
			a := newCoordinatedRoleStateForTest("a", 4-p, 4-p, 0)
			a.userPartition, a.versionTotal, a.versionChanged = p, 4, true
			b := newCoordinatedRoleStateForTest("b", 4, 3, 0)
			policy, err := calculateRoleRolloutLimits([]coordinatedRoleState{a, b}, coordinationForTest("20%"))
			require.NoError(t, err)
			require.Zero(t, policy.roles["b"].remainingDeletions, "a remains a baseline even after its eligible work ends")
			require.Equal(t, p, policy.roles["a"].effectivePartition)
		})
	}
}

func TestCoordinatedFullTailRounding(t *testing.T) {
	coord := coordinationForTest("10%")
	coord.Dependencies = []workloadv1alpha1.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}, {Role: "decode", DependsOn: []string{"fff"}}}
	for _, tc := range []struct {
		name  string
		alter func([]coordinatedRoleState)
		allow bool
	}{
		{"ready full tail", func([]coordinatedRoleState) {}, true},
		{"target not ready", func(s []coordinatedRoleState) { s[1].versionReady--; s[1].tailReady = false }, false},
		{"in flight", func(s []coordinatedRoleState) { s[0].tailReady = false }, false},
		{"partitioned", func(s []coordinatedRoleState) { s[0].userPartition = 1 }, false},
		{"old temporary or unknown", func(s []coordinatedRoleState) { s[2].tailReady = false }, false},
		{"unchanged unavailable dependency", func(s []coordinatedRoleState) { s[2].versionChanged = false; s[2].tailReady = false }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			states := []coordinatedRoleState{newCoordinatedRoleStateForTest("prefill", 8, 7, 0), newCoordinatedRoleStateForTest("decode", 4, 3, 0), newCoordinatedRoleStateForTest("fff", 10, 9, 0)}
			tc.alter(states)
			policy, err := calculateRoleRolloutLimits(states, coord)
			require.NoError(t, err)
			if tc.allow {
				require.Equal(t, 1, policy.roles["prefill"].remainingDeletions)
				require.Zero(t, policy.roles["prefill"].effectivePartition)
			} else {
				require.Zero(t, policy.roles["prefill"].remainingDeletions)
			}
			require.True(t, policy.roles["decode"].retainOldReplica)
			require.True(t, policy.roles["fff"].retainOldReplica)
			require.Equal(t, 1, policy.roles["decode"].effectivePartition)
			require.GreaterOrEqual(t, policy.roles["fff"].effectivePartition, 1)
		})
	}
}

func TestAllowedTargetStartsDoesNotOverflowInt32ReplicaDomain(t *testing.T) {
	state := coordinatedRoleState{versionTotal: 2147483647}
	baseline := coordinatedRoleState{versionTotal: 2147483647, versionReady: 1073741823}
	require.Equal(t, 1288490188, allowedStartedReplicas(state, baseline, 10))
	require.Equal(t, 2147483647, allowedStartedReplicas(state, baseline, 100))
}

func TestFullTailReleasesDependenciesOnlyAfterCallerDisappears(t *testing.T) {
	coord := coordinationForTest("10%")
	coord.Dependencies = []workloadv1alpha1.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}, {Role: "b", DependsOn: []string{"c"}}}
	states := []coordinatedRoleState{newCoordinatedRoleStateForTest("a", 8, 8, 0), newCoordinatedRoleStateForTest("b", 4, 3, 0), newCoordinatedRoleStateForTest("c", 10, 9, 0)}
	policy, err := calculateRoleRolloutLimits(states, coord)
	require.NoError(t, err)
	require.Zero(t, policy.roles["b"].effectivePartition)
	require.False(t, policy.roles["b"].retainOldReplica)
	require.True(t, policy.roles["c"].retainOldReplica)
	states[1] = newCoordinatedRoleStateForTest("b", 4, 4, 0)
	policy, err = calculateRoleRolloutLimits(states, coord)
	require.NoError(t, err)
	require.Zero(t, policy.roles["c"].effectivePartition)
	require.Equal(t, 1, policy.roles["c"].remainingDeletions)

	// Without a retained dependency path there is no terminal cycle to break.
	states[0] = newCoordinatedRoleStateForTest("a", 8, 7, 0)
	states[1] = newCoordinatedRoleStateForTest("b", 4, 3, 0)
	policy, err = calculateRoleRolloutLimits(states, coordinationForTest("10%"))
	require.NoError(t, err)
	require.Zero(t, policy.roles["a"].remainingDeletions)
}
