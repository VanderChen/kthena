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
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// A scale-in removes the group whose Pods are not ready, then the one that
// costs least to delete, then the one with the highest ordinal. Both are read
// from the Pods of the group as the cache shows them: the cost of a group is
// the sum of the pod-deletion-cost annotations of its Pods, where a value that
// is no number counts as nothing, and a group is not ready when one of its
// Pods is not ready or is being deleted, whatever its status says.
//
// This is how a user, or an autoscaler that bin-packs, says which group to
// give up. Read wrongly, a scale-in takes down the group that serves.
func TestTheGroupToScaleInIsChosenByItsPods(t *testing.T) {
	const cost = corev1.PodDeletionCost
	tests := []struct {
		name string
		// costs holds the pod-deletion-cost annotation of Pods, by name.
		costs map[string]string
		// notReady and terminating each name a Pod that stops being ready, or
		// is deleted, without its group having reported on it.
		notReady, terminating string
		// want lists the groups after the round, the one that goes with "!".
		want string
	}{
		{name: "nothing on the Pods: the highest ordinal", want: "0=model:v1 1=model:v1 2=model:v1!"},
		{
			name: "the lowest cost, summed over the Pods of the group",
			costs: map[string]string{
				"llama-0-serve-0-0": "60", "llama-0-serve-0-1": "60",
				"llama-1-serve-0-0": "100",
				"llama-2-serve-0-0": "200", "llama-2-serve-0-1": "200",
			},
			want: "0=model:v1 1=model:v1! 2=model:v1",
		},
		{
			name:  "a negative cost is below none",
			costs: map[string]string{"llama-1-serve-0-1": "-50", "llama-2-serve-0-0": "10"},
			want:  "0=model:v1 1=model:v1! 2=model:v1",
		},
		{
			name: "a cost that is no number counts as none",
			costs: map[string]string{
				"llama-0-serve-0-0": "invalid", "llama-0-serve-0-1": "",
				"llama-1-serve-0-0": "10", "llama-2-serve-0-0": "20",
			},
			want: "0=model:v1! 1=model:v1 2=model:v1",
		},
		{
			name: "a Pod that is not ready, before any cost",
			costs: map[string]string{
				"llama-0-serve-0-0": "1000", "llama-0-serve-0-1": "1000",
			},
			notReady: "llama-0-serve-0-1",
			want:     "0=model:v1! 1=model:v1 2=model:v1",
		},
		{
			name:        "a Pod that is being deleted is not ready",
			terminating: "llama-1-serve-0-0",
			want:        "0=model:v1 1=model:v1! 2=model:v1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing())
			s.settle()
			pods := s.cluster.Kube.CoreV1().Pods(testNamespace)
			for name, value := range tt.costs {
				pod, err := s.cluster.GetPod(testNamespace, name)
				if err != nil {
					t.Fatal(err)
				}
				pod.Annotations = map[string]string{cost: value}
				if _, err := pods.Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			// The groups report on the annotations. What follows they do not see.
			s.settle()
			if tt.notReady != "" {
				pod, err := s.cluster.GetPod(testNamespace, tt.notReady)
				if err != nil {
					t.Fatal(err)
				}
				pod.Status.Conditions = []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(s.cluster.Now()),
				}}
				if _, err := pods.UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.terminating != "" {
				s.cluster.HoldPodTermination(true)
				if err := pods.Delete(context.Background(), tt.terminating, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			for _, group := range s.groups() {
				if !groupReady(group) {
					t.Fatalf("group %s reports that it is not ready; the test wants the order to be read from the Pods", group.Name)
				}
			}

			s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })
			caches, snapshot := s.caches()
			s.ms.SetCaches(caches, snapshot.Listers().Services)
			if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); err != nil && !errors.Is(err, ErrNotSettled) {
				t.Fatal(err)
			}
			if got := s.images(); got != tt.want {
				t.Errorf("groups after the round = %q, want %q", got, tt.want)
			}
		})
	}
}
