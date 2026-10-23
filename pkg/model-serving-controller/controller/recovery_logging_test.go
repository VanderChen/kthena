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
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func recoveryLogsWith(capture *logCapture, field string, value any) []capturedLog {
	var selected []capturedLog
	for _, entry := range capture.entries() {
		if entry.Fields[field] == value {
			selected = append(selected, entry)
		}
	}
	return selected
}

func TestRecoveryLoggingReadyTransitionKeepsRepeatedEnqueue(t *testing.T) {
	ms, pods := recoveryFixture(api.RoleRecreate, 0)
	c, _ := recoveryController(t, ms, pods...)
	key := utils.GetNamespaceName(ms)
	require.NoError(t, c.store.UpdateRoleStatus(key, "ms-0", "prefill", "prefill-0", datastore.RoleCreating))
	require.NoError(t, c.store.UpdateServingGroupStatus(key, "ms-0", datastore.ServingGroupCreating))
	capture := captureModelServingLogs(t, 2)
	for i := 0; i < 2; i++ {
		for c.workqueue.Len() > 0 {
			item, _ := c.workqueue.Get()
			c.workqueue.Done(item)
			c.workqueue.Forget(item)
		}
		require.NoError(t, c.handleObservedReadyPod(ms, "ms-0", pods[0]))
		require.Equal(t, 1, c.workqueue.Len(), "repeated Ready notifications still enqueue the owner")
	}
	transitions := recoveryLogsWith(capture, "outcome", "becameAvailable")
	require.Len(t, transitions, 2, "one Role and one ServingGroup transition, no repeated Running noise")
	for _, entry := range transitions {
		require.Zero(t, entry.Level)
		require.Equal(t, ms.UID, entry.Fields["uid"])
		require.Equal(t, "ms-0", entry.Fields["servingGroup"])
	}
}

func TestRecoveryLoggingScheduleAndCancellationAreEpisodeBoundaries(t *testing.T) {
	ms, pods := recoveryFixture(api.RoleRecreate, 0)
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
	c, kube := newGracePeriodTestController(t, ms, pods[0])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.recoveryCtx = ctx
	capture := captureModelServingLogs(t, 2)
	failed := pods[0].DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	failed.Status.Conditions[0].Status = corev1.ConditionFalse
	require.NoError(t, c.schedulePodRecovery(ctx, ms, failed))
	started, ok := c.graceMap.Load(getPodGracePeriodKey(failed))
	require.True(t, ok)
	require.NoError(t, c.schedulePodRecovery(ctx, ms, failed))
	again, _ := c.graceMap.Load(getPodGracePeriodKey(failed))
	require.Equal(t, started, again)
	episode, current := c.capturePodEpisode(ms, failed, started.(time.Time))
	require.True(t, current)
	for i := 0; i < 2; i++ {
		c.observeLivePodEpisode(ms, pods[0], c.capturePodObservation(pods[0]))
	}
	_, pending := c.graceMap.Load(getPodGracePeriodKey(failed))
	require.False(t, pending, "Ready observation ends the actual episode")
	require.False(t, c.podEpisodeCurrent(ms, failed, started.(time.Time), episode), "cancellation invalidates the old timer")
	cancel()
	require.Len(t, recoveryLogsWith(capture, "observedAt", started), 1)
	require.Len(t, recoveryLogsWith(capture, "reason", "PodReady"), 1)
	require.Empty(t, kube.Actions(), "logging schedules no API work")
}

