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

package apitest

import (
	"context"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// group builds a ServingGroup with a Pod, a Service and a PodGroup owned by
// it, spread over the three clientsets.
func group(t *testing.T, block bool) (*Cluster, *workloadv1alpha1.ServingGroup) {
	t.Helper()
	c := New(newServingGroup("sg"))
	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get serving group: %v", err)
	}
	owner := servingGroupOwner(sg, block)
	ctx := context.Background()
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1", owner), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	if _, err := c.Kube.CoreV1().Services(testNamespace).Create(ctx, newService("svc", owner), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create service: %v", err)
	}
	if _, err := c.Volcano.SchedulingV1beta1().PodGroups(testNamespace).Create(ctx, newPodGroup("pg", owner), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod group: %v", err)
	}
	c.ResetWrites()
	return c, sg
}

func deleteServingGroup(t *testing.T, c *Cluster, name string, policy metav1.DeletionPropagation) {
	t.Helper()
	err := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), name,
		metav1.DeleteOptions{PropagationPolicy: &policy})
	if err != nil {
		t.Fatalf("delete serving group: %v", err)
	}
}

func dependentsGone(t *testing.T, c *Cluster) bool {
	t.Helper()
	_, podErr := c.GetPod(testNamespace, "p1")
	_, svcErr := c.GetService(testNamespace, "svc")
	_, pgErr := c.GetPodGroup(testNamespace, "pg")
	return apierrors.IsNotFound(podErr) && apierrors.IsNotFound(svcErr) && apierrors.IsNotFound(pgErr)
}

func TestForegroundDeletionDeletesDependentsFirst(t *testing.T) {
	c, _ := group(t, true)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationForeground)

	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("the owner disappeared before its dependents: %v", err)
	}
	if sg.DeletionTimestamp == nil {
		t.Error("the owner has no deletionTimestamp")
	}
	if len(sg.Finalizers) != 1 || sg.Finalizers[0] != metav1.FinalizerDeleteDependents {
		t.Errorf("finalizers = %v, want only %q", sg.Finalizers, metav1.FinalizerDeleteDependents)
	}
	if dependentsGone(t, c) {
		t.Error("dependents were collected without a collector pass")
	}

	if n := c.CollectGarbage(); n != 3 {
		t.Errorf("first pass collected %d objects, want 3 dependents", n)
	}
	if !dependentsGone(t, c) {
		t.Error("the first pass did not delete all dependents")
	}
	if _, err := c.GetServingGroup(testNamespace, "sg"); err != nil {
		t.Errorf("the owner went away in the same pass as its dependents: %v", err)
	}

	c.Settle()
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Errorf("after Settle: got %v, want the owner gone", err)
	}
	if got := c.Writes(); len(got) != 1 || got[0].Verb != verbDelete || got[0].Resource != "servinggroups" {
		t.Errorf("writes = %v, want only the delete request", got)
	}
}

func TestForegroundDeletionWaitsForTerminatingPod(t *testing.T) {
	c, _ := group(t, true)
	c.HoldPodTermination(true)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationForeground)
	c.Settle()

	pod, err := c.GetPod(testNamespace, "p1")
	if err != nil {
		t.Fatalf("the held pod disappeared: %v", err)
	}
	if pod.DeletionTimestamp == nil {
		t.Error("the held pod has no deletionTimestamp")
	}
	if pod.DeletionGracePeriodSeconds == nil || *pod.DeletionGracePeriodSeconds != defaultPodGracePeriodSeconds {
		t.Errorf("deletionGracePeriodSeconds = %v, want %d", pod.DeletionGracePeriodSeconds, defaultPodGracePeriodSeconds)
	}
	if _, err := c.GetServingGroup(testNamespace, "sg"); err != nil {
		t.Fatalf("the owner finished while a blocking pod was terminating: %v", err)
	}

	if err := c.FinishTermination(testNamespace, "p1"); err != nil {
		t.Fatalf("finish termination: %v", err)
	}
	c.Settle()
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Errorf("after the pod terminated: got %v, want the owner gone", err)
	}
}

func TestBackgroundDeletionRemovesOwnerAtOnce(t *testing.T) {
	c, _ := group(t, true)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationBackground)

	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Fatalf("owner: got %v, want it gone at once", err)
	}
	if dependentsGone(t, c) {
		t.Error("dependents were collected without a collector pass")
	}
	c.Settle()
	if !dependentsGone(t, c) {
		t.Error("the collector left dependents of a deleted owner behind")
	}
}

