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
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	coretesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func discoveryMaintenanceFixture(t *testing.T) (*Chain, *HookRequest, *fake.Clientset) {
	t.Helper()
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "owner"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "model-0-worker-0", Namespace: ms.Namespace, UID: "pod",
			Labels: map[string]string{workloadv1alpha1.GroupNameLabelKey: "model-0", workloadv1alpha1.RoleLabelKey: "worker"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1"},
	}
	client := fake.NewSimpleClientset(pod)
	chain, err := NewChain(DefaultRegistry, []workloadv1alpha1.PluginSpec{{Name: PodDiscoveryPluginName, Type: workloadv1alpha1.PluginTypeBuiltIn}})
	if err != nil {
		t.Fatal(err)
	}
	return chain, &HookRequest{ModelServing: ms, ServingGroup: "model-0", RoleName: "worker", Pod: pod, KubeClient: client}, client
}

func TestDiscoveryMaintenancePublishesNotReadyWithoutRepeatingReady(t *testing.T) {
	chain, req, client := discoveryMaintenanceFixture(t)
	selected, err := chain.MaintainPodDiscovery(context.Background(), req)
	if !selected || err != nil {
		t.Fatalf("Running/NotReady maintenance: selected=%v err=%v", selected, err)
	}
	cm, err := client.CoreV1().ConfigMaps(req.ModelServing.Namespace).Get(context.Background(), req.ServingGroup+ConfigMapNameSuffix, metav1.GetOptions{})
	if err != nil || cm.Data[DiscoveryFileName] != `{"worker":["10.0.0.1"]}` || !metav1.IsControlledBy(cm, req.ModelServing) {
		t.Fatalf("unexpected output: cm=%+v err=%v", cm, err)
	}
	client.ClearActions()
	req.Pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if !chain.PodDiscoveryEligible(req) {
		t.Fatal("Ready trigger should retain original hook qualification")
	}
	selected, err = chain.MaintainPodDiscovery(context.Background(), req)
	if selected || err != nil || len(client.Actions()) != 0 {
		t.Fatalf("Ready must use only old Ready hook: selected=%v err=%v actions=%v", selected, err, client.Actions())
	}
}

func TestDiscoveryMaintenanceScopeAndRetirement(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*HookRequest)
	}{
		{"role-excluded", func(req *HookRequest) { req.RoleName = "other" }},
		{"target-excluded", func(req *HookRequest) { req.IsEntry = true }},
		{"terminating", func(req *HookRequest) { now := metav1.Now(); req.Pod.DeletionTimestamp = &now }},
		{"pending", func(req *HookRequest) { req.Pod.Status.Phase = corev1.PodPending }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chain, req, client := discoveryMaintenanceFixture(t)
			chain.entries[0].spec.Scope = &workloadv1alpha1.PluginScope{Roles: []string{"worker"}, Target: workloadv1alpha1.PluginTargetWorker}
			tc.mutate(req)
			selected, err := chain.MaintainPodDiscovery(context.Background(), req)
			if selected || err != nil || len(client.Actions()) != 0 {
				t.Fatalf("ineligible retry performed work: selected=%v err=%v actions=%v", selected, err, client.Actions())
			}
		})
	}
}

func TestDiscoveryMaintenanceFailureDoesNotChangeRunningChain(t *testing.T) {
	chain, req, client := discoveryMaintenanceFixture(t)
	var calls []string
	chain.entries = append(chain.entries, entry{plugin: &testPlugin{name: "later", calls: &calls}})
	client.PrependReactor("list", "pods", func(coretesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("discovery unavailable")
	})
	if err := chain.OnPodRunning(context.Background(), req); err != nil || strings.Join(calls, ",") != "running-later" {
		t.Fatalf("original later Running hook lost opportunity: calls=%v err=%v", calls, err)
	}
	for i := 0; i < 2; i++ {
		selected, err := chain.MaintainPodDiscovery(context.Background(), req)
		if !selected || !apierrors.IsServiceUnavailable(err) {
			t.Fatalf("maintenance result: selected=%v err=%v", selected, err)
		}
	}
	if strings.Join(calls, ",") != "running-later" {
		t.Fatalf("maintenance replayed the original chain: %v", calls)
	}
}

