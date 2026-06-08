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

package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	kthenainformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestEvictionHandler(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    &intstr.IntOrString{Type: intstr.Int, IntVal: 2},
				},
			},
		},
	}

	// Create pods for 3 groups
	pods := []*corev1.Pod{
		createPod("pod-g1", "ms-0", true),
		createPod("pod-g2", "ms-1", true),
		createPod("pod-g3", "ms-2", true),
	}

	fakeKubeClient := fake.NewSimpleClientset()
	fakeKthenaClient := kthenafake.NewSimpleClientset(ms)

	kubeInformerFactory := informers.NewSharedInformerFactory(fakeKubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	for _, p := range pods {
		podInformer.Informer().GetStore().Add(p)
	}

	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(fakeKthenaClient, 0)
	msInformer := kthenaInformerFactory.Workload().V1alpha1().ModelServings()
	msInformer.Informer().GetStore().Add(ms)

	handler := NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister())

	t.Run("Allow when above minAvailable", func(t *testing.T) {
		// 3 groups ready, minAvailable 2. Evicting one group leaves 2. Allowed.
		resp := handleEvictionRequest(handler, "pod-g1")
		assert.True(t, resp.Allowed)
	})

	t.Run("Deny when at minAvailable", func(t *testing.T) {
		// Mock Informer state: pod-g1 is now "deleting" (not ready)
		pods[0].DeletionTimestamp = &metav1.Time{Time: time.Now()}
		podInformer.Informer().GetStore().Update(pods[0])

		// 2 groups ready (g2, g3), minAvailable 2. Evicting one more group leaves 1. Denied.
		resp := handleEvictionRequest(handler, "pod-g2")
		assert.False(t, resp.Allowed)
		assert.Equal(t, int32(http.StatusTooManyRequests), resp.Result.Code)
	})

	t.Run("Concurrency protection via tracker", func(t *testing.T) {
		// Reset state: all 3 pods ready
		pods[0].DeletionTimestamp = nil
		podInformer.Informer().GetStore().Update(pods[0])
		clearTracker(t, fakeKubeClient, ms)
		anotherHandler := NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister())

		// 1. Evict pod-g1. Should be allowed and recorded in tracker.
		resp1 := handleEvictionRequest(handler, "pod-g1")
		assert.True(t, resp1.Allowed)

		// 2. Immediately evict pod-g2 through another handler instance. Even if
		// Informer hasn't updated pod-g1, the shared ConfigMap tracker should
		// mark g1 as not ready across webhook replicas.
		// Current effectively ready: g2, g3 (Total 2).
		// Evicting g2 would leave 1. Denied.
		resp2 := handleEvictionRequest(anotherHandler, "pod-g2")
		assert.False(t, resp2.Allowed)
		assert.Contains(t, resp2.Result.Message, "Current ready groups (2) <= minAvailable (2)")
	})
}

func TestEvictionHandlerAllowsSameTrackedServingGroup(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(2)),
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createRolePod("pod-g1-prefill", "ms-0", "prefill", "prefill-0", true),
		createRolePod("pod-g1-decode", "ms-0", "decode", "decode-0", true),
		createRolePod("pod-g2-prefill", "ms-1", "prefill", "prefill-0", true),
		createRolePod("pod-g2-decode", "ms-1", "decode", "decode-0", true),
		createRolePod("pod-g3-prefill", "ms-2", "prefill", "prefill-0", true),
		createRolePod("pod-g3-decode", "ms-2", "decode", "decode-0", true),
	}

	handler := newTestEvictionHandler(ms, pods)

	resp1 := handleEvictionRequest(handler, "pod-g1-prefill")
	assert.True(t, resp1.Allowed)

	resp2 := handleEvictionRequest(handler, "pod-g1-decode")
	assert.True(t, resp2.Allowed)

	resp3 := handleEvictionRequest(handler, "pod-g2-prefill")
	assert.False(t, resp3.Allowed)
	assert.Contains(t, resp3.Result.Message, "Current ready groups (2) <= minAvailable (2)")
}

