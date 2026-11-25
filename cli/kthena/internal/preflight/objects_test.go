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
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// podGroupOf returns the PodGroup an earlier version created for a group.
func podGroupOf(ms *workloadv1alpha1.ModelServing, ordinal int) *schedulingv1beta1.PodGroup {
	group := utils.GenerateServingGroupName(ms.Name, ordinal)
	owner := metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)
	owner.BlockOwnerDeletion = nil
	return &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: ms.Namespace, Name: group, UID: types.UID("uid-podgroup-" + group),
		Labels: map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
			workloadv1alpha1.GroupNameLabelKey:        group,
		},
		OwnerReferences: []metav1.OwnerReference{*owner},
	}}
}

// serviceOf returns the headless Service an earlier version created for the
// Role instance leader-0 of a group.
func serviceOf(ms *workloadv1alpha1.ModelServing, ordinal int) *corev1.Service {
	group := utils.GenerateServingGroupName(ms.Name, ordinal)
	name := utils.GeneratePodName(group, "leader-0", 0)
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: name, UID: types.UID("uid-service-" + name),
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        group,
				workloadv1alpha1.RoleLabelKey:             "leader",
				workloadv1alpha1.RoleIDKey:                "leader-0",
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)},
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}
}

// servingGroupOf returns the ServingGroup object of a group, controlled by owner.
func servingGroupOf(ms *workloadv1alpha1.ModelServing, ordinal int, owner *workloadv1alpha1.ModelServing) *workloadv1alpha1.ServingGroup {
	group := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: ms.Namespace, Name: utils.GenerateServingGroupName(ms.Name, ordinal),
		UID:    types.UID(fmt.Sprintf("uid-servinggroup-%s-%d", ms.Name, ordinal)),
		Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name},
	}}
	if owner != nil {
		group.OwnerReferences = []metav1.OwnerReference{*metav1.NewControllerRef(owner, workloadv1alpha1.ModelServingKind)}
	}
	return group
}

// findingLevels maps the identifier of every finding to its level.
func findingLevels(findings []Finding) map[string]Level {
	levels := map[string]Level{}
	for _, finding := range findings {
		levels[finding.ID] = finding.Level
	}
	return levels
}

func findingByID(findings []Finding, id string) *Finding {
	for i := range findings {
		if findings[i].ID == id {
			return &findings[i]
		}
	}
	return nil
}

func budget(name string, spec policyv1.PodDisruptionBudgetSpec) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name}, Spec: spec}
}

func selecting(name string) *metav1.LabelSelector {
	return &metav1.LabelSelector{MatchLabels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: name}}
}

const controllerNamespace = "kthena-system"

var errForbidden = errors.New("forbidden")

// controllerDeployment returns the Deployment of the controller manager of an
// earlier version, as the chart renders it.
func controllerDeployment(namespace string, mutate ...func(*appsv1.Deployment)) *appsv1.Deployment {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: controllerManagerName},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr(int32(1)),
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{componentLabel: controllerManagerName}},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name: controllerManagerName, Image: "ghcr.io/volcano-sh/kthena-controller-manager:v1.1.0",
					Args: []string{"--enable-webhook=true", "--port=8443"},
				}}},
			},
		},
		Status: appsv1.DeploymentStatus{ReadyReplicas: 1},
	}
	for _, change := range mutate {
		change(deployment)
	}
	return deployment
}

// controllerPod returns a running Pod of a controller manager; protocol is
// the value of its controller-protocol label, empty for an earlier version.
func controllerPod(namespace, name, protocol string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: map[string]string{componentLabel: controllerManagerName}},
		Spec:       corev1.PodSpec{NodeName: "node-1"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	if protocol != "" {
		pod.Labels[controllerProtocolLabel] = protocol
	}
	return pod
}

func upgradedTemplate(deployment *appsv1.Deployment) {
	deployment.Spec.Template.Labels[controllerProtocolLabel] = controllerProtocolServingGroup
}

func withArgs(args ...string) func(*appsv1.Deployment) {
	return func(deployment *appsv1.Deployment) {
		deployment.Spec.Template.Spec.Containers[0].Args = args
	}
}

// keysOf returns the sorted keys of a JSON object.
func keysOf(t *testing.T, value any) []string {
	t.Helper()
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("%v is not a JSON object", value)
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func decode(t *testing.T, value any) map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := WriteJSON(&out, value); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	decoded := map[string]any{}
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("the output is not JSON: %v", err)
	}
	return decoded
}
