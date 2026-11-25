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
	"errors"
	"testing"

	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	volcanoschedulerlister "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func groupOwner() metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ServingGroupKind.Kind,
		Name: "test-ms-0", UID: types.UID("sg-uid"), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

// groupManager returns a manager whose cache holds cached and whose server holds stored.
func groupManager(cached, stored []*schedulingv1beta1.PodGroup) (*Manager, *volcanofake.Clientset) {
	client := volcanofake.NewSimpleClientset()
	for _, podGroup := range stored {
		_ = client.Tracker().Add(podGroup)
	}
	manager := NewManager(nil, client, apiextfake.NewSimpleClientset(), nil)
	manager.hasPodGroupCRD.Store(true)
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	for _, podGroup := range cached {
		_ = indexer.Add(podGroup)
	}
	manager.PodGroupLister = volcanoschedulerlister.NewPodGroupLister(indexer)
	return manager, client
}

func existingPodGroup(mutate ...func(*schedulingv1beta1.PodGroup)) *schedulingv1beta1.PodGroup {
	podGroup := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "test-ms-0", UID: types.UID("pg-uid"), ResourceVersion: "5",
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: "test-ms",
				workloadv1alpha1.GroupNameLabelKey:        "test-ms-0",
			},
			OwnerReferences: []metav1.OwnerReference{groupOwner()},
		},
		Spec: schedulingv1beta1.PodGroupSpec{MinMember: 5, Queue: "assigned"},
	}
	for _, m := range mutate {
		m(podGroup)
	}
	return podGroup
}

func controlledBy(kind, name string, uid types.UID) func(*schedulingv1beta1.PodGroup) {
	return func(podGroup *schedulingv1beta1.PodGroup) {
		podGroup.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: ptr.To(true),
		}}
	}
}

func TestSyncPodGroup(t *testing.T) {
	ms := newMinimalMS("")
	ms.UID = "ms-uid"
	const want = int32(1) // newMinimalMS: one Role, one instance, an entry Pod alone

	byModelServing := controlledBy(workloadv1alpha1.ModelServingKind.Kind, "test-ms", "ms-uid")
	orphaned := func(podGroup *schedulingv1beta1.PodGroup) { podGroup.OwnerReferences = nil }
	withWatcher := func(podGroup *schedulingv1beta1.PodGroup) {
		podGroup.OwnerReferences = append(podGroup.OwnerReferences, metav1.OwnerReference{Kind: "Watcher", Name: "w", UID: "w-uid"})
	}

	tests := []struct {
		name           string
		cached, stored *schedulingv1beta1.PodGroup
		occupied       bool
		retry          bool
		// owners is the UIDs of the owners afterwards, the controller first.
		owners    []types.UID
		minMember int32
	}{
		{name: "none: created with the group as its controller",
			owners: []types.UID{"sg-uid"}, minMember: want},
		{name: "controlled by the group: updated in place",
			cached: existingPodGroup(), stored: existingPodGroup(),
			owners: []types.UID{"sg-uid"}, minMember: want},
		{name: "controlled by the ModelServing: handed over to the group",
			cached: existingPodGroup(byModelServing), stored: existingPodGroup(byModelServing),
			owners: []types.UID{"sg-uid"}, minMember: want},
		{name: "handed over: owners that are not controllers stay",
			cached: existingPodGroup(byModelServing, withWatcher), stored: existingPodGroup(byModelServing, withWatcher),
			owners: []types.UID{"sg-uid", "w-uid"}, minMember: want},
		{name: "left behind without a controller: handed over to the group",
			cached: existingPodGroup(orphaned), stored: existingPodGroup(orphaned),
			owners: []types.UID{"sg-uid"}, minMember: want},
		{name: "without a controller and without the labels of the group: occupied",
			cached:   existingPodGroup(orphaned, func(p *schedulingv1beta1.PodGroup) { p.Labels = nil }),
			stored:   existingPodGroup(orphaned, func(p *schedulingv1beta1.PodGroup) { p.Labels = nil }),
			occupied: true, retry: true, minMember: 5},
		{name: "controlled by an earlier group of the same name: occupied",
			cached:   existingPodGroup(controlledBy("ServingGroup", "test-ms-0", "old-sg-uid")),
			stored:   existingPodGroup(controlledBy("ServingGroup", "test-ms-0", "old-sg-uid")),
			occupied: true, retry: true, owners: []types.UID{"old-sg-uid"}, minMember: 5},
		{name: "controlled by another ModelServing: occupied",
			cached:   existingPodGroup(controlledBy("ModelServing", "test-ms", "other-ms-uid")),
			stored:   existingPodGroup(controlledBy("ModelServing", "test-ms", "other-ms-uid")),
			occupied: true, retry: true, owners: []types.UID{"other-ms-uid"}, minMember: 5},
		{name: "not in the cache but on the server and ours: updated",
			stored: existingPodGroup(),
			owners: []types.UID{"sg-uid"}, minMember: want},
		{name: "not in the cache but on the server and someone else's: occupied",
			stored:   existingPodGroup(controlledBy("ServingGroup", "test-ms-0", "old-sg-uid")),
			occupied: true, retry: true, owners: []types.UID{"old-sg-uid"}, minMember: 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var cached, stored []*schedulingv1beta1.PodGroup
			if tt.cached != nil {
				cached = append(cached, tt.cached)
			}
			if tt.stored != nil {
				stored = append(stored, tt.stored)
			}
			manager, client := groupManager(cached, stored)

			retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), "test-ms-0")
			if errors.Is(err, ErrNameOccupied) != tt.occupied || (err != nil) != tt.occupied {
				t.Fatalf("SyncPodGroup() error = %v, want occupied = %t", err, tt.occupied)
			}
			if (retry > 0) != tt.retry {
				t.Errorf("retry after %v, want a retry = %t", retry, tt.retry)
			}

			got, err := client.SchedulingV1beta1().PodGroups("default").Get(context.Background(), "test-ms-0", metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			var owners []types.UID
			for _, reference := range got.OwnerReferences {
				owners = append(owners, reference.UID)
			}
			if len(owners) != len(tt.owners) {
				t.Fatalf("owners = %v, want %v", owners, tt.owners)
			}
			for i := range owners {
				if owners[i] != tt.owners[i] {
					t.Errorf("owners = %v, want %v", owners, tt.owners)
				}
			}
			if !tt.occupied {
				if controller := metav1.GetControllerOf(got); controller == nil || controller.UID != "sg-uid" ||
					controller.BlockOwnerDeletion == nil || !*controller.BlockOwnerDeletion {
					t.Errorf("controller = %+v, want the group, blocking its deletion", controller)
				}
			}
			if got.Spec.MinMember != tt.minMember {
				t.Errorf("minMember = %d, want %d", got.Spec.MinMember, tt.minMember)
			}
			if tt.stored != nil && (got.UID != "pg-uid" || got.Spec.Queue != "assigned") {
				t.Errorf("the PodGroup has UID %s and queue %s, want it to keep both", got.UID, got.Spec.Queue)
			}
		})
	}
}

