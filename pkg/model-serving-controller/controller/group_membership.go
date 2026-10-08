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
	"encoding/json"
	"fmt"
	"maps"
	"sort"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Applied member counts are operation state, not part of the template revision.
// The owner annotation is only a reconcile-local projection. Authoritative state
// lives in an owned ConfigMap, independently of user PUTs to the ModelServing.
const groupMembersAnnotation = utils.AppliedRoleReplicasAnnotation

type groupMemberTargets map[string]map[string]int32

func servingGroupRollout(ms *api.ModelServing) bool {
	return ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.Type == api.ServingGroupRollingUpdate
}
func memberCounts(roles []api.Role) map[string]int32 {
	result := map[string]int32{}
	for _, role := range roles {
		result[role.Name] = int32(roleReplicas(role))
	}
	return result
}
func groupTargets(ms *api.ModelServing) (groupMemberTargets, error) {
	result := groupMemberTargets{}
	if raw := ms.Annotations[groupMembersAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &result); err != nil {
			return nil, fmt.Errorf("invalid applied member targets: %w", err)
		}
	}
	if result == nil {
		result = groupMemberTargets{}
	}
	return result, nil
}
func rolesWithGroupMembers(ms *api.ModelServing, group string, roles []api.Role) ([]api.Role, error) {
	if !servingGroupRollout(ms) {
		return roles, nil
	}
	targets, err := groupTargets(ms)
	if err != nil {
		return nil, err
	}
	applied, exists := targets[group]
	if !exists {
		return roles, nil
	}
	result := make([]api.Role, 0, len(roles))
	for _, role := range roles {
		copy := role.DeepCopy()
		if count, ok := applied[role.Name]; ok {
			copy.Replicas = ptr.To(count)
		}
		result = append(result, *copy)
	}
	return result, nil
}

func groupMembersStateName(ms *api.ModelServing) string {
	return utils.GroupMembersStateName(ms)
}

func (c *ModelServingController) readGroupMembersState(ctx context.Context, ms *api.ModelServing) (*corev1.ConfigMap, error) {
	return utils.ReadGroupMembersState(ctx, c.kubeClientSet, ms)
}

// Return a copy: event callbacks must never mutate the informer object.
func (c *ModelServingController) withGroupMembersState(ctx context.Context, ms *api.ModelServing) (*api.ModelServing, error) {
	if !servingGroupRollout(ms) || ms.ResourceVersion == "" {
		return ms, nil
	}
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil {
		return nil, err
	}
	if cm == nil {
		return ms, nil
	} // One-time migration from the old owner annotation.
	result := ms.DeepCopy()
	if result.Annotations == nil {
		result.Annotations = map[string]string{}
	}
	result.Annotations[groupMembersAnnotation] = cm.Data["targets.json"]
	return result, nil
}

// The lifecycle lock serializes this owner's operations. resourceVersion checks
// prevent overwriting concurrent state writes; generation checks fence old spec.
func (c *ModelServingController) persistGroupTargets(ctx context.Context, ms *api.ModelServing, targets groupMemberTargets, revisions map[string]string) error {
	raw, err := json.Marshal(targets)
	if err != nil {
		return err
	}
	if ms.ResourceVersion != "" {
		if err := c.checkRolloutIntent(ctx, ms); err != nil {
			return err
		}
		cm, err := c.readGroupMembersState(ctx, ms)
		if err != nil {
			return err
		}
		creating := cm == nil
		if creating {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: groupMembersStateName(ms), Namespace: ms.Namespace,
				Labels:          map[string]string{api.ModelServingNameLabelKey: ms.Name},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, api.SchemeGroupVersion.WithKind("ModelServing"))},
			}, Data: map[string]string{}}
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		savedRevisions := map[string]string{}
		if value := cm.Data["revisions.json"]; value != "" {
			if err := json.Unmarshal([]byte(value), &savedRevisions); err != nil {
				return err
			}
		}
		for name := range savedRevisions {
			if _, ok := targets[name]; !ok {
				delete(savedRevisions, name)
			}
		}
		for name := range targets {
			if revision, ok := c.store.GetServingGroupRevision(utils.GetNamespaceName(ms), name); ok {
				savedRevisions[name] = revision
			}
		}
		for name, revision := range revisions {
			savedRevisions[name] = revision
		}
		revisionsJSON, err := json.Marshal(savedRevisions)
		if err != nil {
			return err
		}
		if creating || cm.Data["targets.json"] != string(raw) || cm.Data["revisions.json"] != string(revisionsJSON) {
			cm.Data["targets.json"] = string(raw)
			cm.Data["revisions.json"] = string(revisionsJSON)
			if creating {
				_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
			} else {
				_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
			}
			if err != nil {
				return err
			}
		}
	}
	if ms.Annotations == nil {
		ms.Annotations = map[string]string{}
	}
	ms.Annotations[groupMembersAnnotation] = string(raw)
	return nil
}

func (c *ModelServingController) setGroupMembers(ctx context.Context, ms *api.ModelServing, group, revision string, roles []api.Role) error {
	if !servingGroupRollout(ms) {
		return nil
	}
	projected, err := c.withGroupMembersState(ctx, ms)
	if err != nil {
		return err
	}
	ms.Annotations = maps.Clone(projected.Annotations)
	targets, err := groupTargets(ms)
	if err != nil {
		return err
	}
	targets[group] = memberCounts(roles)
	return c.persistGroupTargets(ctx, ms, targets, map[string]string{group: revision})
}

