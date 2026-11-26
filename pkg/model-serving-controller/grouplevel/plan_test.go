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

package grouplevel

import (
	"fmt"
	"math/rand"
	"reflect"
	"slices"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

func uidOf(ordinal int) types.UID {
	return types.UID(fmt.Sprintf("uid-%d", ordinal))
}

// group returns a regular group that is confirmed Ready and whose Pods are Ready.
func group(ordinal int, rev string, mods ...func(*Group)) Group {
	g := Group{Ordinal: ordinal, UID: uidOf(ordinal), Revision: rev, Observed: true, Fresh: true, Ready: true, PodsReady: true}
	for _, mod := range mods {
		mod(&g)
	}
	return g
}

func surge(g *Group)      { g.Surge = true }
func notReady(g *Group)   { g.Ready, g.PodsReady = false, false }
func unobserved(g *Group) { g.Observed = false }
func recreate(g *Group)   { g.RecreateRequired = true }

func terminating(g *Group) {
	notReady(g)
	g.Terminating = true
}

func failed(g *Group) {
	notReady(g)
	g.Failed = true
}

func drained(g *Group) {
	failed(g)
	g.Drained = true
}

// podsBad: the Pods stopped being Ready and the status still says Ready.
func podsBad(g *Group) { g.PodsReady, g.Fresh = false, false }

// memberless: the object is there, observed, and has no Pod.
func memberless(g *Group) {
	notReady(g)
	g.Memberless = true
}

// empty: every Role has zero replicas; such a group has no Pod and is Ready.
func empty(g *Group) {
	g.Empty, g.Memberless, g.PodsReady = true, true, false
}

func cost(c int64) func(*Group) {
	return func(g *Group) { g.DeletionCost = c }
}

func create(ordinal int, rev, reason string) Action {
	return Action{Kind: ActionCreate, Ordinal: ordinal, Revision: rev, Surge: reason == ReasonSurge, Reason: reason}
}

func del(ordinal int, reason string) Action {
	return Action{Kind: ActionDelete, Ordinal: ordinal, UID: uidOf(ordinal), Reason: reason}
}

func promote(ordinal int) Action {
	return Action{Kind: ActionPromote, Ordinal: ordinal, UID: uidOf(ordinal)}
}

func retarget(ordinal int, rev, reason string) Action {
	return Action{Kind: ActionRetarget, Ordinal: ordinal, UID: uidOf(ordinal), Revision: rev, Reason: reason}
}

func wait(ordinal int, reason string) Action {
	return Action{Kind: ActionWait, Ordinal: ordinal, Reason: reason}
}

func baseline(rev string) Action {
	return Action{Kind: ActionAdvanceBaseline, Ordinal: -1, Revision: rev, Reason: ReasonComplete}
}

// rollout is a ServingGroupRollingUpdate from v1 to v2.
func rollout(n, u, s, p int) Spec {
	return Spec{Replicas: n, MaxUnavailable: u, MaxSurge: s, Partition: p, Target: "v2", Baseline: "v1"}
}

// movedOn is a rollout whose target moved on to v3 before v2 was complete.
func movedOn(n, u, s int) Spec {
	spec := rollout(n, u, s, 0)
	spec.Target = "v3"
	return spec
}

// steady is a spec whose target is its baseline: nothing to roll.
func steady(n, u, s int) Spec {
	return Spec{Replicas: n, MaxUnavailable: u, MaxSurge: s, Target: "v1", Baseline: "v1"}
}

func roleMode(spec Spec) Spec {
	spec.Mode = ModeRole
	return spec
}

func inPlace(spec Spec) Spec {
	spec.InPlace = true
	return spec
}

func noBaseline(spec Spec) Spec {
	spec.Baseline = ""
	return spec
}

type planCase struct {
	name string
	in   Input
	want []Action
}

func runPlanCases(t *testing.T, cases []planCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Plan(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("Plan(%+v)\n got: %+v\nwant: %+v", tc.in, got, tc.want)
			}
		})
	}
}

