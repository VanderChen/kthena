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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
)

func pendingUpdateFixture(t *testing.T) (*ModelServingController, *kubefake.Clientset, *api.ModelServing, *corev1.Pod, *corev1.Pod) {
	t.Helper()
	ms, pods := recoveryFixture(api.NoneRestartPolicy, 0)
	pod := pods[0]
	pod.ResourceVersion = "rv-new"
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending, Conditions: []corev1.PodCondition{{
		Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: "Unschedulable", Message: "no node fits",
	}}}
	c, kube := recoveryController(t, ms, pod)
	for c.workqueue.Len() > 0 {
		key, _ := c.workqueue.Get()
		c.workqueue.Done(key)
		c.workqueue.Forget(key)
	}
	kube.ClearActions()
	old := pod.DeepCopy()
	old.ResourceVersion = "rv-old"
	old.Status.Conditions[0].Message = "previous scheduling attempt"
	return c, kube, ms, old, pod
}

// One goroutine models the shared listener: B cannot even start until A's
// callback returns. Keep A's lifecycle lock held until both have returned.
func TestPendingPodUpdateDoesNotBlockOtherModelServing(t *testing.T) {
	c, kube, ms, old, pod := pendingUpdateFixture(t)
	other := ms.DeepCopy()
	other.Name, other.UID = "other", "other-owner"
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(other))
	lost := pod.DeepCopy()
	lost.Name, lost.UID = "other-0-prefill-0-0", "lost-uid"
	lost.Labels[api.ModelServingNameLabelKey], lost.Labels[api.GroupNameLabelKey] = other.Name, "other-0"
	lost.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(other, api.SchemeGroupVersion.WithKind("ModelServing"))}
	c.store.AddServingGroupAndRole(utils.GetNamespaceName(other), "other-0", "revision", "hash", "prefill", "prefill-0")

	key := ms.Namespace + "/" + ms.Name
	c.workqueue.Add(key)
	inFlight, _ := c.workqueue.Get()
	var release sync.Once
	unlock := c.lifecycle.lock(key)
	defer release.Do(unlock)
	done := make(chan struct{})
	go func() {
		c.updatePod(old, pod)
		c.deletePod(cache.DeletedFinalStateUnknown{Key: lost.Namespace + "/" + lost.Name, Obj: lost})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		release.Do(unlock)
		<-done
		t.Fatal("ordinary Pending callback blocked the following MS deletion on the lifecycle lock")
	}
	// B was enqueued even though A is still processing. A must also run again
	// after Done: the quick observation must not lose an in-flight wakeup.
	require.Equal(t, 1, c.workqueue.Len())
	next, _ := c.workqueue.Get()
	require.Equal(t, other.Namespace+"/"+other.Name, next)
	c.workqueue.Done(next)
	c.workqueue.Done(inFlight)
	require.Equal(t, 1, c.workqueue.Len())
	next, _ = c.workqueue.Get()
	require.Equal(t, key, next)
	c.workqueue.Done(next)
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
	require.Len(t, kube.Actions(), 1)
	require.True(t, kube.Actions()[0].Matches("get", "pods"), "quick path may only observe the live Pod")
}

