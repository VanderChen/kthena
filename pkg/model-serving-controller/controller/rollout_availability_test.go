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
	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"testing"
)

func TestAvailabilityReadyPlaceholderCannotSpendOldBadBudget(t *testing.T) {
	old := lifecycleMS("ready-cache", 3, 1, 0)
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
	c := lifecycleController(t, ms, old)
	var pods []*corev1.Pod
	for i := 0; i < 3; i++ {
		pods = append(pods, lifecyclePod(t, c, ms, old, i, fmt.Sprintf("pod-%d", i)))
	}
	pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
	require.NoError(t, c.podsInformer.GetIndexer().Update(pods[0]))
	key := utils.GetNamespaceName(ms)
	for _, i := range []int{0, 1} {
		group := fmt.Sprintf("ready-cache-%d", i)
		require.NoError(t, c.store.UpdateRoleStatus(key, group, "prefill", "prefill-0", datastore.RoleCreating))
		require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupCreating))
	}
	require.NoError(t, c.refreshRolloutAvailability(context.Background(), ms))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, "ready-cache-1"))
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.NoError(t, c.manageRollingUpdate(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	require.False(t, lifecycleDeletionMatches(c, pods[1]))
	require.False(t, lifecycleDeletionMatches(c, pods[2]))
	t.Log("physicalReady=2 minimum=2; healthy sg-1 is classified Ready and neither healthy candidate is deleted")
}

func TestAvailabilityScaleDownUsesPhysicalReadyBeforeCost(t *testing.T) {
	ms := lifecycleMS("scale-score", 4, 1, 0)
	c := lifecycleController(t, ms)
	var pods []*corev1.Pod
	for i := 0; i < 4; i++ {
		pod := lifecyclePod(t, c, ms, ms, i, fmt.Sprintf("pod-%d", i))
		cost := "-100"
		if i == 0 || i == 3 {
			cost = "100"
		}
		pod.Annotations = map[string]string{corev1.PodDeletionCost: cost}
		require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
		pods = append(pods, pod)
	}
	key := utils.GetNamespaceName(ms)
	require.NoError(t, c.store.UpdateServingGroupStatus(key, "scale-score-3", datastore.ServingGroupCreating))
	require.NoError(t, c.refreshRolloutAvailability(context.Background(), ms))
	groups, err := c.store.GetServingGroupByModelServing(key)
	require.NoError(t, err)
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.NoError(t, c.scaleDownServingGroups(context.Background(), ms, groups, 2))
	require.False(t, lifecycleDeletionMatches(c, pods[3]))
	require.True(t, lifecycleDeletionMatches(c, pods[1]))
	require.True(t, lifecycleDeletionMatches(c, pods[2]))
	t.Log("all physical Pods Ready; sg-3 cost=100 is preserved after its Creating cache is refreshed")
}

func TestAvailabilityRoleReadyPlaceholderCannotSpendOldBadBudget(t *testing.T) {
	old := lifecycleMS("role-ready-cache", 1, 1, 0)
	old.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
	old.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	old.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
	old.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(1))
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, ms, old)
	key, group := utils.GetNamespaceName(ms), ms.Name+"-0"
	var pods []*corev1.Pod
	for i := 0; i < 3; i++ {
		roleID := fmt.Sprintf("prefill-%d", i)
		pod := utils.GenerateEntryPod(*old.Spec.Template.Roles[0].DeepCopy(), ms, group, roleID, utils.ModelServingRevision(old), utils.CalRoleTemplateHash(old.Spec.Template.Roles[0]))
		pod.UID = types.UID(roleID)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if i == 0 {
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		c.store.AddRole(key, group, "prefill", roleID, utils.ModelServingRevision(old), utils.CalRoleTemplateHash(old.Spec.Template.Roles[0]))
		pods = append(pods, pod)
	}
	require.NoError(t, c.refreshRolloutAvailability(context.Background(), ms))
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(key, group, "prefill", "prefill-1"))
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.NoError(t, c.manageRollingUpdate(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	for _, pod := range pods {
		require.False(t, lifecycleDeletionMatches(c, pod))
	}
}
