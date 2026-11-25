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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
)

// standard returns a harness with a ModelServing, the ControllerRevision
// rev-a of it and its group llama-0 at rev-a.
func standard(t *testing.T, mutate ...func(*workloadv1alpha1.ModelServing)) *harness {
	t.Helper()
	ms := testModelServing(mutate...)
	return newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
}

func recoveryPolicy(policy workloadv1alpha1.RecoveryPolicy) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RecoveryPolicy = policy }
}

func TestCreateWritesTheStatusBeforeTheFirstPod(t *testing.T) {
	h := standard(t)
	h.podGroups.enabled = true

	writes := h.mustRound()
	want := []string{
		"update servinggroups/status llama-0",
		"create pods llama-0-prefill-0-0",
		"create pods llama-0-prefill-0-1",
		"create pods llama-0-prefill-0-2",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	// The PodGroup comes before the write that commits the creation.
	if len(h.podGroups.syncedAt) != 1 || h.podGroups.syncedAt[0] != 0 || h.podGroups.synced[0] != "llama-0 by sg-uid for scheduler volcano" {
		t.Errorf("PodGroup synced %v after %v writes, want once, before the first write", h.podGroups.synced, h.podGroups.syncedAt)
	}

	status := h.group().Status
	if status.Pods != 3 || status.ObservedGeneration != 1 || h.isReady() {
		t.Errorf("status = pods %d, observedGeneration %d, ready %t; want 3, 1, false", status.Pods, status.ObservedGeneration, h.isReady())
	}
	for _, pod := range h.pods() {
		owner := metav1.GetControllerOf(pod)
		if owner == nil || owner.UID != "sg-uid" || owner.Kind != "ServingGroup" || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
			t.Errorf("Pod %s is controlled by %+v, want the group, blocking its deletion", pod.Name, owner)
		}
		if pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey] != "0" || pod.Labels[workloadv1alpha1.RevisionLabelKey] != "rev-a" ||
			pod.Labels[workloadv1alpha1.ModelServingNameLabelKey] != "llama" || pod.Labels[workloadv1alpha1.GroupNameLabelKey] != testGroup {
			t.Errorf("Pod %s has labels %v", pod.Name, pod.Labels)
		}
		if pod.Annotations["test/pod-group"] != "llama-0/prefill-0" {
			t.Errorf("Pod %s is not bound to the PodGroup: %v", pod.Name, pod.Annotations)
		}
		if pod.Spec.Containers[0].Image != "model:v1" || pod.Spec.SchedulerName != "volcano" {
			t.Errorf("Pod %s: image %s, scheduler %s", pod.Name, pod.Spec.Containers[0].Image, pod.Spec.SchedulerName)
		}
	}

	// Pods that are not ready yet: the instance is complete and established, the group not ready.
	h.mustRound()
	if got := h.ledger(); got != "prefill-0:Established/0" || h.isReady() {
		t.Errorf("ledger = %q, ready = %t; want established and not ready", got, h.isReady())
	}
	h.settle()
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}
	status = h.group().Status
	if status.ReadyPods != 3 || status.Revision != "rev-a" || !reflect.DeepEqual(status.ReferencedRevisions, []string{"rev-a"}) ||
		status.LabelSelector != "modelserving.volcano.sh/group-name=llama-0" {
		t.Errorf("status = %+v", status)
	}
}

func TestNoPodIsCreatedWhenTheStatusWriteIsRejected(t *testing.T) {
	h := standard(t)
	h.cluster.BeforeWrite = func(write apitest.Write) error {
		if write.Subresource == "status" {
			return apierrors.NewConflict(workloadv1alpha1.Resource("servinggroups"), write.Name, errors.New("changed"))
		}
		return nil
	}
	if _, err := h.round(); !apierrors.IsConflict(err) {
		t.Fatalf("round error = %v, want the conflict", err)
	}
	if got := h.podSummary(); got != "" {
		t.Errorf("Pods were created although the status write was rejected: %s", got)
	}
}

