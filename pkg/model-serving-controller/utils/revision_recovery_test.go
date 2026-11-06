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

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestRebuildModelServingRevision(t *testing.T) {
	ctx := context.Background()
	ms := revisionTestModelServing(revisionTestRole("decode", "image:v1"))
	ms.Name = "test-ms"
	ms.Namespace = "default"
	ms.UID = "test-uid"
	ms.Generation = 7
	data, err := BuildRevisionData(ms)
	require.NoError(t, err)
	client := kubefake.NewSimpleClientset()

	// The canonical identity was already persisted for the same data; the
	// rebuilt running identity must outrank it so the desired revision settles
	// on what the Pods carry.
	canonical, _, err := RecordModelServingRevision(ctx, client, ms, data)
	require.NoError(t, err)

	rebuilt, err := RebuildModelServingRevision(ctx, client, ms, "legacy")
	require.NoError(t, err)
	require.Equal(t, GenerateControllerRevisionName(ms.Name, "legacy"), rebuilt.Name)
	require.Equal(t, "legacy", rebuilt.Labels[ControllerRevisionRevisionLabelKey])
	require.Equal(t, ControllerRevisionDataVersionV1, rebuilt.Annotations[ControllerRevisionDataVersionAnnotation])
	require.Equal(t, "7", rebuilt.Annotations[RevisionRebuiltFromGenerationAnnotation])
	require.Greater(t, rebuilt.Revision, canonical.Revision)
	require.True(t, revisionDataEqual(rebuilt.Data.Raw, data))

	preferred, _, err := RecordModelServingRevision(ctx, client, ms, data)
	require.NoError(t, err)
	require.Equal(t, rebuilt.Name, preferred.Name, "equal data must resolve to the rebuilt running identity")

	// Idempotent for an equal existing object, refused for a conflicting one.
	again, err := RebuildModelServingRevision(ctx, client, ms, "legacy")
	require.NoError(t, err)
	require.Equal(t, rebuilt.UID, again.UID)
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "image:v2"
	_, err = RebuildModelServingRevision(ctx, client, ms, "legacy")
	require.ErrorContains(t, err, "different identity or template")

	legacy, err := CreateControllerRevision(ctx, client, ms, "roles-only", ms.Spec.Template.Roles)
	require.NoError(t, err)
	_, err = RebuildModelServingRevision(ctx, client, ms, "roles-only")
	require.ErrorContains(t, err, "different identity or template", "a legacy Roles-only object must never be rewritten")
	unchanged, err := client.AppsV1().ControllerRevisions(ms.Namespace).Get(ctx, legacy.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, legacy.Data.Raw, unchanged.Data.Raw)
}
