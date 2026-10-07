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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

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

// A restart may reconstruct a Creating placeholder while the old Pod still
// terminates at the same ordinal. Its reservation is physical evidence and must
// not depend solely on the transient datastore Deleting status.
func (c *ModelServingController) rolloutDeletionPending(ms *workloadv1alpha1.ModelServing, group, role, instance string) bool {
	if c.podsInformer == nil {
		return false
	}
	index, key := GroupNameKey, fmt.Sprintf("%s/%s", ms.Namespace, group)
	if role != "" {
		index, key = RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, group, role, instance)
	}
	pods, err := c.getPodsByIndex(index, key)
	if err != nil {
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true
	}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil && utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			return true
		}
	}
	return false
}

// Share authoritative observations within a reconcile until a Pod write. A
// same-round refill must not inherit the retired Pod's version or Ready state.
// This never writes back into informer-owned storage.
type rolloutPodSnapshotKey struct{}
type rolloutPodSnapshot struct {
	ownerUID types.UID
	roles    map[string][]*corev1.Pod
	dirty    bool
}

func invalidateRolloutPodSnapshot(ctx context.Context) {
	if snapshot, ok := ctx.Value(rolloutPodSnapshotKey{}).(*rolloutPodSnapshot); ok {
		snapshot.dirty = true
	}
}

func (c *ModelServingController) withRolloutPodSnapshot(ctx context.Context, ms *workloadv1alpha1.ModelServing) (context.Context, error) {
	if ms.ResourceVersion == "" {
		return ctx, nil
	}
	snapshot, ok := ctx.Value(rolloutPodSnapshotKey{}).(*rolloutPodSnapshot)
	if ok && snapshot.ownerUID == ms.UID && !snapshot.dirty {
		return ctx, nil
	}
	live, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name}.String()})
	if err != nil {
		return ctx, err
	}
	if !ok || snapshot.ownerUID != ms.UID {
		snapshot = &rolloutPodSnapshot{ownerUID: ms.UID}
		ctx = context.WithValue(ctx, rolloutPodSnapshotKey{}, snapshot)
	}
	snapshot.roles = map[string][]*corev1.Pod{}
	for i := range live.Items {
		pod := &live.Items[i]
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		key := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, pod.Labels[workloadv1alpha1.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod))
		snapshot.roles[key] = append(snapshot.roles[key], pod)
	}
	snapshot.dirty = false
	return ctx, nil
}

func (c *ModelServingController) podsForRoleObservation(ctx context.Context, ms *workloadv1alpha1.ModelServing, group, role, instance string) ([]*corev1.Pod, error) {
	key := fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, group, role, instance)
	if snapshot, ok := ctx.Value(rolloutPodSnapshotKey{}).(*rolloutPodSnapshot); ok && snapshot.ownerUID == ms.UID {
		if snapshot.dirty {
			if _, err := c.withRolloutPodSnapshot(ctx, ms); err != nil {
				return nil, err
			}
		}
		return snapshot.roles[key], nil
	}
	if ms.ResourceVersion == "" {
		return c.getPodsByIndex(RoleIDKey, key)
	}
	live, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workloadv1alpha1.GroupNameLabelKey: group, workloadv1alpha1.RoleLabelKey: role, workloadv1alpha1.RoleIDKey: instance}.String()})
	if err != nil {
		return nil, err
	}
	result := make([]*corev1.Pod, 0, len(live.Items))
	for i := range live.Items {
		if utils.IsOwnedByModelServingWithUID(&live.Items[i], ms.UID) {
			result = append(result, &live.Items[i])
		}
	}
	return result, nil
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
	ctx, err = c.withRolloutPodSnapshot(ctx, ms)
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
