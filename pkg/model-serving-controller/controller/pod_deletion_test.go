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
	"sort"
	"sync"
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
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestPodDeletionPlanIsProcessLocal(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("restart-fact", 1, 1, 0)
	ms.ResourceVersion = "1"
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	c := lifecycleController(t, ms)
	entry := lifecyclePod(t, c, ms, ms, 0, "entry-original")
	role := ms.Spec.Template.Roles[0]
	worker := utils.GenerateWorkerPod(*role.DeepCopy(), ms, "restart-fact-0", "prefill-0", 1, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
	worker.UID = "worker-original"
	kube := c.kubeClientSet.(*kubefake.Clientset)
	_, err := kube.CoreV1().Pods(ms.Namespace).Create(ctx, worker, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(worker))

	plan, err := c.preparePodDeletionPlan(ctx, ms, labels.Set{coreGroupLabel(): "restart-fact-0"}.AsSelector(), deleteGroupScope)
	require.NoError(t, err)
	require.Len(t, plan.Pods, 2)
	kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		if a.(kubetesting.DeleteAction).GetName() == worker.Name {
			return true, nil, apierrors.NewServiceUnavailable("injected partial deletion")
		}
		return false, nil, nil
	})
	require.Error(t, c.deletePlannedPods(ctx, ms, plan))
	require.True(t, plan.Started.Load())
	_, err = kube.CoreV1().Pods(ms.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err))

	// A restarted controller has no old batch to resume. The surviving member
	// is current fact and must not receive another DELETE from the old plan.
	c.deletionPlans = sync.Map{}
	c.store = datastore.New()
	kube.ClearActions()
	require.NoError(t, c.resumePodDeletionPlans(ctx, ms))
	got, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, worker.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, worker.UID, got.UID)
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"))
	}
}

func TestPodDeletionPlanNeverAbsorbsReplacementUID(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("finite", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	original := lifecyclePod(t, c, ms, ms, 0, "original")
	plan, err := c.preparePodDeletionPlan(ctx, ms, labels.SelectorFromSet(original.Labels), deleteRoleScope)
	require.NoError(t, err)

	replacement := original.DeepCopy()
	replacement.UID = "replacement"
	kube := c.kubeClientSet.(*kubefake.Clientset)
	require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
	kube.ClearActions()
	require.NoError(t, c.deletePlannedPods(ctx, ms, plan))
	handled, err := c.finishDeletionWithReplacement(ctx, ms, plan)
	require.NoError(t, err)
	require.True(t, handled)
	got, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, replacement.UID, got.UID)
	for _, action := range kube.Actions() {
		require.False(t, action.Matches("delete", "pods"), "the replacement UID is outside the original plan")
	}
}

func TestPodDeletionPlanUsesUIDPrecondition(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("uid-delete", 1, 1, 0)
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "original")
	plan, err := c.preparePodDeletionPlan(ctx, ms, labels.SelectorFromSet(pod.Labels), deleteRoleScope)
	require.NoError(t, err)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		action := a.(kubetesting.DeleteAction)
		require.NotNil(t, action.GetDeleteOptions().Preconditions)
		require.Equal(t, pod.UID, *action.GetDeleteOptions().Preconditions.UID)
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("UID changed"))
	})
	require.True(t, apierrors.IsConflict(c.deletePlannedPods(ctx, ms, plan)))
	require.False(t, plan.Started.Load(), "a definite rejection does not start the plan")
}

func TestPodDeletionPlanClearsCreatedFactsBeforeDelete(t *testing.T) {
	ctx := context.Background()
	ms := lifecycleMS("facts", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "entry")
	require.NoError(t, c.markRoleCreated(ctx, ms, "facts-0", "prefill", "prefill-0"))
	_, err := c.preparePodDeletionPlan(ctx, ms, labels.SelectorFromSet(pod.Labels), deleteRoleScope)
	require.NoError(t, err)
	cm, err := c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, utils.GroupMembersStateName(ms), metav1.GetOptions{})
	require.NoError(t, err)
	facts, err := readCreatedRoles(cm)
	require.NoError(t, err)
	require.Empty(t, facts)
	deletes := 0
	for _, action := range c.kubeClientSet.(*kubefake.Clientset).Actions() {
		if action.Matches("delete", "pods") {
			deletes++
		}
	}
	require.Zero(t, deletes, "preparing facts is not itself destructive")
}

