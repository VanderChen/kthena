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
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// patchStart is when the in-place updates of the tests were started: 1000
// seconds before testNow.
var patchStart = time.Unix(1700000000, 0)

// servePod returns a ready member Pod of the Role serve that runs rev-a, as
// fakeRevisions renders it.
func servePod(instance, index int, mutate ...func(*corev1.Pod)) *corev1.Pod {
	base := func(pod *corev1.Pod) {
		pod.Spec.Containers[0].Image = "model:rev-a"
		pod.Status.ContainerStatuses[0].Image = "model:rev-a"
	}
	return memberPod("serve", instance, index, append([]func(*corev1.Pod){base}, mutate...)...)
}

// patchedFor marks the Pod as patched in place for revision. The kubelet has
// not restarted the container yet: the Pod still reports the old one ready.
func patchedFor(revision string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		pod.Spec.Containers[0].Image = "model:" + revision
		pod.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: fmt.Sprintf(
			`{"revision":%q,"startedAt":%q,"patched":{"model":{"image":"model:%s","prevContainerID":"containerd://a","prevImageID":"sha256:1"}},"expected":{"model":1}}`,
			revision, patchStart.UTC().Format(time.RFC3339), revision)}
	}
}

// restartedOnce: the container runs the image of the spec in a new container.
func restartedOnce(pod *corev1.Pod) {
	pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: "model", RestartCount: 1, ContainerID: "containerd://b", Image: pod.Spec.Containers[0].Image, ImageID: "sha256:2",
	}
}

// crashedAfterwards: one more restart than the update accounts for.
func crashedAfterwards(pod *corev1.Pod) {
	restartedOnce(pod)
	pod.Status.ContainerStatuses[0].RestartCount = 2
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://c"
}

func labelled(revision string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Labels[workloadv1alpha1.RevisionLabelKey] = revision }
}

// instanceOf returns the established instance serve-<index> with the Pods as
// its members, classified without a fault tracker and with recovery enabled.
func instanceOf(index int, pods ...*corev1.Pod) Instance {
	inst := Instance{Role: "serve", Index: index, Entry: &workloadv1alpha1.RoleInstanceStatus{Role: "serve", Index: int32(index), Phase: established}}
	for ordinal, pod := range pods {
		state, _ := ClassifyMember(pod, MemberContext{Now: testNow, Recovery: Recovery{Enabled: true}})
		inst.Members = append(inst.Members, Member{Pod: pod, Membership: Owned, State: state, Ordinal: ordinal})
	}
	return inst
}

func TestClassifyMemberPastTheProgressDeadline(t *testing.T) {
	enabled := Recovery{Enabled: true, Grace: 60 * time.Second}
	immediate := Recovery{Enabled: true}
	disabled := Recovery{Enabled: false}
	unfinished := servePod(0, 0, patchedFor("rev-b"))
	unpatched := servePod(0, 1)

	tests := []struct {
		name      string
		pod       *corev1.Pod
		recovery  Recovery
		overdue   bool
		want      MemberState
		wantRetry time.Duration
	}{
		{name: "an update within its deadline is not a fault", pod: unfinished, recovery: immediate, want: MemberUpdating},
		{name: "an update past its deadline is a fault", pod: unfinished, recovery: immediate, overdue: true, want: MemberFaulty},
		{name: "which waits for the grace period", pod: unfinished, recovery: enabled, overdue: true, want: MemberFailing, wantRetry: 60 * time.Second},
		{name: "and is none where no Pod is deleted for a fault", pod: unfinished, recovery: disabled, overdue: true, want: MemberUpdating},
		{name: "a Pod that was never patched is healthy", pod: unpatched, recovery: enabled, want: MemberReady},
		{name: "past the deadline of its unit it counts at once", pod: unpatched, recovery: enabled, overdue: true, want: MemberFaulty},
		{name: "unless no Pod is deleted for a fault", pod: unpatched, recovery: disabled, overdue: true, want: MemberReady},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := MemberContext{Now: testNow, Recovery: tt.recovery, Faults: NewFaultTracker()}
			if tt.overdue {
				ctx.Overdue = sets.New(tt.pod.UID)
			}
			got, retry := ClassifyMember(tt.pod, ctx)
			wantRetry := time.Time{}
			if tt.wantRetry > 0 {
				wantRetry = testNow.Add(tt.wantRetry)
			}
			if got != tt.want || !retry.Equal(wantRetry) {
				t.Errorf("state = %s, retry %v; want %s, %v", got, retry, tt.want, wantRetry)
			}
		})
	}
}

