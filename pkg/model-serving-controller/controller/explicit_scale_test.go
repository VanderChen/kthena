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
	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"testing"
)

func TestExplicitShrinkPreservesUsefulSurgeAndDoesNotRepeatDeletion(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("formal", 3, 0, 1)
	ms := old.DeepCopy()
	ms.Spec.Replicas = ptr.To[int32](2)
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, ms, old)
	var pods []*corev1.Pod
	for i := 0; i < 4; i++ {
		source := old
		if i == 3 {
			source = ms
		}
		pod := lifecyclePod(t, c, ms, source, i, fmt.Sprintf("uid-%d", i))
		if i == 3 {
			setSurgeScope(pod, surgeServingGroup)
			require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
		}
		pods = append(pods, pod)
	}
	require.NoError(t, c.syncServingGroupReplicas(ctx, ms, utils.ModelServingRevision(ms)))
	require.True(t, lifecycleDeletionMatches(c, pods[2]), "formal count 3 -> 2 selects highest formal replica")
	require.False(t, lifecycleDeletionMatches(c, pods[3]), "Ready target surge must continue providing capacity")
	require.NoError(t, c.syncServingGroupReplicas(ctx, ms, utils.ModelServingRevision(ms)))
	require.False(t, lifecycleDeletionMatches(c, pods[1]), "pending selected deletion already accounts for shrink")
}

func TestExplicitRoleShrinkIgnoresTemplatePartition(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("role-shrink", 1, 1, 0)
	ms.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
	role := ms.Spec.Template.Roles[0].DeepCopy()
	role.Replicas = ptr.To[int32](2)
	role.Partition = ptr.To(intstr.FromInt(1))
	ms.Spec.Template.Roles[0] = *role
	c := lifecycleController(t, ms)
	var pods []*corev1.Pod
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("prefill-%d", i)
		pod := utils.GenerateEntryPod(*role, ms, "role-shrink-0", id, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(*role))
		pod.UID = types.UID(id)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if i == 0 {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		c.store.AddRole(utils.GetNamespaceName(ms), "role-shrink-0", "prefill", id, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(*role))
		pods = append(pods, pod)
	}
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), "role-shrink-0", "prefill")
	require.NoError(t, err)
	require.NoError(t, c.scaleDownRoles(ctx, ms, "role-shrink-0", *role, roles, 2))
	require.True(t, lifecycleDeletionMatches(c, pods[0]))
	require.False(t, lifecycleDeletionMatches(c, pods[2]))
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "role-shrink-0", "prefill", "prefill-0"))
}
