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
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
)

func auditFixture(policy workloadv1alpha1.RecoveryPolicy, workers int32) (*workloadv1alpha1.ModelServing, []*corev1.Pod) {
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

func auditController(t *testing.T, ms *workloadv1alpha1.ModelServing, pods ...*corev1.Pod) (*ModelServingController, *kubefake.Clientset) {
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
	t.Cleanup(c.workqueue.ShutDown)
	return c, kube
}

func runAudit(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	key := utils.GetNamespaceName(ms).String()
	c.requestAudit(key, func(r *auditRequest) { r.live = true })
	require.NoError(t, c.reconcileModelServing(context.Background(), key))
}

func TestAuditHealthyRestartNeverDegradesOrDeletes(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			ms, pods := auditFixture(policy, 0)
			pods[0].Status.ContainerStatuses = []corev1.ContainerStatus{{Name: "main", Ready: true, RestartCount: 3}}
			c, kube := auditController(t, ms, pods...)
			for i := 0; i < 3; i++ {
				runAudit(t, c, ms)
			}
			require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
			for _, action := range kube.Actions() {
				require.False(t, action.Matches("delete", "pods"))
			}
			assertQueuedKey(t, c.workqueue, utils.GetNamespaceName(ms).String())
			require.NoError(t, c.reconcileModelServing(context.Background(), utils.GetNamespaceName(ms).String()))
			require.Zero(t, c.workqueue.Len(), "stable reconcile must not enqueue itself")
		})
	}
}

func TestAuditMissingWorkerHonorsRecoveryScope(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.NoneRestartPolicy, workloadv1alpha1.RoleRecreate, workloadv1alpha1.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			ms, pods := auditFixture(policy, 1)
			c, kube := auditController(t, ms, pods...)
			runAudit(t, c, ms)
			require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(context.Background(), pods[1].Name, metav1.DeleteOptions{}))
			kube.ClearActions()
			runAudit(t, c, ms)
			entryDeleted := false
			for _, action := range kube.Actions() {
				if d, ok := action.(kubetesting.DeleteAction); ok && action.Matches("delete", "pods") && d.GetName() == pods[0].Name {
					entryDeleted = true
					require.Equal(t, pods[0].UID, *d.GetDeleteOptions().Preconditions.UID)
				}
			}
			require.Equal(t, policy != workloadv1alpha1.NoneRestartPolicy, entryDeleted)
		})
	}
}

func TestAuditInitialIncompleteRoleOnlyFillsMissingPod(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.RoleRecreate, 1)
	c, kube := auditController(t, ms, pods[0])
	runAudit(t, c, ms)
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
	_, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), pods[1].Name, metav1.GetOptions{})
	require.NoError(t, err)
}

func TestAuditDoesNotOverwriteConcurrentInformerUpdate(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, kube := auditController(t, ms, pods...)
	kube.PrependReactor("list", "services", func(kubetesting.Action) (bool, runtime.Object, error) {
		newer := pods[0].DeepCopy()
		newer.ResourceVersion = "2"
		newer.Status.Conditions[0].Status = corev1.ConditionFalse
		require.NoError(t, c.podsInformer.GetIndexer().Update(newer))
		return false, nil, nil
	})
	runAudit(t, c, ms)
	got, err := c.podsLister.Pods(ms.Namespace).Get(pods[0].Name)
	require.NoError(t, err)
	require.Equal(t, "2", got.ResourceVersion)
	state, _ := c.audit.states.Load(utils.GetNamespaceName(ms).String())
	require.True(t, state.(*servingAuditState).live, "cache mismatch must keep subsequent reads authoritative")
}

