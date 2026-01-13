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

package plugins

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	leaderworkerset "sigs.k8s.io/lws/api/leaderworkerset/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const LwsPluginName = "lws"

type LwsPlugin struct {
	name string
}

func init() {
	DefaultRegistry.Register(LwsPluginName, NewLwsPlugin)
}

func NewLwsPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	return &LwsPlugin{name: spec.Name}, nil
}

func (p *LwsPlugin) Name() string { return p.name }

func (p *LwsPlugin) OnPodCreate(_ context.Context, req *HookRequest) error {
	if req == nil || req.Pod == nil {
		return nil
	}

	// 1. Set LWS Name Label
	if req.Pod.Labels == nil {
		req.Pod.Labels = map[string]string{}
	}
	// LWS Name should be unique per Role to support multiple Roles mapping to multiple LWS instances
	req.Pod.Labels[leaderworkerset.SetNameLabelKey] = req.ModelServing.Name + "-" + req.RoleName

	// 2. Set LWS Group Index Label
	_, groupIndex := utils.GetParentNameAndOrdinal(req.ServingGroup)
	if groupIndex >= 0 {
		req.Pod.Labels[leaderworkerset.GroupIndexLabelKey] = strconv.Itoa(groupIndex)
	}

	// 3. Set LWS Group Unique Hash Label
	// LWS uses a hash of the group name to provide a unique label for the group.
	// We generate a SHA1 hash of the ServingGroup name.
	hasher := sha1.New()
	hasher.Write([]byte(req.ServingGroup))
	req.Pod.Labels[leaderworkerset.GroupUniqueHashLabelKey] = hex.EncodeToString(hasher.Sum(nil))

	// 4. Extract Env Vars from Pod Spec to reuse values
	var workerIndex string
	var groupSize string
	var entryAddress string

	// Since env vars are added to all containers, we just check the first one
	if len(req.Pod.Spec.Containers) > 0 {
		for _, env := range req.Pod.Spec.Containers[0].Env {
			switch env.Name {
			case workloadv1alpha1.WorkerIndexEnv:
				workerIndex = env.Value
			case workloadv1alpha1.GroupSizeEnv:
				groupSize = env.Value
			case workloadv1alpha1.EntryAddressEnv:
				entryAddress = env.Value
			}
		}
	}

	// 5. Inject LWS Env Vars and other Labels/Annotations
	var newEnvVars []corev1.EnvVar

	if workerIndex != "" {
		req.Pod.Labels[leaderworkerset.WorkerIndexLabelKey] = workerIndex
		newEnvVars = append(newEnvVars, corev1.EnvVar{
			Name:  leaderworkerset.LwsWorkerIndex,
			Value: workerIndex,
		})
	}

	if groupSize != "" {
		if req.Pod.Annotations == nil {
			req.Pod.Annotations = map[string]string{}
		}
		req.Pod.Annotations[leaderworkerset.SizeAnnotationKey] = groupSize
		newEnvVars = append(newEnvVars, corev1.EnvVar{
			Name:  leaderworkerset.LwsGroupSize,
			Value: groupSize,
		})
	}

	if entryAddress != "" {
		newEnvVars = append(newEnvVars, corev1.EnvVar{
			Name:  leaderworkerset.LwsLeaderAddress,
			Value: entryAddress,
		})
	}

	if len(newEnvVars) > 0 {
		p.addEnvToAllContainers(req.Pod, newEnvVars)
	}

	// 6. Set Replicas Annotation
	if req.ModelServing.Spec.Replicas != nil {
		if req.Pod.Annotations == nil {
			req.Pod.Annotations = map[string]string{}
		}
		req.Pod.Annotations[leaderworkerset.ReplicasAnnotationKey] = strconv.Itoa(int(*req.ModelServing.Spec.Replicas))
	}

	return nil
}

func (p *LwsPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func (p *LwsPlugin) addEnvToAllContainers(pod *corev1.Pod, newEnvs []corev1.EnvVar) {
	for i := range pod.Spec.Containers {
		pod.Spec.Containers[i].Env = append(pod.Spec.Containers[i].Env, newEnvs...)
	}
	for i := range pod.Spec.InitContainers {
		pod.Spec.InitContainers[i].Env = append(pod.Spec.InitContainers[i].Env, newEnvs...)
	}
}