func TestEvictionHandlerDeniesUntrackedNotReadyServingGroupAtMinAvailable(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(2)),
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createPod("pod-g1", "ms-0", false),
		createPod("pod-g2", "ms-1", true),
		createPod("pod-g3", "ms-2", true),
	}

	handler := newTestEvictionHandler(ms, pods)

	resp := handleEvictionRequest(handler, "pod-g1")
	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "Target group ms-0 is not ready and not tracked")
}

func TestEvictionHandlerDeniesWhenTargetPodMissingFromLister(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(3)),
				},
			},
		},
	}
	targetPod := createPod("pod-g1", "ms-0", true)
	podsInCache := []*corev1.Pod{
		createPod("pod-g2", "ms-1", true),
		createPod("pod-g3", "ms-2", true),
	}

	fakeKubeClient := fake.NewSimpleClientset(targetPod)
	fakeKthenaClient := kthenafake.NewSimpleClientset(ms)

	kubeInformerFactory := informers.NewSharedInformerFactory(fakeKubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	for _, p := range podsInCache {
		podInformer.Informer().GetStore().Add(p)
	}

	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(fakeKthenaClient, 0)
	msInformer := kthenaInformerFactory.Workload().V1alpha1().ModelServings()
	msInformer.Informer().GetStore().Add(ms)

	handler := NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister())

	resp := handleEvictionRequest(handler, "pod-g1")
	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Result.Message, "Current ready groups (3) <= minAvailable (3)")
}

func TestEvictionHandlerConcurrentServingGroupBurstAllowsOneGroup(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(2)),
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createRolePod("pod-g1-prefill", "ms-0", "prefill", "prefill-0", true),
		createRolePod("pod-g1-decode", "ms-0", "decode", "decode-0", true),
		createRolePod("pod-g2-prefill", "ms-1", "prefill", "prefill-0", true),
		createRolePod("pod-g2-decode", "ms-1", "decode", "decode-0", true),
		createRolePod("pod-g3-prefill", "ms-2", "prefill", "prefill-0", true),
		createRolePod("pod-g3-decode", "ms-2", "decode", "decode-0", true),
	}
	handler := newTestEvictionHandler(ms, pods)

	var wg sync.WaitGroup
	responses := make(chan *admissionv1.AdmissionResponse, len(pods))
	for _, pod := range pods {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			responses <- handleEvictionRequest(handler, podName)
		}(pod.Name)
	}
	wg.Wait()
	close(responses)

	allowed := 0
	denied := 0
	for resp := range responses {
		if resp.Allowed {
			allowed++
		} else {
			denied++
			assert.Contains(t, resp.Result.Message, "Current ready groups (2) <= minAvailable (2)")
		}
	}

	assert.Equal(t, 2, allowed)
	assert.Equal(t, 4, denied)
}

func TestEvictionHandlerClearsRecoveredServingGroupTracker(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(3),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(2)),
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createRolePodWithUID("pod-g1-prefill", "ms-0", "prefill", "prefill-0", "new-prefill-uid", true),
		createRolePodWithUID("pod-g1-decode", "ms-0", "decode", "decode-0", "new-decode-uid", true),
		createRolePodWithUID("pod-g2-prefill", "ms-1", "prefill", "prefill-0", "g2-prefill-uid", true),
		createRolePodWithUID("pod-g2-decode", "ms-1", "decode", "decode-0", "g2-decode-uid", true),
		createRolePodWithUID("pod-g3-prefill", "ms-2", "prefill", "prefill-0", "g3-prefill-uid", true),
		createRolePodWithUID("pod-g3-decode", "ms-2", "decode", "decode-0", "g3-decode-uid", true),
	}
	handler := newTestEvictionHandler(ms, pods)
	unitKey := servingGroupUnit(ms, "ms-0").key()
	entries := disruptionEntries{
		unitKey: {
			expiresAt:      time.Now().Add(time.Minute),
			triggerPodUID:  "old-prefill-uid",
			triggerPodName: "pod-g1-prefill",
		},
	}
	cleanupRecoveredDisruptionEntries(entries, pods)

	allowed, reason, unit := handler.checkServingGroupProtection(ms, pods[2], ms.Spec.RolloutStrategy.EvictionStrategy, pods, entries)

	assert.True(t, allowed)
	assert.Empty(t, reason)
	assert.NotNil(t, unit)
	assert.Equal(t, servingGroupUnit(ms, "ms-1").key(), unit.key())
	assert.NotContains(t, entries, unitKey)
}

