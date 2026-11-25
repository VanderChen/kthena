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

package sgcontroller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// coordinated turns ms into one whose groups roll the Roles prefill and
// decode together: each instance is an entry Pod and one worker, at most
// maxSkew apart, and prefill depends on decode when dependency is set.
func coordinated(prefill, decode, prefillUnavailable int32, maxSkew string, dependency bool) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		coordination := &workloadv1alpha1.RoleCoordination{MaxSkew: ptr.To(intstr.FromString(maxSkew))}
		if dependency {
			coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
		}
		ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate, RoleCoordination: coordination}
		role := func(name string, replicas, unavailable int32) workloadv1alpha1.Role {
			return workloadv1alpha1.Role{
				Name: name, Replicas: ptr.To(replicas), WorkerReplicas: 1,
				EntryTemplate: podTemplate("model:v1"), WorkerTemplate: ptr.To(podTemplate("model:v1")),
				RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt32(unavailable)), MaxSurge: ptr.To(intstr.FromInt32(0)),
				},
			}
		}
		ms.Spec.Template.Roles = []workloadv1alpha1.Role{role("prefill", prefill, prefillUnavailable), role("decode", decode, 1)}
	}
}

// withImages returns ms with the image of every Role changed.
func withImages(ms *workloadv1alpha1.ModelServing, image string) *workloadv1alpha1.ModelServing {
	changed := ms.DeepCopy()
	for i := range changed.Spec.Template.Roles {
		role := &changed.Spec.Template.Roles[i]
		role.EntryTemplate.Spec.Containers[0].Image = image
		if role.WorkerTemplate != nil {
			role.WorkerTemplate.Spec.Containers[0].Image = image
		}
	}
	return changed
}

// coordinatedHarness returns a harness whose group runs rev-a and knows
// rev-b, which changes the template of every Role. The group is told the
// replica counts of rev-a, as the ModelServing controller tells it.
func coordinatedHarness(t *testing.T, mutate ...func(*workloadv1alpha1.ModelServing)) *harness {
	t.Helper()
	ms := testModelServing(mutate...)
	group := groupOf(ms, "rev-a")
	for i := range group.Spec.Roles {
		group.Spec.Roles[i].BaselineReplicas = ptr.To(group.Spec.Roles[i].Replicas)
	}
	if strategy := ms.Spec.RolloutStrategy; strategy != nil && group.Spec.RolloutStrategy != nil {
		group.Spec.RolloutStrategy.UpdateMethod = strategy.UpdateMethod
	}
	return newHarness(t, ms, group, revisionOf(t, ms, "rev-a"), revisionOf(t, withImages(ms, "model:v2"), "rev-b"))
}

// quiesce runs rounds until one writes nothing. No Pod becomes ready in
// between: the test decides when a Pod is ready. With restarts set, every
// round is run by a controller that remembers nothing.
func (h *harness) quiesce() {
	h.t.Helper()
	for range 40 {
		h.cluster.Settle()
		if h.restarts {
			h.restart()
		}
		if len(h.mustRound()) == 0 {
			h.cluster.ResetWrites()
			return
		}
	}
	h.t.Fatalf("the group did not come to rest; last writes: %v", writesOf(h.cluster.Writes()))
}

// release makes the Pods of the Role that run the image ready: the entry Pods
// alone with entryOnly, else all of them.
func (h *harness) release(role, image string, entryOnly bool) {
	h.t.Helper()
	for _, pod := range h.pods() {
		if pod.Labels[workloadv1alpha1.RoleLabelKey] != role || pod.Spec.Containers[0].Image != image || pod.DeletionTimestamp != nil || utils.IsPodRunningAndReady(pod) {
			continue
		}
		if entryOnly && pod.Labels[workloadv1alpha1.EntryLabelKey] != utils.Entry {
			continue
		}
		h.run(pod.Name)
	}
}

// progress renders how far the Roles are at the image: "<role>:<instances
// that run it>/<of them ready> ...". An instance counts when both its Pods run
// the image, and is ready when both are ready. Any Pod that is missing or
// terminating shows as "!": the group is between two states.
func (h *harness) progress(image string) string {
	h.t.Helper()
	type instance struct{ pods, target, ready int }
	byRole := map[string]map[string]*instance{}
	unsettled := false
	for _, pod := range h.pods() {
		role, id := pod.Labels[workloadv1alpha1.RoleLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey]
		if byRole[role] == nil {
			byRole[role] = map[string]*instance{}
		}
		if byRole[role][id] == nil {
			byRole[role][id] = &instance{}
		}
		inst := byRole[role][id]
		inst.pods++
		unsettled = unsettled || pod.DeletionTimestamp != nil
		if pod.Spec.Containers[0].Image == image {
			inst.target++
			if utils.IsPodRunningAndReady(pod) {
				inst.ready++
			}
		}
	}
	var out []string
	for _, role := range []string{"prefill", "decode"} {
		started, ready := 0, 0
		for _, inst := range byRole[role] {
			unsettled = unsettled || inst.pods != 2 || inst.target == 1
			if inst.target == 2 {
				started++
				if inst.ready == 2 {
					ready++
				}
			}
		}
		out = append(out, fmt.Sprintf("%s:%d/%d", role, started, ready))
	}
	if unsettled {
		return "!" + strings.Join(out, " ")
	}
	return strings.Join(out, " ")
}

