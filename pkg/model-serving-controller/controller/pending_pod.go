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
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// enqueuePendingPodUpdate avoids waiting for an entire reconcile when the
// ordinary Pending observation has no lifecycle bookkeeping left to perform.
// It only requests work; it never grants availability or permission to mutate
// resources. All other observations must be re-read under the lifecycle lock.
func (c *ModelServingController) enqueuePendingPodUpdate(oldPod, pod *corev1.Pod) bool {
	if !c.initialSync.Load() || c.workqueue == nil || !sameOrdinaryPendingPod(oldPod, pod) {
		return false
	}
	// Preserve the live identity/health check: a queued Pending frame can already
	// be Running, failed, terminating, or replaced in the API.
	live := c.currentPodEvent(pod)
	if live == nil {
		// currentPodEvent already requested a delayed retry on read failure,
		// absence, or a different UID, without changing lifecycle state.
		return true
	}
	if !sameOrdinaryPendingPod(pod, live) {
		return false
	}
	ms, group, err := c.getModelServingByChildResource(live)
	if err != nil || ms.UID == "" || ms.DeletionTimestamp != nil || group == "" {
		return false
	}
	owner := metav1.GetControllerOf(live)
	if owner == nil || owner.Name != ms.Name || owner.UID != ms.UID ||
		owner.APIVersion != workloadv1alpha1.SchemeGroupVersion.String() || owner.Kind != "ModelServing" {
		return false
	}
	role, instance := utils.GetRoleName(live), utils.GetRoleID(live)
	if role == "" || instance == "" ||
		!c.store.IsPodUnavailable(utils.GetNamespaceName(ms), group, role, instance, live.Name) {
		return false
	}
	// The store check is only a short observation, not a lock on the next
	// reconcile. Add also marks an in-flight key dirty so it rechecks live state.
	c.enqueueModelServing(ms)
	klog.V(4).InfoS("Enqueued ordinary Pending Pod without lifecycle lock", "pod", klog.KObj(live), "modelServing", klog.KObj(ms))
	return true
}

// Only status updates of an otherwise unchanged, ordinary Pending Pod qualify.
// Synthetic objects and changes to identity, plugin inputs or failure state
// retain the full callback path. Resource versions are opaque, never ordered.
func sameOrdinaryPendingPod(oldPod, pod *corev1.Pod) bool {
	for _, p := range []*corev1.Pod{oldPod, pod} {
		if p == nil || p.UID == "" || p.ResourceVersion == "" || p.Status.Phase != corev1.PodPending ||
			p.DeletionTimestamp != nil || p.DeletionGracePeriodSeconds != nil ||
			p.Annotations[deletionScopeAnnotation] != "" || utils.ContainerRestarted(p) {
			return false
		}
	}
	return oldPod.Namespace == pod.Namespace && oldPod.Name == pod.Name && oldPod.UID == pod.UID &&
		apiequality.Semantic.DeepEqual(oldPod.Labels, pod.Labels) &&
		apiequality.Semantic.DeepEqual(oldPod.Annotations, pod.Annotations) &&
		apiequality.Semantic.DeepEqual(oldPod.OwnerReferences, pod.OwnerReferences) &&
		apiequality.Semantic.DeepEqual(oldPod.Finalizers, pod.Finalizers) &&
		apiequality.Semantic.DeepEqual(oldPod.Spec, pod.Spec) &&
		oldPod.Status.PodIP == pod.Status.PodIP && apiequality.Semantic.DeepEqual(oldPod.Status.PodIPs, pod.Status.PodIPs)
}
