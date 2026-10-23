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
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubetesting "k8s.io/client-go/testing"
)

func TestStatusLoggingOnlyAfterCommittedChange(t *testing.T) {
	for _, failure := range []string{"none", "conflict", "forbidden"} {
		t.Run(failure, func(t *testing.T) {
			logs := captureModelServingLogs(t, 2)
			ms := lifecycleMS("status-logs", 1, 1, 0)
			c := lifecycleController(t, ms)
			pod := lifecyclePod(t, c, ms, ms, 0, "stable-pod")
			client := c.modelServingClient.(*kthenafake.Clientset)
			attempts := 0
			client.PrependReactor("update", "modelservings", func(action kubetesting.Action) (bool, runtime.Object, error) {
				require.Equal(t, "status", action.GetSubresource())
				attempts++
				resource := schema.GroupResource{Group: "workload.serving.volcano.sh", Resource: "modelservings"}
				if failure == "forbidden" {
					return true, nil, apierrors.NewForbidden(resource, ms.Name, fmt.Errorf("denied"))
				}
				if failure == "conflict" && attempts == 1 {
					return true, nil, apierrors.NewConflict(resource, ms.Name, fmt.Errorf("retry"))
				}
				return false, nil, nil
			})
			err := c.updateModelServingStatus(context.Background(), ms, utils.ModelServingRevision(ms), nil)
			count := func() int {
				count := 0
				for _, entry := range logs.entries() {
					if entry.Message == "ModelServing status changed" {
						count++
						require.Equal(t, 2, entry.Level)
						require.Equal(t, ms.UID, entry.Fields["uid"])
						require.EqualValues(t, 1, entry.Fields["available"])
					}
					require.NotEqual(t, "ServingGroup became available", entry.Message, "already Running is not a transition")
				}
				return count
			}
			if failure == "forbidden" {
				require.Error(t, err)
				require.Equal(t, 1, attempts)
				require.Zero(t, count())
				for _, entry := range logs.entries() {
					require.NotEqual(t, "ModelServing condition changed", entry.Message)
				}
				return
			}
			require.NoError(t, err)
			if failure == "conflict" {
				require.Equal(t, 2, attempts)
			} else {
				require.Equal(t, 1, attempts)
			}
			require.Equal(t, 1, count(), "failed conflicts must not log successful status writes")
			latest, err := client.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
			beforeAttempts := attempts
			for range 3 {
				require.NoError(t, c.updateModelServingStatus(context.Background(), latest, utils.ModelServingRevision(ms), nil))
			}
			require.Equal(t, beforeAttempts, attempts, "stable status must retain its no-write behavior")
			require.Equal(t, 1, count())
			current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, pod.UID, current.UID)
		})
	}
}
