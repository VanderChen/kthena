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

// Package ranktableutil shares lifecycle and applied-layout checks between the
// annotation-server and Pod-IP ranktable plugins. Rendering stays in each plugin.
package ranktableutil

import (
	"context"
	"fmt"
	"maps"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	GroupInstance = "workload.kthena.io/group-instance"
	RoleInstance  = "workload.kthena.io/role-instance"
	identityBound = "ranktable.kthena.io/identity-bound"
	generatedAt   = "ranktable.kthena.io/generated-at"
)

// Mode distinguishes preparation, active self-healing and deletion callbacks.
type Mode int

const (
	Prepare Mode = iota
	Refresh
	UpdateExisting
)

type Scope struct {
	Name      string
	Filename  string
	RoleLevel bool
	Labels    map[string]string
}

func NewScope(req *plugins.HookRequest, roleLevel bool, filename string) Scope {
	name := req.ServingGroup
	level := "group"
	cmLabels := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: req.ModelServing.Name,
		workloadv1alpha1.GroupNameLabelKey:        req.ServingGroup,
		"app.kubernetes.io/component":             "ranktable",
	}
	if roleLevel {
		name += "-" + req.RoleID
		level = "role"
		cmLabels[workloadv1alpha1.RoleLabelKey] = req.RoleName
		cmLabels[workloadv1alpha1.RoleIDKey] = req.RoleID
	}
	cmLabels["ranktable-level"] = level
	return Scope{Name: req.ModelServing.Name + "-" + name + "-ranktable", Filename: filename, RoleLevel: roleLevel, Labels: cmLabels}
}

// Render receives validated members and a stable generation timestamp. A plugin
// can still render Initializing when its annotation parser or Pod IP is missing.
type Render func(complete bool, pods []*corev1.Pod, timestamp string) (string, error)

