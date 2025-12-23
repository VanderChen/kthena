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
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestParsePodRanktable(t *testing.T) {
	tests := []struct {
		name           string
		parserTemplate string
		annotation     string
		want           *PodRanktableData
		wantErr        bool
	}{
		{
			name: "parse standard annotation format",
			parserTemplate: `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}
serverId: {{ $data.server_id | quote }}
devices:
{{- range $data.devices }}
  - deviceId: {{ .device_id | quote }}
    deviceIp: {{ .device_ip | quote }}
{{- end }}`,
			annotation: `{"pod_name":"test-pod","server_id":"server-1","devices":[{"device_id":"0","device_ip":"192.168.1.1"},{"device_id":"1","device_ip":"192.168.1.2"}]}`,
			want: &PodRanktableData{
				PodName:  "test-pod",
				ServerId: "server-1",
				Devices: []DeviceInfo{
					{DeviceId: "0", DeviceIp: "192.168.1.1"},
					{DeviceId: "1", DeviceIp: "192.168.1.2"},
				},
			},
			wantErr: false,
		},
		{
			name: "parse CCE annotation format",
			parserTemplate: `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}
serverId: {{ $data.server_id | quote }}
devices:
{{- range $data.devices }}
  - deviceId: {{ .device_id | quote }}
    deviceIp: {{ .device_ip | quote }}
{{- end }}`,
			annotation: `{"pod_name":"cce-pod","server_id":"cce-server-1","devices":[{"device_id":"0","device_ip":"10.0.0.1"}]}`,
			want: &PodRanktableData{
				PodName:  "cce-pod",
				ServerId: "cce-server-1",
				Devices: []DeviceInfo{
					{DeviceId: "0", DeviceIp: "10.0.0.1"},
				},
			},
			wantErr: false,
		},
		{
			name:           "invalid JSON annotation",
			parserTemplate: `{{- $data := . | fromJson -}}`,
			annotation:     `invalid json`,
			want:           nil,
			wantErr:        true,
		},
		{
			name:           "empty annotation",
			parserTemplate: `{{- $data := . | fromJson -}}`,
			annotation:     "",
			want:           nil,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTemplateManager()
			got, err := tm.ParsePodRanktable(tt.parserTemplate, tt.annotation)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestBuildRanktableTemplateData(t *testing.T) {
	tests := []struct {
		name          string
		podRanktables []PodRanktableData
		want          *RanktableTemplateData
	}{
		{
			name: "single server with multiple devices",
			podRanktables: []PodRanktableData{
				{
					PodName:  "pod-1",
					ServerId: "server-1",
					Devices: []DeviceInfo{
						{DeviceId: "0", DeviceIp: "192.168.1.1"},
						{DeviceId: "1", DeviceIp: "192.168.1.2"},
					},
				},
			},
			want: &RanktableTemplateData{
				ServerCount: 1,
				Servers: []ServerInfo{
					{
						ServerId: "server-1",
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
						},
					},
				},
			},
		},
		{
			name: "multiple servers with devices",
			podRanktables: []PodRanktableData{
				{
					PodName:  "pod-1",
					ServerId: "server-1",
					Devices: []DeviceInfo{
						{DeviceId: "0", DeviceIp: "192.168.1.1"},
						{DeviceId: "1", DeviceIp: "192.168.1.2"},
					},
				},
				{
					PodName:  "pod-2",
					ServerId: "server-2",
					Devices: []DeviceInfo{
						{DeviceId: "0", DeviceIp: "192.168.2.1"},
						{DeviceId: "1", DeviceIp: "192.168.2.2"},
					},
				},
			},
			want: &RanktableTemplateData{
				ServerCount: 2,
				Servers: []ServerInfo{
					{
						ServerId: "server-1",
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
						},
					},
					{
						ServerId: "server-2",
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.2.1", RankId: "2"},
							{DeviceId: "1", DeviceIp: "192.168.2.2", RankId: "3"},
						},
					},
				},
			},
		},
		{
			name: "devices sorted by device_id",
			podRanktables: []PodRanktableData{
				{
					PodName:  "pod-1",
					ServerId: "server-1",
					Devices: []DeviceInfo{
						{DeviceId: "3", DeviceIp: "192.168.1.4"},
						{DeviceId: "1", DeviceIp: "192.168.1.2"},
						{DeviceId: "2", DeviceIp: "192.168.1.3"},
						{DeviceId: "0", DeviceIp: "192.168.1.1"},
					},
				},
			},
			want: &RanktableTemplateData{
				ServerCount: 1,
				Servers: []ServerInfo{
					{
						ServerId: "server-1",
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
							{DeviceId: "2", DeviceIp: "192.168.1.3", RankId: "2"},
							{DeviceId: "3", DeviceIp: "192.168.1.4", RankId: "3"},
						},
					},
				},
			},
		},
		{
			name:          "empty pod ranktables",
			podRanktables: []PodRanktableData{},
			want: &RanktableTemplateData{
				ServerCount: 0,
				Servers:     []ServerInfo{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTemplateManager()
			got := tm.BuildRanktableTemplateData("completed", tt.podRanktables)
			// Ignore Timestamp field in comparison as it's dynamic
			assert.Equal(t, tt.want.ServerCount, got.ServerCount)
			assert.Equal(t, tt.want.Servers, got.Servers)
			// TotalDevices is calculated, so verify it matches expected
			expectedTotalDevices := 0
			for _, server := range tt.want.Servers {
				expectedTotalDevices += len(server.Devices)
			}
			assert.Equal(t, expectedTotalDevices, got.TotalDevices)
		})
	}
}