func TestNoPodIsCreatedWithoutThePodGroup(t *testing.T) {
	h := standard(t)
	h.podGroups.enabled, h.podGroups.err = true, errors.New("the name is taken")

	retry, err := h.round()
	if err != nil || retry <= 0 {
		t.Fatalf("round = %v, %v; want a retry later", retry, err)
	}
	if got := h.podSummary(); got != "" {
		t.Errorf("Pods were created without a PodGroup: %s", got)
	}
	status := h.group().Status
	if status.Pods != 0 || h.conditions() != "ReconcileFailed=Error" {
		t.Errorf("status: pods %d, conditions %q; want 0 and ReconcileFailed", status.Pods, h.conditions())
	}

	// The same failure again writes nothing: no loop of status writes.
	if writes := h.mustRound(); len(writes) != 0 {
		t.Errorf("a repeated failure wrote %v", writes)
	}
	h.podGroups.err = nil
	h.settle()
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions after the PodGroup could be created = %q", got)
	}
}

func TestRoleRecreateDrainsTheInstanceAfterALostMember(t *testing.T) {
	h := standard(t)
	h.settle()
	before := h.pods()
	h.cluster.HoldPodTermination(true)
	h.lose("llama-0-prefill-0-1")
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-1")
	h.cluster.ResetWrites()

	// The decision is written before a healthy member is deleted.
	writes := h.mustRound()
	want := []string{
		"update servinggroups/status llama-0",
		"delete pods llama-0-prefill-0-0",
		"delete pods llama-0-prefill-0-2",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.ledger(); got != "prefill-0:Draining/0" || h.isReady() {
		t.Errorf("ledger = %q, ready = %t", got, h.isReady())
	}
	// While the members terminate nothing is created: the status follows the Pods, no more.
	if writes := h.mustRound(); !reflect.DeepEqual(writes, []string{"update servinggroups/status llama-0"}) {
		t.Errorf("writes while the instance drains: %v", writes)
	}
	if writes := h.mustRound(); len(writes) != 0 {
		t.Errorf("writes of a round in which nothing changed: %v", writes)
	}
	if status := h.group().Status; status.TerminatingPods != 2 || status.ReadyPods != 0 || status.Pods != 2 {
		t.Errorf("status while the instance drains: %+v", status)
	}
	if got := h.podSummary(); got != "prefill-0-0@0! prefill-0-2@0!" {
		t.Errorf("Pods = %q", got)
	}

	h.cluster.HoldPodTermination(false)
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-0")
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-2")
	h.cluster.ResetWrites()

	// Drained: the next generation is committed, then created.
	writes = h.mustRound()
	want = []string{
		"update servinggroups/status llama-0",
		"create pods llama-0-prefill-0-0",
		"create pods llama-0-prefill-0-1",
		"create pods llama-0-prefill-0-2",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.podSummary(); got != "prefill-0-0@1 prefill-0-1@1 prefill-0-2@1" {
		t.Errorf("Pods = %q", got)
	}
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/1 Ready=AllInstancesReady" {
		t.Errorf("after the recovery: %q", got)
	}
	for _, pod := range h.pods() {
		if pod.UID == uidOf(before, strings.TrimPrefix(pod.Name, testGroup+"-")) {
			t.Errorf("Pod %s survived the recovery of its Role instance", pod.Name)
		}
	}
}

func TestNoneRecreatesOnlyTheLostMember(t *testing.T) {
	h := standard(t, recoveryPolicy(workloadv1alpha1.NoneRestartPolicy))
	h.settle()
	before := h.pods()
	h.lose("llama-0-prefill-0-1")

	writes := h.mustRound()
	want := []string{"update servinggroups/status llama-0", "create pods llama-0-prefill-0-1"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after the recovery: %q", got)
	}
	after := h.pods()
	if uidOf(after, "prefill-0-0") != uidOf(before, "prefill-0-0") || uidOf(after, "prefill-0-2") != uidOf(before, "prefill-0-2") {
		t.Error("a member that was not lost was replaced")
	}
}

func TestServingGroupRecreateFailsTheGroup(t *testing.T) {
	h := standard(t, recoveryPolicy(workloadv1alpha1.ServingGroupRecreate))
	h.settle()
	h.lose("llama-0-prefill-0-1")

	// The group fails. Its healthy members are not deleted by this
	// controller: the ModelServing controller deletes the group as a whole.
	writes := h.mustRound()
	if want := []string{"update servinggroups/status llama-0"}; !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.conditions(); got != "Drained=NoFaultyMember Failed=MemberLost" {
		t.Errorf("conditions = %q", got)
	}
	if writes := h.mustRound(); len(writes) != 0 {
		t.Errorf("a failed group was written again: %v", writes)
	}
	if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-2@0" {
		t.Errorf("Pods = %q, want the two that were not lost", got)
	}

	// A member that fails afterwards is deleted first, and the group is not
	// drained until it is gone.
	h.cluster.HoldPodTermination(true)
	h.crash("llama-0-prefill-0-2")
	h.cluster.ResetWrites()
	writes = h.mustRound()
	want := []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-0-2"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.conditions(); got != "Failed=MemberLost" {
		t.Errorf("conditions with a faulty member = %q", got)
	}
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-2")
	h.mustRound()
	if got := h.conditions(); got != "Drained=NoFaultyMember Failed=MemberLost" {
		t.Errorf("conditions after the faulty member is gone = %q", got)
	}
	if got := h.podSummary(); got != "prefill-0-0@0" {
		t.Errorf("Pods = %q", got)
	}
}