func TestPendingPodUpdateRoutesConservatively(t *testing.T) {
	for _, change := range []string{"status-only", "initializing", "nil-old", "old-uid", "new-uid", "old-rv", "new-rv", "namespace", "name", "labels", "annotations", "owner", "finalizer", "spec", "ip", "ips", "terminating", "delete-marker", "old-running", "new-running", "old-init-restart", "new-restart", "new-init-restart", "owner-replaced", "owner-deleting", "owner-missing", "foreign-owner", "store-missing"} {
		t.Run(change, func(t *testing.T) {
			c, kube, ms, old, pod := pendingUpdateFixture(t)
			switch change {
			case "initializing":
				c.initialSync.Store(false)
			case "nil-old":
				old = nil
			case "old-uid":
				old.UID = ""
			case "new-uid":
				pod.UID = "replaced"
			case "old-rv":
				old.ResourceVersion = ""
			case "new-rv":
				pod.ResourceVersion = ""
			case "namespace":
				pod.Namespace = "other"
			case "name":
				pod.Name = "other"
			case "labels":
				pod.Labels[api.RoleIDKey] = "prefill-1"
			case "annotations":
				pod.Annotations = map[string]string{"input": "changed"}
			case "owner":
				pod.OwnerReferences[0].UID = "new-owner"
			case "finalizer":
				pod.Finalizers = []string{"test.example/hold"}
			case "spec":
				pod.Spec.NodeName = "scheduled"
			case "ip":
				pod.Status.PodIP = "10.0.0.1"
			case "ips":
				pod.Status.PodIPs = []corev1.PodIP{{IP: "10.0.0.1"}}
			case "terminating":
				pod.DeletionTimestamp = ptr.To(metav1.Now())
			case "delete-marker":
				pod.Annotations = map[string]string{deletionScopeAnnotation: deleteRoleScope}
			case "old-running":
				old.Status.Phase = corev1.PodRunning
			case "new-running":
				pod.Status.Phase = corev1.PodRunning
			case "old-init-restart":
				old.Status.InitContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			case "new-restart":
				pod.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			case "new-init-restart":
				pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			case "owner-replaced":
				ms.UID = "replacement"
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms))
			case "owner-deleting":
				ms.DeletionTimestamp = ptr.To(metav1.Now())
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms))
			case "owner-missing":
				require.NoError(t, c.modelServingsInformer.GetIndexer().Delete(ms))
			case "foreign-owner":
				old.OwnerReferences[0].Name, pod.OwnerReferences[0].Name = "foreign", "foreign"
				_, err := kube.CoreV1().Pods(pod.Namespace).Update(context.Background(), pod, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "store-missing":
				c.store.DeleteModelServing(utils.GetNamespaceName(ms))
			}
			before, err := c.store.DumpCache()
			require.NoError(t, err)
			require.Equal(t, change == "status-only", c.enqueuePendingPodUpdate(old, pod))
			after, err := c.store.DumpCache()
			require.NoError(t, err)
			require.JSONEq(t, string(before), string(after), "routing must not mutate lifecycle bookkeeping")
			require.Equal(t, change == "status-only", c.workqueue.Len() == 1)
		})
	}
}

func TestPendingPodUpdateStoreFallbackRevokesAvailability(t *testing.T) {
	for _, state := range []string{"running-pod", "running-role", "running-group"} {
		t.Run(state, func(t *testing.T) {
			c, kube, ms, old, pod := pendingUpdateFixture(t)
			key := utils.GetNamespaceName(ms)
			switch state {
			case "running-pod":
				c.store.AddRunningPodToServingGroup(key, "ms-0", pod.Name, "revision", "hash", "prefill", "prefill-0")
			case "running-role":
				require.NoError(t, c.store.UpdateRoleStatus(key, "ms-0", "prefill", "prefill-0", datastore.RoleRunning))
			case "running-group":
				require.NoError(t, c.store.UpdateServingGroupStatus(key, "ms-0", datastore.ServingGroupRunning))
			}
			c.updatePod(old, pod)
			require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(key, "ms-0", "prefill", "prefill-0"))
			require.Equal(t, datastore.ServingGroupCreating, c.store.GetServingGroupStatus(key, "ms-0"))
			n, err := c.store.GetRunningPodNumByServingGroup(key, "ms-0")
			require.NoError(t, err)
			require.Zero(t, n, "fallback must retain runningPods cleanup")
			require.Len(t, kube.Actions(), 2, "fallback must re-read after taking the lifecycle lock")
			for _, action := range kube.Actions() {
				require.True(t, action.Matches("get", "pods"))
			}
		})
	}
}

type pendingHookObserver struct {
	plugins.Plugin
	calls []string
}

func (p *pendingHookObserver) Name() string { return "pending-observer" }
func (p *pendingHookObserver) OnPodRunning(_ context.Context, req *plugins.HookRequest) error {
	p.calls = append(p.calls, "running:"+req.Pod.Status.PodIP)
	return nil
}
func (p *pendingHookObserver) OnPodReady(_ context.Context, _ *plugins.HookRequest) error {
	p.calls = append(p.calls, "ready")
	return nil
}

