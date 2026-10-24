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

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
)

// Carries diagnostics back to the worker error boundary without changing the
// returned error, issuing another API read, or retaining state between attempts.
type reconcileLogKey struct{}
type reconcileLog struct {
	logger klog.Logger
	stage  string
}

func modelServingLog(ms *api.ModelServing) klog.Logger {
	return modelServingContextLog(context.Background(), ms)
}

func modelServingContextLog(ctx context.Context, ms *api.ModelServing) klog.Logger {
	logger := klog.FromContext(ctx)
	if ms == nil {
		return logger
	}
	return logger.WithValues("modelServing", klog.KObj(ms), "uid", ms.UID, "generation", ms.Generation)
}

func partitionLogValues(partition *intstr.IntOrString, replicas int) (string, int) {
	if partition == nil {
		return "0", 0
	}
	resolved, err := intstr.GetScaledValueFromIntOrPercent(partition, replicas, true)
	if err != nil {
		// Observation only: validation and rollout decisions remain with their
		// existing owners. Do not present an invalid value as a zero boundary.
		return partition.String(), -1
	}
	return partition.String(), resolved
}

func configuredServingGroupPartition(ms *api.ModelServing) *intstr.IntOrString {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		return nil
	}
	return ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition
}

func logModelServingConfigurationChange(before, after *api.ModelServing) {
	logger := modelServingLog(after).WithValues("entrypoint", "modelServingUpdate", "previousGeneration", before.Generation)
	logger.Info("ModelServing configuration changed", "previousDesiredReplicas", modelServingReplicas(before), "desiredReplicas", modelServingReplicas(after))
	oldRaw, oldBoundary := partitionLogValues(configuredServingGroupPartition(before), modelServingReplicas(before))
	newRaw, newBoundary := partitionLogValues(configuredServingGroupPartition(after), modelServingReplicas(after))
	if oldRaw != newRaw || oldBoundary != newBoundary {
		active := after.Spec.RolloutStrategy == nil || after.Spec.RolloutStrategy.Type == api.ServingGroupRollingUpdate
		logger.Info("ServingGroup rollout partition changed", "action", "configureRollout", "reason", "PartitionChanged", "scope", "ServingGroup",
			"previousPartition", oldRaw, "partition", newRaw, "previousResolvedPartition", oldBoundary, "resolvedPartition", newBoundary, "active", active,
			"currentRevision", after.Status.CurrentRevision, "observedTargetRevision", after.Status.UpdateRevision)
	}
	for _, role := range after.Spec.Template.Roles {
		for _, previous := range before.Spec.Template.Roles {
			if previous.Name != role.Name {
				continue
			}
			oldRaw, oldBoundary := partitionLogValues(previous.Partition, roleReplicas(previous))
			newRaw, newBoundary := partitionLogValues(role.Partition, roleReplicas(role))
			if oldRaw != newRaw || oldBoundary != newBoundary {
				active := after.Spec.RolloutStrategy != nil && after.Spec.RolloutStrategy.Type == api.RoleRollingUpdate
				logger.Info("Role rollout partition changed", "action", "configureRollout", "reason", "PartitionChanged", "scope", "Role", "role", role.Name,
					"previousPartition", oldRaw, "partition", newRaw, "previousResolvedPartition", oldBoundary, "resolvedPartition", newBoundary, "active", active,
					"previousDesiredReplicas", roleReplicas(previous), "desiredReplicas", roleReplicas(role),
					"currentRevision", after.Status.CurrentRevision, "observedTargetRevision", after.Status.UpdateRevision)
			}
			break
		}
	}
}