func TestDefaultPropagationIsBackground(t *testing.T) {
	c, _ := group(t, true)
	if err := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), "sg", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Fatalf("owner: got %v, want it gone at once", err)
	}
}

func TestOrphanDeletionReleasesDependents(t *testing.T) {
	c, sg := group(t, true)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationOrphan)

	stored, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("the owner disappeared before orphaning: %v", err)
	}
	if len(stored.Finalizers) != 1 || stored.Finalizers[0] != metav1.FinalizerOrphanDependents {
		t.Errorf("finalizers = %v, want only %q", stored.Finalizers, metav1.FinalizerOrphanDependents)
	}

	c.Settle()
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Errorf("after Settle: got %v, want the owner gone", err)
	}
	pod, err := c.GetPod(testNamespace, "p1")
	if err != nil {
		t.Fatalf("the pod was collected instead of orphaned: %v", err)
	}
	if ownerReferenceTo(pod, sg.UID) != nil {
		t.Errorf("ownerReferences = %v, want the reference removed", pod.OwnerReferences)
	}
	if _, err := c.GetService(testNamespace, "svc"); err != nil {
		t.Errorf("the service was collected instead of orphaned: %v", err)
	}
	if _, err := c.GetPodGroup(testNamespace, "pg"); err != nil {
		t.Errorf("the pod group was collected instead of orphaned: %v", err)
	}
}

func TestDependentSurvivesWhileOneOwnerExists(t *testing.T) {
	c := New(newServingGroup("sg-a"), newServingGroup("sg-b"))
	ctx := context.Background()
	sgA, err := c.GetServingGroup(testNamespace, "sg-a")
	if err != nil {
		t.Fatalf("get sg-a: %v", err)
	}
	sgB, err := c.GetServingGroup(testNamespace, "sg-b")
	if err != nil {
		t.Fatalf("get sg-b: %v", err)
	}
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx,
		newPod("p1", servingGroupOwner(sgA, false), servingGroupOwner(sgB, false)), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	deleteServingGroup(t, c, "sg-a", metav1.DeletePropagationBackground)
	c.Settle()
	if _, err := c.GetPod(testNamespace, "p1"); err != nil {
		t.Fatalf("the pod was collected while one owner was still there: %v", err)
	}

	deleteServingGroup(t, c, "sg-b", metav1.DeletePropagationBackground)
	c.Settle()
	if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("after the last owner was deleted: got %v, want the pod collected", err)
	}
}

func TestOwnerWithDifferentUIDCountsAsGone(t *testing.T) {
	c := New(newServingGroup("sg"))
	ctx := context.Background()
	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	stale := servingGroupOwner(sg, false)
	stale.UID = types.UID("a-previous-incarnation")
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1", stale), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	c.Settle()
	if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("pod: got %v, want it collected because the owner UID does not match", err)
	}

	// A matching UID keeps the dependent alive.
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p2", servingGroupOwner(sg, false)), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	c.Settle()
	if _, err := c.GetPod(testNamespace, "p2"); err != nil {
		t.Errorf("pod with a live owner: %v", err)
	}
}

func TestCascadingDeletionAcrossTwoLevels(t *testing.T) {
	c := New(newModelServing("ms"))
	ctx := context.Background()
	ms, err := c.GetModelServing(testNamespace, "ms")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	msOwner := ownerRef(workloadv1alpha1.SchemeGroupVersion.String(), "ModelServing", ms.Name, ms.UID, true)
	if _, err := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Create(ctx, newServingGroup("sg", msOwner), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create serving group: %v", err)
	}
	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get serving group: %v", err)
	}
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1", servingGroupOwner(sg, true)), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	if err := c.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, "ms", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete model serving: %v", err)
	}
	c.Settle()
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Errorf("serving group: got %v, want it collected", err)
	}
	if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("pod: got %v, want it collected", err)
	}
}