func TestPartialServingGroupDeletionRestartRefillsMissingRoleAndKeepsSurvivor(t *testing.T) {
	ctx := context.Background()
	old := lifecycleMS("partial-restart", 1, 1, 0)
	old.ResourceVersion = "1"
	decode := *old.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, decode)
	ms := old.DeepCopy()
	ms.Spec.RecoveryPolicy = api.ServingGroupRecreate
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = intstrPtr(1)
	c := lifecycleController(t, ms, old)
	prefill := lifecyclePod(t, c, ms, old, 0, "prefill-old")
	decodeRole := *old.Spec.Template.Roles[1].DeepCopy()
	decodePod := utils.GenerateEntryPod(decodeRole, ms, "partial-restart-0", "decode-0", utils.ModelServingRevision(old), utils.CalRoleTemplateHash(decodeRole))
	decodePod.UID = "decode-old"
	decodePod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	kube := c.kubeClientSet.(*kubefake.Clientset)
	_, err := kube.CoreV1().Pods(ms.Namespace).Create(ctx, decodePod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Add(decodePod))
	c.store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), "partial-restart-0", decodePod.Name, utils.ModelServingRevision(old), utils.CalRoleTemplateHash(decodeRole), "decode", "decode-0")
	require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), "partial-restart-0", "decode", "decode-0", datastore.RoleRunning))
	require.NoError(t, c.setGroupMembers(ctx, ms, "partial-restart-0", utils.ModelServingRevision(old), old.Spec.Template.Roles))
	require.NoError(t, c.markRoleCreated(ctx, ms, "partial-restart-0", "prefill", "prefill-0"))
	require.NoError(t, c.markRoleCreated(ctx, ms, "partial-restart-0", "decode", "decode-0"))

	plan, err := c.preparePodDeletionPlan(ctx, ms, labels.Set{coreGroupLabel(): "partial-restart-0"}.AsSelector(), deleteGroupScope)
	require.NoError(t, err)
	// Make the partial boundary deterministic: prefill disappears, decode has
	// not yet accepted DELETE when the controller process stops.
	sort.Slice(plan.Pods, func(i, j int) bool {
		return plan.Pods[i].Name == prefill.Name && plan.Pods[j].Name != prefill.Name
	})
	kube.PrependReactor("delete", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		if a.(kubetesting.DeleteAction).GetName() == decodePod.Name {
			return true, nil, apierrors.NewServiceUnavailable("stop before decode DELETE")
		}
		return false, nil, nil
	})
	require.Error(t, c.deletePlannedPods(ctx, ms, plan))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(prefill))

	// Process restart: no deletion plan survives. Rebuild cache from live Pods,
	// then reconcile the missing protected Role from historical A.
	c.deletionPlans = sync.Map{}
	c.store = datastore.New()
	liveDecode, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, decodePod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(liveDecode))
	c.syncAll()
	kube.ReactionChain = kube.ReactionChain[1:]
	kube.ClearActions()
	require.NoError(t, c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name))

	gotDecode, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, decodePod.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, decodePod.UID, gotDecode.UID)
	restored, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, prefill.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, "test:v1", restored.Spec.Containers[0].Image, "partition-protected refill uses historical template")
	for _, action := range kube.Actions() {
		if action.Matches("delete", "pods") {
			require.NotEqual(t, decodePod.Name, action.(kubetesting.DeleteAction).GetName(), "restart must not replay the old SG batch")
		}
	}
}

func intstrPtr(value int) *intstr.IntOrString {
	v := intstr.FromInt(value)
	return &v
}

func coreGroupLabel() string {
	return "modelserving.volcano.sh/group-name"
}
