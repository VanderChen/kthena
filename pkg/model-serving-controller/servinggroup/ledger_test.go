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
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	creating    = workloadv1alpha1.RoleInstanceCreating
	established = workloadv1alpha1.RoleInstanceEstablished
	draining    = workloadv1alpha1.RoleInstanceDraining
)

// member builds an owned member of prefill-0 at the given ordinal.
func member(ordinal int, state MemberState, generation int64) Member {
	pod := memberPod("prefill", 0, ordinal, func(p *corev1.Pod) {
		p.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = strconv.FormatInt(generation, 10)
	})
	return Member{Pod: pod, Membership: Owned, State: state, Ordinal: ordinal}
}

func adoptable(ordinal int) Member {
	return Member{Pod: memberPod("prefill", 0, ordinal, noOwner), Membership: Orphan, Ordinal: ordinal}
}

func entry(phase workloadv1alpha1.RoleInstancePhase, generation int64) *workloadv1alpha1.RoleInstanceStatus {
	return &workloadv1alpha1.RoleInstanceStatus{Role: "prefill", Index: 0, Phase: phase, Generation: generation}
}

func inst(e *workloadv1alpha1.RoleInstanceStatus, members ...Member) Instance {
	return Instance{Role: "prefill", Index: 0, Entry: e, Members: members}
}

func names(pods []*corev1.Pod) string {
	var out []string
	for _, pod := range pods {
		out = append(out, strings.TrimPrefix(pod.Name, testGroup+"-prefill-0-"))
	}
	return strings.Join(out, ",")
}

