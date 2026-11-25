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

package mscontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestCreateScaleAndStatus(t *testing.T) {
	s := newSystem(t, modelServing())
	s.settle()

	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after creation:\n got %s\nwant %s", got, want)
	}
	ms := s.modelServing()
	status := ms.Status
	if status.CurrentRevision == "" || status.CurrentRevision != status.UpdateRevision || status.ObservedGeneration != ms.Generation {
		t.Errorf("status = %+v", status)
	}
	if want := "modelserving.volcano.sh/entry=true,modelserving.volcano.sh/name=llama,modelserving.volcano.sh/role-id=serve-0,modelserving.volcano.sh/role=serve"; status.LabelSelector != want {
		t.Errorf("labelSelector = %q, want %q", status.LabelSelector, want)
	}
	if got := ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; got != workloadv1alpha1.ServingGroupAdoptionDone {
		t.Errorf("adoption annotation of a new ModelServing = %q, want Done", got)
	}
	for _, group := range s.groups() {
		owner := metav1.GetControllerOf(group)
		if owner == nil || owner.UID != ms.UID || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
			t.Errorf("group %s is controlled by %+v", group.Name, owner)
		}
		if group.Labels[workloadv1alpha1.ModelServingNameLabelKey] != testName || group.Spec.Revision != status.UpdateRevision ||
			group.Spec.BaselineRevision != status.CurrentRevision || group.Spec.ExpectedPods != 2 || group.Spec.SourceGeneration != ms.Generation {
			t.Errorf("group %s: labels %v, spec %+v", group.Name, group.Labels, group.Spec)
		}
	}

	// Scaling out fills the next ordinals; scaling in removes the highest.
	uids := map[string]types.UID{}
	for _, group := range s.groups() {
		uids[group.Name] = group.UID
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) })
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1 3=model:v1 4=model:v1] replicas=5 updated=5 current=5 available=5 Available"; got != want {
		t.Fatalf("after scaling out:\n got %s\nwant %s", got, want)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1] replicas=2 updated=2 current=2 available=2 Available"; got != want {
		t.Fatalf("after scaling in:\n got %s\nwant %s", got, want)
	}
	for _, group := range s.groups() {
		if group.UID != uids[group.Name] {
			t.Errorf("group %s was replaced by scaling", group.Name)
		}
	}
	if pods := s.pods(); len(pods) != 4 {
		t.Errorf("%d Pods are left, want 4", len(pods))
	}

	// To zero and back.
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](0) })
	s.settle()
	if got, want := s.summary(), "groups[] replicas=0 updated=0 current=0 available=0 Available"; got != want {
		t.Fatalf("scaled to zero:\n got %s\nwant %s", got, want)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) })
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1] replicas=1 updated=1 current=1 available=1 Available"; got != want {
		t.Fatalf("scaled back:\n got %s\nwant %s", got, want)
	}
}

// A rolling update replaces the groups within its budget: never more than
// maxUnavailable groups short of the declared number.
func TestRollingUpdateKeepsTheBudget(t *testing.T) {
	tests := []struct {
		name           string
		maxUnavailable intstr.IntOrString
		maxSurge       intstr.IntOrString
		floor, ceiling int
	}{
		{name: "one unavailable", maxUnavailable: intstr.FromInt32(1), floor: 2, ceiling: 3},
		{name: "two unavailable", maxUnavailable: intstr.FromInt32(2), floor: 1, ceiling: 3},
		{name: "one in three by percentage", maxUnavailable: intstr.FromString("34%"), floor: 2, ceiling: 3},
		{name: "surge only", maxUnavailable: intstr.FromInt32(0), maxSurge: intstr.FromInt32(1), floor: 3, ceiling: 4},
		{name: "surge and unavailable", maxUnavailable: intstr.FromInt32(1), maxSurge: intstr.FromInt32(2), floor: 2, ceiling: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
				config.MaxUnavailable, config.MaxSurge = &tt.maxUnavailable, &tt.maxSurge
			})))
			s.settle()
			before := s.modelServing().Status.UpdateRevision

			lowest, highest := 3, 3
			s.invariant = func() {
				ready, groups := s.readyGroups(), len(s.groups())
				lowest, highest = min(lowest, ready), max(highest, groups)
				if ready < tt.floor || groups > tt.ceiling {
					t.Fatalf("%d groups are ready and %d exist; the budget allows no fewer than %d ready and no more than %d: %s",
						ready, groups, tt.floor, tt.ceiling, s.summary())
				}
			}
			s.update(setImage("model:v2"))
			s.settle()
			s.invariant = nil

			if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
			}
			status := s.modelServing().Status
			if status.UpdateRevision == before || status.CurrentRevision != status.UpdateRevision {
				t.Errorf("revisions after the rollout: current %s, update %s, before %s", status.CurrentRevision, status.UpdateRevision, before)
			}
			// The budget was used, not merely respected.
			if lowest != tt.floor && tt.maxSurge.IntValue() == 0 {
				t.Errorf("the rollout never went below %d ready groups; the budget allows %d", lowest, tt.floor)
			}
			if highest != tt.ceiling {
				t.Errorf("the rollout never had more than %d groups; the budget allows %d", highest, tt.ceiling)
			}
		})
	}
}

