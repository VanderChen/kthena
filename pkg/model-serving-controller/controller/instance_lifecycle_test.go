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

func TestInstanceLifecycle_GraceRevalidatesLiveState(t *testing.T) {
	for _, change := range []string{"none", "infinite", "extend", "ready", "plain-notready", "pod-uid", "ms-uid", "get-failure", "delete-conflict", "recover"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("live-grace", 1, 1, 0)
			ms.ResourceVersion = "1"
			c := lifecycleController(t, ms)
			pod := lifecyclePod(t, c, ms, ms, 0, "failed-uid")
			pod.ResourceVersion = "10"
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", RestartCount: 1}}
			require.NoError(t, c.podsInformer.GetIndexer().Update(pod.DeepCopy()))
			latestPod, latestMS := pod.DeepCopy(), ms.DeepCopy()
			switch change {
			case "none":
				latestMS.Spec.RecoveryPolicy = api.NoneRestartPolicy
			case "infinite":
				latestMS.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
			case "extend":
				latestMS.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](120)
			case "ready":
				latestPod.Status.Conditions[0].Status = corev1.ConditionTrue
			case "plain-notready":
				latestPod.Status.ContainerStatuses = nil
			case "pod-uid":
				latestPod.UID = "replacement"
			case "ms-uid":
				latestMS.UID = "replacement"
			}
			_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, latestMS, metav1.UpdateOptions{})
			require.NoError(t, err)
			_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, latestPod, metav1.UpdateOptions{})
			require.NoError(t, err)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			if change == "get-failure" {
				kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewServiceUnavailable("injected GET failure")
				})
			}
			if change == "delete-conflict" {
				kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					pre := a.(kubetesting.DeleteAction).GetDeleteOptions().Preconditions
					require.Equal(t, pod.UID, *pre.UID)
					require.Equal(t, pod.ResourceVersion, *pre.ResourceVersion)
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("Ready changed after GET"))
				})
			}
			started := time.Now().Add(-time.Minute)
			c.graceMap.Store(getPodGracePeriodKey(pod), started)
			kube.ClearActions()
			delay, err := c.recoverPodAfterGrace(ctx, ms, pod, started)
			if change == "get-failure" || change == "delete-conflict" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			if change == "extend" {
				require.Greater(t, delay, time.Duration(0))
			}
			require.Equal(t, change == "recover" || change == "delete-conflict", lifecycleDeletionMatches(c, pod))
		})
	}
}

func TestInstanceLifecycle_GraceStopsWithController(t *testing.T) {
	ms := lifecycleMS("stopped-grace", 1, 1, 0)
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "failed-uid")
	ctx, cancel := context.WithCancel(context.Background())
	c.recoveryCtx = ctx
	started := time.Now().Add(-time.Minute)
	c.graceMap.Store(getPodGracePeriodKey(pod), started)
	cancel()
	c.handlePodAfterGraceTime(ms, pod, started)
	require.False(t, lifecycleDeletionMatches(c, pod))
	_, pending := c.graceMap.Load(getPodGracePeriodKey(pod))
	require.False(t, pending)
}

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

func TestInstanceLifecycleCreatedRoleRecoversOriginalScopeAfterRestart(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate, api.NoneRestartPolicy} {
		for _, missingIndex := range []int{0, 1} {
			t.Run(fmt.Sprintf("%s/missing=%d", policy, missingIndex), func(t *testing.T) {
				ms := lifecycleMS("cold-scope", 1, 1, 0)
				ms.ResourceVersion, ms.Spec.RecoveryPolicy = "1", policy
				ms.Spec.Template.Roles[0].WorkerReplicas = 1
				ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
				other := *ms.Spec.Template.Roles[0].DeepCopy()
				other.Name, other.WorkerReplicas = "other", 0
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, other)
				c := lifecycleController(t, ms)
				key, group, revision := utils.GetNamespaceName(ms), "cold-scope-0", utils.ModelServingRevision(ms)
				c.store.AddServingGroup(key, 0, revision)
				kube := c.kubeClientSet.(*kubefake.Clientset)
				uid := 0
				kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					uid++
					pod := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
					pod.UID = types.UID(fmt.Sprint(uid))
					return false, nil, nil
				})
				require.NoError(t, c.CreatePodsForServingGroup(context.Background(), ms, 0, revision, ms.Spec.Template.Roles))
				original, err := kube.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
				require.NoError(t, err)
				missing := utils.GeneratePodName(group, "prefill-0", missingIndex)
				require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(context.Background(), missing, metav1.DeleteOptions{}))
				// No deletion callback was delivered while the controller was down.
				// None of these Pods has become Ready; creation is a separate fact.
				c.store = datastore.New()
				for i := range original.Items {
					pod := &original.Items[i]
					require.Equal(t, "true", pod.Annotations[roleCreatedAnnotation])
					if pod.Name == missing {
						continue
					}
					require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
					c.store.AddServingGroupAndRole(key, group, revision, utils.ObjectRoleTemplateHash(pod), utils.GetRoleName(pod), utils.GetRoleID(pod))
				}
				kube.ClearActions()
				ctx, err := c.withRolloutPodSnapshot(context.Background(), ms)
				require.NoError(t, err)
				require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
				require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, group, ms.Spec.Template.Roles[0], 0, revision, nil, true))
				for i := range original.Items {
					pod := &original.Items[i]
					if pod.Name == missing {
						continue
					}
					wantDelete := policy == api.ServingGroupRecreate || (policy == api.RoleRecreate && utils.GetRoleName(pod) == "prefill")
					require.Equal(t, wantDelete, lifecycleDeletionMatches(c, pod), "original member %s", pod.Name)
				}
			})
		}
	}
}

