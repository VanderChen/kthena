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
	"errors"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
)

const syncedName = "test-ms-0"

// writesOf returns the verbs of the writes a client received.
func writesOf(client *volcanofake.Clientset) []string {
	var out []string
	for _, action := range client.Actions() {
		if verb := action.GetVerb(); verb != "get" && verb != "list" && verb != "watch" {
			out = append(out, verb)
		}
	}
	return out
}

// storedPodGroup returns the PodGroup of the group as the server has it.
func storedPodGroup(t *testing.T, client *volcanofake.Clientset) *schedulingv1beta1.PodGroup {
	t.Helper()
	podGroup, err := client.SchedulingV1beta1().PodGroups("default").Get(context.Background(), syncedName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return podGroup
}

// served returns a PodGroup as a cache shows it: its quantities are in the
// notation of the API server, whatever they were written in.
func served(t *testing.T, podGroup *schedulingv1beta1.PodGroup) *schedulingv1beta1.PodGroup {
	t.Helper()
	encoded, err := json.Marshal(podGroup)
	if err != nil {
		t.Fatal(err)
	}
	out := &schedulingv1beta1.PodGroup{}
	if err := json.Unmarshal(encoded, out); err != nil {
		t.Fatal(err)
	}
	return out
}

// managerWith returns a manager whose cache and server hold podGroup.
func managerWith(podGroup *schedulingv1beta1.PodGroup) (*Manager, *volcanofake.Clientset) {
	return groupManager([]*schedulingv1beta1.PodGroup{podGroup}, []*schedulingv1beta1.PodGroup{podGroup.DeepCopy()})
}

// asWanted gives a PodGroup the spec that newMinimalMS asks for, and a queue.
func asWanted(queue string) func(*schedulingv1beta1.PodGroup) {
	return func(podGroup *schedulingv1beta1.PodGroup) {
		podGroup.Spec = schedulingv1beta1.PodGroupSpec{MinMember: 1, MinResources: &corev1.ResourceList{}, Queue: queue}
	}
}

func syncedModelServing(annotation *string) *workloadv1alpha1.ModelServing {
	ms := newMinimalMS("")
	ms.UID = "ms-uid"
	if annotation != nil {
		ms.Annotations = map[string]string{schedulingv1beta1.QueueNameAnnotationKey: *annotation}
	}
	return ms
}

func mustSync(t *testing.T, manager *Manager, ms *workloadv1alpha1.ModelServing) {
	t.Helper()
	if retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), syncedName); err != nil || retry != 0 {
		t.Fatalf("SyncPodGroup() = %v, %v", retry, err)
	}
}

// The queue of a PodGroup is the one the ModelServing names in its
// queue-name annotation, on creation and afterwards. Without the annotation
// the queue is left to the server: none is written, and the one the server
// assigned stays. Naming the default queue is naming a queue.
func TestSyncPodGroupQueue(t *testing.T) {
	tests := []struct {
		name string
		// existing is the queue of the PodGroup that exists; nil when there is none.
		existing *string
		// annotation is the queue-name annotation; nil when there is none.
		annotation *string
		write      string
		want       string
	}{
		{name: "created with the queue of the annotation", annotation: ptr.To("high-priority-queue"), write: "create", want: "high-priority-queue"},
		{name: "created without the annotation: the queue is left to the server", write: "create"},
		{name: "created with an empty annotation: the queue is left to the server", annotation: ptr.To(""), write: "create"},
		{name: "the annotation changes", existing: ptr.To("old-queue"), annotation: ptr.To("new-queue"), write: "update", want: "new-queue"},
		{name: "the annotation is removed: the queue stays", existing: ptr.To("some-queue"), want: "some-queue"},
		{name: "the default queue, named, replaces the queue of the server", existing: ptr.To("namespace-queue"),
			annotation: ptr.To(schedulingv1beta1.DefaultQueue), write: "update", want: schedulingv1beta1.DefaultQueue},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := syncedModelServing(tt.annotation)
			manager, client := groupManager(nil, nil)
			var cached, before *schedulingv1beta1.PodGroup
			if tt.existing != nil {
				cached = existingPodGroup(asWanted(*tt.existing))
				before = cached.DeepCopy()
				manager, client = managerWith(cached)
			}

			mustSync(t, manager, ms)
			var want []string
			if tt.write != "" {
				want = []string{tt.write}
			}
			if got := writesOf(client); !reflect.DeepEqual(got, want) {
				t.Errorf("writes = %v, want %v", got, want)
			}
			if got := storedPodGroup(t, client).Spec.Queue; got != tt.want {
				t.Errorf("queue = %q, want %q", got, tt.want)
			}
			if cached != nil && !reflect.DeepEqual(cached, before) {
				t.Error("the PodGroup of the cache was changed")
			}
		})
	}
}