func TestAllPodsLostFailsAGroupThatIsReplacedAsAWhole(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate} {
		h := standard(t, recoveryPolicy(policy))
		h.settle()
		for _, pod := range h.pods() {
			h.lose(pod.Name)
		}
		writes := h.mustRound()
		if want := []string{"update servinggroups/status llama-0"}; !reflect.DeepEqual(writes, want) {
			t.Fatalf("%s: writes = %v, want %v", policy, writes, want)
		}
		if got := h.conditions(); got != "Drained=NoFaultyMember Failed=AllPodsLost" {
			t.Errorf("%s: conditions = %q", policy, got)
		}
	}
}

// The cache of the controller can be behind the API server. A Pod that the
// cache does not show is not a Pod that is lost.
func TestAStaleCacheDoesNotStartARecovery(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		h := standard(t, recoveryPolicy(policy))
		h.settle()
		h.cluster.ResetWrites()

		stale := h.cluster.Snapshot()
		pod, _, _ := stale.Pods.GetByKey(testNamespace + "/llama-0-prefill-0-1")
		if err := stale.Pods.Delete(pod); err != nil {
			t.Fatal(err)
		}
		if _, err := h.roundOn(stale); err != nil {
			t.Fatal(err)
		}
		if writes := h.cluster.Writes(); len(writes) != 0 {
			t.Errorf("%s: a Pod missing from the cache alone caused %v", policy, writesOf(writes))
		}
		if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0" {
			t.Errorf("%s: Pods = %q", policy, got)
		}
	}
}

// And the other way round: the cache still shows a group that was deleted and
// created again under the same name. Nothing is written to the new object on
// the evidence of the old one.
func TestAGroupThatWasReplacedIsLeftAlone(t *testing.T) {
	h := standard(t)
	h.settle()
	stale := h.cluster.Snapshot()
	pod, _, _ := stale.Pods.GetByKey(testNamespace + "/llama-0-prefill-0-1")
	_ = stale.Pods.Delete(pod)

	groups := h.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace)
	replacement := h.group()
	if err := groups.Delete(context.Background(), testGroup, metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationOrphan)}); err != nil {
		t.Fatal(err)
	}
	h.cluster.Settle()
	replacement.UID, replacement.ResourceVersion, replacement.Status = "", "", workloadv1alpha1.ServingGroupStatus{}
	if _, err := groups.Create(context.Background(), replacement, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.cluster.ResetWrites()

	if _, err := h.roundOn(stale); err != nil {
		t.Fatal(err)
	}
	if writes := h.cluster.Writes(); len(writes) != 0 {
		t.Errorf("a round on a group that was replaced wrote %v", writesOf(writes))
	}
}

