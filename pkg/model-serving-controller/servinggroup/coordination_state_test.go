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
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// The tests of resolveRoleRolloutState and stableRolePopulation below are the
// ones written for the controller that applied these rules before groups were
// objects, with the same instances and the same expectations. There an
// instance was a record with a template hash and a status; here it is a
// coordinatedReplica: "old" is workload.Different, the target hash
// workload.Equivalent, a revision that cannot be read workload.Unknown;
// Running is ready, Creating is not, Deleting is deleting.
const (
	oldTemplate     = workload.Different
	targetTemplate  = workload.Equivalent
	unknownTemplate = workload.Unknown
)

func runningReplica(ordinal int, comparison workload.Comparison) coordinatedReplica {
	return coordinatedReplica{ordinal: ordinal, comparison: comparison, ready: true}
}

func creatingReplica(ordinal int, comparison workload.Comparison) coordinatedReplica {
	return coordinatedReplica{ordinal: ordinal, comparison: comparison}
}

func TestResolveRoleRolloutStateUsesPartitionAndReservesStartedSlots(t *testing.T) {
	// a-3 has completed deletion but has not yet been recorded as Creating.
	replicas := []coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(1, oldTemplate), runningReplica(2, targetTemplate)}
	state := resolveRoleRolloutState("a", 4, 4, replicas, 2, true, nil, nil)
	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 2, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.Equal(t, targetReady, state.targetState)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateUsesExactRolloutOrdinalRange(t *testing.T) {
	replicas := []coordinatedReplica{
		runningReplica(0, targetTemplate), // Below partition.
		runningReplica(1, oldTemplate),
		runningReplica(2, targetTemplate),
		runningReplica(3, oldTemplate),
		runningReplica(7, targetTemplate), // Scale-down excess.
	}
	state := resolveRoleRolloutState("a", 4, 4, replicas, 2, true, nil, nil)
	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 1, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateReconstructsTerminatingOldReplica(t *testing.T) {
	state := resolveRoleRolloutState("a", 2, 2, []coordinatedReplica{runningReplica(0, targetTemplate)}, 0, true,
		map[int]workload.Comparison{
			1: oldTemplate,
			4: oldTemplate, // Scale-down excess is ignored.
		}, nil)
	assert.Equal(t, 2, state.totalToUpdate)
	assert.Equal(t, 2, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateDoesNotReserveUnadmittedScaleUp(t *testing.T) {
	state := resolveRoleRolloutState("a", 4, 2, []coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(1, oldTemplate)}, 0, true, nil, nil)
	assert.Equal(t, 2, state.totalToUpdate)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState)
}

func TestResolveRoleRolloutStateTracksReplacementReadyIndependentlyOfScaleUp(t *testing.T) {
	tests := []struct {
		name       string
		replicas   []coordinatedReplica
		readyCount int
	}{
		{name: "replacement becomes Ready before scale-up", readyCount: 1,
			replicas: []coordinatedReplica{runningReplica(0, targetTemplate), runningReplica(1, oldTemplate), creatingReplica(2, targetTemplate)}},
		{name: "scale-up becomes Ready before replacement", readyCount: 0,
			replicas: []coordinatedReplica{creatingReplica(0, targetTemplate), runningReplica(1, oldTemplate), runningReplica(2, targetTemplate)}},
		{name: "replacement and scale-up are both Ready", readyCount: 1,
			replicas: []coordinatedReplica{runningReplica(0, targetTemplate), runningReplica(1, oldTemplate), runningReplica(2, targetTemplate)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := resolveRoleRolloutState("a", 3, 2, tt.replicas, 0, true, nil, nil)
			assert.Equal(t, 2, state.totalToUpdate)
			assert.Equal(t, 1, state.startedCount)
			assert.Equal(t, tt.readyCount, state.readyCount)
			assert.Equal(t, targetReady, state.targetState)
		})
	}
}

func TestResolveRoleRolloutStateIgnoresScaleDownExcessForReadyState(t *testing.T) {
	state := resolveRoleRolloutState("a", 2, 2,
		[]coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(1, oldTemplate), runningReplica(2, targetTemplate)}, 0, true, nil, nil)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState)

	state = resolveRoleRolloutState("a", 2, 2,
		[]coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(2, targetTemplate)}, 0, true, nil, nil)
	assert.Equal(t, 1, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetNotStarted, state.targetState, "a missing formal slot does not prove dependency admission; excess target capacity cannot open the gate")
}