// blocked returns the reason of the CoordinatedRoleRolloutBlocked condition:
// the blocker while it is true, else the reason in brackets.
func (h *harness) blocked() string {
	h.t.Helper()
	condition := meta.FindStatusCondition(h.group().Status.Conditions, string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked))
	switch {
	case condition == nil:
		return ""
	case condition.Status == "True":
		return condition.Reason
	}
	return "(" + condition.Reason + ")"
}

// expectProgress brings the group to rest and compares how far its Roles are.
func (h *harness) expectProgress(what, want, wantBlocked string) {
	h.t.Helper()
	h.quiesce()
	if got := h.progress("model:v2"); got != want {
		h.t.Fatalf("%s: progress = %s, want %s", what, got, want)
	}
	if got := h.blocked(); got != wantBlocked {
		h.t.Errorf("%s: blocked = %q, want %q", what, got, wantBlocked)
	}
}

// A Role that depends on another one starts its first target instance only
// when the other has target capacity that is ready, and the last old instance
// of a dependency stays for as long as an old instance of its caller does.
// The steps are those of the end-to-end test of the dependency startup.
func TestCoordinatedDependencyStartup(t *testing.T) {
	for _, restarts := range []bool{false, true} {
		t.Run(fmt.Sprintf("a new controller for every round: %t", restarts), func(t *testing.T) {
			testCoordinatedDependencyStartup(t, restarts)
		})
	}
}

func testCoordinatedDependencyStartup(t *testing.T, restarts bool) {
	h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true))
	h.restarts = restarts
	h.settleContainers()
	if got := h.blocked(); got != "(RolloutComplete)" {
		t.Fatalf("before the rollout: blocked = %q", got)
	}
	original := h.uids()

	h.retarget("rev-b")
	h.expectProgress("the dependency can start, its caller cannot", "prefill:0/0 decode:1/0", "DependencyNotReady")
	h.release("decode", "model:v2", true)
	h.expectProgress("the entry Pod alone is not ready capacity", "prefill:0/0 decode:1/0", "DependencyNotReady")
	h.release("decode", "model:v2", false)
	// The budget of decode would allow its last old instance to go. The old
	// instance of prefill still calls the old decode.
	h.expectProgress("ready capacity of decode admits prefill", "prefill:1/0 decode:1/1", "OldVersionDependencyPresent")
	h.expectProgress("the last old decode is kept while an old prefill remains", "prefill:1/0 decode:1/1", "OldVersionDependencyPresent")

	// Once the last old prefill is replaced nothing calls the old decode.
	h.release("prefill", "model:v2", false)
	h.expectProgress("the second prefill follows, and with it the last old decode", "prefill:2/1 decode:2/1", "(ProgressAvailable)")
	h.release("prefill", "model:v2", false)
	h.release("decode", "model:v2", false)
	h.expectProgress("complete", "prefill:2/2 decode:2/2", "(RolloutComplete)")
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}
	for name, uid := range h.uids() {
		if uid == original[name] {
			t.Errorf("Pod %s was not replaced", name)
		}
	}
}

// Roles that are rolled together advance in proportion: a Role may have
// started no more than the ready share of the slowest Role plus maxSkew, of
// its own replica count, rounded up. The steps are those of the end-to-end
// test of the proportional progress.
func TestCoordinatedProportionalProgress(t *testing.T) {
	for _, restarts := range []bool{false, true} {
		t.Run(fmt.Sprintf("a new controller for every round: %t", restarts), func(t *testing.T) {
			testCoordinatedProportionalProgress(t, restarts)
		})
	}
}

func testCoordinatedProportionalProgress(t *testing.T, restarts bool) {
	h := coordinatedHarness(t, coordinated(4, 2, 2, "25%", false))
	h.restarts = restarts
	h.settleContainers()

	h.retarget("rev-b")
	h.expectProgress("rounding up admits one target of each Role", "prefill:1/0 decode:1/0", "MaxSkewLimitReached")
	h.release("prefill", "model:v2", false)
	h.expectProgress("the slowest Role has no ready target: nothing more starts", "prefill:1/1 decode:1/0", "MaxSkewLimitReached")
	h.release("decode", "model:v2", false)
	// Ready progress is 1/4 and 1/2. The slowest plus 25% is half of each Role.
	h.expectProgress("half of each Role may have started", "prefill:2/1 decode:1/1", "MaxSkewLimitReached")
	h.release("prefill", "model:v2", true)
	h.expectProgress("an instance whose worker is not ready is not progress", "prefill:2/1 decode:1/1", "MaxSkewLimitReached")
	h.release("prefill", "model:v2", false)
	h.expectProgress("half of prefill is ready: three quarters may have started", "prefill:3/2 decode:2/1", "MaxSkewLimitReached")

	for range 6 {
		h.release("prefill", "model:v2", false)
		h.release("decode", "model:v2", false)
		h.quiesce()
	}
	if got, blocked := h.progress("model:v2"), h.blocked(); got != "prefill:4/4 decode:2/2" || blocked != "(RolloutComplete)" || h.conditions() != "Ready=AllInstancesReady" {
		t.Errorf("at the end: progress %s, blocked %q, conditions %q", got, blocked, h.conditions())
	}
}