func TestRenderRanktable(t *testing.T) {
	tests := []struct {
		name              string
		ranktableTemplate string
		templateData      *RanktableTemplateData
		wantJSON          map[string]interface{}
		wantErr           bool
	}{
		{
			name: "render mindie format ranktable",
			ranktableTemplate: `{
  "version": "1.0",
  "server_count": "{{ .ServerCount }}",
  "server_list": [
    {{- range $serverIdx, $server := .Servers }}
    {{- if $serverIdx }},{{ end }}
    {
      "server_id": {{ $server.ServerId | quote }},
      "device": [
        {{- range $devIdx, $device := $server.Devices }}
        {{- if $devIdx }},{{ end }}
        {
          "device_id": {{ $device.DeviceId | quote }},
          "device_ip": {{ $device.DeviceIp | quote }},
          "rank_id": {{ $device.RankId | quote }}
        }
        {{- end }}
      ]
    }
    {{- end }}
  ],
  "status": "{{ .Status }}"
}`,
			templateData: &RanktableTemplateData{
				ServerCount: 1,
				Servers: []ServerInfo{
					{
						ServerId: "server-1",
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
						},
					},
				},
				Status: "completed",
			},
			wantJSON: map[string]interface{}{
				"version":      "1.0",
				"server_count": "1",
				"status":       "completed",
				"server_list": []interface{}{
					map[string]interface{}{
						"server_id": "server-1",
						"device": []interface{}{
							map[string]interface{}{
								"device_id": "0",
								"device_ip": "192.168.1.1",
								"rank_id":   "0",
							},
							map[string]interface{}{
								"device_id": "1",
								"device_ip": "192.168.1.2",
								"rank_id":   "1",
							},
						},
					},
				},
			},
			wantErr: false,
		},
		{
			name:              "invalid template syntax",
			ranktableTemplate: `{{ .InvalidField `,
			templateData:      &RanktableTemplateData{},
			wantJSON:          nil,
			wantErr:           true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTemplateManager()
			got, err := tm.RenderRanktable(tt.ranktableTemplate, *tt.templateData)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				// Parse both JSON strings and compare
				var gotMap, wantMap map[string]interface{}
				err = json.Unmarshal([]byte(got), &gotMap)
				assert.NoError(t, err)
				wantBytes, _ := json.Marshal(tt.wantJSON)
				err = json.Unmarshal(wantBytes, &wantMap)
				assert.NoError(t, err)
				assert.Equal(t, wantMap, gotMap)
			}
		})
	}
}

