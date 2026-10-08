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
	"encoding/json"
	"fmt"
	"time"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const recoveryEpisodesKey = "recovery.json"

// A recovery episode is bound to an immutable Pod UID. The containing
// ConfigMap is bound to the immutable ModelServing UID by its name and owner.
type recoveryEpisode struct {
	UID       types.UID   `json:"uid"`
	StartedAt metav1.Time `json:"startedAt"`
}

type recoveryEpisodes map[string]recoveryEpisode

func readRecoveryEpisodes(cm *corev1.ConfigMap) (recoveryEpisodes, error) {
	episodes := recoveryEpisodes{}
	if cm != nil && cm.Data[recoveryEpisodesKey] != "" {
		if err := json.Unmarshal([]byte(cm.Data[recoveryEpisodesKey]), &episodes); err != nil {
			return nil, fmt.Errorf("invalid recovery episode state: %w", err)
		}
	}
	if episodes == nil {
		episodes = recoveryEpisodes{}
	}
	for name, episode := range episodes {
		if name == "" || episode.UID == "" || episode.StartedAt.IsZero() {
			return nil, fmt.Errorf("invalid recovery episode for Pod %q", name)
		}
	}
	return episodes, nil
}

func (c *ModelServingController) writeRecoveryEpisodes(
	ctx context.Context,
	ms *api.ModelServing,
	cm *corev1.ConfigMap,
	episodes recoveryEpisodes,
) error {
	raw, err := json.Marshal(episodes)
	if err != nil {
		return err
	}
	creating := cm == nil
	if creating {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
			Name: groupMembersStateName(ms), Namespace: ms.Namespace,
			Labels:          map[string]string{api.ModelServingNameLabelKey: ms.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, api.SchemeGroupVersion.WithKind("ModelServing"))},
		}}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	if !creating && cm.Data[recoveryEpisodesKey] == string(raw) {
		return nil
	}
	cm.Data[recoveryEpisodesKey] = string(raw)
	if creating {
		_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

func (c *ModelServingController) ensureRecoveryEpisode(
	ctx context.Context,
	ms *api.ModelServing,
	pod *corev1.Pod,
	observed time.Time,
) (time.Time, error) {
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil {
		return time.Time{}, err
	}
	episodes, err := readRecoveryEpisodes(cm)
	if err != nil {
		return time.Time{}, err
	}
	started := observed.UTC()
	if previous, ok := episodes[pod.Name]; ok && previous.UID == pod.UID && previous.StartedAt.Time.Before(started) {
		started = previous.StartedAt.Time
	}
	next := recoveryEpisode{UID: pod.UID, StartedAt: metav1.NewTime(started)}
	if previous, ok := episodes[pod.Name]; ok && previous == next {
		return started, nil
	}
	episodes[pod.Name] = next
	if err := c.writeRecoveryEpisodes(ctx, ms, cm, episodes); err != nil {
		return time.Time{}, err
	}
	return started, nil
}

func (c *ModelServingController) recoveryEpisodeMatches(
	ctx context.Context,
	ms *api.ModelServing,
	podName string,
	uid types.UID,
) (bool, error) {
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil || cm == nil {
		return false, err
	}
	episodes, err := readRecoveryEpisodes(cm)
	if err != nil {
		return false, err
	}
	episode, ok := episodes[podName]
	return ok && episode.UID == uid, nil
}

func (c *ModelServingController) clearRecoveryObservations(pod *corev1.Pod) {
	name := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	c.recoveryObservations.Range(func(key, _ interface{}) bool {
		if key.(podGracePeriodKey).NamespacedName == name {
			c.recoveryObservations.Delete(key)
		}
		return true
	})
}

// An empty UID clears any episode for the current Pod name. A non-empty UID
// fences stale Ready/Delete callbacks from clearing a replacement Pod's fault.
func (c *ModelServingController) clearRecoveryEpisode(
	ctx context.Context,
	ms *api.ModelServing,
	podName string,
	uid types.UID,
) error {
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil || cm == nil {
		return err
	}
	episodes, err := readRecoveryEpisodes(cm)
	if err != nil {
		return err
	}
	previous, ok := episodes[podName]
	if !ok || (uid != "" && previous.UID != uid) {
		return nil
	}
	delete(episodes, podName)
	return c.writeRecoveryEpisodes(ctx, ms, cm, episodes)
}
