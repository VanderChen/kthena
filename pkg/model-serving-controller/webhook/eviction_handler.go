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

package webhook

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// disruptionExpiry defines how long a pod is kept in the tracker to wait for Informer sync.
	disruptionExpiry = 30 * time.Second
)

// EvictionHandler handles pods/eviction admission requests with concurrency safety.
type EvictionHandler struct {
	kubeClient   kubernetes.Interface
	kthenaClient clientset.Interface
	podLister    corelisters.PodLister
	msLister     workloadlisters.ModelServingLister

	// disruptionTracker tracks pods that were recently allowed for eviction.
	// This prevents cache lag from causing multiple evictions that violate minAvailable.
	// Key: pod-namespace/pod-name, Value: expiry time.
	mu                sync.Mutex
	disruptionTracker map[string]time.Time
}

func NewEvictionHandler(kubeClient kubernetes.Interface, kthenaClient clientset.Interface, podLister corelisters.PodLister, msLister workloadlisters.ModelServingLister) *EvictionHandler {
	return &EvictionHandler{
		kubeClient:        kubeClient,
		kthenaClient:      kthenaClient,
		podLister:         podLister,
		msLister:          msLister,
		disruptionTracker: make(map[string]time.Time),
	}
}

func (h *EvictionHandler) Handle(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		klog.Errorf("Failed to decode admission review: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	ar := admissionReview.Request
	if ar == nil {
		klog.Errorf("AdmissionReview request is nil")
		http.Error(w, "request is nil", http.StatusBadRequest)
		return
	}

	if ar.Resource.Resource != "pods" || ar.SubResource != "eviction" {
		h.allow(&admissionReview, w)
		return
	}

	podNamespace := ar.Namespace
	podName := ar.Name

	pod, err := h.podLister.Pods(podNamespace).Get(podName)
	if err != nil {
		klog.Errorf("Failed to get pod %s/%s from lister: %v", podNamespace, podName, err)
		h.allow(&admissionReview, w)
		return
	}

	msName := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]
	if msName == "" {
		h.allow(&admissionReview, w)
		return
	}

	ms, err := h.msLister.ModelServings(podNamespace).Get(msName)
	if err != nil {
		klog.Errorf("Failed to get ModelServing %s/%s: %v", podNamespace, msName, err)
		h.allow(&admissionReview, w)
		return
	}

	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.EvictionStrategy == nil {
		h.allow(&admissionReview, w)
		return
	}

	// Guard with lock to prevent race conditions during concurrent eviction requests.
	h.mu.Lock()
	defer h.mu.Unlock()

	// Cleanup expired entries first.
	h.cleanupTracker()

	allowed, reason := h.checkEvictionWithTracker(ms, pod)

	if allowed {
		// Record this pod in the tracker to account for Informer delay.
		podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
		h.disruptionTracker[podKey] = time.Now().Add(disruptionExpiry)
		h.allow(&admissionReview, w)
	} else {
		h.deny(&admissionReview, reason, w)
	}
}

func (h *EvictionHandler) checkEvictionWithTracker(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod) (bool, string) {
	strategy := ms.Spec.RolloutStrategy.EvictionStrategy

	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	allPods, err := h.podLister.Pods(ms.Namespace).List(selector)
	if err != nil {
		klog.Errorf("Failed to list pods for ModelServing %s: %v", ms.Name, err)
		return true, ""
	}

	if strategy.ProtectionLevel == workloadv1alpha1.ProtectionLevelRole {
		return h.checkRoleProtection(ms, targetPod, strategy, allPods)
	}
	return h.checkServingGroupProtection(ms, targetPod, strategy, allPods)
}

