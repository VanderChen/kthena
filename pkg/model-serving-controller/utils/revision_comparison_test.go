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

package utils

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func comparisonModelServing() *api.ModelServing {
	role := api.Role{
		Name: "inference", Replicas: ptr.To(int32(1)),
		EntryTemplate: api.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox:1.36", Command: []string{"sleep", "86400"}}},
		}},
	}
	return &api.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "comparison"},
		Spec:       api.ModelServingSpec{Replicas: ptr.To(int32(1)), SchedulerName: "volcano", Template: api.ServingGroup{Roles: []api.Role{role}}},
	}
}

func TestSemanticRevisionHistoryRemainsImmutable(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()
	ms := comparisonModelServing()
	ms.Namespace = "default"
	ms.UID = "owner"
	old, err := CreateControllerRevision(ctx, client, ms, "legacy", ms.Spec.Template.Roles)
	require.NoError(t, err)
	scaled := ms.DeepCopy()
	scaled.Spec.Replicas = ptr.To(int32(3))
	scaled.Spec.Template.Roles[0].Replicas = ptr.To(int32(4))
	reused, err := CreateControllerRevision(ctx, client, scaled, "legacy", scaled.Spec.Template.Roles)
	require.NoError(t, err)
	require.Equal(t, old.Data.Raw, reused.Data.Raw)
	changed := ms.DeepCopy()
	changed.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "changed"
	_, err = CreateControllerRevision(ctx, client, changed, "legacy", changed.Spec.Template.Roles)
	require.ErrorContains(t, err, "different template")
	stored, err := GetControllerRevision(ctx, client, ms, "legacy")
	require.NoError(t, err)
	require.Equal(t, old.Data.Raw, stored.Data.Raw)
	foreign := ms.DeepCopy()
	foreign.UID = "foreign"
	_, err = CreateControllerRevision(ctx, client, foreign, "legacy", ms.Spec.Template.Roles)
	require.ErrorContains(t, err, "different owner")
}

func TestSemanticRevisionCleanupRetainsPodHistory(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()
	ms := comparisonModelServing()
	ms.Namespace = "default"
	ms.UID = "owner"
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = "current", "current"
	for _, revision := range []string{"current", "live", "unused"} {
		_, err := CreateControllerRevision(ctx, client, ms, revision, ms.Spec.Template.Roles)
		require.NoError(t, err)
	}
	// Fill the non-live history window so the oldest unused revision is pruned,
	// while the live Pod's older history remains protected outside the limit.
	for i := 0; i < defaultControllerRevisionHistoryLimit; i++ {
		_, err := CreateControllerRevision(ctx, client, ms, fmt.Sprintf("unused-%d", i), ms.Spec.Template.Roles)
		require.NoError(t, err)
	}
	foreign := ms.DeepCopy()
	foreign.UID = "foreign"
	_, err := CreateControllerRevision(ctx, client, foreign, "foreign", foreign.Spec.Template.Roles)
	require.NoError(t, err)
	pod := GenerateEntryPod(*ms.Spec.Template.Roles[0].DeepCopy(), ms, "comparison-0", 0, "live", "hash")
	pod.DeletionTimestamp = ptr.To(metav1.Now())
	_, err = client.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
	for _, revision := range []string{"current", "live", "foreign"} {
		cr, err := GetControllerRevision(ctx, client, ms, revision)
		require.NoError(t, err)
		require.NotNil(t, cr)
	}
	cr, err := GetControllerRevision(ctx, client, ms, "unused")
	require.NoError(t, err)
	require.Nil(t, cr)
}

