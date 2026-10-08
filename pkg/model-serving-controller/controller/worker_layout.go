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

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
)

// roleTemplateForNewReplica applies Role partition to a complete new instance,
// whether its ServingGroup already exists or is being created in the same round.
// Partial repairs use roleTemplateForInstance to preserve their applied layout.
func (c *ModelServingController) roleTemplateForNewReplica(ctx context.Context, ms *workloadv1alpha1.ModelServing, role workloadv1alpha1.Role, ordinal int, targetRevision string) (workloadv1alpha1.Role, string, error) {
	partition, _, err := c.getPartition(rolePartition(ms, role), roleReplicas(role))
	if err != nil {
		return workloadv1alpha1.Role{}, "", fmt.Errorf("parse partition for Role %s: %w", role.Name, err)
	}
	if ordinal >= partition {
		return role, targetRevision, nil
	}
	revision := targetRevision
	if ms.Status.CurrentRevision != "" {
		revision = ms.Status.CurrentRevision
	}
	// The caller persists the initial target before creation. Once a baseline
	// exists, missing history must not fall back to the latest template.
	applied, err := c.revisionHistory(ctx, ms).role(ctx, revision, role.Name)
	if err != nil {
		return workloadv1alpha1.Role{}, "", fmt.Errorf("resolve protected Role %s/%d at revision %s: %w", role.Name, ordinal, revision, err)
	}
	return applied, revision, nil
}

// podGroupRolesForNewGroup projects the layouts that will actually be created.
// Like modelServingForPodGroup, mixed worker layouts use the smaller subgroup
// size; an entirely protected Role must use its historical size in either direction.
func (c *ModelServingController) podGroupRolesForNewGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, roles []workloadv1alpha1.Role, revision string) ([]workloadv1alpha1.Role, error) {
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return roles, nil
	}
	projected := make([]workloadv1alpha1.Role, 0, len(roles))
	for _, role := range roles {
		effective := role
		if replicas := roleReplicas(role); replicas > 0 {
			first, _, err := c.roleTemplateForNewReplica(ctx, ms, role, 0, revision)
			if err != nil {
				return nil, err
			}
			last, _, err := c.roleTemplateForNewReplica(ctx, ms, role, replicas-1, revision)
			if err != nil {
				return nil, err
			}
			effective = first
			if last.WorkerReplicas < first.WorkerReplicas {
				effective = last
			}
			effective.Replicas = role.Replicas
		}
		projected = append(projected, effective)
	}
	return projected, nil
}

// roleTemplateForInstance resolves an existing replica's layout. The entry Pod
// anchors its identity after a controller restart, regardless of informer order.
// Desired Role replicas and rollout settings do not change this identity.
func (c *ModelServingController) roleTemplateForInstance(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	groupName, roleName string,
	observed datastore.Role,
	pods []*corev1.Pod,
) (workloadv1alpha1.Role, string, string, error) {
	for _, pod := range pods {
		if pod.Name == utils.GeneratePodName(groupName, observed.Name, 0) && utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			observed.Revision = utils.ObjectRevision(pod)
			observed.RoleTemplateHash = utils.ObjectRoleTemplateHash(pod)
			break
		}
	}
	if observed.Revision == "" {
		observed.Revision, _ = c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), groupName)
	}
	for _, desired := range ms.Spec.Template.Roles {
		if desired.Name == roleName && observed.Revision != "" &&
			(observed.Revision == utils.ModelServingRevision(ms) || observed.RoleTemplateHash == utils.CalRoleTemplateHash(desired)) {
			return *desired.DeepCopy(), observed.Revision, utils.CalRoleTemplateHash(desired), nil
		}
	}
	if observed.Revision == "" {
		return workloadv1alpha1.Role{}, "", "", fmt.Errorf("cannot resolve revision for Role %s/%s", groupName, observed.Name)
	}
	role, err := c.revisionHistory(ctx, ms).role(ctx, observed.Revision, roleName)
	if err != nil {
		return workloadv1alpha1.Role{}, "", "", err
	}
	hash := observed.RoleTemplateHash
	if hash == "" {
		hash = utils.CalRoleTemplateHash(role)
	}
	return role, observed.Revision, hash, nil
}

// modelServingForPodGroup projects live layouts without changing desired spec.
// A single SubGroupPolicy cannot express different worker counts for replicas
// of one Role. While they coexist, use the smallest observed layout so the next
// replacement can be scheduled in either direction. Role readiness and rollout
// budgets still require every replica's complete, revision-specific layout.
func (c *ModelServingController) modelServingForPodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string) (*workloadv1alpha1.ModelServing, error) {
	groupRevision, exists := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), groupName)
	if !exists {
		return ms, nil
	}
	roles, err := c.rolesForServingGroupReadiness(ms, groupName)
	if err != nil {
		return nil, err
	}
	projected := ms.DeepCopy()
	projected.Spec.Template.Roles = nil
	for _, desired := range roles {
		instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), groupName, desired.Name)
		if err != nil {
			return nil, err
		}
		effective := *desired.DeepCopy()
		found := false
		for _, instance := range instances {
			if instance.Status == datastore.RoleDeleting {
				continue
			}
			pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, groupName, desired.Name, instance.Name))
			if err != nil {
				return nil, err
			}
			template, _, _, err := c.roleTemplateForInstance(ctx, ms, groupName, desired.Name, instance, pods)
			if err != nil {
				return nil, err
			}
			if !found || template.WorkerReplicas < effective.WorkerReplicas {
				effective = template
			}
			found = true
		}
		if !found && (ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate) {
			effective, _, _, err = c.roleTemplateForInstance(ctx, ms, groupName, desired.Name, datastore.Role{Revision: groupRevision}, nil)
			if err != nil {
				return nil, err
			}
		}
		effective.Replicas = desired.Replicas
		projected.Spec.Template.Roles = append(projected.Spec.Template.Roles, effective)
	}
	return projected, nil
}
