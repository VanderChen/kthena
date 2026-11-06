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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// legacyRevisionLabels returns the ModelServing revision and Role template hash a
// pre-canonical controller stamped on Pods: spew-based struct hashes of the Roles
// with replica and rollout fields removed.
func legacyRevisionLabels(ms *workloadv1alpha1.ModelServing, roleName string) (string, string) {
	roles := ms.DeepCopy().Spec.Template.Roles
	var roleHash string
	for i := range roles {
		roles[i].Replicas = nil
		roles[i].RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
		if roles[i].Name == roleName {
			roleHash = utils.Revision(roles[i])
		}
	}
	return utils.Revision(roles), roleHash
}

// seedLegacyWorkload writes what a pre-canonical controller left behind: Running
// Pods labelled with legacy hashes and a Roles-only ControllerRevision. The
// datastore stays empty so the caller can replay a controller restart.
func seedLegacyWorkload(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) (legacyCR *appsv1.ControllerRevision, legacyHash string, originalUIDs map[string]types.UID) {
	t.Helper()
	ctx := context.Background()
	role := ms.Spec.Template.Roles[0]
	legacyHash, legacyRoleHash := legacyRevisionLabels(ms, role.Name)
	require.NotEqual(t, legacyHash, utils.ModelServingRevision(ms), "test requires the legacy and canonical hashes to differ")
	require.NotEqual(t, legacyRoleHash, utils.CalRoleTemplateHash(role), "test requires the legacy and canonical role hashes to differ")

	legacyCR, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, legacyHash, ms.Spec.Template.Roles)
	require.NoError(t, err)
	require.Empty(t, legacyCR.Annotations[utils.ControllerRevisionDataVersionAnnotation], "fixture must be a pre-canonical Roles-only revision")

	group := utils.GenerateServingGroupName(ms.Name, 0)
	roleID := utils.GenerateRoleID(role.Name, 0)
	originalUIDs = map[string]types.UID{}
	for worker := 0; worker <= int(role.WorkerReplicas); worker++ {
		var pod *corev1.Pod
		if worker == 0 {
			pod = utils.GenerateEntryPod(*role.DeepCopy(), ms, group, roleID, legacyHash, legacyRoleHash)
		} else {
			pod = utils.GenerateWorkerPod(*role.DeepCopy(), ms, group, roleID, worker, legacyHash, legacyRoleHash)
		}
		pod.UID = types.UID("legacy-" + pod.Name)
		pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Add(created))
		originalUIDs[created.Name] = created.UID
	}
	return legacyCR, legacyHash, originalUIDs
}

func podMutations(actions []clienttesting.Action) []string {
	var mutations []string
	for _, action := range actions {
		if action.GetResource().Resource != "pods" {
			continue
		}
		switch action.GetVerb() {
		case "create", "delete", "update", "patch", "delete-collection":
			mutations = append(mutations, action.GetVerb())
		}
	}
	return mutations
}

// TestUpgradeFromLegacyRevisionKeepsPodsAndMigratesHistory replays a controller
// upgrade end to end: Pods and a Roles-only ControllerRevision were written by the
// previous hash scheme, the datastore is empty (restart), and the spec is unchanged.
// The upgraded controller must rebuild its state, migrate the legacy history, and
// neither create nor delete a single Pod.
func TestUpgradeFromLegacyRevisionKeepsPodsAndMigratesHistory(t *testing.T) {
	strategies := []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate}
	for _, strategy := range strategies {
		for _, statusRecorded := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/status-recorded-%t", strategy, statusRecorded), func(t *testing.T) {
				ctx := context.Background()
				c := newAppliedHistoryController(t)
				ms := appliedHistoryWorkload(strategy, "image:v1", 1)
				legacyCR, legacyHash, originalUIDs := seedLegacyWorkload(t, c, ms)
				if statusRecorded {
					ms.Status.CurrentRevision, ms.Status.UpdateRevision = legacyHash, legacyHash
				}
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
				_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
				require.NoError(t, err)

				// Restart: the datastore is rebuilt from the live Pods carrying legacy labels.
				c.syncAll()
				key, group := utils.GetNamespaceName(ms), utils.GenerateServingGroupName(ms.Name, 0)
				require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(key, group))
				storedRevision, ok := c.store.GetServingGroupRevision(key, group)
				require.True(t, ok)
				require.Equal(t, legacyHash, storedRevision)

				client := c.kubeClientSet.(*kubefake.Clientset)
				client.ClearActions()
				for i := 0; i < 2; i++ {
					require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)), "sync %d", i)
				}
				require.Empty(t, podMutations(client.Actions()), "unchanged spec must not mutate Pods during upgrade")
				pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, len(originalUIDs))
				for _, pod := range pods.Items {
					require.Equal(t, originalUIDs[pod.Name], pod.UID)
					require.Equal(t, legacyHash, utils.ObjectRevision(&pod))
				}

				// History migration: the legacy revision is pinned and gains exactly one
				// canonical baseline carrying the same identity.
				crs, err := c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, crs.Items, 2, "migration adds one baseline and must not record a new identity")
				var baseline *appsv1.ControllerRevision
				for i := range crs.Items {
					cr := &crs.Items[i]
					require.Equal(t, legacyHash, cr.Labels[utils.ControllerRevisionRevisionLabelKey])
					if cr.Name == legacyCR.Name {
						require.NotEmpty(t, cr.Annotations["modelserving.volcano.sh/legacy-revision-baseline"], "legacy source must be pinned to its baseline")
						continue
					}
					baseline = cr
				}
				require.NotNil(t, baseline)
				require.Equal(t, utils.ControllerRevisionDataVersionV1, baseline.Annotations[utils.ControllerRevisionDataVersionAnnotation])

				updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, legacyHash, updated.Status.UpdateRevision, "desired identity must stay on the legacy hash")
				require.Equal(t, legacyHash, updated.Status.CurrentRevision)
				require.Equal(t, int32(1), updated.Status.UpdatedReplicas)
			})
		}
	}
}

// TestUpgradeFromLegacyRevisionStillRollsRealTemplateChange is the counterpart:
// the same legacy fixture with a changed image must roll, proving that the
// no-mutation assertion above comes from semantic equality, not from a paused
// rollout.
func TestUpgradeFromLegacyRevisionStillRollsRealTemplateChange(t *testing.T) {
	ctx := context.Background()
	c := newAppliedHistoryController(t)
	ms := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 1)
	_, legacyHash, _ := seedLegacyWorkload(t, c, ms)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = legacyHash, legacyHash
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err)

	c.syncAll()
	client := c.kubeClientSet.(*kubefake.Clientset)
	client.ClearActions()
	require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
	require.NotEmpty(t, podMutations(client.Actions()), "a real template change must still roll legacy Pods")
	updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NotEqual(t, legacyHash, updated.Status.UpdateRevision)
	require.Equal(t, utils.ModelServingRevision(ms), updated.Status.UpdateRevision)
}
