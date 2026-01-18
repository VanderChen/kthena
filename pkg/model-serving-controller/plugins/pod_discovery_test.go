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
	"encoding/json"
	"fmt"
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

func TestPodDiscoveryPluginOnPodCreate(t *testing.T) {
	plugin, err := NewPodDiscoveryPlugin(workloadv1alpha1.PluginSpec{
		Name: PodDiscoveryPluginName,
		Type: workloadv1alpha1.PluginTypeBuiltIn,
	})
	if err != nil {
		t.Fatalf("new plugin: %v", err)
	}

	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "c1"}},
			InitContainers: []corev1.Container{{Name: "init-c1"}},
		},
	}
	req := &HookRequest{
		ServingGroup: "group-1",
		Pod:          pod,
	}

	if err := plugin.OnPodCreate(context.Background(), req); err != nil {
		t.Fatalf("on create: %v", err)
	}

	// Verify Volume
	foundVol := false
	expectedVolName := DiscoveryVolumeName
	expectedCMName := "group-1" + ConfigMapNameSuffix
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == expectedVolName {
			foundVol = true
			if vol.ConfigMap == nil {
				t.Errorf("volume configMap is nil")
			} else {
				if vol.ConfigMap.Name != expectedCMName {
					t.Errorf("expected CM name %s, got %s", expectedCMName, vol.ConfigMap.Name)
				}
				if vol.ConfigMap.Optional == nil || !*vol.ConfigMap.Optional {
					t.Errorf("expected optional to be true")
				}
			}
			break
		}
	}
	if !foundVol {
		t.Errorf("volume %s not found", expectedVolName)
	}

	// Verify VolumeMount in Container
	verifyMount := func(containers []corev1.Container, kind string) {
		for _, c := range containers {
			foundMount := false
			for _, m := range c.VolumeMounts {
				if m.Name == expectedVolName {
					foundMount = true
					if m.MountPath != DiscoveryMountPath {
						t.Errorf("expected mount path %s, got %s in %s", DiscoveryMountPath, m.MountPath, kind)
					}
					if !m.ReadOnly {
						t.Errorf("expected readonly mount in %s", kind)
					}
					break
				}
			}
			if !foundMount {
				t.Errorf("volume mount %s not found in %s", expectedVolName, kind)
			}
		}
	}

	verifyMount(pod.Spec.Containers, "containers")
	verifyMount(pod.Spec.InitContainers, "init containers")
}