func TestDeletePreconditions(t *testing.T) {
	tests := []struct {
		name         string
		options      func(pod *metav1.ObjectMeta) metav1.DeleteOptions
		wantConflict bool
	}{
		{
			name: "matching uid and resourceVersion",
			options: func(m *metav1.ObjectMeta) metav1.DeleteOptions {
				return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{
					UID:             &m.UID,
					ResourceVersion: &m.ResourceVersion,
				}}
			},
		},
		{
			name: "wrong uid",
			options: func(m *metav1.ObjectMeta) metav1.DeleteOptions {
				return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: ptr.To(types.UID("other"))}}
			},
			wantConflict: true,
		},
		{
			name: "stale resourceVersion",
			options: func(m *metav1.ObjectMeta) metav1.DeleteOptions {
				return metav1.DeleteOptions{Preconditions: &metav1.Preconditions{ResourceVersion: ptr.To("1")}}
			},
			wantConflict: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(newPod("p1"))
			ctx := context.Background()
			pod := mustGetPod(t, c, "p1")
			pod.Labels = map[string]string{"k": "v"}
			pod, err := c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("priming update: %v", err)
			}

			err = c.Kube.CoreV1().Pods(testNamespace).Delete(ctx, "p1", tt.options(&pod.ObjectMeta))
			if tt.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("delete: got %v, want Conflict", err)
				}
				if _, err := c.GetPod(testNamespace, "p1"); err != nil {
					t.Errorf("the rejected delete removed the pod: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("delete: %v", err)
			}
			if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
				t.Errorf("after delete: got %v, want NotFound", err)
			}
		})
	}
}

func TestDeleteOfMissingObjectIsNotFound(t *testing.T) {
	c := New()
	err := c.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), "nope", metav1.DeleteOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("delete: got %v, want NotFound", err)
	}
	if got := c.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestFinalizersKeepAnObjectTerminating(t *testing.T) {
	c := New(newPod("p1"))
	ctx := context.Background()
	pods := c.Kube.CoreV1().Pods(testNamespace)

	pod := mustGetPod(t, c, "p1")
	pod.Finalizers = []string{"example.com/wait"}
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := pods.Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	pod = mustGetPod(t, c, "p1")
	if pod.DeletionTimestamp == nil {
		t.Fatal("the pod has no deletionTimestamp")
	}
	pod.Finalizers = nil
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("removing the finalizer: %v", err)
	}
	if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("after the last finalizer was removed: got %v, want the pod gone", err)
	}
}

func TestHeldPodTerminationIsExplicit(t *testing.T) {
	c := New(newPod("p1"))
	ctx := context.Background()
	c.HoldPodTermination(true)

	if err := c.Kube.CoreV1().Pods(testNamespace).Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	pod := mustGetPod(t, c, "p1")
	if pod.DeletionTimestamp == nil {
		t.Error("the pod has no deletionTimestamp")
	}
	if err := c.FinishTermination(testNamespace, "p1"); err != nil {
		t.Fatalf("finish termination: %v", err)
	}
	if _, err := c.GetPod(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("after FinishTermination: got %v, want the pod gone", err)
	}
	if err := c.FinishTermination(testNamespace, "p1"); !apierrors.IsNotFound(err) {
		t.Errorf("finishing a gone pod: got %v, want NotFound", err)
	}

	// A pod that is not terminating cannot be finished.
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p2"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.FinishTermination(testNamespace, "p2"); err == nil {
		t.Error("finishing a live pod: got no error, want one")
	}
}

func TestCollectGarbageIsStepwise(t *testing.T) {
	c, _ := group(t, false)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationForeground)

	// Pass one deletes the three dependents, pass two removes the finalizer.
	if n := c.CollectGarbage(); n != 3 {
		t.Errorf("first pass = %d, want 3", n)
	}
	if n := c.CollectGarbage(); n != 1 {
		t.Errorf("second pass = %d, want 1 (the owner)", n)
	}
	if n := c.CollectGarbage(); n != 0 {
		t.Errorf("third pass = %d, want 0", n)
	}
}

func TestForegroundDeletionWithoutDependents(t *testing.T) {
	c := New(newServingGroup("sg"))
	c.Advance(time.Hour)
	deleteServingGroup(t, c, "sg", metav1.DeletePropagationForeground)

	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("the owner disappeared before a collector pass: %v", err)
	}
	if sg.DeletionTimestamp == nil {
		t.Fatal("the owner has no deletionTimestamp")
	}
	if got := sg.DeletionTimestamp.Time; !got.Equal(DefaultStartTime.Add(time.Hour)) {
		t.Errorf("deletionTimestamp = %v, want the advanced clock %v", got, DefaultStartTime.Add(time.Hour))
	}
	if n := c.CollectGarbage(); n != 1 {
		t.Errorf("first pass = %d, want 1", n)
	}
	if _, err := c.GetServingGroup(testNamespace, "sg"); !apierrors.IsNotFound(err) {
		t.Errorf("after one pass: got %v, want the owner gone", err)
	}
}
