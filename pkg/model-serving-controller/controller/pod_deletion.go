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
	"k8s.io/klog/v2"
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

// podDeletionPlan keeps retries bound to the UIDs selected before the first
// DELETE. After a restart, only surviving marked UIDs restore this plan.
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
			c.observeKnownMember(ms, pod)
			pods = append(pods, *pod)
		}
	}
	return pods, nil
}

type observedPodDeletionKey struct{}

// Carry the UIDs selected before the absence GET to the original plan creation
// point. Constructing this snapshot does not accept a deletion operation.
func withObservedPodDeletion(ctx context.Context, ms *api.ModelServing, pod *corev1.Pod, siblings []*corev1.Pod) context.Context {
	if ms.Spec.RecoveryPolicy == api.NoneRestartPolicy {
		return ctx
	}
	group, role, instance := pod.Labels[api.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
	scope := deleteRoleScope
	if ms.Spec.RecoveryPolicy == api.ServingGroupRecreate {
		scope = deleteGroupScope
	}
	selected := []corev1.Pod{*memberIdentity(pod)}
	for _, sibling := range siblings {
		if !utils.IsOwnedByModelServingWithUID(sibling, ms.UID) || sibling.Labels[api.GroupNameLabelKey] != group || sibling.Name == pod.Name ||
			(scope == deleteRoleScope && (utils.GetRoleName(sibling) != role || utils.GetRoleID(sibling) != instance)) {
			continue
		}
		selected = append(selected, *sibling.DeepCopy())
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return context.WithValue(ctx, observedPodDeletionKey{}, &podDeletionPlan{Key: deletionPlanKey(ms, scope, group, role, instance), Pods: selected})
}

func observedPodDeletion(ctx context.Context, key podDeletionPlanKey) *podDeletionPlan {
	plan, _ := ctx.Value(observedPodDeletionKey{}).(*podDeletionPlan)
	if plan != nil && plan.Key == key {
		return plan
	}
	return nil
}

// A partial POST can precede AddRole. After the ordinary intent check, register
// existence so the original deletion path can retire even an empty old scope.
// This never grants Initialized/Ready credit or installs an accepted plan.
func (c *ModelServingController) registerObservedDeletionScope(ctx context.Context, ms *api.ModelServing, scope, group, role, instance string) {
	if plan := observedPodDeletion(ctx, deletionPlanKey(ms, scope, group, role, instance)); plan != nil {
		for i := range plan.Pods {
			pod := &plan.Pods[i]
			c.store.AddServingGroupAndRole(utils.GetNamespaceName(ms), group, utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod), utils.GetRoleName(pod), utils.GetRoleID(pod))
		}
	}
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
	var pods []corev1.Pod
	if observed := observedPodDeletion(ctx, key); observed != nil {
		pods = observed.Pods
	} else {
		var err error
		pods, err = c.ownedDeletionPods(ctx, ms, selector)
		if err != nil {
			return nil, err
		}
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
	modelServingContextLog(ctx, ms).V(4).Info("Pod deletion scope selected", "scope", key.Scope, "servingGroup", key.Group, "role", key.Role, "roleID", key.Instance, "selectedPods", len(pods), "outcome", "selected")
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
	logger := modelServingContextLog(ctx, ms).WithValues("scope", plan.Key.Scope, "servingGroup", plan.Key.Group, "role", plan.Key.Role, "roleID", plan.Key.Instance)
	accepted := 0
	defer func() {
		if accepted > 0 {
			logger.Info("Pod deletion requests accepted", "acceptedPods", accepted, "selectedPods", len(plan.Pods), "outcome", "requestAccepted")
		}
	}()
	if !plan.Started.Load() {
		if err := c.checkRolloutIntent(ctx, ms); err != nil {
			c.deletionPlans.Delete(plan.Key)
			return err
		}
	}
	if err := c.markPodDeletionPlan(ctx, ms, plan); err != nil {
		return err
	}
	for i := range plan.Pods {
		original := &plan.Pods[i]
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			logger.V(4).Info("Selected Pod is absent", "pod", klog.KObj(original), "podUID", original.UID, "outcome", "observedAbsent")
			plan.Started.Store(true)
			continue
		}
		if err != nil {
			return err
		}
		if pod.UID != original.UID || !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			logger.V(4).Info("Selected Pod deletion skipped", "pod", klog.KObj(pod), "podUID", original.UID, "currentPodUID", pod.UID, "reason", "IdentityChanged")
			continue
		}
		if pod.DeletionTimestamp != nil {
			logger.V(4).Info("Selected Pod is still terminating", "pod", klog.KObj(pod), "podUID", pod.UID, "outcome", "waiting")
			plan.Started.Store(true)
			continue
		}
		invalidateRolloutPodSnapshot(ctx)
		err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, *metav1.NewPreconditionDeleteOptions(string(original.UID)))
		if err == nil || apierrors.IsNotFound(err) {
			if err == nil {
				accepted++
				logger.V(4).Info("Pod deletion accepted", "pod", klog.KObj(pod), "podUID", pod.UID, "outcome", "requestAccepted")
			} else {
				logger.V(4).Info("Selected Pod is absent", "pod", klog.KObj(pod), "podUID", pod.UID, "outcome", "observedAbsent")
			}
			plan.Started.Store(true)
			continue
		}
		// A transport/server failure may have lost a successful DELETE response.
		// Keep the process-local plan active and retry only its original UIDs.
		if !definiteDeleteRejection(err) {
			plan.Started.Store(true)
		}
		return fmt.Errorf("delete selected Pod %s/%s uid=%s scope=%s: %w", pod.Namespace, pod.Name, original.UID, plan.Key.Scope, err)
	}
	return nil
}

