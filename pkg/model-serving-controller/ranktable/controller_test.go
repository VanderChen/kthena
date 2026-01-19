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

package ranktable

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestNewrRanktableController(t *testing.T) {
	client := fake.NewSimpleClientset()
	controller := NewrRanktableController(client)

	assert.NotNil(t, controller)
	assert.Equal(t, client, controller.kubeClient)
	assert.NotNil(t, controller.templateManager)
}

func TestNeedsRanktable(t *testing.T) {
	tests := []struct {
		name string
		ms   *workloadv1alpha1.ModelServing
		want bool
	}{
		{
			name: "ModelServing with ranktable annotation",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						RanktableTemplateAnnotation: "test-template",
					},
				},
			},
			want: true,
		},
		{
			name: "ModelServing without ranktable annotation",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"some-other-annotation": "value",
					},
				},
			},
			want: false,
		},
		{
			name: "ModelServing with nil annotations",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &RanktableController{}
			assert.Equal(t, tt.want, controller.NeedsRanktable(tt.ms))
		})
	}
}

func TestControllerGetRanktableTemplate(t *testing.T) {
	tests := []struct {
		name            string
		ms              *workloadv1alpha1.ModelServing
		setupConfigMaps func() *fake.Clientset
		want            *RanktableTemplate
		wantErr         bool
	}{
		{
			name: "valid template with role level override",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						RanktableTemplateAnnotation: "test-template",
						RanktableLevelAnnotation:    string(RoleLevelRanktable),
					},
				},
			},
			setupConfigMaps: func() *fake.Clientset {
				client := fake.NewSimpleClientset()
				parserCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-parser",
						Namespace: RanktableTemplateNamespace,
					},
					Data: map[string]string{
						"annotation-name": "test.io/annotation",
						"parser-template": "podName: {{ .PodName }}",
					},
				}
				client.CoreV1().ConfigMaps(RanktableTemplateNamespace).Create(context.TODO(), parserCM, metav1.CreateOptions{})

				templateCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-template",
						Namespace: RanktableTemplateNamespace,
					},
					Data: map[string]string{
						"inference-engine":    "engine",
						"ranktable-level":     string(GroupLevelRanktable),
						"pod-parser-template": "test-parser",
						"ranktable-template":  `{"version": "1.0"}`,
						"mount-path":          "/mnt",
						"filename":            "file.json",
					},
				}
				client.CoreV1().ConfigMaps(RanktableTemplateNamespace).Create(context.TODO(), templateCM, metav1.CreateOptions{})
				return client
			},
			want: &RanktableTemplate{
				InferenceEngine:    "engine",
				Level:              RoleLevelRanktable,
				PodParserTemplate:  "podName: {{ .PodName }}",
				PodAnnotationName:  "test.io/annotation",
				RanktableTemplate:  `{"version": "1.0"}`,
				MountPath:          "/mnt",
				Filename:           "file.json",
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := tt.setupConfigMaps()
			controller := NewrRanktableController(client)
			got, err := controller.GetRanktableTemplate(context.TODO(), tt.ms)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestEnsureRanktableConfigMaps(t *testing.T) {
	tests := []struct {
		name          string
		ms            *workloadv1alpha1.ModelServing
		template      *RanktableTemplate
		expectCMCount int
	}{
		{
			name: "ensure config maps for role level",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Template: workloadv1alpha1.ServingGroup{
						Roles: []workloadv1alpha1.Role{
							{Name: "role1"},
							{Name: "role2"},
						},
					},
				},
			},
			template: &RanktableTemplate{
				Level:    RoleLevelRanktable,
				Filename: "rt.json",
			},
			expectCMCount: 2,
		},
		{
			name: "ensure config maps for group level",
			ms: &workloadv1alpha1.ModelServing{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-ms",
					Namespace: "default",
				},
				Spec: workloadv1alpha1.ModelServingSpec{
					Replicas: func() *int32 { i := int32(3); return &i }(),
				},
			},
			template: &RanktableTemplate{
				Level:    GroupLevelRanktable,
				Filename: "rt.json",
			},
			expectCMCount: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			controller := NewrRanktableController(client)
			err := controller.EnsureRanktableConfigMaps(context.TODO(), tt.ms, tt.template)

			assert.NoError(t, err)
			cms, _ := client.CoreV1().ConfigMaps(tt.ms.Namespace).List(context.TODO(), metav1.ListOptions{})
			assert.Len(t, cms.Items, tt.expectCMCount)
		})
	}
}

