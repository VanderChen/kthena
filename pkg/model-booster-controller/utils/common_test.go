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

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestTryGetField(t *testing.T) {
	config := []byte(`{"key1": "value1", "key2": 123, "key3": true}`)

	tests := []struct {
		name    string
		key     string
		want    interface{}
		wantErr bool
	}{
		{"exists string", "key1", "value1", false},
		{"exists number", "key2", float64(123), false},
		{"exists bool", "key3", true, false},
		{"not exists", "key4", nil, false},
		{"invalid json", "key1", nil, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			conf := config
			if tt.name == "invalid json" {
				conf = []byte(`{invalid}`)
			}
			got, err := TryGetField(conf, tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("TryGetField() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TryGetField() got = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetDeviceNum(t *testing.T) {
	worker := &workloadv1alpha1.ModelWorker{
		Resources: corev1.ResourceRequirements{
			Limits: corev1.ResourceList{
				"nvidia.com/gpu":         resource.MustParse("2"),
				"huawei.com/ascend-1980": resource.MustParse("4"),
				"cpu":                    resource.MustParse("1"),
			},
		},
	}

	expected := int64(6)
	if got := GetDeviceNum(worker); got != expected {
		t.Errorf("GetDeviceNum() = %v, want %v", got, expected)
	}

	workerNoLimits := &workloadv1alpha1.ModelWorker{
		Resources: corev1.ResourceRequirements{},
	}
	if got := GetDeviceNum(workerNoLimits); got != 0 {
		t.Errorf("GetDeviceNum() with no limits = %v, want 0", got)
	}
}

func TestNewModelOwnerRef(t *testing.T) {
	model := &workloadv1alpha1.ModelBooster{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-model",
			UID:  types.UID("test-uid"),
		},
	}

	got := NewModelOwnerRef(model)

	if got.Name != model.Name {
		t.Errorf("NewModelOwnerRef() Name = %v, want %v", got.Name, model.Name)
	}
	if got.UID != model.UID {
		t.Errorf("NewModelOwnerRef() UID = %v, want %v", got.UID, model.UID)
	}
	if got.Kind != workloadv1alpha1.ModelKind.Kind {
		t.Errorf("NewModelOwnerRef() Kind = %v, want %v", got.Kind, workloadv1alpha1.ModelKind.Kind)
	}
	if *got.Controller != true {
		t.Errorf("NewModelOwnerRef() Controller = %v, want true", *got.Controller)
	}
}