// Mark the entire selected set before sending DELETE. The marker both fences
// delayed normal-deletion callbacks and allows a new process to finish these
// same Pod identities without retaining a separate transaction object.
func (c *ModelServingController) markPodDeletionPlan(ctx context.Context, ms *api.ModelServing, plan *podDeletionPlan) error {
	marked := 0
	defer func() {
		if marked > 0 {
			modelServingContextLog(ctx, ms).Info("Pod deletion scope marked", "scope", plan.Key.Scope, "servingGroup", plan.Key.Group, "role", plan.Key.Role, "roleID", plan.Key.Instance, "markedPods", marked, "selectedPods", len(plan.Pods), "outcome", "markersUpdated")
		}
	}()
	for i := range plan.Pods {
		original := &plan.Pods[i]
		pod, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, original.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if pod.UID != original.UID || !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		if pod.Annotations[deletionScopeAnnotation] == plan.Key.Scope || pod.Annotations[deletionScopeAnnotation] == deleteGroupScope {
			continue
		}
		patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
			"uid": pod.UID, "resourceVersion": pod.ResourceVersion,
			"annotations": map[string]string{deletionScopeAnnotation: plan.Key.Scope},
		}})
		if err != nil {
			return err
		}
		_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("mark selected Pod %s/%s uid=%s scope=%s: %w", pod.Namespace, pod.Name, pod.UID, plan.Key.Scope, err)
		}
		if err == nil {
			marked++
		}
	}
	return nil
}