func TestTransition(t *testing.T) {
	const (
		none  = workloadv1alpha1.NoneRestartPolicy
		role  = workloadv1alpha1.RoleRecreate
		group = workloadv1alpha1.ServingGroupRecreate
	)
	two := &Want{Workers: 2, LayoutKnown: true} // entry and two workers
	unknown := &Want{}
	ledger := func(phase workloadv1alpha1.RoleInstancePhase, generation int64) *LedgerChange {
		return &LedgerChange{Phase: phase, Generation: generation}
	}

	tests := []struct {
		name  string
		inst  Instance
		want  *Want
		scope Scope

		rule       string
		ledger     *LedgerChange
		failGroup  string
		delete     string // ordinals, comma separated
		create     []int
		generation int64
		adopt      string
		roleDelete bool
		waiting    bool
		confirm    bool
	}{
		// E0, E1: the group as a whole is going away.
		{name: "E0 terminating group: nothing", inst: inst(entry(established, 0), member(0, MemberFaulty, 0)), want: two,
			scope: Scope{GroupTerminating: true, GroupFailed: true, Policy: role}, rule: "E0"},
		{name: "E1 failed group: only members with a fault are deleted",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFaulty, 0), member(2, MemberFailing, 0)), want: two,
			scope: Scope{GroupFailed: true, Policy: group}, rule: "E1", delete: "1,2", confirm: true},
		{name: "E1 failed group without a fault: nothing, not even creation",
			inst: inst(entry(established, 0), member(0, MemberReady, 0)), want: two,
			scope: Scope{GroupFailed: true, Policy: group}, rule: "E1"},
		{name: "E1 failed group does not adopt",
			inst: inst(nil, adoptable(0)), want: two, scope: Scope{GroupFailed: true, Policy: group}, rule: "E1"},

		// R2: adoption comes before every other transition of the instance.
		{name: "R2 adopt and do nothing else",
			inst: inst(entry(established, 0), member(0, MemberFaulty, 0), adoptable(1)), want: two,
			scope: Scope{Policy: role}, rule: "R2", adopt: "1"},

		// E2, E3: draining.
		{name: "E2 faulty members first",
			inst: inst(entry(draining, 3), member(0, MemberReady, 3), member(1, MemberFailing, 2), member(2, MemberFaulty, 3)), want: two,
			scope: Scope{Policy: none}, rule: "E2", delete: "1,2", confirm: true},
		{name: "E2 wait for a terminating member before the rest",
			inst: inst(entry(draining, 3), member(0, MemberReady, 3), member(1, MemberTerminating, 3)), want: two,
			scope: Scope{Policy: role}, rule: "E2", waiting: true, confirm: true},
		{name: "E2 then the rest",
			inst: inst(entry(draining, 3), member(0, MemberReady, 3), member(1, MemberNotReady, 0)), want: nil,
			scope: Scope{Policy: role}, rule: "E2", delete: "0,1", confirm: true},
		{name: "E2 leaves Pods of a later generation alone",
			inst: inst(entry(draining, 3), member(0, MemberReady, 3), member(1, MemberReady, 4)), want: two,
			scope: Scope{Policy: role}, rule: "E2", delete: "0", confirm: true},
		{name: "E3 drained and wanted: hook, next generation, create all",
			inst: inst(entry(draining, 3)), want: two,
			scope: Scope{Policy: role}, rule: "E3", roleDelete: true, ledger: ledger(creating, 4), create: []int{0, 1, 2}, generation: 4},
		{name: "E3 the next generation is above every Pod that exists",
			inst: inst(entry(draining, 3), member(1, MemberReady, 7)), want: two,
			scope: Scope{Policy: role}, rule: "E3", roleDelete: true, ledger: ledger(creating, 8), create: []int{0, 2}, generation: 8},
		{name: "E3 drained and wanted, template unknown: no creation",
			inst: inst(entry(draining, 3)), want: unknown,
			scope: Scope{Policy: role}, rule: "E3", roleDelete: true, ledger: ledger(creating, 4)},
		{name: "E3 drained and not wanted: hook, forget",
			inst: inst(entry(draining, 3)), want: nil,
			scope: Scope{Policy: role}, rule: "E3", roleDelete: true, ledger: &LedgerChange{Remove: true}},

		// E4: no longer wanted.
		{name: "E4 established and not wanted",
			inst: inst(entry(established, 1), member(0, MemberReady, 1), member(1, MemberReady, 1)), want: nil,
			scope: Scope{Policy: none}, rule: "E4", ledger: ledger(draining, 1), delete: "0,1", confirm: true},
		{name: "E4 Pods without a ledger entry and not wanted",
			inst: inst(nil, member(0, MemberReady, 2)), want: nil,
			scope: Scope{Policy: role}, rule: "E4", ledger: ledger(draining, 2), delete: "0", confirm: true},
		{name: "E4 a failing member goes first although recovery is disabled",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFailing, 0)), want: nil,
			scope: Scope{Policy: none}, rule: "E4", ledger: ledger(draining, 0), delete: "1", confirm: true},
		{name: "E4 an entry without Pods and not wanted",
			inst: inst(entry(creating, 0)), want: nil,
			scope: Scope{Policy: role}, rule: "E4", ledger: ledger(draining, 0), confirm: true},
		{name: "E9 neither entry nor Pods nor wanted",
			inst: inst(nil), want: nil, scope: Scope{Policy: role}, rule: "E9"},

		// L5: the ledger is behind the Pods.
		{name: "L5 raise the ledger to the generation of the Pods",
			inst: inst(entry(established, 1), member(0, MemberReady, 3)), want: two,
			scope: Scope{Policy: role}, rule: "L5", ledger: ledger(established, 3)},

		// E5: a fault that counts.
		{name: "E5 RoleRecreate: drain, faulty first",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFaulty, 0), member(2, MemberReady, 0)), want: two,
			scope: Scope{Policy: role}, rule: "E5", ledger: ledger(draining, 0), delete: "1", confirm: true},
		{name: "E5 RoleRecreate while creating",
			inst: inst(nil, member(0, MemberFaulty, 0)), want: two,
			scope: Scope{Policy: role}, rule: "E5", ledger: ledger(draining, 0), delete: "0", confirm: true},
		{name: "E5 ServingGroupRecreate: fail the group, delete nothing yet",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFaulty, 0), member(2, MemberReady, 0)), want: two,
			scope: Scope{Policy: group}, rule: "E5", failGroup: FailMemberFaulty, confirm: true},
		{name: "E5 wins over E6",
			inst: inst(entry(established, 0), member(0, MemberFaulty, 0)), want: two,
			scope: Scope{Policy: group}, rule: "E5", failGroup: FailMemberFaulty, confirm: true},
		{name: "a failing member triggers nothing",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFailing, 0), member(2, MemberReady, 0)), want: two,
			scope: Scope{Policy: role}, rule: "E9"},
		{name: "a faulty member under None triggers nothing",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberFaulty, 0), member(2, MemberReady, 0)), want: two,
			scope: Scope{Policy: none}, rule: "E9"},

		// E6: a member of an established instance is lost.
		{name: "E6 None: create the missing member",
			inst: inst(entry(established, 2), member(0, MemberReady, 2), member(2, MemberReady, 2)), want: two,
			scope: Scope{Policy: none}, rule: "E6", create: []int{1}, generation: 2, confirm: true},
		{name: "E6 None: wait for a terminating member",
			inst: inst(entry(established, 2), member(0, MemberReady, 2), member(1, MemberTerminating, 2), member(2, MemberReady, 2)), want: two,
			scope: Scope{Policy: none}, rule: "E6", generation: 2, waiting: true},
		{name: "E6 RoleRecreate: missing member",
			inst: inst(entry(established, 2), member(0, MemberReady, 2), member(2, MemberReady, 2)), want: two,
			scope: Scope{Policy: role}, rule: "E6", ledger: ledger(draining, 2), delete: "0,2", confirm: true},
		{name: "E6 RoleRecreate: terminating member counts as lost; the rest waits for it",
			inst: inst(entry(established, 2), member(0, MemberReady, 2), member(1, MemberTerminating, 2), member(2, MemberReady, 2)), want: two,
			scope: Scope{Policy: role}, rule: "E6", ledger: ledger(draining, 2), waiting: true, confirm: true},
		{name: "E6 ServingGroupRecreate",
			inst: inst(entry(established, 2), member(0, MemberReady, 2), member(2, MemberReady, 2)), want: two,
			scope: Scope{Policy: group}, rule: "E6", failGroup: FailMemberLost, confirm: true},
		{name: "E6 template unknown: cannot tell",
			inst: inst(entry(established, 2), member(0, MemberReady, 2)), want: unknown,
			scope: Scope{Policy: role}, rule: "E9"},

		// E7, E8: creating. A missing member is created whatever the policy is.
		{name: "E7 nothing exists yet",
			inst: inst(nil), want: two, scope: Scope{Policy: group}, rule: "E7", create: []int{0, 1, 2}},
		{name: "E7 missing member while creating is only created",
			inst: inst(entry(creating, 5), member(0, MemberReady, 5), member(2, MemberNotReady, 5)), want: two,
			scope: Scope{Policy: role}, rule: "E7", create: []int{1}, generation: 5},
		{name: "E7 without entry the generation is the one of the Pods",
			inst: inst(nil, member(0, MemberReady, 4)), want: two,
			scope: Scope{Policy: group}, rule: "E7", create: []int{1, 2}, generation: 4},
		{name: "E7 a terminating member holds its name",
			inst: inst(nil, member(0, MemberReady, 0), member(1, MemberTerminating, 0)), want: two,
			scope: Scope{Policy: role}, rule: "E7", create: []int{2}, waiting: true},
		{name: "E7 template unknown: wait",
			inst: inst(nil, member(0, MemberReady, 0)), want: unknown,
			scope: Scope{Policy: role}, rule: "E7", waiting: true},
		{name: "E8 complete: established, ready or not",
			inst: inst(nil, member(0, MemberReady, 0), member(1, MemberNotReady, 0), member(2, MemberUpdating, 0)), want: two,
			scope: Scope{Policy: role}, rule: "E8", ledger: ledger(established, 0)},
		{name: "E8 a Pod beyond the layout does not make the instance complete",
			inst: inst(entry(creating, 1), member(0, MemberReady, 1), member(1, MemberReady, 1), member(5, MemberReady, 1)), want: two,
			scope: Scope{Policy: role}, rule: "E7", create: []int{2}, generation: 1},
		{name: "E9 established and complete",
			inst: inst(entry(established, 0), member(0, MemberReady, 0), member(1, MemberNotReady, 0), member(2, MemberReady, 0)), want: two,
			scope: Scope{Policy: group}, rule: "E9"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Transition(tt.inst, tt.want, tt.scope)
			if got.Rule != tt.rule {
				t.Errorf("rule = %s, want %s", got.Rule, tt.rule)
			}
			if !reflect.DeepEqual(got.Ledger, tt.ledger) {
				t.Errorf("ledger = %+v, want %+v", got.Ledger, tt.ledger)
			}
			if got.FailGroup != tt.failGroup {
				t.Errorf("failGroup = %q, want %q", got.FailGroup, tt.failGroup)
			}
			if names(got.Delete) != tt.delete {
				t.Errorf("delete = %q, want %q", names(got.Delete), tt.delete)
			}
			if !reflect.DeepEqual(got.Create, tt.create) {
				t.Errorf("create = %v, want %v", got.Create, tt.create)
			}
			if len(got.Create) > 0 && got.Generation != tt.generation {
				t.Errorf("generation = %d, want %d", got.Generation, tt.generation)
			}
			if names(got.Adopt) != tt.adopt {
				t.Errorf("adopt = %q, want %q", names(got.Adopt), tt.adopt)
			}
			if got.RoleDelete != tt.roleDelete {
				t.Errorf("roleDelete = %t, want %t", got.RoleDelete, tt.roleDelete)
			}
			if (got.Waiting != "") != tt.waiting {
				t.Errorf("waiting = %q, want waiting=%t", got.Waiting, tt.waiting)
			}
			if got.Confirm != tt.confirm {
				t.Errorf("confirm = %t, want %t", got.Confirm, tt.confirm)
			}
			// A step that deletes a healthy member, or fails the group, must carry a
			// commit: the ledger entry of the instance or the Failed condition.
			if got.Rule != "E1" && got.Rule != "E2" && len(got.Delete) > 0 && got.Ledger == nil {
				t.Errorf("rule %s deletes Pods without writing the ledger first", got.Rule)
			}
		})
	}
}