func TestCheckInPlaceDeadlines(t *testing.T) {
	group := func(deadline *int32, roleMode bool) *workloadv1alpha1.ServingGroup {
		g := &workloadv1alpha1.ServingGroup{Spec: workloadv1alpha1.ServingGroupSpec{
			Revision:        "rev-b",
			RolloutStrategy: &workloadv1alpha1.ServingGroupRolloutStrategy{ProgressDeadlineSeconds: deadline},
		}}
		if roleMode {
			g.Spec.Revision, g.Spec.TargetRevision = "", "rev-b"
			g.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
		}
		return g
	}
	// The updates were started 1000 seconds ago.
	short, long := ptr.To[int32](600), ptr.To[int32](1500)
	finished := func(instance, index int) *corev1.Pod {
		return servePod(instance, index, patchedFor("rev-b"), restartedOnce)
	}
	uid := func(pods ...*corev1.Pod) []types.UID {
		var out []types.UID
		for _, pod := range pods {
			out = append(out, pod.UID)
		}
		return out
	}

	stuck := servePod(0, 1, patchedFor("rev-b"))
	never := servePod(0, 2)
	other := servePod(1, 0)
	crashed := servePod(0, 1, patchedFor("rev-b"), crashedAfterwards)
	tests := []struct {
		name     string
		deadline *int32
		roleMode bool
		disabled bool
		pods     []*corev1.Pod
		overdue  []types.UID
		next     time.Duration // after the start of the update; zero: none
		timedOut bool
	}{
		{name: "no update", deadline: short, pods: []*corev1.Pod{servePod(0, 0), servePod(0, 1)}},
		{name: "an update within its deadline: the next round is due when it passes",
			deadline: long, pods: []*corev1.Pod{finished(0, 0), stuck}, next: 1500 * time.Second},
		{name: "without a deadline in the spec it is 1800 seconds",
			pods: []*corev1.Pod{finished(0, 0), stuck}, next: 1800 * time.Second},
		{name: "past the deadline: the Pods that did not come up and the ones never patched",
			deadline: short, pods: []*corev1.Pod{finished(0, 0), stuck, never}, overdue: uid(stuck, never), timedOut: true},
		{name: "an update that is finished has no deadline, whenever the labels change",
			deadline: short, pods: []*corev1.Pod{finished(0, 0), finished(0, 1)}},
		{name: "a Pod that was created from the revision of the group was not left out",
			deadline: short, pods: []*corev1.Pod{finished(0, 0), servePod(0, 1, labelled("rev-b"))}},
		{name: "a terminating member: the recovery policy acts first",
			deadline: short, pods: []*corev1.Pod{finished(0, 0), stuck, servePod(0, 2, terminating)}},
		{name: "a whole group is one unit, whatever instance a Pod belongs to",
			deadline: short, pods: []*corev1.Pod{stuck, other}, overdue: uid(stuck, other), timedOut: true},
		{name: "rolled Role by Role, every instance is a unit of its own",
			deadline: short, roleMode: true, pods: []*corev1.Pod{stuck, other}, overdue: uid(stuck), timedOut: true},
		{name: "a crash on the new image keeps the update unfinished",
			deadline: short, pods: []*corev1.Pod{finished(0, 0), crashed}, overdue: uid(crashed), timedOut: true},
		{name: "but not where no Pod is ever deleted for a fault",
			deadline: short, disabled: true, pods: []*corev1.Pod{finished(0, 0), crashed}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CheckInPlaceDeadlines(group(tt.deadline, tt.roleMode), tt.pods, !tt.disabled, testNow)
			if want := sets.New(tt.overdue...); !got.Overdue.Equal(want) {
				t.Errorf("overdue = %v, want %v", sets.List(got.Overdue), sets.List(want))
			}
			wantNext := time.Time{}
			if tt.next > 0 {
				wantNext = patchStart.Add(tt.next)
			}
			if !got.Next.Equal(wantNext) {
				t.Errorf("next = %v, want %v", got.Next, wantNext)
			}
			if timedOut := !got.TimedOut.IsZero(); timedOut != tt.timedOut || timedOut && !got.TimedOut.Equal(patchStart) {
				t.Errorf("timed out = %v, want %t", got.TimedOut, tt.timedOut)
			}
		})
	}
}

