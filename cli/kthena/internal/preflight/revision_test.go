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
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// handMade returns a ControllerRevision of ms that records the template of
// source under an identity of the caller's choice.
func handMade(t *testing.T, ms, source *workloadv1alpha1.ModelServing, identity string, revision int64) *appsv1.ControllerRevision {
	t.Helper()
	data, err := utils.BuildRevisionData(source)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: utils.GenerateControllerRevisionName(ms.Name, identity),
			Labels: map[string]string{
				utils.ControllerRevisionLabelKey:         ms.Name,
				utils.ControllerRevisionRevisionLabelKey: identity,
			},
			Annotations:     map[string]string{utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)},
		},
		Revision: revision,
		Data:     runtime.RawExtension{Raw: data},
	}
}

// TestResolveTargetAgreesWithTheController checks the prediction against the
// function the new ModelServing controller obtains its target revision from:
// for the same ModelServing and the same ControllerRevisions both must name
// the same revision.
func TestResolveTargetAgreesWithTheController(t *testing.T) {
	tests := []struct {
		name string
		// existing returns the ControllerRevisions that exist for ms.
		existing func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision
		// wantExisting: the identity of an existing ControllerRevision is reused.
		wantExisting bool
		wantIdentity string
		// wantCollision: the identity is not the one the template alone gives.
		wantCollision bool
	}{
		{
			name:     "no revision is recorded: the identity is computed from the template",
			existing: func(*testing.T, *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision { return nil },
		},
		{
			name: "the template is recorded: its revision is reused",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				return []*appsv1.ControllerRevision{record(t, ms)}
			},
			wantExisting: true,
		},
		{
			name: "only another template is recorded: a new identity",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				other := ms.DeepCopy()
				setImage(other, "model:v0")
				return []*appsv1.ControllerRevision{record(t, other)}
			},
		},
		{
			name: "the template was restored: it gets its old identity back",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				first := record(t, ms)
				changed := ms.DeepCopy()
				setImage(changed, "model:v2")
				second := record(t, changed, first)
				return []*appsv1.ControllerRevision{first, second}
			},
			wantExisting: true,
		},
		{
			name: "the same Pods recorded under an identity another version computed: reused",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				return []*appsv1.ControllerRevision{handMade(t, ms, ms, "0ldidentity", 4)}
			},
			wantExisting: true,
			wantIdentity: "0ldidentity",
		},
		{
			name: "a revision recorded in the format before v1 revision data: reused",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				client := kubefake.NewSimpleClientset()
				revision, err := utils.CreateControllerRevision(context.Background(), client, ms, "legacy1", ms.Spec.Template.Roles)
				if err != nil {
					t.Fatalf("CreateControllerRevision() error = %v", err)
				}
				return []*appsv1.ControllerRevision{revision}
			},
			wantExisting: true,
			wantIdentity: "legacy1",
		},
		{
			name: "two revisions render the same Pods: the later one",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				return []*appsv1.ControllerRevision{handMade(t, ms, ms, "first", 1), handMade(t, ms, ms, "second", 2)}
			},
			wantExisting: true,
			wantIdentity: "second",
		},
		{
			name: "the name is taken by a ModelServing that was deleted: the collision is counted",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				earlier := ms.DeepCopy()
				earlier.UID = "uid-of-the-deleted-one"
				return []*appsv1.ControllerRevision{record(t, earlier)}
			},
			wantCollision: true,
		},
		{
			name: "a revision with a data version this code does not know: not reused",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				revision := handMade(t, ms, ms, "future", 1)
				revision.Annotations[utils.ControllerRevisionDataVersionAnnotation] = "v9"
				return []*appsv1.ControllerRevision{revision}
			},
		},
		{
			name: "a revision whose name does not carry its identity: not reused",
			existing: func(t *testing.T, ms *workloadv1alpha1.ModelServing) []*appsv1.ControllerRevision {
				revision := handMade(t, ms, ms, "renamed", 1)
				revision.Labels[utils.ControllerRevisionRevisionLabelKey] = "other"
				return []*appsv1.ControllerRevision{revision}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newModelServing("llama", 3)
			existing := tt.existing(t, ms)

			got, err := resolveTarget(ms, existing)
			if err != nil {
				t.Fatalf("resolveTarget() error = %v", err)
			}
			want := identityOf(record(t, ms, existing...))
			if got.identity != want {
				t.Errorf("resolveTarget() identity = %q, the controller records %q", got.identity, want)
			}
			if tt.wantIdentity != "" && got.identity != tt.wantIdentity {
				t.Errorf("resolveTarget() identity = %q, want %q", got.identity, tt.wantIdentity)
			}
			if got.existing != tt.wantExisting {
				t.Errorf("resolveTarget() existing = %t, want %t", got.existing, tt.wantExisting)
			}
			if collided := got.identity != utils.ModelServingRevision(ms); !tt.wantExisting && collided != tt.wantCollision {
				t.Errorf("resolveTarget() identity %q differs from the hash of the template: %t, want %t", got.identity, collided, tt.wantCollision)
			}
			if got.name != utils.GenerateControllerRevisionName(ms.Name, got.identity) {
				t.Errorf("resolveTarget() name = %q, does not carry the identity %q", got.name, got.identity)
			}
			// Whatever the target is, the history must be able to read it.
			if result := got.history.Compare(got.identity, got.identity); result != workload.Equivalent {
				t.Errorf("history.Compare(target, target) = %s, want Equivalent", result)
			}
			if _, err := got.history.At(got.identity); err != nil {
				t.Errorf("history.At(target) error = %v", err)
			}
		})
	}
}