func TestPendingPodUpdateLiveTransitionsKeepHooksAndRecovery(t *testing.T) {
	for _, state := range []string{"running", "ready", "failed", "init-restart", "terminating"} {
		t.Run(state, func(t *testing.T) {
			c, kube, ms, old, pod := pendingUpdateFixture(t)
			ms.Spec.RecoveryPolicy = api.RoleRecreate
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](3600)
			ms.Spec.Plugins = []api.PluginSpec{{Name: "pending-observer", Type: api.PluginTypeBuiltIn}}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms.DeepCopy()))
			ctx, cancel := context.WithCancel(context.Background())
			c.recoveryCtx = ctx
			t.Cleanup(cancel)
			observer := &pendingHookObserver{}
			c.pluginsRegistry = plugins.NewRegistry()
			c.pluginsRegistry.Register(observer.Name(), func(api.PluginSpec) (plugins.Plugin, error) { return observer, nil })
			live := pod.DeepCopy()
			switch state {
			case "running", "ready":
				live.Status.Phase, live.Status.PodIP = corev1.PodRunning, "10.0.0.1"
				if state == "ready" {
					live.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
					c.graceMap.Store(getPodGracePeriodKey(pod), time.Now())
				}
			case "failed":
				live.Status.Phase = corev1.PodFailed
			case "init-restart":
				live.Status.InitContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
			case "terminating":
				live.DeletionTimestamp = ptr.To(metav1.Now())
			}
			_, err := kube.CoreV1().Pods(pod.Namespace).Update(context.Background(), live, metav1.UpdateOptions{})
			require.NoError(t, err)
			// Keep the informer behind the API to exercise the preflight fallback.
			kube.ClearActions()
			c.updatePod(old, pod)
			switch state {
			case "running":
				require.Equal(t, []string{"running:10.0.0.1"}, observer.calls)
			case "ready":
				require.Equal(t, []string{"running:10.0.0.1", "ready"}, observer.calls)
			default:
				require.Empty(t, observer.calls)
			}
			_, scheduled := c.graceMap.Load(getPodGracePeriodKey(pod))
			require.Equal(t, state == "failed" || state == "init-restart", scheduled)
			gets := 0
			for _, action := range kube.Actions() {
				if action.Matches("get", "pods") {
					gets++
				}
				require.False(t, action.Matches("delete", "pods"))
			}
			require.Equal(t, 2, gets)
		})
	}
}

func TestPendingPodUpdateRechecksAfterLifecycleLock(t *testing.T) {
	c, kube, ms, old, pod := pendingUpdateFixture(t)
	preflight := make(chan struct{})
	var gets atomic.Int32
	kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		live := pod.DeepCopy()
		if gets.Add(1) == 1 {
			live.Status.Phase = corev1.PodRunning
			live.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
			close(preflight)
		} else {
			live.UID = "replacement"
		}
		return true, live, nil
	})
	var release sync.Once
	unlock := c.lifecycle.lock(ms.Namespace + "/" + ms.Name)
	defer release.Do(unlock)
	done := make(chan struct{})
	go func() { c.updatePod(old, pod); close(done) }()
	select {
	case <-preflight:
	case <-time.After(5 * time.Second):
		t.Fatal("preflight did not execute before the lifecycle lock")
	}
	require.EqualValues(t, 1, gets.Load())
	release.Do(unlock)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fallback did not finish after releasing the lifecycle lock")
	}
	require.EqualValues(t, 2, gets.Load())
	n, err := c.store.GetRunningPodNumByServingGroup(utils.GetNamespaceName(ms), "ms-0")
	require.NoError(t, err)
	require.Zero(t, n, "preflight Ready must not be applied to a replaced UID")
}

type pendingInterleavedStore struct {
	datastore.Store
	afterObservation func()
}

func (s *pendingInterleavedStore) IsPodUnavailable(ms types.NamespacedName, group, role, instance, pod string) bool {
	result := s.Store.IsPodUnavailable(ms, group, role, instance, pod)
	s.afterObservation()
	return result
}

// A worker may publish a stale Ready classification just after the read-only
// guard. The callback must still dirty its in-flight key; the next real sync
// must revoke that classification before it can spend healthy rollout budget.
func TestPendingPodUpdateWorkerStateChangesAfterObservation(t *testing.T) {
	previous := lifecycleMS("pending-budget", 2, 1, 0)
	ms := previous.DeepCopy()
	ms.ResourceVersion = "1"
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	c := lifecycleController(t, ms, previous)
	pod := lifecyclePod(t, c, ms, previous, 0, "pending-uid")
	healthy := lifecyclePod(t, c, ms, previous, 1, "healthy-uid")
	pod.ResourceVersion = "rv-current"
	pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
	updateRecoveryPod(t, c, pod)
	key := utils.GetNamespaceName(ms)
	require.NoError(t, c.markPodUnavailable(ms, "pending-budget-0", pod))
	old := pod.DeepCopy()
	old.ResourceVersion = "rv-previous"
	c.store = &pendingInterleavedStore{Store: c.store, afterObservation: func() {
		require.NoError(t, c.store.UpdateRoleStatus(key, "pending-budget-0", "prefill", "prefill-0", datastore.RoleRunning))
		require.NoError(t, c.store.UpdateServingGroupStatus(key, "pending-budget-0", datastore.ServingGroupRunning))
	}}
	queueKey := ms.Namespace + "/" + ms.Name
	c.workqueue.Add(queueKey)
	inFlight, _ := c.workqueue.Get()
	c.updatePod(old, pod)
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, "pending-budget-0"), "callback cannot rewrite a concurrent worker result outside its lock")
	c.workqueue.Done(inFlight)
	require.Equal(t, 1, c.workqueue.Len())
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.ClearActions()
	require.NoError(t, c.syncModelServing(context.Background(), queueKey))
	require.NotEqual(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, "pending-budget-0"))
	require.False(t, lifecycleDeletionMatches(c, healthy), "the only healthy group cannot be deleted using stale Ready credit")
}

