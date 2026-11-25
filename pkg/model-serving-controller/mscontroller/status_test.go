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

package mscontroller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// statusFixture is what a status test starts from: a ModelServing, the
// revision of its template and the groups it has. Every group runs the Pods
// of its revision, all of them ready, and has reported on them.
type statusFixture struct {
	// mutate changes the ModelServing, which is the one of modelServing: three
	// groups of one Role, replaced group by group, one unavailable at a time.
	mutate []func(*workloadv1alpha1.ModelServing)
	// current is status.currentRevision before the round.
	current string
	// target is the identity under which the template is recorded.
	target string
	// groups holds the revision of the group at every ordinal; "" for an
	// ordinal without a group.
	groups []string
	// surge lists the ordinals of the groups that carry the surge mark.
	surge []int
	// occupied lists the ordinals whose name an object holds that is not a
	// group of the ModelServing.
	occupied []int
	// handedDown is the baseline the groups were told, when the status does
	// not have it any more.
	handedDown string
	// legacy: the groups are the Pods an earlier version of the controller
	// left behind. There are no group objects.
	legacy bool
}

func ofGroups(count int32) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To(count) }
}

// calledAs gives the ModelServing a name and its Role the name prefill.
func calledAs(name string) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		ms.Name = name
		ms.Spec.Template.Roles[0].Name = "prefill"
	}
}

// settleGroups runs the ServingGroup controller alone until it writes
// nothing: every group creates its Pods, the kubelet gets them ready, and the
// group reports on them.
func (s *system) settleGroups() {
	s.t.Helper()
	for range 20 {
		s.kubelet()
		s.cluster.Settle()
		before := len(s.cluster.Writes())
		for _, group := range s.groups() {
			caches, snapshot := s.caches()
			s.sg.SetCaches(caches, snapshot.Listers().Services)
			if _, err := s.sg.Reconcile(context.Background(), testNamespace, group.Name); err != nil {
				s.t.Fatalf("round of ServingGroup %s: %v", group.Name, err)
			}
		}
		if len(s.cluster.Writes()) == before {
			return
		}
	}
	s.t.Fatal("the groups did not settle")
}

// status builds the fixture, runs one round of the ModelServing controller
// on it and returns the status of the ModelServing after that round. The
// status of a round is summed up from what the round read, not from what it
// went on to create or delete.
func (f statusFixture) status(t *testing.T) workloadv1alpha1.ModelServingStatus {
	t.Helper()
	ms := modelServing(f.mutate...)
	ms.UID, ms.Generation = "ms-uid", 1
	ms.Status.CurrentRevision = f.current
	if !f.legacy {
		// Nothing of an earlier version is left to take over.
		ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionDone}
	}
	baseline := f.current
	if baseline == "" {
		baseline = f.handedDown
	}

	// The template is recorded as the target. Every other revision a group
	// runs is the template with another image.
	objects := []runtime.Object{ms, legacyRevision(t, ms, f.target, false)}
	images := map[string]string{f.target: ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image}
	for ordinal, revision := range f.groups {
		if revision == "" {
			continue
		}
		if images[revision] == "" {
			images[revision] = "model:" + revision
			earlier := ms.DeepCopy()
			setImage(images[revision])(earlier)
			objects = append(objects, legacyRevision(t, earlier, revision, false))
		}
		marked := slices.Contains(f.surge, ordinal)
		if f.legacy {
			objects = append(objects, legacyPods(ms, ordinal, revision, images[revision], marked)...)
			continue
		}
		group := &workloadv1alpha1.ServingGroup{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ms.Namespace, Name: utils.GenerateServingGroupName(ms.Name, ordinal),
				Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name},
				OwnerReferences: []metav1.OwnerReference{modelServingOwner(ms, true)},
			},
			Spec: workload.GroupSpec(ms, workload.Settings{
				Revision: revision, TargetRevision: f.target, BaselineRevision: baseline,
				UpdateMethod: workloadv1alpha1.RecreateUpdateMethod, ExpectedPods: workload.ExpectedPods(ms),
			}),
		}
		if marked {
			group.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
		}
		objects = append(objects, group)
	}

	for _, ordinal := range f.occupied {
		objects = append(objects, &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: utils.GenerateServingGroupName(ms.Name, ordinal),
		}})
	}

	s := newSystem(t, objects...)
	s.settleGroups()
	for _, group := range s.groups() {
		if metav1.GetControllerOf(group) != nil && !groupReady(group) {
			t.Fatalf("fixture: group %s is not ready", group.Name)
		}
	}
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), ms.Namespace, ms.Name); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatalf("round of the ModelServing: %v", err)
	}
	written, err := s.cluster.GetModelServing(ms.Namespace, ms.Name)
	if err != nil {
		t.Fatal(err)
	}
	return written.Status
}