// The queue the server assigned to a PodGroup is no difference: it causes no
// write, however often the PodGroup is synced. An update that changes
// something else keeps it. A queue the ModelServing then names replaces it.
func TestSyncPodGroupKeepsTheQueueOfTheServer(t *testing.T) {
	tests := []struct {
		name       string
		queue      string
		annotation *string
	}{
		{name: "default without annotation", queue: schedulingv1beta1.DefaultQueue},
		{name: "namespace queue without annotation", queue: "namespace-queue"},
		{name: "custom queue after annotation removal", queue: "custom-queue"},
		{name: "empty server queue"},
		{name: "default with empty annotation", queue: schedulingv1beta1.DefaultQueue, annotation: ptr.To("")},
		{name: "namespace queue with empty annotation", queue: "namespace-queue", annotation: ptr.To("")},
		{name: "custom queue with empty annotation", queue: "custom-queue", annotation: ptr.To("")},
		{name: "empty queue with empty annotation", annotation: ptr.To("")},
		{name: "explicit matching default", queue: schedulingv1beta1.DefaultQueue, annotation: ptr.To(schedulingv1beta1.DefaultQueue)},
		{name: "explicit matching custom", queue: "custom-queue", annotation: ptr.To("custom-queue")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := syncedModelServing(tt.annotation)
			cached := existingPodGroup(asWanted(tt.queue))
			beforePodGroup, beforeModelServing := cached.DeepCopy(), ms.DeepCopy()
			manager, client := managerWith(cached)
			for range 3 {
				mustSync(t, manager, ms)
			}
			if got := writesOf(client); len(got) != 0 {
				t.Fatalf("writes = %v; the queue of the server must not cause a write", got)
			}
			if !reflect.DeepEqual(cached, beforePodGroup) || !reflect.DeepEqual(ms, beforeModelServing) {
				t.Fatal("SyncPodGroup changed the PodGroup of the cache or the ModelServing")
			}

			*ms.Spec.Template.Roles[0].Replicas = 2
			mustSync(t, manager, ms)
			if got := writesOf(client); !reflect.DeepEqual(got, []string{"update"}) {
				t.Fatalf("writes = %v, want one update", got)
			}
			updated := storedPodGroup(t, client)
			if updated.Spec.MinMember != 2 || updated.Spec.Queue != tt.queue {
				t.Errorf("after a change of the requirements: minMember %d, queue %q; want 2 and the queue of the server, %q",
					updated.Spec.MinMember, updated.Spec.Queue, tt.queue)
			}
			if !reflect.DeepEqual(cached, beforePodGroup) {
				t.Error("the PodGroup of the cache was changed")
			}

			manager, client = managerWith(updated)
			ms.Annotations = map[string]string{schedulingv1beta1.QueueNameAnnotationKey: "override-queue"}
			mustSync(t, manager, ms)
			if got := writesOf(client); !reflect.DeepEqual(got, []string{"update"}) {
				t.Fatalf("writes = %v; a queue that is named must be written", got)
			}
			if got := storedPodGroup(t, client).Spec.Queue; got != "override-queue" {
				t.Errorf("queue = %q, want override-queue", got)
			}
		})
	}
}

