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
	"reflect"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// reconcileObservation calibrates membership while preserving operation intent.
// Its state belongs to the same namespace/name workqueue key as normal rollout.
func (c *ModelServingController) reconcileObservation(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	state := c.servingState
	pods, err := c.podsLister.Pods(ms.Namespace).List(labels.Everything())
	if err != nil {
		return err
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	current := make(map[string]*corev1.Pod, len(pods))
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		group, role, id := pod.Labels[workloadv1alpha1.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
		if group == "" || role == "" || id == "" {
			continue
		}
		current[pod.Name] = pod
		c.store.AddServingGroupAndRole(utils.GetNamespaceName(ms), group, utils.ObjectRevision(pod), c.resolveRoleTemplateHash(ms, role, pod), role, id)
	}
	// A missed Delete/UID-replacement notification is derived from identity,
	// not from a synthetic replay of every informer event.
	for name, previous := range state.pods {
		if pod := current[name]; pod == nil || pod.UID != previous.pod.UID {
			state.deleted[previous.pod.UID] = previous.pod
		}
	}
	for uid, pod := range state.deleted {
		if _, done := state.completedDeletes[uid]; done {
			delete(state.deleted, uid)
			continue
		}
		if !c.observation.live {
			state.live = true
			return fmt.Errorf("confirm missing Pod %s/%s from API server", ms.Namespace, pod.Name)
		}
		if live := current[pod.Name]; live != nil && live.UID == uid {
			// The cache may still show a tombstoned object. Do not act on a
			// negative observation until an authoritative read confirms it.
			state.live = true
			c.enqueueModelServingAfter(ms, enqueueAfter)
			continue
		}
		if err := c.runObservedPodDelete(ctx, ms, pod); err != nil {
			return err
		}
		state.completedDeletes[uid] = time.Now()
		delete(state.deleted, uid)
		if previous := state.pods[pod.Name]; previous != nil && previous.pod.UID == uid {
			delete(state.pods, pod.Name)
		}
		delete(state.grace, uid)
	}
	for uid, finished := range state.completedDeletes {
		if time.Since(finished) > 10*time.Minute {
			delete(state.completedDeletes, uid)
		}
	}
	// This happens before readiness is downgraded, and HasRun persists after a
	// downgrade. An initial incomplete Role is not a failed running Role.
	if err := c.recoverMissingObservedRoles(ctx, ms); err != nil {
		return err
	}
	readyByGroup := make(map[string][]string)
	var hookErr error
	for _, pod := range pods {
		if current[pod.Name] != pod {
			continue
		}
		group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group) == datastore.ServingGroupDeleting ||
			c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, utils.GetRoleName(pod), utils.GetRoleID(pod)) == datastore.RoleDeleting {
			continue
		}
		progress := state.pods[pod.Name]
		if progress == nil || progress.pod.UID != pod.UID {
			progress = &podHookProgress{}
			state.pods[pod.Name] = progress
		}
		progress.pod = pod.DeepCopy()
		if pod.DeletionTimestamp != nil {
			progress.ready = false
			continue
		}
		if pod.Status.Phase == corev1.PodRunning && progress.runningVersion != pod.ResourceVersion+"/"+string(pod.UID) {
			if err := c.handleRunningPod(ms, group, pod); err != nil {
				progress.ready = false
				hookErr = err
				continue
			}
			progress.runningVersion = pod.ResourceVersion + "/" + string(pod.UID)
		}
		if utils.IsPodRunningAndReady(pod) {
			delete(state.grace, pod.UID)
			if !progress.ready {
				if err := c.runObservedPodReady(ctx, ms, pod); err != nil {
					hookErr = err
					continue
				}
				progress.ready = true
			}
			readyByGroup[group] = append(readyByGroup[group], pod.Name)
		} else {
			progress.ready = false
			if utils.IsPodFailed(pod) || utils.ContainerRestarted(pod) {
				if err := c.reconcileObservedUnhealthyPod(ctx, ms, pod); err != nil {
					return err
				}
			}
		}
	}
	c.store.CalibrateReadyPods(utils.GetNamespaceName(ms), readyByGroup)
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err != nil && err != datastore.ErrServingGroupNotFound {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for id, role := range instances {
				if role.Status == datastore.RoleDeleting {
					continue
				}
				ready, err := c.observedRoleReady(ctx, ms, group.Name, name, *role, state)
				if err != nil {
					return err
				}
				status := datastore.RoleCreating
				if ready {
					status = datastore.RoleRunning
				}
				if err := c.store.UpdateRoleStatus(utils.GetNamespaceName(ms), group.Name, name, id, status); err != nil {
					return err
				}
			}
		}
		ready, err := c.checkServingGroupReady(ms, group.Name)
		if err != nil {
			return err
		}
		status := datastore.ServingGroupCreating
		if ready {
			status = datastore.ServingGroupRunning
		}
		if err := c.store.UpdateServingGroupStatus(utils.GetNamespaceName(ms), group.Name, status); err != nil {
			return err
		}
	}
	return hookErr
}

