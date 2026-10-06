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
	"errors"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// rolloutBudget is shared by SG and per-SG/per-Role rollouts. Unavailable counts
// target/unknown unavailable instances and committed deletions still in total.
// A deletion is counted once, regardless of its version (V or in-flight I).
// Old unavailable instances are candidates, not an unconditional charge to V.
type rolloutBudget struct {
	total   int
	healthy int
}

func newRolloutBudget(replicas, maxUnavailable, total, ready, unavailable int) rolloutBudget {
	minimum := max(0, replicas-maxUnavailable)
	return rolloutBudget{total: max(0, total-minimum-unavailable), healthy: max(0, ready-minimum)}
}

func (b *rolloutBudget) take(ready bool) bool {
	if b.total <= 0 || (ready && b.healthy <= 0) {
		return false
	}
	b.total--
	if ready {
		b.healthy--
	}
	return true
}

// Refresh only cached Ready claims. Creating instances gain credit through the
// ordinary readiness path; a missing/erroring observation never grants credit.
func (c *ModelServingController) refreshRolloutAvailability(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	key := utils.GetNamespaceName(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	if errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(key, group.Name)
		if err != nil {
			return err
		}
		for roleName, instances := range roles {
			for _, instance := range instances {
				if instance.Status != datastore.RoleRunning {
					continue
				}
				ready, readyErr := c.checkRoleReadyWithContext(ctx, ms, group.Name, roleName, instance.Name)
				if readyErr == nil && ready {
					continue
				}
				if err := c.store.UpdateRoleStatus(key, group.Name, roleName, instance.Name, datastore.RoleCreating); err != nil {
					return err
				}
			}
		}
		if group.Status == datastore.ServingGroupRunning {
			ready, readyErr := c.checkServingGroupReady(ms, group.Name)
			if readyErr != nil || !ready {
				if err := c.store.UpdateServingGroupStatus(key, group.Name, datastore.ServingGroupCreating); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