// The partition of a Role holds in a coordinated rollout: the instances below
// it stay, and the Role still counts with its whole replica count.
func TestCoordinatedRolloutStopsAtThePartition(t *testing.T) {
	h := coordinatedHarness(t, coordinated(4, 2, 2, "50%", false), func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(2))
		ms.Spec.Template.Roles[1].Partition = ptr.To(intstr.FromInt32(1))
	})
	h.settleContainers()
	original := h.uids()
	h.retarget("rev-b")
	for range 8 {
		h.release("prefill", "model:v2", false)
		h.release("decode", "model:v2", false)
		h.quiesce()
	}
	if got, blocked := h.progress("model:v2"), h.blocked(); got != "prefill:2/2 decode:1/1" || blocked != "(RolloutComplete)" {
		t.Fatalf("at the partitions: progress %s, blocked %q", got, blocked)
	}
	for _, name := range []string{"llama-0-prefill-0-0", "llama-0-prefill-1-1", "llama-0-decode-0-0"} {
		if h.uids()[name] != original[name] {
			t.Errorf("Pod %s below the partition was replaced", name)
		}
	}
}

// With a surge and no unavailable budget, a coordinated Role creates its
// temporary instance only when it may start the target, and the instance
// goes when the rollout is over.
func TestCoordinatedSurge(t *testing.T) {
	h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true), func(ms *workloadv1alpha1.ModelServing) {
		prefill := &ms.Spec.Template.Roles[0]
		prefill.MaxUnavailable, prefill.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
	})
	h.settleContainers()
	h.retarget("rev-b")
	h.quiesce()
	for _, pod := range h.pods() {
		if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" && pod.Spec.Containers[0].Image == "model:v2" {
			t.Fatalf("prefill started the target before decode was ready: %s", pod.Name)
		}
	}
	if got := h.blocked(); got != "DependencyNotReady" {
		t.Errorf("blocked = %q", got)
	}

	h.release("decode", "model:v2", false)
	h.quiesce()
	surge, err := h.cluster.GetPod(testNamespace, "llama-0-prefill-2-0")
	if err != nil || surge.Annotations[workloadv1alpha1.SurgeAnnotationKey] != workloadv1alpha1.SurgeScopeRole || surge.Spec.Containers[0].Image != "model:v2" {
		t.Fatalf("the surge instance of prefill: %+v, %v", surge, err)
	}
	// With no unavailable budget, two instances of prefill are ready at any
	// time: the surge instance does not go before the place it stands in
	// for is filled and ready.
	available := func() int {
		ready := map[string]int{}
		for _, pod := range h.pods() {
			if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" && pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod) {
				ready[pod.Labels[workloadv1alpha1.RoleIDKey]]++
			}
		}
		count := 0
		for _, pods := range ready {
			if pods == 2 {
				count++
			}
		}
		return count
	}
	for range 10 {
		h.release("prefill", "model:v2", false)
		h.release("decode", "model:v2", false)
		for range 40 {
			h.cluster.Settle()
			writes := h.mustRound()
			if got := available(); got < 2 {
				t.Fatalf("%d instances of prefill are ready, want 2 at any time; %s", got, h.podSummary())
			}
			if len(writes) == 0 {
				break
			}
		}
	}
	if got, blocked := h.progress("model:v2"), h.blocked(); got != "prefill:2/2 decode:2/2" || blocked != "(RolloutComplete)" || h.conditions() != "Ready=AllInstancesReady" {
		t.Errorf("at the end: progress %s, blocked %q, conditions %q", got, blocked, h.conditions())
	}
	for _, pod := range h.pods() {
		if pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] != "" {
			t.Errorf("Pod %s still carries the surge mark", pod.Name)
		}
	}
}

// coordinatedInPlaceHarness returns a harness for two Roles of four instances
// that are updated in place together, a quarter apart, and may each have
// three instances unavailable. An unplanned restart replaces the instance.
func coordinatedInPlaceHarness(t *testing.T, mutate ...func(*workloadv1alpha1.ModelServing)) *harness {
	t.Helper()
	return coordinatedHarness(t, append([]func(*workloadv1alpha1.ModelServing){coordinated(4, 4, 3, "25%", false), func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.RoleRecreate
		ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
		ms.Spec.Template.Roles[1].MaxUnavailable = ptr.To(intstr.FromInt32(3))
	}}, mutate...)...)
}

// patched renders, by Role, the instances with a Pod that runs the target
// image: "+<ordinal>" while the update is in progress, "<ordinal>" once the
// labels of its Pods have changed.
func (h *harness) patched() string {
	h.t.Helper()
	byRole := map[string]sets.Set[string]{"prefill": {}, "decode": {}}
	for _, pod := range h.pods() {
		if pod.Spec.Containers[0].Image != "model:v2" {
			continue
		}
		_, ordinal := utils.GetParentNameAndOrdinal(pod.Labels[workloadv1alpha1.RoleIDKey])
		role, done, updating := pod.Labels[workloadv1alpha1.RoleLabelKey], fmt.Sprint(ordinal), fmt.Sprintf("+%d", ordinal)
		if inplace.InProgress(pod) || pod.Labels[workloadv1alpha1.RevisionLabelKey] != "rev-b" {
			byRole[role].Delete(done).Insert(updating)
		} else if !byRole[role].Has(updating) {
			byRole[role].Insert(done)
		}
	}
	return fmt.Sprintf("prefill:%v decode:%v", sets.List(byRole["prefill"]), sets.List(byRole["decode"]))
}