// Restore only marked, current-owner Pod UIDs. Unmarked replacements and
// unrelated members never become part of an old deletion after a restart.
func (c *ModelServingController) restoreDeletionIntents(ctx context.Context, ms *api.ModelServing) error {
	pods, err := c.ownedDeletionPods(ctx, ms, labels.Set{api.ModelServingNameLabelKey: ms.Name}.AsSelector())
	if err != nil {
		return err
	}
	plans := map[podDeletionPlanKey]*podDeletionPlan{}
	groups := map[string]bool{}
	for i := range pods {
		pod := &pods[i]
		scope := pod.Annotations[deletionScopeAnnotation]
		if scope != deleteGroupScope && scope != deleteRoleScope {
			continue
		}
		group, role, instance := pod.Labels[api.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
		if group == "" || role == "" || instance == "" {
			return fmt.Errorf("marked Pod %s has incomplete deletion identity", pod.Name)
		}
		key := deletionPlanKey(ms, scope, group, role, instance)
		plan := plans[key]
		if plan == nil {
			plan = &podDeletionPlan{Key: key}
			plan.Started.Store(true)
			plans[key] = plan
		}
		plan.Pods = append(plan.Pods, *pod)
		groups[group] = groups[group] || scope == deleteGroupScope
	}
	owner := utils.GetNamespaceName(ms)
	for key, plan := range plans {
		// A whole-group marker supersedes any narrower unfinished Role plan.
		if key.Scope == deleteRoleScope && groups[key.Group] {
			continue
		}
		if _, exists := c.deletionPlans.Load(key); exists {
			continue
		}
		sort.Slice(plan.Pods, func(i, j int) bool { return plan.Pods[i].Name < plan.Pods[j].Name })
		for i := range plan.Pods {
			pod := &plan.Pods[i]
			c.store.AddServingGroupAndRole(owner, key.Group, utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod), utils.GetRoleName(pod), utils.GetRoleID(pod))
		}
		if key.Scope == deleteGroupScope {
			if err := c.store.UpdateServingGroupStatus(owner, key.Group, datastore.ServingGroupDeleting); err != nil {
				return err
			}
		} else if err := c.store.UpdateRoleStatus(owner, key.Group, key.Role, key.Instance, datastore.RoleDeleting); err != nil {
			return err
		}
		c.deletionPlans.Store(key, plan)
		modelServingContextLog(ctx, ms).Info("Marked Pod deletion scope restored", "scope", key.Scope, "servingGroup", key.Group, "role", key.Role, "roleID", key.Instance, "markedPods", len(plan.Pods), "reason", "ExistingDeletionMarkers", "outcome", "restored")
	}
	return nil
}

