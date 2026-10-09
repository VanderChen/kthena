/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permsssions and
limstations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	clientfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestGroupMembersScaleAllGroupsWithoutReadyBarrier(t *testing.T) {
	for _, unavailable := range []int32{0, 1} {
		t.Run(fmt.Sprintf("unavailable=%d", unavailable), func(t *testing.T) {
			ctx := context.Background()
			old := lifecycleMS("members", 3, unavailable, 1)
			ms := old.DeepCopy()
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			c := lifecycleController(t, ms, old)
			for i := 0; i < 3; i++ {
				lifecyclePod(t, c, ms, old, i, fmt.Sprintf("old-%d", i))
			}
			require.Equal(t, utils.ModelServingRevision(old), utils.ModelServingRevision(ms))
			require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
			require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 6, "every group expands before any new Pod becomes Ready")
			for i := 0; i < 3; i++ {
				group := fmt.Sprintf("members-%d", i)
				pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, group+"-prefill-0-0", metav1.GetOptions{})
				require.NoError(t, err)
				require.EqualValues(t, fmt.Sprintf("old-%d", i), pod.UID)
				ready, err := c.checkServingGroupReady(ms, group)
				require.NoError(t, err)
				require.False(t, ready)
			}
			require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
			pods, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 6, "reconcile is idempotent")
		})
	}
}

func TestGroupMembersFaultDoesNotBlockScalingOrCreateSurge(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("member-budget", 3, 1, 1)
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	c := lifecycleController(t, ms, old)
	for i := 0; i < 3; i++ {
		pod := lifecyclePod(t, c, ms, old, i, fmt.Sprintf("uid-%d", i))
		if i == 0 {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
		}
	}
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, c.syncServingGroupReplicas(ctx, ms, utils.ModelServingRevision(ms)))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 6)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.Len(t, groups, 3, "member changes alone do not create a surge group")
}

func TestEmptyGroupExpansionStartsMemberShrink(t *testing.T) {
	for _, replicas := range []int32{5, 7} {
		t.Run(fmt.Sprintf("groups=%d", replicas), func(t *testing.T) {
			ctx := context.Background()
			old := lifecycleMS("empty-scale", 4, 1, 0)
			ms := old.DeepCopy()
			ms.ResourceVersion = "1"
			ms.Generation++
			ms.Spec.Replicas = ptr.To(replicas)
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			ms.Status.CurrentRevision = utils.ModelServingRevision(old)
			ms.Status.UpdateRevision = ms.Status.CurrentRevision
			c := lifecycleController(t, ms, old)
			for i := 0; i < 4; i++ {
				lifecyclePod(t, c, ms, old, i, fmt.Sprintf("old-%d", i))
			}
			require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, pods.Items, "all existing groups receive the shrink independently")
			live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			cms, err := c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, cms.Items)
			require.True(t, meta.IsStatusConditionTrue(live.Status.Conditions, string(api.ModelServingProgressing)))
			require.Equal(t, ms.Status.CurrentRevision, live.Status.CurrentRevision)
			require.Equal(t, ms.Status.CurrentRevision, live.Status.UpdateRevision)
			revisions, err := c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, revisions.Items, 1, "replica changes do not create template revisions")
		})
	}
}

