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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestPreserveChildPluginConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*workload.ModelServing)
		allowed bool
	}{
		{"unchanged", func(*workload.ModelServing) {}, true},
		{"metadata", func(ms *workload.ModelServing) { ms.Annotations = map[string]string{"note": "new"} }, true},
		{"SG shrink", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) }, true},
		{"SG zero", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](0) }, true},
		{"Role shrink", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1) }, true},
		{"equivalent defaults", func(ms *workload.ModelServing) { normalizeChildDefaults(ms) }, true},
		{"SG expansion", func(ms *workload.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) }, false},
		{"Role expansion", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3) }, false},
		{"worker shrink is a template change", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].WorkerReplicas = 0 }, false},
		{"image change with SG shrink", func(ms *workload.ModelServing) {
			ms.Spec.Replicas = ptr.To[int32](1)
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "new"
		}, false},
		{"Pod annotation", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles[0].EntryTemplate.Metadata = &workload.Metadata{Annotations: map[string]string{"user": "new"}}
		}, false},
		{"Role rename", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Name = "new" }, false},
		{"explicit plugin scope", func(ms *workload.ModelServing) {
			ms.Spec.Plugins[0].Scope = &workload.PluginScope{Roles: []string{"serve"}}
		}, false},
		{"additional plugin", func(ms *workload.ModelServing) {
			ms.Spec.Plugins = append(ms.Spec.Plugins, workload.PluginSpec{Name: "custom", Type: workload.PluginTypeBuiltIn})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			old := &workload.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "old", Namespace: "default", UID: "old-uid"}, Spec: workload.ModelServingSpec{
				Replicas: ptr.To[int32](2), Template: workload.ServingGroup{Roles: []workload.Role{{Name: "serve", Replicas: ptr.To[int32](2), WorkerReplicas: 1,
					EntryTemplate:  workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "old"}}}},
					WorkerTemplate: &workload.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "engine", Image: "old"}}}},
				}}},
			}}
			original := old.DeepCopy()
			desired := old.DeepCopy()
			desired.Spec.Plugins = []workload.PluginSpec{{Name: HeadlessServicePluginName, Type: workload.PluginTypeBuiltIn}}
			tc.change(desired)
			err := PreserveChildPluginConfiguration(old, desired)
			if tc.allowed {
				require.NoError(t, err)
				assert.Equal(t, old.Spec.Plugins, desired.Spec.Plugins)
			} else {
				require.ErrorContains(t, err, "MigrationRequired")
			}
			assert.Equal(t, original, old, "lister object must remain unchanged")
		})
	}
}

func TestPreserveChildWithHeadlessPluginThroughWorkerTransition(t *testing.T) {
	old := &workload.ModelServing{Spec: workload.ModelServingSpec{Plugins: []workload.PluginSpec{{Name: HeadlessServicePluginName}}, Template: workload.ServingGroup{Roles: []workload.Role{{Name: "serve"}}}}}
	desired := old.DeepCopy()
	desired.Spec.Plugins[0].Type = workload.PluginTypeBuiltIn
	desired.Spec.Template.Roles[0].WorkerReplicas = 2
	require.NoError(t, PreserveChildPluginConfiguration(old, desired))
	assert.Equal(t, old.Spec.Plugins, desired.Spec.Plugins)
	assert.Equal(t, int32(2), desired.Spec.Template.Roles[0].WorkerReplicas)
	desired.Spec.Plugins = nil
	require.ErrorContains(t, PreserveChildPluginConfiguration(old, desired), "immutable plugin")
}