func countsOf(status workloadv1alpha1.ModelServingStatus) string {
	return fmt.Sprintf("replicas=%d updated=%d current=%d available=%d", status.Replicas, status.UpdatedReplicas, status.CurrentReplicas, status.AvailableReplicas)
}

// trueConditions lists the conditions that are true: "Progressing,UpdateInProgress".
func trueConditions(status workloadv1alpha1.ModelServingStatus) string {
	var out []string
	for _, condition := range status.Conditions {
		if condition.Status == metav1.ConditionTrue {
			out = append(out, condition.Type)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

// The status of a ModelServing says what it said before its groups were
// objects. The cases are the ones the status function of the controller
// without group objects was tested with, with their expectations: which of
// Available, Progressing and UpdateInProgress is true while the ModelServing
// is scaled out, scaled in and rolled, the four counts, and the two revisions.
//
// A user, an autoscaler and a GitOps tool wait on these fields. A count or a
// condition that means something else after an upgrade of the controller
// makes them wait forever, or go on too early.
func TestStatusOfAModelServing(t *testing.T) {
	surgeOnly := rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxSurge, config.MaxUnavailable = ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromInt32(0))
	})
	tests := []struct {
		name    string
		fixture statusFixture
		// counts, when not empty, is what countsOf returns.
		counts string
		// condition, when not empty, lists the conditions that are true.
		condition string
		// currentRevision and updateRevision, when not empty, are the revisions of the status.
		currentRevision, updateRevision string
	}{
		// Scaling is progress and no update; a rollout is an update.
		{
			name:      "scaling out is progressing",
			fixture:   statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(2)}, target: "current", groups: []string{"current"}, handedDown: "current"},
			counts:    "replicas=1 updated=1 current=1 available=1",
			condition: "Progressing",
		},
		{
			name:      "scaling in is progressing",
			fixture:   statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(1)}, target: "current", groups: []string{"current", "current"}, handedDown: "current"},
			counts:    "replicas=2 updated=2 current=2 available=2",
			condition: "Progressing",
		},
		{
			name:      "a rollout is an update in progress",
			fixture:   statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(2)}, target: "current", groups: []string{"old", "current"}, handedDown: "old"},
			counts:    "replicas=2 updated=1 current=1 available=2",
			condition: "UpdateInProgress",
		},

		// Every group counts, also one more than declared. Three groups of the
		// new revision where two are declared: the rollout is not over, and
		// the baseline does not advance, until the third is gone.
		{
			name: "a group more than declared is counted, and holds the baseline",
			fixture: statusFixture{
				mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(2)}, current: "old-revision", target: "new-revision",
				groups: []string{"new-revision", "new-revision", "new-revision"},
			},
			counts:          "replicas=3 updated=3 current=0 available=3",
			condition:       "Progressing",
			currentRevision: "old-revision", updateRevision: "new-revision",
		},
		// The last group of the old revision is gone and its ordinal is not
		// filled yet. The counts look like the end of the rollout, and
		// currentReplicas follows updatedReplicas as it does at the end of one.
		// It is not over while the surge group is left.
		{
			name: "a surge group that is left keeps the rollout open",
			fixture: statusFixture{
				mutate: []func(*workloadv1alpha1.ModelServing){surgeOnly}, current: "old", target: "new",
				groups: []string{"", "new", "new", "new"}, surge: []int{3},
			},
			counts:          "replicas=3 updated=3 current=3 available=3",
			condition:       "UpdateInProgress",
			currentRevision: "old", updateRevision: "new",
		},

		// The same when the surge group is one of the declared number: no
		// group can be created beside it and it cannot go, so nothing in the
		// counts tells that the rollout is not over.
		{
			name: "a surge group among the declared number keeps the rollout open",
			fixture: statusFixture{
				mutate: []func(*workloadv1alpha1.ModelServing){rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
					config.MaxSurge, config.MaxUnavailable = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(0))
				})},
				current: "old", target: "new", groups: []string{"new", "new", "", "new"}, surge: []int{3},
			},
			counts:          "replicas=3 updated=3 current=3 available=3",
			condition:       "UpdateInProgress",
			currentRevision: "old", updateRevision: "new",
		},
		// A group is missing and cannot be created: every group that exists
		// is updated and ready, and the rollout is not over.
		{
			name: "a group fewer than declared holds the baseline",
			fixture: statusFixture{
				current: "old", target: "new", groups: []string{"new", "new"}, occupied: []int{2},
			},
			counts:          "replicas=2 updated=2 current=0 available=2",
			condition:       "Progressing,RolloutBlocked",
			currentRevision: "old", updateRevision: "new",
		},

		// The two revisions.
		{
			name: "the baseline of the status is kept during a rollout",
			fixture: statusFixture{
				current: "revision-v1", target: "revision-v2",
				groups: []string{"revision-v1", "revision-v1", "revision-v2"},
			},
			counts:          "replicas=3 updated=1 current=2 available=3",
			condition:       "UpdateInProgress",
			currentRevision: "revision-v1", updateRevision: "revision-v2",
		},
		{
			name: "every group is updated and ready: the baseline advances",
			fixture: statusFixture{
				current: "revision-v1", target: "revision-v2",
				groups: []string{"revision-v2", "revision-v2", "revision-v2"},
			},
			counts:          "replicas=3 updated=3 current=3 available=3",
			condition:       "Available",
			currentRevision: "revision-v2", updateRevision: "revision-v2",
		},
		{
			name:            "without a group the baseline is the target",
			fixture:         statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(0)}, target: "revision-v1"},
			counts:          "replicas=0 updated=0 current=0 available=0",
			condition:       "Available",
			currentRevision: "revision-v1", updateRevision: "revision-v1",
		},
		// A status without a baseline. The groups an earlier version left
		// behind tell it: the revision most of them run, the target left aside.
		{
			name: "no baseline in the status: the revision of the groups of an earlier version",
			fixture: statusFixture{
				legacy: true, target: "revision-v2",
				groups: []string{"revision-v1", "revision-v1", "revision-v1"},
			},
			currentRevision: "revision-v1", updateRevision: "revision-v2",
		},
		{
			name: "no baseline in the status: the revision most groups of an earlier version run",
			fixture: statusFixture{
				legacy: true, mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(4)}, target: "revision-v2",
				groups: []string{"revision-v1", "revision-v1", "revision-v0", "revision-v2"},
			},
			currentRevision: "revision-v1", updateRevision: "revision-v2",
		},
		// Groups that are objects were told the baseline. It is read back
		// from them, whatever most of them run.
		{
			name: "no baseline in the status: the one the groups were told",
			fixture: statusFixture{
				mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(4)}, target: "revision-v2", handedDown: "revision-v0",
				groups: []string{"revision-v1", "revision-v1", "revision-v0", "revision-v2"},
			},
			counts:          "replicas=4 updated=1 current=1 available=4",
			condition:       "UpdateInProgress",
			currentRevision: "revision-v0", updateRevision: "revision-v2",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status := tt.fixture.status(t)
			if got := countsOf(status); tt.counts != "" && got != tt.counts {
				t.Errorf("counts = %s, want %s", got, tt.counts)
			}
			if got := trueConditions(status); tt.condition != "" && got != tt.condition {
				t.Errorf("conditions that are true = %s, want %s", got, tt.condition)
			}
			if tt.currentRevision != "" && status.CurrentRevision != tt.currentRevision {
				t.Errorf("currentRevision = %q, want %q", status.CurrentRevision, tt.currentRevision)
			}
			if tt.updateRevision != "" && status.UpdateRevision != tt.updateRevision {
				t.Errorf("updateRevision = %q, want %q", status.UpdateRevision, tt.updateRevision)
			}
			// What the status says while workloads are taken over is not the
			// subject here, apart from the two revisions.
			if !tt.fixture.legacy && status.ObservedGeneration != 1 {
				t.Errorf("observedGeneration = %d, want 1", status.ObservedGeneration)
			}
		})
	}
}

