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
	"time"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

func recoveryGraceSeconds(ms *workloadv1alpha1.ModelServing) int64 {
	if ms.Spec.Template.RestartGracePeriodSeconds == nil {
		return 0
	}
	return *ms.Spec.Template.RestartGracePeriodSeconds
}

func podUnavailabilityReason(pod *corev1.Pod) string {
	switch {
	case pod.DeletionTimestamp != nil:
		return "PodTerminating"
	case utils.IsPodFailed(pod):
		return "PodFailed"
	case utils.IsPodRunningAndReady(pod):
		return "PodRemoved"
	case utils.ContainerRestarted(pod):
		return "ContainerRestartNotReady"
	default:
		return "PodNotReady"
	}
}

func logRecoveryScheduled(ctx context.Context, ms *workloadv1alpha1.ModelServing, pod *corev1.Pod, started time.Time) {
	modelServingContextLog(ctx, ms).Info("Pod recovery scheduled", "action", "recovery", "reason", podUnavailabilityReason(pod), "pod", klog.KObj(pod), "podUID", pod.UID, "servingGroup", pod.Labels[workloadv1alpha1.GroupNameLabelKey], "role", utils.GetRoleName(pod), "roleID", utils.GetRoleID(pod), "recoveryPolicy", ms.Spec.RecoveryPolicy, "graceSeconds", recoveryGraceSeconds(ms), "observedAt", started)
}

// Capture only identity while the episode lock is held. Logging happens after
// unlocking and never participates in the recovery state transition.
type podRecoveryCancellation struct {
	owner  ownerInputKey
	pod    types.NamespacedName
	podUID types.UID
	reason string
}

func recoveryCancellation(pod *corev1.Pod, reason string) *podRecoveryCancellation {
	owner, _ := childOwnerKey(pod)
	return &podRecoveryCancellation{owner: owner, pod: utils.GetNamespaceName(pod), podUID: pod.UID, reason: reason}
}

func (event *podRecoveryCancellation) log() {
	if event != nil {
		klog.InfoS("Pod recovery canceled", "entrypoint", "podObservation", "action", "recovery", "modelServing", klog.KRef(event.owner.Namespace, event.owner.Name), "uid", event.owner.UID, "pod", klog.KRef(event.pod.Namespace, event.pod.Name), "podUID", event.podUID, "reason", event.reason)
	}
}