func (h *EvictionHandler) checkServingGroupProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod) (bool, string) {
	targetGroupName := targetPod.Labels[workloadv1alpha1.GroupNameLabelKey]
	if targetGroupName == "" {
		return true, ""
	}

	groups := make(map[string][]*corev1.Pod)
	for _, p := range allPods {
		gn := p.Labels[workloadv1alpha1.GroupNameLabelKey]
		if gn != "" {
			groups[gn] = append(groups[gn], p)
		}
	}

	readyGroups := 0
	targetGroupReady := true
	for gn, pods := range groups {
		isReady := true
		for _, p := range pods {
			if !h.isPodEffectivelyReady(p) {
				isReady = false
				break
			}
		}
		if isReady {
			readyGroups++
		}
		if gn == targetGroupName {
			targetGroupReady = isReady
		}
	}

	// If target group is already not ready, allow eviction.
	if !targetGroupReady {
		return true, ""
	}

	totalReplicas := int(replicasOrDefault(ms.Spec.Replicas))
	minAvailable, _ := intstr.GetScaledValueFromIntOrPercent(strategy.MinAvailable, totalReplicas, true)

	if readyGroups > minAvailable {
		return true, ""
	}

	return false, fmt.Sprintf("Eviction denied: protected by ModelServing %s. Current ready groups (%d) <= minAvailable (%d).", ms.Name, readyGroups, minAvailable)
}

func (h *EvictionHandler) checkRoleProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod) (bool, string) {
	targetRole := targetPod.Labels[workloadv1alpha1.RoleLabelKey]
	if targetRole == "" {
		return true, ""
	}

	readyInstances := 0
	totalInstances := 0
	targetPodReady := h.isPodEffectivelyReady(targetPod)

	for _, p := range allPods {
		if p.Labels[workloadv1alpha1.RoleLabelKey] == targetRole {
			totalInstances++
			if h.isPodEffectivelyReady(p) {
				readyInstances++
			}
		}
	}

	if !targetPodReady {
		return true, ""
	}

	minAvailable, _ := intstr.GetScaledValueFromIntOrPercent(strategy.MinAvailable, totalInstances, true)

	if readyInstances > minAvailable {
		return true, ""
	}

	return false, fmt.Sprintf("Eviction denied: protected by ModelServing %s. Role %s ready instances (%d) <= minAvailable (%d).", ms.Name, targetRole, readyInstances, minAvailable)
}

// isPodEffectivelyReady checks if a pod is ready AND not currently being evicted (based on tracker).
func (h *EvictionHandler) isPodEffectivelyReady(pod *corev1.Pod) bool {
	// 1. Check Informer state
	if pod.DeletionTimestamp != nil {
		return false
	}
	ready := false
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			ready = true
			break
		}
	}
	if !ready {
		return false
	}

	// 2. Check Memory Tracker state
	podKey := fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
	if expiry, ok := h.disruptionTracker[podKey]; ok {
		if time.Now().Before(expiry) {
			// Pod is allowed for eviction recently, treat as NotReady.
			return false
		}
	}

	return true
}

func (h *EvictionHandler) cleanupTracker() {
	now := time.Now()
	for key, expiry := range h.disruptionTracker {
		if now.After(expiry) {
			delete(h.disruptionTracker, key)
		}
	}
}

func (h *EvictionHandler) allow(review *admissionv1.AdmissionReview, w http.ResponseWriter) {
	review.Response = &admissionv1.AdmissionResponse{
		Allowed: true,
		UID:     review.Request.UID,
	}
	h.sendResponse(review, w)
}

func (h *EvictionHandler) deny(review *admissionv1.AdmissionReview, reason string, w http.ResponseWriter) {
	review.Response = &admissionv1.AdmissionResponse{
		Allowed: false,
		UID:     review.Request.UID,
		Result: &metav1.Status{
			Code:    http.StatusTooManyRequests,
			Message: reason,
		},
	}
	h.sendResponse(review, w)
}

func (h *EvictionHandler) sendResponse(review *admissionv1.AdmissionReview, w http.ResponseWriter) {
	response, _ := json.Marshal(review)
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
}
