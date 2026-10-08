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
	"encoding/json"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	mslisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func recoveryFixture(policy workloadv1alpha1.RecoveryPolicy, workers int32) (*workloadv1alpha1.ModelServing, []*corev1.Pod) {
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "ms", Namespace: "default", UID: "ms-uid"}, Spec: workloadv1alpha1.ModelServingSpec{
		Replicas: ptr.To[int32](1), RecoveryPolicy: policy, Template: workloadv1alpha1.ServingGroup{RestartGracePeriodSeconds: ptr.To[int64](0), Roles: []workloadv1alpha1.Role{{Name: "prefill", Replicas: ptr.To[int32](1), WorkerReplicas: workers}}},
	}}
	if workers > 0 {
		ms.Spec.Template.Roles[0].WorkerTemplate = &workloadv1alpha1.PodTemplateSpec{}
	}
	var pods []*corev1.Pod
	for i := 0; i <= int(workers); i++ {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: utils.GeneratePodName("ms-0", "prefill-0", i), Namespace: ms.Namespace, UID: types.UID(fmt.Sprintf("pod-%d", i)), ResourceVersion: "1",
			Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name, workloadv1alpha1.GroupNameLabelKey: "ms-0", workloadv1alpha1.RoleLabelKey: "prefill", workloadv1alpha1.RoleIDKey: "prefill-0", workloadv1alpha1.RevisionLabelKey: utils.ModelServingRevision(ms), workloadv1alpha1.RoleTemplateHashLabelKey: utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}
		pods = append(pods, pod)
	}
	return ms, pods
}

func recoveryController(t *testing.T, ms *workloadv1alpha1.ModelServing, pods ...*corev1.Pod) (*ModelServingController, *kubefake.Clientset) {
	t.Helper()
	objects := make([]runtime.Object, 0, len(pods))
	for _, pod := range pods {
		objects = append(objects, pod.DeepCopy())
	}
	kube := kubefake.NewSimpleClientset(objects...)
	c, err := NewModelServingController(kube, kthenafake.NewSimpleClientset(ms.DeepCopy()), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	c.recorder = record.NewFakeRecorder(1000)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms.DeepCopy()))
	for _, pod := range pods {
		require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
		c.addPod(pod)
	}
	c.initialSync.Store(true)
	t.Cleanup(c.workqueue.ShutDown)
	return c, kube
}

// A failure/Ready transition is an API state change before its informer event.
func updateRecoveryPod(t *testing.T, c *ModelServingController, pod *corev1.Pod) {
	t.Helper()
	_, err := c.kubeClientSet.CoreV1().Pods(pod.Namespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
}

func TestRecoveryPolicyTimerUsesCurrentPolicy(t *testing.T) {
	for _, change := range []string{"none", "infinite", "new-owner"} {
		t.Run(change, func(t *testing.T) {
			ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
			pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
			c, kube := newGracePeriodTestController(t, ms, pods[0])
			latest := ms.DeepCopy()
			switch change {
			case "none":
				latest.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			case "infinite":
				latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
			case "new-owner":
				latest.UID = "replacement-modelserving"
			}
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, indexer.Add(latest))
			c.modelServingLister = mslisters.NewModelServingLister(indexer)
			key := getPodGracePeriodKey(pods[0])
			started := time.Now().Add(-time.Minute)
			c.graceMap.Store(key, started)
			c.handlePodAfterGraceTime(ms, pods[0], started)
			for _, action := range kube.Actions() {
				require.False(t, action.Matches("delete", "pods"))
			}
			_, pending := c.graceMap.Load(key)
			require.False(t, pending)
		})
	}
}

func TestRecoveryGraceLargeValueDoesNotOverflow(t *testing.T) {
	ms, _ := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](math.MaxInt64)
	require.Greater(t, restartGraceRemaining(ms, time.Now().Add(-time.Hour)), time.Duration(0))
}

func TestRecoveryGraceRestoresPersistedFaultStartAfterRestart(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.ResourceVersion = "1"
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
	pod := pods[0].DeepCopy()
	pod.Status.Phase = corev1.PodFailed
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	started := time.Now().Add(-2 * time.Minute).UTC()
	raw, err := json.Marshal(map[string]any{pod.Name: map[string]any{"uid": pod.UID, "startedAt": started}})
	require.NoError(t, err)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: utils.GroupMembersStateName(ms), Namespace: ms.Namespace,
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))},
	}, Data: map[string]string{"recovery.json": string(raw)}}
	kube := kubefake.NewSimpleClientset(pod.DeepCopy(), cm)
	c, err := NewModelServingController(kube, kthenafake.NewSimpleClientset(ms.DeepCopy()), volcanofake.NewSimpleClientset(), apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	c.recorder = record.NewFakeRecorder(1000)
	ctx, cancel := context.WithCancel(context.Background())
	c.recoveryCtx = ctx
	t.Cleanup(func() { cancel(); c.workqueue.ShutDown() })
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms.DeepCopy()))
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
	c.addPod(pod)
	c.initialSync.Store(true)

	require.Eventually(t, func() bool {
		return len(recoveryDeletedPods(kube)) == 1
	}, 2*time.Second, 10*time.Millisecond, "offline time must count toward the persisted grace deadline")
}

