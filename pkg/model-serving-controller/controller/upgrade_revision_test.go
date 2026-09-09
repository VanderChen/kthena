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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func newUpgradeController(t *testing.T, ms *workloadv1alpha1.ModelServing) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms.DeepCopy()), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	return c
}

func addUpgradePod(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role, ordinal int, revision string, roleIndices ...int) *corev1.Pod {
	t.Helper()
	groupName := utils.GenerateServingGroupName(ms.Name, ordinal)
	roleIndex := 0
	if len(roleIndices) > 0 {
		roleIndex = roleIndices[0]
	}
	roleID := utils.GenerateRoleID(role.Name, roleIndex)
	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, groupName, roleIndex, revision, "legacy-role-hash")
	pod.UID = types.UID(fmt.Sprintf("%s-uid", pod.Name))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	key := utils.GetNamespaceName(ms)
	c.store.AddRunningPodToServingGroup(key, groupName, pod.Name, revision, "legacy-role-hash", role.Name, roleID)
	require.NoError(t, c.store.UpdateRoleStatus(key, groupName, role.Name, roleID, datastore.RoleRunning))
	require.NoError(t, c.store.UpdateServingGroupStatus(key, groupName, datastore.ServingGroupRunning))
	return pod
}

func TestUpgradeReplicaChangesStillScale(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		for _, tc := range []struct {
			name          string
			groupLevel    bool
			before, after int32
		}{
			{"group-up", true, 1, 2}, {"group-down", true, 2, 1}, {"group-zero", true, 1, 0},
			{"role-up", false, 1, 2}, {"role-down", false, 2, 1}, {"role-zero", false, 1, 0},
			{"protected-role-up", false, 1, 2}, {"protected-role-down", false, 2, 1}, {"protected-role-zero", false, 1, 0},
		} {
			t.Run(string(strategy)+"/"+tc.name, func(t *testing.T) {
				old := createStandardModelServing("replicas", 1, 1)
				old.UID = "owner"
				if tc.groupLevel {
					old.Spec.Replicas = ptr.To(tc.before)
				} else {
					old.Spec.Template.Roles[0].Replicas = ptr.To(tc.before)
				}
				old.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: strategy}
				if strings.HasPrefix(tc.name, "protected-") {
					old.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromString("100%"))}
				}
				old.Status.CurrentRevision, old.Status.UpdateRevision = "legacy", "legacy"
				ms := old.DeepCopy()
				if tc.groupLevel {
					ms.Spec.Replicas = ptr.To(tc.after)
				} else {
					ms.Spec.Template.Roles[0].Replicas = ptr.To(tc.after)
				}
				before := ms.DeepCopy()
				c := newUpgradeController(t, ms)
				defer c.workqueue.ShutDown()
				_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "legacy", old.Spec.Template.Roles)
				require.NoError(t, err)
				for group := 0; group < int(*old.Spec.Replicas); group++ {
					for role := 0; role < int(*old.Spec.Template.Roles[0].Replicas); role++ {
						addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], group, "legacy", role)
					}
				}
				client := c.kubeClientSet.(*kubefake.Clientset)
				client.ClearActions()
				require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
				require.Equal(t, before, ms, "semantic comparison must not change the replica values used by scaling")
				creates, deletes := 0, 0
				createdNames := make(map[string]bool)
				for _, action := range client.Actions() {
					if action.GetResource().Resource != "pods" {
						continue
					}
					switch action.GetVerb() {
					case "create":
						name := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod).Name
						if !createdNames[name] {
							creates++
							createdNames[name] = true
						}
					case "delete-collection":
						deletes++
						if tc.after > 0 {
							selector := action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels.String()
							if tc.groupLevel {
								require.Contains(t, selector, "replicas-1")
							} else {
								require.Contains(t, selector, "prefill-1")
							}
						}
					case "delete", "patch", "update":
						t.Fatalf("unexpected Pod mutation: %s", action.GetVerb())
					}
				}
				if tc.after > tc.before {
					require.EqualValues(t, tc.after-tc.before, creates)
					require.Zero(t, deletes)
				} else {
					require.Zero(t, creates)
					require.EqualValues(t, tc.before-tc.after, deletes)
				}
			})
		}
	}
}

func TestUpgradeEquivalentHistoryRetainsPods(t *testing.T) {
	for _, strategy := range []workloadv1alpha1.RolloutStrategyType{"", workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			ms := createStandardModelServing("upgrade", 3, 1)
			ms.UID = "upgrade-owner"
			ms.Spec.RolloutStrategy = nil
			if strategy != "" {
				ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: strategy}
			}
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = "legacy", "legacy"
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			old, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "legacy", ms.Spec.Template.Roles)
			require.NoError(t, err)
			for _, ordinal := range []int{0, 3, 4} {
				addUpgradePod(t, c, ms, ms.Spec.Template.Roles[0], ordinal, "legacy")
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "pods" {
					require.NotContains(t, []string{"delete", "delete-collection", "patch", "update", "create"}, action.GetVerb(), "an equivalent upgrade must preserve all existing Pods")
				}
			}
			updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "legacy", updated.Status.UpdateRevision)
			require.EqualValues(t, 3, updated.Status.UpdatedReplicas)
			history, err := utils.GetControllerRevision(context.Background(), client, ms, "legacy")
			require.NoError(t, err)
			require.Equal(t, old.Data.Raw, history.Data.Raw)
			groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
			require.NoError(t, err)
			for _, group := range groups {
				require.Equal(t, "legacy", group.Revision)
			}
		})
	}
}

