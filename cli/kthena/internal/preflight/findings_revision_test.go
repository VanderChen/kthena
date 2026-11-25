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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func revisionCases() []modelServingCase {
	return []modelServingCase{
		{
			name:        "steady state: the recorded revision is reused and nothing is rolled",
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				revision := report.Revision
				if revision.Statement != "no rollout after the upgrade" || revision.TargetSource != "existing" || revision.Target != in.ms.Status.UpdateRevision {
					t.Errorf("revision = %+v, want the recorded revision reused and no rollout", revision)
				}
				if revision.Unit != "group" || revision.Units != 3 || revision.ToRoll != 0 {
					t.Errorf("revision = %+v, want 3 groups of which none is rolled", revision)
				}
				for _, g := range report.Groups {
					if g.AtTarget != "yes" {
						t.Errorf("group = %+v, want it at the target", g)
					}
				}
			},
		},
		{
			name: "a rollout is in progress: the groups not at the target are rolled",
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 2, atRevision(rollTo(t, in, "model:v2")))
			},
			want:        map[string]Level{FindingRolloutInProgress: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := report.Revision.Statement; got != "2 groups will be rolled after the upgrade" {
					t.Errorf("statement = %q", got)
				}
				if got := findingByID(report.Findings, FindingRolloutInProgress).Objects; !slices.Equal(got, []string{"llama-0", "llama-1"}) {
					t.Errorf("objects = %v, want the groups 0 and 1", got)
				}
				if !slices.Contains(report.PodChangesExpected, FindingRolloutInProgress) {
					t.Errorf("podChangesExpected = %v, want the rollout", report.PodChangesExpected)
				}
			},
		},
		{
			name: "the new version identifies the unchanged template differently: every group is rolled",
			mutate: func(t *testing.T, in *modelServingInput) {
				// The running version reports the rollout complete at a revision
				// that, read by the new version, does not render the template.
				setImage(in.ms, "model:v2")
			},
			want:        map[string]Level{FindingRevisionIdentity: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				revision := report.Revision
				if revision.Statement != "3 groups will be rolled after the upgrade" || revision.TargetSource != "new" {
					t.Errorf("revision = %+v, want a new identity and 3 groups rolled", revision)
				}
				if revision.Target == in.ms.Status.UpdateRevision {
					t.Errorf("target = %q, want another identity than the one the running version rolled to", revision.Target)
				}
			},
		},
		{
			name: "a rollout is in progress and the new version identifies its target differently",
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 2, atRevision(rollTo(t, in, "model:v2")))
				setImage(in.ms, "model:v3")
			},
			want: map[string]Level{
				FindingRevisionIdentity:  LevelAttention,
				FindingRolloutInProgress: LevelWait,
			},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if got := findingByID(report.Findings, FindingRevisionIdentity).Objects; len(got) != 3 {
					t.Errorf("objects of %s = %v, want every group", FindingRevisionIdentity, got)
				}
				if got := findingByID(report.Findings, FindingRolloutInProgress).Objects; !slices.Equal(got, []string{"llama-0", "llama-1"}) {
					t.Errorf("objects of %s = %v, want the groups the running version has not rolled", FindingRolloutInProgress, got)
				}
			},
		},
		{
			name: "the same Pods recorded under another identity: reused, no rollout",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.revisions = []*appsv1.ControllerRevision{handMade(t, in.ms, in.ms, "0ldidentity", 3)}
				in.ms.Status.CurrentRevision, in.ms.Status.UpdateRevision = "0ldidentity", "0ldidentity"
				for ordinal := range 3 {
					change(in, ordinal, atRevision("0ldidentity"))
				}
			},
			want:        map[string]Level{},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if revision := report.Revision; revision.Target != "0ldidentity" || revision.TargetSource != "existing" || revision.ToRoll != 0 {
					t.Errorf("revision = %+v, want the identity 0ldidentity reused and no rollout", revision)
				}
			},
		},
		{
			name: "a partition holds the groups below it",
			mutate: func(t *testing.T, in *modelServingInput) {
				partition(in, intstr.FromInt32(2))
				change(in, 2, atRevision(rollTo(t, in, "model:v2")))
			},
			want:        map[string]Level{FindingPartitionHolds: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if revision := report.Revision; revision.HeldByPartition != 2 || revision.ToRoll != 0 || revision.Statement != "no rollout after the upgrade" {
					t.Errorf("revision = %+v, want 2 groups held and none rolled", revision)
				}
			},
		},
		{
			name: "a partition in percent is rounded up",
			mutate: func(t *testing.T, in *modelServingInput) {
				partition(in, intstr.FromString("50%"))
				change(in, 2, atRevision(rollTo(t, in, "model:v2")))
			},
			want:        map[string]Level{FindingPartitionHolds: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Rollout.Partition != 2 {
					t.Errorf("partition = %d, want 2", report.Rollout.Partition)
				}
			},
		},
		{
			name: "the ControllerRevision of a group is gone: its revision cannot be compared",
			mutate: func(t *testing.T, in *modelServingInput) {
				change(in, 1, atRevision("gone"))
			},
			want:        map[string]Level{FindingRevisionUnknown: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Revision.Unknown != 1 || report.Groups[1].AtTarget != "unknown" {
					t.Errorf("revision = %+v, group 1 = %+v, want one group that cannot be compared", report.Revision, report.Groups[1])
				}
				if message := findingByID(report.Findings, FindingRevisionUnknown).Message; !strings.Contains(message, "does not exist") {
					t.Errorf("message does not say why: %s", message)
				}
			},
		},
		{
			name: "the Pods of a group carry two revisions and none is updated in place: the group has no revision",
			mutate: func(t *testing.T, in *modelServingInput) {
				updated := rollTo(t, in, "model:v2")
				change(in, 0, atRevision(updated))
				change(in, 2, atRevision(updated))
				atRevision(updated)(podsOf(in, 1)[0])
			},
			want:        map[string]Level{FindingRevisionUnknown: LevelAttention},
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if report.Revision.Unknown != 1 || report.Revision.ToRoll != 0 || len(report.Groups[1].Revisions) != 2 {
					t.Errorf("revision = %+v, group 1 = %+v, want one group of two revisions that is not rolled", report.Revision, report.Groups[1])
				}
				if message := findingByID(report.Findings, FindingRevisionUnknown).Message; !strings.Contains(message, "the Pods of the group carry the revision labels") {
					t.Errorf("message does not say why: %s", message)
				}
			},
		},
		{
			name: "the revision of the template cannot be computed",
			mutate: func(t *testing.T, in *modelServingInput) {
				in.ms.Spec.Template.Roles = nil
			},
			want:        map[string]Level{FindingRevisionUnresolved: LevelBlocked},
			wantVerdict: VerdictBlocked,
		},
		{
			name: "RoleRollingUpdate: the Role instances not at the target are rolled",
			mutate: func(t *testing.T, in *modelServingInput) {
				roleRollingUpdate(in)
				change(in, 0, atRevision(rollTo(t, in, "model:v2")))
			},
			want:        map[string]Level{FindingRolloutInProgress: LevelWait},
			wantVerdict: VerdictWait,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				revision := report.Revision
				if revision.Unit != "role-instance" || revision.Statement != "2 Role instances will be rolled after the upgrade" {
					t.Errorf("revision = %+v, want 2 Role instances rolled", revision)
				}
				if got := findingByID(report.Findings, FindingRolloutInProgress).Objects; !slices.Equal(got, []string{"llama-1/leader-0", "llama-2/leader-0"}) {
					t.Errorf("objects = %v", got)
				}
				if report.Groups[0].AtTarget != "yes" || report.Groups[1].AtTarget != "no" {
					t.Errorf("groups = %+v, want group 0 at the target and group 1 not", report.Groups)
				}
			},
		},
		{
			name: "RoleRollingUpdate: the partition of a Role holds its instances below it",
			mutate: func(t *testing.T, in *modelServingInput) {
				roleRollingUpdate(in)
				in.ms.Spec.Template.Roles[0].Replicas = ptr(int32(2))
				in.ms.Spec.Template.Roles[0].Partition = ptr(intstr.FromInt32(1))
				updated := rollTo(t, in, "model:v2")
				for ordinal := range 3 {
					in.pods = append(in.pods, memberPod(in.ms, ordinal, "leader-1", 0, updated), memberPod(in.ms, ordinal, "leader-1", 1, updated))
				}
			},
			want:        map[string]Level{FindingPartitionHolds: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, in *modelServingInput, report ModelServingReport) {
				if revision := report.Revision; revision.Units != 6 || revision.HeldByPartition != 3 || revision.ToRoll != 0 {
					t.Errorf("revision = %+v, want 6 instances of which 3 are held", revision)
				}
				if report.Rollout.RolePartitions["leader"] != 1 {
					t.Errorf("rolePartitions = %v", report.Rollout.RolePartitions)
				}
			},
		},
	}
}

func TestRevisionFindings(t *testing.T) {
	runModelServingCases(t, revisionCases())
}