// An instance whose in-place update has started counts as started, and not as
// ready until the labels of its Pods have changed: the Roles advance by
// maxSkew although maxUnavailable would allow more, and no Pod is replaced.
// The steps are those of the end-to-end test of the coordinated in-place
// update, which starts with the highest instance of each Role not ready.
func TestCoordinatedInPlaceUpdate(t *testing.T) {
	for _, restarts := range []bool{false, true} {
		t.Run(fmt.Sprintf("a new controller for every round: %t", restarts), func(t *testing.T) {
			h := coordinatedInPlaceHarness(t)
			h.restarts = restarts
			h.settleContainers()
			original := h.uids()
			highest := func(pod *corev1.Pod) bool { return strings.HasSuffix(pod.Labels[workloadv1alpha1.RoleIDKey], "-3") }
			for _, pod := range h.pods() {
				if highest(pod) {
					h.setPodStatus(pod.Name, func(status *corev1.PodStatus) { status.Conditions[0].Status = corev1.ConditionFalse })
				}
			}
			h.quiesce()
			patched := h.patched
			// kubelet makes the patched Pods of the highest instances ready.
			kubelet := func(entryOnly bool) {
				for _, pod := range h.pods() {
					if highest(pod) && (!entryOnly || pod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry) {
						h.run(pod.Name)
					}
				}
			}
			expect := func(what, want, wantBlocked string) {
				t.Helper()
				h.quiesce()
				if got := patched(); got != want || h.blocked() != wantBlocked {
					t.Fatalf("%s: patched %s, blocked %q; want %s, %q", what, got, h.blocked(), want, wantBlocked)
				}
			}

			h.retarget("rev-b")
			expect("after the template change", "prefill:[+3] decode:[+3]", "MaxSkewLimitReached")
			expect("another round", "prefill:[+3] decode:[+3]", "MaxSkewLimitReached")
			kubelet(true)
			expect("the entry Pods alone are ready", "prefill:[+3] decode:[+3]", "MaxSkewLimitReached")
			kubelet(false)
			expect("the highest instances are ready", "prefill:[+2 3] decode:[+2 3]", "MaxSkewLimitReached")
			// The Pods patched last still report the Ready condition they
			// had before the patch: they count as started all the same.
			expect("another round", "prefill:[+2 3] decode:[+2 3]", "MaxSkewLimitReached")

			for range 10 {
				h.runAll()
				h.quiesce()
			}
			if got := patched(); got != "prefill:[0 1 2 3] decode:[0 1 2 3]" || h.blocked() != "(RolloutComplete)" || h.conditions() != "Ready=AllInstancesReady" {
				t.Errorf("at the end: patched %s, blocked %q, conditions %q", got, h.blocked(), h.conditions())
			}
			if !reflect.DeepEqual(h.uids(), original) {
				t.Errorf("Pods were replaced")
			}
		})
	}
}

// An instance of which one Pod is patched is a started instance: the round
// patches its other Pod, and starts no further instance of that Role.
func TestCoordinatedInPlaceFinishesAPartlyPatchedInstance(t *testing.T) {
	for _, name := range []string{"llama-0-prefill-3-0", "llama-0-prefill-3-1"} {
		h := coordinatedInPlaceHarness(t)
		h.settleContainers()
		original := h.uids()
		h.retarget("rev-b")
		pod, err := h.cluster.GetPod(testNamespace, name)
		if err != nil {
			t.Fatal(err)
		}
		images := map[string]string{pod.Spec.Containers[0].Name: "model:v2"}
		if started, err := inplace.Start(context.Background(), h.cluster.Kube, pod, images, "rev-b", h.cluster.Now()); err != nil || !started {
			t.Fatalf("patch %s: %t, %v", name, started, err)
		}
		h.quiesce()
		if got := h.patched(); got != "prefill:[+3] decode:[+3]" || !reflect.DeepEqual(h.uids(), original) {
			t.Errorf("%s patched first: patched %s, Pods replaced = %t", name, got, !reflect.DeepEqual(h.uids(), original))
		}
	}
}

// Without roleCoordination the budget of each Role alone limits how many
// instances are updated at a time.
func TestInPlaceUpdateWithoutCoordinationUsesTheRoleBudget(t *testing.T) {
	h := coordinatedInPlaceHarness(t, func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RolloutStrategy.RoleCoordination = nil })
	h.settleContainers()
	h.retarget("rev-b")
	h.quiesce()
	if got := h.patched(); got != "prefill:[+1 +2 +3] decode:[+1 +2 +3]" || h.blocked() != "" {
		t.Errorf("patched %s, blocked %q; want three instances of each Role and no condition", got, h.blocked())
	}
}

// A change that cannot be made in place replaces the instances, and the
// Roles advance by maxSkew all the same.
func TestCoordinatedRecreateWhenInPlaceIsNotPossible(t *testing.T) {
	ms := testModelServing(coordinated(4, 4, 3, "25%", false), func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
		ms.Spec.Template.Roles[1].MaxUnavailable = ptr.To(intstr.FromInt32(3))
	})
	target := withImages(ms, "model:v2")
	for i := range target.Spec.Template.Roles {
		role := &target.Spec.Template.Roles[i]
		role.EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CHANGED", Value: "true"}}
	}
	group := groupOf(ms, "rev-a")
	for i := range group.Spec.Roles {
		group.Spec.Roles[i].BaselineReplicas = ptr.To(group.Spec.Roles[i].Replicas)
	}
	group.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
	h := newHarness(t, ms, group, revisionOf(t, ms, "rev-a"), revisionOf(t, target, "rev-b"))
	h.settleContainers()
	original := h.uids()

	h.retarget("rev-b")
	h.expectProgress("after the template change", "prefill:1/0 decode:1/0", "MaxSkewLimitReached")
	for _, pod := range h.pods() {
		replaced := pod.UID != types.UID(original[pod.Name])
		if highest := strings.HasSuffix(pod.Labels[workloadv1alpha1.RoleIDKey], "-3"); replaced != highest || inplace.StateOf(pod) != nil {
			t.Errorf("Pod %s: replaced = %t, in-place state = %v; want the highest instances replaced and no Pod patched", pod.Name, replaced, inplace.StateOf(pod))
		}
	}
	h.release("prefill", "model:v2", false)
	h.release("decode", "model:v2", false)
	h.expectProgress("the first target instances are ready", "prefill:2/1 decode:2/1", "MaxSkewLimitReached")
}

