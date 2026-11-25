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

package podgroupmanager

import (
	"context"
	"errors"
	"fmt"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// ErrNameOccupied reports that the name of a PodGroup is held by an object
// that does not belong to the ServingGroup.
var ErrNameOccupied = errors.New("the name of the PodGroup is occupied")

// Enabled reports whether Pods rendered from ms are scheduled through a PodGroup.
func (m *Manager) Enabled(ms *workloadv1alpha1.ModelServing) bool {
	return m.shouldCreatePodGroup(ms)
}

// SyncPodGroup creates or updates the PodGroup of one ServingGroup. ms is the
// ModelServing the group is rendered from, owner the controller reference of
// the group, name the name of the group.
//
// A PodGroup of that name which the ModelServing itself controls, or which has
// no controller and carries the labels of the group, was created before groups
// were objects or was left behind when the group was deleted with its
// dependents orphaned. It is handed over to the group and keeps its UID, so
// the Pods bound to it stay scheduled. Any other PodGroup of that name is left
// alone and reported as ErrNameOccupied.
//
// The returned duration, when positive, says when to try again.
func (m *Manager) SyncPodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, owner metav1.OwnerReference, name string) (time.Duration, error) {
	if !m.shouldCreatePodGroup(ms) {
		return 0, nil
	}
	lister := m.GetPodGroupLister()
	if lister == nil {
		return enqueueAfter, errPodGroupInformerNotInitialized
	}
	client := m.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace)

	current, err := lister.PodGroups(ms.Namespace).Get(name)
	if apierrors.IsNotFound(err) {
		desired := &schedulingv1beta1.PodGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: ms.Namespace,
				Labels: map[string]string{
					workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
					workloadv1alpha1.GroupNameLabelKey:        name,
				},
				Annotations:     map[string]string{schedulingv1beta1.KubeGroupNameAnnotationKey: name},
				OwnerReferences: []metav1.OwnerReference{owner},
			},
		}
		m.applyPodGroupSpec(ms, desired)
		if _, err = client.Create(ctx, desired, metav1.CreateOptions{}); err == nil {
			klog.V(2).Infof("Created PodGroup %s/%s", ms.Namespace, name)
			return 0, nil
		}
		if !apierrors.IsAlreadyExists(err) {
			return 0, fmt.Errorf("create PodGroup %s: %w", name, err)
		}
		// The cache is behind, or the name is taken. Only a direct read can tell.
		current, err = client.Get(ctx, name, metav1.GetOptions{})
	}
	if err != nil {
		return 0, fmt.Errorf("get PodGroup %s: %w", name, err)
	}
	if current.DeletionTimestamp != nil {
		return enqueueAfter, fmt.Errorf("PodGroup %s is being deleted", name)
	}

	updated := current.DeepCopy()
	controller := metav1.GetControllerOfNoCopy(current)
	switch {
	case controller != nil && controller.UID == owner.UID:
	case controller != nil && controller.UID == ms.UID,
		controller == nil && current.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name &&
			current.Labels[workloadv1alpha1.GroupNameLabelKey] == name:
		updated.OwnerReferences = withController(current.OwnerReferences, owner)
	default:
		return enqueueAfter, fmt.Errorf("%w: PodGroup %s/%s is not controlled by ServingGroup %s with UID %s", ErrNameOccupied, ms.Namespace, name, owner.Name, owner.UID)
	}
	m.applyPodGroupSpec(ms, updated)
	if apiequality.Semantic.DeepEqual(current.Spec, updated.Spec) && apiequality.Semantic.DeepEqual(current.OwnerReferences, updated.OwnerReferences) {
		return 0, nil
	}
	// The update carries the resourceVersion that was read: it fails when the
	// PodGroup changed since, and the next round decides again.
	if _, err = client.Update(ctx, updated, metav1.UpdateOptions{}); err != nil {
		return 0, fmt.Errorf("update PodGroup %s: %w", name, err)
	}
	klog.V(2).Infof("Updated PodGroup %s/%s", ms.Namespace, name)
	return 0, nil
}

// applyPodGroupSpec sets what a PodGroup takes from ms. The queue the server
// assigned is kept unless ms names one.
func (m *Manager) applyPodGroupSpec(ms *workloadv1alpha1.ModelServing, podGroup *schedulingv1beta1.PodGroup) {
	minMember, minRoleMember, minResources := m.calculateRequirements(ms)
	podGroup.Spec.MinMember = int32(minMember)
	podGroup.Spec.MinResources = &minResources
	if queue := extractQueueName(ms); queue != "" {
		podGroup.Spec.Queue = queue
	}
	syncPodGroupNetworkTopology(ms, &podGroup.Spec)
	if m.hasSubGroupPolicy.Load() {
		appendSubGroupPolicy(ms, podGroup, minRoleMember)
	}
}

// withController returns references with owner as the only controller.
// References of other owners that are not controllers are kept.
func withController(references []metav1.OwnerReference, owner metav1.OwnerReference) []metav1.OwnerReference {
	out := []metav1.OwnerReference{owner}
	for _, reference := range references {
		if reference.UID != owner.UID && (reference.Controller == nil || !*reference.Controller) {
			out = append(out, reference)
		}
	}
	return out
}
