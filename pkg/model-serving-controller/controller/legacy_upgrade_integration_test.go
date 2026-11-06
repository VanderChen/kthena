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
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

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
	legacyHash, originalUIDs = seedLegacyPods(t, c, ms)
	legacyCR, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, legacyHash, ms.Spec.Template.Roles)
	require.NoError(t, err)
	require.Empty(t, legacyCR.Annotations[utils.ControllerRevisionDataVersionAnnotation], "fixture must be a pre-canonical Roles-only revision")
	return legacyCR, legacyHash, originalUIDs
}

// seedLegacyPods writes only the Running Pods of a pre-canonical controller, as
// left by releases that never persisted ControllerRevisions (kthena <= v0.2) or
// after history was deleted out of band.
func seedLegacyPods(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) (legacyHash string, originalUIDs map[string]types.UID) {
	t.Helper()
	ctx := context.Background()
	role := ms.Spec.Template.Roles[0]
	legacyHash, legacyRoleHash := legacyRevisionLabels(ms, role.Name)
	require.NotEqual(t, legacyHash, utils.ModelServingRevision(ms), "test requires the legacy and canonical hashes to differ")
	require.NotEqual(t, legacyRoleHash, utils.CalRoleTemplateHash(role), "test requires the legacy and canonical role hashes to differ")

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
	return legacyHash, originalUIDs
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

				// Downgrade to a release that garbage-collects the baseline (or an
				// operator deleting it), then upgrade again: the pinned baseline is
				// rebuilt instead of pausing the workload forever, and Pods stay put.
				events := record.NewFakeRecorder(32)
				c.recorder = events
				require.NoError(t, c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, baseline.Name, metav1.DeleteOptions{}))
				client.ClearActions()
				for i := 0; i < 2; i++ {
					require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)), "post-downgrade sync %d", i)
				}
				require.Empty(t, podMutations(client.Actions()), "a rebuilt baseline must not mutate Pods")
				rebuilt, err := c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, baseline.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotEmpty(t, rebuilt.Annotations[utils.LegacyRevisionBaselineRebuiltAnnotation])
				require.Equal(t, baseline.Data, rebuilt.Data, "unchanged spec must rebuild an identical baseline")
				require.Contains(t, drainEvents(events), "LegacyBaselineRebuilt")
			})
		}
	}
}

func drainEvents(recorder *record.FakeRecorder) string {
	var all []string
	for {
		select {
		case e := <-recorder.Events:
			all = append(all, e)
		default:
			return strings.Join(all, "\n")
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

// TestUpgradeWithoutAnyHistoryRebuildsRevisionAndKeepsPods covers workloads whose
// Pods reference a revision no ControllerRevision describes (releases before
// history was persisted, or history deleted out of band). With an unchanged spec
// the live Pods prove the template, so the controller rebuilds the missing
// revision under the running identity instead of pausing forever, and converges
// without touching a Pod.
func TestUpgradeWithoutAnyHistoryRebuildsRevisionAndKeepsPods(t *testing.T) {
	strategies := []workloadv1alpha1.RolloutStrategyType{workloadv1alpha1.ServingGroupRollingUpdate, workloadv1alpha1.RoleRollingUpdate}
	for _, strategy := range strategies {
		for _, statusRecorded := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/status-recorded-%t", strategy, statusRecorded), func(t *testing.T) {
				ctx := context.Background()
				c := newAppliedHistoryController(t)
				events := record.NewFakeRecorder(32)
				c.recorder = events
				ms := appliedHistoryWorkload(strategy, "image:v1", 1)
				legacyHash, originalUIDs := seedLegacyPods(t, c, ms)
				if statusRecorded {
					ms.Status.CurrentRevision, ms.Status.UpdateRevision = legacyHash, legacyHash
				}
				require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
				_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
				require.NoError(t, err)

				c.syncAll()
				client := c.kubeClientSet.(*kubefake.Clientset)
				client.ClearActions()
				for i := 0; i < 3; i++ {
					require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)), "sync %d", i)
				}
				require.Empty(t, podMutations(client.Actions()), "an unchanged spec must not mutate Pods when history is rebuilt")
				pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, pods.Items, len(originalUIDs))
				for _, pod := range pods.Items {
					require.Equal(t, originalUIDs[pod.Name], pod.UID)
					require.Equal(t, legacyHash, utils.ObjectRevision(&pod))
				}

				rebuilt, err := c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, utils.GenerateControllerRevisionName(ms.Name, legacyHash), metav1.GetOptions{})
				require.NoError(t, err, "the running identity must gain a ControllerRevision")
				require.Equal(t, utils.ControllerRevisionDataVersionV1, rebuilt.Annotations[utils.ControllerRevisionDataVersionAnnotation])
				require.Equal(t, fmt.Sprint(ms.Generation), rebuilt.Annotations[utils.RevisionRebuiltFromGenerationAnnotation])
				historical, err := utils.ModelServingForControllerRevision(ms, rebuilt)
				require.NoError(t, err)
				require.Equal(t, "image:v1", historical.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
				equal, err := utils.EqualModelServingRevisions(legacyHash, utils.ModelServingRevision(ms), historical, ms)
				require.NoError(t, err)
				require.True(t, equal, "rebuilt history must describe the current template")

				updated, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, legacyHash, updated.Status.UpdateRevision, "desired identity must settle on the running hash")
				require.Equal(t, legacyHash, updated.Status.CurrentRevision)
				require.Equal(t, int32(1), updated.Status.UpdatedReplicas)

				var rebuiltEvents int
				for len(events.Events) > 0 {
					if event := <-events.Events; strings.Contains(event, "RevisionRebuilt") {
						rebuiltEvents++
					}
				}
				require.Equal(t, 1, rebuiltEvents, "the rebuild must be reported exactly once")
			})
		}
	}
}