// Capacity that is added to a Role while it waits for the Roles it depends on
// is target capacity too: it is not created before they are ready. Meanwhile
// the PodGroup keeps asking for the number of instances it asked for before,
// so that the gang does not wait for instances that are not being created.
func TestCoordinatedExpansionWaitsForDependencies(t *testing.T) {
	h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true))
	h.podGroups.enabled = true
	h.settleContainers()
	h.podGroups.published = map[string]int32{"prefill": 2, "decode": 2}

	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.TargetRevision = "rev-b"
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 4, 12
	})
	h.quiesce()
	prefill := 0
	for _, pod := range h.pods() {
		if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" {
			prefill++
			if pod.Spec.Containers[0].Image != "model:v1" {
				t.Errorf("Pod %s of the target was created before decode was ready", pod.Name)
			}
		}
	}
	if prefill != 4 || h.blocked() != "DependencyNotReady" {
		t.Fatalf("%d Pods of prefill, blocked %q; want the four old Pods and the Role waiting", prefill, h.blocked())
	}
	view := h.podGroups.views[len(h.podGroups.views)-1]
	if view.Spec.Template.GangPolicy == nil || !reflect.DeepEqual(view.Spec.Template.GangPolicy.MinRoleReplicas, map[string]int32{"prefill": 2}) {
		t.Errorf("gang policy of the PodGroup while prefill waits = %+v, want the minimum of 2 instances kept for prefill", view.Spec.Template.GangPolicy)
	}

	h.release("decode", "model:v2", false)
	h.quiesce()
	created := 0
	for _, pod := range h.pods() {
		if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" && pod.Spec.Containers[0].Image == "model:v2" {
			created++
		}
	}
	if created == 0 {
		t.Fatalf("no target Pod of prefill after decode became ready: %s", h.podSummary())
	}
	view = h.podGroups.views[len(h.podGroups.views)-1]
	if policy := view.Spec.Template.GangPolicy; policy != nil && len(policy.MinRoleReplicas) != 0 {
		t.Errorf("gang policy of the PodGroup once prefill may start = %+v, want no minimum held back", policy)
	}
}

// The PodGroup keeps a minimum only for a Role that waits for the Roles it
// depends on, and only one that follows the replica count.
func TestCoordinatedExpansionGangScope(t *testing.T) {
	type fixture struct {
		ms        func(*workloadv1alpha1.ModelServing)
		group     func(*workloadv1alpha1.ServingGroup)
		published map[string]int32
		// unchanged: the target revision renders the same Pods.
		unchanged bool
	}
	tests := []struct {
		name string
		fixture
		want map[string]int32
	}{
		{name: "a Role that waits keeps its minimum", want: map[string]int32{"prefill": 2}},
		{name: "Roles rolled independently", fixture: fixture{group: func(group *workloadv1alpha1.ServingGroup) {
			group.Spec.RolloutStrategy.RoleCoordination = nil
		}}},
		{name: "no dependencies", fixture: fixture{group: func(group *workloadv1alpha1.ServingGroup) {
			group.Spec.RolloutStrategy.RoleCoordination.Dependencies = nil
		}}},
		{name: "another scheduler", fixture: fixture{ms: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.SchedulerName = "default-scheduler" }}},
		{name: "a minimum set in gangPolicy is not replaced", fixture: fixture{ms: func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 3}}
		}}, want: map[string]int32{"prefill": 3}},
		{name: "no PodGroup of the group", fixture: fixture{published: map[string]int32{}}},
		{name: "the PodGroup asks for every instance already", fixture: fixture{published: map[string]int32{"prefill": 4, "decode": 2}}},
		{name: "the PodGroup asks for more than the Role has", fixture: fixture{published: map[string]int32{"prefill": 5, "decode": 2}}},
		{name: "a scale-out without a template change", fixture: fixture{unchanged: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := []func(*workloadv1alpha1.ModelServing){coordinated(2, 2, 1, "100%", true)}
			if tt.ms != nil {
				mutate = append(mutate, tt.ms)
			}
			ms := testModelServing(mutate...)
			target := withImages(ms, "model:v2")
			if tt.unchanged {
				target = ms
			}
			group := groupOf(ms, "rev-a")
			for i := range group.Spec.Roles {
				group.Spec.Roles[i].BaselineReplicas = ptr.To(group.Spec.Roles[i].Replicas)
			}
			h := newHarness(t, ms, group, revisionOf(t, ms, "rev-a"), revisionOf(t, target, "rev-b"))
			h.podGroups.enabled = true
			h.settleContainers()
			h.podGroups.published = map[string]int32{"prefill": 2, "decode": 2}
			if tt.published != nil {
				h.podGroups.published = tt.published
			}
			h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.TargetRevision = "rev-b"
				group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 4, 12
				if tt.group != nil {
					tt.group(group)
				}
			})
			h.mustRound()
			var got map[string]int32
			if policy := h.podGroups.views[len(h.podGroups.views)-1].Spec.Template.GangPolicy; policy != nil {
				got = policy.MinRoleReplicas
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("minimums of the PodGroup = %v, want %v", got, tt.want)
			}
		})
	}
}

