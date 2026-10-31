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
	"sync"
	"time"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
)

// Publication never waits for lifecycle or performs an API call. Business
// consumers retain the existing lifecycle lock, including across slow hooks.
type podInputs struct {
	mu     sync.Mutex
	owners map[ownerInputKey]*ownerPodInputs
}

type ownerInputKey struct {
	types.NamespacedName
	UID types.UID
}

type podInputKey struct {
	Name string
	UID  types.UID
}

type deletedPodInput struct {
	pod           *corev1.Pod
	hookAttempted bool // lifecycle-protected; the callback never changes it
}

type ownerPodInputs struct {
	dirty   map[podInputKey]struct{}
	deleted map[podInputKey]*deletedPodInput
}

func ownerKey(ms *api.ModelServing) ownerInputKey {
	return ownerInputKey{NamespacedName: utils.GetNamespaceName(ms), UID: ms.UID}
}

func childOwnerKey(pod *corev1.Pod) (ownerInputKey, bool) {
	name, _, ok := utils.GetModelServingAndGroupByLabel(pod.Labels)
	if !ok {
		return ownerInputKey{}, false
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind == "ModelServing" && ref.APIVersion == api.SchemeGroupVersion.String() {
			return ownerInputKey{types.NamespacedName{Namespace: pod.Namespace, Name: name}, ref.UID}, true
		}
	}
	return ownerInputKey{}, false
}

func (in *podInputs) get(key ownerInputKey) *ownerPodInputs {
	if in.owners == nil {
		in.owners = make(map[ownerInputKey]*ownerPodInputs)
	}
	if in.owners[key] == nil {
		in.owners[key] = &ownerPodInputs{dirty: make(map[podInputKey]struct{}), deleted: make(map[podInputKey]*deletedPodInput)}
	}
	return in.owners[key]
}

func (c *ModelServingController) publishPodChange(pod *corev1.Pod) {
	key, ok := childOwnerKey(pod)
	if !ok {
		return
	}
	c.inputs.mu.Lock()
	c.inputs.get(key).dirty[podInputKey{pod.Name, pod.UID}] = struct{}{}
	c.inputs.mu.Unlock()
}

func (c *ModelServingController) publishPodDeletion(pod *corev1.Pod) {
	key, ok := childOwnerKey(pod)
	if !ok {
		return
	}
	copy := pod.DeepCopy()
	c.inputs.mu.Lock()
	in := c.inputs.get(key)
	id := podInputKey{pod.Name, pod.UID}
	delete(in.dirty, id)
	if in.deleted[id] == nil {
		in.deleted[id] = &deletedPodInput{pod: copy}
	}
	c.inputs.mu.Unlock()
}

func (c *ModelServingController) retirePodInputs(name types.NamespacedName, keep types.UID) {
	c.inputs.mu.Lock()
	for key := range c.inputs.owners {
		if key.NamespacedName == name && (keep == "" || key.UID != keep) {
			delete(c.inputs.owners, key)
		}
	}
	c.inputs.mu.Unlock()
	c.forgetKnownMemberOwner(name, keep)
}

func deletedPodObject(obj interface{}) *corev1.Pod {
	if pod, ok := obj.(*corev1.Pod); ok {
		return pod
	}
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		if pod, ok := tombstone.Obj.(*corev1.Pod); ok {
			return pod
		}
	}
	return nil
}