// Sync never creates on UpdateExisting. Once a CM has been observed, retries
// remain bound to that UID even if a same-name replacement appears.
func Sync(ctx context.Context, req *plugins.HookRequest, scope Scope, mode Mode, annotationName string, render Render) error {
	if req.ModelServing.DeletionTimestamp != nil {
		return nil
	}
	var observed bool
	var uid types.UID
	klog.V(4).InfoS("Sync ranktable", "namespace", req.ModelServing.Namespace, "scope", scope.Name, "ownerUID", req.ModelServing.UID, "mode", mode)
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		cm, err := req.KubeClient.CoreV1().ConfigMaps(req.ModelServing.Namespace).Get(ctx, scope.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if mode == UpdateExisting || (observed && uid != "") {
				return nil
			}
			cm = nil
		} else if err != nil {
			return err
		}
		if !observed {
			observed = true
			if cm != nil {
				uid = cm.UID
			}
		} else if cm != nil && cm.UID != uid {
			klog.V(4).InfoS("Skip replaced ranktable", "scope", scope.Name, "observedUID", uid, "currentUID", cm.UID)
			return nil
		}
		if cm != nil && (!metav1.IsControlledBy(cm, req.ModelServing) || !matchesScope(cm, scope)) {
			if mode == UpdateExisting {
				return nil
			}
			return fmt.Errorf("ranktable ConfigMap %s/%s belongs to a different owner or scope", cm.Namespace, cm.Name)
		}

		members, anchor, current, err := currentMembers(ctx, req, scope, mode, cm)
		if err != nil || !current {
			return err
		}
		if cm != nil {
			if cm.Annotations[identityBound] == "v1" {
				if !sameInstance(cm.Annotations, anchor, scope.RoleLevel) {
					if mode != Prepare {
						return nil
					}
					// A new creation may replace the retired CM, but not while
					// any member of its old incarnation still occupies the scope.
					for _, pod := range members {
						if sameInstance(cm.Annotations, pod.Annotations, scope.RoleLevel) {
							return fmt.Errorf("ranktable %s still has retiring members", scope.Name)
						}
					}
					if err := req.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Delete(ctx, cm.Name, *metav1.NewPreconditionDeleteOptions(string(cm.UID))); err != nil && !apierrors.IsNotFound(err) {
						return err
					}
					cm = nil
				}
			} else if mode == UpdateExisting || len(members) == 0 {
				// Negative events cannot establish a legacy CM's identity.
				return nil
			}
		}

		active := make([]*corev1.Pod, 0, len(members))
		roleInstances := make(map[string]string)
		for _, pod := range members {
			if !sameInstance(pod.Annotations, anchor, scope.RoleLevel) {
				return fmt.Errorf("ranktable %s has conflicting member incarnations", scope.Name)
			}
			if pod.DeletionTimestamp == nil && (req.Pod == nil || mode != UpdateExisting || pod.UID != req.Pod.UID) {
				roleID := utils.GetRoleID(pod)
				if instance, found := roleInstances[roleID]; found && instance != pod.Annotations[RoleInstance] {
					return fmt.Errorf("ranktable %s has conflicting incarnations for Role %s", scope.Name, roleID)
				}
				roleInstances[roleID] = pod.Annotations[RoleInstance]
				active = append(active, pod)
			}
		}
		if mode == Refresh && len(active) == 0 {
			return nil
		}
		// Preparing another Pod does not reset an already published table.
		if mode == Prepare && cm != nil && cm.Annotations[identityBound] == "v1" {
			return nil
		}
		// Keep the full snapshot for lifecycle and applied-layout checks. Only
		// annotated Pods participate in communication and must be Running.
		participants := make([]*corev1.Pod, 0, len(active))
		for _, pod := range active {
			if _, found := pod.Annotations[annotationName]; found {
				participants = append(participants, pod)
			}
		}
		// Prepare runs before Pod creation, possibly before device annotations.
		// Other callbacks must not create a table without any participants.
		if mode != Prepare && len(participants) == 0 && cm == nil {
			return nil
		}
		complete := mode != Prepare && len(participants) > 0
		var layoutErr error
		if complete && scope.RoleLevel {
			complete, err = completeRole(ctx, req, active)
			if err != nil {
				layoutErr = err
				klog.Warningf("Ranktable %s/%s cannot resolve applied layout: %v", req.ModelServing.Namespace, scope.Name, err)
				if cm == nil {
					return err
				}
				complete = false
			}
		}
		for _, pod := range participants {
			complete = complete && pod.Status.Phase == corev1.PodRunning
		}
		if mode == Prepare {
			participants = nil
		}
		timestamp := time.Now().Format(time.RFC3339)
		if cm != nil && cm.Annotations[generatedAt] != "" {
			timestamp = cm.Annotations[generatedAt]
		}
		content, err := render(complete, participants, timestamp)
		if err != nil {
			return err
		}
		if cm != nil && cm.Data[scope.Filename] != content {
			timestamp = time.Now().Format(time.RFC3339)
			content, err = render(complete, participants, timestamp)
			if err != nil {
				return err
			}
		}
		if cm == nil {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
				Name: scope.Name, Namespace: req.ModelServing.Namespace,
				Labels:          maps.Clone(scope.Labels),
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(req.ModelServing, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))},
			}}
			bind(cm, anchor, scope.RoleLevel, timestamp)
			cm.Data = map[string]string{scope.Filename: content}
			klog.V(4).InfoS("Create active ranktable", "scope", scope.Name, "groupInstance", anchor[GroupInstance], "roleInstance", anchor[RoleInstance], "mode", mode)
			_, err = req.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Create(ctx, cm, metav1.CreateOptions{})
			return err
		}
		updated := cm.DeepCopy()
		bind(updated, anchor, scope.RoleLevel, timestamp)
		updated.Data = map[string]string{scope.Filename: content}
		if maps.Equal(updated.Data, cm.Data) && maps.Equal(updated.Annotations, cm.Annotations) {
			return layoutErr
		}
		klog.V(4).InfoS("Update ranktable", "scope", scope.Name, "cmUID", cm.UID, "mode", mode, "members", len(participants), "complete", complete)
		_, err = req.KubeClient.CoreV1().ConfigMaps(cm.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		return layoutErr
	})
}

func matchesScope(cm *corev1.ConfigMap, scope Scope) bool {
	for key, value := range scope.Labels {
		if cm.Labels[key] != value {
			return false
		}
	}
	return true
}

func sameInstance(left, right map[string]string, roleLevel bool) bool {
	return left[GroupInstance] == right[GroupInstance] && (!roleLevel || left[RoleInstance] == right[RoleInstance])
}

func bind(cm *corev1.ConfigMap, anchor map[string]string, roleLevel bool, timestamp string) {
	if cm.Annotations == nil {
		cm.Annotations = make(map[string]string)
	}
	cm.Annotations[identityBound] = "v1"
	cm.Annotations[GroupInstance] = anchor[GroupInstance]
	if roleLevel {
		cm.Annotations[RoleInstance] = anchor[RoleInstance]
	}
	cm.Annotations[generatedAt] = timestamp
}