// S1′
func TestPlanRecover(t *testing.T) {
	old2 := []Group{group(0, "v1"), group(1, "v1")}
	runPlanCases(t, []planCase{
		{
			name: "an ordinal with ownerless member Pods and no object gets an object of their revision",
			in:   Input{Spec: steady(3, 1, 0), Groups: []Group{group(0, "v1"), group(2, "v1")}, Orphans: map[int]Orphans{1: {Revision: "v0"}}},
			want: []Action{create(1, "v0", ReasonRecover)},
		},
		{
			name: "the mark is taken from the Pods when the ordinal is not below N",
			in:   Input{Spec: rollout(2, 0, 1, 0), Groups: old2, Orphans: map[int]Orphans{2: {Revision: "v2", Marked: true}}},
			want: []Action{{Kind: ActionCreate, Ordinal: 2, Revision: "v2", Surge: true, Reason: ReasonRecover}},
		},
		{
			name: "no mark below N",
			in:   Input{Spec: steady(2, 1, 1), Groups: []Group{group(0, "v1")}, Orphans: map[int]Orphans{1: {Revision: "v1", Marked: true}}},
			want: []Action{create(1, "v1", ReasonRecover)},
		},
		{
			name: "no mark under RoleRollingUpdate",
			in:   Input{Spec: roleMode(steady(2, 1, 1)), Groups: old2, Orphans: map[int]Orphans{2: {Revision: "v1", Marked: true}}},
			want: []Action{create(2, "v1", ReasonRecover)},
		},
		{
			name: "whatever N is: the recovered group awaits adoption and is not scaled down",
			in:   Input{Spec: steady(2, 1, 0), Groups: old2, Orphans: map[int]Orphans{3: {Revision: "v1"}}},
			want: []Action{create(3, "v1", ReasonRecover)},
		},
		{
			name: "a revision that cannot be read: the baseline below the partition",
			in:   Input{Spec: rollout(3, 1, 0, 2), Groups: []Group{group(0, "v1"), group(2, "v2")}, Orphans: map[int]Orphans{1: {}}},
			want: []Action{create(1, "v1", ReasonRecover)},
		},
		{
			name: "a revision that cannot be read: the target at or above the partition",
			in:   Input{Spec: rollout(3, 1, 0, 2), Groups: []Group{group(0, "v1"), group(1, "v1")}, Orphans: map[int]Orphans{2: {}}},
			want: []Action{create(2, "v2", ReasonRecover)},
		},
		{
			name: "neither the revision of the Pods nor the baseline can be read: nothing is created, reported once",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 1)), Groups: []Group{group(1, "v2")}, Orphans: map[int]Orphans{0: {}}},
			want: []Action{wait(0, ReasonBaselineUnknown)},
		},
		{
			name: "the revision of the Pods needs no baseline",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 1)), Groups: []Group{group(1, "v2")}, Orphans: map[int]Orphans{0: {Revision: "v1"}}},
			want: []Action{create(0, "v1", ReasonRecover)},
		},
		{
			name: "an ordinal that has an object is not recovered",
			in:   Input{Spec: steady(2, 1, 0), Groups: old2, Orphans: map[int]Orphans{1: {Revision: "v1"}}},
		},
		{
			name: "an ordinal a foreign object takes is not recovered",
			in:   Input{Spec: steady(2, 1, 0), Groups: old2, Orphans: map[int]Orphans{2: {Revision: "v1"}}, Foreign: []int{2}},
		},
	})
}

// G0
func TestPlanPromote(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "a marked group below N loses the mark while regular < N",
			in:   Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", surge)}},
			want: []Action{promote(2)},
		},
		{
			name: "not while N regular groups exist: it stays surge and is reclaimed",
			in:   Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", surge), group(3, "v1")}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
		{
			name: "not at an ordinal that is not below N: the regular place is filled by G4",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(2, "v1", surge)}},
			want: []Action{create(1, "v1", ReasonRegular)},
		},
		{
			name: "lowest ordinal first, until regular = N",
			in:   Input{Spec: steady(2, 1, 1), Groups: []Group{group(1, "v1", surge), group(0, "v1", surge), group(2, "v1")}},
			want: []Action{promote(0), del(1, ReasonReclaimSurge)},
		},
		{
			name: "a group in any state",
			in:   Input{Spec: steady(2, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1", surge, terminating)}},
			want: []Action{promote(1)},
		},
		{
			name: "RoleRollingUpdate: every marked group loses the mark, then it is a regular group like any other",
			in:   Input{Spec: roleMode(steady(2, 1, 1)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(3, "v1", surge)}},
			want: []Action{promote(3), del(3, ReasonScaleDown)},
		},
		{
			name: "RoleRollingUpdate: also while N regular groups exist and in any state",
			in:   Input{Spec: roleMode(steady(2, 1, 1)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", surge, terminating)}},
			want: []Action{promote(2)},
		},
	})
}

// G1
func TestPlanDeleteFailed(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "a Failed group is deleted once it is Drained; its place is filled when it is gone",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", drained)}},
			want: []Action{del(1, ReasonFailed)},
		},
		{
			name: "not before it is Drained",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", failed)}},
		},
		{
			name: "not twice",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", drained, terminating)}},
		},
		{
			name: "Drained alone means nothing",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", func(g *Group) { g.Drained = true })}},
		},
		{
			name: "a Failed surge group is deleted by G1 once Drained",
			in:   Input{Spec: steady(1, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1", surge, drained)}},
			want: []Action{del(1, ReasonFailed)},
		},
		{
			name: "a Failed surge group that is not Drained is not reclaimed by G3b",
			in:   Input{Spec: steady(1, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1", surge, failed)}},
		},
		{
			name: "lowest ordinal first; under RoleRollingUpdate too",
			in:   Input{Spec: roleMode(steady(3, 1, 0)), Groups: []Group{group(2, "v1", drained), group(0, "v1", drained), group(1, "v1")}},
			want: []Action{del(0, ReasonFailed), del(2, ReasonFailed)},
		},
	})
}

// G2
func TestPlanScaleDown(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "the higher ordinal",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1")}},
			want: []Action{del(2, ReasonScaleDown)},
		},
		{
			name: "the lower deletion cost before the higher ordinal",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1", cost(-5)), group(1, "v1"), group(2, "v1", cost(5))}},
			want: []Action{del(0, ReasonScaleDown)},
		},
		{
			name: "Pods that are not Ready before the lower deletion cost",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1", cost(-5)), group(1, "v1", notReady, cost(10)), group(2, "v1")}},
			want: []Action{del(1, ReasonScaleDown)},
		},
		{
			name: "the order goes by the Pods, not by the status",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1", podsBad), group(1, "v1"), group(2, "v1", unobserved)}},
			want: []Action{del(0, ReasonScaleDown)},
		},
		{
			name: "one at a time until regularLive = N; no budget, no partition",
			in:   Input{Spec: rollout(1, 0, 0, 3), Groups: []Group{group(0, "v2", cost(1)), group(1, "v2"), group(2, "v2", cost(1))}},
			want: []Action{del(1, ReasonScaleDown), del(2, ReasonScaleDown)},
		},
		{
			name: "a group that awaits adoption is not counted",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1")}, Orphans: map[int]Orphans{2: {Revision: "v1"}}},
		},
		{
			name: "a group that awaits adoption is not chosen",
			in: Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1"), group(3, "v1", notReady)},
				Orphans: map[int]Orphans{3: {Revision: "v1"}}},
			want: []Action{del(2, ReasonScaleDown)},
		},
		{
			name: "terminating, failed and surge groups are neither counted nor chosen",
			in: Input{Spec: steady(2, 0, 2), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1", terminating),
				group(3, "v1", failed), group(4, "v1", surge, unobserved)}},
		},
		{
			name: "while regular > N neither surge nor replacement",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1")}},
			want: []Action{del(2, ReasonScaleDown)},
		},
		{
			name: "the group that is being scaled down still counts: neither surge nor replacement",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", terminating)}},
		},
	})
}