func TestSyncPodGroupDoesNothingWithoutAChange(t *testing.T) {
	ms := newMinimalMS("")
	ms.UID = "ms-uid"
	manager, client := groupManager(nil, nil)
	if _, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), "test-ms-0"); err != nil {
		t.Fatal(err)
	}
	created, _ := client.SchedulingV1beta1().PodGroups("default").Get(context.Background(), "test-ms-0", metav1.GetOptions{})
	manager, client = groupManager([]*schedulingv1beta1.PodGroup{created}, []*schedulingv1beta1.PodGroup{created})
	client.ClearActions()

	if _, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), "test-ms-0"); err != nil {
		t.Fatal(err)
	}
	if actions := client.Actions(); len(actions) != 0 {
		t.Errorf("a PodGroup that is as wanted was written: %v", actions)
	}
}

func TestSyncPodGroupWaitsOutADeletion(t *testing.T) {
	ms := newMinimalMS("")
	ms.UID = "ms-uid"
	deleting := existingPodGroup(func(p *schedulingv1beta1.PodGroup) { p.DeletionTimestamp = ptr.To(metav1.Now()) })
	manager, client := groupManager([]*schedulingv1beta1.PodGroup{deleting}, []*schedulingv1beta1.PodGroup{deleting})
	client.ClearActions()

	retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), "test-ms-0")
	if err == nil || retry <= 0 || len(client.Actions()) != 0 {
		t.Errorf("SyncPodGroup() = %v, %v with writes %v, want an error, a retry and no write", retry, err, client.Actions())
	}
}

func TestSyncPodGroupWithoutVolcano(t *testing.T) {
	ms := newMinimalMS("")
	ms.Spec.SchedulerName = "default-scheduler"
	manager, client := groupManager(nil, nil)
	client.ClearActions()
	if manager.Enabled(ms) {
		t.Error("Enabled() = true for a scheduler other than volcano")
	}
	if retry, err := manager.SyncPodGroup(context.Background(), ms, groupOwner(), "test-ms-0"); err != nil || retry != 0 || len(client.Actions()) != 0 {
		t.Errorf("SyncPodGroup() = %v, %v with writes %v, want nothing", retry, err, client.Actions())
	}
}
