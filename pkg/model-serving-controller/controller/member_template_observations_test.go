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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

// Specification 2.11, API 6.1 and SG 0.5: local repair keeps the enclosing
// rollout unit's applied template; full-unit recreation selects history/target
// using the partition at creation time. Observe the first POST, since later
// legal rollout convergence cannot prove which template recovery first used.
func TestMemberObservationRecoveryFirstCreateUsesApplicableTemplate(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		mode                  api.RolloutStrategyType
		policy                api.RecoveryPolicy
		appliedV2, wantV2     bool
		beforePart, afterPart int
	}{
		{"role-unit-release-protection", api.RoleRollingUpdate, api.RoleRecreate, false, true, 1, 0},
		{"role-unit-increase-protection", api.RoleRollingUpdate, api.RoleRecreate, true, false, 0, 1},
		{"group-unit-release-protection", api.ServingGroupRollingUpdate, api.ServingGroupRecreate, false, true, 2, 1},
		{"group-unit-increase-protection", api.ServingGroupRollingUpdate, api.ServingGroupRecreate, true, false, 1, 2},
		{"local-role-retains-protected-v2", api.ServingGroupRollingUpdate, api.RoleRecreate, true, true, 1, 2},
		{"local-role-retains-historical-v1", api.ServingGroupRollingUpdate, api.RoleRecreate, false, false, 2, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			old := lifecycleMS("member-template", 2, 1, 0)
			old.ResourceVersion = "1"
			old.Spec.Template.Roles[0].WorkerReplicas = 1
			old.Spec.Template.Roles[0].WorkerTemplate = old.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			other := *old.Spec.Template.Roles[0].DeepCopy()
			other.Name = "other"
			old.Spec.Template.Roles = append(old.Spec.Template.Roles, other)
			ms := old.DeepCopy()
			ms.Generation++
			ms.Spec.RecoveryPolicy = tc.policy
			ms.Spec.RolloutStrategy.Type = tc.mode
			// Keep group 0 as the completed v1 baseline. In Role mode, protect
			// the unrelated Role too, so its normal rollout is not the subject.
			ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
			for i := range ms.Spec.Template.Roles {
				role := &ms.Spec.Template.Roles[i]
				role.EntryTemplate.Spec.Containers[0].Image = "test:v2"
				role.WorkerTemplate.Spec.Containers[0].Image = "test:v2"
				role.WorkerReplicas = 2
				role.MaxUnavailable = ptr.To(intstr.FromInt(1))
				role.Partition = ptr.To(intstr.FromInt(1))
			}
			setPartition := func(obj *api.ModelServing, partition int) {
				if tc.mode == api.RoleRollingUpdate {
					obj.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(partition))
				} else {
					obj.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(partition))
				}
			}
			setPartition(ms, tc.beforePart)
			ms.Status.CurrentRevision = utils.ModelServingRevision(old)
			ms.Status.UpdateRevision = utils.ModelServingRevision(ms)
			c := lifecycleController(t, ms, old)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			key, group := utils.GetNamespaceName(ms), ms.Name+"-1"
			applied, wanted := old, old
			if tc.appliedV2 {
				applied = ms
			}
			if tc.wantV2 {
				wanted = ms
			}
			var original, preserved []*corev1.Pod
			var missing *corev1.Pod
			for ordinal, template := range []*api.ModelServing{old, applied} {
				for _, role := range template.Spec.Template.Roles {
					groupName, id := utils.GenerateServingGroupName(ms.Name, ordinal), utils.GenerateRoleID(role.Name, 0)
					revision, hash := utils.ModelServingRevision(template), utils.CalRoleTemplateHash(role)
					for worker := 0; worker <= int(role.WorkerReplicas); worker++ {
						var pod *corev1.Pod
						if worker == 0 {
							pod = utils.GenerateEntryPod(*role.DeepCopy(), ms, groupName, id, revision, hash)
						} else {
							pod = utils.GenerateWorkerPod(*role.DeepCopy(), ms, groupName, id, worker, revision, hash)
						}
						pod.UID, pod.ResourceVersion = types.UID(pod.Name+"-original"), "10"
						setInstanceAnnotations(pod, map[string]string{groupInstanceAnnotation: groupName + "-original", roleInstanceAnnotation: groupName + "-" + id + "-original"})
						_, err := kube.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
						require.NoError(t, err)
						require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
						c.store.AddServingGroupAndRole(key, groupName, revision, hash, role.Name, id)
						if ordinal == 1 && (tc.policy == api.ServingGroupRecreate || role.Name == "prefill") {
							original = append(original, pod)
						} else {
							preserved = append(preserved, pod)
						}
						if ordinal == 1 && role.Name == "prefill" && worker == 0 {
							missing = pod
						}
					}
				}
			}
			// The member evidence outlives ordinary dirty consumption. No member
			// ever became Ready or completed CREATE in this fixture.
			for round := 0; round < 3; round++ {
				handled, err := c.processKnownMemberLosses(memberTestSnapshot(t, c, ms), ms)
				require.NoError(t, err)
				require.False(t, handled)
			}
			roles, err := c.store.GetRoleList(key, group, "prefill")
			require.NoError(t, err)
			require.Len(t, roles, 1)
			require.False(t, roles[0].Initialized)
			require.Equal(t, datastore.RoleCreating, roles[0].Status)
			memberTestRemove(t, c, missing) // deliberately no DELETE callback
			firstPosts := map[string]*corev1.Pod{}
			kube.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				pod := action.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
				pod.UID = types.UID(fmt.Sprintf("replacement-%s", pod.Name))
				if pod.Labels[api.GroupNameLabelKey] == group {
					for _, prior := range original {
						obj, getErr := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), ms.Namespace, prior.Name)
						if getErr == nil {
							require.NotEqual(t, prior.UID, obj.(*corev1.Pod).UID, "no refill before original recovery scope retires")
						} else {
							require.True(t, apierrors.IsNotFound(getErr), getErr)
						}
					}
					for _, prior := range preserved {
						obj, getErr := kube.Tracker().Get(corev1.SchemeGroupVersion.WithResource("pods"), ms.Namespace, prior.Name)
						require.NoError(t, getErr)
						require.Equal(t, prior.UID, obj.(*corev1.Pod).UID, "recovery must preserve scope-external UIDs through its first CREATE")
					}
					if firstPosts[pod.Name] == nil {
						firstPosts[pod.Name] = pod.DeepCopy()
					}
				}
				return false, nil, nil
			})
			require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			require.Empty(t, firstPosts, "the known-loss reconcile must stop after recovery handoff")
			for _, prior := range original {
				_, err := kube.CoreV1().Pods(ms.Namespace).Get(ctx, prior.Name, metav1.GetOptions{})
				require.True(t, apierrors.IsNotFound(err), "original recovery member %s must be deleted", prior.Name)
				// Deliver only informer storage convergence, without DELETE input.
				require.NoError(t, c.podsInformer.GetIndexer().Delete(prior))
			}
			// Change partition after physical retirement began and before the
			// first replacement. Reusing old partial-repair arguments is wrong.
			ms = ms.DeepCopy()
			ms.Generation++
			ms.ResourceVersion = "2"
			setPartition(ms, tc.afterPart)
			_, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(ctx, ms, metav1.UpdateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms))
			require.NoError(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			wantRoles := wanted.Spec.Template.Roles[:1]
			if tc.policy == api.ServingGroupRecreate {
				wantRoles = wanted.Spec.Template.Roles
			}
			wantCount := 0
			for _, role := range wantRoles {
				wantCount += 1 + int(role.WorkerReplicas)
				for worker := 0; worker <= int(role.WorkerReplicas); worker++ {
					pod := firstPosts[utils.GeneratePodName(group, utils.GenerateRoleID(role.Name, 0), worker)]
					require.NotNil(t, pod, "first recovery CREATE must include the applicable worker layout")
					require.Equal(t, role.EntryTemplate.Spec.Containers[0].Image, pod.Spec.Containers[0].Image)
					require.Equal(t, utils.ModelServingRevision(wanted), utils.ObjectRevision(pod))
					require.Equal(t, utils.CalRoleTemplateHash(role), utils.ObjectRoleTemplateHash(pod))
				}
			}
			require.Len(t, firstPosts, wantCount)
			// RoleRollingUpdate may legally start an unrelated old Role's
			// rollout later in this reconcile after protection is removed.
			// Scope-external identity is therefore checked at recovery CREATE
			// above, before that separate action, rather than at convergence.
		})
	}
}