func TestPartitionStopsTheRollout(t *testing.T) {
	s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.Partition = ptr.To(intstr.FromInt32(2))
	})))
	s.settle()
	baseline := s.modelServing().Status.CurrentRevision

	s.update(setImage("model:v2"))
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v2] replicas=3 updated=1 current=2 available=3 Available"; got != want {
		t.Fatalf("at the partition:\n got %s\nwant %s", got, want)
	}
	status := s.modelServing().Status
	if status.CurrentRevision != baseline || status.UpdateRevision == baseline {
		t.Errorf("revisions at the partition: current %s, update %s, baseline %s", status.CurrentRevision, status.UpdateRevision, baseline)
	}

	// A protected group that is lost comes back at the baseline revision.
	if err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), "llama-0", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v2] replicas=3 updated=1 current=2 available=3 Available"; got != want {
		t.Fatalf("after a protected group was deleted:\n got %s\nwant %s", got, want)
	}

	// Lowering the partition continues; removing it completes and advances the baseline.
	s.update(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.Partition = ptr.To(intstr.FromInt32(1))
	}))
	s.settle()
	if got, want := s.images(), "0=model:v1 1=model:v2 2=model:v2"; got != want {
		t.Fatalf("partition 1: %s, want %s", got, want)
	}
	s.update(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.Partition = nil }))
	s.settle()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("without a partition:\n got %s\nwant %s", got, want)
	}
	if status := s.modelServing().Status; status.CurrentRevision != status.UpdateRevision {
		t.Errorf("the baseline did not advance: current %s, update %s", status.CurrentRevision, status.UpdateRevision)
	}
}

func TestAFailedGroupIsReplacedUnderTheSameName(t *testing.T) {
	s := newSystem(t, modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
	}))
	s.settle()
	uids := map[string]types.UID{}
	for _, pod := range s.pods() {
		uids[pod.Name] = pod.UID
	}
	group := s.groups()[1]

	if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), "llama-1-serve-0-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()

	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the recovery:\n got %s\nwant %s", got, want)
	}
	if replaced := s.groups()[1]; replaced.Name != group.Name || replaced.UID == group.UID {
		t.Errorf("group %s has UID %s, before %s: want the same name and a new object", replaced.Name, replaced.UID, group.UID)
	}
	for _, pod := range s.pods() {
		inFailedGroup := pod.Labels[workloadv1alpha1.GroupNameLabelKey] == group.Name
		if (pod.UID == uids[pod.Name]) == inFailedGroup {
			t.Errorf("Pod %s: replaced = %t, want %t", pod.Name, pod.UID != uids[pod.Name], inFailedGroup)
		}
	}
}

// An object that holds the name of a group and does not belong to the
// ModelServing is not touched. The other ordinals are filled.
func TestAnOrdinalThatIsOccupiedIsLeftAlone(t *testing.T) {
	squatter := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "llama-1", UID: "squatter-uid"}}
	s := newSystem(t, modelServing(), squatter)
	s.settle()

	if got, want := s.images(), "0=model:v1 1=none 2=model:v1"; got != want {
		t.Fatalf("groups = %s, want %s", got, want)
	}
	if got, err := s.cluster.GetServingGroup(testNamespace, "llama-1"); err != nil || got.UID != "squatter-uid" || len(got.OwnerReferences) != 0 {
		t.Errorf("the object that holds the name: %+v, %v", got, err)
	}
	// It is reported at once: nothing the controller does will free the name.
	if got, want := s.counts()+" "+s.condition(), "replicas=2 updated=2 current=2 available=2 Progressing,RolloutBlocked"; got != want {
		t.Errorf("status = %s, want %s", got, want)
	}
	blocked := meta.FindStatusCondition(s.modelServing().Status.Conditions, string(workloadv1alpha1.ModelServingRolloutBlocked))
	if blocked.Reason != workloadv1alpha1.RolloutBlockedOrdinalOccupied || !strings.Contains(blocked.Message, "llama-1") {
		t.Errorf("RolloutBlocked = %s: %s", blocked.Reason, blocked.Message)
	}

	// The object goes away: the ordinal is filled and the condition clears.
	if err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), "llama-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Errorf("after the name was freed:\n got %s\nwant %s", got, want)
	}
}

