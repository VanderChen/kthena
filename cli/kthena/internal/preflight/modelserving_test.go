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
	"maps"
	"slices"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// steadyInput returns the input of a ModelServing "llama" in steady state:
// its rollout is complete, the groups at the ordinals exist, every Pod is
// ready, and the dry-run write passed. Nothing about it may be reported.
func steadyInput(t *testing.T, replicas int32, ordinals ...int) *modelServingInput {
	t.Helper()
	ms := newModelServing("llama", replicas)
	revision := steady(t, ms)
	return &modelServingInput{
		ms:        ms,
		pods:      groupsPods(ms, identityOf(revision), ordinals...),
		revisions: []*appsv1.ControllerRevision{revision},
		cordoned:  map[string]bool{},
		dryRun:    DryRunResult{Outcome: DryRunPassed},
		now:       testTime,
	}
}

// rollTo changes the image of the template, as a user would, and lets the
// running controller notice: the new revision is recorded and the status
// names it as the one being rolled to. The Pods stay at their revision. It
// returns the identity of the new revision.
func rollTo(t *testing.T, in *modelServingInput, image string) string {
	t.Helper()
	setImage(in.ms, image)
	in.ms.Generation++
	in.ms.Status.ObservedGeneration = in.ms.Generation
	revision := record(t, in.ms, in.revisions...)
	in.revisions = append(in.revisions, revision)
	in.ms.Status.UpdateRevision = identityOf(revision)
	return identityOf(revision)
}

// podsOf returns the Pods of the group at ordinal.
func podsOf(in *modelServingInput, ordinal int) []*corev1.Pod {
	group := utils.GenerateServingGroupName(in.ms.Name, ordinal)
	var pods []*corev1.Pod
	for _, pod := range in.pods {
		if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == group {
			pods = append(pods, pod)
		}
	}
	return pods
}

// change applies mutations to every Pod of the group at ordinal.
func change(in *modelServingInput, ordinal int, mutate ...func(*corev1.Pod)) {
	for _, pod := range podsOf(in, ordinal) {
		for _, apply := range mutate {
			apply(pod)
		}
	}
}

func atRevision(revision string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Labels[workloadv1alpha1.RevisionLabelKey] = revision }
}

func restarted(count int32) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Status.ContainerStatuses[0].RestartCount = count }
}

func failed(pod *corev1.Pod) {
	pod.Status.Phase = corev1.PodFailed
	notReady(pod)
}

func roleRollingUpdate(in *modelServingInput) {
	in.ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
}

func partition(in *modelServingInput, value intstr.IntOrString) {
	in.ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:                       workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{Partition: &value},
	}
}

// modelServingCase is one case of the analysis of a ModelServing: a steady
// state, what is changed about it, and the findings that must follow.
type modelServingCase struct {
	name string
	// replicas and ordinals describe the steady state the case starts from;
	// the default is 3 replicas with the groups 0, 1 and 2. noGroups starts
	// from zero replicas and no group.
	replicas int32
	ordinals []int
	mutate   func(t *testing.T, in *modelServingInput)
	// want is the exact set of findings, by identifier.
	want        map[string]Level
	wantVerdict Verdict
	check       func(t *testing.T, in *modelServingInput, report ModelServingReport)
}

const noGroups = -1

// input builds the ModelServing of the case.
func (tt modelServingCase) input(t *testing.T) *modelServingInput {
	t.Helper()
	replicas, ordinals := tt.replicas, tt.ordinals
	switch {
	case replicas == noGroups:
		replicas, ordinals = 0, []int{}
	case replicas == 0:
		replicas = 3
	}
	if ordinals == nil {
		ordinals = []int{0, 1, 2}
	}
	in := steadyInput(t, replicas, ordinals...)
	if tt.mutate != nil {
		tt.mutate(t, in)
	}
	return in
}

func runModelServingCases(t *testing.T, tests []modelServingCase) {
	t.Helper()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.input(t)
			report := analyzeModelServing(*in)

			if got := findingLevels(report.Findings); !maps.Equal(got, tt.want) {
				t.Errorf("findings = %v, want %v", got, tt.want)
			}
			if report.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %s, want %s", report.Verdict, tt.wantVerdict)
			}
			for _, finding := range report.Findings {
				if finding.Message == "" {
					t.Errorf("finding %s has no message", finding.ID)
				}
				// What to wait for, and what to do about a blocked takeover.
				if (finding.Level == LevelWait || finding.Level == LevelBlocked) && finding.Action == "" {
					t.Errorf("finding %s of level %s names no action", finding.ID, finding.Level)
				}
			}
			if tt.check != nil && !t.Failed() {
				tt.check(t, in, report)
			}
		})
	}
}

func stateCases() []modelServingCase {
	return []modelServingCase{
		{
			name:        "steady state: nothing to report",
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if !slices.Equal(report.Rollout.Ordinals, []int{0, 1, 2}) || len(report.Rollout.Holes) != 0 {
					t.Errorf("rollout = %+v, want the ordinals 0, 1, 2 without a hole", report.Rollout)
				}
				if rollout := report.Rollout; rollout.Strategy != "ServingGroupRollingUpdate" || rollout.Replicas != 3 || rollout.RecoveryPolicy != "RoleRecreate" {
					t.Errorf("rollout = %+v, want the defaults of the API", rollout)
				}
				if len(report.PodChangesExpected) != 0 {
					t.Errorf("podChangesExpected = %v, want none", report.PodChangesExpected)
				}
				if len(report.Groups) != 3 {
					t.Fatalf("groups = %+v, want 3", report.Groups)
				}
				for _, g := range report.Groups {
					if g.SurgeAfterTakeover || !g.Ready || g.Pods != 2 || g.ReadyPods != 2 || g.TerminatingPods != 0 {
						t.Errorf("group = %+v, want a ready regular group of 2 Pods", g)
					}
				}
			},
		},
		{
			name: "the spec was changed and the running controller has not processed it",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Generation = 2
			},
			want:        map[string]Level{FindingNotObserved: LevelWait},
			wantVerdict: VerdictWait,
		},
		{
			name:        "no Pods and zero replicas: nothing to take over",
			replicas:    noGroups,
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},

		// States in which the pre-upgrade findings do not apply.
		{
			name: "the ModelServing is being deleted: nothing else is reported",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.DeletionTimestamp = ptr(metav1.NewTime(testTime))
				change(in, 2, terminating)
			},
			want:        map[string]Level{FindingDeleting: LevelWait},
			wantVerdict: VerdictWait,
		},
		{
			name: "the ModelServing was taken over already",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionDone}
				in.dryRun = DryRunResult{Outcome: DryRunRejected, Webhook: "policy.example.com"}
			},
			want:        map[string]Level{FindingTakenOver: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Adoption != workloadv1alpha1.ServingGroupAdoptionDone {
					t.Errorf("adoption = %q", report.Adoption)
				}
			},
		},
		{
			name: "the takeover is in progress",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionInProgress}
			},
			want:        map[string]Level{FindingTakeoverInProgress: LevelWait},
			wantVerdict: VerdictWait,
		},
		{
			name: "a rollback to the earlier version is in progress",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionRollingBack}
			},
			want:        map[string]Level{FindingRollbackInProgress: LevelWait},
			wantVerdict: VerdictWait,
		},
	}
}

func TestAnalyzeModelServing(t *testing.T) {
	runModelServingCases(t, stateCases())
}
