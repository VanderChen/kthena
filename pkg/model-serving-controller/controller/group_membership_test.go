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
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
			require.NoError(t, c.ensureGroupMembers(ctx, ms))
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

func TestGroupMemberTargetsSurviveRestartAndKeepZeroCounts(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("persist-members", 2, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	zero := []api.Role{*ms.Spec.Template.Roles[0].DeepCopy()}
	zero[0].Replicas = ptr.To[int32](0)
	require.NoError(t, c.setGroupMembers(ctx, ms, "persist-members-0", utils.ModelServingRevision(ms), zero))
	live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	restarted := lifecycleController(t, live)
	cm, err := c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, groupMembersStateName(ms), metav1.GetOptions{})
	require.NoError(t, err)
	_, err = restarted.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, restarted.ensureGroupMembers(ctx, live))
	projected, err := restarted.rolesForServingGroupReadiness(live, "persist-members-0")
	require.NoError(t, err)
	require.Equal(t, 1, roleReplicas(projected[0]), "saved zero counts preserve identity, not an old scaling target")
	require.True(t, restarted.groupMembersPending(live, "persist-members-0"))
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
	require.NoError(t, c.ensureGroupMembers(ctx, ms))
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

func TestGroupMembersFullSpecUpdateRetainsAppliedCounts(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("replace-owner", 3, 1, 0)
	old.ResourceVersion = "1"
	c := lifecycleController(t, old)
	for i := 0; i < 3; i++ {
		lifecyclePod(t, c, old, old, i, fmt.Sprintf("uid-%d", i))
	}
	require.NoError(t, c.ensureGroupMembers(ctx, old))
	ms := old.DeepCopy()
	ms.Annotations = nil // A valid full PUT need not retain internal metadata.
	ms.Generation++
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, ms, metav1.UpdateOptions{})
	require.NoError(t, err)
	// Simulate a delayed Ready callback: complete physical groups remain healthy.
	require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), "replace-owner-1", datastore.ServingGroupCreating))
	require.NoError(t, c.ensureGroupMembers(ctx, ms))
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	targets, err := groupTargets(ms)
	require.NoError(t, err)
	require.Equal(t, groupMemberTargets{
		"replace-owner-0": {"prefill": 2}, "replace-owner-1": {"prefill": 2}, "replace-owner-2": {"prefill": 2},
	}, targets)
	// A fresh controller reconstructs applied counts independently of owner annotations.
	live, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Empty(t, live.Annotations)
	restarted := lifecycleController(t, live)
	restarted.kubeClientSet = c.kubeClientSet
	for i := 0; i < 3; i++ {
		restarted.store.AddServingGroup(utils.GetNamespaceName(ms), i, utils.ModelServingRevision(old))
	}
	require.NoError(t, restarted.ensureGroupMembers(ctx, live))
	after, err := groupTargets(live)
	require.NoError(t, err)
	require.Equal(t, targets, after)
}

func TestEmptyGroupMembersRestartExpandsAllGroups(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("empty-members", 3, 1, 0)
	old.ResourceVersion = "1"
	old.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	c := lifecycleController(t, old)
	for i := 0; i < 3; i++ {
		require.NoError(t, c.setGroupMembers(ctx, old, fmt.Sprintf("empty-members-%d", i), utils.ModelServingRevision(old), old.Spec.Template.Roles))
	}
	ms := old.DeepCopy()
	ms.Annotations = nil
	ms.Generation++
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
	restarted := lifecycleController(t, ms, old)
	restarted.kubeClientSet = c.kubeClientSet
	require.NoError(t, restarted.ensureGroupMembers(ctx, ms))
	require.NoError(t, restarted.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, restarted.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 3)
	// Completed explicit deletion cannot restore an empty group from saved state.
	require.NoError(t, restarted.forgetGroupMembers(ctx, ms, "empty-members-0"))
	restarted.store.DeleteServingGroup(utils.GetNamespaceName(ms), "empty-members-0")
	require.NoError(t, restarted.recordGroupMemberScale(ctx, ms, "empty-members-0"))
	require.NoError(t, restarted.ensureGroupMembers(ctx, ms))
	require.Equal(t, datastore.ServingGroupNotFound, restarted.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "empty-members-0"))
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
			projected, err := c.withGroupMembersState(ctx, live)
			require.NoError(t, err)
			targets, err := groupTargets(projected)
			require.NoError(t, err)
			for i := 0; i < int(replicas); i++ {
				want := int32(0)
				require.Equal(t, want, targets[fmt.Sprintf("empty-scale-%d", i)]["prefill"])
			}
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
	require.NoError(t, c.ensureGroupMembers(ctx, ms.DeepCopy()))
	// Status must inspect actual counts, even when persisted targets already match the spec.
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
	projected, err := c.withGroupMembersState(ctx, ms)
	require.NoError(t, err)
	targets, err := groupTargets(projected)
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		require.EqualValues(t, 2, targets[fmt.Sprintf("nonempty-scale-%d", i)]["prefill"], "existing groups expand without waiting for the new group")
	}
	require.EqualValues(t, 2, targets["nonempty-scale-4"]["prefill"])
	require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "nonempty-scale-4"))
}