// While instances of two revisions exist, the PodGroup is built from the
// smallest layout an instance of a Role runs, with the replica counts and the
// gang policy of the target.
func TestCoordinatedPodGroupUsesTheSmallestLayout(t *testing.T) {
	for _, tt := range []struct {
		name               string
		before, after      int32
		mixed, atTheTarget int32
	}{
		{name: "the target has no workers", before: 1, after: 0, mixed: 0, atTheTarget: 0},
		{name: "the target adds a worker", before: 0, after: 1, mixed: 0, atTheTarget: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			layout := func(ms *workloadv1alpha1.ModelServing, workers int32) *workloadv1alpha1.ModelServing {
				out := ms.DeepCopy()
				role := &out.Spec.Template.Roles[0]
				role.WorkerReplicas, role.WorkerTemplate = workers, nil
				if workers > 0 {
					role.WorkerTemplate = role.EntryTemplate.DeepCopy()
				}
				return out
			}
			ms := layout(testModelServing(coordinated(2, 2, 1, "100%", false)), tt.before)
			target := layout(withImages(ms, "model:v2"), tt.after)
			group := groupOf(ms, "rev-a")
			for i := range group.Spec.Roles {
				group.Spec.Roles[i].BaselineReplicas = ptr.To(group.Spec.Roles[i].Replicas)
			}
			h := newHarness(t, ms, group, revisionOf(t, ms, "rev-a"), revisionOf(t, target, "rev-b"))
			h.podGroups.enabled = true
			h.settleContainers()
			synced := func() workloadv1alpha1.Role {
				return h.podGroups.views[len(h.podGroups.views)-1].Spec.Template.Roles[0]
			}

			h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.TargetRevision, group.Spec.ExpectedPods = "rev-b", 2*(1+tt.after)+4
			})
			h.mustRound()
			if got := synced(); got.WorkerReplicas != tt.before || *got.Replicas != 2 {
				t.Errorf("while every instance runs the old layout: %d workers, %d replicas; want %d workers, 2 replicas", got.WorkerReplicas, *got.Replicas, tt.before)
			}
			h.quiesce()
			h.mustRound()
			if got := synced(); got.WorkerReplicas != tt.mixed {
				t.Errorf("with instances of both revisions: %d workers, want %d", got.WorkerReplicas, tt.mixed)
			}
			for range 8 {
				h.runAll()
				h.quiesce()
			}
			h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Annotations = map[string]string{"touch": "1"} })
			h.mustRound()
			if got := synced(); got.WorkerReplicas != tt.atTheTarget || h.blocked() != "(RolloutComplete)" {
				t.Errorf("at the target: %d workers, blocked %q; want %d workers", got.WorkerReplicas, h.blocked(), tt.atTheTarget)
			}
		})
	}
}

// An instance that is about to be created counts with the layout of the
// revision it is created from.
func TestCoordinatedPodGroupCountsTheInstancesToBeCreated(t *testing.T) {
	ms := testModelServing(coordinated(2, 2, 1, "100%", false))
	target := withImages(ms, "model:v2")
	target.Spec.Template.Roles[0].WorkerReplicas, target.Spec.Template.Roles[0].WorkerTemplate = 0, nil
	group := groupOf(ms, "rev-a")
	for i := range group.Spec.Roles {
		group.Spec.Roles[i].BaselineReplicas = ptr.To(group.Spec.Roles[i].Replicas)
	}
	h := newHarness(t, ms, group, revisionOf(t, ms, "rev-a"), revisionOf(t, target, "rev-b"))
	h.podGroups.enabled = true
	h.settleContainers()

	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.TargetRevision = "rev-b"
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 3, 9
	})
	h.mustRound()
	if got := h.podGroups.views[len(h.podGroups.views)-1].Spec.Template.Roles[0]; got.WorkerReplicas != 0 || *got.Replicas != 3 {
		t.Errorf("the PodGroup of the round that adds an instance without workers: %d workers, %d replicas; want 0 and 3", got.WorkerReplicas, *got.Replicas)
	}
}

// A coordinated rollout measures progress against the replica counts of the
// baseline revision. A group that is not told them rolls nothing, and says
// nothing about the rollout: what it said before was said about another spec.
func TestCoordinatedRolloutNeedsTheBaselineReplicas(t *testing.T) {
	h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true))
	h.settleContainers()
	before := h.uids()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.TargetRevision = "rev-b"
		group.Spec.Roles[1].BaselineReplicas = nil
	})
	if _, err := h.round(); err == nil || !strings.Contains(err.Error(), "replica count of Role decode at the baseline revision rev-a is not known") {
		t.Fatalf("round error = %v", err)
	}
	// The Pods keep serving: the group stays ready while its round fails.
	if got := h.conditions(); got != "Ready=TemplateUnresolved ReconcileFailed=Error" || !reflect.DeepEqual(h.uids(), before) {
		t.Errorf("conditions = %q; Pods changed = %t", got, !reflect.DeepEqual(h.uids(), before))
	}
	if got := h.blocked(); got != "" {
		t.Errorf("blocked = %q, want no condition", got)
	}
}

