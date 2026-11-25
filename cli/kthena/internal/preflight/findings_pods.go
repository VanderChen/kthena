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

package preflight

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// podStates reports the Pods that something is happening to: Pods that are
// terminating, have failed, keep restarting or are being updated in place.
// The state of a Pod is the one the ServingGroup controller will read.
func (a *analysis) podStates() {
	ms := a.in.ms
	recovery := servinggroup.RecoveryOf(ms.Spec.RecoveryPolicy, ms.Spec.Template.RestartGracePeriodSeconds)
	var terminating, failed, restarting, updating []*corev1.Pod
	var gone []string
	for _, g := range a.groups {
		terminating = append(terminating, g.terminating...)
		if len(g.live) == 0 {
			gone = append(gone, g.name)
		}
		for _, pod := range g.live {
			state, _ := servinggroup.ClassifyMember(pod, servinggroup.MemberContext{Now: a.in.now, Recovery: recovery})
			switch {
			case state == servinggroup.MemberUpdating:
				updating = append(updating, pod)
			case state.HasFault() && utils.IsPodFailed(pod):
				failed = append(failed, pod)
			case state.HasFault():
				restarting = append(restarting, pod)
			}
		}
	}
	// A terminating Pod that is not a member of a group goes away as well.
	for _, pod := range a.strays {
		if owner := metav1.GetControllerOfNoCopy(pod); pod.DeletionTimestamp != nil && (owner == nil || owner.UID == ms.UID) {
			terminating = append(terminating, pod)
		}
	}

	if len(terminating) > 0 {
		names := podNames(terminating)
		message := fmt.Sprintf("%s terminating (%s). A terminating Pod is not taken over; its place is filled after it is gone.",
			counted(len(names), "1 Pod is", "%d Pods are"), listed(names))
		if len(gone) > 0 {
			message += fmt.Sprintf(" Every Pod of %s is terminating: no ServingGroup object is created for such a group, and its PodGroup and Services are deleted and created again with the group.", listed(gone))
		}
		a.add(Finding{ID: FindingTerminatingPods, Level: LevelWait, changesPods: true, Objects: names,
			Message: message, Action: "Wait until the Pods are gone."})
	}

	// scope names what the recovery policy recreates because of one Pod.
	scope := func(pod *corev1.Pod) string {
		if ms.Spec.RecoveryPolicy == workloadv1alpha1.ServingGroupRecreate {
			return "group " + pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		}
		return fmt.Sprintf("Role instance %s of group %s", pod.Labels[workloadv1alpha1.RoleIDKey], pod.Labels[workloadv1alpha1.GroupNameLabelKey])
	}
	faulty := func(id, what string, pods []*corev1.Pod) {
		if len(pods) == 0 {
			return
		}
		if !recovery.Enabled {
			a.add(Finding{ID: id, Level: LevelAttention, Objects: podNames(pods),
				Message: fmt.Sprintf("%s: %s. Recovery is disabled (recoveryPolicy None, or restartGracePeriodSeconds -1): such a Pod is tolerated as it is, before and after the upgrade, and nothing is recreated because of it.",
					what, listed(podNames(pods)))})
			return
		}
		var described []string
		for _, pod := range pods {
			described = append(described, fmt.Sprintf("%s (%s is recreated)", pod.Name, scope(pod)))
		}
		a.add(Finding{ID: id, Level: LevelWait, changesPods: true, Objects: podNames(pods),
			Message: fmt.Sprintf("%s: %s. recoveryPolicy is %s: the running version recreates that scope once the restart grace period has passed. When the upgrade comes first, the new version starts the grace period again from the moment it first sees the Pod, and then recreates the same scope.",
				what, listed(described), a.report.Rollout.RecoveryPolicy),
			Action: "Wait until the recovery has finished, or delete the Pod so that only it is created again."})
	}
	faulty(FindingFailedPods, "Pods in phase Failed", failed)
	faulty(FindingRestartingPods, "Pods that are not ready and whose containers restarted", restarting)

	if len(updating) > 0 {
		names := podNames(updating)
		a.add(Finding{ID: FindingInPlaceUpdate, Level: LevelWait, changesPods: true, Objects: names,
			Message: fmt.Sprintf("An in-place update is in progress on %s (%s). The new version reads its progress from the annotation %s and continues it; the containers that are updated restart.",
				counted(len(names), "1 Pod", "%d Pods"), listed(names), workloadv1alpha1.InPlaceUpdateStateAnnotationKey),
			Action: "Wait until the update has finished."})
	}
}

// strayPods reports the Pods that carry the name label of the ModelServing
// and are not members of a group, and the members that nothing controls.
func (a *analysis) strayPods() {
	ms := a.in.ms
	var notMembers, unowned, foreign, adopted []*corev1.Pod
	for _, pod := range a.strays {
		owner := metav1.GetControllerOfNoCopy(pod)
		switch {
		case owner != nil && owner.UID != ms.UID:
			foreign = append(foreign, pod)
		case pod.DeletionTimestamp != nil:
		case owner == nil:
			unowned = append(unowned, pod)
		default:
			notMembers = append(notMembers, pod)
		}
	}
	for _, g := range a.groups {
		for _, pod := range g.live {
			if metav1.GetControllerOfNoCopy(pod) == nil {
				adopted = append(adopted, pod)
			}
		}
	}

	if len(notMembers) > 0 {
		names := podNames(notMembers)
		a.add(Finding{ID: FindingPodNotMember, Level: LevelBlocked, Objects: names,
			Message: fmt.Sprintf("%s controlled by the ModelServing and not a member of any of its groups (%s): the labels and the name are not those of a member, <group>-<role>-<instance>-<member>. No ServingGroup takes such a Pod over, and the takeover of the ModelServing is not recorded as done while a Pod that the ModelServing controls remains. Until then the ModelServing is neither rolled nor scaled.",
				counted(len(names), "1 Pod is", "%d Pods are"), listed(names)),
			Action: "Delete these Pods, or remove their owner reference, before upgrading."})
	}
	if total := len(unowned) + len(adopted); total > 0 {
		var parts []string
		if len(adopted) > 0 {
			parts = append(parts, fmt.Sprintf("%s the name of a member of a group and adopted by the ServingGroup of that group during the takeover (%s)",
				counted(len(adopted), "1 has", "%d have"), listed(podNames(adopted))))
		}
		if len(unowned) > 0 {
			parts = append(parts, fmt.Sprintf("%s not the name of a member and neither taken over nor deleted; the new controller reports them in an event (%s)",
				counted(len(unowned), "1 has", "%d have"), listed(podNames(unowned))))
		}
		a.add(Finding{ID: FindingPodWithoutOwner, Level: LevelAttention, Objects: podNames(append(adopted, unowned...)),
			Message: fmt.Sprintf("%s the labels of the ModelServing and no controller owner; the running version does not manage such a Pod. Of these, %s.",
				counted(total, "1 Pod carries", "%d Pods carry"), strings.Join(parts, "; ")),
			Action: "Decide what these Pods are for, and delete the ones that are not wanted."})
	}
	if len(foreign) > 0 {
		names := podNames(foreign)
		a.add(Finding{ID: FindingPodForeignOwner, Level: LevelAttention, Objects: names,
			Message: fmt.Sprintf("%s the labels of the ModelServing and controlled by another object (%s). Such a Pod is not taken over. When it has the name of a member, that member cannot be created.",
				counted(len(names), "1 Pod carries", "%d Pods carry"), listed(names)),
			Action: "Delete these Pods, or remove the labels."})
	}
}
