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

package utils

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestPodDeviationToleratesAdditionsButNotTemplateChanges(t *testing.T) {
	ms := revisionTestModelServing(revisionTestRole("decode", "image:v1"))
	ms.Name, ms.Namespace = "ms", "default"
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	role := ms.Spec.Template.Roles[0]
	entry := GenerateEntryPod(*role.DeepCopy(), ms, "ms-0", "decode-0", "rev", "")
	worker := GenerateWorkerPod(*role.DeepCopy(), ms, "ms-0", "decode-0", 1, "rev", "")

	require.Equal(t, entry.Name, RenderPodForRecovery(ms, entry).Name)
	require.Equal(t, worker.Name, RenderPodForRecovery(ms, worker).Name)
	require.Empty(t, PodDeviation(RenderPodForRecovery(ms, entry), entry))
	require.Empty(t, PodDeviation(RenderPodForRecovery(ms, worker), worker))

	// API-server defaulting of env sources does not disprove the template.
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = []corev1.EnvVar{
		{Name: "POD_IP", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"}}},
		{Name: "CPU_LIMIT", ValueFrom: &corev1.EnvVarSource{ResourceFieldRef: &corev1.ResourceFieldSelector{Resource: "limits.cpu"}}},
	}
	defaulted := GenerateEntryPod(*ms.Spec.Template.Roles[0].DeepCopy(), ms, "ms-0", "decode-0", "rev", "")
	defaulted.Spec.Containers[0].Env[0].ValueFrom.FieldRef.APIVersion = "v1"
	defaulted.Spec.Containers[0].Env[1].ValueFrom.ResourceFieldRef.Divisor = resource.MustParse("1")
	require.Empty(t, PodDeviation(RenderPodForRecovery(ms, defaulted), defaulted))
	defaulted.Spec.Containers[0].Env[0].ValueFrom.FieldRef.FieldPath = "status.hostIP"
	require.Contains(t, PodDeviation(RenderPodForRecovery(ms, defaulted), defaulted), "env POD_IP")
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Env = nil

	// Additions made by admission or plugins do not disprove the template.
	live := entry.DeepCopy()
	live.Spec.Containers[0].Env = append(live.Spec.Containers[0].Env, corev1.EnvVar{Name: "ENTRY_ADDRESS", Value: "svc"})
	live.Spec.Containers[0].Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("100m")}
	live.Spec.Tolerations = []corev1.Toleration{{Key: "injected"}}
	live.Spec.Volumes = append(live.Spec.Volumes, corev1.Volume{Name: "kube-api-access"})
	require.Empty(t, PodDeviation(RenderPodForRecovery(ms, live), live))

	type mutation struct {
		name   string
		mutate func(*corev1.Pod)
		reason string
	}
	cases := []mutation{
		{"image", func(p *corev1.Pod) { p.Spec.Containers[0].Image = "image:v2" }, "runs image:v2"},
		{"command", func(p *corev1.Pod) { p.Spec.Containers[0].Command = []string{"other"} }, "different command"},
		{"args", func(p *corev1.Pod) { p.Spec.Containers[0].Args = []string{"--flag"} }, "different args"},
		{"group size", func(p *corev1.Pod) {
			for i := range p.Spec.Containers[0].Env {
				if p.Spec.Containers[0].Env[i].Name == "GROUP_SIZE" {
					p.Spec.Containers[0].Env[i].Value = "5"
				}
			}
		}, "env GROUP_SIZE"},
		{"extra container", func(p *corev1.Pod) {
			p.Spec.Containers = append(p.Spec.Containers, corev1.Container{Name: "sidecar", Image: "x"})
		}, "2 containers run but the template declares 1"},
		{"renamed container", func(p *corev1.Pod) { p.Spec.Containers[0].Name = "renamed" }, "container decode is missing"},
		{"worker position beyond template", func(p *corev1.Pod) {
			delete(p.Labels, workloadv1alpha1.EntryLabelKey)
			p.Name = "ms-0-decode-0-2"
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			live := entry.DeepCopy()
			tc.mutate(live)
			rendered := RenderPodForRecovery(ms, live)
			if tc.reason == "" {
				require.Nil(t, rendered)
				return
			}
			require.NotNil(t, rendered)
			require.Contains(t, PodDeviation(rendered, live), tc.reason)
		})
	}

	// Template-declared fields missing from the live Pod are deviations.
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits = corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi")}
	require.Contains(t, PodDeviation(RenderPodForRecovery(ms, entry), entry), "limits.memory")
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Resources.Limits = nil
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.NodeSelector = map[string]string{"gpu": "a100"}
	require.Contains(t, PodDeviation(RenderPodForRecovery(ms, entry), entry), "nodeSelector gpu")
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.NodeSelector = nil
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.RuntimeClassName = ptr.To("kata")
	require.Contains(t, PodDeviation(RenderPodForRecovery(ms, entry), entry), "runtimeClassName")
}