func TestEmptyGroupSurgeSurvivesRestartAndAdoption(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("empty-surge", 2, 0, 1)
	ms.ResourceVersion = "1"
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	c := lifecycleController(t, ms)
	require.NoError(t, c.scaleUpServingGroups(ctx, ms, nil, 3, utils.ModelServingRevision(ms)))
	pending, err := c.hasPendingSurge(ms)
	require.NoError(t, err)
	require.True(t, pending, "an empty temporary group has no Pod to carry its origin")
	live := ms.DeepCopy()
	live.Annotations = nil
	restarted := lifecycleController(t, live)
	restarted.kubeClientSet = c.kubeClientSet
	require.NoError(t, restarted.ensureGroupMembers(ctx, live))
	pending, err = restarted.hasPendingSurge(live)
	require.NoError(t, err)
	require.True(t, pending, "restart must not turn temporary empty capacity into a stable survivor")

	live.Spec.Replicas = ptr.To[int32](3)
	live.Generation++
	_, err = restarted.modelServingClient.WorkloadV1alpha1().ModelServings(live.Namespace).Update(ctx, live, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, restarted.ensureGroupMembers(ctx, live))
	pending, err = restarted.hasPendingSurge(live)
	require.NoError(t, err)
	require.False(t, pending, "expansion adopts the empty group")
	live.Spec.Replicas = ptr.To[int32](2)
	pending, err = restarted.hasPendingSurge(live)
	require.NoError(t, err)
	require.False(t, pending, "a later shrink cannot turn an adopted stable group back into surge")
}

func TestEmptyGroupRestartWaitsForTerminatingMembers(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("empty-terminating", 4, 1, 0)
	old.ResourceVersion = "1"
	c := lifecycleController(t, old)
	for i := 0; i < 4; i++ {
		lifecyclePod(t, c, old, old, i, fmt.Sprintf("old-%d", i))
	}
	require.NoError(t, c.ensureGroupMembers(ctx, old))
	ms := old.DeepCopy()
	ms.Generation++
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, ms, metav1.UpdateOptions{})
	require.NoError(t, err)
	group := "empty-terminating-3"
	require.NoError(t, c.setGroupMembers(ctx, ms, group, utils.ModelServingRevision(old), ms.Spec.Template.Roles))
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, group+"-prefill-0-0", metav1.GetOptions{})
	require.NoError(t, err)
	pod.DeletionTimestamp = ptr.To(metav1.Now())
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
	// Cold-start Pod handling does not recreate Role records from Terminating Pods.
	c.store.DeleteServingGroup(utils.GetNamespaceName(ms), group)
	require.NoError(t, c.ensureGroupMembers(ctx, ms))
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	targets, err := groupTargets(ms)
	require.NoError(t, err)
	for i := 0; i < 4; i++ {
		require.Zero(t, targets[fmt.Sprintf("empty-terminating-%d", i)]["prefill"], "other groups shrink independently of the terminating group")
	}
	ready, err := c.checkServingGroupReady(ms, group)
	require.NoError(t, err)
	require.False(t, ready, "the terminating group still cannot provide Ready credit")
	require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
	ready, err = c.checkServingGroupReady(ms, group)
	require.NoError(t, err)
	require.True(t, ready)
}
