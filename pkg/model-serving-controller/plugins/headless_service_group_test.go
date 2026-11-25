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

package plugins

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// groupRequest returns a request for the Role instance serve-0 of the
// ServingGroup test-ms-0 at the given generation.
func groupRequest(client *kubefake.Clientset, generation int64) *HookRequest {
	req := headlessTestRequest()
	req.Cleanup = nil
	req.KubeClient = client
	req.Generation = generation
	req.Owner = &metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ServingGroupKind.Kind,
		Name: "test-ms-0", UID: types.UID("sg-uid"), Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
	return req
}

// serviceControllers returns, by Service name, the UID of its controller ("" for none),
// the UID of the Service and the generation it carries.
func servicesOf(t *testing.T, client *kubefake.Clientset) map[string][3]string {
	t.Helper()
	list, err := client.CoreV1().Services("default").List(context.Background(), metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][3]string{}
	for i := range list.Items {
		service := &list.Items[i]
		controller := ""
		if owner := metav1.GetControllerOf(service); owner != nil {
			controller = string(owner.UID)
		}
		out[service.Name] = [3]string{controller, string(service.UID), service.Labels[workloadv1alpha1.InstanceGenerationLabelKey]}
	}
	return out
}

func TestHeadlessServiceOfAGroup(t *testing.T) {
	ctx := context.Background()
	plugin, err := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
	if err != nil {
		t.Fatal(err)
	}
	const entry, discovery = "test-ms-0-serve-0-0", "test-ms-0-serve-0-0-discovery"

	// Services made before groups were objects: the ModelServing controls them.
	legacyEntry := headlessTestService(headlessTestRequest())
	legacyEntry.UID = "entry-uid"
	legacyDiscovery := legacyEntry.DeepCopy()
	legacyDiscovery.Name, legacyDiscovery.UID = discovery, "discovery-uid"
	client := kubefake.NewSimpleClientset(legacyEntry, legacyDiscovery)

	// Sync hands them over to the group. They keep their UIDs, so DNS does not change.
	if err := plugin.OnRoleSync(ctx, groupRequest(client, 0)); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{entry: {"sg-uid", "entry-uid", "0"}, discovery: {"sg-uid", "discovery-uid", "0"}}
	if got := servicesOf(t, client); got[entry] != want[entry] || got[discovery] != want[discovery] {
		t.Fatalf("after the hand-over: %v, want %v", got, want)
	}
	client.ClearActions()
	if err := plugin.OnRoleSync(ctx, groupRequest(client, 0)); err != nil {
		t.Fatal(err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() != "get" && action.GetVerb() != "list" {
			t.Errorf("a sync without a change wrote: %v", action)
		}
	}

	// The instance is recreated at generation 1 and its Services follow.
	if err := plugin.OnRoleSync(ctx, groupRequest(client, 1)); err != nil {
		t.Fatal(err)
	}
	if got := servicesOf(t, client); got[entry][2] != "1" || got[discovery][2] != "1" {
		t.Fatalf("generations after the sync of generation 1: %v", got)
	}

	// A deletion decided for generation 0 that is retried late, or by a process
	// with a stale view, does not remove the Services of generation 1.
	if err := plugin.OnRoleDelete(ctx, groupRequest(client, 0)); err != nil {
		t.Fatal(err)
	}
	if got := servicesOf(t, client); len(got) != 2 {
		t.Fatalf("a deletion for generation 0 removed Services of generation 1: %v", got)
	}
	if err := plugin.OnRoleDelete(ctx, groupRequest(client, 1)); err != nil {
		t.Fatal(err)
	}
	if got := servicesOf(t, client); len(got) != 0 {
		t.Fatalf("after the deletion for generation 1: %v, want none", got)
	}

	// Created anew, the Services are controlled by the group from the start.
	if err := plugin.OnRoleSync(ctx, groupRequest(client, 2)); err != nil {
		t.Fatal(err)
	}
	if got := servicesOf(t, client); got[entry][0] != "sg-uid" || got[entry][2] != "2" || got[discovery][0] != "sg-uid" {
		t.Fatalf("created for the group: %v", got)
	}
	created, _ := client.CoreV1().Services("default").Get(ctx, entry, metav1.GetOptions{})
	if owner := metav1.GetControllerOf(created); owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		t.Error("the Service does not block the deletion of its group")
	}
}