func TestPendingPodUpdateDoesNotMergeReadyAndNewFailureEpisodes(t *testing.T) {
	c, kube, ms, old, pod := pendingUpdateFixture(t)
	ms.Spec.RecoveryPolicy = api.RoleRecreate
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](3600)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms.DeepCopy()))
	ctx, cancel := context.WithCancel(context.Background())
	c.recoveryCtx = ctx
	t.Cleanup(cancel)
	recoveryKey := getPodGracePeriodKey(pod)
	oldStarted := time.Now().Add(-time.Minute)
	c.graceMap.Store(recoveryKey, oldStarted)
	c.updatePod(old, pod)
	observed, ok := c.graceMap.Load(recoveryKey)
	require.True(t, ok)
	require.Equal(t, oldStarted, observed, "ordinary Pending is not a Ready observation")
	require.Len(t, kube.Actions(), 1, "the first update must exercise the fast path")

	ready := pod.DeepCopy()
	ready.ResourceVersion = "rv-ready"
	ready.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	updateRecoveryPod(t, c, ready)
	c.updatePod(pod, ready)
	_, ok = c.graceMap.Load(recoveryKey)
	require.False(t, ok, "even a brief observed Ready must end the previous failure")

	failed, err := kube.CoreV1().Pods(pod.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	failed.ResourceVersion = "rv-failed-again"
	failed.Status.Conditions[0].Status = corev1.ConditionFalse
	failed.Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", RestartCount: 1}}
	updateRecoveryPod(t, c, failed)
	c.updatePod(ready, failed)
	newStarted, ok := c.graceMap.Load(recoveryKey)
	require.True(t, ok)
	require.True(t, newStarted.(time.Time).After(oldStarted))
	c.handlePodAfterGraceTime(ms, pod, oldStarted)
	stillPending, ok := c.graceMap.Load(recoveryKey)
	require.True(t, ok)
	require.Equal(t, newStarted, stillPending, "old grace task cannot erase the new episode")
	require.False(t, lifecycleDeletionMatches(c, pod))
}

type pendingRetryQueue struct {
	workqueue.RateLimitingInterface
	keys   []interface{}
	delays []time.Duration
}

func (q *pendingRetryQueue) AddAfter(key interface{}, delay time.Duration) {
	q.keys, q.delays = append(q.keys, key), append(q.delays, delay)
}

func TestPendingPodUpdateReadFailureRetainsRetry(t *testing.T) {
	for _, outcome := range []string{"notfound", "error", "uid"} {
		t.Run(outcome, func(t *testing.T) {
			c, kube, ms, old, pod := pendingUpdateFixture(t)
			q := &pendingRetryQueue{RateLimitingInterface: c.workqueue}
			c.workqueue = q
			kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
				switch outcome {
				case "notfound":
					return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod.Name)
				case "error":
					return true, nil, fmt.Errorf("injected read failure")
				default:
					live := pod.DeepCopy()
					live.UID = "new-uid"
					return true, live, nil
				}
			})
			unlock := c.lifecycle.lock(ms.Namespace + "/" + ms.Name)
			defer unlock()
			// Invoke the helper so failures are unambiguously attributed to the
			// preflight; the held lock prevents any lifecycle work being needed.
			require.True(t, c.enqueuePendingPodUpdate(old, pod))
			require.Equal(t, []interface{}{ms.Namespace + "/" + ms.Name}, q.keys)
			require.Equal(t, []time.Duration{enqueueAfter}, q.delays)
			require.Len(t, kube.Actions(), 1)
			require.Zero(t, q.Len())
		})
	}
}