func TestEvictionHandlerRoleProtection(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(2),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelRole,
					MinAvailable:    intstrPtr(intstr.FromInt(0)),
					RoleMinAvailable: map[string]intstr.IntOrString{
						"decode": intstr.FromInt(2),
					},
				},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:           "decode",
						Replicas:       int32Ptr(3),
						WorkerReplicas: 1,
						WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{},
					},
				},
			},
		},
	}

	pods := []*corev1.Pod{
		createRolePod("decode-0-entry", "ms-0", "decode", "decode-0", true),
		createRolePod("decode-0-worker", "ms-0", "decode", "decode-0", true),
		createRolePod("decode-1-entry", "ms-0", "decode", "decode-1", true),
		createRolePod("decode-1-worker", "ms-0", "decode", "decode-1", true),
		createRolePod("decode-2-entry", "ms-0", "decode", "decode-2", true),
		createRolePod("decode-2-worker", "ms-0", "decode", "decode-2", true),
		createRolePod("decode-other-0-entry", "ms-1", "decode", "decode-0", true),
		createRolePod("decode-other-0-worker", "ms-1", "decode", "decode-0", true),
		createRolePod("decode-other-1-entry", "ms-1", "decode", "decode-1", true),
		createRolePod("decode-other-1-worker", "ms-1", "decode", "decode-1", true),
		createRolePod("decode-other-2-entry", "ms-1", "decode", "decode-2", true),
		createRolePod("decode-other-2-worker", "ms-1", "decode", "decode-2", true),
	}

	fakeKubeClient := fake.NewSimpleClientset()
	fakeKthenaClient := kthenafake.NewSimpleClientset(ms)

	kubeInformerFactory := informers.NewSharedInformerFactory(fakeKubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	for _, p := range pods {
		podInformer.Informer().GetStore().Add(p)
	}

	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(fakeKthenaClient, 0)
	msInformer := kthenaInformerFactory.Workload().V1alpha1().ModelServings()
	msInformer.Informer().GetStore().Add(ms)

	handler := NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister())

	resp1 := handleEvictionRequest(handler, "decode-0-entry")
	assert.True(t, resp1.Allowed)

	// Same role-id is already disrupted, so draining its other Pod should not consume more budget.
	resp2 := handleEvictionRequest(handler, "decode-0-worker")
	assert.True(t, resp2.Allowed)

	// Another role instance in the same ServingGroup would reduce this group's
	// decode role instances below roleMinAvailable.
	resp3 := handleEvictionRequest(handler, "decode-1-entry")
	assert.False(t, resp3.Allowed)
	assert.Contains(t, resp3.Result.Message, "ServingGroup ms-0 role decode ready instances (2) <= minAvailable (2)")

	// The same role in another ServingGroup has its own independent budget.
	resp4 := handleEvictionRequest(handler, "decode-other-0-entry")
	assert.True(t, resp4.Allowed)
}

func TestEvictionHandlerTrackerTTL(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(2),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
					MinAvailable:    intstrPtr(intstr.FromInt(1)),
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createPod("pod-g1", "ms-0", true),
		createPod("pod-g2", "ms-1", true),
	}

	fakeKubeClient := fake.NewSimpleClientset()
	fakeKthenaClient := kthenafake.NewSimpleClientset(ms)

	kubeInformerFactory := informers.NewSharedInformerFactory(fakeKubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	for _, p := range pods {
		podInformer.Informer().GetStore().Add(p)
	}

	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(fakeKthenaClient, 0)
	msInformer := kthenaInformerFactory.Workload().V1alpha1().ModelServings()
	msInformer.Informer().GetStore().Add(ms)

	handler := NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister(), time.Millisecond)

	resp1 := handleEvictionRequest(handler, "pod-g1")
	assert.True(t, resp1.Allowed)

	resp2 := handleEvictionRequest(handler, "pod-g2")
	assert.False(t, resp2.Allowed)

	time.Sleep(2 * time.Millisecond)

	resp3 := handleEvictionRequest(handler, "pod-g2")
	assert.True(t, resp3.Allowed)
}

