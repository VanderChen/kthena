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

package podgroupmanager

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	scheduling "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	listers "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"
)

func TestPodGroupComparisonUsesEffectiveValues(t *testing.T) {
	current := &scheduling.PodGroup{Spec: scheduling.PodGroupSpec{Queue: scheduling.DefaultQueue, MinMember: 1, MinResources: &corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("1Gi"), "example.com/device": resource.MustParse("2"),
	}}}
	desired := current.DeepCopy()
	desired.Spec.Queue = ""
	*desired.Spec.MinResources = corev1.ResourceList{
		corev1.ResourceCPU: *resource.NewMilliQuantity(1000, resource.DecimalSI), corev1.ResourceMemory: resource.MustParse("1024Mi"), "example.com/device": resource.MustParse("2000m"),
	}
	require.False(t, hasPodGroupChanged(current, desired))
	require.False(t, hasPodGroupChanged(desired, current))
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory, "example.com/device"} {
		changed := desired.DeepCopy()
		q := (*changed.Spec.MinResources)[name]
		q.Add(resource.MustParse("1"))
		(*changed.Spec.MinResources)[name] = q
		require.True(t, hasPodGroupChanged(current, changed), "%s must still be updated", name)
	}
	removed := desired.DeepCopy()
	delete(*removed.Spec.MinResources, "example.com/device")
	require.True(t, hasPodGroupChanged(current, removed))
	for _, pair := range [][2]string{{"custom", ""}, {"", "custom"}, {"first", "second"}} {
		current.Spec.Queue, desired.Spec.Queue = pair[0], pair[1]
		require.True(t, hasPodGroupChanged(current, desired))
	}
}

func TestUpdatePodGroupSkipsEquivalentStoredSpec(t *testing.T) {
	for _, queue := range []string{"", scheduling.DefaultQueue, "custom"} {
		t.Run("queue="+queue, func(t *testing.T) {
			ms := newMinimalMS(queue)
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Requests = corev1.ResourceList{
				corev1.ResourceCPU: resource.MustParse("1000m"), corev1.ResourceMemory: resource.MustParse("1024Mi"),
				"example.com/device": resource.MustParse("2"),
			}
			manager := &Manager{}
			manager.hasSubGroupPolicy.Store(true)
			ms.Spec.Template.NetworkTopology = &api.NetworkTopology{
				GroupPolicy: &scheduling.NetworkTopologySpec{Mode: scheduling.HardNetworkTopologyMode, HighestTierAllowed: ptr.To(2)},
				RolePolicy:  &scheduling.NetworkTopologySpec{Mode: scheduling.SoftNetworkTopologyMode, HighestTierAllowed: ptr.To(1)},
			}
			minMember, minRoleMember, resources := manager.calculateRequirements(ms)
			pg := &scheduling.PodGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "test-pg", Namespace: ms.Namespace},
				Spec:       scheduling.PodGroupSpec{MinMember: int32(minMember), MinResources: &resources, Queue: queue},
			}
			if queue == "" {
				pg.Spec.Queue = scheduling.DefaultQueue
			}
			// Preserve fields managed by other writers when a real update is needed.
			pg.Spec.MinTaskMember = map[string]int32{"external": 1}
			pg.Spec.PriorityClassName = "external-priority"
			syncPodGroupNetworkTopology(ms, &pg.Spec)
			pg = appendSubGroupPolicy(ms, pg, minRoleMember)
			// Informers observe serialized quantities and the server's default queue.
			encoded, err := json.Marshal(pg)
			require.NoError(t, err)
			stored := &scheduling.PodGroup{}
			require.NoError(t, json.Unmarshal(encoded, stored))
			client := volcanofake.NewSimpleClientset(stored)
			indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
			require.NoError(t, indexer.Add(stored))
			manager.volcanoClient = client
			manager.PodGroupLister = listers.NewPodGroupLister(indexer)
			for i := 0; i < 3; i++ {
				require.NoError(t, manager.updatePodGroupIfNeeded(context.Background(), stored, ms))
			}
			require.Empty(t, client.Actions(), "equivalent requirements must not issue an UPDATE")

			*ms.Spec.Template.Roles[0].Replicas = 2
			require.NoError(t, manager.updatePodGroupIfNeeded(context.Background(), stored, ms))
			require.Len(t, client.Actions(), 1)
			require.Equal(t, "update", client.Actions()[0].GetVerb())
			updated, err := client.SchedulingV1beta1().PodGroups(ms.Namespace).Get(context.Background(), stored.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, int32(2), updated.Spec.MinMember)
			require.Equal(t, pg.Spec.MinTaskMember, updated.Spec.MinTaskMember)
			require.Equal(t, pg.Spec.PriorityClassName, updated.Spec.PriorityClassName)
			require.Equal(t, int32(2), *updated.Spec.SubGroupPolicy[0].MinSubGroups)
			for name, quantity := range resources {
				quantity.Mul(2)
				actual := (*updated.Spec.MinResources)[name]
				require.Zero(t, actual.Cmp(quantity), "%s must reflect the new replica count", name)
			}
		})
	}
}