func TestRecoveryCancellationLogsFollowEpisodeInvalidation(t *testing.T) {
	for _, reason := range []string{"PodReady", "PodTerminating", "RecoveryDisabled", "PodDeleted", "ModelServingDeleted", "PodIdentityChanged"} {
		t.Run(reason, func(t *testing.T) {
			ms, pods := recoveryFixture(api.RoleRecreate, 0)
			failed := pods[0].DeepCopy()
			failed.Status.Phase = corev1.PodFailed
			failed.Status.Conditions[0].Status = corev1.ConditionFalse
			c, kube := newGracePeriodTestController(t, ms, failed)
			c.observeLivePodEpisode(ms, failed, c.capturePodObservation(failed))
			started, armed := c.armPodEpisode(ms, failed)
			require.True(t, armed)
			episode, current := c.capturePodEpisode(ms, failed, started)
			require.True(t, current)
			capture := captureModelServingLogs(t, 2)
			for i := 0; i < 2; i++ {
				switch reason {
				case "PodReady":
					c.observeLivePodEpisode(ms, pods[0], c.capturePodObservation(pods[0]))
				case "PodTerminating":
					terminating := failed.DeepCopy()
					now := metav1.Now()
					terminating.DeletionTimestamp = &now
					c.observeLivePodEpisode(ms, terminating, c.capturePodObservation(terminating))
				case "RecoveryDisabled":
					disabled := ms.DeepCopy()
					disabled.Spec.RecoveryPolicy = api.NoneRestartPolicy
					c.forgetModelServingEpisodes(disabled)
				case "PodDeleted":
					c.forgetPodEpisode(failed)
				case "ModelServingDeleted":
					c.retireModelServingEpisodes(ms)
				case "PodIdentityChanged":
					replacement := failed.DeepCopy()
					replacement.UID = "replacement"
					c.observeLivePodEpisode(ms, replacement, c.capturePodObservation(replacement))
				}
			}
			require.False(t, c.podEpisodeCurrent(ms, failed, started, episode))
			_, pending := c.graceMap.Load(getPodGracePeriodKey(failed))
			require.False(t, pending)
			cancellations := recoveryLogsWith(capture, "reason", reason)
			require.Len(t, cancellations, 1, "only the actual episode boundary emits a cancellation")
			require.Equal(t, ms.UID, cancellations[0].Fields["uid"])
			require.Equal(t, failed.UID, cancellations[0].Fields["podUID"])
			require.Empty(t, kube.Actions(), "episode logging adds no API work")
		})
	}
}

func TestRecoveryCancellationLoggingIgnoresStaleReadyObservation(t *testing.T) {
	ms, pods := recoveryFixture(api.RoleRecreate, 0)
	failed := pods[0].DeepCopy()
	failed.Status.Phase = corev1.PodFailed
	failed.Status.Conditions[0].Status = corev1.ConditionFalse
	c, kube := newGracePeriodTestController(t, ms, failed)
	c.observeLivePodEpisode(ms, failed, c.capturePodObservation(failed))
	started, armed := c.armPodEpisode(ms, failed)
	require.True(t, armed)
	episode, current := c.capturePodEpisode(ms, failed, started)
	require.True(t, current)
	staleObservation := c.capturePodObservation(failed)
	c.observeLivePodEpisode(ms, failed, staleObservation)
	capture := captureModelServingLogs(t, 2)
	c.observeLivePodEpisode(ms, pods[0], staleObservation)
	require.True(t, c.podEpisodeCurrent(ms, failed, started, episode))
	require.Empty(t, recoveryLogsWith(capture, "reason", "PodReady"))
	require.Empty(t, kube.Actions())
}