// A rollout that waits is reported as blocked once the wait has lasted, with
// what it waits for, and no longer when it moves again.
func TestRolloutBlocked(t *testing.T) {
	blockedOf := func(s *system) (string, string) {
		condition := meta.FindStatusCondition(s.modelServing().Status.Conditions, string(workloadv1alpha1.ModelServingRolloutBlocked))
		if condition == nil {
			return "", ""
		}
		return string(condition.Status), condition.Reason
	}
	round := func(s *system) time.Duration {
		t.Helper()
		caches, snapshot := s.caches()
		s.ms.SetCaches(caches, snapshot.Listers().Services)
		after, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
		if err != nil && !errors.Is(err, ErrNotSettled) {
			t.Fatal(err)
		}
		return after
	}

	t.Run("a group of the target revision does not become ready", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		if status, _ := blockedOf(s); status != "" {
			t.Fatalf("a ModelServing that was never blocked carries the condition: %s", status)
		}
		s.stuck = func(pod *corev1.Pod) bool { return pod.Spec.Containers[0].Image == "model:v2" }
		s.update(setImage("model:v2"))
		s.settle()
		if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v2] replicas=3 updated=1 current=2 available=2 UpdateInProgress"; got != want {
			t.Fatalf("stuck:\n got %s\nwant %s", got, want)
		}

		// Not yet: the round comes back when the wait has lasted.
		s.cluster.Advance(2 * time.Minute)
		if after := round(s); after <= 0 || after > 3*time.Minute {
			t.Errorf("the round is due again after %v, want when the deadline passes", after)
		}
		if status, _ := blockedOf(s); status != "" {
			t.Errorf("RolloutBlocked = %s before the deadline", status)
		}
		s.cluster.Advance(4 * time.Minute)
		round(s)
		if status, reason := blockedOf(s); status != "True" || reason != workloadv1alpha1.RolloutBlockedTargetNotReady {
			t.Errorf("RolloutBlocked = %s/%s after the deadline", status, reason)
		}
		if got := s.condition(); got != "RolloutBlocked,UpdateInProgress" {
			t.Errorf("conditions = %s", got)
		}

		s.stuck = nil
		s.settle()
		if status, reason := blockedOf(s); status != "False" || reason != "NotBlocked" || s.condition() != "Available" {
			t.Errorf("RolloutBlocked = %s/%s, conditions %s; want it cleared once the rollout is complete", status, reason, s.condition())
		}
	})

	t.Run("a group that is deleted does not go away", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		s.cluster.HoldPodTermination(true)
		s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })
		s.settle()
		if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1!"; got != want {
			t.Fatalf("groups = %s, want %s", got, want)
		}
		s.cluster.Advance(6 * time.Minute)
		round(s)
		if status, reason := blockedOf(s); status != "True" || reason != workloadv1alpha1.RolloutBlockedWaitingForDeletion {
			t.Errorf("RolloutBlocked = %s/%s", status, reason)
		}
	})

	t.Run("a group does not report", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		// The ServingGroup controller is away: a new spec is handed down and
		// nobody reports on it.
		s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy })
		round(s)
		s.cluster.Advance(6 * time.Minute)
		round(s)
		if status, reason := blockedOf(s); status != "" {
			// The ModelServing is available: it has nothing to do and is not blocked.
			t.Errorf("RolloutBlocked = %s/%s for a ModelServing that is available", status, reason)
		}
		// With work to do it is.
		s.update(setImage("model:v2"))
		round(s)
		s.cluster.Advance(6 * time.Minute)
		round(s)
		if status, reason := blockedOf(s); status != "True" || reason != workloadv1alpha1.RolloutBlockedGroupNotObserved {
			t.Errorf("RolloutBlocked = %s/%s", status, reason)
		}
	})
}
