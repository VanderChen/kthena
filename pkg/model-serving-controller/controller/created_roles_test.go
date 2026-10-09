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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func TestCreatedRoleFactsRequireCompleteLayout(t *testing.T) {
	for _, state := range []string{"complete-notready", "missing", "mixed-revision", "mixed-incarnation", "terminating", "pod-patch-failure"} {
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
			if state == "pod-patch-failure" {
				c.kubeClientSet.(*kubefake.Clientset).PrependReactor("patch", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, fmt.Errorf("injected Pod patch failure")
				})
			}
			err := c.markRoleCreated(ctx, ms, "created-0", "prefill", "prefill-0")
			if state == "complete-notready" {
				require.NoError(t, err)
				current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, "true", current.Annotations[roleCreatedAnnotation])
				cms, err := c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Empty(t, cms.Items)
			} else {
				require.Error(t, err)
				current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, entry.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NotEqual(t, "true", current.Annotations[roleCreatedAnnotation])
			}
		})
	}
}
