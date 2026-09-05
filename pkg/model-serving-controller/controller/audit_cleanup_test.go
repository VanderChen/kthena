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
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubetesting "k8s.io/client-go/testing"
)

type auditCleanupPlugin struct {
	plugins.DemoPlugin
	fail  bool
	calls []string
}

func (p *auditCleanupPlugin) Name() string                                           { return "audit-cleanup" }
func (p *auditCleanupPlugin) OnRoleSync(context.Context, *plugins.HookRequest) error { return nil }
func (p *auditCleanupPlugin) OnPodDelete(context.Context, *plugins.HookRequest) error {
	p.calls = append(p.calls, "pod")
	return nil
}
func (p *auditCleanupPlugin) OnRoleDelete(context.Context, *plugins.HookRequest) error {
	p.calls = append(p.calls, "role")
	if p.fail {
		return fmt.Errorf("cleanup unavailable")
	}
	return nil
}
func (p *auditCleanupPlugin) OnServingGroupDelete(context.Context, *plugins.HookRequest) error {
	p.calls = append(p.calls, "group")
	return nil
}

func TestAuditRetainsSoleDeletingRoleUntilCleanupSucceeds(t *testing.T) {
	ms, _ := auditFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "audit-cleanup", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	c, _ := auditController(t, ms)
	p := &auditCleanupPlugin{fail: true}
	c.pluginsRegistry = plugins.NewRegistry()
	c.pluginsRegistry.Register(p.Name(), func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return p, nil })
	key := utils.GetNamespaceName(ms)
	c.store.AddServingGroupAndRole(key, "ms-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]), "prefill", "prefill-0")
	require.NoError(t, c.store.UpdateRoleStatus(key, "ms-0", "prefill", "prefill-0", datastore.RoleDeleting))
	c.requestAudit(key.String(), func(r *auditRequest) { r.live = true })
	require.ErrorContains(t, c.reconcileModelServing(context.Background(), key.String()), "cleanup unavailable")
	require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "ms-0", "prefill", "prefill-0"))
	p.fail = false
	runAudit(t, c, ms)
	require.Equal(t, []string{"role", "role"}, p.calls)
	require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(key, "ms-0", "prefill", "prefill-0"))
}

func TestAuditSameNameUIDReplacementDoesNotDeleteNewResources(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.RoleRecreate, 0)
	c, kube := auditController(t, ms, pods...)
	runAudit(t, c, ms)
	newMS := ms.DeepCopy()
	newMS.UID = "new-ms-uid"
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), newMS, metav1.UpdateOptions{})
	require.NoError(t, err)
	newPod := pods[0].DeepCopy()
	newPod.UID = "new-pod-uid"
	newPod.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(newMS, workloadv1alpha1.ModelServingKind)}
	_, err = kube.CoreV1().Pods(ms.Namespace).Update(context.Background(), newPod, metav1.UpdateOptions{})
	require.NoError(t, err)
	kube.ClearActions()
	// Fake clients do not run garbage collection. The old immutable revision
	// must block adoption, then disappear just as it would under Kubernetes GC.
	c.requestAudit(utils.GetNamespaceName(ms).String(), func(r *auditRequest) { r.live = true })
	require.ErrorContains(t, c.reconcileModelServing(context.Background(), utils.GetNamespaceName(ms).String()), "not controlled by the current ModelServing UID")
	revisions, err := kube.AppsV1().ControllerRevisions(ms.Namespace).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for _, revision := range revisions.Items {
		if metav1.IsControlledBy(&revision, ms) {
			require.NoError(t, kube.AppsV1().ControllerRevisions(ms.Namespace).Delete(context.Background(), revision.Name, metav1.DeleteOptions{}))
		}
	}
	runAudit(t, c, newMS)
	// Simulate old tombstones arriving after adoption of the same name.
	c.queueChildObservation(pods[0], true)
	c.queueModelServingObservation(ms, nil)
	require.NoError(t, c.reconcileModelServing(context.Background(), utils.GetNamespaceName(ms).String()))
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
	value, _ := c.audit.states.Load(utils.GetNamespaceName(ms).String())
	require.Equal(t, newMS.UID, value.(*servingAuditState).uid)
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
}

func TestAuditLiveLatchSurvivesOrdinaryNotificationUntilCacheCatchesUp(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, kube := auditController(t, ms, pods...)
	stale := pods[0].DeepCopy()
	stale.ResourceVersion = "stale"
	stale.Status.Conditions[0].Status = corev1.ConditionFalse
	require.NoError(t, c.podsInformer.GetIndexer().Add(stale))
	runAudit(t, c, ms)
	kube.ClearActions()
	c.queueChildObservation(stale, false)
	require.NoError(t, c.reconcileModelServing(context.Background(), utils.GetNamespaceName(ms).String()))
	listed := false
	for _, action := range kube.Actions() {
		listed = listed || action.Matches("list", "pods")
	}
	require.True(t, listed)
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "ms-0", "prefill", "prefill-0"))
	require.NoError(t, c.podsInformer.GetIndexer().Update(pods[0]))
	runAudit(t, c, ms)
	value, _ := c.audit.states.Load(utils.GetNamespaceName(ms).String())
	require.False(t, value.(*servingAuditState).live)
}

func TestAuditDeleteCollectionUsesFreshUIDGuard(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.RoleRecreate, 0)
	c, kube := auditController(t, ms, pods...)
	kube.PrependReactor("delete", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		d := action.(kubetesting.DeleteAction)
		require.Equal(t, pods[0].UID, *d.GetDeleteOptions().Preconditions.UID)
		require.Equal(t, pods[0].ResourceVersion, *d.GetDeleteOptions().Preconditions.ResourceVersion)
		return true, nil, nil
	})
	observation, err := c.readObservation(context.Background(), ms, true)
	require.NoError(t, err)
	view := c.observationView(context.Background(), ms, observation, newServingAuditState(ms))
	require.NoError(t, view.kubeClientSet.CoreV1().Pods(ms.Namespace).DeleteCollection(context.Background(), metav1.DeleteOptions{}, metav1.ListOptions{}))
}