// A round that fails keeps what the last round said about the rollout of the
// same spec.
func TestCoordinatedConditionOutlastsAFailedRound(t *testing.T) {
	h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true))
	h.settleContainers()
	h.retarget("rev-b")
	h.expectProgress("after the template change", "prefill:0/0 decode:1/0", "DependencyNotReady")

	name := utils.GenerateControllerRevisionName("llama", "rev-b")
	if err := h.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.round(); err == nil {
		t.Fatal("the round succeeded without the target revision")
	}
	if got := h.blocked(); got != "DependencyNotReady" || !strings.Contains(h.conditions(), "ReconcileFailed=Error") {
		t.Errorf("blocked = %q, conditions = %q", got, h.conditions())
	}
}

// What a Role may start depends on how ready another Role is. That is read
// from the API server before an instance is replaced or created: a cache that
// still shows the dependency ready admits nothing.
func TestCoordinatedStartIsDecidedOnADirectRead(t *testing.T) {
	for _, tt := range []struct {
		name string
		// replicas is the replica count of prefill at the target.
		replicas int32
	}{
		{name: "an old instance is not replaced", replicas: 2},
		{name: "added capacity is not created", replicas: 4},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := coordinatedHarness(t, coordinated(2, 2, 1, "100%", true))
			h.settleContainers()
			h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.TargetRevision = "rev-b"
				group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = tt.replicas, 2*tt.replicas+4
			})
			h.quiesce()
			h.release("decode", "model:v2", false)
			h.cluster.Settle()
			stale := h.cluster.Snapshot()

			// The worker of the new decode instance stops being ready.
			h.setPodStatus("llama-0-decode-1-1", func(status *corev1.PodStatus) { status.Conditions[0].Status = corev1.ConditionFalse })
			h.cluster.ResetWrites()
			if _, err := h.roundOn(stale); err != nil {
				t.Fatal(err)
			}
			for _, write := range h.cluster.Writes() {
				if write.Resource == "pods" {
					t.Errorf("the round on the stale cache wrote %s", write)
				}
			}
			if got := h.progress("model:v2"); got != "prefill:0/0 decode:1/0" {
				t.Errorf("progress = %s", got)
			}

			// With decode ready again, the same round starts prefill.
			h.run("llama-0-decode-1-1")
			h.quiesce()
			if got := h.progress("model:v2"); !strings.HasPrefix(strings.TrimPrefix(got, "!"), "prefill:") || strings.Contains(got, "prefill:0/") {
				t.Errorf("progress with decode ready = %s, want prefill started", got)
			}
		})
	}
}

// Capacity that is added without a dependency in the way is created at once:
// creation is not limited by maxSkew. The instances it adds are started
// instances of the target all the same, and count against what the old
// instances may start. A Pod whose creation failed is created by a later round
// and does not count twice.
func TestCoordinatedExpansionCountsAgainstTheReplacements(t *testing.T) {
	for _, method := range []workloadv1alpha1.UpdateMethod{workloadv1alpha1.RecreateUpdateMethod, workloadv1alpha1.InPlaceUpdateMethod} {
		for _, partial := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s, a creation fails: %t", method, partial), func(t *testing.T) {
				h := coordinatedHarness(t, coordinated(4, 4, 8, "25%", false), func(ms *workloadv1alpha1.ModelServing) {
					ms.Spec.RolloutStrategy.UpdateMethod = method
					ms.Spec.Template.Roles[1].MaxUnavailable = ptr.To(intstr.FromInt32(8))
				})
				h.settleContainers()
				original := h.uids()
				// touched lists the Pods of the old instances that were
				// replaced or patched.
				touched := func() []string {
					var out []string
					for name, uid := range original {
						pod, err := h.cluster.GetPod(testNamespace, name)
						if err != nil || string(pod.UID) != uid || pod.DeletionTimestamp != nil || pod.Spec.Containers[0].Image != "model:v1" {
							out = append(out, name)
						}
					}
					sort.Strings(out)
					return out
				}

				h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
					group.Spec.TargetRevision = "rev-b"
					group.Spec.Roles[0].Replicas, group.Spec.Roles[1].Replicas, group.Spec.ExpectedPods = 8, 8, 32
				})
				if partial {
					rejected := false
					h.cluster.BeforeWrite = func(write apitest.Write) error {
						if write.Resource == "pods" && write.Verb == "create" && write.Name == "llama-0-prefill-4-1" && !rejected {
							rejected = true
							return errors.New("the API server is busy")
						}
						return nil
					}
					if _, err := h.round(); err == nil {
						t.Fatal("the round succeeded although a creation was rejected")
					}
					if len(touched()) != 0 {
						t.Fatalf("with one Pod of the added capacity missing, old Pods were touched: %v", touched())
					}
				}
				h.quiesce()
				if got := len(h.pods()); got != 32 || len(touched()) != 0 || h.blocked() != "MaxSkewLimitReached" {
					t.Fatalf("after the scale-out: %d Pods, old Pods touched %v, blocked %q; want 32 Pods and the old ones as they were", got, touched(), h.blocked())
				}
				for _, pod := range h.pods() {
					if pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] != "" {
						t.Errorf("Pod %s of the added capacity carries the surge mark", pod.Name)
					}
				}

				// Three of eight are ready: a quarter more, rounded up, is five
				// started. Four are started already, so one old instance of
				// each Role may follow.
				for _, pod := range h.pods() {
					_, ordinal := utils.GetParentNameAndOrdinal(pod.Labels[workloadv1alpha1.RoleIDKey])
					if ordinal >= 4 && ordinal <= 6 {
						h.run(pod.Name)
					}
				}
				h.quiesce()
				want := []string{"llama-0-decode-3-0", "llama-0-decode-3-1", "llama-0-prefill-3-0", "llama-0-prefill-3-1"}
				if got := touched(); !reflect.DeepEqual(got, want) {
					t.Errorf("old Pods touched with three of eight ready: %v, want %v", got, want)
				}
			})
		}
	}
}

