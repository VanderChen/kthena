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
	"testing"

	"github.com/stretchr/testify/require"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"k8s.io/apimachinery/pkg/util/intstr"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/utils/ptr"
)

func TestConfigurationLoggingPartitionUpdateEntrance(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		scope                    string
		before, after            intstr.IntOrString
		oldReplicas, newReplicas int32
		oldBoundary, newBoundary int
	}{
		{"sg-lower", "ServingGroup", intstr.FromInt(3), intstr.FromInt(1), 4, 4, 3, 1},
		{"sg-raise", "ServingGroup", intstr.FromInt(1), intstr.FromInt(3), 4, 4, 1, 3},
		{"role-lower", "Role", intstr.FromInt(2), intstr.FromInt(0), 4, 4, 2, 0},
		{"role-raise", "Role", intstr.FromInt(0), intstr.FromInt(2), 4, 4, 0, 2},
		{"sg-percentage-replicas", "ServingGroup", intstr.FromString("50%"), intstr.FromString("50%"), 3, 5, 2, 3},
		{"role-percentage-replicas", "Role", intstr.FromString("50%"), intstr.FromString("50%"), 3, 5, 2, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := lifecycleMS("partition-logging", tc.oldReplicas, 1, 0)
			before.Generation = 7
			before.Status.CurrentRevision = "completed-revision"
			before.Status.UpdateRevision = "observed-target-revision"
			if tc.scope == "Role" {
				before.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
				before.Spec.Template.Roles[0].Partition = ptr.To(tc.before)
				before.Spec.Template.Roles[0].Replicas = ptr.To(tc.oldReplicas)
			} else {
				before.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(tc.before)
			}
			after := before.DeepCopy()
			after.Generation++
			if tc.scope == "Role" {
				after.Spec.Template.Roles[0].Partition = ptr.To(tc.after)
				after.Spec.Template.Roles[0].Replicas = ptr.To(tc.newReplicas)
			} else {
				after.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(tc.after)
				after.Spec.Replicas = ptr.To(tc.newReplicas)
			}
			originalBefore, originalAfter := before.DeepCopy(), after.DeepCopy()
			c := lifecycleController(t, before)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			client := c.modelServingClient.(*kthenafake.Clientset)
			kube.ClearActions()
			client.ClearActions()
			require.Zero(t, c.workqueue.Len())
			capture := captureModelServingLogs(t, 0)

			c.updateModelServing(before, after)

			require.Equal(t, originalBefore, before, "logging must not mutate the previous observation")
			require.Equal(t, originalAfter, after, "logging must not default or rewrite the current spec/status")
			require.Empty(t, kube.Actions(), "configuration logging must not issue Kubernetes API requests")
			require.Empty(t, client.Actions(), "configuration logging must not fetch or write ModelServing")
			require.Equal(t, 1, c.workqueue.Len())
			key, quit := c.workqueue.Get()
			require.False(t, quit)
			require.Equal(t, utils.GetNamespaceName(after).String(), key)
			c.workqueue.Done(key)
			c.workqueue.Forget(key)
			require.Zero(t, c.workqueue.NumRequeues(key))
			entries := recoveryLogsWith(capture, "reason", "PartitionChanged")
			require.Len(t, entries, 1)
			entry := entries[0]
			require.Zero(t, entry.Level)
			require.Nil(t, entry.Err)
			require.Equal(t, "modelServingUpdate", entry.Fields["entrypoint"])
			require.Equal(t, after.UID, entry.Fields["uid"])
			require.Equal(t, int64(7), entry.Fields["previousGeneration"])
			require.Equal(t, int64(8), entry.Fields["generation"])
			require.Equal(t, tc.scope, entry.Fields["scope"])
			require.Equal(t, true, entry.Fields["active"])
			require.Equal(t, tc.before.String(), entry.Fields["previousPartition"])
			require.Equal(t, tc.after.String(), entry.Fields["partition"])
			require.Equal(t, tc.oldBoundary, entry.Fields["previousResolvedPartition"])
			require.Equal(t, tc.newBoundary, entry.Fields["resolvedPartition"])
			require.Equal(t, "completed-revision", entry.Fields["currentRevision"])
			require.Equal(t, "observed-target-revision", entry.Fields["observedTargetRevision"])
			if tc.scope == "Role" {
				require.Equal(t, after.Spec.Template.Roles[0].Name, entry.Fields["role"])
				require.Equal(t, int(tc.oldReplicas), entry.Fields["previousDesiredReplicas"])
				require.Equal(t, int(tc.newReplicas), entry.Fields["desiredReplicas"])
			}
		})
	}
}

