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
	"fmt"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestMergePatchPreconditions(t *testing.T) {
	tests := []struct {
		name         string
		patch        func(pod *corev1.Pod) string
		wantConflict bool
	}{
		{
			name: "matching uid and resourceVersion",
			patch: func(pod *corev1.Pod) string {
				return fmt.Sprintf(`{"metadata":{"uid":%q,"resourceVersion":%q,"labels":{"k":"v"}}}`,
					pod.UID, pod.ResourceVersion)
			},
		},
		{
			name: "no preconditions",
			patch: func(*corev1.Pod) string {
				return `{"metadata":{"labels":{"k":"v"}}}`
			},
		},
		{
			name: "wrong uid",
			patch: func(pod *corev1.Pod) string {
				return fmt.Sprintf(`{"metadata":{"uid":"not-the-uid","resourceVersion":%q,"labels":{"k":"v"}}}`,
					pod.ResourceVersion)
			},
			wantConflict: true,
		},
		{
			name: "stale resourceVersion",
			patch: func(pod *corev1.Pod) string {
				return fmt.Sprintf(`{"metadata":{"uid":%q,"resourceVersion":"1","labels":{"k":"v"}}}`, pod.UID)
			},
			wantConflict: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(newPod("p1"))
			ctx := context.Background()
			pod := mustGetPod(t, c, "p1")
			// Move the resourceVersion away from the seeded one.
			pod.Annotations = map[string]string{"a": "b"}
			pod, err := c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("priming update: %v", err)
			}

			_, err = c.Kube.CoreV1().Pods(testNamespace).Patch(ctx, "p1", types.MergePatchType, []byte(tt.patch(pod)), metav1.PatchOptions{})
			after := mustGetPod(t, c, "p1")
			if tt.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("patch: got %v, want Conflict", err)
				}
				if len(after.Labels) != 0 {
					t.Errorf("labels = %v, want the patch to have been rejected", after.Labels)
				}
				if after.ResourceVersion != pod.ResourceVersion {
					t.Errorf("resourceVersion = %q, want it unchanged", after.ResourceVersion)
				}
				return
			}
			if err != nil {
				t.Fatalf("patch: %v", err)
			}
			if after.Labels["k"] != "v" {
				t.Errorf("labels = %v, want the patch applied", after.Labels)
			}
			if after.UID != pod.UID {
				t.Errorf("uid = %q, want the patch to leave it alone", after.UID)
			}
			if after.ResourceVersion == pod.ResourceVersion {
				t.Error("resourceVersion was not bumped")
			}
			if after.Annotations["a"] != "b" {
				t.Error("the merge patch dropped an untouched field")
			}
		})
	}
}

func TestMergePatchSemantics(t *testing.T) {
	c := New(newPod("p1"))
	ctx := context.Background()
	pods := c.Kube.CoreV1().Pods(testNamespace)

	pod := mustGetPod(t, c, "p1")
	pod.Labels = map[string]string{"keep": "yes", "drop": "yes"}
	if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}

	patched, err := pods.Patch(ctx, "p1", types.MergePatchType,
		[]byte(`{"metadata":{"labels":{"drop":null,"added":"yes"}}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("patch: %v", err)
	}
	if _, ok := patched.Labels["drop"]; ok {
		t.Error("null in a merge patch did not remove the label")
	}
	if patched.Labels["keep"] != "yes" || patched.Labels["added"] != "yes" {
		t.Errorf("labels = %v, want keep and added", patched.Labels)
	}

	want := []Write{
		{Verb: verbUpdate, Resource: "pods", Namespace: testNamespace, Name: "p1"},
		{Verb: verbPatch, Resource: "pods", Namespace: testNamespace, Name: "p1"},
	}
	if got := c.Writes(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("writes = %v, want %v", got, want)
	}
}

func TestPatchRespectsSubresourceBoundaries(t *testing.T) {
	c := New(newPod("p1"), newServingGroup("sg"))
	ctx := context.Background()

	// A patch of the main resource may not change status.
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Patch(ctx, "p1", types.MergePatchType,
		[]byte(`{"status":{"phase":"Running"},"metadata":{"labels":{"k":"v"}}}`), metav1.PatchOptions{}); err != nil {
		t.Fatalf("patch pod: %v", err)
	}
	pod := mustGetPod(t, c, "p1")
	if pod.Status.Phase != "" {
		t.Errorf("phase = %q, want a main-resource patch to leave status alone", pod.Status.Phase)
	}
	if pod.Labels["k"] != "v" {
		t.Error("the metadata part of the patch was not applied")
	}

	// A patch of the status subresource may not change spec.
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Patch(ctx, "p1", types.MergePatchType,
		[]byte(`{"status":{"phase":"Running"},"spec":{"activeDeadlineSeconds":9}}`), metav1.PatchOptions{}, "status"); err != nil {
		t.Fatalf("patch pod status: %v", err)
	}
	pod = mustGetPod(t, c, "p1")
	if pod.Status.Phase != corev1.PodRunning {
		t.Errorf("phase = %q, want Running", pod.Status.Phase)
	}
	if pod.Spec.ActiveDeadlineSeconds != nil {
		t.Error("a status patch changed spec")
	}
}

func TestStrategicMergePatchOnlyForBuiltins(t *testing.T) {
	c := New(newPod("p1"), newServingGroup("sg"))
	ctx := context.Background()

	pod, err := c.Kube.CoreV1().Pods(testNamespace).Patch(ctx, "p1", types.StrategicMergePatchType,
		[]byte(`{"metadata":{"labels":{"k":"v"}}}`), metav1.PatchOptions{})
	if err != nil {
		t.Fatalf("strategic patch: %v", err)
	}
	if pod.Labels["k"] != "v" {
		t.Errorf("labels = %v, want the patch applied", pod.Labels)
	}
	if len(pod.Spec.Containers) != 1 {
		t.Errorf("containers = %v, want the strategic merge to keep them", pod.Spec.Containers)
	}

	_, err = c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Patch(ctx, "sg", types.StrategicMergePatchType,
		[]byte(`{"metadata":{"labels":{"k":"v"}}}`), metav1.PatchOptions{})
	if err == nil {
		t.Fatal("strategic patch of a custom resource: got no error, want one")
	}
}