// The network topology of the group is set on the PodGroup, follows a change,
// and is cleared when the ModelServing has none any more.
func TestSyncPodGroupNetworkTopology(t *testing.T) {
	const oldMode, newMode = schedulingv1beta1.NetworkTopologyMode("old-mode"), schedulingv1beta1.NetworkTopologyMode("new-mode")
	topology := func(mode schedulingv1beta1.NetworkTopologyMode) *schedulingv1beta1.NetworkTopologySpec {
		return &schedulingv1beta1.NetworkTopologySpec{Mode: mode, HighestTierAllowed: ptr.To(3)}
	}
	tests := []struct {
		name string
		// create: there is no PodGroup yet.
		create   bool
		existing *schedulingv1beta1.NetworkTopologySpec
		wanted   *schedulingv1beta1.NetworkTopologySpec
	}{
		{name: "created with the topology", create: true, wanted: topology(newMode)},
		{name: "a change is followed", existing: &schedulingv1beta1.NetworkTopologySpec{Mode: oldMode}, wanted: topology(newMode)},
		{name: "removed: cleared", existing: &schedulingv1beta1.NetworkTopologySpec{Mode: oldMode}},
		{name: "added: set", wanted: topology(newMode)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := syncedModelServing(nil)
			if tt.wanted != nil {
				ms.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{GroupPolicy: tt.wanted.DeepCopy()}
			}
			manager, client := groupManager(nil, nil)
			if !tt.create {
				manager, client = managerWith(existingPodGroup(asWanted(""), func(podGroup *schedulingv1beta1.PodGroup) {
					podGroup.Spec.NetworkTopology = tt.existing.DeepCopy()
				}))
			}
			mustSync(t, manager, ms)
			if got := storedPodGroup(t, client).Spec.NetworkTopology; !reflect.DeepEqual(got, tt.wanted) {
				t.Errorf("networkTopology = %+v, want %+v", got, tt.wanted)
			}
		})
	}
}

// A PodGroup is read from the cache. While the PodGroup kind is known and its
// cache is not there, nothing can be decided: the caller is told to come
// again, and nothing is written. A manager that went on would create a
// PodGroup that exists, or miss one that has to be updated.
func TestSyncPodGroupWithoutThePodGroupCache(t *testing.T) {
	ms := syncedModelServing(nil)
	crd := testhelper.CreatePodGroupCRD()
	// running returns a manager whose PodGroup cache is fed by an informer.
	running := func(t *testing.T) (*Manager, *volcanofake.Clientset) {
		client := volcanofake.NewSimpleClientset(existingPodGroup())
		manager := NewManager(nil, client, apiextfake.NewSimpleClientset(crd), nil)
		t.Cleanup(manager.stopPodGroupInformer)
		if manager.GetPodGroupLister() == nil {
			t.Fatal("the PodGroup cache was not started")
		}
		client.ClearActions()
		return manager, client
	}

	t.Run("the cache is not there", func(t *testing.T) {
		manager, client := groupManager(nil, nil)
		manager.PodGroupLister = nil
		retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), syncedName)
		if !errors.Is(err, errPodGroupInformerNotInitialized) || retry <= 0 {
			t.Errorf("SyncPodGroup() = %v, %v; want a retry and the error of a missing cache", retry, err)
		}
		if got := writesOf(client); len(got) != 0 {
			t.Errorf("writes without a cache: %v", got)
		}
	})

	// The PodGroup kind was removed from the cluster: PodGroups are not used.
	t.Run("the kind was removed", func(t *testing.T) {
		manager, client := running(t)
		manager.handlePodGroupCRDDelete(crd)
		retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), syncedName)
		if err != nil || retry != 0 || manager.Enabled(ms) {
			t.Errorf("SyncPodGroup() = %v, %v, enabled = %t; want nothing to do", retry, err, manager.Enabled(ms))
		}
		if got := writesOf(client); len(got) != 0 {
			t.Errorf("writes after the kind was removed: %v", got)
		}
	})

	// Run with -race: the removal clears the cache under the lock of the
	// manager while a sync reads it.
	t.Run("the kind is removed during a sync", func(t *testing.T) {
		manager, _ := running(t)
		var err error
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, err = manager.SyncPodGroup(context.Background(), ms, groupOwner(), syncedName)
		}()
		manager.handlePodGroupCRDDelete(crd)
		<-done
		// The sync read the cache before the removal, or found it gone.
		if err != nil && !errors.Is(err, errPodGroupInformerNotInitialized) {
			t.Errorf("SyncPodGroup() error = %v", err)
		}
	})
}