func TestResolveRoleRolloutStateUsesOrdinaryScaleUpForDependencyReadinessOnly(t *testing.T) {
	state := resolveRoleRolloutState("a", 3, 2,
		[]coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(1, oldTemplate), runningReplica(2, targetTemplate)}, 0, true, nil, nil)
	assert.Equal(t, 2, state.totalToUpdate)
	assert.Zero(t, state.startedCount)
	assert.Zero(t, state.readyCount)
	assert.Equal(t, targetReady, state.targetState)
}

func TestResolveRoleRolloutStateRetainsMissingPartitionProtectedOldSlot(t *testing.T) {
	// The partition-protected old ordinal a-0 is temporarily absent.
	state := resolveRoleRolloutState("a", 2, 2, []coordinatedReplica{runningReplica(1, targetTemplate)}, 1, true, nil, nil)
	assert.Equal(t, 1, state.totalToUpdate)
	assert.Equal(t, 1, state.startedCount)
	assert.Equal(t, 1, state.readyCount)
	assert.True(t, state.hasOldVersion)
}

func TestResolveRoleRolloutStateKeepsTerminatingStartAfterRestart(t *testing.T) {
	state := resolveRoleRolloutState("a", 2, 2,
		[]coordinatedReplica{creatingReplica(0, oldTemplate), runningReplica(1, oldTemplate)}, 0, true, map[int]workload.Comparison{0: oldTemplate}, nil)
	require.Equal(t, 1, state.startedCount)
	require.Zero(t, state.readyCount)
	require.True(t, state.hasOldVersion)
	policy, err := calculateRoleRolloutLimits([]coordinatedRoleState{state, newCoordinatedRoleStateForTest("b", 2, 0, 0)}, coordinationForTest("20%"))
	require.NoError(t, err)
	require.Zero(t, policy.roles["a"].remainingDeletions)
}

func TestStableRolePopulationIncludesObservedExpansion(t *testing.T) {
	tests := []struct {
		name                  string
		ordinals              []int
		targets               []int
		terminating           map[int]workload.Comparison
		missingOld            bool
		count, started, ready int
	}{
		{name: "all expanded old members are eligible", ordinals: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, count: 10},
		{name: "uncreated expansion does not reserve starts", ordinals: []int{0, 1, 2, 3, 4, 9}, count: 6},
		{name: "target expansion is counted once", ordinals: []int{0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, targets: []int{5, 6, 7, 8, 9}, count: 10, started: 5, ready: 5},
		{name: "missing prior replacement remains reserved", ordinals: []int{0, 1, 2, 3, 5}, targets: []int{5}, missingOld: true, count: 6, started: 2, ready: 1},
		{name: "terminating expanded replacement remains reserved", ordinals: []int{0, 1, 2, 3, 4}, terminating: map[int]workload.Comparison{9: oldTemplate}, count: 6, started: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var replicas []coordinatedReplica
			for _, ordinal := range tt.ordinals {
				comparison := oldTemplate
				for _, target := range tt.targets {
					if ordinal == target {
						comparison = targetTemplate
					}
				}
				replicas = append(replicas, runningReplica(ordinal, comparison))
			}
			population := stableRolePopulation(10, 5, replicas, tt.terminating, nil)
			require.NotNil(t, population)
			assert.Equal(t, tt.count, population.countFrom(0))
			for _, ordinal := range tt.ordinals {
				assert.True(t, population.contains(ordinal))
			}
			if tt.missingOld {
				assert.True(t, population.contains(4), "missing original replacement holds its start")
			}
			state := resolveRoleRolloutState("a", 10, 5, replicas, 0, true, tt.terminating, population)
			assert.Equal(t, tt.started, state.startedCount)
			assert.Equal(t, tt.ready, state.readyCount)
			assert.Equal(t, tt.started, state.versionStarted, "formal target capacity must not be counted twice")
			assert.Equal(t, 10, state.versionTotal)
		})
	}
}