func TestGetRanktableTemplate(t *testing.T) {
	tests := []struct {
		name            string
		setupConfigMaps func() listerv1.ConfigMapLister
		templateName    string
		want            *RanktableTemplate
		wantErr         bool
	}{
		{
			name: "get valid ranktable template with parser",
			setupConfigMaps: func() listerv1.ConfigMapLister {
				// Create parser ConfigMap
				parserCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-parser",
						Namespace: "kthena-system",
					},
					Data: map[string]string{
						"annotation-name": "test.io/annotation",
						"parser-template": `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}`,
					},
				}

				// Create ranktable template ConfigMap
				templateCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-template",
						Namespace: "kthena-system",
					},
					Data: map[string]string{
						"inference-engine":    "mindie",
						"ranktable-level":     "role",
						"pod-parser-template": "test-parser",
						"ranktable-template":  `{"version": "1.0"}`,
						"mount-path":          "/etc/ranktable",
						"filename":            "ranktable.json",
					},
				}
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				indexer.Add(parserCM)
				indexer.Add(templateCM)
				return listerv1.NewConfigMapLister(indexer)
			},
			templateName: "test-template",
			want: &RanktableTemplate{
				InferenceEngine: "mindie",
				Level:           RoleLevelRanktable,
				PodParserTemplate: `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}`,
				PodAnnotationName: "test.io/annotation",
				RanktableTemplate: `{"version": "1.0"}`,
				MountPath:         "/etc/ranktable",
				Filename:          "ranktable.json",
			},
			wantErr: false,
		},
		{
			name: "template not found",
			setupConfigMaps: func() listerv1.ConfigMapLister {
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				return listerv1.NewConfigMapLister(indexer)
			},
			templateName: "non-existent",
			want:         nil,
			wantErr:      true,
		},
		{
			name: "parser template not found",
			setupConfigMaps: func() listerv1.ConfigMapLister {
				templateCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-template",
						Namespace: "kthena-system",
					},
					Data: map[string]string{
						"pod-parser-template": "non-existent-parser",
						"ranktable-template":  `{"version": "1.0"}`,
					},
				}
				indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				indexer.Add(templateCM)
				return listerv1.NewConfigMapLister(indexer)
			},
			templateName: "test-template",
			want:         nil,
			wantErr:      true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.Setenv("POD_NAMESPACE", "kthena-system")
			defer os.Unsetenv("POD_NAMESPACE")

			lister := tt.setupConfigMaps()
			tm := NewTemplateManager()
			got, err := tm.GetRanktableTemplate(lister, tt.templateName)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Nil(t, got)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestEnsureRanktableConfigMap(t *testing.T) {
	tests := []struct {
		name      string
		namespace string
		cmName    string
		filename  string
		content   string
		wantErr   bool
	}{
		{
			name:      "create new ConfigMap",
			namespace: "default",
			cmName:    "test-ranktable",
			filename:  "ranktable.json",
			content:   `{"version": "1.0"}`,
			wantErr:   false,
		},
		{
			name:      "update existing ConfigMap",
			namespace: "default",
			cmName:    "test-ranktable",
			filename:  "ranktable.json",
			content:   `{"version": "2.0"}`,
			wantErr:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := fake.NewSimpleClientset()
			tm := NewTemplateManager()

			err := tm.EnsureRanktableConfigMap(
				context.TODO(),
				client,
				tt.namespace,
				tt.cmName,
				nil,
				map[string]string{"test": "label"},
				tt.filename,
				tt.content,
			)

			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				// Verify ConfigMap was created/updated
				cm, err := client.CoreV1().ConfigMaps(tt.namespace).Get(context.TODO(), tt.cmName, metav1.GetOptions{})
				assert.NoError(t, err)
				assert.Equal(t, tt.content, cm.Data[tt.filename])
			}
		})
	}
}