// G2′(a)
func TestPlanDropMemberless(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "a regular memberless group outside the range is deleted when G4 can take its place",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(3, "v1", memberless)}},
			want: []Action{del(3, ReasonMemberlessOutOfRange)},
		},
		{
			name: "an empty one has its own reason; no status is asked",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(3, "v1", empty, unobserved)}},
			want: []Action{del(3, ReasonEmptyOutOfRange)},
		},
		{
			name: "under RoleRollingUpdate too",
			in:   Input{Spec: roleMode(steady(2, 1, 0)), Groups: []Group{group(0, "v1"), group(3, "v1", memberless)}},
			want: []Action{del(3, ReasonMemberlessOutOfRange)},
		},
		{
			name: "at most one in a round, the highest ordinal",
			in:   Input{Spec: steady(3, 1, 0), Groups: []Group{group(0, "v1"), group(3, "v1", memberless), group(4, "v1", memberless)}},
			want: []Action{del(4, ReasonMemberlessOutOfRange)},
		},
		{
			name: "not while a group is terminating",
			in:   Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(4, "v1", memberless), group(5, "v1", surge, terminating)}},
		},
		{
			name: "not while a group is Failed",
			in:   Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(4, "v1", memberless), group(5, "v1", surge, failed)}},
		},
		{
			name: "not after a deletion of this round",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", drained), group(3, "v1", memberless)}},
			want: []Action{del(1, ReasonFailed)},
		},
		{
			name: "not while a regular place is not filled yet: G4 fills it and the group stays, its revision untouched",
			in:   Input{Spec: rollout(4, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2"), group(4, "v1", memberless)}},
			want: []Action{create(2, "v2", ReasonRegular)},
		},
		{
			name: "not when the bound N + S leaves no room",
			in:   Input{Spec: steady(2, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1", surge), group(3, "v1", memberless)}},
		},
		{
			name: "not when every ordinal below N is taken: the foreign object is reported",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(3, "v1", memberless)}, Foreign: []int{1}},
			want: []Action{wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "not when the free ordinal needs a baseline that cannot be read",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 2)), Groups: []Group{group(0, "v1"), group(3, "v1", memberless)}},
			want: []Action{wait(1, ReasonBaselineUnknown)},
		},
		{
			name: "a memberless group inside the range stays",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", memberless)}},
		},
		{
			name: "a marked memberless group is not looked at: G3b reclaims it",
			in:   Input{Spec: steady(1, 1, 1), Groups: []Group{group(0, "v1"), group(2, "v1", surge, memberless)}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
	})
}

// G2′(b)
func TestPlanRetargetMemberless(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "a memberless group gets the revision of its ordinal: the target",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1", memberless)}},
			want: []Action{retarget(1, "v2", ReasonMemberless)},
		},
		{
			name: "the baseline below the partition",
			in:   Input{Spec: rollout(2, 1, 0, 1), Groups: []Group{group(0, "v0", memberless, unobserved), group(1, "v2")}},
			want: []Action{retarget(0, "v1", ReasonMemberless)},
		},
		{
			name: "not while the baseline is needed and cannot be read",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 1)), Groups: []Group{group(0, "v0", memberless), group(1, "v2")}},
			want: []Action{wait(0, ReasonBaselineUnknown)},
		},
		{
			name: "nothing to do when it has that revision",
			in:   Input{Spec: rollout(2, 1, 0, 1), Groups: []Group{group(0, "v1", memberless), group(1, "v2")}},
		},
		{
			name: "RecreateRequired without a member is not replaceable: the revision is set",
			in:   Input{Spec: rollout(2, 2, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v1", memberless, recreate)}},
			want: []Action{retarget(1, "v2", ReasonMemberless)},
		},
		{
			name: "a marked memberless group too",
			in:   Input{Spec: rollout(1, 0, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1", surge, memberless, unobserved)}},
			want: []Action{retarget(1, "v2", ReasonMemberless)},
		},
		{
			name: "a regular one outside the range is left alone while it waits for a place below N",
			in:   Input{Spec: rollout(2, 0, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2", surge), group(3, "v1", memberless)}},
		},
		{
			name: "not under RoleRollingUpdate",
			in:   Input{Spec: roleMode(rollout(2, 1, 0, 0)), Groups: []Group{group(0, "v2"), group(1, "v1", memberless)}},
		},
	})
}

// G4
func TestPlanFill(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "the lowest free ordinal below N, one at a time",
			in:   Input{Spec: steady(4, 1, 0), Groups: []Group{group(1, "v1"), group(5, "v1")}},
			want: []Action{create(0, "v1", ReasonRegular), create(2, "v1", ReasonRegular)},
		},
		{
			name: "the baseline below the partition, the target from it on",
			in:   Input{Spec: rollout(3, 1, 0, 2)},
			want: []Action{create(0, "v1", ReasonRegular), create(1, "v1", ReasonRegular), create(2, "v2", ReasonRegular)},
		},
		{
			name: "RoleRollingUpdate creates from the target",
			in:   Input{Spec: roleMode(rollout(2, 1, 0, 2))},
			want: []Action{create(0, "v2", ReasonRegular), create(1, "v2", ReasonRegular)},
		},
		{
			name: "a baseline that cannot be read stops G4 at that ordinal and above it",
			in:   Input{Spec: noBaseline(rollout(3, 1, 0, 2)), Groups: []Group{group(0, "v1")}},
			want: []Action{wait(1, ReasonBaselineUnknown)},
		},
		{
			name: "at or above the partition no baseline is needed",
			in:   Input{Spec: noBaseline(rollout(3, 1, 0, 2)), Groups: []Group{group(0, "v1"), group(1, "v1")}},
			want: []Action{create(2, "v2", ReasonRegular)},
		},
		{
			name: "a foreign ordinal is passed over; a place that stays unfilled reports it",
			in:   Input{Spec: steady(3, 1, 0), Groups: []Group{group(0, "v1")}, Foreign: []int{4, 1}},
			want: []Action{create(2, "v1", ReasonRegular), wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "a foreign ordinal is not reported when every place is filled",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(3, "v1")}, Foreign: []int{1}},
		},
		{
			name: "terminating and failed groups occupy capacity: active < N + S",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(5, "v1", surge, terminating)}},
		},
		{
			name: "up to N + S",
			in:   Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(5, "v1", surge, terminating), group(6, "v1", surge, failed)}},
			want: []Action{create(1, "v1", ReasonRegular)},
		},
		{
			name: "a terminating regular group keeps its place until it is gone",
			in:   Input{Spec: steady(2, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1", terminating)}},
		},
	})
}

