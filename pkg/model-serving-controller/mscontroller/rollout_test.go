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

// surgeVariants are the two kinds of surge group the rule of its reclaim must
// treat alike: the floor of ready groups holds the one, nothing held the other.
var surgeVariants = []struct {
	name  string
	stuck bool
}{
	{name: "a surge group that is ready"},
	{name: "a surge group that never gets ready", stuck: true},
}

// surgeRollout starts a rollout of three groups with one surge group and one
// group that may be unavailable.
func surgeRollout(t *testing.T, stuck bool) *system {
	t.Helper()
	s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxSurge = ptr.To(intstr.FromInt32(1))
	})))
	s.settle()
	if stuck {
		s.stuck = func(pod *corev1.Pod) bool { return pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-3" }
	}
	s.cluster.ResetWrites()
	// The ModelServing controller runs twice in a row, so it also sees an
	// object it created before the ServingGroup controller has seen it.
	s.msRounds = 2
	s.update(setImage("model:v2"))
	return s
}

// leftInOrder checks that the place of the last old group was created, then
// its first Pod, and that the surge group was deleted after that, once.
func leftInOrder(t *testing.T, s *system) {
	t.Helper()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	created, pods, reclaimed, deletions := -1, -1, -1, 0
	writes := s.cluster.Writes()
	for i, write := range writes {
		switch {
		case write.Verb == "create" && write.Resource == "servinggroups" && write.Name == "llama-0":
			created, pods = i, -1
		case write.Verb == "create" && write.Resource == "pods" && strings.HasPrefix(write.Name, "llama-0-") && pods < 0:
			pods = i
		case write.Verb == "delete" && write.Resource == "servinggroups" && write.Name == "llama-3":
			reclaimed = i
			deletions++
		}
	}
	if created < 0 || pods < created || reclaimed < pods || deletions != 1 {
		t.Errorf("llama-0 created at write %d, its first Pod at %d, llama-3 deleted %d time(s), last at %d; want that order and one deletion:\n%v",
			created, pods, deletions, reclaimed, writes)
	}
}

// rest runs passes until one writes nothing, with the termination of Pods
// held: a terminating Pod goes at once unless keep says that it takes its time.
func rest(s *system, keep func(*corev1.Pod) bool) {
	s.t.Helper()
	s.cluster.HoldPodTermination(true)
	for range 100 {
		// The garbage collector first: it is what gives the Pods of a deleted group their deletionTimestamp.
		s.cluster.Settle()
		for _, pod := range s.pods() {
			if pod.DeletionTimestamp != nil && !keep(pod) {
				if err := s.cluster.FinishTermination(testNamespace, pod.Name); err != nil {
					s.t.Fatal(err)
				}
			}
		}
		s.kubelet()
		s.cluster.Settle()
		if s.pass() == 0 {
			return
		}
	}
	s.t.Fatalf("the ModelServing did not come to rest: %s", s.summary())
}

func lastOldGroup(pod *corev1.Pod) bool {
	return pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-0" && pod.Spec.Containers[0].Image == "model:v1"
}

func noPod(*corev1.Pod) bool { return false }

// The surge group stands in for a regular place until three other groups
// have Pods. While the last old group terminates it stays, ready or not; it
// leaves after the replacement has been created and has its first Pod.
func TestTheSurgeGroupStaysWhileTheLastOldGroupTerminates(t *testing.T) {
	for _, tt := range surgeVariants {
		t.Run(tt.name, func(t *testing.T) {
			s := surgeRollout(t, tt.stuck)
			rest(s, lastOldGroup)
			if got, want := s.images(), "0=model:v1! 1=model:v2 2=model:v2 3=model:v2+"; got != want {
				t.Fatalf("at rest while the last old group terminates:\n got %s\nwant %s", got, want)
			}

			rest(s, noPod)
			leftInOrder(t, s)
		})
	}
}

