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

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func groupCases() []modelServingCase {
	return []modelServingCase{
		{
			name:     "more groups without the surge mark than replicas: one is scaled in, the one that is not ready",
			replicas: 2,
			ordinals: []int{0, 1, 3},
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 1, notReady)
			},
			want: map[string]Level{
				FindingGroupsExceedReplicas: LevelWait,
				FindingOrdinalHoles:         LevelInfo,
				FindingUnmarkedBeyond:       LevelInfo,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				finding := findingByID(report.Findings, FindingGroupsExceedReplicas)
				if !slices.Equal(finding.Objects, []string{"llama-1"}) {
					t.Errorf("candidate = %v, want the group that is not ready", finding.Objects)
				}
				if !strings.Contains(finding.Message, "ordinals 0, 1, 3") || !strings.Contains(finding.Message, "llama-1 (not ready") {
					t.Errorf("message = %s", finding.Message)
				}
				if !slices.Contains(report.PodChangesExpected, FindingGroupsExceedReplicas) {
					t.Errorf("podChangesExpected = %v, want the scale-in", report.PodChangesExpected)
				}
			},
		},
		{
			name:     "an extra group that carries the surge mark is temporary capacity, not a scale-in",
			replicas: 2,
			ordinals: []int{0, 1},
			mutate: func(t *testing.T, in *modelServingInput) {
				in.pods = append(in.pods, groupPods(in.ms, 2, rollTo(t, in, "model:v2"), surgeMarked)...)
			},
			want: map[string]Level{
				FindingSurgeGroups:       LevelWait,
				FindingRolloutInProgress: LevelWait,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if g := report.Groups[2]; !g.SurgeAfterTakeover || g.SurgeMarkedPods != 2 {
					t.Errorf("group 2 = %+v, want a surge group with 2 marked Pods", g)
				}
				if report.Revision.Units != 2 {
					t.Errorf("units = %d, want the 2 regular groups", report.Revision.Units)
				}
			},
		},
		{
			name:     "only some Pods of a group carry the surge mark: one is enough",
			replicas: 2,
			ordinals: []int{0, 1},
			mutate: func(t *testing.T, in *modelServingInput) {
				updated := rollTo(t, in, "model:v2")
				in.pods = append(in.pods, memberPod(in.ms, 2, "leader-0", 0, updated, surgeMarked), memberPod(in.ms, 2, "leader-0", 1, updated))
			},
			want: map[string]Level{
				FindingSurgeGroups:       LevelWait,
				FindingSurgeMarkPartial:  LevelAttention,
				FindingRolloutInProgress: LevelWait,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingSurgeMarkPartial).Message; !strings.Contains(message, "llama-2 (1 of 2 Pods)") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "a surge mark below replicas: the group is a regular one",
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 2, surgeMarked)
			},
			want:        map[string]Level{FindingSurgeMarkBelow: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Groups[2].SurgeAfterTakeover {
					t.Errorf("group 2 = %+v, want a regular group", report.Groups[2])
				}
			},
		},
		{
			name:     "sparse ordinals: a group beyond replicas without a mark counts as a replica",
			replicas: 2,
			ordinals: []int{0, 3},
			want: map[string]Level{
				FindingOrdinalHoles:   LevelInfo,
				FindingUnmarkedBeyond: LevelInfo,
			},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if !slices.Equal(report.Rollout.Holes, []int{1, 2}) {
					t.Errorf("holes = %v, want 1 and 2", report.Rollout.Holes)
				}
			},
		},
		{
			name:        "fewer groups than replicas: a scale-out is in progress",
			ordinals:    []int{0, 1},
			want:        map[string]Level{FindingGroupsBelowReplicas: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if len(report.PodChangesExpected) != 0 {
					t.Errorf("podChangesExpected = %v, want none: Pods are only created", report.PodChangesExpected)
				}
			},
		},
		{
			name:     "every Role has zero replicas: no group is missing",
			ordinals: []int{},
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Spec.Template.Roles[0].Replicas = ptr(int32(0))
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
		{
			name:     "RoleRollingUpdate: the group-level surge mark has no meaning",
			replicas: 2,
			ordinals: []int{0, 1},
			mutate: func(t *testing.T, in *modelServingInput) {
				roleRollingUpdate(in)
				in.pods = append(in.pods, groupPods(in.ms, 2, in.ms.Status.UpdateRevision, surgeMarked)...)
			},
			want: map[string]Level{
				FindingSurgeMarkIgnored:     LevelInfo,
				FindingGroupsExceedReplicas: LevelWait,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Groups[2].SurgeAfterTakeover {
					t.Errorf("group 2 = %+v, want a regular group under RoleRollingUpdate", report.Groups[2])
				}
			},
		},
		{
			name: "RoleRollingUpdate: a temporary Role instance of a Role rollout",
			mutate: func(t *testing.T, in *modelServingInput) {
				roleRollingUpdate(in)
				in.pods = append(in.pods, memberPod(in.ms, 0, "leader-1", 0, in.ms.Status.UpdateRevision,
					annotate(workloadv1alpha1.SurgeAnnotationKey, workloadv1alpha1.SurgeScopeRole)))
			},
			want:        map[string]Level{FindingRoleSurgeInstances: LevelWait},
			wantVerdict: VerdictWait,
		},
		{
			name: "ServingGroupRollingUpdate: the mark of a temporary Role instance left behind",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.pods = append(in.pods, memberPod(in.ms, 0, "leader-1", 0, in.ms.Status.UpdateRevision,
					annotate(workloadv1alpha1.SurgeAnnotationKey, workloadv1alpha1.SurgeScopeRole)))
			},
			want:        map[string]Level{FindingRoleSurgeInstances: LevelAttention},
			wantVerdict: VerdictAttention,
		},
	}
}

