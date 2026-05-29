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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// defaultDisruptionTTL defines how long a logical disruption unit is kept in the tracker to wait for Informer sync.
	defaultDisruptionTTL   = 60 * time.Second
	trackerConfigMapPrefix = "kthena-eviction-tracker-"
	trackerEntriesKey      = "entries"
)

type disruptionUnit struct {
	namespace    string
	modelServing string
	level        workloadv1alpha1.ProtectionLevelType
	groupName    string
	role         string
	roleID       string
}

type disruptionEntries map[string]time.Time

// EvictionHandler handles pods/eviction admission requests with concurrency safety.
type EvictionHandler struct {
	kubeClient   kubernetes.Interface
	kthenaClient clientset.Interface
	podLister    corelisters.PodLister
	msLister     workloadlisters.ModelServingLister

	// disruptionTTL controls how long a recently allowed logical unit remains
	// in the shared ConfigMap tracker to cover informer cache lag.
	disruptionTTL time.Duration
}

func NewEvictionHandler(kubeClient kubernetes.Interface, kthenaClient clientset.Interface, podLister corelisters.PodLister, msLister workloadlisters.ModelServingLister, disruptionTTL ...time.Duration) *EvictionHandler {
	ttl := defaultDisruptionTTL
	if len(disruptionTTL) > 0 && disruptionTTL[0] > 0 {
		ttl = disruptionTTL[0]
	}
	return &EvictionHandler{
		kubeClient:    kubeClient,
		kthenaClient:  kthenaClient,
		podLister:     podLister,
		msLister:      msLister,
		disruptionTTL: ttl,
	}
}

func AllowEviction(w http.ResponseWriter, r *http.Request) {
	var admissionReview admissionv1.AdmissionReview
	if err := json.NewDecoder(r.Body).Decode(&admissionReview); err != nil {
		klog.Errorf("Failed to decode admission review: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if admissionReview.Request == nil {
		klog.Errorf("AdmissionReview request is nil")
		http.Error(w, "request is nil", http.StatusBadRequest)
		return
	}
	admissionReview.Response = &admissionv1.AdmissionResponse{
		Allowed: true,
		UID:     admissionReview.Request.UID,
	}
	response, _ := json.Marshal(admissionReview)
	w.Header().Set("Content-Type", "application/json")
	w.Write(response)
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

	allowed, reason := h.checkEvictionWithTracker(r.Context(), ms, pod)

	if allowed {
		h.allow(&admissionReview, w)
	} else {
		h.deny(&admissionReview, reason, w)
	}
}

func (h *EvictionHandler) checkEvictionWithTracker(ctx context.Context, ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod) (bool, string) {
	strategy := ms.Spec.RolloutStrategy.EvictionStrategy

	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	allPods, err := h.podLister.Pods(ms.Namespace).List(selector)
	if err != nil {
		klog.Errorf("Failed to list pods for ModelServing %s: %v", ms.Name, err)
		return true, ""
	}

	var allowed bool
	var reason string
	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := h.getOrCreateTrackerConfigMap(ctx, ms)
		if err != nil {
			return err
		}

		entries, err := decodeDisruptionEntries(cm)
		if err != nil {
			return err
		}
		cleanupDisruptionEntries(entries)

		var unit *disruptionUnit
		if strategy.ProtectionLevel == workloadv1alpha1.ProtectionLevelRole {
			allowed, reason, unit = h.checkRoleProtection(ms, targetPod, strategy, allPods, entries)
		} else {
			allowed, reason, unit = h.checkServingGroupProtection(ms, targetPod, strategy, allPods, entries)
		}
		if !allowed || unit == nil {
			return nil
		}

		entries[unit.key()] = time.Now().Add(h.disruptionTTL)
		updated := cm.DeepCopy()
		if updated.Data == nil {
			updated.Data = map[string]string{}
		}
		encoded, err := encodeDisruptionEntries(entries)
		if err != nil {
			return err
		}
		updated.Data[trackerEntriesKey] = encoded
		_, err = h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return false, fmt.Sprintf("Eviction denied: failed to update ModelServing %s disruption tracker: %v.", ms.Name, err)
	}
	return allowed, reason
}