// stepOf renders an InPlaceStep: "patch[<pod>=<image> ...] complete[<pod> ...]
// obstacle[...] waiting[...]", each part only when it is set, and "idle" for a
// unit without an update.
func stepOf(step InPlaceStep) string {
	if !step.Active {
		return "idle"
	}
	short := func(pod *corev1.Pod) string { return strings.TrimPrefix(pod.Name, testGroup+"-serve-") }
	var parts []string
	if len(step.Patch) > 0 {
		var items []string
		for _, patch := range step.Patch {
			items = append(items, short(patch.Pod)+"="+patch.Images["model"])
		}
		parts = append(parts, "patch["+strings.Join(items, " ")+"]")
	}
	if len(step.Complete) > 0 {
		var items []string
		for _, pod := range step.Complete {
			items = append(items, short(pod))
		}
		parts = append(parts, "complete["+strings.Join(items, " ")+"]")
	}
	if step.Obstacle != "" {
		obstacle := "obstacle[" + step.Obstacle + "]"
		if step.Fallback {
			obstacle += " fallback"
		}
		parts = append(parts, obstacle)
	}
	if step.Waiting != "" {
		parts = append(parts, "waiting["+step.Waiting+"]")
	}
	return strings.Join(parts, " ")
}

func TestPlanInPlace(t *testing.T) {
	const waitingForMembers = "waiting[every member of the unit to be there]"
	unit := func(target string, pods ...*corev1.Pod) InPlaceUnit {
		return InPlaceUnit{Instances: []Instance{instanceOf(0, pods...)}, Target: target, Allowed: true, Settled: true, RecoveryEnabled: true}
	}
	change := func(u InPlaceUnit, mutate func(*InPlaceUnit)) InPlaceUnit {
		mutate(&u)
		return u
	}
	patched, done := patchedFor("rev-b"), func(pod *corev1.Pod) { patchedFor("rev-b")(pod); restartedOnce(pod) }

	tests := []struct {
		name      string
		unit      InPlaceUnit
		revisions fakeRevisions
		want      string
	}{
		{name: "every Pod runs the revision: nothing to do",
			unit: unit("rev-a", servePod(0, 0), servePod(0, 1)), want: "idle"},
		{name: "every Pod is patched, with the image of the target",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1)), want: "patch[0-0=model:rev-b 0-1=model:rev-b]"},
		{name: "a unit that is patched in part is patched to the end",
			unit: unit("rev-b", servePod(0, 0, patched), servePod(0, 1)), want: "patch[0-1=model:rev-b]"},
		{name: "patched Pods that have not restarted are waited for",
			unit: unit("rev-b", servePod(0, 0, done), servePod(0, 1, patched)), want: "waiting[Pod llama-0-serve-0-1 to run its target images and be ready]"},
		{name: "every Pod runs the new image: the labels change",
			unit: unit("rev-b", servePod(0, 0, done), servePod(0, 1, done)), want: "complete[0-0 0-1]"},
		{name: "a Pod whose labels changed already is not patched again",
			unit: unit("rev-b", servePod(0, 0, done), servePod(0, 1, labelled("rev-b"))), want: "complete[0-0]"},
		{name: "the labels do not change while a member may be missing",
			unit: change(unit("rev-b", servePod(0, 0, done), servePod(0, 1, done)), func(u *InPlaceUnit) { u.Settled = false }), want: waitingForMembers},
		{name: "a crash on the new image is recovered before the labels change",
			unit: unit("rev-b", servePod(0, 0, done), servePod(0, 1, patched, crashedAfterwards)), want: "waiting[Pod llama-0-serve-0-1 to run its target images and be ready]"},
		{name: "where no Pod is deleted for a fault, a Pod that came up after a crash is done",
			unit: change(unit("rev-b", servePod(0, 0, done), servePod(0, 1, patched, crashedAfterwards)), func(u *InPlaceUnit) { u.RecoveryEnabled = false }),
			want: "complete[0-0 0-1]"},
		{name: "the target moved on: the Pods are patched again",
			unit: unit("rev-c", servePod(0, 0, done), servePod(0, 1, patched)), want: "patch[0-0=model:rev-c 0-1=model:rev-c]"},
		{name: "the revision went back to the one of the labels: the unit is recreated, and that is no fallback",
			unit: unit("rev-a", servePod(0, 0, patched), servePod(0, 1)),
			want: "obstacle[the revision went back to rev-a after the images of its Pods were patched for another revision]"},
		{name: "a terminating member is waited for",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1, terminating)), want: "waiting[a terminating member]"},
		{name: "a member that is not adopted yet is waited for",
			unit: change(unit("rev-b", servePod(0, 0), servePod(0, 1)), func(u *InPlaceUnit) { u.Instances[0].Members[1].Membership = Legacy }),
			want: "waiting[members that are not adopted yet]"},
		{name: "the update method is not InPlace: nothing is patched",
			unit: change(unit("rev-b", servePod(0, 0, patched), servePod(0, 1)), func(u *InPlaceUnit) { u.Allowed = false }),
			want: "obstacle[the update method that takes effect is not InPlace]"},
		{name: "but an update whose Pods are all patched is finished",
			unit: change(unit("rev-b", servePod(0, 0, done), servePod(0, 1, done)), func(u *InPlaceUnit) { u.Allowed = false }), want: "complete[0-0 0-1]"},
		{name: "the revisions differ in more than images",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1)), revisions: fakeRevisions{env: map[string]string{"rev-b": "changed"}},
			want: "obstacle[changes other than container images] fallback"},
		{name: "the same judged for the Role of the unit alone",
			unit:      change(unit("rev-b", servePod(0, 0), servePod(0, 1)), func(u *InPlaceUnit) { u.Role = "serve" }),
			revisions: fakeRevisions{env: map[string]string{"rev-b": "changed"}},
			want:      "obstacle[changes other than container images] fallback"},
		{name: "the Pods are not restarted by the kubelet",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1, func(pod *corev1.Pod) { pod.Spec.RestartPolicy = corev1.RestartPolicyOnFailure })),
			want: "obstacle[pod llama-0-serve-0-1 restartPolicy is not Always] fallback"},
		{name: "a container of the template is missing in the Pod",
			unit: unit("rev-b", servePod(0, 0, func(pod *corev1.Pod) { pod.Spec.Containers[0].Name = "renamed" }), servePod(0, 1)),
			want: "obstacle[pod llama-0-serve-0-0 has no container [model] of the target template] fallback"},
		{name: "the image of a Pod is neither the old nor the new one",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1, func(pod *corev1.Pod) { pod.Spec.Containers[0].Image = "model:rewritten" })),
			want: "obstacle[pod llama-0-serve-0-1 runs images of containers [model] that match neither the old nor the new template] fallback"},
		{name: "a revision that cannot be read is no reason to recreate",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1)), revisions: fakeRevisions{missing: map[string]bool{"rev-a": true}},
			want: `waiting[revision "rev-a" of Pod llama-0-serve-0-0, which cannot be resolved]`},
		{name: "nor is a target that cannot be read",
			unit: unit("rev-b", servePod(0, 0), servePod(0, 1)), revisions: fakeRevisions{missing: map[string]bool{"rev-b": true}},
			want: "waiting[revision rev-b, which cannot be resolved]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			step := PlanInPlace(tt.unit, tt.revisions)
			if got := stepOf(step); got != tt.want {
				t.Errorf("step = %s\nwant   %s", got, tt.want)
			}
			if step.Revision != tt.unit.Target {
				t.Errorf("revision = %q, want %q", step.Revision, tt.unit.Target)
			}
		})
	}
}

