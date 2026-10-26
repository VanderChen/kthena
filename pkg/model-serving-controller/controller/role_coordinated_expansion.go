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

	"k8s.io/klog/v2"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// stageCoordinatedExpansionGang only changes the transient scheduling input.
// Replica accounting, rollout budgets and Pod creation always use the real spec.
// Keep a blocked caller's published default minimum until its dependency startup
// gate opens, so the missing caller cannot prevent its dependencies scheduling.
func (c *ModelServingController) stageCoordinatedExpansionGang(
	ctx context.Context,
	ms, podGroupMS *workloadv1alpha1.ModelServing,
	groupName string,
) error {
	coordination := roleCoordination(ms)
	if coordination == nil || len(coordination.Dependencies) == 0 || ms.Spec.SchedulerName != "volcano" {
		return nil
	}
	informer := c.podGroupManager.GetPodGroupInformer()
	if informer == nil {
		return nil
	}
	obj, exists, err := informer.GetIndexer().GetByKey(ms.Namespace + "/" + groupName)
	if err != nil {
		return fmt.Errorf("read coordinated expansion PodGroup %s: %w", groupName, err)
	}
	if !exists {
		return nil
	}
	pg, ok := obj.(*schedulingv1beta1.PodGroup)
	if !ok || !pg.DeletionTimestamp.IsZero() || !utils.IsOwnedByModelServingWithUID(pg, ms.UID) {
		return nil
	}
	// Published minima are committed capacity, not the current number of healthy
	// Pods. In particular, an ordinary failure must not lower this baseline.
	deferred := make(map[string]int32)
	for _, role := range ms.Spec.Template.Roles {
		if ms.Spec.Template.GangPolicy != nil {
			if _, explicit := ms.Spec.Template.GangPolicy.MinRoleReplicas[role.Name]; explicit {
				continue
			}
		}
		for _, subgroup := range pg.Spec.SubGroupPolicy {
			if subgroup.Name == role.Name && subgroup.MinSubGroups != nil &&
				*subgroup.MinSubGroups >= 0 && int(*subgroup.MinSubGroups) < roleReplicas(role) {
				deferred[role.Name] = *subgroup.MinSubGroups
				break
			}
		}
	}
	if len(deferred) == 0 {
		return nil
	}
	history := c.revisionHistory(ctx, ms)
	revision := history.target
	if revision == "" {
		revision, err = history.desiredRevision(ctx)
		if err != nil {
			return err
		}
	}
	policy, err := c.resolveRoleRolloutPolicy(ctx, ms, revision)
	if err != nil {
		return fmt.Errorf("resolve coordinated expansion for PodGroup %s: %w", groupName, err)
	}
	for roleName, minimum := range deferred {
		if policy.allowTargetStart(groupName, roleName) {
			continue
		}
		if podGroupMS.Spec.Template.GangPolicy == nil {
			podGroupMS.Spec.Template.GangPolicy = &workloadv1alpha1.GangPolicy{}
		}
		if podGroupMS.Spec.Template.GangPolicy.MinRoleReplicas == nil {
			podGroupMS.Spec.Template.GangPolicy.MinRoleReplicas = make(map[string]int32)
		}
		podGroupMS.Spec.Template.GangPolicy.MinRoleReplicas[roleName] = minimum
		klog.V(4).InfoS("[MS-PENDING-DEBUG] Deferring coordinated expansion Gang minimum until dependencies are ready",
			"modelServing", klog.KObj(ms), "servingGroup", groupName, "role", roleName, "minimum", minimum)
	}
	return nil
}