func TestRecoveryEpisodePersistsWhileRecoveryDisabled(t *testing.T) {
	for _, disabled := range []string{"none", "infinite"} {
		t.Run(disabled, func(t *testing.T) {
			ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
			ms.ResourceVersion = "1"
			if disabled == "none" {
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
			} else {
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
			}
			c, kube := recoveryController(t, ms, pods...)
			ctx, cancel := context.WithCancel(context.Background())
			c.recoveryCtx = ctx
			t.Cleanup(cancel)
			failed := pods[0].DeepCopy()
			failed.Status.Phase = corev1.PodFailed
			failed.Status.Conditions[0].Status = corev1.ConditionFalse
			updateRecoveryPod(t, c, failed)
			c.updatePod(pods[0], failed)
			_, scheduled := c.graceMap.Load(getPodGracePeriodKey(failed))
			require.False(t, scheduled)
			cm, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, utils.GroupMembersStateName(ms), metav1.GetOptions{})
			require.NoError(t, err)
			episodes, err := readRecoveryEpisodes(cm)
			require.NoError(t, err)
			first := episodes[failed.Name]
			require.Equal(t, failed.UID, first.UID)

			latest := ms.DeepCopy()
			latest.Generation++
			latest.Spec.RecoveryPolicy = workloadv1alpha1.RoleRecreate
			latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, latest, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
			require.NoError(t, c.revisitPodRecovery(ctx, latest))
			started, scheduled := c.graceMap.Load(getPodGracePeriodKey(failed))
			require.True(t, scheduled)
			require.True(t, started.(time.Time).Equal(first.StartedAt.Time))

			ready := failed.DeepCopy()
			ready.Status.Phase = corev1.PodRunning
			ready.Status.Conditions[0].Status = corev1.ConditionTrue
			updateRecoveryPod(t, c, ready)
			c.updatePod(failed, ready)
			_, scheduled = c.graceMap.Load(getPodGracePeriodKey(failed))
			require.False(t, scheduled)
			cm, err = kube.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, utils.GroupMembersStateName(ms), metav1.GetOptions{})
			require.NoError(t, err)
			episodes, err = readRecoveryEpisodes(cm)
			require.NoError(t, err)
			require.Empty(t, episodes)
		})
	}
}

func TestRecoveryEpisodeDoesNotCrossPodUID(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.ResourceVersion = "1"
	c, kube := recoveryController(t, ms, pods...)
	oldStart := time.Now().Add(-time.Hour)
	started, err := c.ensureRecoveryEpisode(context.Background(), ms, pods[0], oldStart)
	require.NoError(t, err)
	require.True(t, started.Equal(oldStart))
	replacement := pods[0].DeepCopy()
	replacement.UID = "replacement-pod"
	newObservation := time.Now()
	started, err = c.ensureRecoveryEpisode(context.Background(), ms, replacement, newObservation)
	require.NoError(t, err)
	require.True(t, started.Equal(newObservation), "a replacement UID starts a new fault episode")
	cm, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), utils.GroupMembersStateName(ms), metav1.GetOptions{})
	require.NoError(t, err)
	episodes, err := readRecoveryEpisodes(cm)
	require.NoError(t, err)
	require.Equal(t, replacement.UID, episodes[replacement.Name].UID)
}

func TestRecoveryEpisodeWriteFailureDoesNotScheduleDeletion(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.ResourceVersion = "1"
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](0)
	c, kube := recoveryController(t, ms, pods...)
	kube.PrependReactor("update", "configmaps", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected recovery state write failure")
	})
	failed := pods[0].DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	failed.Status.Conditions[0].Status = corev1.ConditionFalse
	err := c.handleErrorPod(ms, "ms-0", failed)
	require.ErrorContains(t, err, "injected recovery state write failure")
	_, scheduled := c.graceMap.Load(getPodGracePeriodKey(failed))
	require.False(t, scheduled)
	require.Empty(t, recoveryDeletedPods(kube))
}