func TestStableRoleExpansionDistinguishesAdoptedAndTemporarySurge(t *testing.T) {
	var replicas []coordinatedReplica
	for _, ordinal := range []int{0, 1, 2, 3, 4, 5, 10} {
		replicas = append(replicas, runningReplica(ordinal, targetTemplate))
	}
	// Both 5 and 10 carry the surge mark. Only the one beyond the desired
	// count is temporary.
	population := stableRolePopulation(10, 5, replicas, nil, sets.New(10))
	assert.Equal(t, 6, population.countFrom(0))
	assert.True(t, population.contains(5), "an adopted formal expansion member participates")
	assert.False(t, population.contains(10), "temporary surge never joins stable progress")
	assert.Equal(t, 4, population.countFrom(2), "partition protects absolute original ordinals")
}

func TestCoordinatedVersionCountsProtectedTargetAndFormalExpansion(t *testing.T) {
	state := resolveRoleRolloutState("a", 4, 3,
		[]coordinatedReplica{
			runningReplica(0, targetTemplate), // protected target
			runningReplica(1, oldTemplate),
			creatingReplica(2, targetTemplate),
			runningReplica(3, targetTemplate), // formal expansion
			runningReplica(4, targetTemplate), // surge
		}, 1, true, nil, nil)
	require.Equal(t, 4, state.versionTotal)
	require.Equal(t, 2, state.versionReady)
	require.Equal(t, 3, state.versionStarted)
	require.Equal(t, 2, state.totalToUpdate)
	require.Zero(t, state.readyCount, "neither protected nor expansion/surge satisfies eligible replacement readiness")
	require.False(t, state.tailReady)
}

func TestProtectedTargetDoesNotRetainPhantomOldDependency(t *testing.T) {
	replicas := []coordinatedReplica{runningReplica(0, targetTemplate), runningReplica(1, targetTemplate)}
	state := resolveRoleRolloutState("a", 2, 2, replicas, 1, true, nil, nil)
	require.False(t, state.hasOldVersion)
	require.Equal(t, 2, state.versionReady)
}

func TestMissingFormalCapacityDoesNotBypassDependencyStartup(t *testing.T) {
	state := resolveRoleRolloutState("a", 2, 2, nil, 0, true, nil, nil)
	require.Equal(t, targetNotStarted, state.targetState)
	coordination := coordinationForTest("100%")
	coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b"}}}
	decision, err := calculateRoleRolloutLimits([]coordinatedRoleState{state, newCoordinatedRoleStateForTest("b", 2, 0, 0)}, coordination)
	require.NoError(t, err)
	require.False(t, decision.roles["a"].allowTargetStart)
}

func TestFullTailRequiresObservedReadyKnownCapacity(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*[]coordinatedReplica, map[int]workload.Comparison)
		ready bool
	}{
		{"ready", func(*[]coordinatedReplica, map[int]workload.Comparison) {}, true},
		{"missing", func(r *[]coordinatedReplica, _ map[int]workload.Comparison) { *r = (*r)[:1] }, false},
		{"unready target", func(r *[]coordinatedReplica, _ map[int]workload.Comparison) { (*r)[1].ready = false }, false},
		{"unknown history", func(r *[]coordinatedReplica, _ map[int]workload.Comparison) { (*r)[0].comparison = unknownTemplate }, false},
		{"terminating", func(_ *[]coordinatedReplica, m map[int]workload.Comparison) { m[0] = oldTemplate }, false},
		{"old surge", func(r *[]coordinatedReplica, _ map[int]workload.Comparison) {
			*r = append(*r, runningReplica(2, oldTemplate))
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			replicas := []coordinatedReplica{runningReplica(0, oldTemplate), runningReplica(1, targetTemplate)}
			terminating := map[int]workload.Comparison{}
			tc.alter(&replicas, terminating)
			state := resolveRoleRolloutState("a", 2, 2, replicas, 0, true, terminating, nil)
			require.Equal(t, tc.ready, state.tailReady)
		})
	}
}

