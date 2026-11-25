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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestCreateControllerRevision(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroupTemplate{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
	}

	templateData := ms.Spec.Template.Roles

	// Test creating a ControllerRevision
	cr, err := CreateControllerRevision(ctx, client, ms, "revision-v1", templateData)
	assert.NoError(t, err)
	assert.NotNil(t, cr)
	assert.Equal(t, "test-ms-revision-v1", cr.Name)
	assert.Equal(t, "default", cr.Namespace)
	assert.Equal(t, "test-ms", cr.Labels[ControllerRevisionLabelKey])
	assert.Equal(t, "revision-v1", cr.Labels[ControllerRevisionRevisionLabelKey])

	// A hash collision or corrupted historical snapshot must never be repaired
	// by overwriting data referenced by live stable or surge resources.
	_, err = CreateControllerRevision(ctx, client, ms, "revision-v1", []workloadv1alpha1.Role{{Name: "decode"}})
	assert.ErrorContains(t, err, "already exists with different template data")
	persisted, err := GetControllerRevision(ctx, client, ms, "revision-v1")
	assert.NoError(t, err)
	roles, err := GetRolesFromControllerRevision(persisted)
	assert.NoError(t, err)
	assert.Equal(t, "prefill", roles[0].Name)
}

func TestGetControllerRevision(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		TypeMeta: metav1.TypeMeta{
			APIVersion: "workload.kthena.io/v1alpha1",
			Kind:       "ModelServing",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroupTemplate{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
	}

	templateData := ms.Spec.Template.Roles

	// Create multiple ControllerRevisions
	revisions := []string{"revision-v1", "revision-v2", "revision-v3"}
	for _, rev := range revisions {
		_, err := CreateControllerRevision(ctx, client, ms, rev, templateData)
		assert.NoError(t, err)
	}

	// GetControllerRevision should return the ControllerRevision
	cr, err := GetControllerRevision(ctx, client, ms, "revision-v2")
	assert.NoError(t, err)
	assert.NotNil(t, cr)
	assert.Equal(t, "test-ms-revision-v2", cr.Name)
	assert.Equal(t, "revision-v2", cr.Labels[ControllerRevisionRevisionLabelKey])

	// A complete, owned suffix object is not a substitute for a missing source.
	alias := cr.DeepCopy()
	alias.Name += "-baseline"
	alias.Annotations = map[string]string{
		ControllerRevisionDataVersionAnnotation:          ControllerRevisionDataVersionV1,
		"modelserving.volcano.sh/legacy-revision-source": cr.Name,
	}
	data, err := BuildRevisionData(ms)
	require.NoError(t, err)
	alias.Data.Raw = data
	_, err = client.AppsV1().ControllerRevisions(ms.Namespace).Create(ctx, alias, metav1.CreateOptions{})
	require.NoError(t, err)
	require.NoError(t, client.AppsV1().ControllerRevisions(ms.Namespace).Delete(ctx, cr.Name, metav1.DeleteOptions{}))
	client.ClearActions()
	missing, err := GetControllerRevision(ctx, client, ms, "revision-v2")
	require.NoError(t, err)
	require.Nil(t, missing)
	require.Len(t, client.Actions(), 1, "missing history must not probe a suffix alias")
	require.Equal(t, cr.Name, client.Actions()[0].(clienttesting.GetAction).GetName())

	wantErr := apierrors.NewServiceUnavailable("injected primary read failure")
	client.PrependReactor("get", "controllerrevisions", func(action clienttesting.Action) (bool, runtime.Object, error) {
		if action.(clienttesting.GetAction).GetName() == cr.Name {
			return true, nil, wantErr
		}
		return false, nil, nil
	})
	missing, err = GetControllerRevision(ctx, client, ms, "revision-v2")
	require.ErrorIs(t, err, wantErr)
	require.Nil(t, missing)
}

func TestModelServingAtRevision(t *testing.T) {
	ctx := context.Background()
	newMS := func(schedulerName, image string) *workloadv1alpha1.ModelServing {
		return &workloadv1alpha1.ModelServing{
			ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: "test-uid"},
			Spec: workloadv1alpha1.ModelServingSpec{
				SchedulerName: schedulerName,
				Template: workloadv1alpha1.ServingGroupTemplate{Roles: []workloadv1alpha1.Role{{
					Name: "decode",
					EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Name: "engine", Image: image}},
					}},
				}}},
			},
		}
	}
	current := newMS("current-scheduler", "engine:v2")
	source := newMS("source-scheduler", "engine:v1")

	t.Run("legacy revision restores the Roles only", func(t *testing.T) {
		client := kubefake.NewSimpleClientset()
		cr, err := CreateControllerRevision(ctx, client, current, "legacy", source.Spec.Template.Roles)
		require.NoError(t, err)

		got, err := ModelServingAtRevision(current, cr)
		require.NoError(t, err)
		assert.Equal(t, "engine:v1", got.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
		assert.Equal(t, "current-scheduler", got.Spec.SchedulerName)
		assert.Equal(t, "engine:v2", current.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image, "the input is not mutated")
	})

	t.Run("v1 revision restores every revisioned field", func(t *testing.T) {
		client := kubefake.NewSimpleClientset()
		data, err := BuildRevisionData(source)
		require.NoError(t, err)
		cr, _, err := RecordModelServingRevision(ctx, client, current, data)
		require.NoError(t, err)

		got, err := ModelServingAtRevision(current, cr)
		require.NoError(t, err)
		assert.Equal(t, "engine:v1", got.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image)
		assert.Equal(t, "source-scheduler", got.Spec.SchedulerName)
	})

	t.Run("unreadable revision", func(t *testing.T) {
		_, err := ModelServingAtRevision(current, &appsv1.ControllerRevision{})
		assert.Error(t, err)
	})
}
