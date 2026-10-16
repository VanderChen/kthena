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
	"time"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
)

// Model an accepted DELETE held by a finalizer. Keep API and informer state in
// agreement so this probe does not depend on the known late-callback race.
func holdPlannedPodDeletion(t *testing.T, c *ModelServingController, name string) {
	t.Helper()
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		if a.(kubetesting.DeleteAction).GetName() != name {
			return false, nil, nil
		}
		resource := corev1.SchemeGroupVersion.WithResource("pods")
		obj, err := kube.Tracker().Get(resource, a.GetNamespace(), name)
		require.NoError(t, err)
		pod := obj.(*corev1.Pod).DeepCopy()
		now := metav1.Now()
		pod.DeletionTimestamp = &now
		pod.Finalizers = []string{"review.example/hold"}
		require.NoError(t, kube.Tracker().Update(resource, pod, pod.Namespace))
		require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
		return true, nil, nil
	})
}

func TestPodDeletionPlan_UnrelatedRoleRecovery(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("review-scope", 1, 1, 0)
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = api.RoleRecreate
	decode := *ms.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)
	c := lifecycleController(t, ms)
	prefill := lifecyclePod(t, c, ms, ms, 0, "prefill-original")
	group := ms.Name + "-0"
	revision := utils.ModelServingRevision(ms)
	failed := utils.GenerateEntryPod(decode, ms, group, "decode-0", revision, utils.CalRoleTemplateHash(decode))
	failed.UID = "decode-original"
	failed.Status.Phase = corev1.PodFailed
	kube := c.kubeClientSet.(*kubefake.Clientset)
	_, err := kube.CoreV1().Pods(ms.Namespace).Create(ctx, failed, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(failed))
	c.store.AddServingGroupAndRole(utils.GetNamespaceName(ms), group, revision, utils.CalRoleTemplateHash(decode), "decode", "decode-0")
	holdPlannedPodDeletion(t, c, prefill.Name)
	require.NoError(t, c.DeleteRole(ctx, ms, group, "prefill", "prefill-0"))
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, "prefill", "prefill-0"))
	started := time.Now().Add(-time.Hour)
	c.graceMap.Store(getPodGracePeriodKey(failed), started)
	kube.ClearActions()
	for i := 0; i < 3; i++ {
		delay, err := c.recoverPodAfterGrace(ctx, ms, failed, started)
		require.NoError(t, err)
		t.Logf("attempt=%d retry=%s issuedDecodeDelete=%t", i+1, delay, lifecycleDeletionMatches(c, failed))
	}
	require.True(t, lifecycleDeletionMatches(c, failed), "a pending prefill Role deletion must not indefinitely suppress failed decode Role recovery")
}

func TestPodDeletionPlan_ScopeFencesRecoveryAndConflictCleanup(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		for _, scope := range []string{"same-instance", "other-instance", "other-role", "other-group", "whole-group", "other-owner"} {
			t.Run(fmt.Sprintf("%s/conflict=%t", scope, conflict), func(t *testing.T) {
				ctx := context.Background()
				ms := lifecycleMS("plan-scope", 1, 1, 0)
				ms.ResourceVersion = "1"
				c := lifecycleController(t, ms)
				pod := lifecyclePod(t, c, ms, ms, 0, "current-pod")
				pod.Status.Phase = corev1.PodFailed
				pod.Status.Conditions = nil
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				key := deletionPlanKey(ms, deleteRoleScope, ms.Name+"-0", "prefill", "prefill-0")
				switch scope {
				case "other-instance":
					key.Instance = "prefill-1"
				case "other-role":
					key.Role, key.Instance = "decode", "decode-0"
				case "other-group":
					key.Group = ms.Name + "-1"
				case "whole-group":
					key = deletionPlanKey(ms, deleteGroupScope, ms.Name+"-0", "", "")
				case "other-owner":
					key.UID = "old-owner"
				}
				// The current Pod is not an original UID: an overlapping plan must
				// still prevent a second path from absorbing this replacement.
				old := pod.DeepCopy()
				old.UID = "old-pod"
				c.deletionPlans.Store(key, &podDeletionPlan{Key: key, Pods: []corev1.Pod{*old}})
				blocked := scope == "same-instance" || scope == "whole-group"
				c.kubeClientSet.(*kubefake.Clientset).ClearActions()
				if conflict {
					err = c.deleteConflictingPod(ctx, ms, pod)
					require.Equal(t, blocked, err != nil)
				} else {
					started := time.Now().Add(-time.Hour)
					c.graceMap.Store(getPodGracePeriodKey(pod), started)
					delay, err := c.recoverPodAfterGrace(ctx, ms, pod, started)
					require.NoError(t, err)
					require.Equal(t, blocked, delay > 0)
				}
				require.Equal(t, !blocked, lifecycleDeletionMatches(c, pod))
			})
		}
	}
}

