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

package mscontroller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// RollbackSwitch is the name of the ConfigMap, in the namespace of the
// controller manager, whose existence puts the controllers into rollback
// mode: instead of reconciling, they hand every workload back to its
// ModelServing, in the shape a controller manager from before ServingGroup
// objects expects, and delete the ServingGroup objects without their Pods.
// When every ModelServing is done, that earlier version can be installed.
// Deleting the ConfigMap before then abandons the rollback: the workloads are
// taken over again.
const RollbackSwitch = "kthena-servinggroup-rollback"

// rollbackRetry is how long a rollback waits for a group that is being deleted.
const rollbackRetry = 2 * time.Second

// modelServingOwner is the controller reference an earlier version put on the
// Pods and Services of a ModelServing.
func modelServingOwner(ms *workloadv1alpha1.ModelServing, blockOwnerDeletion bool) metav1.OwnerReference {
	owner := metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(),
		Kind:       workloadv1alpha1.ModelServingKind.Kind,
		Name:       ms.Name,
		UID:        ms.UID,
		Controller: ptr.To(true),
	}
	if blockOwnerDeletion {
		owner.BlockOwnerDeletion = ptr.To(true)
	}
	return owner
}

// handedBack returns the owner references of an object that group controlled,
// with ms as the controller instead. Owners that are not controllers stay.
func handedBack(references []metav1.OwnerReference, group types.UID, owner metav1.OwnerReference) []metav1.OwnerReference {
	out := []metav1.OwnerReference{owner}
	for _, reference := range references {
		if reference.UID != group && reference.UID != owner.UID && (reference.Controller == nil || !*reference.Controller) {
			out = append(out, reference)
		}
	}
	return out
}

func controlledBy(object metav1.Object, uid types.UID) bool {
	owner := metav1.GetControllerOfNoCopy(object)
	return owner != nil && owner.UID == uid
}

// metadataPatch is a merge patch of the metadata of an object that only
// applies to the object as it was read.
func metadataPatch(object metav1.Object, metadata map[string]any) ([]byte, error) {
	metadata["uid"] = object.GetUID()
	metadata["resourceVersion"] = object.GetResourceVersion()
	return json.Marshal(map[string]any{"metadata": metadata})
}

// rollBack hands the workloads of one ModelServing back. Every step reads
// from the API server and is safe to repeat, so a rollback that was
// interrupted is continued by the next round, of this process or another.
func (c *Controller) rollBack(ctx context.Context, cached *workloadv1alpha1.ModelServing) (time.Duration, error) {
	ms, groups, ok, err := c.readDirect(ctx, cached)
	if err != nil || !ok {
		return 0, err
	}
	state := ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]
	if len(groups.own) == 0 && state == "" {
		// Never taken over, or handed back already.
		return 0, nil
	}
	// A group that is being deleted takes its Pods with it. They cannot be
	// handed back; the group has to be gone first.
	for _, group := range groups.own {
		if group.DeletionTimestamp != nil {
			klog.V(2).InfoS("The rollback waits for a ServingGroup that is being deleted", "servingGroup", klog.KObj(group))
			return rollbackRetry, nil
		}
	}
	// From here on the ServingGroup controller adopts nothing of this
	// ModelServing: it reads this annotation from the API server before it
	// changes an owner.
	if state != workloadv1alpha1.ServingGroupAdoptionRollingBack {
		patch, err := metadataPatch(ms, map[string]any{"annotations": map[string]any{
			workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionRollingBack,
		}})
		if err != nil {
			return 0, err
		}
		if ms, err = c.kthena.WorkloadV1alpha1().ModelServings(ms.Namespace).Patch(ctx, ms.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return 0, fmt.Errorf("record the rollback of ModelServing %s: %w", cached.Name, err)
		}
	}

	for _, group := range groups.own {
		if err := c.handBack(ctx, ms, group); err != nil {
			return 0, fmt.Errorf("hand back ServingGroup %s: %w", group.Name, err)
		}
	}

	// Nothing of the take-over is left on the ModelServing: an earlier version
	// only maintains the conditions it knows and would leave the others forever.
	updated := ms.DeepCopy()
	if meta.RemoveStatusCondition(&updated.Status.Conditions, string(workloadv1alpha1.ModelServingRolloutBlocked)) {
		if updated, err = c.kthena.WorkloadV1alpha1().ModelServings(ms.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{}); err != nil {
			return 0, fmt.Errorf("clear the conditions of ModelServing %s: %w", ms.Name, err)
		}
	}
	patch, err := metadataPatch(updated, map[string]any{"annotations": map[string]any{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: nil}})
	if err != nil {
		return 0, err
	}
	if _, err := c.kthena.WorkloadV1alpha1().ModelServings(ms.Namespace).Patch(ctx, ms.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return 0, fmt.Errorf("finish the rollback of ModelServing %s: %w", ms.Name, err)
	}
	c.recorder.Eventf(ms, corev1.EventTypeNormal, "RolledBack", "the workloads were handed back to the ModelServing; its ServingGroup objects are gone")
	return 0, nil
}