func TestEvictionHandlerAllowsZeroReplicas(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(0),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelServingGroup,
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createPod("pod-g1", "ms-0", true),
	}

	handler := newTestEvictionHandler(ms, pods)

	resp := handleEvictionRequest(handler, "pod-g1")
	assert.True(t, resp.Allowed)
}

func TestEvictionHandlerAllowsZeroRoleReplicas(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: int32Ptr(1),
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				EvictionStrategy: &workloadv1alpha1.EvictionStrategySpec{
					ProtectionLevel: workloadv1alpha1.ProtectionLevelRole,
					RoleMinAvailable: map[string]intstr.IntOrString{
						"decode": intstr.FromInt(1),
					},
				},
			},
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "decode",
						Replicas: int32Ptr(0),
					},
				},
			},
		},
	}
	pods := []*corev1.Pod{
		createRolePod("decode-0-entry", "ms-0", "decode", "decode-0", true),
	}

	handler := newTestEvictionHandler(ms, pods)

	resp := handleEvictionRequest(handler, "decode-0-entry")
	assert.True(t, resp.Allowed)
}

func newTestEvictionHandler(ms *workloadv1alpha1.ModelServing, pods []*corev1.Pod) *EvictionHandler {
	fakeKubeClient := fake.NewSimpleClientset()
	fakeKthenaClient := kthenafake.NewSimpleClientset(ms)

	kubeInformerFactory := informers.NewSharedInformerFactory(fakeKubeClient, 0)
	podInformer := kubeInformerFactory.Core().V1().Pods()
	for _, p := range pods {
		podInformer.Informer().GetStore().Add(p)
	}

	kthenaInformerFactory := kthenainformers.NewSharedInformerFactory(fakeKthenaClient, 0)
	msInformer := kthenaInformerFactory.Workload().V1alpha1().ModelServings()
	msInformer.Informer().GetStore().Add(ms)

	return NewEvictionHandler(fakeKubeClient, fakeKthenaClient, podInformer.Lister(), msInformer.Lister())
}

func createPod(name, groupName string, ready bool) *corev1.Pod {
	return createRolePod(name, groupName, "worker", "worker-0", ready)
}

func createRolePod(name, groupName, role, roleID string, ready bool) *corev1.Pod {
	return createRolePodWithUID(name, groupName, role, roleID, "", ready)
}

func createRolePodWithUID(name, groupName, role, roleID, uid string, ready bool) *corev1.Pod {
	status := corev1.ConditionFalse
	if ready {
		status = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(uid),
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-ms",
				workloadv1alpha1.GroupNameLabelKey:        groupName,
				workloadv1alpha1.RoleLabelKey:             role,
				workloadv1alpha1.RoleIDKey:                roleID,
			},
		},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{
					Type:   corev1.PodReady,
					Status: status,
				},
			},
		},
	}
}

func handleEvictionRequest(handler *EvictionHandler, podName string) *admissionv1.AdmissionResponse {
	ar := &admissionv1.AdmissionReview{
		Request: &admissionv1.AdmissionRequest{
			UID: "test-uid",
			Resource: metav1.GroupVersionResource{
				Group:    "",
				Version:  "v1",
				Resource: "pods",
			},
			SubResource: "eviction",
			Name:        podName,
			Namespace:   "default",
			Operation:   admissionv1.Create,
		},
	}
	body, _ := json.Marshal(ar)
	req := httptest.NewRequest(http.MethodPost, "/validate-eviction", bytes.NewBuffer(body))
	w := httptest.NewRecorder()

	handler.Handle(w, req)

	var resp admissionv1.AdmissionReview
	json.Unmarshal(w.Body.Bytes(), &resp)
	return resp.Response
}

func clearTracker(t *testing.T, kubeClient *fake.Clientset, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	err := kubeClient.CoreV1().ConfigMaps(ms.Namespace).Delete(context.Background(), trackerConfigMapName(ms.Name), metav1.DeleteOptions{})
	if err != nil {
		t.Logf("tracker ConfigMap was not deleted: %v", err)
	}
}

func intstrPtr(value intstr.IntOrString) *intstr.IntOrString {
	return &value
}
