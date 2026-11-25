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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// adoption decides whether the round is gated: whether workloads that an
// earlier version of the controller created for the ModelServing, with the
// ModelServing itself as their owner, are still being taken over by groups.
//
// The answer is recorded in an annotation of the ModelServing. Without the
// annotation nothing is known: the ModelServing is new, or it comes from an
// earlier version. Done is recorded once a direct read shows that no Pod is
// left that the ModelServing controls, and every group has reported on the
// Pods it adopted; from then on a Pod without an owner and without the
// instance-generation label is not a Pod of this ModelServing.
//
// While it is InProgress the ModelServing is not rolled and not scaled: group
// objects are created for the groups that exist as Pods, and the ServingGroup
// controller adopts them. The counts of the status are kept as the earlier
// version wrote them, which is why the groups have to report first: a group
// that was just created has not said that it is ready.
func (c *Controller) adoption(ctx context.Context, r *round) (bool, error) {
	ms := r.ms
	// RollingBack on a ModelServing that is reconciled means that a rollback
	// was abandoned: whatever was handed back is taken over again.
	if ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionDone {
		return false, nil
	}

	legacy, err := c.legacyPods(ctx, ms)
	if err != nil {
		return true, err
	}
	pending := legacy > 0 || len(r.view.input.Orphans) > 0
	for _, g := range r.view.groups {
		pending = pending || !g.unit.Terminating && !(g.unit.Observed && g.unit.Fresh)
	}
	state := workloadv1alpha1.ServingGroupAdoptionDone
	if pending {
		state = workloadv1alpha1.ServingGroupAdoptionInProgress
	}
	if ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] != state {
		patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
			"uid":             ms.UID,
			"resourceVersion": ms.ResourceVersion,
			"annotations":     map[string]any{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: state},
		}})
		if err != nil {
			return true, err
		}
		updated, err := c.kthena.WorkloadV1alpha1().ModelServings(ms.Namespace).Patch(ctx, ms.Name, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil {
			return true, fmt.Errorf("record the adoption state %s: %w", state, err)
		}
		r.ms = updated
	}
	return pending, nil
}

// ReasonNotTakenOver is the reason of the event about a Pod that the
// ModelServing controls and that no group takes over.
const ReasonNotTakenOver = "PodNotTakenOver"

// legacyPods counts, on a direct read, the member Pods that the ModelServing
// itself controls and that are not terminating.
//
// A Pod that the ModelServing controls without being a member of one of its
// groups, a copy of a member under another name for instance, is not taken
// over by any group. It is not waited for, or the take-over would never end;
// it is left as it is, and an event says so.
func (c *Controller) legacyPods(ctx context.Context, ms *workloadv1alpha1.ModelServing) (int, error) {
	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	pods, err := c.kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return 0, fmt.Errorf("list the Pods of ModelServing %s: %w", ms.Name, err)
	}
	legacy := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if owner := metav1.GetControllerOfNoCopy(pod); owner == nil || owner.UID != ms.UID || pod.DeletionTimestamp != nil {
			continue
		}
		group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if prefix, _, ok := objcache.OrdinalName(group); ok && prefix == ms.Name && servinggroup.HasMemberIdentity(pod, ms.Name, group) {
			legacy++
			continue
		}
		c.recorder.Eventf(ms, corev1.EventTypeWarning, ReasonNotTakenOver,
			"Pod %s is controlled by the ModelServing and is not a member of one of its groups; it is not taken over and not deleted", pod.Name)
	}
	return legacy, nil
}
