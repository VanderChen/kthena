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
	"errors"
	"sort"
	"sync"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
)

type discoveryScope struct{ group, instance string }
type discoveryOutput struct {
	fingerprint string
	pending     bool
}

type discoveryObservations struct {
	mu     sync.Mutex // different MS workers access the outer map concurrently
	owners map[ownerInputKey]map[discoveryScope]discoveryOutput
}

type discoverySnapshot struct {
	fingerprint string
	trigger     *corev1.Pod
}

func (c *ModelServingController) discoveryRequest(ms *api.ModelServing, pod *corev1.Pod) *plugins.HookRequest {
	return &plugins.HookRequest{ModelServing: ms, ServingGroup: pod.Labels[api.GroupNameLabelKey], RoleName: utils.GetRoleName(pod), RoleID: utils.GetRoleID(pod), IsEntry: pod.Labels[api.EntryLabelKey] == utils.Entry, Pod: pod, KubeClient: c.kubeClientSet}
}

// This is cache-only and hashes only membership inputs. ResourceVersion,
// Ready, restart counts and condition messages do not change Discovery output.
func (c *ModelServingController) discoverySnapshots(ms *api.ModelServing, chain *plugins.Chain) (map[discoveryScope]discoverySnapshot, error) {
	pods, err := c.podsLister.Pods(ms.Namespace).List(labels.SelectorFromSet(labels.Set{api.ModelServingNameLabelKey: ms.Name}))
	if err != nil {
		return nil, err
	}
	type member struct{ Name, UID, Role, IP, Target string }
	members := make(map[discoveryScope][]member)
	snapshots := make(map[discoveryScope]discoverySnapshot)
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(pod, ms.UID) || pod.Status.Phase != corev1.PodRunning || pod.Status.PodIP == "" || utils.GetRoleName(pod) == "" {
			continue
		}
		scope := discoveryScope{pod.Labels[api.GroupNameLabelKey], pod.Annotations[groupInstanceAnnotation]}
		if scope.group == "" || c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), scope.group) == datastore.ServingGroupDeleting {
			continue
		}
		members[scope] = append(members[scope], member{pod.Name, string(pod.UID), utils.GetRoleName(pod), pod.Status.PodIP, pod.Labels[api.EntryLabelKey]})
		snapshot := snapshots[scope]
		if pod.DeletionTimestamp == nil && chain.PodDiscoveryEligible(c.discoveryRequest(ms, pod)) && (snapshot.trigger == nil || (utils.IsPodRunningAndReady(snapshot.trigger) && !utils.IsPodRunningAndReady(pod))) {
			snapshot.trigger = pod
		}
		snapshots[scope] = snapshot
	}
	for scope, values := range members {
		sort.Slice(values, func(i, j int) bool { return values[i].Name < values[j].Name })
		data, err := json.Marshal(values)
		if err != nil {
			return nil, err
		}
		snapshot := snapshots[scope]
		snapshot.fingerprint = string(data)
		snapshots[scope] = snapshot
	}
	return snapshots, nil
}

func (c *ModelServingController) storeDiscoveryOutput(ms *api.ModelServing, scope discoveryScope, output discoveryOutput) {
	c.discovery.mu.Lock()
	defer c.discovery.mu.Unlock()
	if c.discovery.owners == nil {
		c.discovery.owners = make(map[ownerInputKey]map[discoveryScope]discoveryOutput)
	}
	key := ownerKey(ms)
	if c.discovery.owners[key] == nil {
		c.discovery.owners[key] = make(map[discoveryScope]discoveryOutput)
	}
	c.discovery.owners[key][scope] = output
}

// recordPodDiscovery captures inputs BEFORE the original Ready hook. Invoke the
// returned function only after that hook chain succeeds; reading inputs after
// its API calls could incorrectly acknowledge a newer cache change.
func (c *ModelServingController) recordPodDiscovery(ms *api.ModelServing, pod *corev1.Pod) func() {
	chain, err := c.buildPluginChain(ms)
	if err != nil || !chain.PodDiscoveryEligible(c.discoveryRequest(ms, pod)) {
		return func() {}
	}
	snapshots, err := c.discoverySnapshots(ms, chain)
	scope := discoveryScope{pod.Labels[api.GroupNameLabelKey], pod.Annotations[groupInstanceAnnotation]}
	snapshot, exists := snapshots[scope]
	if err != nil || !exists {
		return func() {}
	}
	return func() { c.storeDiscoveryOutput(ms, scope, discoveryOutput{fingerprint: snapshot.fingerprint}) }
}

// maintainPodDiscovery only performs API work for changed Running/NotReady
// membership or a previously failed independent output update. It never
// replays the original Running/Ready hook chain.
func (c *ModelServingController) maintainPodDiscovery(ctx context.Context, ms *api.ModelServing) error {
	configured := false
	for _, spec := range ms.Spec.Plugins {
		if spec.Name == plugins.PodDiscoveryPluginName {
			configured = true
			break
		}
	}
	if !configured {
		c.retirePodDiscovery(ms)
		return nil
	}
	chain, err := c.buildPluginChain(ms)
	if err != nil {
		return err
	}
	snapshots, err := c.discoverySnapshots(ms, chain)
	if err != nil {
		return err
	}
	key := ownerKey(ms)
	c.discovery.mu.Lock()
	previous := make(map[discoveryScope]discoveryOutput)
	for owner, outputs := range c.discovery.owners {
		if owner.NamespacedName == key.NamespacedName && owner.UID != key.UID {
			delete(c.discovery.owners, owner)
		}
		if owner != key {
			continue
		}
		for scope, output := range outputs {
			if snapshot, exists := snapshots[scope]; !exists || snapshot.trigger == nil {
				delete(outputs, scope)
			} else {
				previous[scope] = output
			}
		}
	}
	c.discovery.mu.Unlock()
	var errs []error
	for scope, snapshot := range snapshots {
		output := previous[scope]
		if snapshot.trigger == nil || (!output.pending && output.fingerprint == snapshot.fingerprint) {
			continue
		}
		// New Ready-only work belongs to the original best-effort Ready hook.
		// A pending NotReady update may finish after its trigger becomes Ready.
		if utils.IsPodRunningAndReady(snapshot.trigger) && !output.pending {
			continue
		}
		attempted, err := chain.RetryPodDiscovery(ctx, c.discoveryRequest(ms, snapshot.trigger))
		if err != nil {
			output.pending = true
			c.storeDiscoveryOutput(ms, scope, output)
			errs = append(errs, err)
		} else if attempted {
			c.storeDiscoveryOutput(ms, scope, discoveryOutput{fingerprint: snapshot.fingerprint})
		}
	}
	return errors.Join(errs...)
}

func (c *ModelServingController) retirePodDiscovery(ms *api.ModelServing) {
	c.discovery.mu.Lock()
	defer c.discovery.mu.Unlock()
	delete(c.discovery.owners, ownerKey(ms))
}
