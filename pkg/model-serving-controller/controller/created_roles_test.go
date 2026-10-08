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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestCreatedRoleFactsRequireCompleteLayout(t *testing.T) {
	for _, state := range []string{"complete-notready", "missing", "mixed-revision", "mixed-incarnation", "terminating", "write-failure"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("created", 1, 1, 0)
			ms.ResourceVersion = "1"
			ms.Spec.Template.Roles[0].WorkerReplicas = 1
			ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
			c := lifecycleController(t, ms)
			entry := lifecyclePod(t, c, ms, ms, 0, "entry")
			role := ms.Spec.Template.Roles[0]
			worker := utils.GenerateWorkerPod(role, ms, "created-0", "prefill-0", 1, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
			worker.UID = "worker"
			switch state {
			case "mixed-revision":
				worker.Labels[api.RevisionLabelKey] = "another"
			case "mixed-incarnation":
				worker.Annotations = map[string]string{roleInstanceAnnotation: "another"}
			case "terminating":
				now := metav1.Now()
				worker.DeletionTimestamp = &now
			}
			if state != "missing" {
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, worker, metav1.CreateOptions{})
				require.NoError(t, err)
			}
			if state == "write-failure" {
				c.kubeClientSet.(*kubefake.Clientset).PrependReactor("create", "configmaps", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("injected fact write failure")
				})
			}
			err := c.markRoleCreated(ctx, ms, "created-0", "prefill", "prefill-0")
			if state == "complete-notready" {
				require.NoError(t, err)
				c.store = datastore.New()
				require.NoError(t, c.restoreCreatedRoles(ctx, ms))
				roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), "created-0", "prefill")
				require.NoError(t, err)
				require.Len(t, roles, 1)
				require.True(t, roles[0].Initialized)
				require.Equal(t, datastore.RoleCreating, roles[0].Status)
			} else {
				require.Error(t, err)
				cm, err := c.readGroupMembersState(ctx, ms)
				require.NoError(t, err)
				facts, err := readCreatedRoles(cm)
				require.NoError(t, err)
				require.Empty(t, facts)
				current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotEqual(t, "true", current.Annotations[roleCreatedAnnotation])
			}
		})
	}
}

func TestCreatedRoleRestorationUsesCurrentIncarnation(t *testing.T) {
	for _, change := range []string{"lost-role", "new-group", "new-role", "whole-group-gone", "desired-zero"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			ms := lifecycleMS("fence", 1, 1, 0)
			ms.ResourceVersion = "1"
			other := *ms.Spec.Template.Roles[0].DeepCopy()
			other.Name = "decode"
			ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, other)
			c := lifecycleController(t, ms)
			lost := lifecyclePod(t, c, ms, ms, 0, "lost")
			survivor := utils.GenerateEntryPod(other, ms, "fence-0", "decode-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(other))
			survivor.UID = "survivor"
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, survivor, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.markRoleCreated(ctx, ms, "fence-0", "prefill", "prefill-0"))
			require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, lost.Name, metav1.DeleteOptions{}))
			switch change {
			case "new-group":
				survivor.Annotations = map[string]string{groupInstanceAnnotation: "new"}
				_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, survivor, metav1.UpdateOptions{})
				require.NoError(t, err)
			case "new-role":
				replacement := lost.DeepCopy()
				replacement.UID = "new"
				replacement.Annotations = map[string]string{roleInstanceAnnotation: "new"}
				_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, replacement, metav1.CreateOptions{})
				require.NoError(t, err)
			case "whole-group-gone":
				require.NoError(t, c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, survivor.Name, metav1.DeleteOptions{}))
			case "desired-zero":
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](0)
			}
			c.store = datastore.New()
			require.NoError(t, c.restoreCreatedRoles(ctx, ms))
			roles, _ := c.store.GetRoleList(utils.GetNamespaceName(ms), "fence-0", "prefill")
			if change == "lost-role" {
				require.Len(t, roles, 1)
				require.True(t, roles[0].Initialized)
				require.Equal(t, datastore.RoleCreating, roles[0].Status)
			} else {
				require.Empty(t, roles, "old facts must not initialize a different or intentionally retiring instance")
			}
			if change == "desired-zero" {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
				c.store = datastore.New()
				require.NoError(t, c.restoreCreatedRoles(ctx, ms))
				roles, _ = c.store.GetRoleList(utils.GetNamespaceName(ms), "fence-0", "prefill")
				require.Empty(t, roles, "a later 0-to-1 scale-up cannot inherit completion from the removed member")
			}
		})
	}
}
