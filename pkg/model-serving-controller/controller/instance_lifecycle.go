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
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	groupInstanceAnnotation = "workload.kthena.io/group-instance"
	roleInstanceAnnotation  = "workload.kthena.io/role-instance"
	roleCreatedAnnotation   = "workload.kthena.io/role-created"
)

// Informer callbacks and workqueue reconciles both change lifecycle state. The
// queue alone does not serialize them. Locks are per key, and are released from
// the map when the last waiter leaves; unrelated ModelServings never share a lock.
type lifecycleLocks struct {
	mu      sync.Mutex
	entries map[string]*lifecycleLock
}

type lifecycleLock struct {
	mu    sync.Mutex
	users int
}

func (l *lifecycleLocks) lock(key string) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*lifecycleLock)
	}
	e := l.entries[key]
	if e == nil {
		e = &lifecycleLock{}
		l.entries[key] = e
	}
	e.users++
	l.mu.Unlock()
	e.mu.Lock()
	return func() {
		e.mu.Unlock()
		l.mu.Lock()
		defer l.mu.Unlock()
		e.users--
		if e.users == 0 {
			delete(l.entries, key)
		}
	}
}

// currentPodEvent discards a queued event for a previous UID and uses the latest
// observation for the same UID (in particular, Ready must not revive Deleting).
func (c *ModelServingController) currentPodEvent(pod *corev1.Pod) *corev1.Pod {
	if pod.UID == "" {
		return pod
	} // Synthetic objects without an API identity.
	latest, err := c.podsLister.Pods(pod.Namespace).Get(pod.Name)
	// A stale Watch frame can overwrite the informer store before this callback.
	// API objects always carry resourceVersion; synthetic fixtures may not.
	if pod.ResourceVersion != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		latest, err = c.kubeClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	}
	if err != nil || latest.UID != pod.UID {
		c.enqueueModelServingByChildResourceAfter(pod, enqueueAfter)
		return nil
	}
	return latest
}

func (c *ModelServingController) stalePodDeletion(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	latest, err := c.podsLister.Pods(pod.Namespace).Get(pod.Name)
	if ms.ResourceVersion != "" {
		latest, err = c.kubeClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		// An object still present in the API is not an observed physical loss,
		// irrespective of whether the replayed frame carries its current UID.
		if err == nil {
			return true
		}
	}
	if err == nil && latest.UID != pod.UID {
		return true
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return true
	}
	// A removed worker ordinal may have no same-name replacement. Its surviving
	// siblings still identify whether the Role/SG has already been replaced.
	selector := labels.Set{workloadv1alpha1.GroupNameLabelKey: pod.Labels[workloadv1alpha1.GroupNameLabelKey]}
	pods, err := c.podsLister.Pods(pod.Namespace).List(selector.AsSelector())
	if ms.ResourceVersion != "" {
		var live *corev1.PodList
		live, err = c.kubeClientSet.CoreV1().Pods(pod.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
		pods = nil
		if err == nil {
			for i := range live.Items {
				pods = append(pods, &live.Items[i])
			}
		}
	}
	if err != nil {
		return true
	}
	for _, sibling := range pods {
		if !utils.IsOwnedByModelServingWithUID(sibling, ms.UID) {
			continue
		}
		if sibling.Annotations[groupInstanceAnnotation] != pod.Annotations[groupInstanceAnnotation] {
			return true
		}
		// RecoveryPolicy and rollout mode are mutable. The event must still
		// belong to the current Role incarnation before its failure can be
		// expanded to a whole-group recovery under the latest policy.
		if utils.GetRoleName(sibling) == utils.GetRoleName(pod) && utils.GetRoleID(sibling) == utils.GetRoleID(pod) &&
			sibling.Annotations[roleInstanceAnnotation] != pod.Annotations[roleInstanceAnnotation] {
			return true
		}
	}
	return false
}

// instanceAnnotations survives controller restarts and partial Pod creation.
// Existing legacy members remain in their untagged cohort until replaced.
func (c *ModelServingController) instanceAnnotations(ctx context.Context, ms *workloadv1alpha1.ModelServing, group, role, instance string) (map[string]string, error) {
	list, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: labels.Set{workloadv1alpha1.GroupNameLabelKey: group}.AsSelector().String(),
	})
	if err != nil {
		return nil, err
	}
	groupID, roleID := string(uuid.NewUUID()), string(uuid.NewUUID())
	groupFound, roleFound := false, false
	for i := range list.Items {
		pod := &list.Items[i]
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		if !groupFound {
			groupID = pod.Annotations[groupInstanceAnnotation]
			groupFound = true
		}
		if utils.GetRoleName(pod) != role || utils.GetRoleID(pod) != instance {
			continue
		}
		if pod.DeletionTimestamp != nil {
			return nil, fmt.Errorf("Role %s/%s still has retiring Pod UID %s", group, instance, pod.UID)
		}
		if !roleFound {
			roleID = pod.Annotations[roleInstanceAnnotation]
			roleFound = true
		}
	}
	return map[string]string{groupInstanceAnnotation: groupID, roleInstanceAnnotation: roleID}, nil
}

func setInstanceAnnotations(pod *corev1.Pod, instance map[string]string) {
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	for key, value := range instance {
		pod.Annotations[key] = value
	}
	// Templates and plugins cannot supply lifecycle evidence for a new Pod.
	delete(pod.Annotations, roleCreatedAnnotation)
}

// Record that all members were created, independently of whether they are
// Ready. Surviving members retain this evidence across a controller restart;
// an unfinished initial creation must still be completed without recovery churn.
func (c *ModelServingController) markRoleCreated(ctx context.Context, ms *workloadv1alpha1.ModelServing, group, role, instance string) error {
	if ms.ResourceVersion == "" {
		return nil
	}
	pods, err := c.podsForRoleObservation(ctx, ms, group, role, instance)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return fmt.Errorf("cannot record creation of absent Role %s/%s", group, instance)
	}
	if err := c.persistRoleCreated(ctx, ms, group, role, instance, pods); err != nil {
		return err
	}
	var failures []error
	for _, pod := range pods {
		if pod.Annotations[roleCreatedAnnotation] == "true" {
			continue
		}
		if pod.DeletionTimestamp != nil {
			failures = append(failures, fmt.Errorf("Role %s/%s has retiring member %s", group, instance, pod.Name))
			continue
		}
		patch, err := json.Marshal(map[string]interface{}{"metadata": map[string]interface{}{
			"uid": pod.UID, "resourceVersion": pod.ResourceVersion,
			"annotations": map[string]string{roleCreatedAnnotation: "true"},
		}})
		if err != nil {
			return err
		}
		invalidateRolloutPodSnapshot(ctx)
		_, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func roleCreationObserved(role datastore.Role, pods []*corev1.Pod) bool {
	if role.Initialized || role.Status == datastore.RoleRunning {
		return true
	}
	for _, pod := range pods {
		if pod.Annotations[roleCreatedAnnotation] == "true" {
			return true
		}
	}
	return false
}
