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
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
)

// Focused lifecycle tests consume the business path synchronously. Queue and
// informer tests exercise the public asynchronous callbacks separately.
func (c *ModelServingController) syncPodDeletion(obj interface{}) {
	pod := deletedPodObject(obj)
	if pod == nil {
		return
	}
	if key, ok := modelServingKeyFromChildResource(pod); ok {
		defer c.lifecycle.lock(key)()
	}
	c.forgetPodEpisode(pod)
	ms, _, _, _ := c.getModelServingAndResourceDetails(pod)
	if ms == nil {
		return
	}
	if _, err := c.processPodDeletionLocked(context.Background(), ms, &deletedPodInput{pod: pod}); err != nil {
		klog.ErrorS(err, "Handle deleted Pod", "pod", klog.KObj(pod))
		c.enqueueModelServingAfter(ms, enqueueAfter)
	}
}

func TestPodInputsPublishWhileLifecycleIsBusy(t *testing.T) {
	ms, pods := recoveryFixture(api.RoleRecreate, 19)
	c, kube := recoveryController(t, ms, pods...)
	drainWorkqueue(t, c.workqueue)
	kube.ClearActions()
	c.enqueueModelServing(ms)
	key, shutdown := c.workqueue.Get()
	require.False(t, shutdown)
	unlock := c.lifecycle.lock(ms.Namespace + "/" + ms.Name)
	defer unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 6000; i++ {
			c.updatePod(nil, pods[i%len(pods)])
		}
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pod callbacks waited for the worker lifecycle lock")
	}
	require.Empty(t, kube.Actions(), "callback publication cannot issue API calls")
	c.inputs.mu.Lock()
	dirty := len(c.inputs.owners[ownerKey(ms)].dirty)
	c.inputs.mu.Unlock()
	require.Equal(t, len(pods), dirty, "UPDATE memory follows Pod identities, not event count")
	require.Zero(t, c.workqueue.Len(), "a processing key is dirtied, not queued concurrently")
	c.workqueue.Done(key)
	require.Equal(t, 1, c.workqueue.Len(), "Done makes the dirty key eligible for one follow-up")
}

func TestPodInputsConsumeOtherRoleDespiteDeletionRetryFailure(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	ctx := context.Background()
	selector := labels.Set{api.GroupNameLabelKey: ms.Name + "-0", api.RoleLabelKey: "prefill", api.RoleIDKey: "prefill-0"}.AsSelector()
	_, err := c.preparePodDeletionPlan(ctx, ms, selector, deleteRoleScope)
	require.NoError(t, err)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("patch", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		if strings.Contains(a.(kubetesting.PatchAction).GetName(), "-prefill-") {
			return true, nil, apierrors.NewServiceUnavailable("Role A cannot persist deletion yet")
		}
		return false, nil, nil
	})
	memberTestRemove(t, c, pods[3])
	c.deletePod(pods[3])
	err = c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name)
	require.Error(t, err, "the old Role A failure remains visible")
	require.True(t, lifecycleDeletionMatches(c, pods[2]), "Role B DELETE must reach recovery despite Role A failure")
}

func TestPodInputsPolicyChangeRejectsUnstartedRecovery(t *testing.T) {
	for _, changeAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+changeAt)), func(t *testing.T) {
			c, ms, pods := memberTestFixture(t, api.RoleRecreate)
			memberTestRemove(t, c, pods[1])
			latest := ms.DeepCopy()
			latest.Generation++
			latest.Spec.RecoveryPolicy = api.NoneRestartPolicy
			reads := 0
			// Change intent either before observation installation or at the
			// ordinary deletion guard immediately after installation.
			c.modelServingClient.(*kthenafake.Clientset).PrependReactor("get", "modelservings", func(kubetesting.Action) (bool, runtime.Object, error) {
				reads++
				if reads >= changeAt {
					return true, latest.DeepCopy(), nil
				}
				return true, ms.DeepCopy(), nil
			})
			c.deletePod(pods[1])
			err := c.processPodInputs(context.Background(), ms)
			require.ErrorIs(t, err, errStaleRolloutIntent)
			plans := 0
			c.deletionPlans.Range(func(_, _ interface{}) bool { plans++; return true })
			require.Zero(t, plans, "a rejected old policy cannot survive as an accepted plan")
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
			require.NoError(t, c.processPodInputs(context.Background(), latest))
			require.NoError(t, c.resumePodDeletionPlans(context.Background(), latest))
			require.False(t, lifecycleDeletionMatches(c, pods[0]), "None must preserve the healthy sibling")
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pods[0].Name, metav1.GetOptions{})
			require.NoError(t, err)
		})
	}
}

func TestPodInputsIntentReadErrorDoesNotAuthorizeLaterRecovery(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	memberTestRemove(t, c, pods[1])
	latest := ms.DeepCopy()
	latest.Generation++
	latest.Spec.RecoveryPolicy = api.NoneRestartPolicy
	reads := 0
	c.modelServingClient.(*kthenafake.Clientset).PrependReactor("get", "modelservings", func(kubetesting.Action) (bool, runtime.Object, error) {
		reads++
		if reads == 1 {
			return true, nil, apierrors.NewServiceUnavailable("intent read unavailable")
		}
		return true, latest.DeepCopy(), nil
	})
	c.deletePod(pods[1])
	require.Error(t, c.processPodInputs(context.Background(), ms))
	plans := 0
	c.deletionPlans.Range(func(_, _ interface{}) bool { plans++; return true })
	require.Zero(t, plans)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
	require.NoError(t, c.processPodInputs(context.Background(), latest))
	require.NoError(t, c.resumePodDeletionPlans(context.Background(), latest))
	require.False(t, lifecycleDeletionMatches(c, pods[0]))
}
