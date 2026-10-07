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

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
)

func lifecycleMS(name string, n, u, s int32) *api.ModelServing {
	ms := createStandardModelServing(name, n, 1)
	ms.UID = types.UID(name + "-uid")
	ms.Generation = 1
	ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &api.RollingUpdateConfiguration{
			MaxUnavailable: ptr.To(intstr.FromInt32(u)), MaxSurge: ptr.To(intstr.FromInt32(s)),
		}}
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v1"
	return ms
}

func lifecycleController(t *testing.T, ms *api.ModelServing, histories ...*api.ModelServing) *ModelServingController {
	t.Helper()
	c := newRevisionTestController(t, ms)
	c.recorder = record.NewFakeRecorder(1000)
	c.initialSync.Store(true)
	t.Cleanup(c.workqueue.ShutDown)
	for _, historical := range append(histories, ms) {
		_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, historical, utils.ModelServingRevision(historical), historical.Spec.Template.Roles)
		require.NoError(t, err)
	}
	return c
}

func lifecyclePod(t *testing.T, c *ModelServingController, ms, template *api.ModelServing, group int, uid string) *corev1.Pod {
	t.Helper()
	role := *template.Spec.Template.Roles[0].DeepCopy()
	groupName := utils.GenerateServingGroupName(ms.Name, group)
	hash, revision := utils.CalRoleTemplateHash(role), utils.ModelServingRevision(template)
	pod := utils.GenerateEntryPod(role, ms, groupName, "prefill-0", revision, hash)
	pod.UID = types.UID(uid)
	pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	key := utils.GetNamespaceName(ms)
	c.store.AddRunningPodToServingGroup(key, groupName, pod.Name, revision, hash, "prefill", "prefill-0")
	require.NoError(t, c.store.UpdateRoleStatus(key, groupName, "prefill", "prefill-0", datastore.RoleRunning))
	require.NoError(t, c.store.UpdateServingGroupStatus(key, groupName, datastore.ServingGroupRunning))
	return pod
}

func lifecycleDeletionMatches(c *ModelServingController, pod *corev1.Pod) bool {
	for _, action := range c.kubeClientSet.(*kubefake.Clientset).Actions() {
		if action.Matches("delete", "pods") && action.(kubetesting.DeleteAction).GetName() == pod.Name {
			return true
		}
		if action.Matches("delete-collection", "pods") {
			if action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels.Matches(labels.Set(pod.Labels)) {
				return true
			}
		}
	}
	return false
}

func TestInstanceLifecycle_LateDeleteTargetsHealthyReplacement(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate, api.NoneRestartPolicy} {
		for _, sameRevision := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/same-revision=%t", policy, sameRevision), func(t *testing.T) {
				ms := lifecycleMS("late-delete", 1, 0, 1)
				ms.Spec.RecoveryPolicy = policy
				c := lifecycleController(t, ms)
				current := lifecyclePod(t, c, ms, ms, 0, "new-healthy-uid")
				old := current.DeepCopy()
				old.UID = "old-deleted-uid"
				if !sameRevision {
					old.Labels[api.RevisionLabelKey] = "previous-revision"
				}
				c.kubeClientSet.(*kubefake.Clientset).ClearActions()
				c.deletePod(old)
				matched := lifecycleDeletionMatches(c, current)
				require.False(t, matched)
				count, err := c.store.GetRunningPodNumByServingGroup(utils.GetNamespaceName(ms), "late-delete-0")
				require.NoError(t, err)
				require.Equal(t, 1, count, "old event must preserve the replacement Ready membership")
				t.Logf("oldUID=%s currentUID=%s currentReady=true currentTarget=true U=0 destructiveSelectorMatchesNewUID=%t", old.UID, current.UID, matched)
			})
		}
	}
}

