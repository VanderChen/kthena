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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestEnqueueAndSyncWithUID(t *testing.T) {
	// 1. Setup
	kubeClient := kubefake.NewSimpleClientset()
	kthenaClient := kthenafake.NewSimpleClientset()
	volcanoClient := volcanofake.NewSimpleClientset()
	apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

	// Add Reactor to fail Pod creation
	kubeClient.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("induced pod creation error")
	})

	controller, err := NewModelServingController(kubeClient, kthenaClient, volcanoClient, apiextClient)
	assert.NoError(t, err)

	// Override workqueue with one we can inspect easily (though standard rate limiting queue is fine)
	controller.workqueue = workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "ModelServings")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start internal informers
	go controller.modelServingsInformer.Run(ctx.Done())
	go controller.podsInformer.Run(ctx.Done())
	go controller.servicesInformer.Run(ctx.Done())
	
	// Start CrdInformer
	go controller.podGroupManager.CrdInformer.Run(ctx.Done())

	// Wait for sync
	cache.WaitForCacheSync(ctx.Done(), 
		controller.modelServingsInformer.HasSynced,
		controller.podsInformer.HasSynced,
		controller.servicesInformer.HasSynced,
	)

	if !cache.WaitForCacheSync(ctx.Done(), controller.podGroupManager.CrdInformer.HasSynced) {
		t.Fatal("Timed out waiting for CRD informer sync")
	}

	msIndexer := controller.modelServingsInformer.GetIndexer()

	// 2. Scenario objects
	msA := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-ms",
			UID:       types.UID("uid-a"),
		},
	}

	msB := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      "test-ms",
			UID:       types.UID("uid-b"),
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To[int32](1),
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				GangPolicy: &workloadv1alpha1.GangPolicy{
					MinRoleReplicas: map[string]int32{"role-1": 1},
				},
				Roles: []workloadv1alpha1.Role{
					{
						Name:     "role-1",
						Replicas: ptr.To[int32](1),
						EntryTemplate: workloadv1alpha1.PodTemplateSpec{
							Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: "img"}}},
						},
					},
				},
			},
		},
	}

	// 3. Test Enqueue Format
	controller.enqueueModelServing(msA)
	assert.Equal(t, 1, controller.workqueue.Len())
	key, _ := controller.workqueue.Get()
	assert.Equal(t, "default/test-ms/uid-a", key)
	controller.workqueue.Done(key)

	controller.enqueueModelServing(msB)
	assert.Equal(t, 1, controller.workqueue.Len())
	keyB, _ := controller.workqueue.Get()
	assert.Equal(t, "default/test-ms/uid-b", keyB)
	controller.workqueue.Done(keyB)

	// Verify Reactor works
	_, err = kubeClient.CoreV1().Pods("default").Create(ctx, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "test"}}, metav1.CreateOptions{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "induced pod creation error")
	
	// 4. Test Sync Logic (Stale Event)
	// Add msB to lister (valid object)
	err = msIndexer.Add(msB)
	assert.NoError(t, err)

	// Call sync with Key for A (stale)
	// Expectation: Should return nil (ignore), NOT hitting the "induced error".
	err = controller.syncModelServing(ctx, "default/test-ms/uid-a")
	assert.NoError(t, err, "Should return nil for stale event (early exit)")

	// 5. Test Sync Logic (Correct Event)
	// Call sync with Key for B (valid)
	// Expectation: Should hit the "induced error" because it tries to create Pods.
	err = controller.syncModelServing(ctx, "default/test-ms/uid-b")
	assert.Error(t, err, "Should return error for valid event (hit induced error)")
	assert.Contains(t, err.Error(), "induced pod creation error")
}