// With maxUnavailable at replicas the only old group is deleted in the round
// that creates the surge group. The surge group is new then and no Pod of it
// exists; the next round of the ModelServing controller sees it that way. It
// stays, gets its Pods, and leaves after the replacement of the old group has
// its first Pod.
func TestTheSurgeGroupIsNotReclaimedBeforeItHasAPod(t *testing.T) {
	s := newSystem(t, modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1) // maxUnavailable is 1: all of them
	}, rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxSurge = ptr.To(intstr.FromInt32(1))
	})))
	s.settle()
	s.cluster.ResetWrites()
	// The ModelServing controller runs twice in a row: its second round sees
	// the surge group before the ServingGroup controller has seen it.
	s.msRounds = 2
	s.update(setImage("model:v2"))

	rest(s, lastOldGroup)
	if got, want := s.images(), "0=model:v1! 1=model:v2+"; got != want {
		t.Fatalf("at rest while the old group terminates:\n got %s\nwant %s", got, want)
	}

	rest(s, noPod)
	if got, want := s.summary(), "groups[0=model:v2] replicas=1 updated=1 current=1 available=1 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	surgePod, created, pods, reclaimed, deletions := -1, -1, -1, -1, 0
	writes := s.cluster.Writes()
	for i, write := range writes {
		switch {
		case write.Verb == "create" && write.Resource == "pods" && strings.HasPrefix(write.Name, "llama-1-") && surgePod < 0:
			surgePod = i
		case write.Verb == "create" && write.Resource == "servinggroups" && write.Name == "llama-0":
			created, pods = i, -1
		case write.Verb == "create" && write.Resource == "pods" && strings.HasPrefix(write.Name, "llama-0-") && pods < 0:
			pods = i
		case write.Verb == "delete" && write.Resource == "servinggroups" && write.Name == "llama-1":
			reclaimed = i
			deletions++
		}
	}
	if surgePod < 0 || created < surgePod || pods < created || reclaimed < pods || deletions != 1 {
		t.Errorf("first Pod of llama-1 at write %d, llama-0 created at %d, its first Pod at %d, llama-1 deleted %d time(s), last at %d; want that order and one deletion:\n%v",
			surgePod, created, pods, deletions, reclaimed, writes)
	}
}