func TestInstanceLifecycle_LateReadyResurrectsOldIdentityThenRollsNewPod(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			oldMS := lifecycleMS("late-ready", 1, 1, 0)
			ms := oldMS.DeepCopy()
			ms.Spec.RolloutStrategy.Type = mode
			oldMS.Spec.RolloutStrategy.Type = mode
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
			ms.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
			c := lifecycleController(t, ms, oldMS)
			current := lifecyclePod(t, c, ms, ms, 0, "new-v2-uid")
			old := current.DeepCopy()
			old.UID = "old-v1-uid"
			old.Labels[api.RevisionLabelKey] = utils.ModelServingRevision(oldMS)
			old.Labels[api.RoleTemplateHashLabelKey] = utils.CalRoleTemplateHash(oldMS.Spec.Template.Roles[0])
			key := utils.GetNamespaceName(ms)
			c.store.DeleteServingGroup(key, "late-ready-0")
			// The informer already contains the replacement, but an old Ready callback is queued.
			require.NoError(t, c.handleReadyPod(ms, "late-ready-0", old))
			c.store.AddServingGroup(key, 0, utils.ModelServingRevision(ms))
			c.store.AddRole(key, "late-ready-0", "prefill", "prefill-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]))
			groups, err := c.store.GetServingGroupByModelServing(key)
			require.NoError(t, err)
			require.Equal(t, utils.ModelServingRevision(ms), groups[0].Revision)
			require.Equal(t, templateEquivalent, c.compareServingGroupTemplate(context.Background(), ms, groups[0], utils.ModelServingRevision(ms)))
			c.kubeClientSet.(*kubefake.Clientset).ClearActions()
			require.NoError(t, c.manageRollingUpdate(context.Background(), ms, utils.ModelServingRevision(ms), nil))
			require.False(t, lifecycleDeletionMatches(c, current))
			t.Logf("liveUID=%s liveVersion=v2 cachedVersion=v2 comparison=Equivalent deletion=false", current.UID)
		})
	}
}

func TestInstanceLifecycle_QueuedReadyRevivesDeletingGroup(t *testing.T) {
	ms := lifecycleMS("revive", 1, 1, 0)
	c := lifecycleController(t, ms)
	oldReady := lifecyclePod(t, c, ms, ms, 0, "terminating-uid")
	terminating := oldReady.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	require.NoError(t, c.podsInformer.GetIndexer().Update(terminating))
	key := utils.GetNamespaceName(ms)
	require.NoError(t, c.store.UpdateServingGroupStatus(key, "revive-0", datastore.ServingGroupDeleting))
	require.NoError(t, c.handleReadyPod(ms, "revive-0", oldReady))
	require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, "revive-0"))
	t.Log("latest Pod terminating; queued Ready event preserves Deleting lifecycle fence")
}

func TestInstanceLifecycleDeletionUIDPrecondition(t *testing.T) {
	ms := lifecycleMS("uid-delete", 1, 1, 0)
	c := lifecycleController(t, ms)
	original := lifecyclePod(t, c, ms, ms, 0, "original-uid")
	kube := c.kubeClientSet.(*kubefake.Clientset)
	selected, err := c.preparePodDeletion(context.Background(), ms, labels.SelectorFromSet(original.Labels), deleteRoleScope)
	require.NoError(t, err)
	require.Len(t, selected, 1)
	marked, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, deleteRoleScope, marked.Annotations[deletionScopeAnnotation])
	replacement := original.DeepCopy()
	replacement.UID = "replacement-uid"
	require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
	kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		action := a.(kubetesting.DeleteAction)
		require.NotNil(t, action.GetDeleteOptions().Preconditions)
		require.Equal(t, original.UID, *action.GetDeleteOptions().Preconditions.UID)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, original.Name, fmt.Errorf("UID changed"))
	})
	require.True(t, apierrors.IsConflict(c.deletePodUIDs(context.Background(), selected)))
	got, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, replacement.UID, got.UID)
}

func TestInstanceLifecycleRestoresDeletionBeforeCreatingMembers(t *testing.T) {
	ms := lifecycleMS("restart-delete", 1, 1, 0)
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "retiring-uid")
	_, err := c.preparePodDeletion(context.Background(), ms, labels.SelectorFromSet(pod.Labels), deleteRoleScope)
	require.NoError(t, err)
	// Restart with an empty store but the original persisted Pod deletion intent.
	c.store = datastore.New()
	require.NoError(t, c.restoreDeletionIntents(context.Background(), ms))
	_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))
	for _, action := range c.kubeClientSet.(*kubefake.Clientset).Actions() {
		if action.Matches("delete", "pods") {
			require.Equal(t, pod.UID, *action.(kubetesting.DeleteAction).GetDeleteOptions().Preconditions.UID)
		}
	}
}