func (c *ModelServingController) observedRolePods(ctx context.Context, ms *workloadv1alpha1.ModelServing, group, name string, role datastore.Role) ([]*corev1.Pod, int, error) {
	spec, err := c.revisionHistory(ctx, ms).role(ctx, role.Revision, name)
	if err != nil {
		return nil, 0, err
	}
	expected := 1 + int(spec.WorkerReplicas)
	pods, err := c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", ms.Namespace, group, name, role.Name))
	if err != nil {
		return nil, 0, err
	}
	owned := make([]*corev1.Pod, 0, len(pods))
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			continue
		}
		for i := 0; i < expected; i++ {
			if pod.Name == utils.GeneratePodName(group, role.Name, i) {
				owned = append(owned, pod)
				break
			}
		}
	}
	return owned, expected, nil
}

func (c *ModelServingController) observedRoleReady(ctx context.Context, ms *workloadv1alpha1.ModelServing, group, name string, role datastore.Role, state *servingAuditState) (bool, error) {
	pods, expected, err := c.observedRolePods(ctx, ms, group, name, role)
	if err != nil {
		return false, err
	}
	if len(pods) != expected {
		return false, nil
	}
	for _, pod := range pods {
		progress := state.pods[pod.Name]
		if pod.DeletionTimestamp != nil || !utils.IsPodRunningAndReady(pod) || progress == nil || !progress.ready || progress.pod.UID != pod.UID {
			return false, nil
		}
	}
	return true, nil
}

