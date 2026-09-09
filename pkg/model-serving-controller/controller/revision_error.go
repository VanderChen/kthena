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
	corev1 "k8s.io/api/core/v1"
)

// scaleDownOnRevisionError applies only explicit replica reductions. It does
// not need to render templates, create Pods, or advance rollout identities.
func (c *ModelServingController) scaleDownOnRevisionError(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	key := utils.GetNamespaceName(ms)
	// A controller restart may encounter invalid history before populating its
	// datastore. Recover membership from this reconcile's scoped observation;
	// leave readiness and lifecycle hooks to their normal reconciliation paths.
	if c.observation != nil {
		for _, obj := range c.observation.pods.List() {
			pod := obj.(*corev1.Pod)
			if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
				continue
			}
			group, role, id := pod.Labels[workloadv1alpha1.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
			if group != "" && role != "" && id != "" {
				c.store.AddServingGroupAndRole(key, group, utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod), role, id)
			}
		}
	}
	groups, err := c.store.GetServingGroupByModelServing(key)
	if errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(groups) > modelServingReplicas(ms) {
		if err := c.scaleDownServingGroups(ctx, ms, groups, modelServingReplicas(ms)); err != nil {
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
			if len(roles) > int(*target.Replicas) {
				if err := c.scaleDownRoles(ctx, ms, group.Name, target, roles, int(*target.Replicas)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
