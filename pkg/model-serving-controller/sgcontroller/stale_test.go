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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// createdPods returns the Pods a list of writes creates.
func createdPods(writes []string) (out []string) {
	for _, write := range writes {
		if name, ok := strings.CutPrefix(write, "create pods "); ok {
			out = append(out, name)
		}
	}
	return out
}

// The ModelServing lowers the count of a Role. Its controller tells the group
// a moment later; until then the spec of the group still has the old count.
// An instance that is lost in that moment is not created again from the old
// count: the group would have more instances than the ModelServing asks for,
// and would take them away right after. This is what a controller that was
// down while the count was lowered finds when it starts.
func TestNoInstanceIsCreatedFromACountTheModelServingHasLowered(t *testing.T) {
	ms := testModelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{}
		rolledByRole(ms)
	})
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
	h.settle()
	if got := len(h.pods()); got != 7 {
		t.Fatalf("%d Pods, want three instances of two Pods and one of one", got)
	}
	lowerTo := func(count int32, generation int64) {
		t.Helper()
		current, err := h.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		current.Spec.Template.Roles[0].Replicas = ptr.To(count)
		current.Generation = generation
		if _, err := h.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Update(context.Background(), current, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	loseInstance := func(index string) {
		t.Helper()
		for _, member := range []string{"0", "1"} {
			h.lose("llama-0-prefill-" + index + "-" + member)
		}
		h.cluster.Settle()
		h.cluster.ResetWrites()
	}

	// The ModelServing asks for two instances; the group was told three.
	lowerTo(2, ms.Generation+1)
	loseInstance("1")
	for range 3 {
		if created := createdPods(h.mustRound()); len(created) != 0 {
			t.Fatalf("created %v from a count the ModelServing has lowered", created)
		}
		h.cluster.Settle()
	}

	// The group is told. Two instances are there, 0 and 2: nothing is created.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods, group.Spec.SourceGeneration = 2, 5, ms.Generation+1
	})
	h.settle()
	if got := h.podSummary(); strings.Contains(got, "prefill-1-") {
		t.Errorf("Pods after the group was told: %s", got)
	}

	// A count that was raised, or a ModelServing that did not change, holds
	// nothing back: a lost instance is created again.
	lowerTo(4, ms.Generation+2)
	loseInstance("2")
	h.settle()
	if got := len(h.pods()); got != 5 || !h.isReady() {
		t.Errorf("%d Pods, ready %t; want the lost instance created again: %s", got, h.isReady(), h.podSummary())
	}
}

// The instances of a Role that are deleted in one round are deleted in
// descending order of their index.
func TestInstancesOfABatchLeaveInDescendingOrder(t *testing.T) {
	ms := testModelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{}
		rolledByRole(ms)
		ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromString("100%"))
	})
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), revisionOf(t, withImage(ms, "model:v2"), "rev-b"))
	h.settle()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.TargetRevision = "rev-b" })
	h.cluster.ResetWrites()

	var deleted []string
	for _, write := range h.mustRound() {
		if name, ok := strings.CutPrefix(write, "delete pods llama-0-prefill-"); ok {
			deleted = append(deleted, strings.SplitN(name, "-", 2)[0])
		}
	}
	if want := []string{"2", "2", "1", "1", "0", "0"}; !reflect.DeepEqual(deleted, want) {
		t.Errorf("instances deleted in the order %v, want %v", deleted, want)
	}
}

// The cache still shows a Pod of an earlier object of the group although the
// Pod is gone: its deletion never reached the cache. The group is not held
// back for good by it. Holding back rests on a direct read.
func TestAPodOfAnEarlierObjectThatIsGoneDoesNotHoldTheGroup(t *testing.T) {
	ms := testModelServing()
	earlier := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: testNamespace, Name: "llama-0-prefill-0-1", UID: types.UID("earlier"),
		Labels: map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: ms.Name, workloadv1alpha1.GroupNameLabelKey: testGroup,
			workloadv1alpha1.RoleLabelKey: "prefill", workloadv1alpha1.RoleIDKey: "prefill-0",
		},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ServingGroupKind.Kind,
			Name: testGroup, UID: "earlier-sg-uid", Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
		}},
	}}
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), earlier)
	// What the cache shows from now on: the Pod of the earlier object.
	behind := h.cluster.Snapshot()
	if err := h.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), earlier.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.cluster.ResetWrites()

	if _, err := h.roundOn(behind); err != nil {
		t.Fatalf("round on a cache that still shows the Pod: %v", err)
	}
	if created := createdPods(writesOf(h.cluster.Writes())); len(created) != 3 {
		t.Errorf("created %v, want the three members: the Pod of the earlier object is gone", created)
	}
}