func TestGroupFindings(t *testing.T) {
	runModelServingCases(t, groupCases())
}

// TestScaleInCandidate checks which group is named as the one the new
// controller scales in when more regular groups than replicas exist: Pods not
// all ready first, then the lowest deletion cost, then the highest ordinal.
func TestScaleInCandidate(t *testing.T) {
	cost := func(value string) func(*corev1.Pod) {
		return annotate(corev1.PodDeletionCost, value)
	}
	tests := []struct {
		name     string
		replicas int32
		mutate   func(in *modelServingInput)
		want     []string
	}{
		{
			name:     "all ready, equal cost: the highest ordinal",
			replicas: 2,
			mutate:   func(*modelServingInput) {},
			want:     []string{"llama-3"},
		},
		{
			name:     "one group is not ready: that group, whatever its ordinal",
			replicas: 2,
			mutate:   func(in *modelServingInput) { notReady(podsOf(in, 0)[1]) },
			want:     []string{"llama-0"},
		},
		{
			name:     "all ready: the lowest deletion cost",
			replicas: 2,
			mutate: func(in *modelServingInput) {
				change(in, 0, cost("100"))
				change(in, 1, cost("5"))
				change(in, 3, cost("50"))
			},
			want: []string{"llama-1"},
		},
		{
			name:     "not ready comes before a low deletion cost",
			replicas: 2,
			mutate: func(in *modelServingInput) {
				change(in, 1, cost("-100"))
				change(in, 3, notReady, cost("100"))
			},
			want: []string{"llama-3"},
		},
		{
			name:     "two groups too many: two candidates, in the order they are deleted",
			replicas: 1,
			mutate:   func(in *modelServingInput) { change(in, 0, cost("-1")) },
			want:     []string{"llama-0", "llama-3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := steadyInput(t, tt.replicas, 0, 1, 3)
			tt.mutate(in)
			report := analyzeModelServing(*in)
			finding := findingByID(report.Findings, FindingGroupsExceedReplicas)
			if finding == nil {
				t.Fatalf("findings = %v, want %s", findingLevels(report.Findings), FindingGroupsExceedReplicas)
			}
			if !slices.Equal(finding.Objects, tt.want) {
				t.Errorf("candidates = %v, want %v", finding.Objects, tt.want)
			}
		})
	}
}
