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
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func podStateCases() []modelServingCase {
	started := metav1.NewTime(testTime.Add(-time.Minute)).Format(time.RFC3339)
	return []modelServingCase{
		{
			name: "a Pod is terminating",
			mutate: func(t *testing.T, in *modelServingInput) {
				terminating(podsOf(in, 2)[0])
			},
			want:        map[string]Level{FindingTerminatingPods: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if g := report.Groups[2]; g.Pods != 1 || g.TerminatingPods != 1 {
					t.Errorf("group 2 = %+v, want 1 Pod and 1 terminating Pod", g)
				}
			},
		},
		{
			name: "every Pod of a group is terminating: the group is not taken over",
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 2, terminating)
			},
			want: map[string]Level{
				FindingTerminatingPods:     LevelWait,
				FindingGroupsBelowReplicas: LevelWait,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingTerminatingPods).Message; !strings.Contains(message, "Every Pod of llama-2 is terminating") {
					t.Errorf("message = %s", message)
				}
				if !slices.Equal(report.Rollout.Ordinals, []int{0, 1}) {
					t.Errorf("ordinals = %v, want 0 and 1", report.Rollout.Ordinals)
				}
			},
		},
		{
			name: "a Pod has failed and recovery is enabled: its Role instance is recreated",
			mutate: func(t *testing.T, in *modelServingInput) {
				failed(podsOf(in, 1)[1])
			},
			want:        map[string]Level{FindingFailedPods: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingFailedPods).Message; !strings.Contains(message, "llama-1-leader-0-1 (Role instance leader-0 of group llama-1 is recreated)") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "a Pod has failed under ServingGroupRecreate: its group is recreated",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
				failed(podsOf(in, 1)[1])
			},
			want:        map[string]Level{FindingFailedPods: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingFailedPods).Message; !strings.Contains(message, "(group llama-1 is recreated)") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "a Pod has failed and recoveryPolicy is None: it is tolerated",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
				failed(podsOf(in, 1)[1])
			},
			want:        map[string]Level{FindingFailedPods: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if len(report.PodChangesExpected) != 0 {
					t.Errorf("podChangesExpected = %v, want none: nothing is recreated", report.PodChangesExpected)
				}
			},
		},
		{
			name: "a Pod has failed and the grace period is -1: it is tolerated",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Spec.Template.RestartGracePeriodSeconds = ptr(int64(-1))
				failed(podsOf(in, 1)[1])
			},
			want:        map[string]Level{FindingFailedPods: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "a container restarted and the Pod is not ready",
			mutate: func(t *testing.T, in *modelServingInput) {
				pod := podsOf(in, 0)[0]
				restarted(3)(pod)
				notReady(pod)
			},
			want:        map[string]Level{FindingRestartingPods: LevelWait},
			wantVerdict: VerdictWait,
		},
		{
			name: "a container restarted and the Pod is ready again: not a fault",
			mutate: func(t *testing.T, in *modelServingInput) {
				restarted(3)(podsOf(in, 0)[0])
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
		{
			name: "a Pod that is not ready and never restarted: not a fault",
			mutate: func(t *testing.T, in *modelServingInput) {
				notReady(podsOf(in, 0)[0])
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if g := report.Groups[0]; g.Ready || g.ReadyPods != 1 {
					t.Errorf("group 0 = %+v, want a group that is not ready with 1 ready Pod", g)
				}
			},
		},
		{
			name: "an in-place update is in progress",
			mutate: func(t *testing.T, in *modelServingInput) {
				updated := rollTo(t, in, "model:v2")
				change(in, 0, atRevision(updated))
				change(in, 1, atRevision(updated))
				// One Pod of group 2 is patched and still runs the old container;
				// the other one is not patched yet. Both carry the old label.
				pod := podsOf(in, 2)[0]
				annotate(workloadv1alpha1.InPlaceUpdateStateAnnotationKey,
					`{"revision":"`+updated+`","startedAt":"`+started+`","patched":{"model":{"image":"model:v2","prevContainerID":"containerd://`+pod.Name+`"}},"expected":{"model":1}}`)(pod)
			},
			want:        map[string]Level{FindingInPlaceUpdate: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Groups[2].AtTarget != "yes" || report.Revision.ToRoll != 0 {
					t.Errorf("group 2 = %+v, want the group counted at the revision it is being updated to", report.Groups[2])
				}
				if got := findingByID(report.Findings, FindingInPlaceUpdate).Objects; !slices.Equal(got, []string{"llama-2-leader-0-0"}) {
					t.Errorf("objects = %v, want the Pod that is being updated", got)
				}
			},
		},
		{
			name: "the restart of a finished in-place update: not a fault",
			mutate: func(t *testing.T, in *modelServingInput) {
				pod := podsOf(in, 2)[0]
				annotate(workloadv1alpha1.InPlaceUpdateStateAnnotationKey,
					`{"revision":"`+in.ms.Status.UpdateRevision+`","startedAt":"`+started+`","done":true,"expected":{"model":1}}`)(pod)
				restarted(1)(pod)
				notReady(pod)
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
		},
	}
}

func TestPodStateFindings(t *testing.T) {
	runModelServingCases(t, podStateCases())
}

func strayPodCases() []modelServingCase {
	return []modelServingCase{
		{
			name: "a Pod the ModelServing controls that is not a member of a group",
			mutate: func(t *testing.T, in *modelServingInput) {
				stray := memberPod(in.ms, 0, "leader-0", 0, in.ms.Status.UpdateRevision)
				stray.Name, stray.UID = "llama-0-leader-0-0-copy", "uid-copy"
				in.pods = append(in.pods, stray)
			},
			want:        map[string]Level{FindingPodNotMember: LevelBlocked},
			wantVerdict: VerdictBlocked,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := findingByID(report.Findings, FindingPodNotMember).Objects; !slices.Equal(got, []string{"llama-0-leader-0-0-copy"}) {
					t.Errorf("objects = %v", got)
				}
				if report.Groups[0].Pods != 2 {
					t.Errorf("group 0 = %+v, want the copy not counted", report.Groups[0])
				}
			},
		},
		{
			name: "a member Pod without a controller owner is adopted",
			mutate: func(t *testing.T, in *modelServingInput) {
				withoutOwner(podsOf(in, 1)[0])
			},
			want:        map[string]Level{FindingPodWithoutOwner: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingPodWithoutOwner).Message; !strings.Contains(message, "adopted by the ServingGroup") {
					t.Errorf("message = %s", message)
				}
				if report.Groups[1].Pods != 2 {
					t.Errorf("group 1 = %+v, want the Pod counted as a member", report.Groups[1])
				}
			},
		},
		{
			name: "a labelled Pod without a controller owner that is not a member is left alone",
			mutate: func(t *testing.T, in *modelServingInput) {
				stray := memberPod(in.ms, 0, "leader-0", 0, in.ms.Status.UpdateRevision, withoutOwner)
				stray.Name, stray.UID = "llama-debug", "uid-debug"
				in.pods = append(in.pods, stray)
			},
			want:        map[string]Level{FindingPodWithoutOwner: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if message := findingByID(report.Findings, FindingPodWithoutOwner).Message; !strings.Contains(message, "neither taken over nor deleted") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "a labelled Pod that another object controls",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.pods = append(in.pods, memberPod(in.ms, 7, "leader-0", 0, in.ms.Status.UpdateRevision, func(pod *corev1.Pod) {
					pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "other", UID: "uid-other", Controller: ptr(true)}}
				}))
			},
			want:        map[string]Level{FindingPodForeignOwner: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if len(report.Groups) != 3 {
					t.Errorf("groups = %+v, want no group for the Pod of another owner", report.Groups)
				}
			},
		},
	}
}

func TestStrayPodFindings(t *testing.T) {
	runModelServingCases(t, strayPodCases())
}