// status.labelSelector is what the scale subresource reports to an
// autoscaler. spec.replicas counts groups, so the selector matches one Pod of
// every group: the entry Pod of the first instance of the first Role. It is
// there before the first group is, and it is built from the names as they
// are.
func TestLabelSelectorOfTheStatus(t *testing.T) {
	tests := []struct {
		name    string
		msName  string
		fixture statusFixture
	}{
		{
			name:    "no group yet",
			msName:  "my-llm",
			fixture: statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(1)}, target: "rev-1"},
		},
		{
			name:    "with groups",
			msName:  "my-llm",
			fixture: statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(2)}, current: "rev-1", target: "rev-1", groups: []string{"rev-1", "rev-1"}},
		},
		{
			name:    "a name with digits and dashes",
			msName:  "serving-gpt-4o-mini",
			fixture: statusFixture{mutate: []func(*workloadv1alpha1.ModelServing){ofGroups(1)}, current: "rev-abc", target: "rev-abc", groups: []string{"rev-abc"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.fixture.mutate = append(tt.fixture.mutate, calledAs(tt.msName))
			status := tt.fixture.status(t)
			want := labels.Set{
				workloadv1alpha1.ModelServingNameLabelKey: tt.msName,
				workloadv1alpha1.EntryLabelKey:            utils.Entry,
				workloadv1alpha1.RoleLabelKey:             "prefill",
				workloadv1alpha1.RoleIDKey:                utils.GenerateRoleID("prefill", 0),
			}.String()
			if status.LabelSelector != want {
				t.Errorf("labelSelector = %q, want %q", status.LabelSelector, want)
			}
		})
	}
}