func (c *ModelServingController) recoverMissingObservedRoles(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	groups, err := c.store.GetServingGroupByModelServing(utils.GetNamespaceName(ms))
	if err == datastore.ErrServingGroupNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	for _, group := range groups {
		if group.Status == datastore.ServingGroupDeleting {
			continue
		}
		roles, err := c.store.GetRolesByGroup(utils.GetNamespaceName(ms), group.Name)
		if err != nil {
			return err
		}
		for name, instances := range roles {
			for _, role := range instances {
				if role.Status == datastore.RoleDeleting || !role.HasRun {
					continue
				}
				pods, expected, err := c.observedRolePods(ctx, ms, group.Name, name, *role)
				if err != nil {
					return err
				}
				if len(pods) == expected {
					continue
				}
				// Absence in a lagging informer must not delete healthy peers.
				if !c.observation.live {
					c.servingState.live = true
					c.enqueueModelServingAfter(ms, enqueueAfter)
					return fmt.Errorf("confirm incomplete role %s/%s from API server", group.Name, role.Name)
				}
				switch ms.Spec.RecoveryPolicy {
				case workloadv1alpha1.RoleRecreate:
					if err := c.DeleteRole(ctx, ms, group.Name, name, role.Name); err != nil {
						return err
					}
				case workloadv1alpha1.ServingGroupRecreate:
					if err := c.deleteServingGroup(ctx, ms, group.Name); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func (c *ModelServingController) podHookRequest(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) *plugins.HookRequest {
	return &plugins.HookRequest{ModelServing: ms, Pod: pod, ServingGroup: pod.Labels[workloadv1alpha1.GroupNameLabelKey],
		RoleName: utils.GetRoleName(pod), RoleID: utils.GetRoleID(pod), IsEntry: pod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry,
		PodLister: c.podsLister, ConfigMapLister: c.configMapsLister, ServiceLister: c.servicesLister, KubeClient: c.kubeClientSet}
}

func (c *ModelServingController) runObservedPodReady(ctx context.Context, ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) error {
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return err
	}
	if chain != nil {
		return chain.OnPodReady(ctx, c.podHookRequest(ms, pod))
	}
	return nil
}

func (c *ModelServingController) runObservedPodDelete(ctx context.Context, ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) error {
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return err
	}
	if chain != nil {
		if err := chain.OnPodDelete(ctx, c.podHookRequest(ms, pod)); err != nil {
			return err
		}
	}
	group, name, id := pod.Labels[workloadv1alpha1.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
	if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group) == datastore.ServingGroupDeleting || c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, name, id) == datastore.RoleDeleting {
		return nil
	}
	// Ignore an old tombstone arriving after its replacement was already
	// adopted. Previously tracked identity changes are handled exactly once.
	previous := c.servingState.pods[pod.Name]
	if previous == nil || previous.pod.UID != pod.UID {
		return nil
	}
	roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), group, name)
	if err != nil {
		return nil
	}
	wasRunning := false
	for _, role := range roles {
		if role.Name == id {
			wasRunning = role.HasRun
		}
	}
	if !wasRunning {
		return nil
	}
	return c.handleDeletedPod(ms, group, pod)
}

func (c *ModelServingController) reconcileObservedUnhealthyPod(ctx context.Context, ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) error {
	if utils.IsPodRunningAndReady(pod) || pod.DeletionTimestamp != nil {
		return nil
	}
	if ms.Spec.RecoveryPolicy == workloadv1alpha1.NoneRestartPolicy && !utils.IsPodFailed(pod) {
		return nil
	}
	deadline, exists := c.servingState.grace[pod.UID]
	if !exists {
		deadline = time.Now()
		if seconds := ms.Spec.Template.RestartGracePeriodSeconds; seconds != nil && *seconds > 0 {
			deadline = deadline.Add(time.Duration(*seconds) * time.Second)
		}
		c.servingState.grace[pod.UID] = deadline
	}
	if remaining := time.Until(deadline); remaining > 0 {
		c.enqueueModelServingAfter(ms, remaining)
		return nil
	}
	latest, err := c.kubeClientSet.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		delete(c.servingState.grace, pod.UID)
		c.servingState.live = true
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return nil
	}
	if err != nil {
		return err
	}
	if latest.UID != pod.UID || !utils.IsOwnedByModelServingWithUID(latest, ms.UID) || utils.IsPodRunningAndReady(latest) {
		delete(c.servingState.grace, pod.UID)
		return nil
	}
	if err := c.validateCurrentModelServing(ctx, ms); err != nil {
		return err
	}
	if err := c.deleteConflictingPod(ctx, latest); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	delete(c.servingState.grace, pod.UID)
	c.servingState.live = true
	c.enqueueModelServingAfter(ms, enqueueAfter)
	return nil
}

func (c *ModelServingController) validateCurrentModelServing(ctx context.Context, ms *workloadv1alpha1.ModelServing) error {
	latest, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if latest.UID != ms.UID || !reflect.DeepEqual(latest.Spec, ms.Spec) {
		return fmt.Errorf("ModelServing %s/%s changed during reconciliation", ms.Namespace, ms.Name)
	}
	return nil
}