func TestRecoveryFailedPodCreationDoesNotBypassPolicy(t *testing.T) {
	for _, tc := range []struct {
		policy workloadv1alpha1.RecoveryPolicy
		grace  int64
	}{
		{workloadv1alpha1.NoneRestartPolicy, 0},
		{workloadv1alpha1.RoleRecreate, -1},
		{workloadv1alpha1.ServingGroupRecreate, -1},
		{workloadv1alpha1.RoleRecreate, 30},
	} {
		t.Run(fmt.Sprintf("%s/%d", tc.policy, tc.grace), func(t *testing.T) {
			ms, pods := recoveryFixture(tc.policy, 0)
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To(tc.grace)
			pods[0].Status.Phase = corev1.PodFailed
			pods[0].Status.Conditions[0].Status = corev1.ConditionFalse
			c, kube := recoveryController(t, ms, pods...)
			desired := pods[0].DeepCopy()
			desired.Status = corev1.PodStatus{}
			require.NoError(t, c.createPod(context.Background(), ms, "ms-0", "prefill", "prefill-0", &ms.Spec.Template.Roles[0], desired, true, nil, "entry"))
			for _, action := range kube.Actions() {
				require.False(t, action.Matches("delete", "pods"), "creation must not bypass failure recovery")
			}
		})
	}
}

func recoveryDeletedPods(kube *kubefake.Clientset) []string {
	var names []string
	for _, a := range kube.Actions() {
		if a.Matches("delete", "pods") {
			names = append(names, a.(kubetesting.DeleteAction).GetName())
		} else if a.Matches("delete-collection", "pods") {
			names = append(names, "collection:"+a.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels.String())
		}
	}
	return names
}

func TestRecoveryPolicyUnhealthyPods(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		for _, grace := range []int64{-1, 0, 60} {
			for _, failure := range []string{"container", "init-container", "failed"} {
				t.Run(fmt.Sprintf("%s/%d/%s", policy, grace, failure), func(t *testing.T) {
					ms, pods := recoveryFixture(policy, 1)
					ms.Spec.Template.RestartGracePeriodSeconds = ptr.To(grace)
					c, kube := recoveryController(t, ms, pods...)
					pod := pods[1].DeepCopy()
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
					switch failure {
					case "container":
						pod.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
					case "init-container":
						pod.Status.Phase = corev1.PodPending
						pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
					case "failed":
						pod.Status.Phase = corev1.PodFailed
					}
					updateRecoveryPod(t, c, pod)
					c.updatePod(pods[1], pod)
					require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
					require.Equal(t, datastore.ServingGroupCreating, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "ms-0"))
					key := getPodGracePeriodKey(pod)
					if policy != workloadv1alpha1.NoneRestartPolicy && grace == 0 {
						require.Eventually(t, func() bool { _, ok := c.graceMap.Load(key); return !ok }, 2*time.Second, 10*time.Millisecond)
						require.Equal(t, []string{pod.Name}, recoveryDeletedPods(kube))
						return
					}
					require.Empty(t, recoveryDeletedPods(kube))
					_, waiting := c.graceMap.Load(key)
					require.Equal(t, policy != workloadv1alpha1.NoneRestartPolicy && grace > 0, waiting)
					// Cancel any finite wait and verify the real asynchronous task exits.
					latest := ms.DeepCopy()
					latest.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
					require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
					require.Eventually(t, func() bool { _, ok := c.graceMap.Load(key); return !ok }, 2*time.Second, 10*time.Millisecond)
					require.Empty(t, recoveryDeletedPods(kube))
				})
			}
		}
	}
}

func TestRecoveryPolicyChangesDuringGrace(t *testing.T) {
	for _, change := range []string{"none", "infinite", "extend", "shorten", "ready"} {
		t.Run(change, func(t *testing.T) {
			ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](1)
			c, kube := recoveryController(t, ms, pods...)
			pod := pods[0].DeepCopy()
			pod.Status.Conditions[0].Status = corev1.ConditionFalse
			pod.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			updateRecoveryPod(t, c, pod)
			c.updatePod(pods[0], pod)
			key := getPodGracePeriodKey(pod)
			started, waiting := c.graceMap.Load(key)
			require.True(t, waiting)
			latest := ms.DeepCopy()
			switch change {
			case "none":
				latest.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			case "infinite":
				latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
			case "extend":
				latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
			case "shorten":
				latest.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](0)
			case "ready":
				ready := pod.DeepCopy()
				ready.Status.Conditions[0].Status = corev1.ConditionTrue
				updateRecoveryPod(t, c, ready)
				c.updatePod(pod, ready)
			}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
			if change == "extend" {
				require.Never(t, func() bool { return len(recoveryDeletedPods(kube)) > 0 }, 1200*time.Millisecond, 10*time.Millisecond)
				got, ok := c.graceMap.Load(key)
				require.True(t, ok)
				require.Equal(t, started, got)
				latest = latest.DeepCopy()
				latest.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
			}
			require.Eventually(t, func() bool { _, ok := c.graceMap.Load(key); return !ok }, 2*time.Second, 10*time.Millisecond)
			if change == "shorten" {
				require.Equal(t, []string{pod.Name}, recoveryDeletedPods(kube))
			} else {
				require.Empty(t, recoveryDeletedPods(kube))
			}
			if change == "ready" {
				require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "ms-0"))
			}
		})
	}
}

