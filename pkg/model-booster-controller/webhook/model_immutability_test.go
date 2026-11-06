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

package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"

	registryv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The ModelServing webhook keeps spec.schedulerName and the role set immutable.
// ModelBooster must reject the same changes up front rather than leave the
// booster controller forever failing to update its ModelServing.
func TestModelBoosterBackendImmutability(t *testing.T) {
	tests := []struct {
		name       string
		operation  admissionv1.Operation
		oldMutate  func(*registryv1alpha1.ModelBooster)
		mutate     func(*registryv1alpha1.ModelBooster)
		withoutOld bool
		wantDenied string
	}{
		{
			name:       "scheduler change is rejected",
			operation:  admissionv1.Update,
			mutate:     func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "volcano" },
			wantDenied: "spec.backend.schedulerName",
		},
		{
			name:       "clearing a non-default scheduler is rejected",
			operation:  admissionv1.Update,
			oldMutate:  func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "volcano" },
			mutate:     func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "" },
			wantDenied: "spec.backend.schedulerName",
		},
		{
			name:      "empty and default-scheduler are the same scheduler",
			operation: admissionv1.Update,
			mutate:    func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "default-scheduler" },
		},
		{
			name:      "backend type change is rejected",
			operation: admissionv1.Update,
			mutate: func(m *registryv1alpha1.ModelBooster) {
				m.Spec.Backend.Type = registryv1alpha1.ModelBackendTypeVLLMDisaggregated
				m.Spec.Backend.Workers = []registryv1alpha1.ModelWorker{
					{Type: registryv1alpha1.ModelWorkerTypePrefill, Pods: 1, Image: "test-image:latest"},
					{Type: registryv1alpha1.ModelWorkerTypeDecode, Pods: 1, Image: "test-image:latest"},
				}
			},
			wantDenied: "spec.backend.type",
		},
		{
			name:      "unrelated spec change is allowed",
			operation: admissionv1.Update,
			mutate:    func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.Replicas = 3 },
		},
		{
			name:       "update without old object is not blocked",
			operation:  admissionv1.Update,
			withoutOld: true,
			mutate:     func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "volcano" },
		},
		{
			name:      "create is unaffected",
			operation: admissionv1.Create,
			mutate:    func(m *registryv1alpha1.ModelBooster) { m.Spec.Backend.SchedulerName = "volcano" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldModel := validModelBoosterForDeprecationTest()
			if tt.oldMutate != nil {
				tt.oldMutate(oldModel)
			}
			newModel := oldModel.DeepCopy()
			tt.mutate(newModel)
			if tt.withoutOld || tt.operation == admissionv1.Create {
				oldModel = nil
			}

			response := handleModelBoosterAdmission(t, tt.operation, "", newModel, oldModel)
			if tt.wantDenied == "" {
				assert.True(t, response.Allowed, "unexpected denial: %v", response.Result)
				return
			}
			require.False(t, response.Allowed)
			require.NotNil(t, response.Result)
			assert.Contains(t, response.Result.Message, tt.wantDenied)
			assert.Contains(t, response.Result.Message, "immutable")
		})
	}
}
