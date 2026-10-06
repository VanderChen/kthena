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
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestRevisionErrorStillScalesDown(t *testing.T) {
	for _, scenario := range []string{"groups", "roles"} {
		t.Run(scenario, func(t *testing.T) {
			ms := createStandardModelServing("shrink", 1, 1)
			ms.UID = "shrink-uid"
			c := newRevisionTestController(t, ms)
			t.Cleanup(c.workqueue.ShutDown)
			ctx := context.Background()
			revision := utils.ModelServingRevision(ms)
			cr, err := utils.CreateControllerRevision(ctx, c.kubeClientSet, ms, revision, ms.Spec.Template.Roles)
			require.NoError(t, err)
			cr.Data = runtime.RawExtension{Raw: []byte(`{"data":"invalid"}`)}
			_, err = c.kubeClientSet.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
			require.NoError(t, err)
			count := 2
			if scenario == "roles" {
				count = 1
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			}
			for i := 0; i < count; i++ {
				addReadyLegacyGroupToController(t, c, ms, ms.Spec.Template.Roles[0], i, "legacy")
			}
			client := c.kubeClientSet.(*kubefake.Clientset)
			client.ClearActions()
			require.Error(t, c.syncModelServing(ctx, namespacedKey(ms.Namespace, ms.Name)))
			key := utils.GetNamespaceName(ms)
			if scenario == "groups" {
				require.Equal(t, datastore.ServingGroupDeleting, c.store.GetServingGroupStatus(key, "shrink-1"))
			} else {
				require.Equal(t, datastore.RoleDeleting, c.store.GetRoleStatus(key, "shrink-0", "prefill", "prefill-0"))
			}
			for _, action := range client.Actions() {
				if action.GetResource().Resource == "pods" {
					require.NotEqual(t, "create", action.GetVerb())
				}
			}
		})
	}
}

func TestUnresolvedHistorySchedulesRetry(t *testing.T) {
	ms := createStandardModelServing("history-retry", 1, 1)
	ms.UID = "retry-uid"
	c := newRevisionTestController(t, ms)
	t.Cleanup(c.workqueue.ShutDown)
	_, err := c.revisionHistory(context.Background(), ms).roles(context.Background(), "missing")
	require.Error(t, err)
	require.Eventually(t, func() bool { return c.workqueue.Len() > 0 }, 7*time.Second, 20*time.Millisecond)
}

func TestRevisionErrorDoesNotHideMutationFailure(t *testing.T) {
	unresolved := &revisionResolutionError{fmt.Errorf("missing history")}
	require.True(t, isRevisionResolutionError(fmt.Errorf("Role template: %w", unresolved)))
	require.False(t, isRevisionResolutionError(errors.Join(unresolved, fmt.Errorf("delete hook failed"))))
}