func TestInstanceLifecyclePartialCreationIsCompletedWithoutRecoveryChurn(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			ms := lifecycleMS("partial-create", 1, 1, 0)
			ms.ResourceVersion, ms.Spec.RecoveryPolicy = "1", policy
			ms.Spec.Template.Roles[0].WorkerReplicas = 1
			ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			c := lifecycleController(t, ms)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			failWorker := true
			kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
				pod := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
				pod.UID = types.UID(pod.Name)
				if failWorker && pod.Name == "partial-create-0-prefill-0-1" {
					return true, nil, apierrors.NewServiceUnavailable("worker create held")
				}
				return false, nil, nil
			})
			revision, group := utils.ModelServingRevision(ms), "partial-create-0"
			require.Error(t, c.CreatePodsForServingGroup(context.Background(), ms, 0, revision, ms.Spec.Template.Roles))
			entry, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), group+"-prefill-0-0", metav1.GetOptions{})
			require.NoError(t, err)
			require.Empty(t, entry.Annotations[roleCreatedAnnotation])
			c.store = datastore.New()
			c.store.AddServingGroupAndRole(utils.GetNamespaceName(ms), group, revision, utils.ObjectRoleTemplateHash(entry), "prefill", "prefill-0")
			require.NoError(t, c.podsInformer.GetIndexer().Add(entry))
			failWorker = false
			kube.ClearActions()
			require.NoError(t, c.manageRoleReplicasPerGroup(context.Background(), ms, group, ms.Spec.Template.Roles[0], 0, revision, nil, true))
			require.False(t, lifecycleDeletionMatches(c, entry))
			pods, err := kube.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, 2)
			for _, pod := range pods.Items {
				require.Equal(t, "true", pod.Annotations[roleCreatedAnnotation])
			}
		})
	}
}

func TestInstanceLifecycle_ColdStartRetainsFailedInstanceForRollout(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
			for _, recovery := range []string{"none", "grace-minus-one"} {
				for _, health := range []string{"failed", "restarting", "plain-not-ready"} {
					t.Run(fmt.Sprintf("changed=%t/%s/%s/%s", changed, mode, recovery, health), func(t *testing.T) {
						ctx := context.Background()
						old := lifecycleMS("cold-fault", 1, 1, 0)
						old.ResourceVersion = "1"
						old.Spec.RolloutStrategy.Type = mode
						old.Spec.Template.Roles[0].MaxUnavailable = ptr.To(intstr.FromInt(1))
						old.Spec.RecoveryPolicy = api.NoneRestartPolicy
						if recovery == "grace-minus-one" {
							old.Spec.RecoveryPolicy = api.RoleRecreate
							old.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
						}
						ms := old.DeepCopy()
						if changed {
							ms.Generation++
							ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
						}
						ms.Status.CurrentRevision = utils.ModelServingRevision(old)
						ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
						c := lifecycleController(t, ms, old)
						role := *old.Spec.Template.Roles[0].DeepCopy()
						pod := utils.GenerateEntryPod(role, ms, "cold-fault-0", "prefill-0", utils.ModelServingRevision(old), utils.CalRoleTemplateHash(role))
						pod.UID, pod.ResourceVersion = "old-fault", "5"
						pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}
						if health == "failed" {
							pod.Status.Phase = corev1.PodFailed
						} else if health == "restarting" {
							pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: role.EntryTemplate.Spec.Containers[0].Name, RestartCount: 1}}
						}
						_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						require.NoError(t, err)
						require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
						c.initialSync.Store(false)
						c.syncAll()
						c.kubeClientSet.(*kubefake.Clientset).ClearActions()
						var lastErr error
						for attempt := 0; attempt < 3; attempt++ {
							lastErr = c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
							if lifecycleDeletionMatches(c, pod) {
								break
							}
						}
						t.Logf("health=%s observedGroup=%s reconcileError=%v", health, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "cold-fault-0"), lastErr)
						require.Equal(t, changed, lifecycleDeletionMatches(c, pod), "rollout must recognize an existing unhealthy old instance after restart; None/-1 only disable proactive failure recovery")
					})
				}
			}
		}
	}
}
func TestColdStartPreservesWholeGroupRecoveryScope(t *testing.T) {
	for _, cold := range []bool{false, true} {
		for _, policy := range []api.RecoveryPolicy{api.ServingGroupRecreate, api.NoneRestartPolicy} {
			t.Run(fmt.Sprintf("cold=%t/policy=%s", cold, policy), func(t *testing.T) {
				ctx := context.Background()
				ms := lifecycleMS("lost-role", 1, 1, 0)
				ms.ResourceVersion = "1"
				ms.Spec.RecoveryPolicy = policy
				other := *ms.Spec.Template.Roles[0].DeepCopy()
				other.Name = "decode"
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, other)
				c := lifecycleController(t, ms)
				lost := lifecyclePod(t, c, ms, ms, 0, "lost-entry")
				revision, hash := utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(other)
				survivor := utils.GenerateEntryPod(*other.DeepCopy(), ms, "lost-role-0", "decode-0", revision, hash)
				survivor.UID, survivor.ResourceVersion = "survivor", "2"
				survivor.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, survivor, metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Add(survivor))
				c.store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), "lost-role-0", survivor.Name, revision, hash, "decode", "decode-0")
				require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), "lost-role-0", "decode", "decode-0", datastore.RoleRunning))
				require.NoError(t, c.setGroupMembers(ctx, ms, "lost-role-0", revision, ms.Spec.Template.Roles))
				require.NoError(t, c.markRoleCreated(ctx, ms, "lost-role-0", "prefill", "prefill-0"))
				require.NoError(t, c.markRoleCreated(ctx, ms, "lost-role-0", "decode", "decode-0"))
				// Physical loss happens without a PodDeleted callback. A cold
				// controller retains API objects and member state but loses RAM.
				require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, lost.Name, metav1.DeleteOptions{}))
				require.NoError(t, c.podsInformer.GetIndexer().Delete(lost))
				if cold {
					fresh := lifecycleController(t, ms)
					fresh.kubeClientSet = c.kubeClientSet
					require.NoError(t, fresh.podsInformer.GetIndexer().Add(survivor))
					fresh.initialSync.Store(false)
					fresh.syncAll()
					c = fresh
				}
				c.kubeClientSet.(*kubefake.Clientset).ClearActions()
				err = c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
				require.NoError(t, err)
				deleted := lifecycleDeletionMatches(c, survivor)
				t.Logf("cold=%t policy=%s survivingRoleDeleted=%t", cold, policy, deleted)
				require.Equal(t, policy == api.ServingGroupRecreate, deleted, "physical loss of an established Role must keep the requested recovery scope after restart")
			})
		}
	}
}

