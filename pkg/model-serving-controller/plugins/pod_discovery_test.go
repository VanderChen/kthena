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
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	coretesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func discoveryFixture(t *testing.T, config string, objects ...runtime.Object) (Plugin, *HookRequest, *fake.Clientset) {
	t.Helper()
	spec := workloadv1alpha1.PluginSpec{Name: PodDiscoveryPluginName}
	if config != "" {
		spec.Config = &apiextensionsv1.JSON{Raw: []byte(config)}
	}
	p, err := NewPodDiscoveryPlugin(spec)
	if err != nil {
		t.Fatal(err)
	}
	client := fake.NewSimpleClientset(objects...)
	ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "default", UID: "model-uid"}}
	ms.Status.UpdateRevision = "v2"
	return p, &HookRequest{ModelServing: ms, ServingGroup: "group-1", Pod: discoveryPod("pod-1", "worker", "instance-1"), KubeClient: client}, client
}

func discoveryPod(name, role, instance string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: map[string]string{
			workloadv1alpha1.GroupNameLabelKey: "group-1", workloadv1alpha1.RoleLabelKey: role,
			workloadv1alpha1.RoleIDKey: instance, workloadv1alpha1.RevisionLabelKey: "v2",
		}},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main"}}, InitContainers: []corev1.Container{{Name: "init"}}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.1", HostIP: "192.0.2.1",
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}},
	}
}

func discoveryCM(data string) *corev1.ConfigMap {
	return &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "group-1-groupinfo", Namespace: "default"},
		Data: map[string]string{"groupinfo.config": data, "unrelated": "preserved"}}
}

func readDiscoveryCM(t *testing.T, client *fake.Clientset) *corev1.ConfigMap {
	t.Helper()
	cm, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "group-1-groupinfo", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return cm
}

func assertNoDiscoveryWrites(t *testing.T, client *fake.Clientset) {
	t.Helper()
	for _, action := range client.Actions() {
		if action.Matches("create", "configmaps") || action.Matches("update", "configmaps") || action.Matches("delete", "configmaps") {
			t.Fatalf("unexpected write: %v", action)
		}
	}
}

func TestPodDiscoveryPluginOnPodCreate(t *testing.T) {
	for _, tc := range []struct {
		name, config, target, revision string
		mounted                        bool
	}{
		{"no config", "", "v2", "v2", false},
		{"empty config", `{}`, "v2", "v2", false},
		{"current revision", `{"autoMountPath":"/etc/groupinfo"}`, "v2", "v2", true},
		{"old revision", `{"autoMountPath":"/etc/groupinfo"}`, "v2", "v1", false},
		{"empty target falls back", `{"autoMountPath":"/etc/groupinfo"}`, "", "v1", true},
		{"both revisions empty", `{"autoMountPath":"/etc/groupinfo"}`, "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, req, _ := discoveryFixture(t, tc.config)
			req.ModelServing.Status.UpdateRevision = tc.target
			req.Pod.Labels[workloadv1alpha1.RevisionLabelKey] = tc.revision
			before := req.Pod.DeepCopy()
			for i := 0; i < 2; i++ {
				if err := p.OnPodCreate(context.Background(), req); err != nil {
					t.Fatal(err)
				}
			}
			if !tc.mounted {
				if !reflect.DeepEqual(before, req.Pod) {
					t.Fatal("skipped hook mutated Pod")
				}
				return
			}
			if len(req.Pod.Spec.Volumes) != 1 {
				t.Fatalf("volumes: %#v", req.Pod.Spec.Volumes)
			}
			v := req.Pod.Spec.Volumes[0]
			if v.Name != "groupinfo-volume" || v.ConfigMap == nil || v.ConfigMap.Name != "group-1-groupinfo" || v.ConfigMap.Optional == nil || !*v.ConfigMap.Optional {
				t.Fatalf("volume: %#v", v)
			}
			for _, c := range append(req.Pod.Spec.Containers, req.Pod.Spec.InitContainers...) {
				want := []corev1.VolumeMount{{Name: "groupinfo-volume", MountPath: "/etc/groupinfo", ReadOnly: true}}
				if !reflect.DeepEqual(c.VolumeMounts, want) {
					t.Fatalf("mounts: %#v", c.VolumeMounts)
				}
			}
		})
	}
	t.Run("existing same-name configuration retained", func(t *testing.T) {
		p, req, _ := discoveryFixture(t, `{"autoMountPath":"/new"}`)
		req.Pod.Spec.Volumes = []corev1.Volume{{Name: "groupinfo-volume", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
		req.Pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "groupinfo-volume", MountPath: "/existing"}}
		if err := p.OnPodCreate(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(req.Pod.Spec.Volumes) != 1 || req.Pod.Spec.Volumes[0].EmptyDir == nil || len(req.Pod.Spec.Containers[0].VolumeMounts) != 1 || req.Pod.Spec.Containers[0].VolumeMounts[0].MountPath != "/existing" {
			t.Fatal("existing configuration replaced")
		}
	})
}