func TestPodGroupComparisonIncludesPolicies(t *testing.T) {
	base := &scheduling.PodGroup{Spec: scheduling.PodGroupSpec{
		MinMember: 2, Queue: scheduling.DefaultQueue,
		MinTaskMember: map[string]int32{"task": 1}, PriorityClassName: "priority",
		NetworkTopology: &scheduling.NetworkTopologySpec{Mode: scheduling.HardNetworkTopologyMode, HighestTierAllowed: ptr.To(2)},
		SubGroupPolicy: []scheduling.SubGroupPolicySpec{{
			Name: "role", SubGroupSize: ptr.To[int32](1), MinSubGroups: ptr.To[int32](1),
			LabelSelector:   &metav1.LabelSelector{MatchLabels: map[string]string{"role": "a"}},
			MatchLabelKeys:  []string{"role", "index"},
			NetworkTopology: &scheduling.NetworkTopologySpec{Mode: scheduling.SoftNetworkTopologyMode},
		}},
		TopologyAffinity: &scheduling.TopologyAffinitySpec{SubGroupAffinity: &scheduling.SubGroupAffinity{
			Required: []scheduling.SubGroupAffinityTerm{{SubGroups: []string{"a", "b"}, TopologyTierName: "rack"}},
		}},
	}}
	changes := map[string]func(*scheduling.PodGroupSpec){
		"task minimum":         func(s *scheduling.PodGroupSpec) { s.MinTaskMember["task"]++ },
		"priority":             func(s *scheduling.PodGroupSpec) { s.PriorityClassName = "different" },
		"group mode":           func(s *scheduling.PodGroupSpec) { s.NetworkTopology.Mode = scheduling.SoftNetworkTopologyMode },
		"group tier":           func(s *scheduling.PodGroupSpec) { *s.NetworkTopology.HighestTierAllowed++ },
		"group policy removed": func(s *scheduling.PodGroupSpec) { s.NetworkTopology = nil },
		"role name":            func(s *scheduling.PodGroupSpec) { s.SubGroupPolicy[0].Name = "different" },
		"role size":            func(s *scheduling.PodGroupSpec) { *s.SubGroupPolicy[0].SubGroupSize++ },
		"role minimum":         func(s *scheduling.PodGroupSpec) { *s.SubGroupPolicy[0].MinSubGroups++ },
		"role selector":        func(s *scheduling.PodGroupSpec) { s.SubGroupPolicy[0].LabelSelector.MatchLabels["role"] = "b" },
		"role selector absent": func(s *scheduling.PodGroupSpec) { s.SubGroupPolicy[0].LabelSelector = nil },
		"role mode": func(s *scheduling.PodGroupSpec) {
			s.SubGroupPolicy[0].NetworkTopology.Mode = scheduling.HardNetworkTopologyMode
		},
		"role grouping key":  func(s *scheduling.PodGroupSpec) { s.SubGroupPolicy[0].MatchLabelKeys[1] = "other" },
		"grouping key order": func(s *scheduling.PodGroupSpec) { s.SubGroupPolicy[0].MatchLabelKeys = []string{"index", "role"} },
		"affinity weight":    func(s *scheduling.PodGroupSpec) { s.TopologyAffinity.SubGroupAffinity.Required[0].Weight = 1 },
		"affinity tier": func(s *scheduling.PodGroupSpec) {
			s.TopologyAffinity.SubGroupAffinity.Required[0].TopologyTierName = "node"
		},
		"affinity members": func(s *scheduling.PodGroupSpec) {
			s.TopologyAffinity.SubGroupAffinity.Required[0].SubGroups = []string{"b", "a"}
		},
		"affinity removed": func(s *scheduling.PodGroupSpec) { s.TopologyAffinity = nil },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			updated := base.DeepCopy()
			change(&updated.Spec)
			require.True(t, hasPodGroupChanged(base, updated))
		})
	}

	t.Run("empty collections and metadata do not cause a write", func(t *testing.T) {
		updated := base.DeepCopy()
		updated.Spec.Queue = ""
		updated.Spec.SubGroupPolicy[0].LabelSelector.MatchExpressions = []metav1.LabelSelectorRequirement{}
		updated.Spec.TopologyAffinity.SubGroupAffinity.Preferred = []scheduling.SubGroupAffinityTerm{}
		updated.ResourceVersion = "new-version"
		updated.Annotations = map[string]string{"other": "writer"}
		updated.Status.Phase = scheduling.PodGroupRunning
		beforeCurrent, beforeUpdated := base.DeepCopy(), updated.DeepCopy()
		require.False(t, hasPodGroupChanged(base, updated))
		require.Equal(t, beforeCurrent, base)
		require.Equal(t, beforeUpdated, updated, "comparison must not mutate caller-owned inputs")
	})
	for _, field := range []string{"selector", "network policy", "minimum"} {
		t.Run("absent differs from explicit "+field, func(t *testing.T) {
			current, updated := base.DeepCopy(), base.DeepCopy()
			switch field {
			case "selector":
				current.Spec.SubGroupPolicy[0].LabelSelector = nil
				updated.Spec.SubGroupPolicy[0].LabelSelector = &metav1.LabelSelector{}
			case "network policy":
				current.Spec.NetworkTopology = nil
				updated.Spec.NetworkTopology = &scheduling.NetworkTopologySpec{}
			case "minimum":
				current.Spec.SubGroupPolicy[0].MinSubGroups = nil
				updated.Spec.SubGroupPolicy[0].MinSubGroups = ptr.To[int32](0)
			}
			require.True(t, hasPodGroupChanged(current, updated))
		})
	}
}