func TestInstanceLifecycle_EnablingRecoveryRevisitsExistingFault(t *testing.T) {
	for _, initial := range []string{"none", "infinite", "long-grace"} {
		for _, health := range []string{"failed", "restarting", "ready-restarted", "plain-notready"} {
			t.Run(initial+"/"+health, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				ms := lifecycleMS("enable-recovery", 1, 1, 0)
				ms.ResourceVersion = "1"
				ms.Spec.RecoveryPolicy = api.NoneRestartPolicy
				if initial != "none" {
					ms.Spec.RecoveryPolicy = api.RoleRecreate
					ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
				}
				if initial == "long-grace" {
					ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](10000)
				}
				c := lifecycleController(t, ms)
				c.recoveryCtx = ctx
				pod := lifecyclePod(t, c, ms, ms, 0, "existing-fault")
				pod.Status.Conditions[0].Status = corev1.ConditionFalse
				if health == "failed" {
					pod.Status.Phase = corev1.PodFailed
				}
				if health == "restarting" || health == "ready-restarted" {
					pod.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "server", RestartCount: 1}}
				}
				if health == "ready-restarted" {
					pod.Status.Conditions[0].Status = corev1.ConditionTrue
				}
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).UpdateStatus(ctx, pod, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
				// Replay the observation under the original disabled/long grace setting.
				c.updatePod(nil, pod)
				latest := ms.DeepCopy()
				latest.Generation++
				latest.Spec.RecoveryPolicy = api.RoleRecreate
				latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](100)
				_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
				require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
				started, scheduled := c.graceMap.Load(getPodGracePeriodKey(pod))
				fault := health == "failed" || health == "restarting"
				require.Equal(t, fault, scheduled, "MS reconcile must revisit actual faults only")
				for i := 0; i < 3; i++ {
					require.NoError(t, c.revisitPodRecovery(context.Background(), latest))
				}
				again, ok := c.graceMap.Load(getPodGracePeriodKey(pod))
				require.Equal(t, scheduled, ok)
				if scheduled {
					require.True(t, started.(time.Time).Equal(again.(time.Time)), "reconcile must not reset or duplicate an episode")
				}
				if fault {
					latest = latest.DeepCopy()
					latest.Generation++
					latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](0)
					_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
					require.NoError(t, err)
					require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
					require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
					require.Eventually(t, func() bool { return lifecycleDeletionMatches(c, pod) }, 3*time.Second, 10*time.Millisecond)
				} else {
					require.False(t, lifecycleDeletionMatches(c, pod))
				}
			})
		}
	}
}