func (c *ModelServingController) ensureGroupMembers(ctx context.Context, ms *api.ModelServing) error {
	if !servingGroupRollout(ms) {
		return nil
	}
	if err := c.checkRolloutIntent(ctx, ms); err != nil {
		return err
	}
	projected, err := c.withGroupMembersState(ctx, ms)
	if err != nil {
		return err
	}
	ms.Annotations = maps.Clone(projected.Annotations)
	targets, err := groupTargets(ms)
	if err != nil {
		return err
	}
	// Empty groups have no Pods from which the informer could reconstruct them.
	if ms.ResourceVersion != "" {
		cm, err := c.readGroupMembersState(ctx, ms)
		if err != nil {
			return err
		}
		if cm != nil {
			revisions := map[string]string{}
			if raw := cm.Data["revisions.json"]; raw != "" {
				if err := json.Unmarshal([]byte(raw), &revisions); err != nil {
					return err
				}
			}
			for name, counts := range targets {
				empty := len(counts) > 0
				for _, n := range counts {
					empty = empty && n == 0
				}
				if !empty || c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), name) != datastore.ServingGroupNotFound {
					continue
				}
				parent, ordinal := utils.GetParentNameAndOrdinal(name)
				if parent != ms.Name || ordinal < 0 || revisions[name] == "" {
					return fmt.Errorf("invalid empty group member state for %s", name)
				}
				c.store.AddServingGroup(utils.GetNamespaceName(ms), ordinal, revisions[name])
			}
		}
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil {
		return nil
	} // No groups before initial placement.
	active := map[string]bool{}
	for _, group := range groups {
		active[group.Name] = true
		if _, exists := targets[group.Name]; exists {
			continue
		}
		counts := memberCounts(ms.Spec.Template.Roles)
		// Bootstrap a complete pre-upgrade group. Never reinterpret an incomplete
		// or unknown group as having a smaller desired layout after a cold start.
		if group.Status == datastore.ServingGroupRunning {
			for _, role := range ms.Spec.Template.Roles {
				instances, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group.Name, role.Name)
				if err != nil {
					return err
				}
				complete := true
				for _, instance := range instances {
					ready, err := c.checkRoleReadyWithContext(ctx, ms, group.Name, role.Name, instance.Name)
					if err != nil || !ready {
						complete = false
						break
					}
				}
				if complete {
					counts[role.Name] = int32(len(instances))
				}
			}
		}
		targets[group.Name] = counts
	}
	for group := range targets {
		if !active[group] {
			delete(targets, group)
		}
	}
	return c.persistGroupTargets(ctx, ms, targets, nil)
}

// A member change reserves one complete SG's availability before updating its
// PodGroup or Pods. Untouched groups continue using their own applied counts.
func (c *ModelServingController) applyGroupMemberChanges(ctx context.Context, ms *api.ModelServing, groups []datastore.ServingGroup) error {
	if !servingGroupRollout(ms) {
		return nil
	}
	projected, err := c.withGroupMembersState(ctx, ms)
	if err != nil {
		return err
	}
	ms.Annotations = maps.Clone(projected.Annotations)
	targets, err := groupTargets(ms)
	if err != nil {
		return err
	}
	unavailable, err := utils.GetMaxUnavailable(ms)
	if err != nil {
		return err
	}
	ready := 0
	for _, group := range groups {
		if group.Status == datastore.ServingGroupRunning {
			ready++
		}
	}
	budget := max(0, ready-max(0, modelServingReplicas(ms)-unavailable))
	sort.Slice(groups, func(i, j int) bool {
		_, a := utils.GetParentNameAndOrdinal(groups[i].Name)
		_, b := utils.GetParentNameAndOrdinal(groups[j].Name)
		return a > b
	})
	desired := memberCounts(ms.Spec.Template.Roles)
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting || group.Status == datastore.ServingGroupReadinessUnknown {
			continue
		}
		applied, exists := targets[group.Name]
		if !exists || maps.Equal(applied, desired) {
			continue
		}
		if group.Status == datastore.ServingGroupRunning && budget == 0 {
			continue
		}
		if group.Status == datastore.ServingGroupRunning {
			budget--
		}
		targets[group.Name] = maps.Clone(desired)
		if err := c.persistGroupTargets(ctx, ms, targets, nil); err != nil {
			return err
		}
		if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, datastore.ServingGroupScaling); err != nil {
			return err
		}
	}
	return nil
}

func groupMembersPending(ms *api.ModelServing, group string) bool {
	if !servingGroupRollout(ms) {
		return false
	}
	targets, err := groupTargets(ms)
	if err != nil {
		return false
	}
	applied, exists := targets[group]
	return exists && !maps.Equal(applied, memberCounts(ms.Spec.Template.Roles))
}

// Forget state only after all physical resources are gone. This prevents a
// completed empty-group deletion from being resurrected after restart.
func (c *ModelServingController) forgetGroupMembers(ctx context.Context, ms *api.ModelServing, group string) error {
	if !servingGroupRollout(ms) {
		return nil
	}
	projected, err := c.withGroupMembersState(ctx, ms.DeepCopy())
	if err != nil {
		return err
	}
	targets, err := groupTargets(projected)
	if err != nil {
		return err
	}
	if _, exists := targets[group]; !exists {
		return nil
	}
	delete(targets, group)
	if err := c.persistGroupTargets(ctx, projected, targets, nil); err != nil {
		return err
	}
	// A reconcile uses its copy in later phases; event callbacks must not write
	// into the informer. Callers that continue reconciling rehydrate from state.
	return nil
}