// TestUpgradeWithoutAnyHistoryStaysPausedWhenPodsDoNotProveTheTemplate is the
// safety counterpart: when the live Pods cannot prove they run the current spec
// the missing revision is not rebuilt, template-driven mutation stays paused,
// and the reason is actionable.
func TestUpgradeWithoutAnyHistoryStaysPausedWhenPodsDoNotProveTheTemplate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, legacyHash string)
		reason string
	}{
		{
			name: "image changed while history was missing",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
			},
			reason: "renders image:v2",
		},
		{
			name: "status names another running revision",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				ms.Status.CurrentRevision = "elsewhere"
			},
			reason: `status.currentRevision is "elsewhere"`,
		},
		{
			name: "env changed while history was missing",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				container := &ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0]
				container.Env = append(container.Env, corev1.EnvVar{Name: "NEW_FLAG", Value: "on"})
			},
			reason: "env NEW_FLAG differs",
		},
		{
			name: "worker replicas increased while history was missing",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				ms.Spec.Template.Roles[0].WorkerReplicas = 2
			},
			reason: "env GROUP_SIZE differs",
		},
		{
			name: "worker replicas decreased while history was missing",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				ms.Spec.Template.Roles[0].WorkerReplicas = 0
			},
			reason: "env GROUP_SIZE differs",
		},
		{
			name: "container removed while history was missing",
			mutate: func(_ *testing.T, _ *ModelServingController, ms *workloadv1alpha1.ModelServing, _ string) {
				ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers = append(ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers, corev1.Container{Name: "sidecar", Image: "image:v1"})
			},
			reason: "1 containers run but the template declares 2",
		},
		{
			name: "a second revision is running",
			mutate: func(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, legacyHash string) {
				// A desired second group keeps this a history question, not an explicit scale-down.
				ms.Spec.Replicas = ptr.To(int32(2))
				role := ms.Spec.Template.Roles[0]
				pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, utils.GenerateServingGroupName(ms.Name, 1), utils.GenerateRoleID(role.Name, 0), "other-"+legacyHash, "other")
				pod.Status = corev1.PodStatus{Phase: corev1.PodRunning}
				created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Add(created))
			},
			reason: "runs revision \"",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := newAppliedHistoryController(t)
			ms := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 1)
			legacyHash, originalUIDs := seedLegacyPods(t, c, ms)
			tc.mutate(t, c, ms, legacyHash)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
			_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
			require.NoError(t, err)

			c.syncAll()
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			err = c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
			require.ErrorContains(t, err, "ControllerRevision is missing")
			require.ErrorContains(t, err, tc.reason)
			for _, action := range podMutations(client.Actions()) {
				require.NotContains(t, []string{"create", "delete", "delete-collection"}, action, "unproven history must keep template-driven creation and deletion paused")
			}
			_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, utils.GenerateControllerRevisionName(ms.Name, legacyHash), metav1.GetOptions{})
			require.True(t, apierrors.IsNotFound(err), "an unproven revision must not be rebuilt")
			pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			for _, pod := range pods.Items {
				if uid, ok := originalUIDs[pod.Name]; ok {
					require.Equal(t, uid, pod.UID)
				}
			}
		})
	}
}