func (h *EvictionHandler) checkServingGroupProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod, entries disruptionEntries) (bool, string, *disruptionUnit) {
	targetGroupName := targetPod.Labels[workloadv1alpha1.GroupNameLabelKey]
	if targetGroupName == "" {
		return true, "", nil
	}

	groups := make(map[string][]*corev1.Pod)
	for _, p := range allPods {
		gn := p.Labels[workloadv1alpha1.GroupNameLabelKey]
		if gn != "" {
			groups[gn] = append(groups[gn], p)
		}
	}

	readyGroups := 0
	targetGroupReady := false
	targetGroupFound := false
	for gn, pods := range groups {
		isReady := h.isServingGroupReady(ms, gn, pods, entries)
		if isReady {
			readyGroups++
		}
		if gn == targetGroupName {
			targetGroupFound = true
			targetGroupReady = isReady
		}
	}
	if !targetGroupFound {
		return true, "", nil
	}

	// If target group is already not ready, allow eviction.
	if !targetGroupReady {
		return true, "", nil
	}

	totalReplicas := int(replicasOrDefault(ms.Spec.Replicas))
	if totalReplicas == 0 {
		return true, "", nil
	}
	minAvailable, _ := intstr.GetScaledValueFromIntOrPercent(minAvailableOrDefault(strategy.MinAvailable), totalReplicas, true)

	if readyGroups > minAvailable {
		unit := servingGroupUnit(ms, targetGroupName)
		return true, "", &unit
	}

	return false, fmt.Sprintf("Eviction denied: protected by ModelServing %s. Current ready groups (%d) <= minAvailable (%d).", ms.Name, readyGroups, minAvailable), nil
}

func (h *EvictionHandler) checkRoleProtection(ms *workloadv1alpha1.ModelServing, targetPod *corev1.Pod, strategy *workloadv1alpha1.EvictionStrategySpec, allPods []*corev1.Pod, entries disruptionEntries) (bool, string, *disruptionUnit) {
	targetRole := targetPod.Labels[workloadv1alpha1.RoleLabelKey]
	targetRoleID := targetPod.Labels[workloadv1alpha1.RoleIDKey]
	if targetRole == "" || targetRoleID == "" {
		return true, "", nil
	}

	roleInstances := make(map[string][]*corev1.Pod)
	for _, p := range allPods {
		if p.Labels[workloadv1alpha1.RoleLabelKey] == targetRole {
			roleID := p.Labels[workloadv1alpha1.RoleIDKey]
			if roleID != "" {
				roleInstances[roleID] = append(roleInstances[roleID], p)
			}
		}
	}

	readyInstances := 0
	targetInstanceReady := false
	targetInstanceFound := false
	for roleID, pods := range roleInstances {
		isReady := h.isRoleInstanceReady(ms, targetRole, roleID, pods, entries)
		if isReady {
			readyInstances++
		}
		if roleID == targetRoleID {
			targetInstanceFound = true
			targetInstanceReady = isReady
		}
	}
	if !targetInstanceFound {
		return true, "", nil
	}

	if !targetInstanceReady {
		return true, "", nil
	}

	totalInstances := h.expectedRoleInstances(ms, targetRole, len(roleInstances))
	if totalInstances == 0 {
		return true, "", nil
	}
	minAvailableValue := roleMinAvailableOrDefault(strategy, targetRole)
	minAvailable, _ := intstr.GetScaledValueFromIntOrPercent(minAvailableValue, totalInstances, true)

	if readyInstances > minAvailable {
		unit := roleUnit(ms, targetRole, targetRoleID)
		return true, "", &unit
	}

	return false, fmt.Sprintf("Eviction denied: protected by ModelServing %s. Role %s ready instances (%d) <= minAvailable (%d).", ms.Name, targetRole, readyInstances, minAvailable), nil
}

func (h *EvictionHandler) isServingGroupReady(ms *workloadv1alpha1.ModelServing, groupName string, pods []*corev1.Pod, entries disruptionEntries) bool {
	if isUnitDisrupted(entries, servingGroupUnit(ms, groupName)) {
		return false
	}
	return arePodsReady(pods)
}

func (h *EvictionHandler) isRoleInstanceReady(ms *workloadv1alpha1.ModelServing, role, roleID string, pods []*corev1.Pod, entries disruptionEntries) bool {
	if isUnitDisrupted(entries, roleUnit(ms, role, roleID)) {
		return false
	}
	return arePodsReady(pods)
}

func arePodsReady(pods []*corev1.Pod) bool {
	if len(pods) == 0 {
		return false
	}
	for _, pod := range pods {
		if !isPodReady(pod) {
			return false
		}
	}
	return true
}

