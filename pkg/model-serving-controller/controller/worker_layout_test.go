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
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func layoutController(t *testing.T, before, after int32, command bool, partition int, strategy api.RolloutStrategyType) (*ModelServingController, *api.ModelServing, *api.ModelServing) {
	t.Helper()
	old := createStandardModelServing("layout", 3, 1)
	old.UID = "layout-owner"
	role := &old.Spec.Template.Roles[0]
	role.WorkerReplicas = before
	role.EntryTemplate.Spec.Containers[0].Command = []string{"sh", "-c", "sleep 86400"}
	role.WorkerTemplate = role.EntryTemplate.DeepCopy()
	old.Spec.RolloutStrategy = &api.RolloutStrategy{Type: strategy, RollingUpdateConfiguration: &api.RollingUpdateConfiguration{
		MaxUnavailable: ptr.To(intstr.FromInt(1)), Partition: ptr.To(intstr.FromInt(partition)),
	}}
	old.Status.CurrentRevision, old.Status.UpdateRevision = "old", "old"
	ms := old.DeepCopy()
	ms.Spec.Template.Roles[0].WorkerReplicas = after
	if command {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Command[2] = "sleep 86399"
		ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Command[2] = "sleep 86399"
	}
	c := newUpgradeController(t, ms)
	t.Cleanup(c.workqueue.ShutDown)
	_, err := utils.CreateControllerRevision(context.Background(), c.kubeClientSet, ms, "old", old.Spec.Template.Roles)
	require.NoError(t, err)
	for group := 0; group < 3; group++ {
		entry := addUpgradePod(t, c, ms, *role, group, "old")
		for worker := 1; worker <= int(before); worker++ {
			pod := utils.GenerateWorkerPod(*role.DeepCopy(), old, entry, utils.GenerateServingGroupName(ms.Name, group), 0, worker, "old", "legacy-role-hash")
			pod.Status = entry.Status
			addLayoutPod(t, c, pod)
		}
	}
	return c, old, ms
}

func addLayoutPod(t *testing.T, c *ModelServingController, pod *corev1.Pod) {
	t.Helper()
	require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
	_, err := c.kubeClientSet.CoreV1().Pods(pod.Namespace).Create(context.Background(), pod.DeepCopy(), metav1.CreateOptions{})
	require.NoError(t, err)
}

func TestWorkerLayoutRolloutKeepsOldInstances(t *testing.T) {
	for _, strategy := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, change := range []struct {
			before, after int32
			command       bool
		}{{1, 1, true}, {0, 1, false}, {1, 2, false}, {1, 2, true}, {2, 1, true}, {1, 0, true}} {
			for _, partition := range []int{0, 1, 3} {
				t.Run(fmt.Sprintf("%s/%d-%d/command-%t/partition-%d", strategy, change.before, change.after, change.command, partition), func(t *testing.T) {
					c, old, ms := layoutController(t, change.before, change.after, change.command, partition, strategy)
					client := c.kubeClientSet.(*kubefake.Clientset)
					client.ClearActions()
					require.NoError(t, c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name))
					deletes := 0
					for _, action := range client.Actions() {
						if action.GetResource().Resource != "pods" {
							continue
						}
						require.NotEqual(t, "create", action.GetVerb(), "old groups must not receive target-layout pods before rollout")
						if action.GetVerb() == "delete-collection" {
							deletes++
						}
					}
					if partition == 3 {
						require.Zero(t, deletes)
					} else {
						require.Equal(t, 1, deletes)
					}
					for i := 0; i < 3; i++ {
						group := utils.GenerateServingGroupName(ms.Name, i)
						ready, err := c.checkRoleReady(ms, group, "prefill", "prefill-0")
						require.NoError(t, err)
						require.True(t, ready, "healthy old layout must remain Ready, including after restart")
						if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group) == datastore.ServingGroupDeleting {
							continue
						}
						pgTemplate, err := c.templates(context.Background(), ms).podGroupModelServing(context.Background(), group)
						require.NoError(t, err)
						if c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, "prefill", "prefill-0") == datastore.RoleDeleting {
							require.Equal(t, ms.Spec.Template.Roles, pgTemplate.Spec.Template.Roles, "selected Role replacement uses the target requirements")
						} else {
							require.Equal(t, old.Spec.Template.Roles, pgTemplate.Spec.Template.Roles)
						}
					}
				})
			}
		}
	}
}

func TestWorkerLayoutRestoresMissingOldPods(t *testing.T) {
	for _, missing := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("pod-%d", missing), func(t *testing.T) {
			c, old, ms := layoutController(t, 2, 1, true, 0, api.ServingGroupRollingUpdate)
			name := utils.GeneratePodName("layout-0", "prefill-0", missing)
			pod, err := c.podsLister.Pods(ms.Namespace).Get(name)
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
			require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(context.Background(), name, metav1.DeleteOptions{}))
			// An extra ordinal must not conceal a hole just because counts match.
			extra := pod.DeepCopy()
			extra.Name = utils.GeneratePodName("layout-0", "prefill-0", 9)
			addLayoutPod(t, c, extra)
			require.NoError(t, c.manageRole(context.Background(), ms, "new"))
			restored, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "old", utils.ObjectRevision(restored))
			require.Equal(t, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Command, restored.Spec.Containers[0].Command)
		})
	}
}