func TestGroupMemberPendingStatusAndContinuation(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("pending-members", 4, 1, 0)
	ms := old.DeepCopy()
	ms.ResourceVersion = "1"
	ms.Generation++
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	ms.Status.CurrentRevision = utils.ModelServingRevision(old)
	ms.Status.UpdateRevision = ms.Status.CurrentRevision
	c := lifecycleController(t, ms, old)
	for i := 0; i < 4; i++ {
		lifecyclePod(t, c, ms, old, i, fmt.Sprintf("old-%d", i))
	}
	// Status must inspect actual counts rather than grant Ready from the desired count.
	require.Empty(t, c.modelServingsInformer.GetIndexer().List()[0].(*api.ModelServing).Annotations)
	require.NoError(t, c.updateModelServingStatus(ctx, ms, utils.ModelServingRevision(ms), nil))
	live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Zero(t, live.Status.AvailableReplicas, "old members do not satisfy the new count")
	require.True(t, meta.IsStatusConditionTrue(live.Status.Conditions, string(api.ModelServingProgressing)))
	require.False(t, meta.IsStatusConditionTrue(live.Status.Conditions, string(api.ModelServingAvailable)))
	require.False(t, meta.IsStatusConditionTrue(live.Status.Conditions, string(api.ModelServingUpdateInProgress)))
	require.Empty(t, live.Annotations, "private member projection is not written onto the owner")
	c.updateModelServing(ms, live) // Status-only events provide no continuation.
	require.Eventually(t, func() bool { return c.workqueue.Len() > 0 }, 3*time.Second, 10*time.Millisecond)
	key, shutdown := c.workqueue.Get()
	require.False(t, shutdown)
	require.Equal(t, utils.GetNamespaceName(ms).String(), key)
	c.workqueue.Done(key)
}

func TestEmptyGroupCreationDoesNotGrantMissingMemberCredit(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("nonempty-scale", 4, 1, 0)
	ms := old.DeepCopy()
	ms.ResourceVersion = "1"
	ms.Spec.Replicas = ptr.To[int32](5)
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	c := lifecycleController(t, ms, old)
	for i := 0; i < 4; i++ {
		lifecyclePod(t, c, ms, old, i, fmt.Sprintf("old-%d", i))
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 10, "all five groups have the latest two instances")
	require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "nonempty-scale-4"))
}

func TestEmptyGroupRestartWaitsForTerminatingMembers(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("empty-terminating", 4, 1, 0)
	old.ResourceVersion = "1"
	c := lifecycleController(t, old)
	for i := 0; i < 4; i++ {
		lifecyclePod(t, c, old, old, i, fmt.Sprintf("old-%d", i))
	}
	ms := old.DeepCopy()
	ms.Generation++
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, ms, metav1.UpdateOptions{})
	require.NoError(t, err)
	group := "empty-terminating-3"
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, group+"-prefill-0-0", metav1.GetOptions{})
	require.NoError(t, err)
	pod.DeletionTimestamp = ptr.To(metav1.Now())
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
	// Cold-start Pod handling does not recreate Role records from Terminating Pods.
	c.store.DeleteServingGroup(utils.GetNamespaceName(ms), group)
	c.store.AddServingGroup(utils.GetNamespaceName(ms), 3, utils.ModelServingRevision(old))
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 1, "other groups shrink independently of the terminating group")
	ready, err := c.checkServingGroupReady(ms, group)
	require.NoError(t, err)
	require.False(t, ready, "the terminating group still cannot provide Ready credit")
	require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
	ready, err = c.checkServingGroupReady(ms, group)
	require.NoError(t, err)
	require.True(t, ready)
}

func TestEmptyGroupsUseCurrentPartitionAfterExpansion(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restarted=%t", restarted), func(t *testing.T) {
			ctx := context.Background()
			old := lifecycleMS("empty-partition", 2, 1, 0)
			old.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			ms := old.DeepCopy()
			ms.ResourceVersion = "1"
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = intstrPtr(2)
			ms.Status.CurrentRevision = utils.ModelServingRevision(old)
			ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
			c := lifecycleController(t, ms, old)
			// Old sparse/temporary empty identities cannot outlive zero members.
			c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, utils.ModelServingRevision(old))
			c.store.AddServingGroup(utils.GetNamespaceName(ms), 3, utils.ModelServingRevision(ms))
			require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.EqualValues(t, 2, live.Status.Replicas)
			require.EqualValues(t, 2, live.Status.AvailableReplicas)
			require.EqualValues(t, 2, live.Status.CurrentReplicas)
			require.Zero(t, live.Status.UpdatedReplicas)
			require.Equal(t, ms.Status.CurrentRevision, live.Status.CurrentRevision)
			if restarted {
				fresh := lifecycleController(t, live, old)
				fresh.kubeClientSet = c.kubeClientSet
				c = fresh
			}
			live.Generation++
			live.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, live, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(live))
			require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2)
			for _, pod := range pods.Items {
				require.Contains(t, []string{"empty-partition-0", "empty-partition-1"}, pod.Labels[api.GroupNameLabelKey])
				require.Equal(t, "test:v1", pod.Spec.Containers[0].Image)
				require.Equal(t, utils.ModelServingRevision(old), utils.ObjectRevision(&pod))
			}
			cms, err := c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			require.Empty(t, cms.Items)
		})
	}
}