// policyModelServing returns a ModelServing whose PodGroup has everything a
// PodGroup can take from a ModelServing: two Roles with resource requests, a
// queue, and a network topology for the group and for the Roles.
func policyModelServing(queue string) *workloadv1alpha1.ModelServing {
	ms := newMinimalMS(queue)
	ms.UID = "ms-uid"
	requests := corev1.ResourceList{
		corev1.ResourceCPU: resource.MustParse("1000m"), corev1.ResourceMemory: resource.MustParse("1024Mi"),
		"example.com/device": resource.MustParse("2"),
	}
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Requests = requests
	router := *ms.Spec.Template.Roles[0].DeepCopy()
	router.Name = "role1"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, router)
	ms.Spec.Template.NetworkTopology = &workloadv1alpha1.NetworkTopology{
		GroupPolicy: &schedulingv1beta1.NetworkTopologySpec{Mode: schedulingv1beta1.HardNetworkTopologyMode, HighestTierAllowed: ptr.To(2)},
		RolePolicy:  &schedulingv1beta1.NetworkTopologySpec{Mode: schedulingv1beta1.SoftNetworkTopologyMode, HighestTierAllowed: ptr.To(1)},
	}
	return ms
}

// createdPodGroup has SyncPodGroup create the PodGroup of ms and returns it
// as a cache shows it afterwards.
func createdPodGroup(t *testing.T, ms *workloadv1alpha1.ModelServing) *schedulingv1beta1.PodGroup {
	t.Helper()
	manager, client := groupManager(nil, nil)
	manager.hasSubGroupPolicy.Store(true)
	mustSync(t, manager, ms)
	return served(t, storedPodGroup(t, client))
}

// What SyncPodGroup puts into a PodGroup: the minimum of members and of
// resources, the queue, the network topology of the group, and one subgroup
// for every Role, which is what makes the scheduler place the Pods of a Role
// instance together.
func TestSyncPodGroupSpec(t *testing.T) {
	subgroup := func(role string) schedulingv1beta1.SubGroupPolicySpec {
		return schedulingv1beta1.SubGroupPolicySpec{
			Name: role,
			LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-ms", workloadv1alpha1.RoleLabelKey: role,
			}},
			MatchLabelKeys:  []string{workloadv1alpha1.RoleIDKey},
			SubGroupSize:    ptr.To[int32](1),
			MinSubGroups:    ptr.To[int32](1),
			NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{Mode: schedulingv1beta1.SoftNetworkTopologyMode, HighestTierAllowed: ptr.To(1)},
		}
	}
	want := schedulingv1beta1.PodGroupSpec{
		MinMember: 2,
		Queue:     "custom",
		MinResources: &corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("2Gi"),
			"example.com/device": resource.MustParse("4"),
		},
		NetworkTopology: &schedulingv1beta1.NetworkTopologySpec{Mode: schedulingv1beta1.HardNetworkTopologyMode, HighestTierAllowed: ptr.To(2)},
		SubGroupPolicy:  []schedulingv1beta1.SubGroupPolicySpec{subgroup("role0"), subgroup("role1")},
	}
	if got := createdPodGroup(t, policyModelServing("custom")).Spec; !apiequality.Semantic.DeepEqual(got, want) {
		t.Errorf("spec of the PodGroup:\n got %+v\nwant %+v", got, want)
	}

	// A cluster whose PodGroup kind has no subgroups gets none.
	manager, client := groupManager(nil, nil)
	mustSync(t, manager, policyModelServing("custom"))
	if got := storedPodGroup(t, client).Spec.SubGroupPolicy; len(got) != 0 {
		t.Errorf("subgroups where the PodGroup kind has none: %+v", got)
	}
}

