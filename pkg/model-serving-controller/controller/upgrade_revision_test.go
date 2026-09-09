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
	"time"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/client-go/util/workqueue"
	clocktesting "k8s.io/utils/clock/testing"
	"k8s.io/utils/ptr"
)

func podDeletionSelectors(c *ModelServingController) []string {
	var selectors []string
	for _, action := range c.kubeClientSet.(*kubefake.Clientset).Actions() {
		if action.GetResource().Resource == "pods" && action.GetVerb() == "delete-collection" {
			selectors = append(selectors, action.(kubetesting.DeleteCollectionAction).GetListRestrictions().Labels.String())
		}
	}
	return selectors
}

func TestMissingHistoryRecordFailureRetainsPreviousTarget(t *testing.T) {
	for _, fail := range []string{"read", "write"} {
		t.Run(fail, func(t *testing.T) {
			ctx := context.Background()
			old := createStandardModelServing("record-failure", 3, 1)
			old.UID = "owner"
			ms := old.DeepCopy()
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = "lost", "previous"
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "previous", old.Spec.Template.Roles)
			require.NoError(t, err)
			for i := 0; i < 3; i++ {
				addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], i, "lost")
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.PrependReactor("*", "controllerrevisions", func(a kubetesting.Action) (bool, runtime.Object, error) {
				if fail == "write" && a.GetVerb() == "update" {
					return true, nil, fmt.Errorf("injected write failure")
				}
				if fail == "read" && a.GetVerb() == "get" && a.(kubetesting.GetAction).GetName() == "record-failure-previous" {
					return true, nil, fmt.Errorf("injected read failure")
				}
				return false, nil, nil
			})
			require.Error(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
			require.Empty(t, podDeletionSelectors(c))
			stored, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "previous", stored.Status.UpdateRevision)
		})
	}
}

func TestRestoreRevisionDoesNotLabelUnobservedSpecAsHistory(t *testing.T) {
	ctx := context.Background()
	ms := createStandardModelServing("unobserved", 3, 1)
	ms.UID, ms.Generation = "owner", 2
	ms.Status.ObservedGeneration = 1
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "old", "old"
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	for i := 0; i < 3; i++ {
		addUpgradePod(t, c, ms, ms.Spec.Template.Roles[0], i, "old")
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, "old")
	require.NoError(t, err)
	require.Nil(t, cr)
	stale := ms.DeepCopy()
	stale.Generation = 1
	require.ErrorContains(t, c.UpdateModelServingStatus(stale, "old"), "generation changed")
}

func TestPartitionRecreatesMissingProtectedTemplate(t *testing.T) {
	ctx := context.Background()
	old := createStandardModelServing("protected", 3, 1)
	old.UID = "owner"
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "old", "new"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate, RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(1))}}
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "old", old.Spec.Template.Roles)
	require.NoError(t, err)
	for _, ordinal := range []int{1, 2} {
		addUpgradePod(t, c, ms, ms.Spec.Template.Roles[0], ordinal, "new")
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	require.NoError(t, c.scaleUpServingGroups(ctx, ms, groups, 3, "new"))
	created, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName("protected-0", "prefill-0", 0), metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image, created.Spec.Containers[0].Image)
}

func TestMissingHistoryUpgradeThenTemplateChange(t *testing.T) {
	for _, drift := range []string{"none", "group", "role", "both", "unrelated-hash-encoding"} {
		t.Run(drift, func(t *testing.T) {
			ctx := context.Background()
			ms := createStandardModelServing("deleted", 3, 1)
			ms.UID, ms.Generation = "owner", 3
			ms.Status.ObservedGeneration = 3
			target := utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles)
			oldRevision := target
			roleHash := utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0])
			if drift == "group" || drift == "both" || drift == "unrelated-hash-encoding" {
				oldRevision = "lost-" + drift
			}
			if drift == "role" || drift == "both" || drift == "unrelated-hash-encoding" {
				roleHash = "old-role-" + drift
			}
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = oldRevision, oldRevision
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate,
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(1))}}
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			for i := 0; i < 3; i++ {
				addUpgradePodWithHash(t, c, ms, ms.Spec.Template.Roles[0], i, oldRevision, roleHash)
			}
			require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
			require.Empty(t, podDeletionSelectors(c), "upgrade must not roll Pods for any hash drift")
			cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, target)
			require.NoError(t, err)
			require.NotNil(t, cr)
			roles, err := utils.GetRolesFromControllerRevision(cr)
			require.NoError(t, err)
			require.True(t, utils.EqualRoleTemplates(roles, ms.Spec.Template.Roles))
			if oldRevision != target {
				oldCR, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, oldRevision)
				require.NoError(t, err)
				require.Nil(t, oldCR, "current spec must not be assigned to a lost historical identity")
			}

			changed, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			changed.Generation++
			changed.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, changed, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(changed))
			c.kubeClientSet.(*kubefake.Clientset).ClearActions()
			require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
			require.Len(t, podDeletionSelectors(c), 1, "a later real update must roll")
			require.NotContains(t, podDeletionSelectors(c)[0], "name=deleted-0", "partition must retain the first group")
		})
	}
}

