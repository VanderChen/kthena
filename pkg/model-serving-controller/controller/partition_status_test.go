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
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestPartitionRolloutStatus(t *testing.T) {
	for _, tc := range []struct {
		name              string
		partition         intstr.IntOrString
		oldOrdinals       []int
		ordinals          []int
		groups, roles     int
		coordinated       bool
		dependencyBlocked bool
		groupMode         bool
		noChange          bool
		zero              bool
		unready           *int
		deleting          *int
		missing           *int
		unknown           *int
		surge             bool
		lastGroupOld      bool
		want              api.ModelServingConditionType
	}{
		{name: "role partition stop", partition: intstr.FromInt(1), oldOrdinals: []int{0}, want: api.ModelServingAvailable},
		{name: "percentage rounds up", partition: intstr.FromString("34%"), oldOrdinals: []int{0, 1}, want: api.ModelServingAvailable},
		{name: "full partition", partition: intstr.FromInt(3), oldOrdinals: []int{0, 1, 2}, want: api.ModelServingAvailable},
		{name: "protected failure", partition: intstr.FromInt(1), oldOrdinals: []int{0}, unready: ptr.To(0), want: api.ModelServingProgressing},
		{name: "target not ready", partition: intstr.FromInt(1), oldOrdinals: []int{0}, unready: ptr.To(2), want: api.ModelServingProgressing},
		{name: "eligible old remains", partition: intstr.FromInt(1), oldOrdinals: []int{0, 1}, want: api.ModelServingUpdateInProgress},
		{name: "protected failure consumes rollout budget", partition: intstr.FromInt(1), oldOrdinals: []int{0, 1}, unready: ptr.To(0), want: api.ModelServingUpdateInProgress},
		{name: "old deletion still in flight", partition: intstr.FromInt(1), oldOrdinals: []int{0, 1}, deleting: ptr.To(1), want: api.ModelServingUpdateInProgress},
		{name: "protected deletion", partition: intstr.FromInt(1), oldOrdinals: []int{0}, deleting: ptr.To(0), want: api.ModelServingProgressing},
		{name: "target surge remains", partition: intstr.FromInt(1), oldOrdinals: []int{0}, surge: true, want: api.ModelServingUpdateInProgress},
		{name: "target missing", partition: intstr.FromInt(1), oldOrdinals: []int{0}, missing: ptr.To(2), want: api.ModelServingProgressing},
		{name: "sparse stop", partition: intstr.FromInt(1), oldOrdinals: []int{0}, ordinals: []int{0, 3, 4}, want: api.ModelServingAvailable},
		{name: "sparse ordinal beyond full partition", partition: intstr.FromInt(3), oldOrdinals: []int{0, 3}, ordinals: []int{0, 3, 4}, want: api.ModelServingUpdateInProgress},
		{name: "multiple groups and roles", partition: intstr.FromInt(1), oldOrdinals: []int{0}, groups: 2, roles: 2, want: api.ModelServingAvailable},
		{name: "last group last role unfinished", partition: intstr.FromInt(1), oldOrdinals: []int{0}, groups: 2, roles: 2, lastGroupOld: true, want: api.ModelServingUpdateInProgress},
		{name: "coordinated stop", partition: intstr.FromInt(1), oldOrdinals: []int{0}, roles: 2, coordinated: true, want: api.ModelServingAvailable},
		{name: "coordinated unfinished", partition: intstr.FromInt(1), oldOrdinals: []int{0, 1}, roles: 2, coordinated: true, want: api.ModelServingUpdateInProgress},
		{name: "old dependency blocks eligible replica", partition: intstr.FromInt(0), oldOrdinals: []int{0}, roles: 2, coordinated: true, dependencyBlocked: true, want: api.ModelServingUpdateInProgress},
		{name: "unresolved eligible history", partition: intstr.FromInt(1), oldOrdinals: []int{0}, unknown: ptr.To(1), want: api.ModelServingUpdateInProgress},
		{name: "SG stop unchanged", partition: intstr.FromInt(1), oldOrdinals: []int{0}, groupMode: true, want: api.ModelServingAvailable},
		{name: "full target", partition: intstr.FromInt(0), want: api.ModelServingAvailable},
		{name: "unchanged templates", partition: intstr.FromInt(1), noChange: true, want: api.ModelServingAvailable},
		{name: "zero role replicas", partition: intstr.FromInt(0), zero: true, want: api.ModelServingAvailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			groups, instances, roles := max(1, tc.groups), 3, max(1, tc.roles)
			if tc.groupMode {
				groups, instances = 3, 1
			}
			if tc.zero {
				instances = 0
			}
			old := lifecycleMS("partition-status", int32(groups), 1, 0)
			old.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
			// This inactive SG partition must not hide Role updates in group zero.
			old.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
			old.Spec.Template.Roles[0].WorkerReplicas = 0
			old.Spec.Template.Roles[0].Replicas = ptr.To(int32(instances))
			old.Spec.Template.Roles[0].Partition = &tc.partition
			if roles == 2 {
				other := *old.Spec.Template.Roles[0].DeepCopy()
				other.Name = "decode"
				old.Spec.Template.Roles = append(old.Spec.Template.Roles, other)
			}
			if tc.coordinated {
				old.Spec.RolloutStrategy.RoleCoordination = &api.RoleCoordination{
					Roles: []string{"prefill", "decode"}, MaxSkew: ptr.To(intstr.FromString("50%")),
					Dependencies: []api.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}},
				}
			}
			if tc.groupMode {
				old.Spec.RolloutStrategy.Type = api.ServingGroupRollingUpdate
				old.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = &tc.partition
			}
			ms := old.DeepCopy()
			ms.Generation++
			if !tc.noChange {
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].EntryTemplate.Spec.Containers[0].Image = "test:v2"
				}
			}
			baseline, target := utils.ModelServingRevision(old), utils.ModelServingRevision(ms)
			ms.Status.CurrentRevision, ms.Status.UpdateRevision = baseline, target
			ms.Status.Conditions = []metav1.Condition{{Type: string(api.ModelServingUpdateInProgress), Status: metav1.ConditionTrue, Reason: "GroupsUpdating", LastTransitionTime: metav1.Now()}}
			c := lifecycleController(t, ms, old)
			key := utils.GetNamespaceName(ms)
			for g := 0; g < groups; g++ {
				group := utils.GenerateServingGroupName(ms.Name, g)
				for r := range ms.Spec.Template.Roles {
					count := instances
					if tc.surge {
						count++
					}
					for i := 0; i < count; i++ {
						ordinal := i
						if tc.ordinals != nil {
							ordinal = tc.ordinals[i]
						}
						if tc.missing != nil && ordinal == *tc.missing {
							continue
						}
						versionOrdinal := ordinal
						if tc.groupMode {
							versionOrdinal = g
						}
						template := ms
						if slices.Contains(tc.oldOrdinals, versionOrdinal) || (tc.lastGroupOld && g == groups-1 && r == roles-1 && ordinal == 2) {
							template = old
						}
						if tc.dependencyBlocked && r == 1 {
							template = ms
							if ordinal == 1 {
								template = old
							}
						}
						spec := *template.Spec.Template.Roles[r].DeepCopy()
						id := utils.GenerateRoleID(spec.Name, ordinal)
						revision, hash := utils.ModelServingRevision(template), utils.CalRoleTemplateHash(spec)
						if tc.unknown != nil && ordinal == *tc.unknown {
							revision, hash = "unresolved", "unresolved"
						}
						pod := utils.GenerateEntryPod(spec, ms, group, id, revision, hash)
						pod.UID = types.UID(fmt.Sprintf("g%d-r%d-i%d", g, r, ordinal))
						pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
						status := datastore.RoleRunning
						if tc.unready != nil && ordinal == *tc.unready {
							pod.Status.Conditions[0].Status = corev1.ConditionFalse
							status = datastore.RoleCreating
						}
						if tc.deleting != nil && ordinal == *tc.deleting {
							pod.DeletionTimestamp = ptr.To(metav1.Now())
							status = datastore.RoleDeleting
						}
						if tc.unknown != nil && ordinal == *tc.unknown {
							status = datastore.RoleReadinessUnknown
						}
						if tc.surge && ordinal == instances {
							pod.Annotations = map[string]string{surgeAnnotation: surgeRole}
						}
						require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
						c.store.AddRunningPodToServingGroup(key, group, pod.Name, revision, hash, spec.Name, id)
						require.NoError(t, c.store.UpdateRoleStatus(key, group, spec.Name, id, status))
					}
				}
			}
			require.NoError(t, c.UpdateModelServingStatus(ms, target))
			actual, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			for _, condition := range []api.ModelServingConditionType{api.ModelServingAvailable, api.ModelServingProgressing, api.ModelServingUpdateInProgress} {
				require.Equal(t, condition == tc.want, meta.IsStatusConditionTrue(actual.Status.Conditions, string(condition)), "%s: %+v", condition, actual.Status.Conditions)
			}
			if len(tc.oldOrdinals) > 0 || tc.zero {
				require.Equal(t, baseline, actual.Status.CurrentRevision)
			} else if tc.want == api.ModelServingAvailable {
				require.Equal(t, target, actual.Status.CurrentRevision)
			}
			if tc.coordinated && tc.want == api.ModelServingAvailable {
				require.False(t, meta.IsStatusConditionTrue(actual.Status.Conditions, string(api.ModelServingCoordinatedRoleRolloutBlocked)))
			}
			if tc.dependencyBlocked {
				blocked := meta.FindStatusCondition(actual.Status.Conditions, string(api.ModelServingCoordinatedRoleRolloutBlocked))
				require.NotNil(t, blocked)
				require.Equal(t, metav1.ConditionTrue, blocked.Status)
				require.Equal(t, "OldVersionDependencyPresent", blocked.Reason)
			}
			// A stable stop must not repeatedly write status or change transition times.
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(actual.DeepCopy()))
			before := actual.Status.DeepCopy()
			client := c.modelServingClient.(*kthenafake.Clientset)
			client.ClearActions()
			require.NoError(t, c.UpdateModelServingStatus(actual, target))
			for _, action := range client.Actions() {
				require.False(t, action.Matches("update", "modelservings") && action.GetSubresource() == "status", "stable status was rewritten")
			}
			after, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, *before, after.Status)
		})
	}
}