// Under RoleRollingUpdate the temporary capacity of a rollout is an instance
// of a Role inside a group. When the last instance of the old revision is
// replaced, every Pod of the group runs the target and the counts match. The
// rollout is not over while the temporary instance is left: the baseline
// stays, and UpdateInProgress stays true.
func TestARoleSurgeInstanceThatIsLeftKeepsTheRolloutOpen(t *testing.T) {
	s := newSystem(t, modelServing(roleRolling(func(role *workloadv1alpha1.Role) {
		role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
	}), ofGroups(1)))
	s.settle()
	baseline := s.modelServing().Status.CurrentRevision

	// rolled: every instance runs the new image and the temporary one is still there.
	rolled := false
	s.invariant = func() {
		instances := s.instances()
		if !strings.Contains(instances, "+") {
			return
		}
		rolled = rolled || instances == "0[0=model:v2 1=model:v2 2=model:v2 3=model:v2+]"
		status := s.modelServing().Status
		if status.CurrentRevision != baseline || !strings.Contains(trueConditions(status), string(workloadv1alpha1.ModelServingUpdateInProgress)) {
			t.Fatalf("a temporary instance is left (%s) and the status says the rollout is over: currentRevision %s (before %s), %s %s",
				instances, status.CurrentRevision, baseline, countsOf(status), trueConditions(status))
		}
	}
	s.update(setImage("model:v2"))
	s.settle()
	s.invariant = nil

	if !rolled {
		t.Fatal("the rollout never had every instance at the new image beside the temporary one")
	}
	status := s.modelServing().Status
	if got, want := s.instances()+" "+countsOf(status)+" "+trueConditions(status), "0[0=model:v2 1=model:v2 2=model:v2] replicas=1 updated=1 current=1 available=1 Available"; got != want {
		t.Errorf("after the rollout:\n got %s\nwant %s", got, want)
	}
	if status.CurrentRevision != status.UpdateRevision || status.CurrentRevision == baseline {
		t.Errorf("revisions after the rollout: current %s, update %s, before %s", status.CurrentRevision, status.UpdateRevision, baseline)
	}
}
