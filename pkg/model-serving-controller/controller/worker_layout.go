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

// instanceTemplate resolves the layout of an existing Role, independently of
// the desired replica count. An entry Pod anchors the identity after restart;
// a newly added worker must not promote the whole Role to its revision.
func (h *revisionHistory) instanceTemplate(ctx context.Context, groupName, roleName string, observed datastore.Role, pods []*corev1.Pod) (workloadv1alpha1.Role, string, string, error) {
	for _, pod := range pods {
		if pod.Name == utils.GeneratePodName(groupName, observed.Name, 0) && utils.IsOwnedByModelServingWithUID(pod, h.ms.UID) {
			observed.Revision = utils.ObjectRevision(pod)
			observed.RoleTemplateHash = utils.ObjectRoleTemplateHash(pod)
			break
		}
	}
	if observed.Revision == "" {
		observed.Revision, _ = h.controller.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName)
	}
	// An exact template hash proves the current template can be used even when
	// this Role kept an older group revision after a different Role was updated.
	for _, desired := range h.ms.Spec.Template.Roles {
		if desired.Name == roleName && observed.RoleTemplateHash == utils.CalRoleTemplateHash(desired) && observed.Revision != "" {
			return desired, observed.Revision, observed.RoleTemplateHash, nil
		}
	}
	roles, err := h.roles(ctx, observed.Revision)
	if err != nil {
		return workloadv1alpha1.Role{}, "", "", err
	}
	for _, role := range roles {
		if role.Name == roleName {
			hash := observed.RoleTemplateHash
			if hash == "" {
				hash = utils.CalRoleTemplateHash(role)
			}
			return role, observed.Revision, hash, nil
		}
	}
	return workloadv1alpha1.Role{}, "", "", &revisionResolutionError{fmt.Errorf("role %s is missing from ControllerRevision %s", roleName, observed.Revision)}
}

func (h *revisionHistory) podMatchesTemplate(ctx context.Context, pod *corev1.Pod, role workloadv1alpha1.Role, revision string) (bool, error) {
	if (revision != "" && utils.ObjectRevision(pod) == revision) || utils.ObjectRoleTemplateHash(pod) == utils.CalRoleTemplateHash(role) {
		return true, nil
	}
	roles, err := h.roles(ctx, utils.ObjectRevision(pod))
	if err != nil {
		return false, err
	}
	for _, historical := range roles {
		if historical.Name == role.Name {
			return utils.EqualRoleTemplatesForRevision([]workloadv1alpha1.Role{historical}, []workloadv1alpha1.Role{role}), nil
		}
	}
	return false, fmt.Errorf("role %s is missing from Pod %s revision", role.Name, pod.Name)
}

// podGroupModelServing changes only the Roles used for scheduling requirements.
// Global scheduling settings remain current. Each live Role type keeps its own
// layout, including during RoleRollingUpdate of other Role types in this group.
func (h *revisionHistory) podGroupModelServing(ctx context.Context, groupName string) (*workloadv1alpha1.ModelServing, error) {
	if h.controller.store == nil {
		return h.ms, nil
	}
	if _, exists := h.controller.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName); !exists {
		return h.ms, nil
	}
	roles, err := h.controller.rolesForServingGroupReadiness(h.ms, groupName)
	if err != nil {
		return nil, err
	}
	copy := h.ms.DeepCopy()
	copy.Spec.Template.Roles = nil
	for _, desired := range roles {
		instances, err := h.controller.store.GetRoleList(utils.GetNamespaceName(h.ms), groupName, desired.Name)
		if err != nil {
			return nil, err
		}
		effective := desired
		found := false
		for _, instance := range instances {
			if instance.Status == datastore.RoleDeleting {
				continue
			}
			pods, err := h.controller.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", h.ms.Namespace, groupName, desired.Name, instance.Name))
			if err != nil {
				return nil, err
			}
			template, _, _, err := h.instanceTemplate(ctx, groupName, desired.Name, instance, pods)
			if err != nil {
				return nil, err
			}
			if found && !utils.EqualRoleTemplatesForRevision([]workloadv1alpha1.Role{effective}, []workloadv1alpha1.Role{template}) {
				// One SubGroupPolicy cannot describe different layouts of the
				// same Role type. Retain its existing requirements until rollout
				// replaces these instances; do not invent a new scheduling layout.
				return nil, &revisionResolutionError{fmt.Errorf("role %s in ServingGroup %s has mixed instance templates", desired.Name, groupName)}
			}
			effective, found = template, true
		}
		if !found && (h.ms.Spec.RolloutStrategy == nil || h.ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate) {
			revision, _ := h.controller.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName)
			effective, _, _, err = h.instanceTemplate(ctx, groupName, desired.Name, datastore.Role{Revision: revision}, nil)
			if err != nil {
				return nil, err
			}
		}
		effective.Replicas = desired.Replicas
		copy.Spec.Template.Roles = append(copy.Spec.Template.Roles, *effective.DeepCopy())
	}
	return copy, nil
}