// TestSteadyStateRevisionLookupsAreBounded guards the per-sync ControllerRevision
// traffic: history is resolved once per round and a migrated legacy revision
// costs one baseline read, never a re-read of its source.
func TestSteadyStateRevisionLookupsAreBounded(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprintf("legacy-history-%t", legacy), func(t *testing.T) {
			ctx := context.Background()
			c := newAppliedHistoryController(t)
			ms := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 1)
			ms.Spec.Replicas = ptr.To(int32(4))
			if legacy {
				seedLegacyWorkload(t, c, ms)
			}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
			_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
			require.NoError(t, err)
			c.syncAll()
			for i := 0; i < 3; i++ {
				require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)), "warm-up sync %d", i)
			}

			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			var lists, gets, writes int
			for _, action := range client.Actions() {
				if action.GetResource().Resource != "controllerrevisions" {
					continue
				}
				switch action.GetVerb() {
				case "list":
					lists++
				case "get":
					gets++
				default:
					writes++
				}
			}
			require.LessOrEqual(t, lists, 2, "history must be listed at most twice per sync regardless of group count")
			require.Zero(t, writes, "an unchanged spec must not write history")
			if legacy {
				require.Equal(t, 1, gets, "a migrated legacy revision costs exactly one baseline read")
			} else {
				require.Zero(t, gets)
			}
		})
	}
}

// reloadFromAPI makes the next sync observe the status the previous sync wrote,
// as a real informer would.
func reloadFromAPI(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.ModelServing {
	t.Helper()
	latest, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
	return latest
}

// TestUpgradeWithoutAnyHistoryKeepsStatusEvidenceAcrossSyncs covers a pre-upgrade
// rollout the Pods never received: status.updateRevision already names another
// hash while every Pod still runs the old one. Recording the new identity in
// status would erase that evidence after one sync and let the next sync adopt
// the pending template as "already running"; the status must stay frozen and
// the rebuild must keep being refused.
func TestUpgradeWithoutAnyHistoryKeepsStatusEvidenceAcrossSyncs(t *testing.T) {
	ctx := context.Background()
	c := newAppliedHistoryController(t)
	ms := appliedHistoryWorkload(workloadv1alpha1.ServingGroupRollingUpdate, "image:v1", 1)
	legacyHash, originalUIDs := seedLegacyPods(t, c, ms)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = legacyHash, "pending"
	container := &ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: "NEW_FLAG", Value: "on"})
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Create(ctx, ms, metav1.CreateOptions{})
	require.NoError(t, err)
	c.syncAll()

	client := c.kubeClientSet.(*kubefake.Clientset)
	for i := 0; i < 3; i++ {
		client.ClearActions()
		err := c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
		require.ErrorContains(t, err, "ControllerRevision is missing", "sync %d", i)
		for _, action := range podMutations(client.Actions()) {
			require.NotContains(t, []string{"create", "delete", "delete-collection"}, action, "sync %d", i)
		}
		latest := reloadFromAPI(t, c, ms)
		require.Equal(t, legacyHash, latest.Status.CurrentRevision, "sync %d must not advance currentRevision", i)
		require.Equal(t, "pending", latest.Status.UpdateRevision, "sync %d must not erase the pending identity", i)
	}
	_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, utils.GenerateControllerRevisionName(ms.Name, legacyHash), metav1.GetOptions{})
	require.True(t, apierrors.IsNotFound(err), "a pending rollout must never be adopted as running")
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	for _, pod := range pods.Items {
		require.Equal(t, originalUIDs[pod.Name], pod.UID)
	}

	// Reverting the spec alone is not enough: the recorded pending identity is
	// evidence the operator must clear deliberately (status subresource).
	reverted := reloadFromAPI(t, c, ms).DeepCopy()
	reverted.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = nil
	_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, reverted, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(reverted))
	err = c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name))
	require.ErrorContains(t, err, `status.updateRevision "pending" is not the running revision`)
	require.Equal(t, "pending", reloadFromAPI(t, c, ms).Status.UpdateRevision)

	// Clearing the pending identity (the Pods now prove the template) lets
	// recovery proceed on the next sync, and the frozen status resumes normally.
	fixed := reloadFromAPI(t, c, ms).DeepCopy()
	fixed.Status.UpdateRevision = legacyHash
	_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).UpdateStatus(ctx, fixed, metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(fixed))
	for i := 0; i < 3; i++ {
		require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)), "recovery sync %d", i)
		reloadFromAPI(t, c, ms)
	}
	_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, utils.GenerateControllerRevisionName(ms.Name, legacyHash), metav1.GetOptions{})
	require.NoError(t, err, "the rebuilt revision must exist once the Pods prove the template")
	latest := reloadFromAPI(t, c, ms)
	require.Equal(t, legacyHash, latest.Status.UpdateRevision)
	require.Equal(t, legacyHash, latest.Status.CurrentRevision)
}