func TestResolveTargetComparesPodsWithTheTarget(t *testing.T) {
	ms := newModelServing("llama", 3)
	old := ms.DeepCopy()
	setImage(old, "model:v0")
	oldRevision := record(t, old)
	renamed := handMade(t, ms, ms, "0ldidentity", 7)

	got, err := resolveTarget(ms, []*appsv1.ControllerRevision{oldRevision, renamed})
	if err != nil {
		t.Fatalf("resolveTarget() error = %v", err)
	}
	tests := []struct {
		name     string
		revision string
		want     workload.Comparison
	}{
		{"the target itself", got.identity, workload.Equivalent},
		{"a revision of another template", identityOf(oldRevision), workload.Different},
		{"a revision that is not recorded", "gone", workload.Unknown},
		{"no revision", "", workload.Unknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if result := got.history.Compare(tt.revision, got.identity); result != tt.want {
				t.Errorf("Compare(%q, target) = %s, want %s", tt.revision, result, tt.want)
			}
		})
	}
	if reason := got.why("gone"); reason == "" {
		t.Errorf("why() gave no reason for a revision that is not recorded")
	}
}

func TestResolveTargetRejectsATemplateWithoutRoles(t *testing.T) {
	ms := newModelServing("llama", 3)
	ms.Spec.Template.Roles = nil
	if _, err := resolveTarget(ms, nil); err == nil {
		t.Fatalf("resolveTarget() succeeded for a template without a Role")
	}
}

func TestRunningRevision(t *testing.T) {
	ms := newModelServing("llama", 1)
	started := metav1.NewTime(testTime.Add(-time.Minute)).Format(time.RFC3339)
	tests := []struct {
		name   string
		mutate func(*corev1.Pod)
		want   string
	}{
		{name: "the revision label", mutate: func(*corev1.Pod) {}, want: "labelled"},
		{
			name: "the revision an unfinished in-place update leads to",
			mutate: annotate(workloadv1alpha1.InPlaceUpdateStateAnnotationKey,
				`{"revision":"next","startedAt":"`+started+`","patched":{"model":{"image":"model:v2"}}}`),
			want: "next",
		},
		{
			name: "the label again once the in-place update is done",
			mutate: annotate(workloadv1alpha1.InPlaceUpdateStateAnnotationKey,
				`{"revision":"next","startedAt":"`+started+`","done":true}`),
			want: "labelled",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := memberPod(ms, 0, "leader-0", 0, "labelled", tt.mutate)
			if got := runningRevision(pod); got != tt.want {
				t.Errorf("runningRevision() = %q, want %q", got, tt.want)
			}
		})
	}
}