func TestAFaultCountsAfterTheGracePeriod(t *testing.T) {
	h := standard(t, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
	})
	h.settle()
	h.cluster.HoldPodTermination(true)
	h.crash("llama-0-prefill-0-1")
	h.cluster.ResetWrites()

	// Tolerated: the group is not ready, nothing is deleted, the round comes back in time.
	retry, err := h.round()
	if err != nil || retry != 60*time.Second {
		t.Fatalf("round = %v, %v; want a retry after the grace period", retry, err)
	}
	if got := h.ledger(); got != "prefill-0:Established/0" || h.isReady() || h.group().Status.FaultyMembers != 0 {
		t.Errorf("ledger = %q, ready = %t, faultyMembers = %d", got, h.isReady(), h.group().Status.FaultyMembers)
	}
	h.cluster.Advance(59 * time.Second)
	if retry, _ := h.round(); retry != time.Second {
		t.Errorf("retry after 59 seconds = %v, want one second", retry)
	}

	// A controller that starts now has not seen the fault before: a full grace period again.
	h.restart()
	if retry, _ := h.round(); retry != 60*time.Second {
		t.Errorf("retry of a new process = %v, want a full grace period", retry)
	}
	h.cluster.Advance(60 * time.Second)
	h.cluster.ResetWrites()

	// The fault counts: the faulty member goes first, the others after it.
	writes := h.mustRound()
	want := []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-0-1"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.ledger(); got != "prefill-0:Draining/0" || h.group().Status.FaultyMembers != 1 {
		t.Errorf("ledger = %q, faultyMembers = %d", got, h.group().Status.FaultyMembers)
	}
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-1")
	h.cluster.ResetWrites()
	writes = h.mustRound()
	want = []string{"delete pods llama-0-prefill-0-0", "delete pods llama-0-prefill-0-2"}
	if !reflect.DeepEqual(writes[len(writes)-2:], want) {
		t.Fatalf("writes = %v, want the healthy members deleted after the faulty one is gone", writes)
	}
}

func TestAFaultIsToleratedForeverWhenRecoveryIsOff(t *testing.T) {
	for name, mutate := range map[string]func(*workloadv1alpha1.ModelServing){
		"None": recoveryPolicy(workloadv1alpha1.NoneRestartPolicy),
		"grace -1": func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
		},
	} {
		h := standard(t, mutate)
		h.settle()
		h.crash("llama-0-prefill-0-1")
		h.cluster.ResetWrites()
		h.cluster.Advance(time.Hour)

		retry, err := h.round()
		if err != nil || retry != 0 {
			t.Errorf("%s: round = %v, %v", name, retry, err)
		}
		if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0" || h.isReady() {
			t.Errorf("%s: Pods = %q, ready = %t; want all three kept and the group not ready", name, got, h.isReady())
		}
	}
}

// The ModelServing is the first to know that recovery was turned off. The
// group, whose spec still says otherwise, does not delete on its old spec.
func TestTheModelServingCanOnlyMakeRecoveryMoreReluctant(t *testing.T) {
	h := standard(t)
	h.settle()
	ms, _ := h.cluster.GetModelServing(testNamespace, "llama")
	ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	if _, err := h.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Update(context.Background(), ms, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.crash("llama-0-prefill-0-1")
	h.lose("llama-0-prefill-0-2")

	h.settle()
	if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0" {
		t.Errorf("Pods = %q, want the lost member created again and nothing deleted", got)
	}
	if got := h.ledger(); got != "prefill-0:Established/0" {
		t.Errorf("ledger = %q", got)
	}
}

