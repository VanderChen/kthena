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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func episodeFault(pod *corev1.Pod) *corev1.Pod {
	fault := pod.DeepCopy()
	fault.Status.Conditions[0].Status = corev1.ConditionFalse
	fault.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: 1}}
	return fault
}

func TestPodEpisodeObservedFaultStillArmsTimer(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
	c, _ := recoveryController(t, ms, pods...)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	observed, ok := c.graceMap.Load(getPodGracePeriodKey(fault))
	require.True(t, ok)
	started, armed := c.armPodEpisode(ms, fault)
	require.True(t, armed, "a callback timestamp is not an already running timer")
	require.Equal(t, observed, started)
	_, armed = c.armPodEpisode(ms, fault)
	require.False(t, armed, "one timer per episode")
	c.observePodEpisode(fault)
	again, _ := c.graceMap.Load(getPodGracePeriodKey(fault))
	require.Equal(t, observed, again, "repeated faults keep their first observation")
}

func TestPodEpisodeReadyAndNewFaultRejectOldSelection(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	c, _ := recoveryController(t, ms, pods...)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	started, armed := c.armPodEpisode(ms, fault)
	require.True(t, armed)
	token, ok := c.capturePodEpisode(ms, fault, started)
	require.True(t, ok)
	c.observePodEpisode(fault)
	require.True(t, c.podEpisodeCurrent(ms, fault, started, token), "same-episode updates must not starve recovery")
	updateRecoveryPod(t, c, pods[0])
	c.observePodEpisode(pods[0])
	_, armed = c.armPodEpisode(ms, fault)
	require.False(t, armed, "old worker fault snapshot cannot undo Ready")
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	newStart, armed := c.armPodEpisode(ms, fault)
	require.True(t, armed)
	require.False(t, c.podEpisodeCurrent(ms, fault, started, token))
	c.finishPodEpisode(ms, fault, started)
	actual, ok := c.graceMap.Load(getPodGracePeriodKey(fault))
	require.True(t, ok)
	require.Equal(t, newStart, actual, "old task must not retire the new task")
}

func TestPodEpisodeStaleReadyFrameAndLiveReadDoNotResetNewObservation(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	c, _ := recoveryController(t, ms, pods...)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	readVersion := c.capturePodObservation(fault)
	// A newer fault callback is accepted while an earlier live Ready read is in flight.
	c.observePodEpisode(fault)
	c.observeLivePodEpisode(ms, pods[0], readVersion)
	c.observePodEpisode(pods[0]) // stale Ready notification, current cache is fault
	_, armed := c.armPodEpisode(ms, fault)
	require.True(t, armed)
	current := c.capturePodObservation(fault)
	c.observeLivePodEpisode(ms, pods[0], current)
	_, waiting := c.graceMap.Load(getPodGracePeriodKey(fault))
	require.False(t, waiting, "accepted current live Ready must retain original reset semantics")
}

func TestPodEpisodePolicyUIDAndRetirement(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	c, _ := recoveryController(t, ms, pods...)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	started, _ := c.armPodEpisode(ms, fault)
	token, _ := c.capturePodEpisode(ms, fault, started)
	c.forgetModelServingEpisodes(ms)
	require.False(t, c.podEpisodeCurrent(ms, fault, started, token))
	_, armed := c.armPodEpisode(ms, fault)
	require.False(t, armed)
	c.observePodEpisode(fault) // latest enabled policy begins a fresh episode
	newStart, armed := c.armPodEpisode(ms, fault)
	require.True(t, armed)
	require.NotEqual(t, started, newStart)
	replacement := fault.DeepCopy()
	replacement.UID = "replacement"
	require.NoError(t, c.podsInformer.GetIndexer().Update(replacement))
	c.observePodEpisode(replacement)
	_, old := c.graceMap.Load(getPodGracePeriodKey(fault))
	require.False(t, old)
	c.forgetPodEpisode(fault)
	_, current := c.graceMap.Load(getPodGracePeriodKey(replacement))
	require.True(t, current, "late old DELETE cannot cancel a new UID")
	c.retireModelServingEpisodes(ms)
	require.Empty(t, c.episodes.byName)
	_, current = c.graceMap.Load(getPodGracePeriodKey(replacement))
	require.False(t, current)
}

func TestPodEpisodeTerminatingAndDisabledDoNotArm(t *testing.T) {
	for _, state := range []string{"terminating", "none", "infinite", "plain-notready"} {
		t.Run(state, func(t *testing.T) {
			ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
			c, _ := recoveryController(t, ms, pods...)
			fault := episodeFault(pods[0])
			switch state {
			case "terminating":
				now := metav1.Now()
				fault.DeletionTimestamp = &now
			case "none":
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			case "infinite":
				ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
			case "plain-notready":
				fault.Status.ContainerStatuses = nil
			}
			require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms))
			require.NoError(t, c.podsInformer.GetIndexer().Update(fault))
			c.observePodEpisode(fault)
			_, arm := c.armPodEpisode(ms, fault)
			require.False(t, arm)
		})
	}
}

func TestPodEpisodeSecondCheckAfterLiveGet(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	c, kube := recoveryController(t, ms, pods...)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	started, _ := c.armPodEpisode(ms, fault)
	kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		// The GET response is the old fault, while current observations complete
		// Ready -> fault. Both states use the same Pod UID and resourceVersion
		// in this fake; token comparison must independently reject old permission.
		require.NoError(t, c.podsInformer.GetIndexer().Update(pods[0]))
		c.observePodEpisode(pods[0])
		require.NoError(t, c.podsInformer.GetIndexer().Update(fault))
		c.observePodEpisode(fault)
		return true, fault.DeepCopy(), nil
	})
	_, err := c.recoverPodAfterGrace(context.Background(), ms, fault, started)
	require.NoError(t, err)
	require.Empty(t, recoveryDeletedPods(kube), "old live result cannot authorize deleting a later episode")
}

func TestPodEpisodeObservedTimestampActuallyStartsProductionTimer(t *testing.T) {
	ms, pods := recoveryFixture(workloadv1alpha1.RoleRecreate, 0)
	c, kube := recoveryController(t, ms, pods...)
	ctx, cancel := context.WithCancel(context.Background())
	c.recoveryCtx = ctx
	t.Cleanup(cancel)
	fault := episodeFault(pods[0])
	updateRecoveryPod(t, c, fault)
	c.observePodEpisode(fault)
	require.NoError(t, c.schedulePodRecovery(ctx, ms, fault))
	require.Eventually(t, func() bool { return len(recoveryDeletedPods(kube)) == 1 }, 2*time.Second, 10*time.Millisecond)
}