// A PodGroup is written when something SyncPodGroup puts into it differs,
// and then it is as wanted again. It is not written for what only looks
// different: a quantity in another notation, an empty collection, metadata
// and status, and fields that other writers set. Every write of a PodGroup
// makes the scheduler look at the group again; a write in every round would
// never let it rest.
func TestSyncPodGroupWritesWhatDiffersAndNothingElse(t *testing.T) {
	ms := policyModelServing("custom")
	wanted := createdPodGroup(t, ms)
	tests := []struct {
		name    string
		drift   func(*schedulingv1beta1.PodGroup)
		written bool
	}{
		{name: "nothing"},
		{name: "quantities in another notation", drift: func(p *schedulingv1beta1.PodGroup) {
			*p.Spec.MinResources = corev1.ResourceList{
				corev1.ResourceCPU: *resource.NewMilliQuantity(2000, resource.DecimalSI), corev1.ResourceMemory: resource.MustParse("2048Mi"),
				"example.com/device": resource.MustParse("4000m"),
			}
		}},
		{name: "empty collections", drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.MinTaskMember = map[string]int32{}
			p.Spec.SubGroupPolicy[0].LabelSelector.MatchExpressions = []metav1.LabelSelectorRequirement{}
		}},
		{name: "metadata and status", drift: func(p *schedulingv1beta1.PodGroup) {
			p.ResourceVersion = "new-version"
			p.Annotations = map[string]string{"other": "writer"}
			p.Status.Phase = schedulingv1beta1.PodGroupRunning
		}},
		{name: "fields of other writers", drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.MinTaskMember = map[string]int32{"external": 1}
			p.Spec.PriorityClassName = "external-priority"
		}},

		{name: "the minimum of members", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.MinMember = 3 }},
		{name: "the cpu", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			(*p.Spec.MinResources)[corev1.ResourceCPU] = resource.MustParse("3")
		}},
		{name: "the memory", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			(*p.Spec.MinResources)[corev1.ResourceMemory] = resource.MustParse("3Gi")
		}},
		{name: "an extended resource", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			(*p.Spec.MinResources)["example.com/device"] = resource.MustParse("5")
		}},
		{name: "a resource is missing", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			delete(*p.Spec.MinResources, "example.com/device")
		}},
		{name: "no resources", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.MinResources = nil }},
		{name: "the queue", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.Queue = "another" }},
		{name: "the mode of the group policy", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.NetworkTopology.Mode = schedulingv1beta1.SoftNetworkTopologyMode
		}},
		{name: "the tier of the group policy", written: true, drift: func(p *schedulingv1beta1.PodGroup) { *p.Spec.NetworkTopology.HighestTierAllowed++ }},
		{name: "no group policy", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.NetworkTopology = nil }},
		{name: "an empty group policy", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.NetworkTopology = &schedulingv1beta1.NetworkTopologySpec{}
		}},
		{name: "the name of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.SubGroupPolicy[0].Name = "different" }},
		{name: "the size of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { *p.Spec.SubGroupPolicy[0].SubGroupSize++ }},
		{name: "the minimum of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { *p.Spec.SubGroupPolicy[0].MinSubGroups++ }},
		{name: "no minimum of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.SubGroupPolicy[0].MinSubGroups = nil }},
		{name: "the selector of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0].LabelSelector.MatchLabels[workloadv1alpha1.RoleLabelKey] = "b"
		}},
		{name: "no selector of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.SubGroupPolicy[0].LabelSelector = nil }},
		{name: "an empty selector of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0].LabelSelector = &metav1.LabelSelector{}
		}},
		{name: "the mode of a Role policy", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0].NetworkTopology.Mode = schedulingv1beta1.HardNetworkTopologyMode
		}},
		{name: "the grouping key of a subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0].MatchLabelKeys[0] = "other"
		}},
		{name: "one more grouping key", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0].MatchLabelKeys = append(p.Spec.SubGroupPolicy[0].MatchLabelKeys, "other")
		}},
		{name: "one more subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy = append(p.Spec.SubGroupPolicy, schedulingv1beta1.SubGroupPolicySpec{})
		}},
		{name: "the order of the subgroups", written: true, drift: func(p *schedulingv1beta1.PodGroup) {
			p.Spec.SubGroupPolicy[0], p.Spec.SubGroupPolicy[1] = p.Spec.SubGroupPolicy[1], p.Spec.SubGroupPolicy[0]
		}},
		{name: "no subgroup", written: true, drift: func(p *schedulingv1beta1.PodGroup) { p.Spec.SubGroupPolicy = nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cached := wanted.DeepCopy()
			if tt.drift != nil {
				tt.drift(cached)
			}
			before := cached.DeepCopy()
			manager, client := managerWith(cached)
			manager.hasSubGroupPolicy.Store(true)
			mustSync(t, manager, ms)
			if !reflect.DeepEqual(cached, before) {
				t.Error("the PodGroup of the cache was changed")
			}
			if !tt.written {
				// Not by the first sync and not by a later one.
				mustSync(t, manager, ms)
				mustSync(t, manager, ms)
				if got := writesOf(client); len(got) != 0 {
					t.Errorf("writes = %v, want none", got)
				}
				return
			}
			if got := writesOf(client); !reflect.DeepEqual(got, []string{"update"}) {
				t.Fatalf("writes = %v, want one update", got)
			}
			if got := storedPodGroup(t, client).Spec; !apiequality.Semantic.DeepEqual(got, wanted.Spec) {
				t.Errorf("spec after the update:\n got %+v\nwant %+v", got, wanted.Spec)
			}
		})
	}
}