// Pods that are terminating are on their way out: a group that has no other
// Pod is not one of the three. Here the Pods of a group of the target are
// deleted from outside while the last old group terminates.
func TestTheSurgeGroupStaysWhileThePodsOfAnotherGroupAreAllTerminating(t *testing.T) {
	for _, tt := range surgeVariants {
		t.Run(tt.name, func(t *testing.T) {
			s := surgeRollout(t, tt.stuck)
			rest(s, lastOldGroup)
			lost := map[types.UID]bool{}
			for _, pod := range s.pods() {
				if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-1" {
					lost[pod.UID] = true
					if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
						t.Fatal(err)
					}
				}
			}
			s.cluster.ResetWrites()

			// The last old group goes and is replaced; the lost Pods are still terminating.
			rest(s, func(pod *corev1.Pod) bool { return lost[pod.UID] })
			if got, want := s.images(), "0=model:v2 1=model:v2 2=model:v2 3=model:v2+"; got != want {
				t.Fatalf("at rest while the Pods of llama-1 terminate:\n got %s\nwant %s", got, want)
			}
			for _, pod := range s.pods() {
				if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-1" && pod.DeletionTimestamp == nil {
					t.Fatalf("Pod %s of llama-1 is not terminating; the test wants a group whose Pods are all on their way out", pod.Name)
				}
			}

			rest(s, noPod)
			if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// What the status of a group says about Pods that are being created is not a
// Pod. The replacement of the last old group exists and has committed itself
// to its Pods, and their creation is refused: the surge group stays. It
// leaves when the first of them is there.
func TestTheSurgeGroupStaysWhileThePodsOfTheReplacementCannotBeCreated(t *testing.T) {
	for _, tt := range surgeVariants {
		t.Run(tt.name, func(t *testing.T) {
			s := surgeRollout(t, tt.stuck)
			// Every Pod of llama-0 created from here on belongs to its replacement.
			s.refused = errors.New("exceeded quota")
			s.cluster.BeforeWrite = func(write apitestWrite) error {
				if write.Verb == "create" && write.Resource == "pods" && strings.HasPrefix(write.Name, "llama-0-") {
					return s.refused
				}
				return nil
			}
			// Time passes, so that the surge group is older than the replacement.
			s.invariant = func() { s.cluster.Advance(time.Second) }
			s.settle()
			s.invariant = nil

			if got, want := s.images(), "0=none 1=model:v2 2=model:v2 3=model:v2+"; got != want {
				t.Fatalf("at rest while the Pods of the replacement are refused:\n got %s\nwant %s", got, want)
			}
			if replacement := s.groups()[0]; replacement.Status.Pods == 0 {
				t.Fatalf("status.pods of %s is 0; the test wants a group that has committed itself to its Pods", replacement.Name)
			}

			// Nothing moves. After the deadline the ModelServing says what it
			// waits for: the place without Pods, not the surge group, although
			// a surge group that never gets ready has been not ready for longer.
			s.cluster.Advance(6 * time.Minute)
			s.pass()
			blocked := meta.FindStatusCondition(s.modelServing().Status.Conditions, string(workloadv1alpha1.ModelServingRolloutBlocked))
			if blocked == nil || blocked.Status != metav1.ConditionTrue || blocked.Reason != workloadv1alpha1.RolloutBlockedTargetNotReady ||
				!strings.Contains(blocked.Message, "ServingGroup llama-0 ") || !strings.Contains(blocked.Message, s.refused.Error()) {
				t.Errorf("RolloutBlocked = %+v; want TargetNotReady that names llama-0 and says why it has no Pods", blocked)
			}
			if got, want := s.images(), "0=none 1=model:v2 2=model:v2 3=model:v2+"; got != want {
				t.Errorf("after the deadline:\n got %s\nwant %s", got, want)
			}

			s.cluster.BeforeWrite = nil
			s.settle()
			leftInOrder(t, s)
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

// A group that lost the label naming its ModelServing is still its group. The
// list by that label does not return it; the round must not take its ordinal
// for free.
func TestAGroupWithoutItsLabelIsStillCounted(t *testing.T) {
	s := newSystem(t, modelServing())
	s.settle()
	group := s.groups()[0]
	uid := group.UID
	delete(group.Labels, workloadv1alpha1.ModelServingNameLabelKey)
	if _, err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(context.Background(), group, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// A round that writes reads from the API server: here it adds a group.
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](4) })
	s.settle()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1 3=model:v1] replicas=4 updated=4 current=4 available=4 Available"; got != want {
		t.Errorf("after the scale-up:\n got %s\nwant %s", got, want)
	}
	if got := s.groups()[0]; got.UID != uid {
		t.Errorf("group %s was replaced", got.Name)
	}
}

// A round that is about to write reads the ModelServing and its groups from
// the API server and decides again on what it read. The cache may be behind:
// here it still shows a scale-down that was taken back.
func TestAStaleCacheDoesNotDeleteAGroup(t *testing.T) {
	s := newSystem(t, modelServing())
	s.settle()
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })
	stale, snapshot := s.caches()
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) })
	s.cluster.ResetWrites()

	s.ms.SetCaches(stale, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatal(err)
	}
	for _, write := range s.cluster.Writes() {
		if write.Verb == "delete" {
			t.Errorf("a round on a cache that was behind issued: %s", write)
		}
	}
	if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1"; got != want {
		t.Errorf("groups = %s, want %s", got, want)
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

	t.Run("a group that has reported in between starts to count anew", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		// A new group has not reported yet: a wait begins. Then it reports.
		s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](4) })
		round(s)
		round(s)
		s.settle()
		if got := s.condition(); got != "Available" {
			t.Fatalf("after the scale-up: %s", got)
		}
		// Much later every group is behind again, for a moment. That wait has
		// only just begun.
		s.cluster.Advance(6 * time.Minute)
		s.update(func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Replicas = ptr.To[int32](5)
			ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
		})
		round(s)
		if after := round(s); after <= 0 {
			t.Errorf("the round is due again after %v, want when the deadline passes", after)
		}
		if status, reason := blockedOf(s); status == "True" {
			t.Errorf("RolloutBlocked = True/%s at once, for groups that reported six minutes ago", reason)
		}
	})
}