func TestRoleReplicasAreScaled(t *testing.T) {
	h := standard(t)
	h.settle()
	before := h.pods()

	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 3, 9
	})
	h.settle()
	if got := h.ledger(); got != "prefill-0:Established/0 prefill-1:Established/0 prefill-2:Established/0" {
		t.Errorf("ledger after scaling out = %q", got)
	}
	status := h.group().Status
	if status.ObservedGeneration != 2 || status.Pods != 9 || !h.isReady() {
		t.Errorf("status after scaling out: observedGeneration %d, pods %d, ready %t", status.ObservedGeneration, status.Pods, h.isReady())
	}

	// Scaling in removes the instance with the highest index, decision first.
	h.cluster.HoldPodTermination(true)
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 2, 6
	})
	h.cluster.ResetWrites()
	writes := h.mustRound()
	want := []string{
		"update servinggroups/status llama-0",
		"delete pods llama-0-prefill-2-0", "delete pods llama-0-prefill-2-1", "delete pods llama-0-prefill-2-2",
	}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.ledger(); got != "prefill-0:Established/0 prefill-1:Established/0 prefill-2:Draining/0" || h.isReady() {
		t.Errorf("ledger = %q, ready = %t", got, h.isReady())
	}
	h.cluster.HoldPodTermination(false)
	for _, name := range []string{"llama-0-prefill-2-0", "llama-0-prefill-2-1", "llama-0-prefill-2-2"} {
		h.cluster.FinishTermination(testNamespace, name)
	}
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/0 prefill-1:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after scaling in: %q", got)
	}
	if after := h.pods(); len(after) != 6 || uidOf(after, "prefill-0-0") != uidOf(before, "prefill-0-0") {
		t.Errorf("after scaling in there are %d Pods; the first instance must be untouched", len(after))
	}

	// To zero: the group has no member and is ready.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 0, 0
	})
	h.settle()
	if got := h.podSummary() + "|" + h.ledger() + "|" + h.conditions(); got != "||Ready=AllInstancesReady" {
		t.Errorf("scaled to zero: %q", got)
	}
	if pods := h.group().Status.Pods; pods != 0 {
		t.Errorf("pods of a group without a member = %d", pods)
	}
}

func TestLegacyPodsAreAdoptedWithoutARestart(t *testing.T) {
	ms := testModelServing()
	group := groupOf(ms, "rev-a")
	legacy := func(ordinal string) *corev1.Pod {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNamespace, Name: "llama-0-prefill-0-" + ordinal,
				Labels: map[string]string{
					workloadv1alpha1.ModelServingNameLabelKey: "llama", workloadv1alpha1.GroupNameLabelKey: testGroup,
					workloadv1alpha1.RoleLabelKey: "prefill", workloadv1alpha1.RoleIDKey: "prefill-0",
					workloadv1alpha1.RevisionLabelKey: "rev-a",
				},
				OwnerReferences: []metav1.OwnerReference{
					{APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: "ModelServing", Name: "llama", UID: "ms-uid", Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true)},
					{APIVersion: "v1", Kind: "ConfigMap", Name: "watcher", UID: "watcher-uid"},
				},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "model:v1"}}},
		}
		return pod
	}
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), group, legacy("0"), legacy("1"), legacy("2"))
	h.readyAll()
	before := h.pods()
	h.cluster.ResetWrites()

	writes := h.mustRound()
	if len(writes) < 3 || !reflect.DeepEqual(writes[:3], []string{"patch pods llama-0-prefill-0-0", "patch pods llama-0-prefill-0-1", "patch pods llama-0-prefill-0-2"}) {
		t.Fatalf("writes = %v, want the three Pods adopted first", writes)
	}
	for _, write := range writes {
		if strings.HasPrefix(write, "create ") || strings.HasPrefix(write, "delete ") {
			t.Errorf("adoption %s", write)
		}
	}
	h.settle()
	after := h.pods()
	for _, pod := range after {
		owners := map[string]bool{}
		for _, reference := range pod.OwnerReferences {
			owners[string(reference.UID)] = reference.Controller != nil && *reference.Controller
		}
		if !reflect.DeepEqual(owners, map[string]bool{"sg-uid": true, "watcher-uid": false}) {
			t.Errorf("Pod %s: owners %v, want the group as the controller and the other owner kept", pod.Name, owners)
		}
		if pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey] != "0" {
			t.Errorf("Pod %s has no instance generation after adoption", pod.Name)
		}
		if pod.UID != uidOf(before, strings.TrimPrefix(pod.Name, testGroup+"-")) {
			t.Errorf("Pod %s was replaced by the adoption", pod.Name)
		}
	}
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after the adoption: %q", got)
	}
}

