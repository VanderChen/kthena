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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Episodes are short observations, not lifecycle state. The mutex never spans
// API calls, plugins or acquisition of the ModelServing lifecycle lock.
// graceMap remains the existing timestamp view used by the timer.
type podEpisodes struct {
	mu     sync.Mutex
	next   uint64
	byName map[types.NamespacedName]*podEpisode
}

type podEpisode struct {
	owner       types.UID
	pod         types.UID
	version     uint64
	observation uint64
	started     time.Time
	active      bool
	armed       bool
}

// observePodEpisode reads the cache inside the short critical section: a queued
// old Ready frame must not clear a newer fault already visible in the cache.
func (c *ModelServingController) observePodEpisode(pod *corev1.Pod) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	latest, err := c.podsLister.Pods(pod.Namespace).Get(pod.Name)
	if err != nil {
		if apierrors.IsNotFound(err) {
			c.cancelPodEpisodeLocked(pod)
		}
		return
	}
	ms, _, err := c.getModelServingByChildResource(latest)
	if err != nil || !utils.IsOwnedByModelServingWithUID(latest, ms.UID) {
		c.cancelPodEpisodeLocked(pod)
		return
	}
	c.observePodEpisodeLocked(ms, latest)
}

// A live read may update observations only if no callback accepted a newer
// observation while that read was in flight. Episode identity is separate from
// this observation version so repeated fault updates do not cancel a timer.
func (c *ModelServingController) capturePodObservation(pod *corev1.Pod) uint64 {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	if episode := c.episodes.byName[utils.GetNamespaceName(pod)]; episode != nil {
		return episode.observation
	}
	return 0
}

func (c *ModelServingController) observeLivePodEpisode(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod, observed uint64) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	current := uint64(0)
	if episode := c.episodes.byName[utils.GetNamespaceName(pod)]; episode != nil {
		current = episode.observation
	}
	if current == observed && utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
		c.observePodEpisodeLocked(ms, pod)
	}
}

func (c *ModelServingController) observePodEpisodeLocked(ms *workloadv1alpha1.ModelServing, latest *corev1.Pod) {
	name := utils.GetNamespaceName(latest)
	if c.episodes.byName == nil {
		c.episodes.byName = make(map[types.NamespacedName]*podEpisode)
	}
	episode := c.episodes.byName[name]
	if episode == nil || episode.owner != ms.UID || episode.pod != latest.UID {
		if episode != nil {
			c.graceMap.Delete(podGracePeriodKey{NamespacedName: name, UID: episode.pod})
		}
		c.episodes.next++
		episode = &podEpisode{owner: ms.UID, pod: latest.UID, version: c.episodes.next}
		c.episodes.byName[name] = episode
	}
	c.episodes.next++
	episode.observation = c.episodes.next
	if latest.DeletionTimestamp != nil || ms.DeletionTimestamp != nil || podRecoveryDisabled(ms) || utils.IsPodRunningAndReady(latest) {
		c.cancelPodEpisodeLocked(latest)
		return
	}
	if !utils.IsPodFailed(latest) && !utils.ContainerRestarted(latest) {
		return
	}
	if !episode.active {
		c.episodes.next++
		episode.version = c.episodes.next
		episode.observation = c.episodes.next
		now := time.Now()
		if !now.After(episode.started) {
			// The timestamp also identifies old timer invocations. Keep it
			// distinct even on clocks whose reads have coarse resolution.
			now = episode.started.Add(time.Nanosecond)
		}
		episode.started = now
		episode.active = true
		episode.armed = false
	}
	c.graceMap.Store(getPodGracePeriodKey(latest), episode.started)
}

func (c *ModelServingController) cancelPodEpisodeLocked(pod *corev1.Pod) {
	c.graceMap.Delete(getPodGracePeriodKey(pod))
	if episode := c.episodes.byName[utils.GetNamespaceName(pod)]; episode != nil && episode.pod == pod.UID {
		c.episodes.next++
		episode.version = c.episodes.next
		episode.observation = c.episodes.next
		episode.active = false
		episode.armed = false
	}
}

