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
	"sort"
	"sync/atomic"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
)

const (
	deleteGroupScope = "ServingGroup"
	deleteRoleScope  = "Role"
)

type podDeletionPlanKey struct {
	Owner    types.NamespacedName
	UID      types.UID
	Scope    string
	Group    string
	Role     string
	Instance string
}

// podDeletionPlan exists only while this controller process is running. It
// keeps retries within one Role/ServingGroup recovery bound to the UIDs that
// were selected before the first DELETE. A controller restart intentionally
// discards the plan and reconciles from the Pods that still exist.
type podDeletionPlan struct {
	Key     podDeletionPlanKey
	Pods    []corev1.Pod
	Started atomic.Bool
}

func deletionPlanKey(ms *api.ModelServing, scope, group, role, instance string) podDeletionPlanKey {
	if scope == deleteGroupScope {
		role, instance = "", ""
	}
	return podDeletionPlanKey{
		Owner:    utils.GetNamespaceName(ms),
		UID:      ms.UID,
		Scope:    scope,
		Group:    group,
		Role:     role,
		Instance: instance,
	}
}

func (c *ModelServingController) ownedDeletionPods(ctx context.Context, ms *api.ModelServing, selector labels.Selector) ([]corev1.Pod, error) {
	list, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return nil, err
	}
	pods := make([]corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			pods = append(pods, *pod)
		}
	}
	return pods, nil
}

func (c *ModelServingController) preparePodDeletionPlan(ctx context.Context, ms *api.ModelServing, selector labels.Selector, scope string) (*podDeletionPlan, error) {
	group, ok := selector.RequiresExactMatch(api.GroupNameLabelKey)
	if !ok || group == "" {
		return nil, fmt.Errorf("Pod deletion requires an exact ServingGroup")
	}
	role, _ := selector.RequiresExactMatch(api.RoleLabelKey)
	instance, _ := selector.RequiresExactMatch(api.RoleIDKey)
	key := deletionPlanKey(ms, scope, group, role, instance)
	if current, ok := c.deletionPlans.Load(key); ok {
		return current.(*podDeletionPlan), nil
	}
	if err := c.checkRolloutIntent(ctx, ms); err != nil {
		return nil, err
	}
	pods, err := c.ownedDeletionPods(ctx, ms, selector)
	if err != nil {
		return nil, err
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	if err := c.checkRolloutIntent(ctx, ms); err != nil {
		return nil, err
	}
	plan := &podDeletionPlan{Key: key, Pods: pods}
	actual, loaded := c.deletionPlans.LoadOrStore(key, plan)
	if loaded {
		return actual.(*podDeletionPlan), nil
	}
	return plan, nil
}

func deletionPlanContains(plan *podDeletionPlan, pod *corev1.Pod) bool {
	for i := range plan.Pods {
		if plan.Pods[i].Name == pod.Name && plan.Pods[i].UID == pod.UID {
			return true
		}
	}
	return false
}

func definiteDeleteRejection(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsForbidden(err) || apierrors.IsInvalid(err) || apierrors.IsUnauthorized(err)
}

func (c *ModelServingController) deletePlannedPods(ctx context.Context, ms *api.ModelServing, plan *podDeletionPlan) error {
	if !plan.Started.Load() {
		if err := c.checkRolloutIntent(ctx, ms); err != nil {
			c.deletionPlans.Delete(plan.Key)
			return err
		}
	}
	for i := range plan.Pods {
		original := &plan.Pods[i]
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			plan.Started.Store(true)
			continue
		}
		if err != nil {
			return err
		}
		if pod.UID != original.UID || !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		if pod.DeletionTimestamp != nil {
			plan.Started.Store(true)
			continue
		}
		invalidateRolloutPodSnapshot(ctx)
		err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, *metav1.NewPreconditionDeleteOptions(string(original.UID)))
		if err == nil || apierrors.IsNotFound(err) {
			plan.Started.Store(true)
			continue
		}
		// A transport/server failure may have lost a successful DELETE response.
		// Keep the process-local plan active and retry only its original UIDs.
		if !definiteDeleteRejection(err) {
			plan.Started.Store(true)
		}
		return err
	}
	return nil
}