// The zero value of the ledger must never lead to a deletion: an instance
// whose entry was lost is created again where needed, and nothing else.
func TestTransitionWithoutLedgerNeverDeletes(t *testing.T) {
	two := &Want{Workers: 2, LayoutKnown: true}
	states := []MemberState{MemberReady, MemberNotReady, MemberFailing, MemberUpdating, MemberTerminating}
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		for mask := 0; mask < 8; mask++ {
			for _, state := range states {
				var members []Member
				for ordinal := 0; ordinal < 3; ordinal++ {
					if mask&(1<<ordinal) != 0 {
						members = append(members, member(ordinal, state, 0))
					}
				}
				step := Transition(inst(nil, members...), two, Scope{Policy: policy})
				if len(step.Delete) > 0 || step.FailGroup != "" || (step.Ledger != nil && step.Ledger.Phase == draining) {
					t.Errorf("policy %s, members %03b in state %s: step %+v deletes although the ledger is empty", policy, mask, state, step)
				}
			}
		}
	}
}

func TestObserve(t *testing.T) {
	ledger := []workloadv1alpha1.RoleInstanceStatus{
		{Role: "prefill", Index: 0, Phase: established, Generation: 1},
		{Role: "decode", Index: 3, Phase: draining, Generation: 2},
	}
	stray := memberPod("prefill", 0, 2, func(p *corev1.Pod) { delete(p.Labels, workloadv1alpha1.RoleIDKey) })
	pods := []*corev1.Pod{
		memberPod("prefill", 1, 0, noOwner),
		memberPod("prefill", 0, 1, restarted),
		memberPod("prefill", 0, 0),
		memberPod("decode", 0, 0, ownedBy("ServingGroup", "llama-1", "other-uid")),
		stray,
	}
	parent := Parent{Name: testMS, UID: testMSUID, AdoptionDone: true}
	ctx := MemberContext{Now: testNow, Recovery: Recovery{Enabled: true, Grace: time.Minute}, Faults: NewFaultTracker()}

	instances, strays := Observe(pods, ledger, testGroup, testGroupUID, parent, ctx)

	if len(strays) != 1 || strays[0] != stray {
		t.Errorf("strays = %v, want the Pod without a role-id label", strays)
	}
	var got []string
	for _, i := range instances {
		line := i.Role + "-" + strconv.Itoa(i.Index) + ":" + string(i.Phase()) + "/" + strconv.FormatInt(i.Generation(), 10)
		for _, m := range i.Members {
			line += " " + strconv.Itoa(m.Ordinal) + "=" + string(m.State)
			if m.Membership.Adoptable() {
				line += "adopt"
			}
		}
		got = append(got, line)
	}
	want := []string{
		"decode-3:Draining/2",
		"prefill-0:Established/1 0=Ready 1=Failing",
		"prefill-1:Creating/0 0=adopt",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("instances:\n got %q\nwant %q", got, want)
	}
	if retry := instances[1].Members[1].RetryAt; !retry.Equal(testNow.Add(time.Minute)) {
		t.Errorf("retry of the failing member = %v, want one grace period from now", retry)
	}
}