func TestAuditPartialListFailurePreservesStoreAndRetryIntent(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, kube := auditController(t, ms, pods...)
	c.store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), "ms-0", "old", "rev", "hash", "prefill", "prefill-0")
	kube.PrependReactor("list", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		options := action.(kubetesting.ListActionImpl).ListOptions
		if options.Continue == "" {
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next"}, Items: []corev1.Pod{*pods[0]}}, nil
		}
		return true, nil, fmt.Errorf("page two unavailable")
	})
	key := utils.GetNamespaceName(ms).String()
	c.requestAudit(key, func(r *auditRequest) { r.live = true })
	require.ErrorContains(t, c.reconcileModelServing(context.Background(), key), "page two unavailable")
	n, err := c.store.GetRunningPodNumByServingGroup(utils.GetNamespaceName(ms), "ms-0")
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.True(t, c.audit.requests[key].live)
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
}

func TestAuditUnknownHistoryBlocksRecovery(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.RoleRecreate, 0)
	pods[0].Labels[workloadv1alpha1.RevisionLabelKey] = "missing-history"
	c, kube := auditController(t, ms, pods...)
	key := utils.GetNamespaceName(ms).String()
	c.requestAudit(key, func(r *auditRequest) { r.live = true })
	require.Error(t, c.reconcileModelServing(context.Background(), key))
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
}

func TestAuditPeriodIncludesStoreOnlyKeysAndDoesNotCallAPI(t *testing.T) {
	ms, _ := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, kube := auditController(t, ms)
	ghost := types.NamespacedName{Namespace: "default", Name: "ghost"}
	c.store.BindModelServing(ghost, "old")
	kube.ClearActions()
	require.NoError(t, c.enqueuePeriodicAudit())
	require.Len(t, c.audit.requests, 2)
	require.True(t, c.audit.requests[ghost.String()].live)
	require.Empty(t, kube.Actions())
	require.NoError(t, c.reconcileModelServing(context.Background(), ghost.String()))
	require.NotContains(t, c.store.ListModelServings(), ghost)
}

func TestAuditHooksRetryWithoutPromotingFailedReady(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "audit-hooks", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	// Plugins do not identify Role templates but do affect ModelServing hashes.
	pods[0].Labels[workloadv1alpha1.RevisionLabelKey] = utils.ModelServingRevision(ms)
	c, _ := auditController(t, ms, pods...)
	p := &auditHookPlugin{fail: true, calls: make(map[string]int)}
	c.pluginsRegistry = plugins.NewRegistry()
	c.pluginsRegistry.Register("audit-hooks", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return p, nil })
	key := utils.GetNamespaceName(ms).String()
	c.requestAudit(key, func(r *auditRequest) { r.live = true })
	require.ErrorContains(t, c.reconcileModelServing(context.Background(), key), "hook unavailable")
	require.Equal(t, datastore.RoleCreating, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
	p.fail = false
	runAudit(t, c, ms)
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
	counts := map[string]int{}
	for k, v := range p.calls {
		counts[k] = v
	}
	runAudit(t, c, ms)
	require.Equal(t, counts, p.calls, "stable audit must not replay Pod lifecycle hooks")
}

type auditHookPlugin struct {
	plugins.DemoPlugin
	fail  bool
	calls map[string]int
}

func (p *auditHookPlugin) Name() string { return "audit-hooks" }
func (p *auditHookPlugin) OnPodRunning(context.Context, *plugins.HookRequest) error {
	p.calls["running"]++
	return nil
}
func (p *auditHookPlugin) OnPodReady(context.Context, *plugins.HookRequest) error {
	p.calls["ready"]++
	if p.fail {
		return fmt.Errorf("hook unavailable")
	}
	return nil
}
func (p *auditHookPlugin) OnRoleSync(context.Context, *plugins.HookRequest) error { return nil }

func TestAuditGraceChecksFreshReadiness(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.RoleRecreate, 0)
	c, kube := auditController(t, ms, pods...)
	state := newServingAuditState(ms)
	state.grace[pods[0].UID] = time.Now().Add(-time.Second)
	view := c.observationView(context.Background(), ms, &servingObservation{pods: newObservationIndexer(), services: newObservationIndexer()}, state)
	stale := pods[0].DeepCopy()
	stale.Status.Conditions[0].Status = corev1.ConditionFalse
	stale.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	require.NoError(t, view.reconcileObservedUnhealthyPod(context.Background(), ms, stale))
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
	require.Empty(t, state.grace)
}
