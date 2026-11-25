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
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
)

// A deletion is addressed to the Pod the round judged, and to no Pod that
// took its name since. Between the read a round decides on and its DELETE
// another controller manager can have carried the recovery on: the Pod is
// gone and the Pod of the next generation of its Role instance has its name.
// A DELETE by name alone would take that one down.
func TestADeletionDoesNotHitAPodThatTookTheName(t *testing.T) {
	const name = "llama-0-prefill-0-0"
	h := standard(t)
	h.settle()
	h.lose("llama-0-prefill-0-1")

	// The DELETE of the round reaches the API server after the Pod was replaced.
	pods := corev1.SchemeGroupVersion.WithResource("pods")
	tracker := h.cluster.Kube.Tracker()
	replaced := false
	h.cluster.Kube.PrependReactor("delete", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if replaced || action.(k8stesting.DeleteAction).GetName() != name {
			return false, nil, nil
		}
		replaced = true
		stored, err := tracker.Get(pods, testNamespace, name)
		if err != nil {
			return true, nil, err
		}
		replacement := stored.(*corev1.Pod).DeepCopy()
		replacement.UID = "replacement-uid"
		return false, nil, tracker.Update(pods, replacement, testNamespace)
	})

	if _, err := h.round(); !apierrors.IsConflict(err) {
		t.Errorf("round error = %v, want the conflict of a deletion that no longer applies", err)
	}
	if !replaced {
		t.Fatal("the round did not delete the Pod")
	}
	pod, err := h.cluster.GetPod(testNamespace, name)
	if err != nil {
		t.Fatalf("the Pod that took the name: %v", err)
	}
	if pod.UID != "replacement-uid" || pod.DeletionTimestamp != nil {
		t.Errorf("the Pod that took the name: UID %s, deleted at %v; want it untouched", pod.UID, pod.DeletionTimestamp)
	}
}
