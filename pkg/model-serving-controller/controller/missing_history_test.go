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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMissingHistoryAllowsOnlyProvenLaterChanges(t *testing.T) {
	old := createStandardModelServing("missing", 3, 1)
	old.UID = "missing-uid"
	old.Generation = 1
	decode := *old.Spec.Template.Roles[0].DeepCopy()
	decode.Name = "decode"
	old.Spec.Template.Roles = append(old.Spec.Template.Roles, decode)
	old.Status.UpdateRevision = "opaque-legacy"
	c := newRevisionTestController(t, old)
	t.Cleanup(c.workqueue.ShutDown)
	for _, role := range old.Spec.Template.Roles {
		addReadyLegacyGroupToController(t, c, old, role, 0, "opaque-legacy")
	}
	ctx := c.withRevisionHistory(context.Background(), old)
	checkpoint, err := c.revisionHistory(ctx, old).desiredRevision(ctx)
	require.NoError(t, err)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(old))
	require.NoError(t, err)
	require.Equal(t, templateUnknown, c.compareServingGroupTemplate(ctx, old, groups[0], checkpoint), "upgrade alone cannot authorize deletion")
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, old, "opaque-legacy")
	require.NoError(t, err)
	require.Nil(t, cr, "must not invent old history")
	ms := old.DeepCopy()
	ms.Status.UpdateRevision = checkpoint
	ms.Generation++
	ms.Spec.Template.Roles[1].EntryTemplate.Spec.Containers[0].Image = "decode:v2"
	ctx = c.withRevisionHistory(context.Background(), ms)
	target, err := c.revisionHistory(ctx, ms).desiredRevision(ctx)
	require.NoError(t, err)
	require.Equal(t, templateDifferent, c.compareServingGroupTemplate(ctx, ms, groups[0], target))
	// Fresh history context and a status already pointing at the target model a restart.
	ms.Status.UpdateRevision = target
	ctx = c.withRevisionHistory(context.Background(), ms)
	require.Equal(t, templateDifferent, c.compareServingGroupTemplate(ctx, ms, groups[0], target))
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	selected, _, err := c.rolesToDeleteForRoleRollingUpdate(ctx, ms, groups[0], nil)
	require.NoError(t, err)
	require.Equal(t, []roleToDelete{{roleName: "decode", roleID: "decode-0"}}, selected, "unmodified prefill must not be replaced")
	require.Equal(t, datastore.RoleRunning, c.store.GetRoleStatus(utils.GetNamespaceName(ms), groups[0].Name, "prefill", "prefill-0"))
}
func TestStatusRejectsNewGenerationAfterOldReconcile(t *testing.T) {
	ms := createStandardModelServing("generation", 0, 1)
	ms.UID = "generation-uid"
	ms.Generation = 1
	c := newRevisionTestController(t, ms)
	t.Cleanup(c.workqueue.ShutDown)
	latest := ms.DeepCopy()
	latest.Generation = 2
	latest.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "v2"
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
	require.Error(t, c.updateModelServingStatus(context.Background(), ms, utils.ModelServingRevision(ms), nil))
	stored, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Zero(t, stored.Status.ObservedGeneration)
}
