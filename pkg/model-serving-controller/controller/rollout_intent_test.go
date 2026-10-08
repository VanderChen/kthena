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
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
	"testing"
)

func TestRolloutIntentRejectsStaleGeneration(t *testing.T) {
	old := lifecycleMS("stale-spec", 1, 0, 1)
	rolling := old.DeepCopy()
	rolling.ResourceVersion = "1"
	rolling.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, rolling, old)
	pod := lifecyclePod(t, c, rolling, old, 0, "healthy-old")
	lifecyclePod(t, c, rolling, rolling, 1, "healthy-surge")
	latest := rolling.DeepCopy()
	latest.Spec.Replicas = ptr.To[int32](3)
	latest.Generation++
	// Keep the informer stale to prove the action uses the current API generation.
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(latest.Namespace).Update(context.Background(), latest, metav1.UpdateOptions{})
	require.NoError(t, err)
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.ErrorIs(t, c.manageRollingUpdate(context.Background(), rolling, utils.ModelServingRevision(rolling), nil), errStaleRolloutIntent)
	require.False(t, lifecycleDeletionMatches(c, pod))
	t.Log("latest N=3 U=0 Ready=2, stale reconcile N=1 is rejected before deletion")
}

func TestRolloutIntentWaitsForShrinkCompletion(t *testing.T) {
	old := lifecycleMS("shrink-first", 5, 1, 0)
	ms := old.DeepCopy()
	ms.Spec.Replicas = ptr.To[int32](3)
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, ms, old)
	var pods []*corev1.Pod
	for i := 0; i < 5; i++ {
		pods = append(pods, lifecyclePod(t, c, ms, old, i, fmt.Sprintf("pod-%d", i)))
	}
	for _, i := range []int{3, 4} {
		terminating := pods[i].DeepCopy()
		now := metav1.Now()
		terminating.DeletionTimestamp = &now
		require.NoError(t, c.podsInformer.GetIndexer().Update(terminating))
		require.NoError(t, c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), fmt.Sprintf("shrink-first-%d", i), datastore.ServingGroupDeleting))
	}
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.NoError(t, c.manageRollingUpdate(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	require.False(t, lifecycleDeletionMatches(c, pods[2]))
	t.Log("C=5 N=3 U=1 I=2 Q=1 B=1: shrink sg-3/4 still physically exist; phase barrier prevents sg-2 deletion")
}

func TestRolloutIntentChangeBeforeFirstDeleteAbandonsPlan(t *testing.T) {
	for _, groupScope := range []bool{false, true} {
		t.Run(fmt.Sprintf("group=%t", groupScope), func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("cancel-plan", 1, 1, 0)
			ms.ResourceVersion = "1"
			c := lifecycleController(t, ms)
			pod := lifecyclePod(t, c, ms, ms, 0, "retained-uid")
			client := c.kubeClientSet.(*kubefake.Clientset)
			scope := deleteRoleScope
			if groupScope {
				scope = deleteGroupScope
			}
			plan, err := c.preparePodDeletionPlan(ctx, ms, labels.SelectorFromSet(pod.Labels), scope)
			require.NoError(t, err)
			latest := ms.DeepCopy()
			latest.Generation++
			latest.Spec.Replicas = ptr.To[int32](3)
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
			require.NoError(t, err)
			client.ClearActions()
			err = c.deletePlannedPods(ctx, ms, plan)
			require.ErrorIs(t, err, errStaleRolloutIntent)
			require.False(t, lifecycleDeletionMatches(c, pod))
			require.False(t, c.podDeletionPlanActive(ms, ms.Name+"-0"))
		})
	}
}

func TestRolloutIntentWaitsForRoleShrinkCompletion(t *testing.T) {
	old := lifecycleMS("role-shrink", 1, 1, 0)
	old.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
	old.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
	old.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, ms, old)
	group := ms.Name + "-0"
	key := utils.GetNamespaceName(ms)
	var pods []*corev1.Pod
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("prefill-%d", i)
		pod := utils.GenerateEntryPod(*old.Spec.Template.Roles[0].DeepCopy(), ms, group, id, utils.ModelServingRevision(old), utils.CalRoleTemplateHash(old.Spec.Template.Roles[0]))
		pod.UID = types.UID(id)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if i >= 3 {
			now := metav1.Now()
			pod.DeletionTimestamp = &now
		}
		_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
		c.store.AddRole(key, group, "prefill", id, utils.ModelServingRevision(old), utils.CalRoleTemplateHash(old.Spec.Template.Roles[0]))
		state := datastore.RoleRunning
		if i >= 3 {
			state = datastore.RoleDeleting
		}
		require.NoError(t, c.store.UpdateRoleStatus(key, group, "prefill", id, state))
		pods = append(pods, pod)
	}
	c.kubeClientSet.(*kubefake.Clientset).ClearActions()
	require.NoError(t, c.manageRollingUpdate(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	for _, pod := range pods[:3] {
		require.False(t, lifecycleDeletionMatches(c, pod))
	}
}
