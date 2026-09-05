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
	"fmt"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
)

var errAuditRequeue = errors.New("refresh observation after cleanup")

// Cleanup intent outlives a failed API request, an empty Pod observation, and
// the legacy state machine's status rollback. Never infer cleanup from Pods alone.
func (c *ModelServingController) resumeObservedDeletions(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	state := c.servingState
	key := utils.GetNamespaceName(ms)
	groups, err := c.store.GetServingGroupByModelServing(key)
	if err != nil && err != datastore.ErrServingGroupNotFound {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			state.groupDeletes[group.Name] = struct{}{}
		}
		roles, err := c.store.GetRolesByGroup(key, group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for id, role := range instances {
				if role.Status == datastore.RoleDeleting {
					state.roleDeletes[roleCleanupKey{group.Name, name, id}] = struct{}{}
				}
			}
		}
	}
	progressed := len(state.groupDeletes) > 0 || len(state.roleDeletes) > 0
	for group := range state.groupDeletes {
		if err := c.deleteServingGroup(ctx, ms, group); err != nil {
			return err
		}
		if c.store.GetServingGroupStatus(key, group) == datastore.ServingGroupNotFound {
			delete(state.groupDeletes, group)
			for role := range state.roleDeletes {
				if role.group == group {
					delete(state.roleDeletes, role)
				}
			}
		} else {
			state.live = true
			c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
		}
	}
	for role := range state.roleDeletes {
		if _, deleting := state.groupDeletes[role.group]; deleting {
			continue
		}
		if c.store.GetRoleStatus(key, role.group, role.role, role.id) != datastore.RoleNotFound {
			if err := c.store.UpdateRoleStatus(key, role.group, role.role, role.id, datastore.RoleDeleting); err != nil {
				return err
			}
		}
		selector := labels.SelectorFromSet(map[string]string{workloadv1alpha1.GroupNameLabelKey: role.group, workloadv1alpha1.RoleLabelKey: role.role, workloadv1alpha1.RoleIDKey: role.id})
		if err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).DeleteCollection(ctx, metav1.DeleteOptions{}, metav1.ListOptions{LabelSelector: selector.String()}); err != nil {
			return err
		}
		if err := c.runRoleDeletePlugins(ctx, ms, role.group, role.role, role.id); err != nil {
			return fmt.Errorf("resume Role cleanup: %w", err)
		}
		deleted, err := c.isRoleDeletedLive(ctx, ms, role.group, role.role, role.id)
		if err != nil {
			return err
		}
		if !deleted {
			state.live = true
			c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
			continue
		}
		c.store.DeleteRole(key, role.group, role.role, role.id)
		c.clearRoleDeletionProgress(ms, role.group, role.role, role.id)
		delete(state.roleDeletes, role)
		c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
	}
	if progressed {
		state.live = true
		c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
		return errAuditRequeue
	}
	return nil
}

func (c *ModelServingController) cleanupRetiredModelServing(ctx context.Context, ms *workloadv1alpha1.ModelServing, state *servingAuditState) error {
	observation, err := c.readObservation(ctx, ms, true)
	if err != nil {
		return err
	}
	view := c.observationView(ctx, ms, observation, state)
	// Retirement is authorized for the old UID only. Do not run normal
	// reconciliation or let old cleanup adopt/delete a same-name new object.
	for _, object := range observation.pods.List() {
		pod := object.(*corev1.Pod)
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		if err := c.deleteConflictingPod(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	if state != nil {
		for _, progress := range state.pods {
			pod := progress.pod
			latest, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil && latest.UID == pod.UID {
				return fmt.Errorf("retired Pod %s is still deleting", pod.Name)
			}
			chain, err := view.buildPluginChain(ms)
			if err != nil {
				return err
			}
			if chain != nil {
				if err := chain.OnPodDelete(ctx, view.podHookRequest(ms, pod)); err != nil {
					return err
				}
			}
		}
	}
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && err != datastore.ErrServingGroupNotFound {
		return err
	}
	for _, group := range groups {
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for id := range instances {
				if err := view.runRoleDeletePlugins(ctx, ms, group.Name, name, id); err != nil {
					return err
				}
			}
		}
		if err := view.runServingGroupDeletePlugins(ctx, ms, group.Name); err != nil {
			return err
		}
		if err := c.podGroupManager.DeletePodGroup(ctx, ms, group.Name); err != nil {
			return err
		}
	}
	remaining, err := c.readObservation(ctx, ms, true)
	if err != nil {
		return err
	}
	for _, object := range append(remaining.pods.List(), remaining.services.List()...) {
		if utils.IsOwnedByModelServingWithUID(object.(metav1.Object), ms.UID) {
			return fmt.Errorf("retired ModelServing resources are still deleting")
		}
	}
	if remaining.podGroups != nil {
		for _, object := range remaining.podGroups.List() {
			if utils.IsOwnedByModelServingWithUID(object.(metav1.Object), ms.UID) {
				return fmt.Errorf("retired ModelServing PodGroups are still deleting")
			}
		}
	}
	return nil
}
