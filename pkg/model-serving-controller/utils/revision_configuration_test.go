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
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func configurationFixture() *api.ModelServing {
	return &api.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "config", Namespace: "default", UID: "owner"}, Spec: api.ModelServingSpec{
		SchedulerName: "volcano", Plugins: []api.PluginSpec{{Name: "demo-pod-tweaks", Config: &extensions.JSON{Raw: []byte(`{"annotations":{"version":"old"}}`)}}},
		Template: api.ServingGroup{Roles: []api.Role{{Name: "p", Replicas: ptr.To[int32](3), EntryTemplate: api.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "server", Image: "old"}}}}}}},
	}}
}

func TestRevisionConfigurationIdentityAndRecordedCounts(t *testing.T) {
	ctx := context.Background()
	ms := configurationFixture()
	client := fake.NewSimpleClientset()
	revision := ModelServingRevision(ms)
	cr, err := CreateControllerRevision(ctx, client, ms, revision, ms.Spec.Template.Roles)
	require.NoError(t, err)
	for _, change := range []func(*api.ModelServing){
		func(m *api.ModelServing) { m.Spec.SchedulerName = "custom" },
		func(m *api.ModelServing) { m.Spec.Plugins = nil },
	} {
		changed := ms.DeepCopy()
		change(changed)
		require.NotEqual(t, revision, ModelServingRevision(changed))
		_, err = CreateControllerRevision(ctx, client, changed, revision, changed.Spec.Template.Roles)
		require.Error(t, err, "same Roles cannot hide changed globals under the old identity")
		restored, err := ModelServingForControllerRevision(changed, cr)
		require.NoError(t, err)
		equal, err := EqualRevisionConfiguration(ms, restored)
		require.NoError(t, err)
		require.True(t, equal)
	}
	scaled := ms.DeepCopy()
	scaled.Spec.Template.Roles[0].Replicas = ptr.To[int32](7)
	require.Equal(t, revision, ModelServingRevision(scaled))
	same, err := CreateControllerRevision(ctx, client, scaled, revision, scaled.Spec.Template.Roles)
	require.NoError(t, err)
	require.Equal(t, cr.Data.Raw, same.Data.Raw)
	roles, err := GetRolesFromControllerRevision(same)
	require.NoError(t, err)
	require.EqualValues(t, 3, *roles[0].Replicas)
	equivalent := ms.DeepCopy()
	equivalent.Spec.Plugins[0].Config.Raw = []byte(`{ "annotations": { "version": "old" } }`)
	require.Equal(t, revision, ModelServingRevision(equivalent))
}

func TestLegacyConfigurationBaselineIsPinnedAndFailClosed(t *testing.T) {
	ctx := context.Background()
	ms := configurationFixture()
	client := fake.NewSimpleClientset()
	source, err := CreateControllerRevision(ctx, client, ms, "legacy", ms.Spec.Template.Roles)
	require.NoError(t, err)
	source.Annotations = nil
	source.UID = "source-uid"
	source.Data.Raw, err = json.Marshal(map[string]any{"data": ms.Spec.Template.Roles})
	require.NoError(t, err)
	source, err = client.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, source, metav1.UpdateOptions{})
	require.NoError(t, err)
	original := append([]byte(nil), source.Data.Raw...)
	baseline, err := EnsureRevisionBaseline(ctx, client, ms, source)
	require.NoError(t, err)
	changed := ms.DeepCopy()
	changed.Spec.Plugins = nil
	changed.Spec.SchedulerName = "custom"
	pinned, err := EnsureRevisionBaseline(ctx, client, changed, source)
	require.NoError(t, err)
	require.Equal(t, baseline.Data.Raw, pinned.Data.Raw)
	historical, err := ModelServingForControllerRevision(changed, pinned)
	require.NoError(t, err)
	equal, err := EqualRevisionConfiguration(ms, historical)
	require.NoError(t, err)
	require.True(t, equal)
	require.EqualValues(t, 3, *historical.Spec.Template.Roles[0].Replicas)
	live, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, source.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, original, live.Data.Raw)
	require.NoError(t, client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, baseline.Name, metav1.DeleteOptions{}))
	_, err = EnsureRevisionBaseline(ctx, client, changed, source)
	require.ErrorContains(t, err, "previously established")
}
