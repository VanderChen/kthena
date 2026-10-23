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
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func rolloutActionLogs(capture *logCapture, action string) []capturedLog {
	var result []capturedLog
	for _, entry := range capture.entries() {
		if entry.Level == 0 && entry.Fields["action"] == action && entry.Fields["outcome"] == "requestAccepted" {
			result = append(result, entry)
		}
	}
	return result
}

func TestRolloutLoggingCreatesReportOnlyAcceptedPods(t *testing.T) {
	for _, failWorker := range []bool{false, true} {
		t.Run(fmt.Sprintf("workerFailure=%t", failWorker), func(t *testing.T) {
			capture := captureModelServingLogs(t, 0)
			ctx := withModelServingAction(context.Background(), "create", "CapacityBelowDesired")
			ms := lifecycleMS("logging-create", 1, 1, 0)
			role := &ms.Spec.Template.Roles[0]
			role.WorkerReplicas = 1
			role.WorkerTemplate = role.EntryTemplate.DeepCopy()
			c := lifecycleController(t, ms)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			if failWorker {
				kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					pod := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
					if pod.Name == utils.GeneratePodName(ms.Name+"-0", "prefill-0", 1) {
						return true, nil, fmt.Errorf("worker admission failed")
					}
					return false, nil, nil
				})
			}
			create := func() error {
				return c.CreatePodsByRole(ctx, *role.DeepCopy(), ms, 0, 0, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(*role), "")
			}
			err := create()
			expected := int32(2)
			if failWorker {
				require.ErrorContains(t, err, "worker admission failed")
				expected = 1
			} else {
				require.NoError(t, err)
			}
			entries := rolloutActionLogs(capture, "create")
			require.Len(t, entries, 1)
			require.Equal(t, expected, entries[0].Fields["acceptedPods"])
			require.Equal(t, ms.UID, entries[0].Fields["uid"])
			pods, err := kube.CoreV1().Pods(ms.Namespace).List(context.Background(), metav1.ListOptions{})
			require.NoError(t, err)
			require.Len(t, pods.Items, int(expected), "log count must equal actual accepted creates")
			for i := range pods.Items {
				require.NoError(t, c.podsInformer.GetIndexer().Add(&pods.Items[i]))
			}
			// AlreadyExists (and a repeated rejected worker) must not be reported
			// as another accepted creation batch.
			err = create()
			if failWorker {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			require.Len(t, rolloutActionLogs(capture, "create"), 1)
		})
	}
}

func TestRolloutLoggingExplicitShrinkCoversEarlyReturn(t *testing.T) {
	for _, roleMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("role=%t", roleMode), func(t *testing.T) {
			capture := captureModelServingLogs(t, 0)
			ctx := context.Background()
			ms := lifecycleMS("logging-shrink", 1, 1, 0)
			if roleMode {
				ms.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
			}
			c := lifecycleController(t, ms)
			first := lifecyclePod(t, c, ms, ms, 0, "keep")
			var selected *corev1.Pod
			if roleMode {
				selected = first.DeepCopy()
				selected.Name = utils.GeneratePodName(ms.Name+"-0", "prefill-1", 0)
				selected.UID = "remove"
				selected.Labels[api.RoleIDKey] = "prefill-1"
				_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(ctx, selected, metav1.CreateOptions{})
				require.NoError(t, err)
				require.NoError(t, c.podsInformer.GetIndexer().Add(selected))
				c.store.AddRole(utils.GetNamespaceName(ms), ms.Name+"-0", "prefill", "prefill-1", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(ms.Spec.Template.Roles[0]))
			} else {
				selected = lifecyclePod(t, c, ms, ms, 1, "remove")
			}
			holdPlannedPodDeletion(t, c, selected.Name)
			sync := func() error {
				if roleMode {
					return c.manageRoleReplicasPerGroup(ctx, ms, ms.Name+"-0", ms.Spec.Template.Roles[0], 0, utils.ModelServingRevision(ms), nil, true)
				}
				return c.syncServingGroupReplicas(ctx, ms, utils.ModelServingRevision(ms))
			}
			require.NoError(t, sync())
			require.True(t, lifecycleDeletionMatches(c, selected))
			require.False(t, lifecycleDeletionMatches(c, first))
			entries := rolloutActionLogs(capture, "scaleDown")
			require.Len(t, entries, 1)
			require.Equal(t, "DesiredReplicasReduced", entries[0].Fields["reason"])
			require.Equal(t, 1, entries[0].Fields["acceptedPods"])
			require.NoError(t, sync())
			require.Len(t, rolloutActionLogs(capture, "scaleDown"), 1, "Terminating wait must not claim a new deletion")
		})
	}
}