func TestPodDiscoveryPluginOnPodReady(t *testing.T) {
	namespace := "default"
	servingGroup := "group-1"
	mi := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model-1",
			Namespace: namespace,
		},
	}

	// Create a fake client with some existing pods
	existingPods := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-1",
				Namespace: namespace,
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: servingGroup,
					workloadv1alpha1.RoleLabelKey:      "worker",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.1",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-2",
				Namespace: namespace,
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: servingGroup,
					workloadv1alpha1.RoleLabelKey:      "worker",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.2",
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-3",
				Namespace: namespace,
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: servingGroup,
					workloadv1alpha1.RoleLabelKey:      "master",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.3",
			},
		},
		// A pod that is not ready (no IP)
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-4",
				Namespace: namespace,
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: servingGroup,
					workloadv1alpha1.RoleLabelKey:      "worker",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodPending,
			},
		},
		// A pod from another group
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pod-other",
				Namespace: namespace,
				Labels: map[string]string{
					workloadv1alpha1.GroupNameLabelKey: "other-group",
					workloadv1alpha1.RoleLabelKey:      "worker",
				},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				PodIP: "10.0.0.100",
			},
		},
	}

	client := fake.NewSimpleClientset(existingPods...)

	plugin, _ := NewPodDiscoveryPlugin(workloadv1alpha1.PluginSpec{Name: PodDiscoveryPluginName})

	// Current pod becoming ready
	currentPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: namespace,
			Labels: map[string]string{
				workloadv1alpha1.GroupNameLabelKey: servingGroup,
				workloadv1alpha1.RoleLabelKey:      "worker",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.0.0.1",
		},
	}

	req := &HookRequest{
		ModelServing: mi,
		ServingGroup: servingGroup,
		Pod:          currentPod,
		KubeClient:   client,
	}

	if err := plugin.OnPodReady(context.Background(), req); err != nil {
		t.Fatalf("on ready: %v", err)
	}

	// Verify ConfigMap creation
	cmName := servingGroup + ConfigMapNameSuffix
	cm, err := client.CoreV1().ConfigMaps(namespace).Get(context.Background(), cmName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get configmap %s: %v", cmName, err)
	}

	jsonStr := cm.Data[DiscoveryFileName]
	if jsonStr == "" {
		t.Fatalf("configmap data empty")
	}

	var ipMap map[string][]string
	if err := json.Unmarshal([]byte(jsonStr), &ipMap); err != nil {
		t.Fatalf("failed to unmarshal json: %v", err)
	}

	// Verify content
	if len(ipMap) != 2 {
		t.Errorf("expected 2 roles, got %d", len(ipMap))
	}

	// Worker IPs
	workers := ipMap["worker"]
	if len(workers) != 2 {
		t.Errorf("expected 2 worker ips, got %d", len(workers))
	}
	// "10.0.0.1", "10.0.0.2" (should be sorted)
	if workers[0] != "10.0.0.1" || workers[1] != "10.0.0.2" {
		t.Errorf("unexpected worker ips: %v", workers)
	}

	// Master IPs
	masters := ipMap["master"]
	if len(masters) != 1 {
		t.Errorf("expected 1 master ip, got %d", len(masters))
	}
	if masters[0] != "10.0.0.3" {
		t.Errorf("unexpected master ip: %v", masters)
	}
}

// TestPodDiscoveryPluginRetry checks that the plugin retries on conflict
func TestPodDiscoveryPluginRetry(t *testing.T) {
	namespace := "default"
	servingGroup := "group-retry"
	cmName := servingGroup + ConfigMapNameSuffix
	mi := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: namespace}}

	client := fake.NewSimpleClientset()
	
	// Inject a Conflict error on the first Update call
	conflictCount := 0
	client.PrependReactor("update", "configmaps", func(action coretesting.Action) (handled bool, ret runtime.Object, err error) {
		updateAction := action.(coretesting.UpdateAction)
		cm := updateAction.GetObject().(*corev1.ConfigMap)
		if cm.Name == cmName {
			if conflictCount == 0 {
				conflictCount++
				return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "", Resource: "configmaps"}, cmName, fmt.Errorf("conflict"))
			}
		}
		return false, nil, nil
	})

	// Pre-create the ConfigMap so we trigger the Update path
	existingCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cmName,
			Namespace: namespace,
		},
		Data: map[string]string{
			DiscoveryFileName: "{}",
		},
	}
	client.CoreV1().ConfigMaps(namespace).Create(context.Background(), existingCM, metav1.CreateOptions{})

	// Add a pod
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-1",
			Namespace: namespace,
			Labels: map[string]string{
				workloadv1alpha1.GroupNameLabelKey: servingGroup,
				workloadv1alpha1.RoleLabelKey:      "role",
			},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "1.1.1.1"},
	}
	client.CoreV1().Pods(namespace).Create(context.Background(), pod, metav1.CreateOptions{})

	plugin, _ := NewPodDiscoveryPlugin(workloadv1alpha1.PluginSpec{Name: PodDiscoveryPluginName})
	req := &HookRequest{
		ModelServing: mi,
		ServingGroup: servingGroup,
		Pod:          pod,
		KubeClient:   client,
	}

	// This should succeed eventually despite the injected conflict
	if err := plugin.OnPodReady(context.Background(), req); err != nil {
		t.Fatalf("on ready with retry failed: %v", err)
	}

	if conflictCount != 1 {
		t.Errorf("expected 1 conflict, got %d", conflictCount)
	}
}
