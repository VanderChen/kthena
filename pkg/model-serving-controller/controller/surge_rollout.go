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
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Persist the origin of temporary capacity on Pods so restart and recovery do
// not turn surge replicas into permanent high ordinals. This is not part of the
// user template or its revision hash. Unmarked binpack survivors remain stable.
const (
	surgeAnnotation   = "modelserving.volcano.sh/surge"
	surgeServingGroup = "serving-group"
	surgeRole         = "role"
)

func setSurgeScope(pod *corev1.Pod, scope string) {
	if scope == "" {
		delete(pod.Annotations, surgeAnnotation)
		return
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[surgeAnnotation] = scope
}

func inheritedSurgeScope(pods []*corev1.Pod) string {
	for _, pod := range pods {
		if scope := pod.Annotations[surgeAnnotation]; scope == surgeServingGroup || scope == surgeRole {
			return scope
		}
	}
	return ""
}

func (c *ModelServingController) surgePods(ms *workloadv1alpha1.ModelServing, scope, groupName, roleName string) ([]*corev1.Pod, error) {
	selector := labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name}
	if groupName != "" {
		selector[workloadv1alpha1.GroupNameLabelKey] = groupName
	}
	if roleName != "" {
		selector[workloadv1alpha1.RoleLabelKey] = roleName
	}
	var pods []*corev1.Pod
	var err error
	if groupName != "" {
		pods, err = c.getPodsByIndex(GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, groupName))
	} else {
		pods, err = c.podsLister.Pods(ms.Namespace).List(selector.AsSelector())
	}
	if err != nil {
		return nil, err
	}
	result := make([]*corev1.Pod, 0)
	for _, pod := range pods {
		if selector.AsSelector().Matches(labels.Set(pod.Labels)) && utils.IsOwnedByModelServingWithUID(pod, ms.UID) && pod.Annotations[surgeAnnotation] == scope {
			result = append(result, pod)
		}
	}
	return result, nil
}

func surgeIdentity(pod *corev1.Pod, scope string) (string, int) {
	name := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
	if scope == surgeRole {
		name = pod.Labels[workloadv1alpha1.RoleIDKey]
	}
	_, ordinal := utils.GetParentNameAndOrdinal(name)
	return name, ordinal
}

// Expansion may adopt a temporary instance into the desired ordinal range.
// Remove its marker without replacing it, including on every worker Pod, so a
// later binpack scale-down does not mistake the survivor for temporary capacity.
func (c *ModelServingController) adoptSurgeReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, scope, groupName, roleName string, replicas int) error {
	pods, err := c.surgePods(ms, scope, groupName, roleName)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		_, ordinal := surgeIdentity(pod, scope)
		if ordinal < 0 || ordinal >= replicas || pod.DeletionTimestamp != nil {
			continue
		}
		patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
			"uid": pod.UID, "resourceVersion": pod.ResourceVersion,
			"annotations": map[string]interface{}{surgeAnnotation: nil},
		}})
		if err != nil {
			return err
		}
		if _, err := c.kubeClientSet.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("adopt surge pod %s/%s: %w", pod.Namespace, pod.Name, err)
		}
	}
	return nil
}

func markedSurgeNames(pods []*corev1.Pod, scope string, replicas int) sets.Set[string] {
	names := sets.New[string]()
	for _, pod := range pods {
		name, ordinal := surgeIdentity(pod, scope)
		if ordinal >= replicas {
			names.Insert(name)
		}
	}
	return names
}

// Low holes may hold stable capacity only when capacity is missing or an
// outdated high identity will be retired. Otherwise surge must stay outside
// the desired range: filling a low hole with it would displace an existing
// identity when the last in-range old replica is deleted (SG-S03).
func forEachRolloutOrdinal(replicas, stableSlots int, existing []int, limit int, handler func(int) bool) {
	occupied := sets.New(existing...)
	for ordinal := 0; ordinal < replicas && stableSlots > 0 && limit > 0; ordinal++ {
		if occupied.Has(ordinal) {
			continue
		}
		if !handler(ordinal) {
			return
		}
		stableSlots--
		limit--
	}
	for ordinal := replicas; limit > 0; ordinal++ {
		if occupied.Has(ordinal) {
			continue
		}
		if !handler(ordinal) {
			return
		}
		limit--
	}
}

type surgeReplica struct {
	name      string
	ready     bool
	deleting  bool
	temporary bool
	protected bool
}

// planSurgeCompletion replaces only marked temporary instances. Existing high
// ordinals without a marker count towards the desired stable pool. Deleting
// instances still occupy capacity; Ready instances alone provide availability.
// Callers supply replicas in ascending ordinal order.
func planSurgeCompletion(replicas, maxSurge, maxUnavailable int, instances []surgeReplica) (int, []string) {
	stable, available := 0, 0
	for _, instance := range instances {
		if !instance.temporary || instance.protected {
			stable++
		}
		if instance.ready && !instance.deleting {
			available++
		}
	}
	missing := max(0, replicas-stable)
	toCreate := min(missing, max(0, replicas+maxSurge-len(instances)))
	if toCreate > 0 {
		return len(instances) + toCreate, nil
	}
	if maxSurge == 0 && maxUnavailable == 0 {
		return len(instances), nil
	}
	budget := max(0, available-max(0, replicas-maxUnavailable))
	var toDelete []string
	// Prefer unavailable temporary replicas, then highest ordinal, while keeping
	// the existing stable instances regardless of their deletion-cost settings.
	for _, ready := range []bool{false, true} {
		for _, instance := range slices.Backward(instances) {
			if !instance.temporary || instance.protected || instance.deleting || instance.ready != ready {
				continue
			}
			if ready {
				if budget == 0 {
					continue
				}
				budget--
			}
			toDelete = append(toDelete, instance.name)
		}
	}
	return len(instances), toDelete
}