// inPlaceInstances returns three established instances of serve at rev-a,
// with Pods that fakeRevisions renders.
func inPlaceInstances(mutate map[int]func(*corev1.Pod)) []Instance {
	var instances []Instance
	for index := range 3 {
		var extra []func(*corev1.Pod)
		if m := mutate[index]; m != nil {
			extra = append(extra, m)
		}
		instances = append(instances, instanceOf(index, servePod(index, 0, extra...), servePod(index, 1, extra...)))
	}
	return instances
}

func TestPlanRolesUpdatesInPlace(t *testing.T) {
	patched := patchedFor("rev-b")
	done := func(pod *corev1.Pod) { patched(pod); restartedOnce(pod) }
	one, zero := intOrString("1"), intOrString("0")
	tests := []struct {
		name      string
		role      workloadv1alpha1.ServingGroupRole
		method    workloadv1alpha1.UpdateMethod
		target    string
		instances []Instance
		revisions fakeRevisions
		wants     string
		inPlace   string
		fallback  string
	}{
		{name: "instances are updated in place within the budget, the highest index first",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, instances: inPlaceInstances(nil),
			wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-b"},
		{name: "without maxUnavailable a Role is not limited",
			instances: inPlaceInstances(nil), wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "0>rev-b 1>rev-b 2>rev-b"},
		{name: "a budget of no unavailable instance: the Role is rolled by recreation, with its surge",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: zero, MaxSurge: one}, instances: inPlaceInstances(nil),
			wants: "0@rev-a 1@rev-a 2@rev-a 3@rev-b+"},
		{name: "the update method that takes effect is Recreate",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, method: workloadv1alpha1.RecreateUpdateMethod, instances: inPlaceInstances(nil),
			wants: "0@rev-a 1@rev-a"},
		{name: "an update under way keeps its share of the budget until the labels change",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, instances: inPlaceInstances(map[int]func(*corev1.Pod){2: done}),
			wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-b"},
		{name: "the next instance follows when they have",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: one},
			instances: inPlaceInstances(map[int]func(*corev1.Pod){2: func(pod *corev1.Pod) { labelled("rev-b")(pod); pod.Spec.Containers[0].Image = "model:rev-b" }}),
			wants:     "0@rev-a 1@rev-a 2@rev-b", inPlace: "1>rev-b"},
		{name: "an update under way is carried on below a partition that was raised",
			role: workloadv1alpha1.ServingGroupRole{Partition: intOrString("3")}, instances: inPlaceInstances(map[int]func(*corev1.Pod){2: patched}),
			wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-b"},
		{name: "instances below the partition are not updated",
			role: workloadv1alpha1.ServingGroupRole{Partition: intOrString("2")}, instances: inPlaceInstances(nil),
			wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-b"},
		{name: "an instance that cannot be updated in place is recreated, within the budget",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, instances: inPlaceInstances(nil),
			revisions: fakeRevisions{env: map[string]string{"rev-b": "changed"}},
			wants:     "0@rev-a 1@rev-a", fallback: "2:changes other than container images"},
		{name: "asking does not make an instance below the partition replaceable",
			role: workloadv1alpha1.ServingGroupRole{Partition: intOrString("2")}, instances: inPlaceInstances(nil),
			revisions: fakeRevisions{env: map[string]string{"rev-b": "changed"}},
			wants:     "0@rev-a 1@rev-a", fallback: "2:changes other than container images"},
		{name: "the target moved on: the update under way is patched again",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, target: "rev-c", instances: inPlaceInstances(map[int]func(*corev1.Pod){2: patched}),
			wants: "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-c"},
		{name: "the Role went back to the template of the labels: the instance is recreated, and that is no fallback",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, target: "rev-a", instances: inPlaceInstances(map[int]func(*corev1.Pod){2: patched}),
			wants: "0@rev-a 1@rev-a"},
		{name: "the update method became Recreate: an instance that is patched in part is recreated",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, method: workloadv1alpha1.RecreateUpdateMethod,
			instances: func() []Instance {
				instances := inPlaceInstances(nil)
				instances[2] = instanceOf(2, servePod(2, 0, patched), servePod(2, 1))
				return instances
			}(),
			wants: "0@rev-a 1@rev-a"},
		{name: "one whose Pods are all patched is finished",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: one}, method: workloadv1alpha1.RecreateUpdateMethod,
			instances: inPlaceInstances(map[int]func(*corev1.Pod){2: patched}),
			wants:     "0@rev-a 1@rev-a 2@rev-a", inPlace: "2>rev-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := tt.role
			group := planGroup(3, &role)
			group.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
			if tt.method != "" {
				group.Spec.RolloutStrategy.UpdateMethod = tt.method
			}
			if tt.target != "" {
				group.Spec.TargetRevision = tt.target
			}
			plan, err := PlanRoles(group, tt.instances, tt.revisions)
			if err != nil {
				t.Fatal(err)
			}
			if got := wantsOf(plan); got != tt.wants {
				t.Errorf("wants = %q, want %q", got, tt.wants)
			}
			var inPlace, fallback []string
			for key, revision := range plan.InPlace {
				inPlace = append(inPlace, fmt.Sprintf("%d>%s", key.Index, revision))
			}
			for key, reason := range plan.Fallback {
				fallback = append(fallback, fmt.Sprintf("%d:%s", key.Index, reason))
			}
			sort.Strings(inPlace)
			sort.Strings(fallback)
			if got := strings.Join(inPlace, " "); got != tt.inPlace {
				t.Errorf("in place = %q, want %q", got, tt.inPlace)
			}
			if got := strings.Join(fallback, " "); got != tt.fallback {
				t.Errorf("fallback = %q, want %q", got, tt.fallback)
			}
		})
	}
}