// handBack makes the ModelServing the controller of everything a group
// controls, checks that nothing is left, and deletes the group object alone.
func (c *Controller) handBack(ctx context.Context, ms *workloadv1alpha1.ModelServing, group *workloadv1alpha1.ServingGroup) error {
	namespace := group.Namespace
	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.GroupNameLabelKey: group.Name}).String()
	surge := group.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup

	pods, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !controlledBy(pod, group.UID) {
			continue
		}
		metadata := map[string]any{"ownerReferences": handedBack(pod.OwnerReferences, group.UID, modelServingOwner(ms, true))}
		// An earlier version tells a temporary group by the mark on its Pods.
		// The Pods carry a copy of the mark of the group; it is made to agree
		// with the group in the write that changes the owner.
		mark := pod.Annotations[workloadv1alpha1.SurgeAnnotationKey]
		switch {
		case surge && mark != workloadv1alpha1.SurgeScopeServingGroup && mark != workloadv1alpha1.SurgeScopeRole:
			metadata["annotations"] = map[string]any{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
		case !surge && mark == workloadv1alpha1.SurgeScopeServingGroup:
			metadata["annotations"] = map[string]any{workloadv1alpha1.SurgeAnnotationKey: nil}
		}
		patch, err := metadataPatch(pod, metadata)
		if err != nil {
			return err
		}
		if _, err := c.kube.CoreV1().Pods(namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("Pod %s: %w", pod.Name, err)
		}
	}

	services, err := c.kube.CoreV1().Services(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range services.Items {
		service := &services.Items[i]
		if !controlledBy(service, group.UID) {
			continue
		}
		patch, err := metadataPatch(service, map[string]any{"ownerReferences": handedBack(service.OwnerReferences, group.UID, modelServingOwner(ms, true))})
		if err != nil {
			return err
		}
		if _, err := c.kube.CoreV1().Services(namespace).Patch(ctx, service.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
			return fmt.Errorf("Service %s: %w", service.Name, err)
		}
	}

	if err := c.handBackPodGroup(ctx, ms, group); err != nil {
		return err
	}

	// Deleting the group object orphans what it still controls. An earlier
	// version does not adopt a Pod without an owner; so nothing may be left.
	pods, err = c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		if controlledBy(&pods.Items[i], group.UID) {
			return fmt.Errorf("Pod %s is still controlled by the group", pods.Items[i].Name)
		}
	}
	err = c.kthena.WorkloadV1alpha1().ServingGroups(namespace).Delete(ctx, group.Name, metav1.DeleteOptions{
		Preconditions:     &metav1.Preconditions{UID: &group.UID},
		PropagationPolicy: ptr.To(metav1.DeletePropagationOrphan),
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// handBackPodGroup hands the PodGroup of a group back. A cluster without
// Volcano has none.
func (c *Controller) handBackPodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, group *workloadv1alpha1.ServingGroup) error {
	if c.volcano == nil {
		return nil
	}
	podGroups := c.volcano.SchedulingV1beta1().PodGroups(group.Namespace)
	podGroup, err := podGroups.Get(ctx, group.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("PodGroup %s: %w", group.Name, err)
	}
	if !controlledBy(podGroup, group.UID) {
		return nil
	}
	// An earlier version creates PodGroups without blockOwnerDeletion.
	patch, err := metadataPatch(podGroup, map[string]any{"ownerReferences": handedBack(podGroup.OwnerReferences, group.UID, modelServingOwner(ms, false))})
	if err != nil {
		return err
	}
	if _, err := podGroups.Patch(ctx, podGroup.Name, types.MergePatchType, patch, metav1.PatchOptions{}); err != nil {
		return fmt.Errorf("PodGroup %s: %w", podGroup.Name, err)
	}
	return nil
}