// G3a
func TestPlanCreateSurge(t *testing.T) {
	old2 := []Group{group(0, "v1"), group(1, "v1")}
	runPlanCases(t, []planCase{
		{
			name: "work: surge of the target revision at the lowest free ordinal not below N",
			in:   Input{Spec: rollout(2, 0, 1, 0), Groups: old2},
			want: []Action{create(2, "v2", ReasonSurge)},
		},
		{
			name: "until active = N + S, passing taken ordinals",
			in:   Input{Spec: rollout(2, 0, 3, 0), Groups: append([]Group{group(3, "v2", surge, terminating)}, old2...), Foreign: []int{2}},
			want: []Action{create(4, "v2", ReasonSurge), create(5, "v2", ReasonSurge)},
		},
		{
			name: "no work when the partition protects every old group",
			in:   Input{Spec: rollout(2, 0, 1, 2), Groups: old2},
		},
		{
			name: "no work when every group is of the target revision",
			in:   Input{Spec: steady(2, 0, 1), Groups: old2},
		},
		{
			name: "RecreateRequired is work",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", recreate)}},
			want: []Action{create(2, "v1", ReasonSurge)},
		},
		{
			name: "a replaceable group that is not settled is work, and no candidate yet",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v2"), group(1, "v1", unobserved)}},
			want: []Action{create(2, "v2", ReasonSurge)},
		},
		{
			name: "no work when the place of the replaceable group cannot be filled: no surge, and the plan says why",
			in:   Input{Spec: rollout(2, 1, 1, 1), Groups: []Group{group(0, "v1"), group(3, "v1")}, Foreign: []int{1}},
			want: []Action{wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "not under RoleRollingUpdate",
			in:   Input{Spec: roleMode(rollout(2, 1, 1, 0)), Groups: old2},
		},
	})
}

// G3b
func TestPlanReclaimSurge(t *testing.T) {
	runPlanCases(t, []planCase{
		{
			name: "no work: a confirmed Ready surge group is reclaimed while ready stays at the floor",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", surge)}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
		{
			name: "it is kept while ready would fall below the floor",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1", surge)}},
		},
		{
			name: "one that is confirmed not Ready is reclaimed at once",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1", surge, notReady)}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
		{
			// The end of a rollout: the last old group is terminating and its
			// replacement is not created yet. N others do not have their
			// members, so the surge group is not surplus.
			name: "one that is confirmed not Ready is kept while the last old group is terminating",
			in: Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(0, "v1", terminating), group(1, "v2"), group(2, "v2"),
				group(3, "v2", surge, notReady)}},
		},
		{
			name: "so is a confirmed Ready one, although ready would stay at the floor",
			in: Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(0, "v1", terminating), group(1, "v2"), group(2, "v2"),
				group(3, "v2", surge)}},
		},
		{
			name: "the group created in this round has no member yet: the place is filled, the surge group stays",
			in:   Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(1, "v2"), group(2, "v2"), group(3, "v2", surge, notReady)}},
			want: []Action{create(0, "v2", ReasonRegular)},
		},
		{
			name: "it stays while the replacement has no Pod",
			in: Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(0, "v2", memberless), group(1, "v2"), group(2, "v2"),
				group(3, "v2", surge, notReady)}},
		},
		{
			name: "it is reclaimed once N other groups have their members, Ready or not",
			in: Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(0, "v2", notReady), group(1, "v2"), group(2, "v2"),
				group(3, "v2", surge, notReady)}},
			want: []Action{del(3, ReasonReclaimSurge)},
		},
		{
			name: "a group that awaits adoption has not adopted its members: it does not count",
			in: Input{Spec: steady(3, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1"), group(3, "v1", surge)},
				Orphans: map[int]Orphans{0: {Revision: "v1"}}},
		},
		{
			name: "an empty group expects no member: it counts",
			in:   Input{Spec: steady(2, 1, 1), Groups: []Group{group(0, "v1", empty), group(1, "v1"), group(2, "v1", surge)}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
		{
			name: "of two surge groups the one that is surplus is reclaimed, the other stays",
			in: Input{Spec: steady(3, 1, 2), Groups: []Group{group(0, "v1", terminating), group(1, "v1"), group(2, "v1"),
				group(3, "v1", surge, notReady), group(4, "v1", surge, notReady)}},
			want: []Action{del(4, ReasonReclaimSurge)},
		},
		{
			// maxSurge was lowered to 0 with a surge group left. The place of
			// ordinal 1 cannot be created while two groups are live.
			name: "no room: fewer than N regular groups are live and the live groups fill N + S",
			in:   Input{Spec: steady(2, 1, 0), Groups: []Group{group(0, "v1"), group(2, "v1", surge)}},
			want: []Action{del(2, ReasonReclaimSurge)},
		},
		{
			name: "no room, and the floor still holds a confirmed Ready one",
			in:   Input{Spec: steady(2, 0, 0), Groups: []Group{group(0, "v1"), group(2, "v1", surge)}},
		},
		{
			name: "no room only until the live groups are below N + S",
			in: Input{Spec: steady(3, 2, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(3, "v1", surge, notReady),
				group(4, "v1", surge, notReady)}},
			want: []Action{del(4, ReasonReclaimSurge)},
		},
		{
			name: "one that is not observed asks the floor of ready itself: kept",
			in:   Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1", surge, unobserved)}},
		},
		{
			name: "one that is not observed asks the floor of ready itself: reclaimed, and it takes nothing off ready",
			in: Input{Spec: steady(2, 0, 2), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", surge),
				group(3, "v1", surge, notReady, unobserved)}},
			want: []Action{del(3, ReasonReclaimSurge), del(2, ReasonReclaimSurge)},
		},
		{
			name: "one whose status is not fresh is not reclaimed as a not Ready one",
			in: Input{Spec: steady(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", notReady),
				group(2, "v1", surge, notReady, func(g *Group) { g.Fresh, g.PodsReady = false, true })}},
		},
		{
			name: "Pods not Ready first, then the higher ordinal",
			in: Input{Spec: steady(2, 0, 3), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1", surge, podsBad),
				group(3, "v1", surge), group(4, "v1", surge)}},
			want: []Action{del(2, ReasonReclaimSurge), del(4, ReasonReclaimSurge)},
		},
		{
			name: "work and live > N + S: down to N + S, then the replacement goes on",
			in: Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", surge),
				group(3, "v2", surge)}},
			want: []Action{del(3, ReasonReclaimSurge), del(1, ReasonReplace), del(0, ReasonReplace)},
		},
		{
			name: "work and live = N + S: nothing is reclaimed",
			in:   Input{Spec: rollout(2, 0, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", surge, notReady)}},
		},
		{
			name: "a marked group that awaits adoption is no candidate",
			in:   Input{Spec: steady(1, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v1", surge)}, Orphans: map[int]Orphans{1: {Revision: "v1", Marked: true}}},
		},
		{
			name: "a marked empty group is confirmed Ready: reclaimed within the floor",
			in:   Input{Spec: steady(1, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1", surge, empty)}},
			want: []Action{del(1, ReasonReclaimSurge)},
		},
	})
}

// G3c
func TestPlanReplace(t *testing.T) {
	old3 := []Group{group(0, "v1"), group(1, "v1"), group(2, "v1")}
	runPlanCases(t, []planCase{
		{
			name: "the highest ordinal, within maxUnavailable",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: old3},
			want: []Action{del(2, ReasonReplace)},
		},
		{
			name: "the budgets are taken again after each",
			in:   Input{Spec: rollout(3, 2, 0, 0), Groups: old3},
			want: []Action{del(2, ReasonReplace), del(1, ReasonReplace)},
		},
		{
			name: "nothing while the budget is used: a terminating group, a target group that is not Ready",
			in:   Input{Spec: rollout(4, 2, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", notReady), group(3, "v1", terminating)}},
		},
		{
			name: "confirmed not Ready first: it needs maxScaleDown alone",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1")}},
			want: []Action{del(1, ReasonReplaceNotReady)},
		},
		{
			name: "a not Ready old group does not use the healthy budget of the others",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v1", notReady), group(1, "v1"), group(2, "v1")}},
			want: []Action{del(0, ReasonReplaceNotReady)},
		},
		{
			name: "the healthy budget: ready at the floor, nothing is replaced, and a group that is not settled is no candidate",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", unobserved)}},
		},
		{
			name: "a status that is not fresh is neither Ready nor not Ready",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", podsBad)}},
		},
		{
			name: "the partition protects the ordinals below it",
			in:   Input{Spec: rollout(3, 3, 0, 2), Groups: old3},
			want: []Action{del(2, ReasonReplace)},
		},
		{
			name: "RecreateRequired is replaceable below the partition and at the target revision",
			in:   Input{Spec: rollout(2, 1, 0, 2), Groups: []Group{group(0, "v2", recreate), group(1, "v1")}},
			want: []Action{del(0, ReasonReplace)},
		},
		{
			name: "RecreateRequired below the partition is not replaceable while the baseline cannot be read",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 2)), Groups: []Group{group(0, "v1", recreate), group(1, "v1")}},
		},
		{
			name: "a replaceable surge group is a candidate, highest ordinal first",
			in:   Input{Spec: movedOn(2, 0, 1), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", surge)}},
			want: []Action{del(2, ReasonReplace)},
		},
		{
			name: "after the last replaceable regular group a replaceable surge group is still replaced in the round",
			in:   Input{Spec: movedOn(1, 1, 1), Groups: []Group{group(0, "v1"), group(1, "v2", surge, notReady)}},
			want: []Action{del(1, ReasonReplaceNotReady), del(0, ReasonReplace)},
		},
		{
			name: "a group that awaits adoption is no candidate",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v1")}, Orphans: map[int]Orphans{1: {Revision: "v1"}}},
			want: []Action{del(0, ReasonReplace)},
		},
		{
			name: "a regular group whose place cannot be filled is passed over; when only such are left the plan says why",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v1"), group(3, "v1")}, Foreign: []int{1}},
			want: []Action{del(0, ReasonReplace), wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "a free ordinal serves one need: the second group that counts on it stays",
			in:   Input{Spec: rollout(3, 3, 0, 0), Groups: []Group{group(0, "v2"), group(3, "v1"), group(4, "v1")}, Foreign: []int{1}},
			want: []Action{del(4, ReasonReplace), wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "a marked group below N that G0 would promote is a place",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v1"), group(1, "v2", surge), group(3, "v1")}},
			want: []Action{del(3, ReasonReplace), del(0, ReasonReplace)},
		},
		{
			name: "no place when the replacement would need a baseline that cannot be read: nothing is rolled, reported once",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 1)), Groups: []Group{group(1, "v1"), group(3, "v1")}},
			want: []Action{wait(0, ReasonBaselineUnknown)},
		},
		{
			name: "the bound N + S takes no part in whether a place can be filled: a surge group the floor keeps",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v1", notReady), group(1, "v1", notReady), group(2, "v2", surge, unobserved)}},
			want: []Action{del(1, ReasonReplaceNotReady)},
		},
		{
			name: "not under RoleRollingUpdate",
			in:   Input{Spec: roleMode(rollout(3, 1, 0, 0)), Groups: old3},
		},
	})
}

// G6
func TestPlanAdvanceBaseline(t *testing.T) {
	done := []Group{group(0, "v2"), group(1, "v2")}
	partitioned := func(spec Spec) Spec {
		spec.RolePartitioned = true
		return spec
	}
	runPlanCases(t, []planCase{
		{
			name: "every group of the target revision and confirmed Ready",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: done},
			want: []Action{baseline("v2")},
		},
		{
			name: "nothing to advance when the baseline is the target",
			in:   Input{Spec: Spec{Replicas: 2, Target: "v2", Baseline: "v2"}, Groups: done},
		},
		{
			name: "a baseline that cannot be read is advanced as well",
			in:   Input{Spec: noBaseline(rollout(2, 1, 0, 0)), Groups: done},
			want: []Action{baseline("v2")},
		},
		{
			name: "not while a partition is set",
			in:   Input{Spec: rollout(2, 1, 0, 1), Groups: done},
		},
		{
			name: "not with N = 0",
			in:   Input{Spec: rollout(0, 0, 0, 0)},
		},
		{
			name: "not while active differs from N",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: append([]Group{group(2, "v2", terminating)}, done...)},
		},
		{
			name: "not while a group is of another revision",
			in:   Input{Spec: rollout(2, 0, 0, 0), Groups: []Group{group(0, "v1"), group(1, "v2")}},
		},
		{
			name: "not while a group is not Ready",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2", notReady)}},
		},
		{
			name: "not while a group is not observed",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2", unobserved)}},
		},
		{
			name: "not while a status is not fresh",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2", podsBad)}},
		},
		{
			name: "not while a group is empty, although it is Ready",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2", empty)}},
		},
		{
			name: "a marked group that is promoted in the round does not hold it",
			in:   Input{Spec: rollout(2, 1, 1, 0), Groups: []Group{group(0, "v2"), group(1, "v2", surge)}},
			want: []Action{promote(1), baseline("v2")},
		},
		{
			name: "not in the round that still writes a group",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2")}},
			want: []Action{create(1, "v2", ReasonRegular)},
		},
		{
			name: "RoleRollingUpdate: no Role has a partition; the top-level partition is not looked at",
			in:   Input{Spec: roleMode(rollout(2, 1, 0, 1)), Groups: done},
			want: []Action{baseline("v2")},
		},
		{
			name: "RoleRollingUpdate: not while a Role has a partition",
			in:   Input{Spec: partitioned(roleMode(rollout(2, 1, 0, 0))), Groups: done},
		},
		{
			name: "ServingGroupRollingUpdate does not look at RolePartitioned",
			in:   Input{Spec: partitioned(rollout(2, 1, 0, 0)), Groups: done},
			want: []Action{baseline("v2")},
		},
	})
}