func TestRecoveryPolicyPodDeletionScope(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failed=%t", policy, failed), func(t *testing.T) {
				ms, pods := recoveryFixture(policy, 1)
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
				if failed {
					pods[1].Status.Phase = corev1.PodFailed
					pods[1].Status.Conditions[0].Status = corev1.ConditionFalse
				}
				c, kube := recoveryController(t, ms, pods...)
				require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(context.Background(), pods[1].Name, metav1.DeleteOptions{}))
				require.NoError(t, c.podsInformer.GetIndexer().Delete(pods[1]))
				kube.ClearActions()
				c.deletePod(pods[1])
				if policy == workloadv1alpha1.NoneRestartPolicy {
					require.Empty(t, recoveryDeletedPods(kube))
					require.Positive(t, c.workqueue.Len())
					require.NoError(t, c.CreatePodsByRole(context.Background(), ms.Spec.Template.Roles[0], ms, 0, 0, utils.ObjectRevision(pods[0]), utils.ObjectRoleTemplateHash(pods[0]), ""))
					_, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), pods[1].Name, metav1.GetOptions{})
					require.NoError(t, err)
				} else {
					require.Equal(t, []string{pods[0].Name}, recoveryDeletedPods(kube))
				}
			})
		}
	}
}

func TestRecoveryDeleteEventScopeAcrossControllerRestart(t *testing.T) {
	for _, restarted := range []bool{false, true} {
		t.Run(fmt.Sprintf("restarted=%t", restarted), func(t *testing.T) {
			ctx := context.Background()
			ms, pods := recoveryFixture(workloadv1alpha1.ServingGroupRecreate, 1)
			ms.ResourceVersion = "1"
			failed := pods[1].DeepCopy()
			failed.Status.Phase = corev1.PodFailed
			failed.Status.Conditions[0].Status = corev1.ConditionFalse
			c, kube := recoveryController(t, ms, pods...)
			_, err := c.ensureRecoveryEpisode(ctx, ms, failed, time.Now().Add(-time.Minute))
			require.NoError(t, err)
			if !restarted {
				c.recoveryObservations.Store(getPodGracePeriodKey(failed), struct{}{})
			}
			require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(ctx, failed.Name, metav1.DeleteOptions{}))
			require.NoError(t, c.podsInformer.GetIndexer().Delete(failed))
			kube.ClearActions()

			c.deletePod(failed)
			if restarted {
				require.Empty(t, recoveryDeletedPods(kube), "a new process converges from the surviving Pod")
				require.Positive(t, c.workqueue.Len())
			} else {
				require.Equal(t, []string{pods[0].Name}, recoveryDeletedPods(kube), "continuous ServingGroupRecreate keeps its configured scope")
			}
			matched, err := c.recoveryEpisodeMatches(ctx, ms, failed.Name, failed.UID)
			require.NoError(t, err)
			require.False(t, matched)
		})
	}
}

func TestRecoveryReadyStartsNewGraceEpisode(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
	c, kube := recoveryController(t, ms, pods...)
	pod := pods[0].DeepCopy()
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	pod.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	updateRecoveryPod(t, c, pod)
	key := getPodGracePeriodKey(pod)
	oldStart := time.Now().Add(-time.Minute)
	c.graceMap.Store(key, oldStart)
	// Ready clears the old episode before a second unhealthy event arrives.
	updateRecoveryPod(t, c, pods[0])
	c.updatePod(pod, pods[0])
	updateRecoveryPod(t, c, pod)
	c.updatePod(pods[0], pod)
	newStart, ok := c.graceMap.Load(key)
	require.True(t, ok)
	require.NotEqual(t, oldStart, newStart)
	// A delayed old task must neither delete the Pod nor remove the newer wait.
	c.handlePodAfterGraceTime(ms, pod, oldStart)
	got, ok := c.graceMap.Load(key)
	require.True(t, ok)
	require.Equal(t, newStart, got)
	require.Empty(t, recoveryDeletedPods(kube))
	latest := ms.DeepCopy()
	latest.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
	require.Eventually(t, func() bool { _, ok := c.graceMap.Load(key); return !ok }, 2*time.Second, 10*time.Millisecond)
	require.Empty(t, recoveryDeletedPods(kube))
}