func TestUpgradeRealTemplateChangeAndUnknownHistory(t *testing.T) {
	for _, historyKind := range []string{"changed", "missing", "foreign", "malformed"} {
		t.Run(historyKind, func(t *testing.T) {
			ms := createStandardModelServing("history", 1, 1)
			ms.UID = "owner"
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate}
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			old := ms.DeepCopy()
			old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "old-image"
			if historyKind != "missing" {
				cr, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "legacy", old.Spec.Template.Roles)
				require.NoError(t, err)
				if historyKind == "foreign" {
					cr.OwnerReferences[0].UID = "someone-else"
				}
				if historyKind == "malformed" {
					cr.Data.Raw = []byte(`{"data":"broken"}`)
				}
				_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(context.Background(), cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			}
			addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], 0, "legacy")
			require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
			status := c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "history-0")
			if historyKind == "changed" {
				require.Equal(t, datastore.ServingGroupDeleting, status)
				// A genuine update must have persisted its target before deletion.
				target, err := utils.GetControllerRevision(context.Background(), c.kubeClientSet, ms, utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles))
				require.NoError(t, err)
				require.NotNil(t, target)
			} else {
				require.Equal(t, datastore.ServingGroupRunning, status)
				updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Zero(t, updated.Status.UpdatedReplicas)
			}
		})
	}
}

func TestUpgradePartialRoleUpdateUsesEachRolesHistory(t *testing.T) {
	ms := createStandardModelServing("partial", 1, 1)
	ms.UID = "owner"
	decode := *ms.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)
	old := ms.DeepCopy()
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "decode-new"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	for _, reversed := range []bool{false, true} {
		c := newUpgradeController(t, ms)
		defer c.workqueue.ShutDown()
		// Both histories describe the old workload but have independent observed identities.
		for _, rev := range []string{"prefill-history", "decode-history"} {
			_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, rev, old.Spec.Template.Roles)
			require.NoError(t, err)
		}
		order := []int{0, 1}
		if reversed {
			order = []int{1, 0}
		}
		for _, i := range order {
			addUpgradePod(t, c, ms, old.Spec.Template.Roles[i], 0, old.Spec.Template.Roles[i].Name+"-history")
		}
		groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
		require.NoError(t, err)
		originalIdentity := groups[0].Revision
		require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
		require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "partial-0", "prefill", "prefill-0"))
		require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(ms), "partial-0", "decode", "decode-0"))
		observed, _ := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), "partial-0")
		require.Equal(t, originalIdentity, observed)
	}
}

func TestUpgradeRoleScalingAndRestartRetainOldPod(t *testing.T) {
	old := createStandardModelServing("scale", 1, 1)
	old.UID = "owner"
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	for run := 0; run < 2; run++ {
		c := newUpgradeController(t, ms)
		defer c.workqueue.ShutDown()
		_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "legacy", old.Spec.Template.Roles)
		require.NoError(t, err)
		original := addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], 0, "legacy")
		client := c.kubeClientSet.(*kubefake.Clientset)
		client.ClearActions()
		require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
		for _, action := range client.Actions() {
			if action.GetResource().Resource == "pods" {
				require.NotContains(t, []string{"delete", "delete-collection", "patch", "update"}, action.GetVerb())
			}
		}
		pods, err := client.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
		require.NoError(t, err)
		require.Len(t, pods.Items, 2)
		retained, err := client.CoreV1().Pods(ms.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
		require.NoError(t, err)
		require.Equal(t, original.UID, retained.UID)
		require.Equal(t, original.Labels, retained.Labels)
	}
}

func TestEnsureControllerRevisionRejectsConflictingHistory(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(fmt.Sprintf("foreign=%v", foreign), func(t *testing.T) {
			ms := createStandardModelServing("conflict", 1, 1)
			ms.UID = "owner"
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			old := ms.DeepCopy()
			if foreign {
				old.UID = "another-owner"
			} else {
				old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "different-template"
			}
			ctx := context.Background()
			history, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, old, "existing", old.Spec.Template.Roles)
			require.NoError(t, err)
			require.Error(t, c.ensureControllerRevision(ctx, ms, "existing"))
			retained, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, "existing")
			require.NoError(t, err)
			require.Equal(t, history, retained)
		})
	}
}

func TestUpgradeHistoryFailurePreventsWorkloadMutation(t *testing.T) {
	ms := createStandardModelServing("failure", 1, 1)
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.PrependReactor("create", "controllerrevisions", func(action kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("injected history write failure")
	})
	require.ErrorContains(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name), "injected history write failure")
	for _, action := range client.Actions() {
		require.NotEqual(t, "pods", action.GetResource().Resource)
	}
}
