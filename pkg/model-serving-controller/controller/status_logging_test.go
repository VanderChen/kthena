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
	for _, verbosity := range []int{2, 4} {
		for _, failure := range []string{"none", "conflict", "forbidden"} {
			t.Run(fmt.Sprintf("v%d/%s", verbosity, failure), func(t *testing.T) {
				ms := lifecycleMS("status-logs", 1, 1, 0)
				c := lifecycleController(t, ms)
				pod := lifecyclePod(t, c, ms, ms, 0, "stable-pod")
				logs := captureModelServingLogs(t, verbosity)
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
						if entry.Message == "ModelServing status updated" {
							count++
							require.Equal(t, 4, entry.Level)
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
				expectedLogs := 0
				if verbosity == 4 {
					expectedLogs = 1
				}
				require.Equal(t, expectedLogs, count(), "status diagnostics require V4 and a successful API write")
				if verbosity == 2 {
					require.Empty(t, logs.entries(), "status persistence does not emit normal-level business logs")
				}
				latest, err := client.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
				beforeAttempts := attempts
				for range 3 {
					require.NoError(t, c.updateModelServingStatus(context.Background(), latest, utils.ModelServingRevision(ms), nil))
				}
				require.Equal(t, beforeAttempts, attempts, "stable status must retain its no-write behavior")
				require.Equal(t, expectedLogs, count())
				current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), pod.Name, metav1.GetOptions{})
				require.NoError(t, err)
				require.Equal(t, pod.UID, current.UID)
			})
		}
	}
}

func TestCoordinationLoggingUsesExistingChangeBranch(t *testing.T) {
	ms := lifecycleMS("coordination-logs", 1, 1, 0)
	c := lifecycleController(t, ms)
	lifecyclePod(t, c, ms, ms, 0, "stable-pod")
	logs := captureModelServingLogs(t, 2)
	client := c.modelServingClient.(*kthenafake.Clientset)
	deny := true
	client.PrependReactor("update", "modelservings", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if deny {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "modelservings"}, ms.Name, fmt.Errorf("denied"))
		}
		return false, nil, nil
	})
	policy := &roleRolloutPolicy{enabled: true, blocker: &coordinatedRoleBlocker{
		servingGroupName: "coordination-logs-0", reason: coordinatedRoleDependencyNotReady, message: "dependency is not ready",
	}}
	require.Error(t, c.updateModelServingStatus(context.Background(), ms, utils.ModelServingRevision(ms), policy))
	require.Empty(t, logs.entries(), "failed writes cannot claim a coordination change")
	deny = false
	require.NoError(t, c.updateModelServingStatus(context.Background(), ms, utils.ModelServingRevision(ms), policy))
	entries := logs.entries()
	require.Len(t, entries, 1)
	require.Equal(t, "Coordinated role rollout state changed", entries[0].Message)
	require.Equal(t, true, entries[0].Fields["blocked"])
	require.Equal(t, coordinatedRoleDependencyNotReady, entries[0].Fields["reason"])
	require.Equal(t, ms.UID, entries[0].Fields["uid"])

	latest, err := client.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(context.Background(), ms.Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(latest))
	client.ClearActions()
	for range 3 {
		require.NoError(t, c.updateModelServingStatus(context.Background(), latest, utils.ModelServingRevision(ms), policy))
	}
	require.Empty(t, client.Actions(), "unchanged coordination retains its no-write behavior")
	require.Len(t, logs.entries(), 1, "unchanged coordination does not add logs")

	policy.blocker = nil
	require.NoError(t, c.updateModelServingStatus(context.Background(), latest, utils.ModelServingRevision(ms), policy))
	entries = logs.entries()
	require.Len(t, entries, 2)
	require.Equal(t, false, entries[1].Fields["blocked"])
	require.Equal(t, "RolloutComplete", entries[1].Fields["reason"])
}
