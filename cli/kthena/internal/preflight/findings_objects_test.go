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
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func existingObjectCases() []modelServingCase {
	return []modelServingCase{
		{
			name: "a ServingGroup object holds the name of a group without belonging to the ModelServing",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.groups = []*workloadv1alpha1.ServingGroup{servingGroupOf(in.ms, 1, nil)}
			},
			want:        map[string]Level{FindingServingGroupNameTaken: LevelBlocked},
			wantVerdict: VerdictBlocked,
		},
		{
			name: "a ServingGroup object that the ModelServing controls",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.groups = []*workloadv1alpha1.ServingGroup{servingGroupOf(in.ms, 1, in.ms)}
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
		{
			name: "a PodGroup and a Service of a group without Pods are deleted by the takeover",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.podGroups = []*schedulingv1beta1.PodGroup{podGroupOf(in.ms, 0), podGroupOf(in.ms, 1), podGroupOf(in.ms, 2), podGroupOf(in.ms, 5)}
				in.services = []*corev1.Service{serviceOf(in.ms, 0), serviceOf(in.ms, 5)}
			},
			want:        map[string]Level{FindingObjectsWithoutPods: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := findingByID(report.Findings, FindingObjectsWithoutPods).Objects; !slices.Equal(got, []string{"PodGroup llama-5", "Service llama-5-leader-0-0"}) {
					t.Errorf("objects = %v", got)
				}
			},
		},
	}
}

func TestExistingObjectFindings(t *testing.T) {
	runModelServingCases(t, existingObjectCases())
}

func dryRunCases() []modelServingCase {
	return []modelServingCase{
		{
			name: "the dry-run write is rejected by an admission webhook",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.dryRun = DryRunResult{Outcome: DryRunRejected, Webhook: "policy.example.com", Message: "annotations are locked"}
			},
			want:        map[string]Level{FindingDryRunRejected: LevelBlocked},
			wantVerdict: VerdictBlocked,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingDryRunRejected).Message; !strings.Contains(message, `"policy.example.com"`) || !strings.Contains(message, "annotations are locked") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "the dry-run write is rejected by the validation of the API server",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.dryRun = DryRunResult{Outcome: DryRunRejected, Message: "spec.replicas: Invalid value"}
			},
			want:        map[string]Level{FindingDryRunRejected: LevelBlocked},
			wantVerdict: VerdictBlocked,
		},
		{
			name: "the dry-run write is rejected by the ModelServing webhook of the running version",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.dryRun = DryRunResult{Outcome: DryRunRejected, Webhook: modelServingWebhookName, Message: "spec.template.roles[0].name: Invalid value"}
			},
			want:        map[string]Level{FindingDryRunRejectedByOwn: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "the dry-run write could not be judged",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.dryRun = DryRunResult{Outcome: DryRunUnknown, Message: "forbidden"}
			},
			want:        map[string]Level{FindingDryRunUnknown: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "the dry-run write was skipped",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.dryRun = DryRunResult{Outcome: DryRunSkipped}
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
	}
}

func TestDryRunFindings(t *testing.T) {
	runModelServingCases(t, dryRunCases())
}

func environmentCases() []modelServingCase {
	return []modelServingCase{
		{
			name: "Pods run on a cordoned node",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.cordoned = map[string]bool{"node-2": true}
				change(in, 1, onNode("node-2"))
			},
			want:        map[string]Level{FindingPodsOnCordonedNodes: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := findingByID(report.Findings, FindingPodsOnCordonedNodes).Objects; len(got) != 2 {
					t.Errorf("objects = %v, want the 2 Pods of group 1", got)
				}
			},
		},
		{
			name: "a cordoned node without Pods of the ModelServing",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.cordoned = map[string]bool{"node-9": true}
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
		{
			name: "PodDisruptionBudgets whose expected Pod count comes from the owner of the Pods",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.budgets = []*policyv1.PodDisruptionBudget{
					budget("max-unavailable", policyv1.PodDisruptionBudgetSpec{Selector: selecting("llama"), MaxUnavailable: ptr(intstr.FromInt32(1))}),
					budget("min-available-percent", policyv1.PodDisruptionBudgetSpec{Selector: selecting("llama"), MinAvailable: ptr(intstr.FromString("50%"))}),
					budget("min-available-absolute", policyv1.PodDisruptionBudgetSpec{Selector: selecting("llama"), MinAvailable: ptr(intstr.FromInt32(2))}),
					budget("other-workload", policyv1.PodDisruptionBudgetSpec{Selector: selecting("qwen"), MaxUnavailable: ptr(intstr.FromInt32(1))}),
					budget("no-selector", policyv1.PodDisruptionBudgetSpec{MaxUnavailable: ptr(intstr.FromInt32(1))}),
				}
			},
			want:        map[string]Level{FindingDisruptionBudget: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := findingByID(report.Findings, FindingDisruptionBudget).Objects; !slices.Equal(got, []string{"max-unavailable", "min-available-percent"}) {
					t.Errorf("objects = %v", got)
				}
			},
		},
		{
			name: "a PodDisruptionBudget with an absolute minAvailable",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.budgets = []*policyv1.PodDisruptionBudget{
					budget("min-available-absolute", policyv1.PodDisruptionBudgetSpec{Selector: selecting("llama"), MinAvailable: ptr(intstr.FromInt32(2))}),
				}
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
	}
}

func TestEnvironmentFindings(t *testing.T) {
	runModelServingCases(t, environmentCases())
}