func TestPodDiscoveryPluginInvalidConfig(t *testing.T) {
	for _, raw := range []string{`{`, `{"autoMountPath":17}`} {
		if _, err := NewPodDiscoveryPlugin(workloadv1alpha1.PluginSpec{Name: PodDiscoveryPluginName, Config: &apiextensionsv1.JSON{Raw: []byte(raw)}}); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestPodDiscoveryPluginOnPodRunning(t *testing.T) {
	pods := []*corev1.Pod{discoveryPod("z-worker", "worker", "instance-2"), discoveryPod("b-worker", "worker", "instance-1"), discoveryPod("a-worker", "worker", "instance-1"), discoveryPod("master", "master", "instance-0")}
	// Publication is based on Running+IP; neither Ready nor the listed member's revision gates membership.
	pods[2].Labels[workloadv1alpha1.RevisionLabelKey] = "v1"
	noIP := discoveryPod("no-ip", "worker", "instance-1")
	noIP.Status.PodIP = ""
	pending := discoveryPod("pending", "worker", "instance-1")
	pending.Status.Phase = corev1.PodPending
	other := discoveryPod("other", "worker", "instance-1")
	other.Labels[workloadv1alpha1.GroupNameLabelKey] = "other-group"
	objects := []runtime.Object{noIP, pending, other, discoveryPod("no-role", "", "instance-1"), discoveryPod("no-instance", "worker", "")}
	for _, pod := range pods {
		objects = append(objects, pod)
	}
	p, req, client := discoveryFixture(t, "", objects...)
	if err := p.OnPodRunning(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	cm := readDiscoveryCM(t, client)
	want := `{"group":"group-1","roles":[{"role":"master","roleInstanceList":[{"roleInstanceName":"instance-0","podList":[{"podName":"master","podIP":"10.0.0.1","nodeIP":"192.0.2.1"}]}]},{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1","podList":[{"podName":"a-worker","podIP":"10.0.0.1","nodeIP":"192.0.2.1"},{"podName":"b-worker","podIP":"10.0.0.1","nodeIP":"192.0.2.1"}]},{"roleInstanceName":"instance-2","podList":[{"podName":"z-worker","podIP":"10.0.0.1","nodeIP":"192.0.2.1"}]}]}]}`
	if cm.Data["groupinfo.config"] != want {
		t.Fatalf("unexpected wire format:\n%s", cm.Data["groupinfo.config"])
	}
	if !metav1.IsControlledBy(cm, req.ModelServing) || cm.Labels[workloadv1alpha1.GroupNameLabelKey] != "group-1" {
		t.Fatalf("metadata: %#v", cm.ObjectMeta)
	}
	client.ClearActions()
	if err := p.OnPodRunning(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	assertNoDiscoveryWrites(t, client)
}

func TestPodDiscoveryPluginHookGates(t *testing.T) {
	for _, target := range []string{"", "v1"} {
		p, req, client := discoveryFixture(t, "")
		req.ModelServing.Status.UpdateRevision = target
		if err := p.OnPodRunning(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if len(client.Actions()) != 0 {
			t.Fatal("revision gate accessed API")
		}
	}
	p, req, client := discoveryFixture(t, "")
	for _, hook := range []func(context.Context, *HookRequest) error{p.OnPodReady, p.OnRoleSync, p.OnRoleDelete} {
		if err := hook(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if len(client.Actions()) != 0 {
		t.Fatal("no-op hook accessed API")
	}
	req.KubeClient = nil
	for _, hook := range []func(context.Context, *HookRequest) error{p.OnPodRunning, p.OnPodDelete, p.OnServingGroupDelete} {
		if err := hook(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPodDiscoveryPluginRetry(t *testing.T) {
	for _, hookName := range []string{"running", "delete"} {
		t.Run(hookName, func(t *testing.T) {
			cm := discoveryCM(`{"group":"group-1","roles":[{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1","podList":[{"podName":"pod-1"},{"podName":"keep"}]}]}]}`)
			p, req, client := discoveryFixture(t, "", cm, discoveryPod("pod-1", "worker", "instance-1"))
			updates := 0
			client.PrependReactor("update", "configmaps", func(action coretesting.Action) (bool, runtime.Object, error) {
				updates++
				if updates == 1 {
					return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, cm.Name, fmt.Errorf("conflict"))
				}
				return false, nil, nil
			})
			hook := p.OnPodRunning
			if hookName == "delete" {
				hook = p.OnPodDelete
			}
			if err := hook(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if updates != 2 {
				t.Fatalf("update attempts: %d", updates)
			}
			got := readDiscoveryCM(t, client)
			if got.Data["unrelated"] != "preserved" {
				t.Fatal("lost unrelated data")
			}
			if hookName == "delete" {
				want := `{"group":"group-1","roles":[{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1","podList":[{"podName":"keep"}]}]}]}`
				if got.Data["groupinfo.config"] != want {
					t.Fatalf("delete result: %s", got.Data["groupinfo.config"])
				}
			}
		})
	}
	t.Run("concurrent create", func(t *testing.T) {
		p, req, client := discoveryFixture(t, "", discoveryPod("pod-1", "worker", "instance-1"))
		client.PrependReactor("create", "configmaps", func(action coretesting.Action) (bool, runtime.Object, error) {
			cm := discoveryCM(`{}`)
			if err := client.Tracker().Create(corev1.SchemeGroupVersion.WithResource("configmaps"), cm, "default"); err != nil {
				return true, nil, err
			}
			return true, nil, apierrors.NewAlreadyExists(schema.GroupResource{Resource: "configmaps"}, cm.Name)
		})
		if err := p.OnPodRunning(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		got := readDiscoveryCM(t, client)
		if got.Data["groupinfo.config"] == `{}` || got.Data["unrelated"] != "preserved" {
			t.Fatalf("retry result: %v", got.Data)
		}
	})
}

func TestPodDiscoveryPluginDeleteNoopAndCleanup(t *testing.T) {
	for _, data := range []string{"missing", "", "invalid json", `{"group":"group-1"}`} {
		t.Run(data, func(t *testing.T) {
			var objects []runtime.Object
			if data != "missing" {
				objects = append(objects, discoveryCM(data))
			}
			p, req, client := discoveryFixture(t, "", objects...)
			if err := p.OnPodDelete(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			assertNoDiscoveryWrites(t, client)
		})
	}
	p, req, client := discoveryFixture(t, "", discoveryCM(`{"group":"group-1","roles":[{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1","podList":[{"podName":"pod-1"}]}]}]}`))
	if err := p.OnPodDelete(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	want := `{"group":"group-1","roles":[{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1"}]}]}`
	if got := readDiscoveryCM(t, client).Data["groupinfo.config"]; got != want {
		t.Fatalf("empty instance result: %s", got)
	}
	for i := 0; i < 2; i++ {
		if err := p.OnServingGroupDelete(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.CoreV1().ConfigMaps("default").Get(context.Background(), "group-1-groupinfo", metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("cleanup: %v", err)
	}
}

func TestPodDiscoveryPluginErrorsAndCancellation(t *testing.T) {
	for _, verb := range []string{"list", "get", "create", "update", "delete"} {
		t.Run(verb, func(t *testing.T) {
			var objects []runtime.Object
			if verb == "update" {
				objects = append(objects, discoveryCM(`{}`))
			}
			p, req, client := discoveryFixture(t, "", objects...)
			want := errors.New("API unavailable")
			client.PrependReactor(verb, "*", func(coretesting.Action) (bool, runtime.Object, error) { return true, nil, want })
			hook := p.OnPodRunning
			if verb == "delete" {
				hook = p.OnServingGroupDelete
			}
			if err := hook(context.Background(), req); !errors.Is(err, want) {
				t.Fatalf("error: %v", err)
			}
		})
	}
	for _, deleting := range []bool{false, true} {
		p, req, client := discoveryFixture(t, "", discoveryCM(`{"group":"group-1","roles":[{"role":"worker","roleInstanceList":[{"roleInstanceName":"instance-1","podList":[{"podName":"pod-1"}]}]}]}`))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		client.PrependReactor("update", "configmaps", func(coretesting.Action) (bool, runtime.Object, error) {
			cancel()
			return true, nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, "group-1-groupinfo", fmt.Errorf("conflict"))
		})
		hook := p.OnPodRunning
		if deleting {
			hook = p.OnPodDelete
		}
		start := time.Now()
		if err := hook(ctx, req); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancellation: %v", err)
		}
		if time.Since(start) > time.Second {
			t.Fatal("cancellation waited for poll timeout")
		}
	}
}