func TestInjectRanktableMount(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers:     []corev1.Container{{Name: "main"}},
			InitContainers: []corev1.Container{{Name: "init"}},
		},
	}
	template := &RanktableTemplate{MountPath: "/etc/rt"}
	cmName := "test-cm"

	controller := &RanktableController{}
	controller.InjectRanktableMount(pod, template, cmName)

	assert.Len(t, pod.Spec.Volumes, 1)
	assert.Equal(t, "ranktable", pod.Spec.Volumes[0].Name)
	assert.Equal(t, cmName, pod.Spec.Volumes[0].ConfigMap.Name)
	assert.Len(t, pod.Spec.Containers[0].VolumeMounts, 1)
	assert.Equal(t, "/etc/rt", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
	assert.Len(t, pod.Spec.InitContainers[0].VolumeMounts, 1)
	assert.Equal(t, "/etc/rt", pod.Spec.InitContainers[0].VolumeMounts[0].MountPath)
}

func TestCheckPodsRanktableReady(t *testing.T) {
	tests := []struct {
		name string
		pods []*corev1.Pod
		want bool
	}{
		{
			name: "all pods ready",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"rt": "data"}}},
				{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"rt": "data"}}},
			},
			want: true,
		},
		{
			name: "one pod missing annotation",
			pods: []*corev1.Pod{
				{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{"rt": "data"}}},
				{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{}}},
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			controller := &RanktableController{}
			assert.Equal(t, tt.want, controller.CheckPodsRanktableReady(tt.pods, "rt"))
		})
	}
}

func TestGenerateAndUpdateRanktables(t *testing.T) {
	client := fake.NewSimpleClientset()
	controller := NewrRanktableController(client)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "ms1", Namespace: "default", UID: "ms1-uid"},
		TypeMeta:   metav1.TypeMeta{APIVersion: "workload.volcano.sh/v1alpha1", Kind: "ModelServing"},
	}
	template := &RanktableTemplate{
		Level:             RoleLevelRanktable,
		Filename:          "rt.json",
		PodAnnotationName: "rt-anno",
		PodParserTemplate: `{{- $data := . | fromJson -}}
podName: {{ index $data "pod_name" }}
serverId: {{ index $data "server_id" }}
devices:
{{- range (index $data "devices") }}
  - deviceId: {{ .device_id }}
    deviceIp: {{ .device_ip }}
{{- end }}`,
		RanktableTemplate: `{"status": "{{ .Status }}", "server_count": {{ .ServerCount }}}`,
	}
	pods := []*corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:        "pod1",
				Namespace:   "default",
				Labels:      map[string]string{workloadv1alpha1.RoleLabelKey: "worker"},
				Annotations: map[string]string{"rt-anno": `{"pod_name":"pod1","server_id":"s1","devices":[{"device_id":"0","device_ip":"10.0.0.1"}]}`},
			},
		},
	}

	// Test Case 1: Ready
	err := controller.GenerateAndUpdateRanktables(context.TODO(), ms, template, pods)
	assert.NoError(t, err)

	cm, err := client.CoreV1().ConfigMaps("default").Get(context.TODO(), "ms1-worker-ranktable", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Contains(t, cm.Data["rt.json"], `"status": "completed"`)
	assert.Contains(t, cm.Data["rt.json"], `"server_count": 1`)

	// Test Case 2: Not Ready (e.g. restart)
	// Clear annotation
	pods[0].Annotations = nil
	err = controller.GenerateAndUpdateRanktables(context.TODO(), ms, template, pods)
	assert.NoError(t, err)

	cm, err = client.CoreV1().ConfigMaps("default").Get(context.TODO(), "ms1-worker-ranktable", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, "", cm.Data["rt.json"])
}
