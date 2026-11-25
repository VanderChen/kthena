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

package controller

import (
	"context"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func managerPod(namespace, name string, mutate ...func(*corev1.Pod)) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			Labels: map[string]string{componentLabel: componentManager},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
	for _, m := range mutate {
		m(pod)
	}
	return pod
}

func TestPredecessors(t *testing.T) {
	current := func(pod *corev1.Pod) { pod.Labels[ControllerProtocolLabel] = ControllerProtocolServingGroup }
	phase := func(phase corev1.PodPhase) func(*corev1.Pod) {
		return func(pod *corev1.Pod) { pod.Status.Phase = phase }
	}
	terminating := func(pod *corev1.Pod) { pod.DeletionTimestamp = ptr.To(metav1.Now()) }

	kube := fake.NewSimpleClientset(
		// This process, and another instance of this version.
		managerPod("kthena-system", "self", current),
		managerPod("kthena-system", "peer", current),
		// Earlier versions: running, not yet started, and still shutting down.
		managerPod("kthena-system", "old-running"),
		managerPod("kthena-system", "old-pending", phase(corev1.PodPending)),
		managerPod("kthena-system", "old-terminating", terminating),
		// Earlier versions that have stopped for good.
		managerPod("kthena-system", "old-succeeded", phase(corev1.PodSucceeded)),
		managerPod("kthena-system", "old-failed", phase(corev1.PodFailed)),
		// Not a controller manager, and a controller manager elsewhere.
		managerPod("kthena-system", "router", func(pod *corev1.Pod) { pod.Labels[componentLabel] = "kthena-router" }),
		managerPod("other", "old-elsewhere"),
	)

	got, err := predecessors(context.Background(), kube, "kthena-system", "self")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"old-pending", "old-running", "old-terminating"}; !reflect.DeepEqual(got, want) {
		t.Errorf("predecessors() = %v, want %v", got, want)
	}

	// A Pod of this version that lacks the label, as the process itself does
	// when the Deployment was edited by hand, is not its own predecessor.
	if got, _ := predecessors(context.Background(), kube, "kthena-system", "old-running"); len(got) != 2 {
		t.Errorf("predecessors() of an unlabelled process = %v, want the two other Pods without the label", got)
	}
}

func TestLabelPod(t *testing.T) {
	kube := fake.NewSimpleClientset(
		// Two replicas of a Deployment whose Pod template lacks the label.
		managerPod("kthena-system", "self"),
		managerPod("kthena-system", "peer"),
	)
	ctx := context.Background()
	if got, _ := predecessors(ctx, kube, "kthena-system", "peer"); !reflect.DeepEqual(got, []string{"self"}) {
		t.Fatalf("before: the peer waits for %v, want [self]", got)
	}

	// Twice: the second time there is nothing to do.
	for range 2 {
		if err := labelPod(ctx, kube, "kthena-system", "self"); err != nil {
			t.Fatal(err)
		}
	}
	if err := labelPod(ctx, kube, "kthena-system", "gone"); err == nil {
		t.Error("labelling a Pod that does not exist reported success")
	}

	pod, err := kube.CoreV1().Pods("kthena-system").Get(ctx, "self", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Labels[ControllerProtocolLabel] != ControllerProtocolServingGroup || pod.Labels[componentLabel] != componentManager {
		t.Errorf("labels of the Pod of the process = %v", pod.Labels)
	}
	if got, _ := predecessors(ctx, kube, "kthena-system", "peer"); len(got) != 0 {
		t.Errorf("after: the peer still waits for %v", got)
	}
	if other, _ := kube.CoreV1().Pods("kthena-system").Get(ctx, "peer", metav1.GetOptions{}); other.Labels[ControllerProtocolLabel] != "" {
		t.Errorf("the process labelled another Pod: %v", other.Labels)
	}
}