func TestWorkerLayoutReplicaGrowthUsesActiveTemplate(t *testing.T) {
	for _, strategy := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(strategy), func(t *testing.T) {
			c, old, ms := layoutController(t, 1, 2, true, 0, strategy)
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			require.NoError(t, c.manageRole(context.Background(), ms, "new"))
			for i := 0; i < 3; i++ {
				group := utils.GenerateServingGroupName(ms.Name, i)
				for j := 0; j <= 1; j++ {
					pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), utils.GeneratePodName(group, "prefill-1", j), metav1.GetOptions{})
					require.NoError(t, err)
					require.Equal(t, "old", utils.ObjectRevision(pod))
					require.Equal(t, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Command, pod.Spec.Containers[0].Command)
				}
				pgTemplate, err := c.templates(context.Background(), ms).podGroupModelServing(context.Background(), group)
				require.NoError(t, err)
				require.EqualValues(t, 2, *pgTemplate.Spec.Template.Roles[0].Replicas)
				require.EqualValues(t, 1, pgTemplate.Spec.Template.Roles[0].WorkerReplicas)
			}
		})
	}
}

func TestWorkerLayoutEmptyRoleRecoveryKeepsServingGroupTemplate(t *testing.T) {
	c, old, ms := layoutController(t, 1, 2, true, 0, api.ServingGroupRollingUpdate)
	ctx := context.Background()
	for i := 0; i <= 1; i++ {
		name := utils.GeneratePodName("layout-0", "prefill-0", i)
		pod, err := c.podsLister.Pods(ms.Namespace).Get(name)
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
		require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, name, metav1.DeleteOptions{}))
	}
	c.store.DeleteRole(utils.GetNamespaceName(ms), "layout-0", "prefill", "prefill-0")
	pgTemplate, err := c.templates(ctx, ms).podGroupModelServing(ctx, "layout-0")
	require.NoError(t, err)
	require.Equal(t, old.Spec.Template.Roles, pgTemplate.Spec.Template.Roles)
	require.NoError(t, c.manageRole(ctx, ms, "new"))
	pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, pods.Items, 6)
	for _, pod := range pods.Items {
		require.Equal(t, "old", utils.ObjectRevision(&pod))
	}
}

func TestWorkerLayoutPodGroupUsesEachRoleTypeAndCurrentGlobalSettings(t *testing.T) {
	c, _, ms := layoutController(t, 1, 2, true, 0, api.RoleRollingUpdate)
	ctx := context.Background()
	decode := *ms.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	decode.WorkerReplicas = 0
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, decode)
	ms.Spec.SchedulerName = "current-scheduler"
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, "new", ms.Spec.Template.Roles)
	require.NoError(t, err)
	addUpgradePod(t, c, ms, decode, 0, "new")
	pgTemplate, err := c.templates(ctx, ms).podGroupModelServing(ctx, "layout-0")
	require.NoError(t, err)
	require.Equal(t, "current-scheduler", pgTemplate.Spec.SchedulerName)
	require.EqualValues(t, 1, pgTemplate.Spec.Template.Roles[0].WorkerReplicas)
	require.EqualValues(t, 0, pgTemplate.Spec.Template.Roles[1].WorkerReplicas)
	require.Equal(t, decode, pgTemplate.Spec.Template.Roles[1])
}

func TestWorkerLayoutMixedPodsDoNotPromoteGroupRevision(t *testing.T) {
	c, _, ms := layoutController(t, 1, 2, true, 0, api.ServingGroupRollingUpdate)
	ctx := context.Background()
	revision := utils.Revision(utils.RemoveRoleReplicasForRevision(ms).Spec.Template.Roles)
	_, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, revision, ms.Spec.Template.Roles)
	require.NoError(t, err)
	entry, err := c.podsLister.Pods(ms.Namespace).Get("layout-0-prefill-0-0")
	require.NoError(t, err)
	role := ms.Spec.Template.Roles[0]
	worker := utils.GenerateWorkerPod(*role.DeepCopy(), ms, entry, "layout-0", 0, 2, revision, utils.CalRoleTemplateHash(role))
	worker.Status = entry.Status
	addLayoutPod(t, c, worker)
	// Reconstruct the datastore as if the new worker was observed first.
	c.store.DeleteServingGroup(utils.GetNamespaceName(ms), "layout-0")
	c.store.AddRunningPodToServingGroup(utils.GetNamespaceName(ms), "layout-0", worker.Name, revision, utils.CalRoleTemplateHash(role), role.Name, "prefill-0")
	ready, err := c.checkRoleReady(ms, "layout-0", "prefill", "prefill-0")
	require.NoError(t, err)
	require.False(t, ready)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	require.NoError(t, err)
	history := c.templates(ctx, ms)
	matches, err := history.groupMatches(ctx, groups[0], revision)
	require.NoError(t, err)
	require.False(t, matches, "all Pods must be compared even when cached Role and SG hashes match")
	pgTemplate, err := history.podGroupModelServing(ctx, "layout-0")
	require.NoError(t, err)
	require.EqualValues(t, 1, pgTemplate.Spec.Template.Roles[0].WorkerReplicas)
}

