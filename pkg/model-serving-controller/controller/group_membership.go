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

	"k8s.io/apimachinery/pkg/labels"

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
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
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
		// Informer storage may lose the last Pod before its deletion callback
		// completes the Role plan. Keep the identity until that cleanup finishes.
		if c.podDeletionPlanActive(ms, group.Name) {
			c.enqueueModelServingAfter(ms, enqueueAfter)
			continue
		}
		if err := c.runServingGroupDeletePlugins(ctx, ms, group.Name); err != nil {
			return err
		}
		c.store.DeleteServingGroup(utils.GetNamespaceName(ms), group.Name)
	}
	return nil
}

// Empty capacity exists only as a status projection. Never insert these
// records into the store: they have no applied template to preserve when
// membership becomes positive again. Actual/retiring groups still occupy slots.
func (c *ModelServingController) emptyServingGroupStatus(ms *api.ModelServing, groups []datastore.ServingGroup, revision string, partition int) ([]datastore.ServingGroup, error) {
	if !allRolesZero(ms) {
		return nil, nil
	}
	occupied := make(map[string]bool, len(groups))
	for _, group := range groups {
		occupied[group.Name] = true
	}
	var empty []datastore.ServingGroup
	// A cold-start Terminating Pod may not have a store record yet. Include
	// its physical identity (also outside the formal range) without Ready credit.
	pods, err := c.podsLister.Pods(ms.Namespace).List(labels.Set{api.ModelServingNameLabelKey: ms.Name}.AsSelector())
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		name := pod.Labels[api.GroupNameLabelKey]
		if name == "" || occupied[name] || !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		occupied[name] = true
		empty = append(empty, datastore.ServingGroup{Name: name, Revision: utils.ObjectRevision(pod), Status: datastore.ServingGroupCreating})
	}
	for ordinal := 0; len(groups)+len(empty) < modelServingReplicas(ms); ordinal++ {
		name := utils.GenerateServingGroupName(ms.Name, ordinal)
		if occupied[name] {
			continue
		}
		selected := revision
		if servingGroupRollout(ms) && ordinal < partition && ms.Status.CurrentRevision != "" {
			selected = ms.Status.CurrentRevision
		}
		status := datastore.ServingGroupRunning
		if c.podDeletionPlanActive(ms, name) {
			status = datastore.ServingGroupCreating
		}
		empty = append(empty, datastore.ServingGroup{Name: name, Revision: selected, Status: status})
	}
	return empty, nil
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