// An update changes what SyncPodGroup puts into a PodGroup and keeps what
// other writers put there.
func TestSyncPodGroupKeepsTheFieldsOfOtherWriters(t *testing.T) {
	for _, queue := range []string{"", schedulingv1beta1.DefaultQueue, "custom"} {
		t.Run("queue="+queue, func(t *testing.T) {
			ms := policyModelServing(queue)
			cached := createdPodGroup(t, ms)
			if queue == "" {
				// The server assigned the default queue.
				cached.Spec.Queue = schedulingv1beta1.DefaultQueue
			}
			cached.Spec.MinTaskMember = map[string]int32{"external": 1}
			cached.Spec.PriorityClassName = "external-priority"
			resources := cached.Spec.MinResources.DeepCopy()
			manager, client := managerWith(cached)
			manager.hasSubGroupPolicy.Store(true)
			for range 3 {
				mustSync(t, manager, ms)
			}
			if got := writesOf(client); len(got) != 0 {
				t.Fatalf("writes = %v; a PodGroup that is as wanted must not be written", got)
			}

			*ms.Spec.Template.Roles[0].Replicas = 2
			mustSync(t, manager, ms)
			if got := writesOf(client); !reflect.DeepEqual(got, []string{"update"}) {
				t.Fatalf("writes = %v, want one update", got)
			}
			updated := storedPodGroup(t, client)
			if updated.Spec.MinMember != 3 || *updated.Spec.SubGroupPolicy[0].MinSubGroups != 2 || *updated.Spec.SubGroupPolicy[1].MinSubGroups != 1 {
				t.Errorf("minMember %d, minimums of the subgroups %d and %d; want 3, 2 and 1",
					updated.Spec.MinMember, *updated.Spec.SubGroupPolicy[0].MinSubGroups, *updated.Spec.SubGroupPolicy[1].MinSubGroups)
			}
			if !reflect.DeepEqual(updated.Spec.MinTaskMember, map[string]int32{"external": 1}) || updated.Spec.PriorityClassName != "external-priority" {
				t.Errorf("fields of other writers after the update: %v, %q", updated.Spec.MinTaskMember, updated.Spec.PriorityClassName)
			}
			if updated.Spec.Queue != cached.Spec.Queue {
				t.Errorf("queue = %q, want %q", updated.Spec.Queue, cached.Spec.Queue)
			}
			// Three instances where there were two: half as much again.
			for name, quantity := range resources {
				quantity.Mul(3)
				actual := (*updated.Spec.MinResources)[name]
				doubled := actual.DeepCopy()
				doubled.Mul(2)
				if doubled.Cmp(quantity) != 0 {
					t.Errorf("%s = %s, want half as much again as before", name, actual.String())
				}
			}
		})
	}
}