func TestInstanceLifecycleRemovedWorkerCannotRecoverReplacement(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			ms := lifecycleMS("old-worker", 1, 1, 0)
			ms.Spec.RecoveryPolicy = policy
			c := lifecycleController(t, ms)
			current := lifecyclePod(t, c, ms, ms, 0, "new-entry")
			current.Annotations = map[string]string{groupInstanceAnnotation: "same-group", roleInstanceAnnotation: "new-role"}
			require.NoError(t, c.podsInformer.GetIndexer().Update(current))
			oldWorker := current.DeepCopy()
			oldWorker.Name = "old-worker-0-prefill-0-1"
			oldWorker.UID = "retired-worker"
			oldWorker.Annotations[roleInstanceAnnotation] = "old-role"
			c.kubeClientSet.(*kubefake.Clientset).ClearActions()
			c.deletePod(oldWorker)
			require.False(t, lifecycleDeletionMatches(c, current), "changing recovery scope cannot make an old Role incarnation fault a replacement")
		})
	}
}

func TestInstanceLifecycleSnapshotExpiresWhenSlotIsRefilled(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, ready := range []bool{false, true} {
			for _, alreadyExists := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/ready=%t/already-exists=%t", mode, ready, alreadyExists), func(t *testing.T) {
					oldMS := lifecycleMS("refilled", 1, 1, 0)
					oldMS.Spec.RolloutStrategy.Type = mode
					oldMS.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
					ms := oldMS.DeepCopy()
					ms.ResourceVersion = "1"
					ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
					c := lifecycleController(t, ms, oldMS)
					old := lifecyclePod(t, c, ms, oldMS, 0, "old-v1")
					ctx, err := c.withRolloutPodSnapshot(context.Background(), ms)
					require.NoError(t, err)
					// The old Pod disappears after the round's initial List. Scaling
					// can now reuse its ordinal before rolling update in the same round.
					require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}))
					require.NoError(t, c.podsInformer.GetIndexer().Delete(old))
					key, group := utils.GetNamespaceName(ms), "refilled-0"
					c.store.DeleteServingGroup(key, group)
					c.store.AddServingGroup(key, 0, utils.ModelServingRevision(ms))
					kube := c.kubeClientSet.(*kubefake.Clientset)
					kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
						pod := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
						pod.UID = "new-v2"
						if ready {
							pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
						}
						return false, nil, nil
					})
					if alreadyExists {
						role := *ms.Spec.Template.Roles[0].DeepCopy()
						pod := utils.GenerateEntryPod(role, ms, group, "prefill-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
						_, err = kube.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						require.NoError(t, err)
					}
					require.NoError(t, c.CreatePodsForServingGroup(ctx, ms, 0, utils.ModelServingRevision(ms), ms.Spec.Template.Roles))
					current, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, old.Name, metav1.GetOptions{})
					require.NoError(t, err)
					kube.ClearActions()
					require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
					require.NoError(t, c.manageRollingUpdate(ctx, ms, utils.ModelServingRevision(ms), nil))
					require.False(t, lifecycleDeletionMatches(c, current), "a replacement already at the target must not inherit the retired Pod's version")
					observed, err := c.podsForRoleObservation(ctx, ms, group, "prefill", "prefill-0")
					require.NoError(t, err)
					require.Len(t, observed, 1)
					require.Equal(t, current.UID, observed[0].UID)
				})
			}
		}
	}
}

func TestInstanceLifecycleSnapshotReadErrorCannotRestoreDeletedCapacity(t *testing.T) {
	ms := lifecycleMS("snapshot-error", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "retired")
	ctx, err := c.withRolloutPodSnapshot(context.Background(), ms)
	require.NoError(t, err)
	require.NoError(t, c.deletePodUIDs(ctx, []corev1.Pod{*pod}))
	failed := false
	c.kubeClientSet.(*kubefake.Clientset).PrependReactor("list", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		if !failed {
			failed = true
			return true, nil, fmt.Errorf("API observation unavailable")
		}
		return false, nil, nil
	})
	observed, err := c.podsForRoleObservation(ctx, ms, "snapshot-error-0", "prefill", "prefill-0")
	require.Error(t, err)
	require.Empty(t, observed, "the old Ready Pod cannot grant capacity when refreshing fails")
	observed, err = c.podsForRoleObservation(ctx, ms, "snapshot-error-0", "prefill", "prefill-0")
	require.NoError(t, err)
	require.Empty(t, observed, "a failed refresh must remain invalid until API observation succeeds")
}