func TestPodDeletionPlan_OwnerLifecycleReleasesPlans(t *testing.T) {
	for _, path := range []string{"delete", "tombstone", "late-delete", "late-tombstone", "not-found", "new-uid", "terminating"} {
		t.Run(path, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("plan-owner", 1, 1, 0)
			ms.ResourceVersion = "1"
			c := lifecycleController(t, ms)
			pod := lifecyclePod(t, c, ms, ms, 0, "old-pod")
			holdPlannedPodDeletion(t, c, pod.Name)
			require.NoError(t, c.DeleteRole(ctx, ms, ms.Name+"-0", "prefill", "prefill-0"))
			oldKey := deletionPlanKey(ms, deleteRoleScope, ms.Name+"-0", "prefill", "prefill-0")
			_, exists := c.deletionPlans.Load(oldKey)
			require.True(t, exists)
			groupKey := deletionPlanKey(ms, deleteGroupScope, ms.Name+"-1", "", "")
			c.deletionPlans.Store(groupKey, &podDeletionPlan{Key: groupKey, Pods: []corev1.Pod{*pod}})

			preserved := []podDeletionPlanKey{}
			for _, owner := range []types.NamespacedName{{Namespace: ms.Namespace, Name: "other"}, {Namespace: "other", Name: ms.Name}} {
				key := oldKey
				key.Owner = owner
				c.deletionPlans.Store(key, &podDeletionPlan{Key: key, Pods: []corev1.Pod{*pod}})
				preserved = append(preserved, key)
			}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Delete(ms))
			current := ms.DeepCopy()
			switch path {
			case "late-delete", "late-tombstone", "new-uid":
				current.UID = "new-owner"
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
				key := deletionPlanKey(current, deleteRoleScope, ms.Name+"-0", "decode", "decode-0")
				c.deletionPlans.Store(key, &podDeletionPlan{Key: key, Pods: []corev1.Pod{*pod}})
				preserved = append(preserved, key)
			case "terminating":
				now := metav1.Now()
				current.DeletionTimestamp = &now
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(current))
			}

			switch path {
			case "delete", "late-delete":
				c.deleteModelServing(ms)
			case "tombstone", "late-tombstone":
				c.deleteModelServing(cache.DeletedFinalStateUnknown{Key: ms.Namespace + "/" + ms.Name, Obj: ms})
			case "new-uid":
				// Cleanup cannot depend on later reconciliation succeeding.
				c.kubeClientSet.(*kubefake.Clientset).PrependReactor("list", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("injected LIST failure")
				})
				require.Error(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
			default:
				require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))
			}
			for _, key := range []podDeletionPlanKey{oldKey, groupKey} {
				_, exists := c.deletionPlans.Load(key)
				require.False(t, exists, "obsolete owner must release all retained Pod objects")
			}
			for _, key := range preserved {
				_, exists := c.deletionPlans.Load(key)
				require.True(t, exists, "cleanup must preserve other owners and the replacement UID")
			}
			if path == "late-delete" || path == "late-tombstone" {
				require.NotEqual(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(current), ms.Name+"-0", "prefill", "prefill-0"), "late callback must preserve the current owner's store")
			}
		})
	}
}