func (c *ModelServingController) clearDeletionPlan(ms *api.ModelServing, scope, group, role, instance string) {
	c.deletionPlans.Delete(deletionPlanKey(ms, scope, group, role, instance))
}

func (c *ModelServingController) resetDeletionStatus(ms *api.ModelServing, group, role, instance string) {
	key := utils.GetNamespaceName(ms)
	_ = c.store.UpdateServingGroupStatus(key, group, datastore.ServingGroupCreating)
	roles, _ := c.store.GetRolesByGroup(key, group)
	for name, instances := range roles {
		for id := range instances {
			if role == "" || (name == role && id == instance) {
				_ = c.store.UpdateRoleStatus(key, group, name, id, datastore.RoleCreating)
			}
		}
	}
}

// A same-name replacement is never added to an existing plan. Once every
// original UID is gone, preserve replacement resources and rebuild the
// process-local observation instead of deleting the replacement.
func (c *ModelServingController) finishDeletionWithReplacement(ctx context.Context, ms *api.ModelServing, plan *podDeletionPlan) (bool, error) {
	selector := labels.Set{api.GroupNameLabelKey: plan.Key.Group}
	if plan.Key.Scope == deleteRoleScope {
		selector[api.RoleLabelKey] = plan.Key.Role
		selector[api.RoleIDKey] = plan.Key.Instance
	}
	pods, err := c.ownedDeletionPods(ctx, ms, selector.AsSelector())
	if err != nil {
		return false, err
	}
	replacement, original := false, false
	for i := range pods {
		if deletionPlanContains(plan, &pods[i]) {
			original = true
		} else {
			replacement = true
		}
	}
	if !replacement {
		return false, nil
	}
	c.enqueueModelServingAfter(ms, enqueueAfter)
	if original {
		return true, nil
	}
	c.deletionPlans.Delete(plan.Key)
	key := utils.GetNamespaceName(ms)
	if plan.Key.Scope == deleteGroupScope {
		c.store.DeleteServingGroup(key, plan.Key.Group)
	} else {
		c.store.DeleteRole(key, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	}
	for i := range pods {
		pod := &pods[i]
		c.store.AddServingGroupAndRole(key, plan.Key.Group, utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod), utils.GetRoleName(pod), utils.GetRoleID(pod))
	}
	c.resetDeletionStatus(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	return true, nil
}

// Single-Pod recovery and conflict cleanup must not expand an active Role/SG
// plan while this controller is still completing that plan.
func (c *ModelServingController) podDeletionPlanActive(ms *api.ModelServing, group string) bool {
	active := false
	c.deletionPlans.Range(func(key, _ interface{}) bool {
		planKey := key.(podDeletionPlanKey)
		if planKey.Owner == utils.GetNamespaceName(ms) && planKey.UID == ms.UID && planKey.Group == group {
			active = true
			return false
		}
		return true
	})
	return active
}

// Retry plans only within the process that started them. A new controller has
// no plans and therefore converges from current Pods, current configuration and
// the normal rollout/recovery budgets.
func (c *ModelServingController) resumePodDeletionPlans(ctx context.Context, ms *api.ModelServing) error {
	plans := []*podDeletionPlan{}
	c.deletionPlans.Range(func(key, value interface{}) bool {
		planKey := key.(podDeletionPlanKey)
		if planKey.Owner == utils.GetNamespaceName(ms) && planKey.UID == ms.UID {
			plans = append(plans, value.(*podDeletionPlan))
		}
		return true
	})
	sort.Slice(plans, func(i, j int) bool {
		a, b := plans[i].Key, plans[j].Key
		return a.Group+"/"+a.Scope+"/"+a.Role+"/"+a.Instance < b.Group+"/"+b.Scope+"/"+b.Role+"/"+b.Instance
	})
	for _, plan := range plans {
		var err error
		if plan.Key.Scope == deleteGroupScope {
			err = c.deleteServingGroup(ctx, ms, plan.Key.Group)
		} else {
			err = c.DeleteRole(ctx, ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