// In-place update: G3c changes spec.revision instead of deleting.
func TestPlanInPlace(t *testing.T) {
	old3 := []Group{group(0, "v1"), group(1, "v1"), group(2, "v1")}
	runPlanCases(t, []planCase{
		{
			name: "the candidate is retargeted, not deleted",
			in:   Input{Spec: inPlace(rollout(3, 1, 0, 0)), Groups: old3},
			want: []Action{retarget(2, "v2", ReasonInPlace)},
		},
		{
			name: "a retargeted group is a target group that is not Ready: it uses both budgets",
			in:   Input{Spec: inPlace(rollout(3, 2, 0, 0)), Groups: old3},
			want: []Action{retarget(2, "v2", ReasonInPlace), retarget(1, "v2", ReasonInPlace)},
		},
		{
			name: "a group that is being updated in place holds the budget in the next round",
			in:   Input{Spec: inPlace(rollout(3, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", notReady, unobserved)}},
		},
		{
			name: "confirmed not Ready first",
			in:   Input{Spec: inPlace(rollout(3, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1", notReady), group(2, "v1")}},
			want: []Action{retarget(1, "v2", ReasonInPlace)},
		},
		{
			name: "the healthy budget and the settled status are asked as for a deletion",
			in:   Input{Spec: inPlace(rollout(3, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", unobserved)}},
		},
		{
			name: "the partition protects as for a deletion",
			in:   Input{Spec: inPlace(rollout(3, 3, 0, 2)), Groups: old3},
			want: []Action{retarget(2, "v2", ReasonInPlace)},
		},
		{
			name: "RecreateRequired is replaced by deletion",
			in:   Input{Spec: inPlace(rollout(3, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v1", recreate)}},
			want: []Action{del(2, ReasonReplace)},
		},
		{
			name: "both in one round, the budgets shared",
			in:   Input{Spec: inPlace(rollout(3, 2, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1", recreate), group(2, "v1")}},
			want: []Action{retarget(2, "v2", ReasonInPlace), del(1, ReasonReplace)},
		},
		{
			name: "no surge for work that is executed in place",
			in:   Input{Spec: inPlace(rollout(2, 1, 1, 0)), Groups: []Group{group(0, "v1"), group(1, "v1")}},
			want: []Action{retarget(1, "v2", ReasonInPlace)},
		},
		{
			name: "surge for the group that is executed by recreation",
			in:   Input{Spec: inPlace(rollout(2, 1, 1, 0)), Groups: []Group{group(0, "v1", recreate), group(1, "v1")}},
			want: []Action{create(2, "v2", ReasonSurge), retarget(1, "v2", ReasonInPlace)},
		},
		{
			name: "surge that is left is reclaimed when only in-place work remains",
			in:   Input{Spec: inPlace(rollout(2, 1, 1, 0)), Groups: []Group{group(0, "v1"), group(1, "v1"), group(2, "v2", surge)}},
			want: []Action{del(2, ReasonReclaimSurge), retarget(1, "v2", ReasonInPlace)},
		},
		{
			name: "a replaceable surge group is retargeted while recreation work keeps it",
			in:   Input{Spec: inPlace(rollout(2, 1, 1, 0)), Groups: []Group{group(0, "v1", recreate), group(1, "v2"), group(2, "v1", surge)}},
			want: []Action{retarget(2, "v2", ReasonInPlace), del(0, ReasonReplace)},
		},
		{
			name: "in place does not ask whether the place can be filled: a foreign object",
			in:   Input{Spec: inPlace(rollout(2, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(3, "v1")}, Foreign: []int{1}},
			want: []Action{retarget(3, "v2", ReasonInPlace)},
		},
		{
			name: "in place does not ask whether the place can be filled: a baseline that cannot be read",
			in:   Input{Spec: inPlace(noBaseline(rollout(2, 1, 0, 1))), Groups: []Group{group(1, "v1"), group(3, "v1")}},
			want: []Action{retarget(3, "v2", ReasonInPlace)},
		},
		{
			name: "RecreateRequired still asks it: passed over, the in-place candidate is taken, and the plan says why the other stays",
			in:   Input{Spec: inPlace(rollout(2, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(3, "v1", recreate)}, Foreign: []int{1}},
			want: []Action{retarget(0, "v2", ReasonInPlace), wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "only RecreateRequired groups whose place cannot be filled: no work, and the plan says why",
			in:   Input{Spec: inPlace(rollout(2, 1, 1, 1)), Groups: []Group{group(0, "v1"), group(3, "v1", recreate)}, Foreign: []int{1}},
			want: []Action{wait(1, ReasonOrdinalOccupied)},
		},
		{
			name: "not while scaling down",
			in:   Input{Spec: inPlace(rollout(2, 1, 0, 0)), Groups: old3},
			want: []Action{del(2, ReasonScaleDown)},
		},
		{
			name: "a group that awaits adoption is no candidate",
			in:   Input{Spec: inPlace(rollout(2, 1, 0, 0)), Groups: []Group{group(0, "v1"), group(1, "v1")}, Orphans: map[int]Orphans{1: {Revision: "v1"}}},
			want: []Action{retarget(0, "v2", ReasonInPlace)},
		},
		{
			name: "not under RoleRollingUpdate",
			in:   Input{Spec: inPlace(roleMode(rollout(3, 1, 0, 0))), Groups: old3},
		},
	})
}

// A group whose revision cannot be compared with the target is neither rolled
// nor counted as rolled.
func TestPlanRevisionUnknown(t *testing.T) {
	unknown := func(g *Group) { g.RevisionUnknown = true }
	runPlanCases(t, []planCase{
		{
			name: "not replaced, and no surge is made for it",
			in:   Input{Spec: rollout(3, 1, 1, 0), Groups: []Group{group(0, "v1", unknown), group(1, "v1", unknown), group(2, "v1", unknown)}},
		},
		{
			name: "the groups that can be compared are rolled",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v1", unknown), group(1, "v1"), group(2, "v2")}},
			want: []Action{del(1, ReasonReplace)},
		},
		{
			name: "the baseline does not advance over it",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v2"), group(2, "v1", unknown)}},
		},
		{
			name: "it is still deleted when it failed",
			in:   Input{Spec: rollout(3, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v1", unknown, drained), group(2, "v2")}},
			want: []Action{del(1, ReasonFailed)},
		},
		{
			name: "it is still scaled in",
			in:   Input{Spec: rollout(1, 1, 0, 0), Groups: []Group{group(0, "v1", unknown), group(1, "v1", unknown)}},
			want: []Action{del(1, ReasonScaleDown)},
		},
		{
			name: "it is still replaced when the group itself asks to be recreated",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v1", unknown, recreate), group(1, "v2")}},
			want: []Action{del(0, ReasonReplace)},
		},
		{
			name: "without a member it gets the revision of its ordinal",
			in:   Input{Spec: rollout(2, 1, 0, 0), Groups: []Group{group(0, "v2"), group(1, "v1", unknown, memberless)}},
			want: []Action{retarget(1, "v2", ReasonMemberless)},
		},
	})

	spec := rollout(3, 1, 0, 0)
	view := Classify(spec, []Group{group(0, "v1", unknown), group(1, "v1", unknown, notReady), group(2, "v2")}, nil)
	if view.Classes[0] != ClassProtected || view.Work || view.NotReadyTarget != 0 || view.Ready != 2 {
		t.Errorf("Classify() = %+v, want the unknown groups protected, no work, and the one that is not ready off the ready count", view)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	in := Input{
		Spec: rollout(4, 2, 1, 1),
		Groups: []Group{
			group(0, "v0", memberless),
			group(1, "v1", notReady),
			group(2, "v1", drained),
			group(4, "v1", cost(-1)),
			group(5, "v2", surge, unobserved),
			group(6, "v1", surge),
			group(7, "v1"),
		},
		Orphans: map[int]Orphans{7: {Revision: "v1"}, 9: {Revision: "v1", Marked: true}},
		Foreign: []int{3, 8},
	}
	before := cloneInput(in)
	want := Plan(in)
	if len(want) == 0 {
		t.Fatal("the input is meant to produce writes")
	}
	if !reflect.DeepEqual(in, before) {
		t.Errorf("Plan modified its input\n got: %+v\nwant: %+v", in, before)
	}
	rng := rand.New(rand.NewSource(10))
	for i := range 50 {
		shuffled := cloneInput(in)
		rng.Shuffle(len(shuffled.Groups), func(a, b int) { shuffled.Groups[a], shuffled.Groups[b] = shuffled.Groups[b], shuffled.Groups[a] })
		slices.Reverse(shuffled.Foreign)
		if got := Plan(shuffled); !slices.Equal(got, want) {
			t.Fatalf("shuffle %d: Plan depends on the order of its input\n got: %+v\nwant: %+v", i, got, want)
		}
	}
}

func TestClassify(t *testing.T) {
	tests := []struct {
		name     string
		spec     Spec
		groups   []Group
		awaiting map[int]bool
		want     View
	}{
		{
			name: "no group",
			spec: rollout(3, 1, 1, 0),
			want: View{Classes: map[int]Class{}, Floor: 2},
		},
		{
			name: "every class under ServingGroupRollingUpdate",
			spec: rollout(5, 2, 1, 2),
			groups: []Group{
				group(0, "v1"),                        // protected
				group(1, "v1", recreate),              // replaceable below the partition
				group(2, "v1", notReady),              // replaceable
				group(3, "v2", notReady),              // target, not Ready
				group(4, "v2", unobserved),            // target, not settled
				group(5, "v1", memberless),            // memberless
				group(6, "v1", surge),                 // replaceable surge
				group(7, "v2", surge, terminating),    // terminating
				group(8, "v2", failed),                // failed
				group(9, "v2", empty),                 // memberless, and Ready
				group(10, "v2", memberless, recreate), // memberless: RecreateRequired needs members
			},
			awaiting: map[int]bool{2: true},
			want: View{
				Classes: map[int]Class{
					0: ClassProtected, 1: ClassReplaceable, 2: ClassReplaceable, 3: ClassTarget, 4: ClassTarget, 5: ClassMemberless,
					6: ClassReplaceable, 7: ClassTerminating, 8: ClassFailed, 9: ClassMemberless, 10: ClassMemberless,
				},
				Active: 11, Live: 9, Ready: 4, Regular: 9, RegularLive: 7, Surge: 1, NotReadyTarget: 3,
				Floor: 3, MaxScaleDown: 3, MaxHealthyScaleDown: 1, Work: true, ScalingDown: true,
			},
		},
		{
			name: "nothing below the partition is replaceable while the baseline cannot be read",
			spec: noBaseline(rollout(2, 1, 0, 1)),
			groups: []Group{
				group(0, "v1", recreate),
				group(1, "v1"),
			},
			want: View{
				Classes: map[int]Class{0: ClassProtected, 1: ClassReplaceable},
				Active:  2, Live: 2, Ready: 2, Regular: 2, RegularLive: 2,
				Floor: 1, MaxScaleDown: 1, MaxHealthyScaleDown: 1, Work: true,
			},
		},
		{
			name: "a replaceable surge group alone is no work",
			spec: rollout(1, 0, 1, 0),
			groups: []Group{
				group(0, "v2"),
				group(1, "v1", surge),
			},
			want: View{
				Classes: map[int]Class{0: ClassTarget, 1: ClassReplaceable},
				Active:  2, Live: 2, Ready: 2, Regular: 1, RegularLive: 1, Surge: 1,
				Floor: 1, MaxScaleDown: 1, MaxHealthyScaleDown: 1,
			},
		},
		{
			name: "RoleRollingUpdate: no revision classes, every group is regular",
			spec: roleMode(rollout(2, 1, 1, 1)),
			groups: []Group{
				group(0, "v1"),
				group(1, "v1", recreate, notReady),
				group(2, "v2", surge, notReady),
				group(3, "v1", memberless),
			},
			want: View{
				Classes: map[int]Class{0: ClassTarget, 1: ClassTarget, 2: ClassTarget, 3: ClassMemberless},
				Active:  4, Live: 4, Ready: 1, Regular: 4, RegularLive: 4, NotReadyTarget: 1,
				Floor: 1, MaxScaleDown: 2, ScalingDown: true,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Classify(tt.spec, tt.groups, tt.awaiting); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Classify()\n got: %+v\nwant: %+v", got, tt.want)
			}
		})
	}
}