func currentMembers(ctx context.Context, req *plugins.HookRequest, scope Scope, mode Mode, cm *corev1.ConfigMap) ([]*corev1.Pod, map[string]string, bool, error) {
	selector := labels.Set{
		workloadv1alpha1.ModelServingNameLabelKey: req.ModelServing.Name,
		workloadv1alpha1.GroupNameLabelKey:        req.ServingGroup,
	}
	if scope.RoleLevel {
		selector[workloadv1alpha1.RoleLabelKey] = req.RoleName
		selector[workloadv1alpha1.RoleIDKey] = req.RoleID
	}
	list, err := req.KubeClient.CoreV1().Pods(req.ModelServing.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.AsSelector().String()})
	if err != nil {
		return nil, nil, false, err
	}
	members := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		if utils.IsOwnedByModelServingWithUID(pod, req.ModelServing.UID) {
			members = append(members, pod)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	var anchor map[string]string
	if req.Pod != nil {
		if !utils.IsOwnedByModelServingWithUID(req.Pod, req.ModelServing.UID) ||
			req.Pod.Labels[workloadv1alpha1.GroupNameLabelKey] != req.ServingGroup ||
			req.Pod.Labels[workloadv1alpha1.RoleLabelKey] != req.RoleName || req.Pod.Labels[workloadv1alpha1.RoleIDKey] != req.RoleID {
			return nil, nil, false, nil
		}
		anchor = req.Pod.Annotations
		if mode != Prepare {
			live, getErr := req.KubeClient.CoreV1().Pods(req.Pod.Namespace).Get(ctx, req.Pod.Name, metav1.GetOptions{})
			if getErr != nil && !apierrors.IsNotFound(getErr) {
				return nil, nil, false, getErr
			}
			if mode == Refresh {
				if getErr != nil || live.UID != req.Pod.UID || live.DeletionTimestamp != nil || !utils.IsOwnedByModelServingWithUID(live, req.ModelServing.UID) {
					return nil, nil, false, nil
				}
				anchor = live.Annotations
			} else if getErr == nil {
				// Physical deletion has not occurred, or the name has been reused.
				return nil, nil, false, nil
			}
		}
		if mode == UpdateExisting {
			for _, pod := range members {
				if utils.GetRoleID(pod) == req.RoleID && utils.GetRoleName(pod) == req.RoleName && !sameInstance(pod.Annotations, anchor, true) {
					return nil, nil, false, nil
				}
			}
		}
	} else {
		for _, pod := range members {
			if pod.DeletionTimestamp == nil {
				anchor = pod.Annotations
				if anchor == nil {
					anchor = map[string]string{}
				}
				break
			}
		}
		if anchor == nil && cm != nil && mode == UpdateExisting {
			anchor = cm.Annotations
		} else if anchor == nil {
			return nil, nil, false, nil
		}
	}
	return members, anchor, true, nil
}

func completeRole(ctx context.Context, req *plugins.HookRequest, pods []*corev1.Pod) (bool, error) {
	// An entry is the anchor; a surviving worker can resolve the layout when
	// entry is missing, but the expected-name check will still reject that gap.
	anchor := pods[0]
	entryName := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	for _, pod := range pods {
		if pod.Name == entryName {
			anchor = pod
			break
		}
	}
	cache := make(map[string]workloadv1alpha1.Role)
	resolve := func(pod *corev1.Pod) (workloadv1alpha1.Role, error) {
		if req.Pod == nil && req.Role != nil && utils.ObjectRoleTemplateHash(pod) == utils.CalRoleTemplateHash(*req.Role) {
			return *req.Role.DeepCopy(), nil
		}
		revision := utils.ObjectRevision(pod)
		if revision == "" {
			return workloadv1alpha1.Role{}, fmt.Errorf("Pod %s has no applied revision", pod.Name)
		}
		if role, ok := cache[revision]; ok {
			return role, nil
		}
		cr, err := utils.GetControllerRevision(ctx, req.KubeClient, req.ModelServing, revision)
		if err != nil {
			return workloadv1alpha1.Role{}, err
		}
		roles, err := utils.GetRolesFromControllerRevision(cr)
		if err != nil {
			return workloadv1alpha1.Role{}, err
		}
		for _, role := range roles {
			if role.Name == req.RoleName {
				cache[revision] = role
				return role, nil
			}
		}
		return workloadv1alpha1.Role{}, fmt.Errorf("revision %s has no Role %s", revision, req.RoleName)
	}
	role, err := resolve(anchor)
	if err != nil {
		return false, err
	}
	if role.WorkerReplicas < 0 || int64(len(pods)) != 1+int64(role.WorkerReplicas) {
		return false, nil
	}
	expected := make(map[string]bool, len(pods))
	for i := 0; i <= int(role.WorkerReplicas); i++ {
		expected[utils.GeneratePodName(req.ServingGroup, req.RoleID, i)] = i == 0
	}
	for _, pod := range pods {
		entry, found := expected[pod.Name]
		if !found || (pod.Labels[workloadv1alpha1.EntryLabelKey] == utils.Entry) != entry {
			return false, nil
		}
		applied, err := resolve(pod)
		if err != nil {
			return false, err
		}
		if !utils.EqualRoleTemplateForRevision(role, applied) {
			return false, fmt.Errorf("Pod %s has a conflicting applied Role template", pod.Name)
		}
		delete(expected, pod.Name)
	}
	return len(expected) == 0, nil
}