// A group whose Pods all report ready is not ready while one of its instances
// is being updated in place.
func TestComputeStatusOfAnInstanceThatIsUpdatedInPlace(t *testing.T) {
	updating := func(in *StatusInput) { in.Updating = map[InstanceKey]bool{{Role: "prefill", Index: 0}: true} }
	status := statusOf(statusGroup(), ledgerOf(established, 0), threePods(), updating)
	value, reason := conditionOf(t, status, condReady)
	if value != "False" || reason != ReasonInstancesNotReady || status.ReadyPods != 3 {
		t.Errorf("Ready = %s (%s), readyPods %d; want False (%s), 3", value, reason, status.ReadyPods, ReasonInstancesNotReady)
	}
	for _, condition := range status.Conditions {
		if condition.Type == condReady && condition.Message != "instance prefill-0 is being updated in place" {
			t.Errorf("message = %q", condition.Message)
		}
	}
	if value, _ := conditionOf(t, statusOf(statusGroup(), ledgerOf(established, 0), threePods()), condReady); value != "True" {
		t.Errorf("Ready without an update = %s", value)
	}

	// The revision the group reports is the one of the labels; the target of
	// the unfinished update is referenced.
	pods := threePods(func(pod *corev1.Pod) { patchedFor("rev-b")(pod) })
	status = statusOf(statusGroup(), ledgerOf(established, 0), pods, updating)
	if status.Revision != "rev-a" || !reflect.DeepEqual(status.ReferencedRevisions, []string{"rev-a", "rev-b"}) {
		t.Errorf("revision = %q, referenced = %v", status.Revision, status.ReferencedRevisions)
	}
}