func TestMissingHistoryRolloutSurvivesRestartAndPartition(t *testing.T) {
	ctx := context.Background()
	old := createStandardModelServing("missing", 3, 1)
	old.UID = "owner"
	ms := old.DeepCopy()
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "lost", "before-change"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(1))}}
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "before-change", old.Spec.Template.Roles)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], i, "lost")
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	require.Len(t, podDeletionSelectors(c), 1)
	target := utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles)
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, target)
	require.NoError(t, err)
	require.Equal(t, "lost", cr.Annotations[rolloutFromRevisionsAnnotation])

	// A new controller has only persisted history and the next observed Pod set.
	ms.Status.UpdateRevision = target
	restarted := newUpgradeController(t, ms)
	defer restarted.workqueue.ShutDown()
	_, err = restarted.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Create(ctx, cr.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		addUpgradePod(t, restarted, ms, old.Spec.Template.Roles[0], i, "lost")
	}
	addUpgradePod(t, restarted, ms, ms.Spec.Template.Roles[0], 2, target)
	require.NoError(t, restarted.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	deletes := podDeletionSelectors(restarted)
	require.Len(t, deletes, 1)
	require.Contains(t, deletes[0], "missing-1")
	require.NotContains(t, deletes[0], "missing-0")
	// An unrelated missing revision is not covered by the persisted decision.
	h := restarted.templates(ctx, ms)
	_, err = h.groupMatches(ctx, datastore.ServingGroup{Name: "missing-0", Revision: "unrelated"}, target)
	require.True(t, apierrors.IsNotFound(err))
}

func TestReplicaAndPartitionChangesDoNotAuthorizeMissingHistory(t *testing.T) {
	for _, change := range []string{"none", "servinggroup-replicas", "role-replicas", "partition"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			old := createStandardModelServing("unchanged", 3, 1)
			old.UID = "owner"
			ms := old.DeepCopy()
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = "lost", "observed"
			switch change {
			case "servinggroup-replicas":
				ms.Spec.Replicas = ptr.To(int32(4))
			case "role-replicas":
				ms.Spec.Template.Roles[0].Replicas = ptr.To(int32(2))
			case "partition":
				ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate, RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(1))}}
			}
			c := newUpgradeController(t, ms)
			defer c.workqueue.ShutDown()
			_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "observed", old.Spec.Template.Roles)
			require.NoError(t, err)
			addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], 0, "lost")
			h := c.templates(ctx, ms)
			target := h.desiredRevision(ctx, utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles))
			require.NoError(t, c.ensureControllerRevision(ctx, ms, target))
			require.NoError(t, h.recordMissingRevisionRollout(ctx, target))
			require.False(t, h.canReplaceMissingRevision(ctx, "lost"))
		})
	}
}

func TestMissingHistoryDoesNotTrustLegacyObservedGeneration(t *testing.T) {
	ctx := context.Background()
	old := createStandardModelServing("legacy-status", 3, 1)
	old.UID = "owner"
	oldRevision := utils.Revision(utils.RemoveRoleReplicasForRevision(old).Spec.Template.Roles)
	ms := old.DeepCopy()
	ms.Generation, ms.Status.ObservedGeneration = 2, 2
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = oldRevision, oldRevision
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	for i := 0; i < 3; i++ {
		addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], i, oldRevision)
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, oldRevision)
	require.NoError(t, err)
	require.Nil(t, cr, "a stale observedGeneration cannot recreate the old template")
	stored, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Zero(t, stored.Status.UpdatedReplicas, "old Pods must not be counted as updated")
	require.Empty(t, podDeletionSelectors(c))
}

func TestProtectedMalformedHistoryDoesNotBlockOtherGroups(t *testing.T) {
	ctx := context.Background()
	old := createStandardModelServing("protected-error", 3, 1)
	old.UID = "owner"
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed:image"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(1))}}
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "bad", "good"
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "good", old.Spec.Template.Roles)
	require.NoError(t, err)
	_, err = utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "bad", map[string]string{"unexpected": "value"})
	require.NoError(t, err)
	addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], 0, "bad")
	for i := 1; i < 3; i++ {
		addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], i, "good")
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	deletes := podDeletionSelectors(c)
	require.Len(t, deletes, 1)
	require.Contains(t, deletes[0], "protected-error-2")
}

func TestMissingHistoryRoleRolloutOnlyReplacesChangedRole(t *testing.T) {
	ctx := context.Background()
	old := createStandardModelServing("role-scope", 1, 1)
	old.UID = "owner"
	decode := old.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, *decode)
	ms := old.DeepCopy()
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "lost", "before-change"
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "changed:image"
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	c := newUpgradeController(t, ms)
	defer c.workqueue.ShutDown()
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "before-change", old.Spec.Template.Roles)
	require.NoError(t, err)
	for _, role := range old.Spec.Template.Roles {
		addUpgradePod(t, c, ms, role, 0, "lost")
	}
	require.NoError(t, c.syncModelServing(ctx, utils.GetNamespaceName(ms).String()))
	deletes := podDeletionSelectors(c)
	require.Len(t, deletes, 1)
	require.Contains(t, deletes[0], "decode")
	require.NotContains(t, deletes[0], "prefill")
}