func TestProtectedSingleRoleRecoveryKeepsGroupTemplate(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("protected-recovery", 2, 1, 0)
	ms := old.DeepCopy()
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.RoleRecreate
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = intstrPtr(2)
	ms.Status.CurrentRevision = utils.ModelServingRevision(old)
	ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
	c := lifecycleController(t, ms, old)
	survivor := lifecyclePod(t, c, ms, old, 0, "old-v1")
	deleted := lifecyclePod(t, c, ms, ms, 1, "protected-v2")
	require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, deleted.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(deleted))
	c.deletePod(deleted)
	group := "protected-recovery-1"
	revision, exists := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), group)
	require.True(t, exists)
	require.Equal(t, utils.ModelServingRevision(ms), revision)
	roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group, "prefill")
	require.NoError(t, err)
	require.Empty(t, roles, "Role recovery temporarily leaves the SG without members")
	require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
	replacement, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, deleted.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "test:v2", replacement.Spec.Containers[0].Image, "local Role recovery retains the enclosing SG's applied template")
	require.Equal(t, utils.ModelServingRevision(ms), utils.ObjectRevision(replacement))
	retained, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, survivor.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, survivor.UID, retained.UID)
	live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, utils.ModelServingRevision(old), live.Status.CurrentRevision)
}

func TestPartitionIncreasePreservesExistingTargetUID(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("protect-target", 2, 1, 0)
	ms := old.DeepCopy()
	ms.ResourceVersion = "1"
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = intstrPtr(2)
	ms.Status.CurrentRevision = utils.ModelServingRevision(old)
	c := lifecycleController(t, ms, old)
	first := lifecyclePod(t, c, ms, old, 0, "v1-uid")
	second := lifecyclePod(t, c, ms, ms, 1, "v2-uid")
	require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
	for _, pod := range []*corev1.Pod{first, second} {
		actual, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, pod.UID, actual.UID)
		require.Equal(t, pod.Spec.Containers[0].Image, actual.Spec.Containers[0].Image)
		require.False(t, lifecycleDeletionMatches(c, pod))
	}
}

func TestStableStatusDoesNotWriteAgain(t *testing.T) {
	ctx := context.Background()
	for _, empty := range []bool{false, true} {
		t.Run(fmt.Sprintf("empty=%t", empty), func(t *testing.T) {
			ms := lifecycleMS("status-stable", 1, 1, 0)
			ms.ResourceVersion = "1"
			if empty {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			}
			c := lifecycleController(t, ms)
			if empty {
				c.store.AddServingGroup(utils.GetNamespaceName(ms), 0, utils.ModelServingRevision(ms))
			} else {
				lifecyclePod(t, c, ms, ms, 0, "ready")
			}
			require.NoError(t, c.UpdateModelServingStatus(ms, utils.ModelServingRevision(ms)))
			live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(live))
			client := c.modelServingClient.(*clientfake.Clientset)
			client.ClearActions()
			require.NoError(t, c.UpdateModelServingStatus(live, utils.ModelServingRevision(ms)))
			for _, action := range client.Actions() {
				require.False(t, action.Matches("update", "modelservings") && action.GetSubresource() == "status")
			}
		})
	}
}

func TestReconciliationHasNoMemberConfigMapAPIDependency(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("no-private-cm", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	lifecyclePod(t, c, ms, ms, 0, "ready")
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.ClearActions()
	kube.PrependReactor("*", "configmaps", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("ConfigMap API unavailable")
	})
	require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
	live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.EqualValues(t, 1, live.Status.AvailableReplicas)
	for _, action := range kube.Actions() {
		require.NotEqual(t, "configmaps", action.GetResource().Resource)
	}
}
