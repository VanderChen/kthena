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

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestGroupMembersApplyOneCompleteGroupAtATime(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("members", 3, 1, 0)
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	c := lifecycleController(t, ms, old)
	for i := 0; i < 3; i++ {
		lifecyclePod(t, c, ms, old, i, fmt.Sprintf("old-%d", i))
	}
	require.NoError(t, c.ensureGroupMembers(ctx, ms))
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 4)
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, "members-2-prefill-1-0", metav1.GetOptions{})
	require.NoError(t, err, "highest group receives the only new member")
	targets, err := groupTargets(ms)
	require.NoError(t, err)
	require.Equal(t, int32(1), targets["members-0"]["prefill"])
	require.Equal(t, int32(1), targets["members-1"]["prefill"])
	require.Equal(t, int32(2), targets["members-2"]["prefill"])
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "members-0"))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "members-1"))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	pods, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 4, "pending member cannot release the next group's budget")
	pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, "members-2-prefill-1-0", metav1.GetOptions{})
	require.NoError(t, err)
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, c.syncRoleReplicas(ctx, ms, utils.ModelServingRevision(ms), nil))
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, "members-1-prefill-1-0", metav1.GetOptions{})
	require.NoError(t, err, "next group starts after complete readiness")
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
	require.Zero(t, roleReplicas(projected[0]))
	require.True(t, groupMembersPending(live, "persist-members-0"))
}

func TestGroupMembersFaultConsumesBudgetAndSurgeCanReleaseIt(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("member-budget", 3, 1, 1)
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	c := lifecycleController(t, ms, old)
	for i := 0; i < 3; i++ {
		lifecyclePod(t, c, ms, old, i, fmt.Sprintf("uid-%d", i))
	}
	require.NoError(t, c.ensureGroupMembers(ctx, ms))
	require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), "member-budget-0", datastore.ServingGroupCreating))
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.NoError(t, c.applyGroupMemberChanges(ctx, ms, groups))
	targets, err := groupTargets(ms)
	require.NoError(t, err)
	require.Equal(t, int32(1), targets["member-budget-2"]["prefill"], "fault leaves no healthy-group budget")
	lifecyclePod(t, c, ms, old, 3, "surge")
	require.NoError(t, c.setGroupMembers(ctx, ms, "member-budget-3", utils.ModelServingRevision(ms), ms.Spec.Template.Roles))
	groups, err = c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.NoError(t, c.applyGroupMemberChanges(ctx, ms, groups))
	targets, err = groupTargets(ms)
	require.NoError(t, err)
	require.Equal(t, int32(2), targets["member-budget-2"]["prefill"])
	require.Equal(t, int32(1), targets["member-budget-1"]["prefill"])
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
		"replace-owner-0": {"prefill": 1}, "replace-owner-1": {"prefill": 1}, "replace-owner-2": {"prefill": 2},
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

func TestEmptyGroupMembersRestartExpansionUsesOneGroupBudget(t *testing.T) {
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
	require.Len(t, pods.Items, 1)
	require.Equal(t, "empty-members-2-prefill-0-0", pods.Items[0].Name)
	// Completed explicit deletion cannot restore an empty group from saved state.
	require.NoError(t, restarted.forgetGroupMembers(ctx, ms, "empty-members-0"))
	restarted.store.DeleteServingGroup(utils.GetNamespaceName(ms), "empty-members-0")
	require.NoError(t, restarted.ensureGroupMembers(ctx, ms))
	require.Equal(t, datastore.ServingGroupNotFound, restarted.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "empty-members-0"))
}
