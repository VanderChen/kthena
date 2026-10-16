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
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

// The informer cache may advance ahead of queued deletion callbacks.
func TestDeletionScopeRoleShrinkCallbacksPreserveRetainedUIDs(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		workers               int32
		cleanupBeforeCallback bool
	}{
		{"single-pod-callback-first-control", 0, false},
		{"single-pod-reconcile-first", 0, true},
		{"entry-worker-callbacks-after-cache-removal", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("shrink080", 1, 1, 0)
			ms.ResourceVersion = "1"
			ms.Spec.RecoveryPolicy = api.ServingGroupRecreate
			role := &ms.Spec.Template.Roles[0]
			role.WorkerReplicas = tc.workers
			if tc.workers > 0 {
				role.WorkerTemplate = role.EntryTemplate.DeepCopy()
			}
			c := lifecycleController(t, ms)
			key := utils.GetNamespaceName(ms)
			group := ms.Name + "-0"
			revision, hash := utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(*role)
			var retained, selected []*corev1.Pod
			for instance := 0; instance < 2; instance++ {
				id := fmt.Sprintf("prefill-%d", instance)
				entry := utils.GenerateEntryPod(*role, ms, group, id, revision, hash)
				for ordinal := int32(0); ordinal <= tc.workers; ordinal++ {
					pod := entry.DeepCopy()
					pod.Name = utils.GeneratePodName(group, id, int(ordinal))
					if ordinal > 0 {
						pod.Labels[api.EntryLabelKey] = "false"
					}
					pod.UID = types.UID(pod.Name + "-uid")
					pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
					_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
					c.store.AddRunningPodToServingGroup(key, group, pod.Name, revision, hash, role.Name, id)
					if instance == 0 {
						retained = append(retained, pod)
					} else {
						selected = append(selected, pod)
					}
				}
				require.NoError(t, c.store.UpdateRoleStatus(key, group, role.Name, id, datastore.RoleRunning))
			}
			require.NoError(t, c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupRunning))
			// Deliver the actual API object at DELETE time, including any historical
			// controller-issued annotation, rather than the original creation frame.
			deleteEvents := map[string]*corev1.Pod{}
			kube := c.kubeClientSet.(*kubefake.Clientset)
			kube.PrependReactor("delete", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				name := action.(kubetesting.DeleteAction).GetName()
				object, err := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), ms.Namespace, name)
				if err == nil {
					deleteEvents[name] = object.(*corev1.Pod).DeepCopy()
				}
				return false, nil, nil
			})
			roles, err := c.store.GetRoleList(key, group, role.Name)
			require.NoError(t, err)
			require.NoError(t, c.scaleDownRoles(ctx, ms, group, *role, roles, 1))
			for _, pod := range selected {
				require.True(t, lifecycleDeletionMatches(c, pod), "only surplus instance selected")
				require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
			}
			if tc.cleanupBeforeCallback {
				require.True(t, c.reconcileDeletingRole(ctx, ms, group, role.Name, "prefill-1"))
			}
			for _, pod := range selected {
				event := deleteEvents[pod.Name]
				require.NotNil(t, event)
				c.deletePod(event)
				t.Logf("after callback %s: removed role=%s group=%s", pod.Name,
					c.store.GetRoleStatus(key, group, role.Name, "prefill-1"), c.store.GetServingGroupStatus(key, group))
			}
			for _, pod := range retained {
				require.False(t, lifecycleDeletionMatches(c, pod), "pure role replica shrink must preserve retained Pod UID")
			}
		})
	}
}

func TestDeletionScopeRestartPreservesUnmarkedReplacementUID(t *testing.T) {
	for _, scope := range []string{deleteRoleScope, deleteGroupScope} {
		t.Run(scope, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("replacement080", 1, 1, 0)
			ms.ResourceVersion = "1"
			c := lifecycleController(t, ms)
			entry := lifecyclePod(t, c, ms, ms, 0, "old-entry")
			worker := entry.DeepCopy()
			worker.Name = utils.GeneratePodName(ms.Name+"-0", "prefill-0", 1)
			worker.UID = "original-worker"
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, worker, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(worker))
			plan := &podDeletionPlan{Key: deletionPlanKey(ms, scope, ms.Name+"-0", "prefill", "prefill-0"), Pods: []corev1.Pod{*entry, *worker}}
			require.NoError(t, c.markPodDeletionPlan(ctx, ms, plan))
			kube := c.kubeClientSet.(*kubefake.Clientset)
			replacement := entry.DeepCopy()
			replacement.UID = "new-entry"
			require.Empty(t, replacement.Annotations[deletionScopeAnnotation])
			require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
			require.NoError(t, c.podsInformer.GetIndexer().Update(replacement))
			c.deletionPlans = sync.Map{}
			c.store = datastore.New()
			kube.ClearActions()
			require.NoError(t, c.restoreDeletionIntents(ctx, ms))
			restored, exists := c.deletionPlans.Load(plan.Key)
			require.True(t, exists)
			require.Len(t, restored.(*podDeletionPlan).Pods, 1)
			require.Equal(t, worker.UID, restored.(*podDeletionPlan).Pods[0].UID)
			require.NoError(t, c.resumePodDeletionPlans(ctx, ms))
			got, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, replacement.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, replacement.UID, got.UID)
			require.Empty(t, got.Annotations[deletionScopeAnnotation])
			require.False(t, lifecycleDeletionMatches(c, replacement))
			require.True(t, lifecycleDeletionMatches(c, worker))
		})
	}
}

func TestDeletionScopeMarkFailureSendsNoDelete(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("mark-failure080", 1, 1, 0)
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "original")
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("patch", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("injected marker rejection"))
	})
	plan := &podDeletionPlan{Key: deletionPlanKey(ms, deleteRoleScope, ms.Name+"-0", "prefill", "prefill-0"), Pods: []corev1.Pod{*pod}}
	require.Error(t, c.deletePlannedPods(ctx, ms, plan))
	require.False(t, lifecycleDeletionMatches(c, pod))
}

func TestDeletionScopeGenuineLossRecreatesGroup(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("fault080", 1, 1, 0)
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.ServingGroupRecreate
	decode := *ms.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)
	c := lifecycleController(t, ms)
	lost := lifecyclePod(t, c, ms, ms, 0, "lost-prefill")
	group := ms.Name + "-0"
	peer := utils.GenerateEntryPod(decode, ms, group, "decode-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(decode))
	peer.UID = "healthy-decode"
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, peer, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(peer))
	c.store.AddRole(utils.GetNamespaceName(ms), group, decode.Name, "decode-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(decode))
	// This loss has no controller deletion plan and the current Role is tracked.
	require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, lost.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(lost))
	c.deletePod(lost)
	require.True(t, lifecycleDeletionMatches(c, peer), "genuine loss must retain the configured whole-group scope")
}