func TestRecoveryLoggingDeleteAcceptanceMatchesAPIOutcome(t *testing.T) {
	for _, outcome := range []string{"accepted", "forbidden", "replacement"} {
		t.Run(outcome, func(t *testing.T) {
			ms, pods := recoveryFixture(api.RoleRecreate, 0)
			failed := pods[0].DeepCopy()
			failed.Status.Phase = corev1.PodFailed
			failed.Status.Conditions[0].Status = corev1.ConditionFalse
			c, kube := newGracePeriodTestController(t, ms, failed)
			if outcome == "forbidden" {
				kube.PrependReactor("delete", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, failed.Name, fmt.Errorf("test denial"))
				})
			}
			if outcome == "replacement" {
				replacement := failed.DeepCopy()
				replacement.UID = "replacement"
				require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
			}
			capture := captureModelServingLogs(t, 2)
			started := time.Now().Add(-time.Minute)
			c.graceMap.Store(getPodGracePeriodKey(failed), started)
			delay, err := c.recoverPodAfterGrace(context.Background(), ms, failed, started)
			require.Zero(t, delay)
			if outcome == "forbidden" {
				require.True(t, apierrors.IsForbidden(err))
			} else {
				require.NoError(t, err)
			}
			accepted := recoveryLogsWith(capture, "outcome", "requestAccepted")
			if outcome == "accepted" {
				require.Len(t, accepted, 1)
				require.Equal(t, failed.UID, accepted[0].Fields["podUID"])
				require.Len(t, recoveryDeletedPods(kube), 1)
			} else {
				require.Empty(t, accepted, "failure and UID replacement must never claim accepted recovery")
			}
			if outcome == "replacement" {
				require.Empty(t, recoveryDeletedPods(kube))
			}
		})
	}
}

func TestDeletionLoggingCountsAcceptedRequestsOnly(t *testing.T) {
	for _, outcome := range []string{"accepted", "absent", "terminating", "replacement", "forbidden"} {
		t.Run(outcome, func(t *testing.T) {
			ms, pods := recoveryFixture(api.RoleRecreate, 0)
			c, kube := newGracePeriodTestController(t, ms, pods[0])
			pod := pods[0].DeepCopy()
			switch outcome {
			case "absent":
				require.NoError(t, kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), ms.Namespace, pod.Name))
			case "terminating":
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, ms.Namespace))
			case "replacement":
				pod.UID = "replacement"
				require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), pod, ms.Namespace))
			case "forbidden":
				kube.PrependReactor("delete", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, pod.Name, fmt.Errorf("test denial"))
				})
			}
			capture := captureModelServingLogs(t, 2)
			plan := &podDeletionPlan{Key: deletionPlanKey(ms, deleteRoleScope, "ms-0", "prefill", "prefill-0"), Pods: []corev1.Pod{*pods[0]}}
			err := c.deletePlannedPods(context.Background(), ms, plan)
			if outcome == "forbidden" {
				require.True(t, apierrors.IsForbidden(err))
			} else {
				require.NoError(t, err)
			}
			accepted := recoveryLogsWith(capture, "outcome", "requestAccepted")
			if outcome == "accepted" {
				require.Len(t, accepted, 1)
				require.Equal(t, 1, accepted[0].Fields["acceptedPods"])
				require.Equal(t, deleteRoleScope, accepted[0].Fields["scope"])
				require.NoError(t, c.deletePlannedPods(context.Background(), ms, plan))
				require.Len(t, recoveryLogsWith(capture, "outcome", "requestAccepted"), 1, "observing an absent UID during retry is not another accepted deletion")
			} else {
				require.Empty(t, accepted)
			}
		})
	}
}

func TestRecoveryLoggingIdentityLookupErrorsRemainVisible(t *testing.T) {
	ms, pods := recoveryFixture(api.RoleRecreate, 0)
	ms.ResourceVersion = "1"
	c, kube := newGracePeriodTestController(t, ms, pods[0])
	kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, fmt.Errorf("identity lookup unavailable")
	})
	capture := captureModelServingLogs(t, 0)
	require.Nil(t, c.currentPodEvent(pods[0]), "failed lookup still rejects the event")
	done, err := c.processPodDeletionLocked(context.Background(), ms, &deletedPodInput{pod: pods[0]})
	require.False(t, done, "failed lookup retains the DELETE input for retry")
	require.ErrorContains(t, err, "identity lookup unavailable", "the worker receives the lookup failure")
	var errors []capturedLog
	for _, entry := range capture.entries() {
		if entry.Err != nil {
			errors = append(errors, entry)
			require.Equal(t, pods[0].UID, entry.Fields["podUID"])
		}
	}
	require.Len(t, errors, 1, "the synchronous observation logs locally; deletion propagates its error to the worker")
	require.Empty(t, recoveryDeletedPods(kube))
}