func TestWorkerLayoutUnresolvedHistoryBlocksRecoveryButNotScaleDown(t *testing.T) {
	for _, fault := range []string{"missing", "owner", "malformed", "read-error"} {
		t.Run(fault, func(t *testing.T) {
			c, _, ms := layoutController(t, 1, 2, true, 0, api.ServingGroupRollingUpdate)
			client := c.kubeClientSet.(*kubefake.Clientset)
			ctx := context.Background()
			name := utils.GenerateControllerRevisionName(ms.Name, "old")
			cr, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
			require.NoError(t, err)
			switch fault {
			case "missing":
				require.NoError(t, client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, name, metav1.DeleteOptions{}))
			case "owner":
				cr.OwnerReferences[0].UID = "someone-else"
				_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "malformed":
				cr.Data.Raw = []byte(`{"data":"invalid-roles"}`)
				_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "read-error":
				client.PrependReactor("get", "controllerrevisions", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("read unavailable")
				})
			}
			missing, err := c.podsLister.Pods(ms.Namespace).Get("layout-0-prefill-0-1")
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Delete(missing))
			client.ClearActions()
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			require.NoError(t, c.manageRole(ctx, ms, "new"))
			deletes := 0
			for _, a := range client.Actions() {
				if a.GetResource().Resource != "pods" {
					continue
				}
				require.NotEqual(t, "create", a.GetVerb())
				if a.GetVerb() == "delete-collection" {
					deletes++
				}
			}
			require.Equal(t, 3, deletes)
		})
	}
}

func TestWorkerLayoutPartitionKeepsLatestRoleReplicas(t *testing.T) {
	for _, strategy := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		for _, partition := range []int{1, 3} {
			for _, replicas := range []int32{0, 2} {
				t.Run(fmt.Sprintf("%s/partition-%d/replicas-%d", strategy, partition, replicas), func(t *testing.T) {
					c, old, ms := layoutController(t, 1, 2, true, partition, strategy)
					ms.Spec.Template.Roles[0].Replicas = ptr.To(replicas)
					ctx := context.Background()
					history := c.templates(ctx, ms)
					targets, err := history.groupRoleTargets(ctx, "layout-0")
					require.NoError(t, err)
					require.Equal(t, replicas, *targets[0].Replicas)
					require.EqualValues(t, 1, targets[0].WorkerReplicas)
					require.NoError(t, c.manageRole(ctx, ms, "new"))
					for i := 0; i < 3; i++ {
						group := utils.GenerateServingGroupName(ms.Name, i)
						if replicas == 0 {
							require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, "prefill", "prefill-0"))
							continue
						}
						pgTemplate, err := history.podGroupModelServing(ctx, group)
						require.NoError(t, err)
						require.Equal(t, replicas, *pgTemplate.Spec.Template.Roles[0].Replicas)
						require.EqualValues(t, 1, pgTemplate.Spec.Template.Roles[0].WorkerReplicas)
						for worker := 0; worker <= 1; worker++ {
							pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName(group, "prefill-1", worker), metav1.GetOptions{})
							require.NoError(t, err)
							require.Equal(t, "old", utils.ObjectRevision(pod))
							require.Equal(t, old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Command, pod.Spec.Containers[0].Command)
							pod.Status.Phase = corev1.PodRunning
							pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
							require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
						}
						require.NoError(t, c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group, "prefill", "prefill-1", datastore.RoleRunning))
						ready, err := c.checkServingGroupReady(ms, group)
						require.NoError(t, err)
						require.True(t, ready, "historical worker layout with latest Role replicas must become Ready")
					}
					stored, err := history.get(ctx, "old")
					require.NoError(t, err)
					require.EqualValues(t, 1, *stored[0].Replicas, "scaling must not rewrite the historical snapshot")
				})
			}
		}
	}
}

func TestWorkerLayoutProtectedGroupRecoveryUsesLatestRoleReplicas(t *testing.T) {
	c, _, ms := layoutController(t, 1, 2, true, 3, api.ServingGroupRollingUpdate)
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	ctx := context.Background()
	for worker := 0; worker <= 1; worker++ {
		name := utils.GeneratePodName("layout-0", "prefill-0", worker)
		pod, err := c.podsLister.Pods(ms.Namespace).Get(name)
		require.NoError(t, err)
		require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
		require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, name, metav1.DeleteOptions{}))
	}
	c.store.DeleteServingGroup(utils.GetNamespaceName(ms), "layout-0")
	require.NoError(t, c.manageServingGroupReplicas(ctx, ms, "new"))
	for role := 0; role < 2; role++ {
		for worker := 0; worker <= 1; worker++ {
			pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, utils.GeneratePodName("layout-0", utils.GenerateRoleID("prefill", role), worker), metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, "old", utils.ObjectRevision(pod))
		}
	}
}
