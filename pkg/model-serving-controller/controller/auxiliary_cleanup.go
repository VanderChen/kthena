/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permsssions and
limstations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sync"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

type auxiliaryCleanupKey struct {
	uid             types.UID
	group, role, id string
}

// finishPendingDeletions retries failed API requests before any new capacity can
// reuse an identity. Services with an accepted DELETE may still have finalizers.
func (c *ModelServingController) finishPendingDeletions(ctx context.Context, ms *workload.ModelServing) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && !errors.Is(err, datastore.ErrServingGroupNotFound) {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			if err := c.deleteServingGroup(ctx, ms, group.Name); err != nil {
				return err
			}
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for id, role := range instances {
				if role.Status == datastore.RoleDeleting {
					if err := c.DeleteRole(ctx, ms, group.Name, name, id); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// cleanupOrphanServices runs only after successful count/rollout reconciliation,
// when this key has no in-flight creates. Restart does not resurrect Role
// identities from Services. Current Pods and store creation/deletion facts win.
func (c *ModelServingController) cleanupOrphanServices(ctx context.Context, ms *workload.ModelServing) error {
	services, err := c.kubeClientSet.CoreV1().Services(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workload.ModelServingNameLabelKey: ms.Name}.String()})
	if err != nil {
		return err
	}
	for i := range services.Items {
		service := &services.Items[i]
		group, role, id, managed := plugins.ManagedHeadlessServiceIdentity(service, ms)
		if !managed || service.DeletionTimestamp != nil {
			continue
		}
		if c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role, id) != datastore.RoleNotFound {
			continue
		}
		// Also preserve desired slots while cache observation catches up. Orphans
		// outside the desired range (including zero-replica teardown) can be removed.
		_, groupIndex := utils.GetParentNameAndOrdinal(group)
		_, roleIndex := utils.GetParentNameAndOrdinal(id)
		needed := false
		if groupIndex < modelServingReplicas(ms) {
			for _, desired := range ms.Spec.Template.Roles {
				if desired.Name == role && roleIndex < roleReplicas(desired) {
					needed = true
				}
			}
		}
		if needed {
			continue
		}
		selector := labels.Set{workload.GroupNameLabelKey: group, workload.RoleLabelKey: role, workload.RoleIDKey: id}
		pods, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		if err != nil {
			return err
		}
		// Even an object with uncertain ownership is reason to wait, not to delete
		// networking while another actor might be reusing this instance.
		if len(pods.Items) != 0 {
			continue
		}
		if c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role, id) != datastore.RoleNotFound {
			continue
		}
		uid := service.UID
		err = c.kubeClientSet.CoreV1().Services(ms.Namespace).Delete(ctx, service.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil {
			return fmt.Errorf("cleanup orphan Headless Service %s/%s: %w", ms.Namespace, service.Name, err)
		}
	}
	return nil
}

func (c *ModelServingController) prepareRoleCleanup(ctx context.Context, ms *workload.ModelServing, group, role, id string) error {
	cleanup, _ := c.roleCleanups.LoadOrStore(auxiliaryCleanupKey{ms.UID, group, role, id}, &plugins.RoleCleanup{})
	return cleanup.(*plugins.RoleCleanup).Capture(ctx, &plugins.HookRequest{ModelServing: ms, ServingGroup: group, RoleName: role, RoleID: id, KubeClient: c.kubeClientSet})
}

func (c *ModelServingController) forgetGroupCleanup(uid types.UID, group string) {
	for _, records := range []*sync.Map{&c.roleCleanups, &c.podDeletions} {
		records.Range(func(key, _ any) bool {
			k := key.(auxiliaryCleanupKey)
			if k.uid == uid && k.group == group {
				records.Delete(key)
			}
			return true
		})
	}
}