// coordinatedGroup returns a group that rolls the Role serve from rev-a to
// rev-b together with other Roles: replicas instances, of which unavailable
// may be unavailable, one surge instance, and the partition.
func coordinatedGroup(replicas, unavailable, partition int) *workloadv1alpha1.ServingGroup {
	role := workloadv1alpha1.ServingGroupRole{
		MaxUnavailable: ptr.To(intstr.FromInt(unavailable)), MaxSurge: ptr.To(intstr.FromInt(1)),
		Partition: ptr.To(intstr.FromInt(partition)), BaselineReplicas: ptr.To(int32(replicas)),
	}
	group := planGroup(int32(replicas), &role)
	group.Spec.RolloutStrategy.RoleCoordination = coordinationForTest("20%")
	return group
}

// removedBy returns the instances a planner run does not want any more.
func removedBy(plan RolePlan, instances []Instance) []string {
	var removed []string
	for _, inst := range instances {
		if plan.Wants[inst.Key()] == nil && inst.Phase() != draining {
			removed = append(removed, fmt.Sprintf("serve-%d", inst.Index))
		}
	}
	sort.Strings(removed)
	return removed
}

// The choice of the instances a rollout replaces, with the cases and the
// expectations of the test that was written for the controller before groups
// were objects (there the Role was called prefill). A Role of a group whose
// Roles are rolled together is replaced in descending ordinal and never past
// an instance that has to wait; temporary capacity is not part of that order.
func TestRoleRolloutAvailabilityAndOrderedCandidates(t *testing.T) {
	for _, tc := range []struct {
		name                                       string
		n, u, partition                            int
		coordinated, exhausted, retained, excluded bool
		unready, temporary, target, terminating    []int
		count                                      int
		unknown                                    int // one-based ordinal, zero disables
		want                                       []string
		// excess lists instances that go as capacity beyond the replica count,
		// not as replacements. The Role plan scales in an instance beyond the
		// replica count that does not carry the surge mark; the controller
		// this table was written for let it count against the surge.
		excess []string
	}{
		{name: "terminating low Pod reserves budget after restart", n: 3, u: 1, count: 3, unready: []int{0, 2}, terminating: []int{0}},
		{name: "independent skips healthy high for low old fault", n: 3, u: 1, count: 3, unready: []int{0}, want: []string{"serve-0"}},
		{name: "coordination waits at healthy high", n: 3, u: 1, count: 3, coordinated: true, unready: []int{0}},
		{name: "nonparticipating Role still ordered when coordination configured", n: 3, u: 1, count: 3, coordinated: true, excluded: true, unready: []int{0}},
		{name: "all old failed repairs highest", n: 3, u: 1, count: 3, coordinated: true, unready: []int{0, 1, 2}, want: []string{"serve-2"}},
		{name: "new failed replacement consumes allowance", n: 3, u: 1, count: 3, unready: []int{0, 1, 2}, target: []int{2}},
		{name: "protected fault cannot fund healthy deletion", n: 3, u: 1, count: 3, partition: 1, unready: []int{0}},
		{name: "049 single obsolete surge only", n: 1, u: 0, count: 2, coordinated: true, unready: []int{1}, temporary: []int{1}, want: []string{"serve-1"}},
		{name: "049 triple obsolete surge only", n: 3, u: 0, count: 4, coordinated: true, unready: []int{3}, temporary: []int{3}, want: []string{"serve-3"}},
		{name: "obsolete surge cleanup does not need stable start credit", n: 3, u: 0, count: 4, coordinated: true, exhausted: true, unready: []int{3}, temporary: []int{3}, want: []string{"serve-3"}},
		{name: "target surge unavailable is not rolled again", n: 1, u: 0, count: 2, coordinated: true, unready: []int{1}, temporary: []int{1}, target: []int{1}},
		{name: "healthy surge retains availability for new stable fault", n: 1, u: 0, count: 2, coordinated: true, unready: []int{0}, temporary: []int{1}, target: []int{0}},
		{name: "ready target surge permits stable replacement", n: 1, u: 0, count: 2, coordinated: true, temporary: []int{1}, target: []int{1}, want: []string{"serve-0"}},
		{name: "unmarked high ordinal is not obsolete surge", n: 3, u: 1, count: 4, coordinated: true, unready: []int{3}, excess: []string{"serve-3"}},
		{name: "absorbed surge obeys stable start limit", n: 3, u: 1, count: 3, coordinated: true, exhausted: true, unready: []int{2}, temporary: []int{2}},
		{name: "necessary last old dependency survives", n: 1, u: 1, count: 2, coordinated: true, retained: true, unready: []int{1}, temporary: []int{1}, target: []int{0}},
		{name: "unknown high history blocks lower old fault", n: 3, u: 1, count: 3, coordinated: true, unknown: 3, unready: []int{0}},
		{name: "unknown high history blocks the healthy old instances below it", n: 3, u: 2, count: 3, coordinated: true, unknown: 3},
		{name: "an old instance below the partition does not make the last old temporary one dispensable", n: 2, u: 1, partition: 1, count: 3, coordinated: true, retained: true, temporary: []int{2}, target: []int{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contains := func(xs []int, i int) bool {
				for _, x := range xs {
					if i == x {
						return true
					}
				}
				return false
			}
			revisions := fakeRevisions{missing: map[string]bool{"missing": true}}
			var instances []Instance
			temporary := sets.New[int]()
			unknownOrdinal := -1
			for ordinal := 0; ordinal < tc.count; ordinal++ {
				mutate := []func(*corev1.Pod){}
				switch {
				case ordinal+1 == tc.unknown:
					mutate = append(mutate, labelled("missing"))
					unknownOrdinal = ordinal
				case contains(tc.target, ordinal):
					mutate = append(mutate, labelled("rev-b"), func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "model:rev-b" })
				}
				if contains(tc.temporary, ordinal) {
					mutate = append(mutate, func(pod *corev1.Pod) {
						pod.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeRole}
					})
					if ordinal >= tc.n {
						temporary.Insert(ordinal)
					}
				}
				if contains(tc.unready, ordinal) {
					mutate = append(mutate, notReady)
				}
				if contains(tc.terminating, ordinal) {
					mutate = append(mutate, terminating)
				}
				inst := instanceOf(ordinal, servePod(ordinal, 0, mutate...), servePod(ordinal, 1, mutate...))
				if contains(tc.terminating, ordinal) {
					// A Pod is deleted only after its instance is recorded as
					// drained: that record is what survives a restart.
					inst.Entry.Phase = draining
				}
				instances = append(instances, inst)
			}
			group := coordinatedGroup(tc.n, tc.u, tc.partition)
			var coordinated *coordinatedRole
			if tc.coordinated {
				coordinated = &coordinatedRole{temporary: temporary, unknownOrdinal: unknownOrdinal}
				if !tc.excluded {
					remaining := tc.n
					if tc.exhausted {
						remaining = 0
					}
					coordinated.limits = &roleRolloutLimits{rolloutEnd: tc.n, effectivePartition: tc.partition, remainingDeletions: remaining, retainOldReplica: tc.retained, allowTargetStart: true}
				}
			} else {
				group.Spec.RolloutStrategy.RoleCoordination = nil
			}
			run := func(instances []Instance) RolePlan {
				spec, err := roleSpec(group, group.Spec.Roles[0], true)
				require.NoError(t, err)
				plan := RolePlan{Wants: map[InstanceKey]*Want{}, InPlace: map[InstanceKey]string{}, Fallback: map[InstanceKey]string{}}
				p := rolePlanner{role: "serve", spec: spec, revisions: revisions, plan: &plan, updating: map[int]string{}, fallback: map[int]string{}, coordinated: coordinated}
				p.run(instances)
				return plan
			}
			var selected []string
			removed := removedBy(run(instances), instances)
			for _, name := range removed {
				if !slices.Contains(tc.excess, name) {
					selected = append(selected, name)
				}
			}
			require.Equal(t, tc.want, selected)

			// Selected deletions remain physically present. Their reservation must
			// prevent another stable deletion before replacement readiness.
			for i := range instances {
				if contains(indices(removed), instances[i].Index) {
					instances[i].Entry.Phase = draining
				}
			}
			require.Empty(t, removedBy(run(instances), instances))
		})
	}
}

func indices(names []string) []int {
	var out []int
	for _, name := range names {
		var index int
		fmt.Sscanf(strings.TrimPrefix(name, "serve-"), "%d", &index)
		out = append(out, index)
	}
	return out
}