func (c *ModelServingController) forgetPodEpisode(pod *corev1.Pod) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	c.cancelPodEpisodeLocked(pod)
	if episode := c.episodes.byName[utils.GetNamespaceName(pod)]; episode != nil && episode.pod == pod.UID {
		delete(c.episodes.byName, utils.GetNamespaceName(pod))
	}
}

// Policy disablement ends existing episodes even if a later enablement is
// coalesced into the same worker turn. Inactive records prevent an older worker
// snapshot from arming the just-cancelled episode.
func (c *ModelServingController) forgetModelServingEpisodes(ms *workloadv1alpha1.ModelServing) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	for name, episode := range c.episodes.byName {
		if episode.owner != ms.UID {
			continue
		}
		c.graceMap.Delete(podGracePeriodKey{NamespacedName: name, UID: episode.pod})
		c.episodes.next++
		episode.version = c.episodes.next
		episode.observation = c.episodes.next
		episode.active = false
		episode.armed = false
	}
}

// Only owner retirement removes inactive records. It cannot remove a same-name
// replacement owner's observations.
func (c *ModelServingController) retireModelServingEpisodes(ms *workloadv1alpha1.ModelServing) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	for name, episode := range c.episodes.byName {
		if episode.owner == ms.UID {
			c.graceMap.Delete(podGracePeriodKey{NamespacedName: name, UID: episode.pod})
			delete(c.episodes.byName, name)
		}
	}
}

// armPodEpisode is called by the original locked business path. Observation
// and arming are separate so a callback's timestamp never suppresses its timer.
func (c *ModelServingController) armPodEpisode(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) (time.Time, bool) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	if podRecoveryDisabled(ms) {
		c.cancelPodEpisodeLocked(pod)
		return time.Time{}, false
	}
	name := utils.GetNamespaceName(pod)
	episode := c.episodes.byName[name]
	if episode == nil {
		// The original synchronous bootstrap and direct recovery paths may
		// schedule before a callback has observed this Pod.
		started := time.Now()
		if previous, ok := c.graceMap.Load(getPodGracePeriodKey(pod)); ok {
			started = previous.(time.Time)
		}
		if c.episodes.byName == nil {
			c.episodes.byName = make(map[types.NamespacedName]*podEpisode)
		}
		c.episodes.next++
		episode = &podEpisode{owner: ms.UID, pod: pod.UID, version: c.episodes.next, observation: c.episodes.next, started: started, active: true}
		c.episodes.byName[name] = episode
	}
	if episode.owner != ms.UID || episode.pod != pod.UID || !episode.active || episode.armed {
		return time.Time{}, false
	}
	episode.armed = true
	c.graceMap.Store(getPodGracePeriodKey(pod), episode.started)
	return episode.started, true
}

// capturePodEpisode precedes live reads. Only this same observation may be
// selected for deletion after those reads. A nil token retains compatibility
// with the original timestamp-only bootstrap/direct timer callers.
func (c *ModelServingController) capturePodEpisode(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod, started time.Time) (*podEpisode, bool) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	current, ok := c.graceMap.Load(getPodGracePeriodKey(pod))
	if !ok || current != started {
		return nil, false
	}
	episode := c.episodes.byName[utils.GetNamespaceName(pod)]
	if episode == nil {
		return nil, true
	}
	if episode.owner != ms.UID || episode.pod != pod.UID || !episode.active || episode.started != started {
		return nil, false
	}
	snapshot := *episode
	return &snapshot, true
}

func (c *ModelServingController) podEpisodeCurrent(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod, started time.Time, token *podEpisode) bool {
	current, ok := c.capturePodEpisode(ms, pod, started)
	if !ok {
		return false
	}
	if token == nil || current == nil {
		return token == nil && current == nil
	}
	return token.version == current.version
}

func (c *ModelServingController) finishPodEpisode(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod, started time.Time) {
	c.episodes.mu.Lock()
	defer c.episodes.mu.Unlock()
	c.graceMap.CompareAndDelete(getPodGracePeriodKey(pod), started)
	episode := c.episodes.byName[utils.GetNamespaceName(pod)]
	if episode != nil && episode.owner == ms.UID && episode.pod == pod.UID && episode.started == started {
		c.episodes.next++
		episode.version = c.episodes.next
		episode.observation = c.episodes.next
		episode.active = false
		episode.armed = false
	}
}