func newUpgradeController(t *testing.T, ms *workloadv1alpha1.ModelServing) *ModelServingController {
	t.Helper()
	c, err := NewModelServingController(kubefake.NewSimpleClientset(), kthenafake.NewSimpleClientset(ms.DeepCopy()), nil, apiextfake.NewSimpleClientset())
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Add(ms))
	return c
}

func addUpgradePod(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role, ordinal int, revision string, roleIndices ...int) *corev1.Pod {
	t.Helper()
	return addUpgradePodWithHash(t, c, ms, role, ordinal, revision, "legacy-role-hash", roleIndices...)
}

func addUpgradePodWithHash(t *testing.T, c *ModelServingController, ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role, ordinal int, revision, roleHash string, roleIndices ...int) *corev1.Pod {
	t.Helper()
	groupName := utils.GenerateServingGroupName(ms.Name, ordinal)
	roleIndex := 0
	if len(roleIndices) > 0 {
		roleIndex = roleIndices[0]
	}
	roleID := utils.GenerateRoleID(role.Name, roleIndex)
	pod := utils.GenerateEntryPod(*role.DeepCopy(), ms, groupName, roleIndex, revision, roleHash)
	pod.UID = types.UID(fmt.Sprintf("%s-uid", pod.Name))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
	key := utils.GetNamespaceName(ms)
	c.store.AddRunningPodToServingGroup(key, groupName, pod.Name, revision, roleHash, role.Name, roleID)
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

func TestRevisionFailureStillAllowsScaleDown(t *testing.T) {
	for _, groupLevel := range []bool{true, false} {
		for _, after := range []int32{0, 1, 3} {
			for _, fault := range []string{"different-template", "foreign-owner", "malformed"} {
				t.Run(fmt.Sprintf("group=%v/after=%d/%s", groupLevel, after, fault), func(t *testing.T) {
					old := createStandardModelServing("scale-history", 1, 1)
					old.UID = "owner"
					if groupLevel {
						old.Spec.Replicas = ptr.To[int32](2)
					} else {
						old.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
					}
					ms := old.DeepCopy()
					if groupLevel {
						ms.Spec.Replicas = ptr.To(after)
					} else {
						ms.Spec.Template.Roles[0].Replicas = ptr.To(after)
					}
					revision := utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles)
					ms.Status.CurrentRevision, ms.Status.UpdateRevision = revision, revision
					c := newUpgradeController(t, ms)
					defer c.workqueue.ShutDown()
					owner := ms.DeepCopy()
					data := interface{}(ms.Spec.Template.Roles)
					switch fault {
					case "different-template":
						owner.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "wrong-history"
						data = owner.Spec.Template.Roles
					case "foreign-owner":
						owner.UID = "previous-owner"
					case "malformed":
						data = "not-roles"
					}
					history, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, owner, revision, data)
					require.NoError(t, err)
					for group := 0; group < int(*old.Spec.Replicas); group++ {
						for role := 0; role < int(*old.Spec.Template.Roles[0].Replicas); role++ {
							addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], group, revision, role)
						}
					}
					client := c.kubeClientSet.(*kubefake.Clientset)
					client.ClearActions()
					require.ErrorContains(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name), "cannot ensure ControllerRevision")
					deletes := 0
					for _, action := range client.Actions() {
						if action.GetResource().Resource == "pods" {
							require.NotContains(t, []string{"create", "patch", "update"}, action.GetVerb())
							if action.GetVerb() == "delete-collection" {
								deletes++
							}
						}
					}
					require.Equal(t, max(0, 2-int(after)), deletes)
					retained, err := utils.GetControllerRevision(context.Background(), client, ms, revision)
					require.NoError(t, err)
					require.Equal(t, history, retained)
				})
			}
		}
	}
}

func TestUnresolvedRevisionRetriesAfterHistoryRecovery(t *testing.T) {
	ms := createStandardModelServing("retry-history", 1, 1)
	ms.UID = "owner"
	old := ms.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "updated-image"
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "legacy", "legacy"
	c := newUpgradeController(t, ms)
	c.workqueue.ShutDown()
	clock := clocktesting.NewFakeClock(time.Now())
	c.workqueue = workqueue.NewRateLimitingQueueWithConfig(workqueue.DefaultControllerRateLimiter(), workqueue.RateLimitingQueueConfig{Clock: clock})
	defer c.workqueue.ShutDown()
	addUpgradePod(t, c, ms, old.Spec.Template.Roles[0], 0, "legacy")
	require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "retry-history-0"))
	// Restoring history emits no ModelServing/Pod event. The delayed retry must
	// make progress without an external workload change or controller restart.
	_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "legacy", old.Spec.Template.Roles)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		clock.Step(time.Second)
		return c.workqueue.Len() > 0
	}, time.Second, 10*time.Millisecond)
	key, quit := c.workqueue.Get()
	require.False(t, quit)
	defer c.workqueue.Done(key)
	require.NoError(t, c.syncModelServing(context.Background(), key.(string)))
	require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "retry-history-0"))
}