func TestInstanceSettled(t *testing.T) {
	want := &Want{Workers: 1, LayoutKnown: true}
	complete := func() Instance { return instanceOf(0, servePod(0, 0), servePod(0, 1)) }
	tests := []struct {
		name string
		inst Instance
		want *Want
		ok   bool
	}{
		{name: "established with every member", inst: complete(), want: want, ok: true},
		{name: "not wanted", inst: complete()},
		{name: "the layout is unknown", inst: complete(), want: &Want{Workers: 1}},
		{name: "a member is missing", inst: instanceOf(0, servePod(0, 0)), want: want},
		{name: "a member is terminating", inst: instanceOf(0, servePod(0, 0), servePod(0, 1, terminating)), want: want},
		{name: "a member too many", inst: instanceOf(0, servePod(0, 0), servePod(0, 1), servePod(0, 2)), want: want},
		{name: "being created", inst: func() Instance { i := complete(); i.Entry.Phase = creating; return i }(), want: want},
		{name: "being drained", inst: func() Instance { i := complete(); i.Entry.Phase = draining; return i }(), want: want},
		{name: "a member is not adopted", inst: func() Instance { i := complete(); i.Members[1].Membership = Orphan; return i }(), want: want},
	}
	for _, tt := range tests {
		if got := tt.inst.Settled(tt.want); got != tt.ok {
			t.Errorf("%s: Settled() = %t, want %t", tt.name, got, tt.ok)
		}
	}
}