func TestConfigurationLoggingStatusOnlyUpdateDoesNotEnqueue(t *testing.T) {
	before := lifecycleMS("status-only-logging", 3, 1, 0)
	before.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromString("50%"))
	after := before.DeepCopy()
	after.ResourceVersion = "new-status-version"
	after.Status.AvailableReplicas = 2
	after.Status.UpdateRevision = "new-observed-status-revision"
	originalBefore, originalAfter := before.DeepCopy(), after.DeepCopy()
	c := lifecycleController(t, before)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	client := c.modelServingClient.(*kthenafake.Clientset)
	kube.ClearActions()
	client.ClearActions()
	c.workqueue.Add("unrelated/object")
	capture := captureModelServingLogs(t, 0)

	c.updateModelServing(before, after)

	require.Empty(t, capture.entries(), "status-only updates must produce no default configuration logs")
	require.Equal(t, originalBefore, before)
	require.Equal(t, originalAfter, after)
	require.Empty(t, kube.Actions())
	require.Empty(t, client.Actions())
	require.Equal(t, 1, c.workqueue.Len(), "preserve the existing queue without adding the ModelServing")
	key, quit := c.workqueue.Get()
	require.False(t, quit)
	require.Equal(t, "unrelated/object", key)
	c.workqueue.Done(key)
	c.workqueue.Forget(key)
	require.Zero(t, c.workqueue.Len())
}

func TestConfigurationLoggingInactiveServingGroupPartitionChange(t *testing.T) {
	before := lifecycleMS("inactive-sg-partition", 4, 1, 0)
	before.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
	before.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(3))
	after := before.DeepCopy()
	after.Generation++
	after.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(1))
	originalBefore, originalAfter := before.DeepCopy(), after.DeepCopy()
	c := lifecycleController(t, before)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	client := c.modelServingClient.(*kthenafake.Clientset)
	kube.ClearActions()
	client.ClearActions()
	capture := captureModelServingLogs(t, 0)

	c.updateModelServing(before, after)

	entries := recoveryLogsWith(capture, "reason", "PartitionChanged")
	require.Len(t, entries, 1, "inactive raw configuration changes remain observable")
	entry := entries[0]
	require.Equal(t, "ServingGroup", entry.Fields["scope"])
	require.Equal(t, false, entry.Fields["active"])
	require.Equal(t, "3", entry.Fields["previousPartition"])
	require.Equal(t, "1", entry.Fields["partition"])
	require.Equal(t, 3, entry.Fields["previousResolvedPartition"])
	require.Equal(t, 1, entry.Fields["resolvedPartition"])
	require.Equal(t, after.UID, entry.Fields["uid"])
	require.Equal(t, before.Generation, entry.Fields["previousGeneration"])
	require.Equal(t, after.Generation, entry.Fields["generation"])
	require.Equal(t, originalBefore, before)
	require.Equal(t, originalAfter, after)
	require.Empty(t, kube.Actions())
	require.Empty(t, client.Actions())
	require.Equal(t, 1, c.workqueue.Len())
	key, quit := c.workqueue.Get()
	require.False(t, quit)
	require.Equal(t, utils.GetNamespaceName(after).String(), key)
	c.workqueue.Done(key)
	c.workqueue.Forget(key)
}

func TestConfigurationLoggingStrategySwitchDoesNotInventPartitionChange(t *testing.T) {
	for _, mode := range []api.RolloutStrategyType{api.ServingGroupRollingUpdate, api.RoleRollingUpdate} {
		t.Run(string(mode), func(t *testing.T) {
			before := lifecycleMS("switch-with-partition", 4, 1, 0)
			before.Spec.RolloutStrategy.Type = mode
			before.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition = ptr.To(intstr.FromInt(2))
			before.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt(1))
			after := before.DeepCopy()
			after.Generation++
			if mode == api.ServingGroupRollingUpdate {
				after.Spec.RolloutStrategy.Type = api.RoleRollingUpdate
			} else {
				after.Spec.RolloutStrategy.Type = api.ServingGroupRollingUpdate
			}
			originalBefore, originalAfter := before.DeepCopy(), after.DeepCopy()
			c := lifecycleController(t, before)
			kube := c.kubeClientSet.(*kubefake.Clientset)
			client := c.modelServingClient.(*kthenafake.Clientset)
			kube.ClearActions()
			client.ClearActions()
			capture := captureModelServingLogs(t, 0)

			c.updateModelServing(before, after)

			require.Empty(t, recoveryLogsWith(capture, "reason", "PartitionChanged"), "switching active rollout scope does not rewrite either configured partition")
			require.NotEmpty(t, capture.entries(), "the actual configuration change remains visible")
			for _, entry := range capture.entries() {
				require.Equal(t, after.UID, entry.Fields["uid"])
				require.Equal(t, after.Generation, entry.Fields["generation"])
			}
			require.Equal(t, originalBefore, before)
			require.Equal(t, originalAfter, after)
			require.Empty(t, kube.Actions())
			require.Empty(t, client.Actions())
			require.Equal(t, 1, c.workqueue.Len())
			key, quit := c.workqueue.Get()
			require.False(t, quit)
			require.Equal(t, utils.GetNamespaceName(after).String(), key)
			c.workqueue.Done(key)
			c.workqueue.Forget(key)
		})
	}
}