func TestHeadlessServiceOfAGroupIdentity(t *testing.T) {
	ctx := context.Background()
	plugin, _ := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
	const entry = "test-ms-0-serve-0-0"
	controller := func(kind, name string, uid types.UID) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: kind, Name: name, UID: uid, Controller: ptr.To(true),
		}}
	}

	tests := []struct {
		name    string
		mutate  func(*corev1.Service)
		managed bool
	}{
		{"controlled by the group", func(s *corev1.Service) { s.OwnerReferences = controller("ServingGroup", "test-ms-0", "sg-uid") }, true},
		{"controlled by the ModelServing", func(*corev1.Service) {}, true},
		{"left without a controller, labelled by the plugin", func(s *corev1.Service) { s.OwnerReferences = nil }, true},
		{"without a controller and without the label of the plugin", func(s *corev1.Service) {
			s.OwnerReferences = nil
			delete(s.Labels, HeadlessServicePluginLabelKey)
		}, false},
		{"controlled by an earlier group of the same name", func(s *corev1.Service) {
			s.OwnerReferences = controller("ServingGroup", "test-ms-0", "old-sg-uid")
		}, false},
		{"controlled by another ModelServing of the same name", func(s *corev1.Service) {
			s.OwnerReferences = controller("ModelServing", "test-ms", "other-ms-uid")
		}, false},
		{"labelled for another Role instance", func(s *corev1.Service) { s.Labels[workloadv1alpha1.RoleIDKey] = "serve-1" }, false},
		{"not headless", func(s *corev1.Service) { s.Spec.ClusterIP = "10.0.0.1" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			service := headlessTestService(headlessTestRequest())
			service.UID = "entry-uid"
			tt.mutate(service)
			client := kubefake.NewSimpleClientset(service)
			req := groupRequest(client, 3)

			// A Service that is not ours blocks the sync and survives the deletion.
			err := plugin.OnRoleSync(ctx, req)
			if (err == nil) != tt.managed {
				t.Errorf("OnRoleSync() error = %v, want managed = %t", err, tt.managed)
			}
			if err := plugin.OnRoleDelete(ctx, req); err != nil {
				t.Fatal(err)
			}
			_, kept := servicesOf(t, client)[entry]
			if kept == tt.managed {
				t.Errorf("after OnRoleDelete the Service exists = %t, want %t", kept, !tt.managed)
			}
		})
	}
}

func TestHeadlessLegacyWorkerOfAGroupBlocksSync(t *testing.T) {
	plugin, _ := NewHeadlessServicePlugin(workloadv1alpha1.PluginSpec{Name: HeadlessServicePluginName})
	// A worker created by an older layout uses the entry Service as its
	// subdomain. It counts whether the ModelServing or the group controls it.
	worker := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "test-ms-0-serve-0-1",
			Labels: map[string]string{
				workloadv1alpha1.GroupNameLabelKey: "test-ms-0",
				workloadv1alpha1.RoleLabelKey:      "serve",
				workloadv1alpha1.RoleIDKey:         "serve-0",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: "ServingGroup", Name: "test-ms-0", UID: "sg-uid", Controller: ptr.To(true),
			}},
		},
		Spec: corev1.PodSpec{Subdomain: "test-ms-0-serve-0-0"},
	}
	client := kubefake.NewSimpleClientset(worker)
	if err := plugin.OnRoleSync(context.Background(), groupRequest(client, 0)); err == nil {
		t.Error("OnRoleSync() succeeded although a worker of the group still uses the legacy subdomain")
	}
	if got := servicesOf(t, client); len(got) != 0 {
		t.Errorf("Services were written before the check: %v", got)
	}
}
