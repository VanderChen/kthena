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

// scaleDownOnRevisionError applies only explicit replica reductions. It does
// not need to render templates, create Pods, or advance rollout identities.
func (c *ModelServingController) scaleDownOnRevisionError(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	key := utils.GetNamespaceName(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	if errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	replicas := modelServingReplicas(ms)
	formalGroups := groups
	if replicas > 0 {
		pods, err := c.surgePods(ms, surgeServingGroup, "", "")
		if err != nil {
			return err
		}
		temporary := markedSurgeNames(pods, surgeServingGroup, replicas)
		formalGroups = make([]datastore.ServingGroup, 0, len(groups))
		for _, group := range groups {
			if !temporary.Has(group.Name) {
				formalGroups = append(formalGroups, group)
			}
		}
	}
	// Temporary capacity is not an explicit replica reduction. Scaling to
	// zero is the exception: it removes all capacity without reading history.
	if len(formalGroups) > replicas {
		if err := c.scaleDownServingGroups(ctx, ms, formalGroups, replicas); err != nil {
			return err
		}
	}
	for _, group := range groups {
		if c.store.GetServingGroupStatus(key, group.Name) == datastore.ServingGroupDeleting {
			continue
		}
		// Only explicitly configured counts of existing named Roles can shrink.
		// Missing/deleted Role types and unknown historical templates are untouched.
		for _, target := range ms.Spec.Template.Roles {
			if target.Replicas == nil {
				continue
			}
			roles, err := c.store.GetRoleList(key, group.Name, target.Name)
			if err != nil {
				return err
			}
			replicas := int(*target.Replicas)
			formalRoles := roles
			if replicas > 0 {
				pods, err := c.surgePods(ms, surgeRole, group.Name, target.Name)
				if err != nil {
					return err
				}
				temporary := markedSurgeNames(pods, surgeRole, replicas)
				formalRoles = make([]datastore.Role, 0, len(roles))
				for _, role := range roles {
					if !temporary.Has(role.Name) {
						formalRoles = append(formalRoles, role)
					}
				}
			}
			if len(formalRoles) > replicas {
				if err := c.scaleDownRoles(ctx, ms, group.Name, target, formalRoles, replicas); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
