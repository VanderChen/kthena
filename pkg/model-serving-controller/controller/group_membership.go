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

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func servingGroupRollout(ms *api.ModelServing) bool {
	return ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == api.ServingGroupRollingUpdate
}

// All-zero desired membership discards empty groups' applied templates and
// temporary identities after actual Pods (including Terminating Pods) disappear.
// Positive desired membership retains the SG's applied template during local
// Role recovery, even when no Role or Pod currently remains in the group.
func (c *ModelServingController) forgetEmptyServingGroups(ctx context.Context, ms *api.ModelServing, revision string) error {
	if !allRolesZero(ms) {
		return nil
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		return nil
	}
	partition, _, err := c.getPartition(modelServingPartition(ms), modelServingReplicas(ms))
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		virtual := true
		for _, instances := range roles {
			virtual = virtual && len(instances) == 0
		}
		pods, err := c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, group.Name))
		if err != nil {
			return err
		}
		occupied := false
		for _, pod := range pods {
			occupied = occupied || utils.IsOwnedByModelServingWithUID(pod, ms.UID)
		}
		if occupied {
			continue
		}
		_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
		expected := revision
		if ordinal < partition && ms.Status.CurrentRevision != "" {
			expected = ms.Status.CurrentRevision
		}
		if virtual && ordinal >= 0 && ordinal < modelServingReplicas(ms) && group.Revision == expected {
			continue
		}
		if err := c.runServingGroupDeletePlugins(ctx, ms, group.Name); err != nil {
			return err
		}
		c.store.DeleteServingGroup(utils.GetNamespaceName(ms), group.Name)
	}
	return nil
}

func allRolesZero(ms *api.ModelServing) bool {
	for _, role := range ms.Spec.Template.Roles {
		if roleReplicas(role) != 0 {
			return false
		}
	}
	return true
}

// Scaling progress is measured from actual instances, not a persisted target
// that can already equal the spec while old Pods still terminate.
func (c *ModelServingController) groupMembersPending(ms *api.ModelServing, group string) bool {
	if !servingGroupRollout(ms) {
		return false
	}
	for _, role := range ms.Spec.Template.Roles {
		instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group, role.Name)
		if err != nil || len(instances) != roleReplicas(role) {
			return true
		}
		for _, instance := range instances {
			if instance.Status == datastore.RoleDeleting {
				return true
			}
		}
	}
	return c.rolloutDeletionPending(ms, group, "", "")
}
