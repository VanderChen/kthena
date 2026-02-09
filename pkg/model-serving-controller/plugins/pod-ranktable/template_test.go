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

package podranktable

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParsePodRanktable(t *testing.T) {
	tests := []struct {
		name           string
		parserTemplate string
		annotation     string
		podIP          string
		podName        string
		want           *PodRanktableData
		wantErr        bool
	}{
		{
			name: "parse with pod IP as server ID",
			parserTemplate: `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}
serverId: {{ $data.server_id | quote }}
devices:
{{- range $data.devices }}
  - deviceId: {{ .device_id | quote }}
    deviceIp: {{ .device_ip | quote }}
{{- end }}`,
			annotation: `{"pod_name":"test-pod","server_id":"ignored-server-id","devices":[{"device_id":"0","device_ip":"192.168.1.1"},{"device_id":"1","device_ip":"192.168.1.2"}]}`,
			podIP:      "10.244.1.5",
			podName:    "test-pod",
			want: &PodRanktableData{
				PodName:  "test-pod",
				ServerId: "10.244.1.5", // Pod IP is used, not "ignored-server-id"
				Devices: []DeviceInfo{
					{DeviceId: "0", DeviceIp: "192.168.1.1"},
					{DeviceId: "1", DeviceIp: "192.168.1.2"},
				},
			},
			wantErr: false,
		},
		{
			name: "parse CCE annotation format with pod IP",
			parserTemplate: `{{- $data := . | fromJson -}}
podName: {{ $data.pod_name | quote }}
serverId: {{ $data.server_id | quote }}
devices:
{{- range $data.devices }}
  - deviceId: {{ .device_id | quote }}
    deviceIp: {{ .device_ip | quote }}
{{- end }}`,
			annotation: `{"pod_name":"cce-pod","server_id":"cce-server-1","devices":[{"device_id":"0","device_ip":"10.0.0.1"}]}`,
			podIP:      "10.244.2.10",
			podName:    "cce-pod",
			want: &PodRanktableData{
				PodName:  "cce-pod",
				ServerId: "10.244.2.10", // Pod IP is used, not "cce-server-1"
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
			podIP:          "10.244.1.5",
			podName:        "test-pod",
			want:           nil,
			wantErr:        true,
		},
		{
			name:           "empty annotation",
			parserTemplate: `{{- $data := . | fromJson -}}`,
			annotation:     "",
			podIP:          "10.244.1.5",
			podName:        "test-pod",
			want:           nil,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tm := NewTemplateManager()
			got, err := tm.ParsePodRanktable(tt.parserTemplate, tt.annotation, tt.podIP, tt.podName)
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
			name: "single pod with pod IP as server ID",
			podRanktables: []PodRanktableData{
				{
					PodName:  "pod-1",
					ServerId: "10.244.1.5", // Pod IP
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
						ServerId: "10.244.1.5", // Pod IP
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
						},
					},
				},
			},
		},
		{
			name: "multiple pods with different pod IPs",
			podRanktables: []PodRanktableData{
				{
					PodName:  "pod-1",
					ServerId: "10.244.1.5", // Pod IP
					Devices: []DeviceInfo{
						{DeviceId: "0", DeviceIp: "192.168.1.1"},
						{DeviceId: "1", DeviceIp: "192.168.1.2"},
					},
				},
				{
					PodName:  "pod-2",
					ServerId: "10.244.1.6", // Pod IP
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
						ServerId: "10.244.1.5", // Pod IP sorted lexicographically
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.1.1", RankId: "0"},
							{DeviceId: "1", DeviceIp: "192.168.1.2", RankId: "1"},
						},
					},
					{
						ServerId: "10.244.1.6", // Pod IP sorted lexicographically
						Devices: []DeviceWithRank{
							{DeviceId: "0", DeviceIp: "192.168.2.1", RankId: "2"},
							{DeviceId: "1", DeviceIp: "192.168.2.2", RankId: "3"},
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
			name: "render ranktable with pod IPs",
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
						ServerId: "10.244.1.5", // Pod IP
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
						"server_id": "10.244.1.5", // Pod IP
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
