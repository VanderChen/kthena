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

package utils

import (
	"reflect"
	"testing"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestReplaceEmbeddedPlaceholders(t *testing.T) {
	values := map[string]interface{}{
		"key1": "value1",
		"key2": 123,
		"key3": true,
		"key4": []string{"a", "b"},
	}

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"replace string", "hello ${key1}", "hello value1", false},
		{"replace int", "count ${key2}", "count 123", false},
		{"replace bool", "is_true ${key3}", "is_true true", false},
		{"replace list", "list ${key4}", `list ["a","b"]`, false},
		{"key not found", "missing ${key5}", "", true},
		{"no placeholder", "plain text", "plain text", false},
		{"unclosed placeholder", "hello ${key1", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ReplaceEmbeddedPlaceholders(tt.input, &values)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplaceEmbeddedPlaceholders() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if got != tt.want {
				t.Errorf("ReplaceEmbeddedPlaceholders() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConvertVLLMArgsFromJson(t *testing.T) {
	tests := []struct {
		name    string
		json    string
		want    []string
		wantErr bool
	}{
		{
			name:    "valid config",
			json:    `{"model": "test-model", "gpu_memory_utilization": 0.9, "trust_remote_code": true}`,
			want:    []string{"--gpu-memory-utilization", "0.9", "--model", "test-model", "--trust-remote-code", "true"},
			wantErr: false,
		},
		{
			name:    "empty config",
			json:    `{}`,
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "nil config",
			json:    "",
			want:    []string{},
			wantErr: false,
		},
		{
			name:    "invalid json",
			json:    `{invalid}`,
			want:    nil,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var config *apiextensionsv1.JSON
			if tt.json != "" {
				config = &apiextensionsv1.JSON{Raw: []byte(tt.json)}
			}
			got, err := ConvertVLLMArgsFromJson(config)
			if (err != nil) != tt.wantErr {
				t.Errorf("ConvertVLLMArgsFromJson() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("ConvertVLLMArgsFromJson() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestReplacePlaceholders(t *testing.T) {
	values := map[string]interface{}{
		"key1": "value1",
		"key2": map[string]interface{}{"inner": "val"},
	}

	tests := []struct {
		name    string
		data    interface{}
		want    interface{}
		wantErr bool
	}{
		{
			name: "full string placeholder",
			data: "${key1}",
			want: "value1",
		},
		{
			name: "nested placeholder",
			data: "${key2}",
			want: map[string]interface{}{"inner": "val"},
		},
		{
			name: "embedded placeholder",
			data: "prefix ${key1} suffix",
			want: "prefix value1 suffix",
		},
		{
			name: "map with placeholder",
			data: map[string]interface{}{"a": "${key1}"},
			want: map[string]interface{}{"a": "value1"},
		},
		{
			name: "slice with placeholder",
			data: []interface{}{"${key1}", "literal"},
			want: []interface{}{"value1", "literal"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data := tt.data
			err := ReplacePlaceholders(&data, &values)
			if (err != nil) != tt.wantErr {
				t.Errorf("ReplacePlaceholders() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(data, tt.want) {
				t.Errorf("ReplacePlaceholders() got = %v, want %v", data, tt.want)
			}
		})
	}
}

func TestGetBackendResourceName(t *testing.T) {
	if got := GetBackendResourceName("model", "backend"); got != "model-backend" {
		t.Errorf("GetBackendResourceName() = %v, want model-backend", got)
	}
	if got := GetBackendResourceName("model", ""); got != "model" {
		t.Errorf("GetBackendResourceName() with empty backend = %v, want model", got)
	}
}

func TestGetModelControllerLabels(t *testing.T) {
	model := &workload.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-model",
			UID:  types.UID("test-uid"),
		},
	}

	got := GetModelControllerLabels(model, "test-backend", "v1")

	expected := map[string]string{
		ModelNameLabelKey:   "test-model",
		BackendNameLabelKey: "test-backend",
		ManageBy:            workload.GroupName,
		RevisionLabelKey:    "v1",
		OwnerUIDKey:         "test-uid",
	}

	if !reflect.DeepEqual(got, expected) {
		t.Errorf("GetModelControllerLabels() = %v, want %v", got, expected)
	}
}