// processPodInputs borrows a finite batch. New callbacks publish into another
// dirty set and Add while processing causes the queue to run this owner again.
// The caller holds lifecycle for this owner.
func (c *ModelServingController) processPodInputs(ctx context.Context, ms *api.ModelServing) error {
	c.inputs.mu.Lock()
	in := c.inputs.get(ownerKey(ms))
	dirty := in.dirty
	in.dirty = make(map[podInputKey]struct{})
	deleted := make(map[podInputKey]*deletedPodInput, len(in.deleted))
	for id, item := range in.deleted {
		deleted[id] = item
	}
	c.inputs.mu.Unlock()
	klog.V(4).InfoS("Processing Pod inputs", "modelServing", klog.KObj(ms), "updates", len(dirty), "deletions", len(deleted))
	var errs []error
	for id := range dirty {
		pod, err := c.readPodInput(ctx, ms, id)
		if err == nil && pod != nil {
			err = c.applyPodObservationLocked(pod, false)
		}
		if err != nil {
			c.inputs.mu.Lock()
			in.dirty[id] = struct{}{}
			c.inputs.mu.Unlock()
			errs = append(errs, err)
		}
	}
	for id, item := range deleted {
		done, err := c.processPodDeletionLocked(ctx, ms, item)
		if err != nil {
			errs = append(errs, err)
		}
		if done {
			c.inputs.mu.Lock()
			if in.deleted[id] == item {
				delete(in.deleted, id)
			}
			c.inputs.mu.Unlock()
		}
	}
	if err := c.maintainPodDiscovery(ctx, ms); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (c *ModelServingController) readPodInput(ctx context.Context, ms *api.ModelServing, id podInputKey) (*corev1.Pod, error) {
	observation := c.capturePodObservation(&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: ms.Namespace, Name: id.Name, UID: id.UID}})
	var pod *corev1.Pod
	var err error
	if ms.ResourceVersion == "" {
		pod, err = c.podsLister.Pods(ms.Namespace).Get(id.Name)
	} else {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		pod, err = c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, id.Name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		return nil, nil
	} // preserve the separate existence witness
	if err != nil {
		return nil, err
	}
	if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
		return nil, nil
	}
	c.observeKnownMember(ms, pod)
	if pod.UID != id.UID {
		return nil, nil
	}
	c.observeLivePodEpisode(ms, pod, observation)
	return pod, nil
}

func (c *ModelServingController) processPodDeletionLocked(ctx context.Context, ms *api.ModelServing, item *deletedPodInput) (bool, error) {
	pod := item.pod
	group, role, id := pod.Labels[api.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
	if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) || c.shouldSkipHandling(ms, group, pod) {
		return true, nil
	}
	stale, siblings, err := c.deletionObservation(ctx, ms, pod)
	if err != nil {
		return false, err
	}
	if stale {
		return true, nil
	}
	c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), group, pod.Name)
	if !item.hookAttempted {
		item.hookAttempted = true
		chain, hookErr := c.buildPluginChain(ms)
		if hookErr == nil && chain != nil {
			hookErr = chain.OnPodDelete(ctx, &plugins.HookRequest{ModelServing: ms, ServingGroup: group, RoleName: role, RoleID: id, IsEntry: pod.Labels[api.EntryLabelKey] == utils.Entry, Pod: pod, PodLister: c.podsLister, ConfigMapLister: c.configMapsLister, KubeClient: c.kubeClientSet, ServiceLister: c.servicesLister})
		}
		if hookErr != nil {
			klog.ErrorS(hookErr, "Best-effort OnPodDelete", "pod", pod.Name)
		}
	}
	if c.handleDeletionInProgress(ms, group, role, id) {
		c.forgetKnownMember(ms, pod)
		return true, nil
	}
	ctx = withObservedPodDeletion(ctx, ms, pod, siblings)
	if err := c.handlePodDeletionObservation(ctx, ms, group, pod, false); err != nil {
		return false, err
	}
	c.forgetKnownMember(ms, pod)
	return true, nil
}

// A received DELETE must be handed off before a new UID can occupy its scope.
// This check never retains the input mutex across the caller's API request.
func (c *ModelServingController) checkPendingPodDeletion(ms *api.ModelServing, group, role, id string) error {
	c.inputs.mu.Lock()
	defer c.inputs.mu.Unlock()
	in := c.inputs.owners[ownerKey(ms)]
	if in == nil {
		return nil
	}
	for _, item := range in.deleted {
		p := item.pod
		if p.Labels[api.GroupNameLabelKey] == group && (ms.Spec.RecoveryPolicy == api.ServingGroupRecreate || (utils.GetRoleName(p) == role && utils.GetRoleID(p) == id)) {
			return fmt.Errorf("Pod %s deletion awaits recovery handoff", p.Name)
		}
	}
	return nil
}