func (c *ModelServingController) finishServingGroupSurge(ctx context.Context, ms *workloadv1alpha1.ModelServing, groups []datastore.ServingGroup, revision string, maxSurge, partition int) (bool, error) {
	pods, err := c.surgePods(ms, surgeServingGroup, "", "")
	if err != nil {
		return false, err
	}
	replicas := modelServingReplicas(ms)
	temporary := markedSurgeNames(pods, surgeServingGroup, replicas)
	if temporary.Len() == 0 {
		return false, nil
	}
	c.enqueueModelServingAfter(ms, enqueueAfter)
	maxUnavailable, err := utils.GetMaxUnavailable(ms)
	if err != nil {
		return true, err
	}
	instances := make([]surgeReplica, 0, len(groups))
	for _, group := range groups {
		_, ordinal := utils.GetParentNameAndOrdinal(group.Name)
		instances = append(instances, surgeReplica{
			name: group.Name, ready: group.Status == datastore.ServingGroupRunning,
			deleting:  group.Status == datastore.ServingGroupDeleting,
			temporary: temporary.Has(group.Name), protected: ordinal < partition,
		})
	}
	expected, toDelete := planSurgeCompletion(replicas, maxSurge, maxUnavailable, instances)
	if expected > len(groups) {
		return true, c.scaleUpServingGroups(ctx, ms, groups, expected, revision)
	}
	if c.hasUpdateableOutdatedServingGroup(ctx, ms, groups, revision, partition) {
		// A reduced budget may leave excess surge while old replicas remain.
		// Preserve the currently allowed surge for those remaining replacements.
		toDelete = toDelete[:min(len(toDelete), max(0, len(groups)-replicas-maxSurge))]
	}
	for _, name := range toDelete {
		if err := c.deleteServingGroup(ctx, ms, name); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (c *ModelServingController) finishRoleSurge(ctx context.Context, ms *workloadv1alpha1.ModelServing, groupName string, role workloadv1alpha1.Role, roles []datastore.Role, groupOrdinal int, revision string, allowTargetStart bool) (bool, error) {
	pods, err := c.surgePods(ms, surgeRole, groupName, role.Name)
	if err != nil {
		return false, err
	}
	replicas := roleReplicas(role)
	temporary := markedSurgeNames(pods, surgeRole, replicas)
	if temporary.Len() == 0 {
		return false, nil
	}
	c.enqueueModelServingAfter(ms, enqueueAfter)
	maxSurge, err := utils.GetMaxSurgeForRole(role)
	if err != nil {
		return true, err
	}
	maxUnavailable, configured, err := utils.GetMaxUnavailableForRole(role)
	if err != nil {
		return true, err
	}
	if !configured {
		maxUnavailable = replicas
	}
	partition, _, err := c.getPartition(rolePartition(ms, role), replicas)
	if err != nil {
		return true, err
	}
	instances := make([]surgeReplica, 0, len(roles))
	for _, instance := range roles {
		_, ordinal := utils.GetParentNameAndOrdinal(instance.Name)
		instances = append(instances, surgeReplica{
			name: instance.Name, ready: instance.Status == datastore.RoleRunning,
			deleting:  instance.Status == datastore.RoleDeleting,
			temporary: temporary.Has(instance.Name), protected: ordinal < partition,
		})
	}
	expected, toDelete := planSurgeCompletion(replicas, maxSurge, maxUnavailable, instances)
	if expected > len(roles) {
		return true, c.scaleUpRoles(ctx, ms, groupName, role, roles, expected, groupOrdinal, revision, allowTargetStart)
	}
	if c.hasUpdateableOutdatedRole(ctx, ms, groupName, role, roles) {
		toDelete = toDelete[:min(len(toDelete), max(0, len(roles)-replicas-maxSurge))]
	}
	for _, name := range toDelete {
		if err := c.DeleteRole(ctx, ms, groupName, role.Name, name); err != nil {
			return true, err
		}
	}
	return true, nil
}

func (c *ModelServingController) hasPendingSurge(ms *workloadv1alpha1.ModelServing) (bool, error) {
	scope := surgeServingGroup
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
		scope = surgeRole
	}
	pods, err := c.surgePods(ms, scope, "", "")
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		replicas := modelServingReplicas(ms)
		if scope == surgeRole {
			found := false
			for _, role := range ms.Spec.Template.Roles {
				if role.Name == pod.Labels[workloadv1alpha1.RoleLabelKey] {
					replicas, found = roleReplicas(role), true
					break
				}
			}
			if !found {
				continue
			}
		}
		_, ordinal := surgeIdentity(pod, scope)
		if ordinal >= replicas {
			return true, nil
		}
	}
	return false, nil
}
