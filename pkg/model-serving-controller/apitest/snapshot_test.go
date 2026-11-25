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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
)

func TestSnapshotIsIndependentOfLaterWrites(t *testing.T) {
	c := New(newPod("p1"), newServingGroup("sg"), newPodGroup("pg"))
	ctx := context.Background()

	before := c.Snapshot()

	pod := mustGetPod(t, c, "p1")
	pod.Labels = map[string]string{"k": "v"}
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p2"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.Volcano.SchedulingV1beta1().PodGroups(testNamespace).Delete(ctx, "pg", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	stale := before.Listers()
	stalePod, err := stale.Pods.Pods(testNamespace).Get("p1")
	if err != nil {
		t.Fatalf("stale get: %v", err)
	}
	if len(stalePod.Labels) != 0 {
		t.Errorf("stale pod labels = %v, want none", stalePod.Labels)
	}
	if _, err := stale.Pods.Pods(testNamespace).Get("p2"); !apierrors.IsNotFound(err) {
		t.Errorf("stale get of a later pod: got %v, want NotFound", err)
	}
	if _, err := stale.PodGroups.PodGroups(testNamespace).Get("pg"); err != nil {
		t.Errorf("stale get of a deleted pod group: %v, want it still cached", err)
	}

	fresh := c.Snapshot().Listers()
	freshPod, err := fresh.Pods.Pods(testNamespace).Get("p1")
	if err != nil {
		t.Fatalf("fresh get: %v", err)
	}
	if freshPod.Labels["k"] != "v" {
		t.Errorf("fresh pod labels = %v, want the update", freshPod.Labels)
	}
	pods, err := fresh.Pods.Pods(testNamespace).List(labels.Everything())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(pods) != 2 {
		t.Errorf("fresh pods = %d, want 2", len(pods))
	}
	if _, err := fresh.PodGroups.PodGroups(testNamespace).Get("pg"); !apierrors.IsNotFound(err) {
		t.Errorf("fresh get of a deleted pod group: got %v, want NotFound", err)
	}
}

func TestSnapshotHoldsCopies(t *testing.T) {
	c := New(newPod("p1"))
	snapshot := c.Snapshot()

	item, exists, err := snapshot.Pods.GetByKey(testNamespace + "/p1")
	if err != nil || !exists {
		t.Fatalf("GetByKey: exists=%v err=%v", exists, err)
	}
	item.(*corev1.Pod).Labels = map[string]string{"mutated": "yes"}

	stored := mustGetPod(t, c, "p1")
	if len(stored.Labels) != 0 {
		t.Errorf("the cluster object changed with the snapshot: labels = %v", stored.Labels)
	}
}

func TestSnapshotIndexesByNamespace(t *testing.T) {
	other := newPod("p1")
	other.Namespace = "other"
	c := New(newPod("p1"), other)

	indexed, err := c.Snapshot().Pods.ByIndex(cache.NamespaceIndex, testNamespace)
	if err != nil {
		t.Fatalf("ByIndex: %v", err)
	}
	if len(indexed) != 1 {
		t.Fatalf("pods in %s = %d, want 1", testNamespace, len(indexed))
	}
	if got := indexed[0].(*corev1.Pod).Namespace; got != testNamespace {
		t.Errorf("namespace = %q, want %q", got, testNamespace)
	}
}