func (c *ModelServingController) clearDeletionPlan(ms *api.ModelServing, scope, group, role, instance string) {
	c.deletionPlans.Delete(deletionPlanKey(ms, scope, group, role, instance))
	if scope == deleteGroupScope {
		role, instance = "", ""
	}
	c.forgetKnownMemberScope(ms, group, role, instance)
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
		modelServingContextLog(ctx, ms).V(4).Info("Waiting for original Pods while preserving replacements", "scope", plan.Key.Scope, "servingGroup", plan.Key.Group, "role", plan.Key.Role, "roleID", plan.Key.Instance)
		return true, nil
	}
	c.deletionPlans.Delete(plan.Key)
	c.forgetKnownMemberScope(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	key := utils.GetNamespaceName(ms)
	if plan.Key.Scope == deleteGroupScope {
		c.store.DeleteServingGroup(key, plan.Key.Group)
	} else {
		c.store.DeleteRole(key, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	}
	for i := range pods {
		pod := &pods[i]
		c.observeKnownMember(ms, pod)
		c.store.AddServingGroupAndRole(key, plan.Key.Group, utils.ObjectRevision(pod), utils.ObjectRoleTemplateHash(pod), utils.GetRoleName(pod), utils.GetRoleID(pod))
	}
	c.resetDeletionStatus(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	modelServingContextLog(ctx, ms).Info("Original Pod deletion observed with replacements retained", "scope", plan.Key.Scope, "servingGroup", plan.Key.Group, "role", plan.Key.Role, "roleID", plan.Key.Instance, "retainedPods", len(pods), "outcome", "observedAbsent")
	return true, nil
}

// Single-Pod recovery and conflict cleanup must not expand the UID set of an
// overlapping plan. A Role plan does not fence other instances in its group.
func (c *ModelServingController) podDeletionPlanCoversPod(ms *api.ModelServing, pod *corev1.Pod) bool {
	group := pod.Labels[api.GroupNameLabelKey]
	if _, active := c.deletionPlans.Load(deletionPlanKey(ms, deleteGroupScope, group, "", "")); active {
		return true
	}
	_, active := c.deletionPlans.Load(deletionPlanKey(ms, deleteRoleScope, group, utils.GetRoleName(pod), utils.GetRoleID(pod)))
	return active
}

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

// Called under the ModelServing lifecycle lock. An absent/terminating owner
// retains no plans; a recreated owner retains only plans for its current UID.
func (c *ModelServingController) prunePodDeletionPlans(owner types.NamespacedName, currentUID types.UID) {
	c.forgetKnownMemberOwner(owner, currentUID)
	c.deletionPlans.Range(func(key, _ interface{}) bool {
		planKey := key.(podDeletionPlanKey)
		if planKey.Owner == owner && (currentUID == "" || planKey.UID != currentUID) {
			c.deletionPlans.Delete(key)
		}
		return true
	})
}

// A Role record can disappear before its process-local deletion plan, for
// example when the enclosing empty group is forgotten or replaced. Finish
// only the original UIDs and cleanup; absence from the store is not proof of
// physical deletion and must not authorize deleting a same-name replacement.
func (c *ModelServingController) resumeOrphanedRoleDeletion(ctx context.Context, ms *api.ModelServing, plan *podDeletionPlan) error {
	if err := c.checkRolloutIntent(ctx, ms); err != nil {
		return err
	}
	if err := c.deletePlannedPods(ctx, ms, plan); err != nil {
		return err
	}
	if handled, err := c.finishDeletionWithReplacement(ctx, ms, plan); err != nil {
		return err
	} else if handled {
		if _, pending := c.deletionPlans.Load(plan.Key); pending {
			return fmt.Errorf("waiting for original Role UIDs %s/%s/%s", plan.Key.Group, plan.Key.Role, plan.Key.Instance)
		}
		c.clearRoleDeletionProgress(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
		return nil
	}
	selector := labels.Set{api.GroupNameLabelKey: plan.Key.Group, api.RoleLabelKey: plan.Key.Role, api.RoleIDKey: plan.Key.Instance}
	pods, err := c.ownedDeletionPods(ctx, ms, selector.AsSelector())
	if err != nil {
		return err
	}
	if len(pods) > 0 {
		c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
		return fmt.Errorf("waiting for orphaned Role deletion %s/%s/%s", plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	}
	if err := c.runRoleDeletePlugins(ctx, ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance); err != nil {
		return err
	}
	deleted, err := c.isRoleDeletedLive(ctx, ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	if err != nil {
		return err
	}
	if !deleted {
		c.enqueueModelServingAfter(ms, roleDeletionRecheckDelay)
		return fmt.Errorf("waiting for orphaned Role cleanup %s/%s/%s", plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	}
	c.deletionPlans.Delete(plan.Key)
	c.forgetKnownMemberScope(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	c.clearRoleDeletionProgress(ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
	modelServingContextLog(ctx, ms).Info("Orphaned Role deletion observed", "scope", plan.Key.Scope, "servingGroup", plan.Key.Group, "role", plan.Key.Role, "roleID", plan.Key.Instance, "outcome", "observedAbsent")
	c.enqueueModelServing(ms)
	return nil
}

// Retry the original UID set, including plans restored from marked survivors.
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
		} else if c.store.GetRoleStatus(utils.GetNamespaceName(ms), plan.Key.Group, plan.Key.Role, plan.Key.Instance) == datastore.RoleNotFound {
			err = c.resumeOrphanedRoleDeletion(ctx, ms, plan)
		} else {
			err = c.DeleteRole(ctx, ms, plan.Key.Group, plan.Key.Role, plan.Key.Instance)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
