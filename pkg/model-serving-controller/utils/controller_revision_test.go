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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	kubefake "k8s.io/client-go/kubernetes/fake"

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
			Template: workloadv1alpha1.ServingGroup{
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
	assert.Equal(t, int64(1), cr.Revision)

	next, err := CreateControllerRevision(ctx, client, ms, "revision-v2", templateData)
	assert.NoError(t, err)
	assert.Equal(t, int64(2), next.Revision)
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
			Template: workloadv1alpha1.ServingGroup{
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
}

// TestCleanupOldControllerRevisionsRetainsHistoryWithinLimit verifies that
// convergence does not immediately remove traceable history.
func TestCleanupOldControllerRevisionsRetainsHistoryWithinLimit(t *testing.T) {
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
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill",
					},
				},
			},
		},
		Status: workloadv1alpha1.ModelServingStatus{
			// Set CurrentRevision and UpdateRevision to older revisions that would normally be deleted
			CurrentRevision: "revision-v1",
			UpdateRevision:  "revision-v5",
		},
	}

	templateData := ms.Spec.Template.Roles

	// All five revisions fit within the ten-entry non-live history limit.
	revisions := []string{"revision-v1", "revision-v2", "revision-v3", "revision-v4", "revision-v5"}
	for _, rev := range revisions {
		_, err := CreateControllerRevision(ctx, client, ms, rev, templateData)
		assert.NoError(t, err)
	}

	// Manually run cleanup
	err := CleanupOldControllerRevisions(ctx, client, ms)
	assert.NoError(t, err)

	// List all remaining ControllerRevisions
	selector := labels.SelectorFromSet(map[string]string{
		ControllerRevisionLabelKey: ms.Name,
	})
	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	assert.NoError(t, err)

	// Verify CurrentRevision and UpdateRevision are preserved
	currentRevisionName := GenerateControllerRevisionName(ms.GetName(), ms.Status.CurrentRevision)
	updateRevisionName := GenerateControllerRevisionName(ms.GetName(), ms.Status.UpdateRevision)

	remainingRevisionNames := make(map[string]bool)
	for _, cr := range list.Items {
		remainingRevisionNames[cr.Name] = true
	}

	// CurrentRevision should be preserved even though it's old
	currentCR, err := GetControllerRevision(ctx, client, ms, ms.Status.CurrentRevision)
	assert.NoError(t, err, "CurrentRevision should be preserved")
	assert.NotNil(t, currentCR, "CurrentRevision ControllerRevision should exist")
	assert.True(t, remainingRevisionNames[currentRevisionName],
		"CurrentRevision %s should be in remaining revisions", currentRevisionName)

	// UpdateRevision should be preserved even though it's old
	updateCR, err := GetControllerRevision(ctx, client, ms, ms.Status.UpdateRevision)
	assert.NoError(t, err, "UpdateRevision should be preserved")
	assert.NotNil(t, updateCR, "UpdateRevision ControllerRevision should exist")
	assert.True(t, remainingRevisionNames[updateRevisionName],
		"UpdateRevision %s should be in remaining revisions", updateRevisionName)

	assert.True(t, remainingRevisionNames["test-ms-revision-v2"])
	assert.True(t, remainingRevisionNames["test-ms-revision-v3"])
	assert.True(t, remainingRevisionNames["test-ms-revision-v4"])
	assert.Equal(t, 5, len(list.Items))
}

func TestCleanupOldControllerRevisionsTruncatesOldestNonLiveHistory(t *testing.T) {
	ctx := context.Background()
	client := kubefake.NewSimpleClientset()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		Status: workloadv1alpha1.ModelServingStatus{
			CurrentRevision: "revision-v12",
			UpdateRevision:  "revision-v13",
		},
	}

	for i := 1; i <= 13; i++ {
		created, err := CreateControllerRevision(ctx, client, ms, fmt.Sprintf("revision-v%d", i), []string{fmt.Sprintf("v%d", i)})
		require.NoError(t, err)
		assert.Equal(t, int64(i), created.Revision)
	}

	require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
	oldest, err := GetControllerRevision(ctx, client, ms, "revision-v1")
	require.NoError(t, err)
	assert.Nil(t, oldest)

	list, err := client.AppsV1().ControllerRevisions(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{ControllerRevisionLabelKey: ms.Name}).String(),
	})
	require.NoError(t, err)
	assert.Len(t, list.Items, defaultControllerRevisionHistoryLimit+2)
}

func TestCleanupOldControllerRevisionsPreservesTerminatingPodRevision(t *testing.T) {
	ctx := context.Background()
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
			UID:       "test-uid",
		},
		Status: workloadv1alpha1.ModelServingStatus{
			CurrentRevision: "revision-v13",
			UpdateRevision:  "revision-v13",
		},
	}
	deletionTimestamp := metav1.Now()
	client := kubefake.NewSimpleClientset(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      "terminating-old-revision",
		Namespace: ms.Namespace,
		Labels: map[string]string{
			ControllerRevisionLabelKey:         ms.Name,
			ControllerRevisionRevisionLabelKey: "revision-v1",
		},
		OwnerReferences:   []metav1.OwnerReference{newModelServingOwnerRef(ms)},
		DeletionTimestamp: &deletionTimestamp,
		Finalizers:        []string{"test.finalizer"},
	}})

	for i := 1; i <= 13; i++ {
		_, err := CreateControllerRevision(ctx, client, ms, fmt.Sprintf("revision-v%d", i), []string{fmt.Sprintf("v%d", i)})
		require.NoError(t, err)
	}

	require.NoError(t, CleanupOldControllerRevisions(ctx, client, ms))
	protected, err := GetControllerRevision(ctx, client, ms, "revision-v1")
	require.NoError(t, err)
	assert.NotNil(t, protected)
	deleted, err := GetControllerRevision(ctx, client, ms, "revision-v2")
	require.NoError(t, err)
	assert.Nil(t, deleted)
}