func TestRolloutLoggingSurgeAdoptionPreservesIdentity(t *testing.T) {
	for _, scope := range []string{surgeServingGroup, surgeRole} {
		t.Run(scope, func(t *testing.T) {
			capture := captureModelServingLogs(t, 0)
			c, ms, _ := newSurgeCompletionController(t, scope)
			pods, err := c.surgePods(ms, scope, "", "")
			require.NoError(t, err)
			require.Len(t, pods, 1)
			original := pods[0]
			require.NoError(t, c.adoptSurgeReplicas(context.Background(), ms, scope, "", "", 4))
			current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(context.Background(), original.Name, metav1.GetOptions{})
			require.NoError(t, err)
			require.Equal(t, original.UID, current.UID)
			require.Empty(t, current.Annotations[surgeAnnotation])
			entries := rolloutActionLogs(capture, "surgeAdoption")
			require.Len(t, entries, 1)
			require.Equal(t, 1, entries[0].Fields["acceptedPods"])
			require.NoError(t, c.podsInformer.GetIndexer().Update(current))
			require.NoError(t, c.adoptSurgeReplicas(context.Background(), ms, scope, "", "", 4))
			require.Len(t, rolloutActionLogs(capture, "surgeAdoption"), 1)
		})
	}
}

func TestRolloutLoggingPartitionOnlyChangeReleasesOutdatedInstance(t *testing.T) {
	for _, roleMode := range []bool{false, true} {
		t.Run(fmt.Sprintf("role=%t", roleMode), func(t *testing.T) {
			capture := captureModelServingLogs(t, 0)
			ctx := context.Background()
			old := lifecycleMS("logging-partition", 1, 1, 0)
			if roleMode {
				old.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
			}
			protected := old.DeepCopy()
			protected.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
			if roleMode {
				protected.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(1))
			} else {
				protected.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
			}
			c := lifecycleController(t, protected, old)
			original := lifecyclePod(t, c, protected, old, 0, "protected-original")
			targetRevision := utils.ModelServingRevision(protected)
			require.NoError(t, c.manageRollingUpdate(ctx, protected, targetRevision, nil))
			require.False(t, lifecycleDeletionMatches(c, original))
			require.Empty(t, rolloutActionLogs(capture, "rollout"), "protected instance must not emit a deletion action")

			released := protected.DeepCopy()
			released.Generation++
			if roleMode {
				released.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(0))
			} else {
				released.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(0))
			}
			require.Equal(t, targetRevision, utils.ModelServingRevision(released), "partition-only changes keep target template revision")
			holdPlannedPodDeletion(t, c, original.Name)
			require.NoError(t, c.manageRollingUpdate(ctx, released, targetRevision, nil))
			require.True(t, lifecycleDeletionMatches(c, original))
			entries := rolloutActionLogs(capture, "rollout")
			require.Len(t, entries, 1)
			entry := entries[0]
			require.Equal(t, "OutdatedTemplate", entry.Fields["reason"], "do not claim this request changed the template")
			require.Equal(t, released.UID, entry.Fields["uid"])
			require.Equal(t, released.Generation, entry.Fields["generation"])
			require.Equal(t, released.Name+"-0", entry.Fields["servingGroup"])
			require.Equal(t, utils.ModelServingRevision(old), entry.Fields["currentRevision"])
			require.Equal(t, targetRevision, entry.Fields["targetRevision"])
			require.Equal(t, "0", entry.Fields["configuredPartition"])
			require.Equal(t, 0, entry.Fields["resolvedPartition"])
			require.Equal(t, 0, entry.Fields["ordinal"])
			if roleMode {
				require.Equal(t, "prefill", entry.Fields["role"])
				require.Equal(t, "prefill-0", entry.Fields["roleID"])
			}
			require.NoError(t, c.manageRollingUpdate(ctx, released, targetRevision, nil))
			require.Len(t, rolloutActionLogs(capture, "rollout"), 1, "accepted deletion in flight is not another action")
		})
	}
}