// coordinatedStopPoints rolls two Roles together with a controller that dies
// when it is about to make its stopAt-th write (never for stopAt 0). A
// controller that remembers nothing takes over; with staleCache its first
// round reads the cache as it was before the round that died. After every
// round the Roles are no further apart than maxSkew allows. It returns the
// state the group ends in and the number of writes of the controllers.
func coordinatedStopPoints(t *testing.T, method workloadv1alpha1.UpdateMethod, stopAt int, staleCache bool) (state string, writes int) {
	t.Helper()
	replicas := map[string]int{"prefill": 4, "decode": 2}
	h := coordinatedHarness(t, coordinated(4, 2, 2, "25%", false), func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy.UpdateMethod = method
	})
	h.settleContainers()
	h.retarget("rev-b")

	// A Role may have started no more instances than the share of the
	// slowest Role that is ready, plus maxSkew, of its own replica count,
	// rounded up.
	check := func() {
		t.Helper()
		type instance struct{ old, target, ready int }
		byRole := map[string]map[string]*instance{"prefill": {}, "decode": {}}
		for _, pod := range h.pods() {
			instances := byRole[pod.Labels[workloadv1alpha1.RoleLabelKey]]
			id := pod.Labels[workloadv1alpha1.RoleIDKey]
			if instances[id] == nil {
				instances[id] = &instance{}
			}
			switch {
			case pod.DeletionTimestamp != nil:
			case pod.Spec.Containers[0].Image == "model:v1":
				instances[id].old++
			default:
				instances[id].target++
				if utils.IsPodRunningAndReady(pod) && pod.Labels[workloadv1alpha1.RevisionLabelKey] == "rev-b" {
					instances[id].ready++
				}
			}
		}
		started, slowest := map[string]int{}, 1.0
		for role, instances := range byRole {
			old, ready := 0, 0
			for _, inst := range instances {
				if inst.old == 2 {
					old++
				}
				if inst.ready == 2 {
					ready++
				}
			}
			started[role] = replicas[role] - old
			slowest = min(slowest, float64(ready)/float64(replicas[role]))
		}
		for role, count := range started {
			if allowed := int(math.Ceil((slowest + 0.25) * float64(replicas[role]))); count > allowed {
				t.Fatalf("stop at write %d: %s has started %d instances, the slowest Role is at %.2f: at most %d may have; %s", stopAt, role, count, slowest, allowed, h.podFacts())
			}
		}
	}

	inRound, dead := false, false
	h.cluster.BeforeWrite = func(apitest.Write) error {
		if !inRound {
			return nil
		}
		if dead {
			return errCrash
		}
		writes++
		if writes == stopAt {
			dead = true
			return errCrash
		}
		return nil
	}
	for range 200 {
		changed := h.runAll()
		h.cluster.Settle()
		before := h.cluster.Snapshot()
		accepted := len(h.cluster.Writes())
		inRound = true
		_, err := h.round()
		inRound = false
		check()
		if dead {
			dead = false
			h.restart()
			if staleCache {
				inRound = true
				_, _ = h.roundOn(before)
				inRound = false
				check()
			}
			continue
		}
		if err != nil {
			t.Fatalf("stop at write %d: round: %v", stopAt, err)
		}
		if len(h.cluster.Writes()) == accepted && changed == 0 {
			var out []string
			for _, pod := range h.pods() {
				out = append(out, fmt.Sprintf("%s=%s/%s/%t", pod.Name, pod.Spec.Containers[0].Image, pod.Labels[workloadv1alpha1.RevisionLabelKey], utils.IsPodRunningAndReady(pod)))
			}
			return strings.Join(out, " ") + " " + h.blocked() + " " + h.ledger(), writes
		}
	}
	t.Fatalf("stop at write %d: the group did not settle", stopAt)
	return "", 0
}

// A coordinated rollout that is interrupted at any write ends in the state it
// ends in undisturbed, and never lets the Roles drift further apart than
// maxSkew on the way: what a round decides rests on the group, its Pods and
// the revisions alone.
func TestCoordinatedRolloutSurvivesACrashAtAnyWrite(t *testing.T) {
	for _, method := range []workloadv1alpha1.UpdateMethod{workloadv1alpha1.RecreateUpdateMethod, workloadv1alpha1.InPlaceUpdateMethod} {
		t.Run(string(method), func(t *testing.T) {
			want, total := coordinatedStopPoints(t, method, 0, false)
			if !strings.Contains(want, "(RolloutComplete)") || strings.Contains(want, "model:v1") || total < 12 {
				t.Fatalf("undisturbed: %d writes, state %s", total, want)
			}
			for stopAt := 1; stopAt <= total; stopAt++ {
				for _, staleCache := range []bool{false, true} {
					if got, _ := coordinatedStopPoints(t, method, stopAt, staleCache); got != want {
						t.Errorf("stop at write %d, stale cache %t:\n got %s\nwant %s", stopAt, staleCache, got, want)
					}
				}
			}
		})
	}
}