func TestDiscoveryMaintenanceRejectsForeignConfigMap(t *testing.T) {
	for _, owned := range []bool{false, true} {
		t.Run(map[bool]string{false: "unowned", true: "foreign-owner"}[owned], func(t *testing.T) {
			chain, req, client := discoveryMaintenanceFixture(t)
			cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: req.ServingGroup + ConfigMapNameSuffix, Namespace: req.ModelServing.Namespace, UID: "cm", ResourceVersion: "1"}, Data: map[string]string{DiscoveryFileName: "original"}}
			if owned {
				other := req.ModelServing.DeepCopy()
				other.UID = "other-owner"
				cm.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(other, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))}
			}
			if err := client.Tracker().Add(cm); err != nil {
				t.Fatal(err)
			}
			selected, err := chain.MaintainPodDiscovery(context.Background(), req)
			if !selected || err == nil {
				t.Fatalf("foreign output accepted: selected=%v err=%v", selected, err)
			}
			for _, action := range client.Actions() {
				if action.GetVerb() == "update" {
					t.Fatalf("foreign output updated: %v", action)
				}
			}
			// The independent entry's new guard does not redefine the old Ready hook.
			if err := chain.OnPodReady(context.Background(), req); err != nil {
				t.Fatalf("old Ready behavior changed: %v", err)
			}
		})
	}
}

func TestDiscoveryMaintenanceRechecksOwnerAfterConflict(t *testing.T) {
	chain, req, client := discoveryMaintenanceFixture(t)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
		Name: req.ServingGroup + ConfigMapNameSuffix, Namespace: req.ModelServing.Namespace, UID: "cm-original", ResourceVersion: "7",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(req.ModelServing, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))},
	}, Data: map[string]string{DiscoveryFileName: "original"}}
	if err := client.Tracker().Add(cm); err != nil {
		t.Fatal(err)
	}
	updates := 0
	client.PrependReactor("update", "configmaps", func(action coretesting.Action) (bool, runtime.Object, error) {
		updates++
		update := action.(coretesting.UpdateAction).GetObject().(*corev1.ConfigMap)
		if update.UID != cm.UID || update.ResourceVersion != cm.ResourceVersion {
			t.Fatalf("update discarded GET identity: %+v", update.ObjectMeta)
		}
		replacement := cm.DeepCopy()
		replacement.UID = "cm-replacement"
		replacement.ResourceVersion = "8"
		replacement.OwnerReferences = nil
		if err := client.Tracker().Update(corev1.SchemeGroupVersion.WithResource("configmaps"), replacement, cm.Namespace); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, assertError)
	})
	selected, err := chain.MaintainPodDiscovery(context.Background(), req)
	if !selected || err == nil || updates != 1 {
		t.Fatalf("retry adopted replacement: selected=%v err=%v updates=%d", selected, err, updates)
	}
}

func TestDiscoveryMaintenanceRechecksOwnerAfterCreateRace(t *testing.T) {
	chain, req, client := discoveryMaintenanceFixture(t)
	client.PrependReactor("create", "configmaps", func(action coretesting.Action) (bool, runtime.Object, error) {
		created := action.(coretesting.CreateAction).GetObject().(*corev1.ConfigMap).DeepCopy()
		created.OwnerReferences = nil
		created.UID = "foreign-cm"
		if err := client.Tracker().Add(created); err != nil {
			t.Fatal(err)
		}
		return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, created.Name)
	})
	selected, err := chain.MaintainPodDiscovery(context.Background(), req)
	if !selected || err == nil {
		t.Fatalf("create race adopted foreign CM: selected=%v err=%v", selected, err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" {
			t.Fatalf("foreign CM was updated: %v", action)
		}
	}
}