func isPodReady(pod *corev1.Pod) bool {
	if pod.DeletionTimestamp != nil {
		return false
	}
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func isUnitDisrupted(entries disruptionEntries, unit disruptionUnit) bool {
	expiry, ok := entries[unit.key()]
	return ok && time.Now().Before(expiry)
}

func cleanupDisruptionEntries(entries disruptionEntries) {
	now := time.Now()
	for key, expiry := range entries {
		if now.After(expiry) {
			delete(entries, key)
		}
	}
}

func (h *EvictionHandler) getOrCreateTrackerConfigMap(ctx context.Context, ms *workloadv1alpha1.ModelServing) (*corev1.ConfigMap, error) {
	name := trackerConfigMapName(ms.Name)
	cm, err := h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		return cm, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}

	cm = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			},
		},
		Data: map[string]string{
			trackerEntriesKey: "{}",
		},
	}
	if ms.UID != "" {
		cm.OwnerReferences = []metav1.OwnerReference{
			{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
				Kind:       "ModelServing",
				Name:       ms.Name,
				UID:        ms.UID,
			},
		}
	}
	cm, err = h.kubeClient.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		return nil, apierrors.NewConflict(schema.GroupResource{Resource: "configmaps"}, name, err)
	}
	return cm, err
}

func trackerConfigMapName(modelServingName string) string {
	return trackerConfigMapPrefix + modelServingName
}

func decodeDisruptionEntries(cm *corev1.ConfigMap) (disruptionEntries, error) {
	if cm.Data == nil || cm.Data[trackerEntriesKey] == "" {
		return disruptionEntries{}, nil
	}
	raw := map[string]string{}
	if err := json.Unmarshal([]byte(cm.Data[trackerEntriesKey]), &raw); err != nil {
		return nil, fmt.Errorf("decode tracker entries: %w", err)
	}
	entries := make(disruptionEntries, len(raw))
	for key, value := range raw {
		expiry, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			return nil, fmt.Errorf("decode tracker entry %q expiry: %w", key, err)
		}
		entries[key] = expiry
	}
	return entries, nil
}

func encodeDisruptionEntries(entries disruptionEntries) (string, error) {
	raw := make(map[string]string, len(entries))
	for key, expiry := range entries {
		raw[key] = expiry.Format(time.RFC3339Nano)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return "", fmt.Errorf("encode tracker entries: %w", err)
	}
	return string(data), nil
}

func servingGroupUnit(ms *workloadv1alpha1.ModelServing, groupName string) disruptionUnit {
	return disruptionUnit{
		namespace:    ms.Namespace,
		modelServing: ms.Name,
		level:        workloadv1alpha1.ProtectionLevelServingGroup,
		groupName:    groupName,
	}
}

func roleUnit(ms *workloadv1alpha1.ModelServing, role, roleID string) disruptionUnit {
	return disruptionUnit{
		namespace:    ms.Namespace,
		modelServing: ms.Name,
		level:        workloadv1alpha1.ProtectionLevelRole,
		role:         role,
		roleID:       roleID,
	}
}

func (u disruptionUnit) key() string {
	switch u.level {
	case workloadv1alpha1.ProtectionLevelRole:
		return fmt.Sprintf("%s/%s/%s/%s/%s", u.level, u.namespace, u.modelServing, u.role, u.roleID)
	default:
		return fmt.Sprintf("%s/%s/%s/%s", u.level, u.namespace, u.modelServing, u.groupName)
	}
}

func minAvailableOrDefault(value *intstr.IntOrString) *intstr.IntOrString {
	if value != nil {
		return value
	}
	defaultValue := intstr.FromInt(1)
	return &defaultValue
}

func roleMinAvailableOrDefault(strategy *workloadv1alpha1.EvictionStrategySpec, role string) *intstr.IntOrString {
	if strategy.RoleMinAvailable != nil {
		if value, ok := strategy.RoleMinAvailable[role]; ok {
			return &value
		}
	}
	return minAvailableOrDefault(strategy.MinAvailable)
}

func (h *EvictionHandler) expectedRoleInstances(ms *workloadv1alpha1.ModelServing, roleName string, fallback int) int {
	modelServingReplicas := replicasOrDefault(ms.Spec.Replicas)
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == roleName {
			return int(modelServingReplicas * replicasOrDefault(role.Replicas))
		}
	}
	if fallback > 0 {
		return fallback
	}
	return 1
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