func TestRevisionComparisonPreservesModelServingSpecBoundary(t *testing.T) {
	// These are all fields outside Roles in the v0.4 ModelServing Spec, plus
	// Role.replicas. They may affect scaling or scheduling, but not template rollout.
	tests := map[string]func(*api.ModelServing){
		"serving-group-scale-up":      func(ms *api.ModelServing) { ms.Spec.Replicas = ptr.To(int32(3)) },
		"serving-group-scale-to-zero": func(ms *api.ModelServing) { ms.Spec.Replicas = ptr.To(int32(0)) },
		"role-scale-up":               func(ms *api.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To(int32(3)) },
		"role-scale-to-zero":          func(ms *api.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To(int32(0)) },
		"scheduler":                   func(ms *api.ModelServing) { ms.Spec.SchedulerName = "another" },
		"plugins":                     func(ms *api.ModelServing) { ms.Spec.Plugins = []api.PluginSpec{{Name: "demo"}} },
		"recovery-policy":             func(ms *api.ModelServing) { ms.Spec.RecoveryPolicy = api.NoneRestartPolicy },
		"rollout-strategy": func(ms *api.ModelServing) {
			ms.Spec.RolloutStrategy = &api.RolloutStrategy{Type: api.RoleRollingUpdate, RollingUpdateConfiguration: &api.RollingUpdateConfiguration{Partition: ptr.To(intstr.FromInt(2)), MaxUnavailable: ptr.To(intstr.FromInt(1))}}
		},
		"restart-grace-period": func(ms *api.ModelServing) { ms.Spec.Template.RestartGracePeriodSeconds = ptr.To(int64(30)) },
		"gang-policy": func(ms *api.ModelServing) {
			ms.Spec.Template.GangPolicy = &api.GangPolicy{MinRoleReplicas: map[string]int32{"inference": 1}}
		},
		"topology-affinity": func(ms *api.ModelServing) {
			ms.Spec.Template.NetworkTopology = &api.NetworkTopology{ServingGroupAntiAffinity: &api.ServingGroupAntiAffinity{Preferred: []api.ServingGroupAffinityTerm{{TopologyTierName: "node"}}}}
		},
		"top-level-metadata-and-status": func(ms *api.ModelServing) {
			ms.Labels = map[string]string{"label": "changed"}
			ms.Status.UpdateRevision = "changed"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			old := comparisonModelServing()
			desired := old.DeepCopy()
			mutate(desired)
			before := desired.DeepCopy()
			require.Equal(t, Revision(RemoveRoleReplicasForRevision(old).Spec.Template.Roles), Revision(RemoveRoleReplicasForRevision(desired).Spec.Template.Roles))
			require.True(t, EqualRoleTemplates(old.Spec.Template.Roles, desired.Spec.Template.Roles))
			require.Equal(t, before, desired, "comparison must not strip replicas or change the scaling input")
		})
	}
}

func TestRevisionComparisonKeepsWorkloadChanges(t *testing.T) {
	tests := map[string]func(*api.Role){
		"name":            func(role *api.Role) { role.Name = "another" },
		"worker-replicas": func(role *api.Role) { role.WorkerReplicas++ },
		"worker-template": func(role *api.Role) { role.WorkerTemplate = role.EntryTemplate.DeepCopy() },
		"image":           func(role *api.Role) { role.EntryTemplate.Spec.Containers[0].Image = "busybox:1.37" },
		"command":         func(role *api.Role) { role.EntryTemplate.Spec.Containers[0].Command = []string{"sleep", "86399"} },
		"env": func(role *api.Role) {
			role.EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "CONFIG", Value: "new"}}
		},
		"resources": func(role *api.Role) {
			role.EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
		},
		"template-label": func(role *api.Role) {
			role.EntryTemplate.Metadata = &api.Metadata{Labels: map[string]string{"label": "value"}}
		},
		"template-annotation": func(role *api.Role) {
			role.EntryTemplate.Metadata = &api.Metadata{Annotations: map[string]string{"annotation": "value"}}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			old := comparisonModelServing()
			desired := old.DeepCopy()
			mutate(&desired.Spec.Template.Roles[0])
			require.NotEqual(t, Revision(RemoveRoleReplicasForRevision(old).Spec.Template.Roles), Revision(RemoveRoleReplicasForRevision(desired).Spec.Template.Roles))
			require.False(t, EqualRoleTemplates(old.Spec.Template.Roles, desired.Spec.Template.Roles))
		})
	}
	old := comparisonModelServing()
	other := *old.Spec.Template.Roles[0].DeepCopy()
	other.Name = "decode"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, other)
	desired := old.DeepCopy()
	desired.Spec.Template.Roles[0], desired.Spec.Template.Roles[1] = desired.Spec.Template.Roles[1], desired.Spec.Template.Roles[0]
	require.False(t, EqualRoleTemplates(old.Spec.Template.Roles, desired.Spec.Template.Roles), "Role order remains significant, as in the existing group hash")
}

func TestRevisionComparisonUsesKubernetesSemantics(t *testing.T) {
	old := comparisonModelServing()
	desired := old.DeepCopy()
	desired.Spec.Template.Roles[0].EntryTemplate.Spec.NodeSelector = map[string]string{}
	require.NotEqual(t, Revision(old.Spec.Template.Roles), Revision(desired.Spec.Template.Roles))
	require.True(t, EqualRoleTemplates(old.Spec.Template.Roles, desired.Spec.Template.Roles))
	old.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1")}
	desired.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: *resource.NewQuantity(1, resource.DecimalSI)}
	require.NotEqual(t, Revision(old.Spec.Template.Roles), Revision(desired.Spec.Template.Roles))
	require.True(t, EqualRoleTemplates(old.Spec.Template.Roles, desired.Spec.Template.Roles))
}