func TestAGroupThatIsGoingAwayOrHasNoOwnerIsLeftAlone(t *testing.T) {
	ms := testModelServing()
	deleting := groupOf(ms, "rev-a")
	deleting.DeletionTimestamp, deleting.Finalizers = ptr.To(metav1.Now()), []string{metav1.FinalizerDeleteDependents}
	orphaned := groupOf(ms, "rev-a")
	orphaned.OwnerReferences[0].UID = "previous-ms-uid"

	for name, group := range map[string]*workloadv1alpha1.ServingGroup{"terminating": deleting, "controlled by a ModelServing that is gone": orphaned} {
		h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), group)
		if writes := h.mustRound(); len(writes) != 0 {
			t.Errorf("%s: writes = %v", name, writes)
		}
	}
}

func TestANameThatIsTakenIsReported(t *testing.T) {
	ms := testModelServing()
	squatter := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "llama-0-prefill-0-1"}}
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), squatter)

	if _, err := h.round(); !errors.Is(err, errNameOccupied) {
		t.Fatalf("round error = %v, want the occupied name", err)
	}
	if got := h.conditions(); got != "ReconcileFailed=NameOccupied" {
		t.Errorf("conditions = %q", got)
	}
	// The squatter is not touched, and the member before it exists.
	pod, err := h.cluster.GetPod(testNamespace, "llama-0-prefill-0-1")
	if err != nil || len(pod.OwnerReferences) != 0 {
		t.Errorf("the Pod that holds the name: %v, %v", pod, err)
	}
	if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@" {
		t.Errorf("Pods = %q", got)
	}
}

func TestATemplateThatCannotBeResolvedStopsCreationOnly(t *testing.T) {
	ms := testModelServing()
	h := newHarness(t, ms, groupOf(ms, "rev-a"))
	if writes := h.mustRound(); !reflect.DeepEqual(writes, []string{"update servinggroups/status llama-0"}) {
		t.Fatalf("writes = %v", writes)
	}
	ready := meta.FindStatusCondition(h.group().Status.Conditions, "Ready")
	if ready == nil || ready.Status != metav1.ConditionFalse || h.podSummary() != "" {
		t.Errorf("Ready = %+v, Pods = %q", ready, h.podSummary())
	}
	// The ControllerRevision appears: the group is created.
	if _, err := h.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Create(context.Background(), revisionOf(t, ms, "rev-a"), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}
}

// A CustomResourceDefinition older than the controller drops status fields it
// does not know, without an error. What follows a status write relies on it.
func TestAStatusThatIsNotKeptStopsTheRound(t *testing.T) {
	h := standard(t)
	h.cluster.Kthena.PrependReactor("update", "servinggroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateAction)
		if update.GetSubresource() != "status" {
			return false, nil, nil
		}
		pruned := update.GetObject().(*workloadv1alpha1.ServingGroup).DeepCopy()
		pruned.Status.Pods = 0
		return true, pruned, nil
	})
	if _, err := h.round(); err == nil || !strings.Contains(err.Error(), "not kept") {
		t.Fatalf("round error = %v, want the status that was not kept", err)
	}
	if got := h.podSummary(); got != "" {
		t.Errorf("Pods were created on a status that was not kept: %s", got)
	}
}