func TestInstanceGenerationOfInstance(t *testing.T) {
	tests := []struct {
		name string
		inst Instance
		want int64
	}{
		{"no entry, no Pod", inst(nil), 0},
		{"no entry: the Pods", inst(nil, member(0, MemberReady, 2), member(1, MemberReady, 5)), 5},
		{"entry above the Pods", inst(entry(established, 7), member(0, MemberReady, 2)), 7},
		{"entry below the Pods", inst(entry(creating, 1), member(0, MemberReady, 2)), 2},
		{"draining keeps its generation", inst(entry(draining, 1), member(0, MemberReady, 2)), 1},
		{"Pods to adopt do not count", inst(nil, Member{Pod: memberPod("prefill", 0, 0, noOwner, func(p *corev1.Pod) {
			p.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = "9"
		}), Membership: Orphan}), 0},
	}
	for _, tt := range tests {
		if got := tt.inst.Generation(); got != tt.want {
			t.Errorf("%s: Generation() = %d, want %d", tt.name, got, tt.want)
		}
	}
}

func TestAllPodsLost(t *testing.T) {
	tests := []struct {
		name      string
		instances []Instance
		strays    []*corev1.Pod
		want      bool
	}{
		{"an established instance and no Pod", []Instance{inst(entry(established, 0)), inst(entry(creating, 0))}, nil, true},
		{"never established", []Instance{inst(entry(creating, 0)), inst(nil)}, nil, false},
		{"empty ledger", nil, nil, false},
		{"a terminating Pod is still a Pod", []Instance{inst(entry(established, 0), member(0, MemberTerminating, 0))}, nil, false},
		{"a Pod to adopt is still a Pod", []Instance{inst(entry(established, 0)), inst(nil, adoptable(0))}, nil, false},
		{"a stray Pod is still a Pod", []Instance{inst(entry(established, 0))}, []*corev1.Pod{memberPod("prefill", 0, 0)}, false},
	}
	for _, tt := range tests {
		if got := AllPodsLost(tt.instances, tt.strays); got != tt.want {
			t.Errorf("%s: AllPodsLost() = %t, want %t", tt.name, got, tt.want)
		}
	}
}

func TestApplyLedger(t *testing.T) {
	ledger := []workloadv1alpha1.RoleInstanceStatus{
		{Role: "prefill", Index: 1, Phase: established, Generation: 1},
		{Role: "decode", Index: 0, Phase: draining, Generation: 4},
		{Role: "prefill", Index: 0, Phase: established},
	}
	before := append([]workloadv1alpha1.RoleInstanceStatus(nil), ledger...)
	got := ApplyLedger(ledger, map[InstanceKey]LedgerChange{
		{Role: "decode", Index: 0}:  {Remove: true},
		{Role: "prefill", Index: 1}: {Phase: draining, Generation: 1},
		{Role: "prefill", Index: 2}: {Phase: creating, Generation: 0},
		{Role: "decode", Index: 9}:  {Remove: true},
	})
	want := []workloadv1alpha1.RoleInstanceStatus{
		{Role: "prefill", Index: 0, Phase: established},
		{Role: "prefill", Index: 1, Phase: draining, Generation: 1},
		{Role: "prefill", Index: 2, Phase: creating},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyLedger() = %+v, want %+v", got, want)
	}
	if !reflect.DeepEqual(ledger, before) {
		t.Error("ApplyLedger modified its input")
	}
}
