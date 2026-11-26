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
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// heldByARolePartition turns ms into two groups of three instances of the
// Role serve, rolled Role by Role with the lowest instance held at the
// baseline, and one instance of a Role route that no test changes.
func heldByARolePartition(ms *workloadv1alpha1.ModelServing) {
	roleRolling(func(role *workloadv1alpha1.Role) {
		role.MaxSurge = ptr.To(intstr.FromInt32(0))
		role.Partition = ptr.To(intstr.FromInt32(1))
	})(ms)
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
		Name: "route", Replicas: ptr.To[int32](1), EntryTemplate: podTemplate("route:v1"),
	})
}

func threeGroups(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) }

// created lists the groups with the revision each says it was created from,
// under the name the test gives the revision: "0=first 1=second". A group
// that does not say shows "-".
func (s *system) created(names map[string]string) string {
	s.t.Helper()
	var out []string
	for _, group := range s.groups() {
		revision, ok := group.Annotations[workloadv1alpha1.CreatedRevisionAnnotationKey]
		name := "-"
		if ok {
			if name = names[revision]; name == "" {
				name = "(" + revision + ")"
			}
		}
		out = append(out, strings.TrimPrefix(group.Name, testName+"-")+"="+name)
	}
	return strings.Join(out, " ")
}

// A group that is created new under RoleRollingUpdate says which revision it
// was created from: one of the first groups, one that is added, and the new
// object that takes the place of a group that failed.
func TestANewGroupSaysWhichRevisionItWasCreatedFrom(t *testing.T) {
	s := newSystem(t, modelServing(heldByARolePartition, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
	}))
	s.settle()
	names := map[string]string{s.modelServing().Status.UpdateRevision: "first"}
	if got, want := s.created(names), "0=first 1=first"; got != want {
		t.Fatalf("created: %s, want %s", got, want)
	}

	s.update(threeGroups)
	s.settle()
	if got, want := s.created(names), "0=first 1=first 2=first"; got != want {
		t.Errorf("after a group was added: %s, want %s", got, want)
	}

	// A group that failed is replaced by a new object of the same name. It is
	// a new group: nothing of the one before is left.
	failed := s.groups()[0]
	if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), "llama-0-serve-1-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()
	if replaced := s.groups()[0]; replaced.Name != failed.Name || replaced.UID == failed.UID {
		t.Fatalf("group %s has UID %s, before %s: want the same name and a new object", replaced.Name, replaced.UID, failed.UID)
	}
	if got, want := s.created(names), "0=first 1=first 2=first"; got != want {
		t.Errorf("after a group was replaced: %s, want %s", got, want)
	}
}

// Under ServingGroupRollingUpdate the spec of a group names the revision the
// group runs as a whole. No group says more than that, a surge group neither.
func TestAGroupThatIsReplacedAsAWholeDoesNotSayWhereItComesFrom(t *testing.T) {
	s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxSurge = ptr.To(intstrOne)
	})))
	surge := false
	s.invariant = func() {
		for _, group := range s.groups() {
			surge = surge || group.Annotations[workloadv1alpha1.SurgeAnnotationKey] != ""
			if revision, ok := group.Annotations[workloadv1alpha1.CreatedRevisionAnnotationKey]; ok {
				t.Fatalf("group %s says that it was created from %q", group.Name, revision)
			}
		}
	}
	s.settle()
	s.update(setImage("model:v2"))
	s.settle()
	if got, want := s.images(), "0=model:v2 1=model:v2 2=model:v2"; got != want || !surge {
		t.Fatalf("rolled: %s, a surge group was seen: %t; want %s with a surge group", got, surge, want)
	}
}

// An object that is created for Pods that exist stands for a group that ran
// before the object was there. It does not say that it is new.
func TestAnObjectForPodsThatExistDoesNotSayWhereItComesFrom(t *testing.T) {
	t.Run("the object of a group was deleted and its Pods were left", func(t *testing.T) {
		s := newSystem(t, modelServing(heldByARolePartition))
		s.settle()
		first := s.modelServing().Status.UpdateRevision
		lost, pods := s.groups()[1], s.podUIDs()

		err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), lost.Name,
			metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationOrphan)})
		if err != nil {
			t.Fatal(err)
		}
		s.settle()
		if found := s.groups()[1]; found.Name != lost.Name || found.UID == lost.UID {
			t.Fatalf("group %s has UID %s, before %s: want the same name and a new object", found.Name, found.UID, lost.UID)
		}
		if after := s.podUIDs(); !reflect.DeepEqual(after, pods) {
			t.Fatalf("Pods were replaced: %v, before %v", after, pods)
		}
		if got, want := s.created(map[string]string{first: "first"}), "0=first 1=-"; got != want {
			t.Errorf("after the object was found back: %s, want %s", got, want)
		}
	})

	t.Run("workloads of an earlier version are taken over", func(t *testing.T) {
		ms := modelServing(func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		})
		ms.UID = "ms-uid"
		ms.Status = workloadv1alpha1.ModelServingStatus{
			Replicas: 3, UpdatedReplicas: 3, CurrentReplicas: 3, AvailableReplicas: 3,
			CurrentRevision: "old-identity", UpdateRevision: "old-identity",
		}
		objects := []runtime.Object{ms, legacyRevision(t, ms, "old-identity", false)}
		for ordinal := range 3 {
			objects = append(objects, legacyPods(ms, ordinal, "old-identity", "model:v1", false)...)
		}
		s := newSystem(t, objects...)
		pods := s.podUIDs()
		s.settle()
		if got := s.modelServing().Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; got != workloadv1alpha1.ServingGroupAdoptionDone {
			t.Fatalf("adoption annotation = %q, want Done", got)
		}
		if after := s.podUIDs(); !reflect.DeepEqual(after, pods) {
			t.Fatalf("Pods were replaced: %v, before %v", after, pods)
		}
		names := map[string]string{"old-identity": "old"}
		if got, want := s.created(names), "0=- 1=- 2=-"; got != want {
			t.Errorf("after the take-over: %s, want %s", got, want)
		}

		// A group that is added afterwards is new.
		s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](4) })
		s.settle()
		if got, want := s.created(names), "0=- 1=- 2=- 3=old"; got != want {
			t.Errorf("after a group was added: %s, want %s", got, want)
		}
	})
}

// The write that tells a group its spec takes the annotation away exactly
// when the target it tells is another revision than the one the annotation
// names. It never adds it.
func TestTheWriteThatTellsAGroupAnotherTarget(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	surge := map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup, "example.com/note": "kept"}
	with := func(annotations map[string]string, revision string) map[string]string {
		out := maps.Clone(annotations)
		out[created] = revision
		return out
	}
	tests := []struct {
		name        string
		annotations map[string]string
		// target is the target the group has, told the one it is told.
		target, told string
		want         map[string]string
	}{
		{name: "another target: the group stops saying where it comes from, and keeps what else it carries",
			annotations: with(surge, "rev-b"), target: "rev-b", told: "rev-c", want: surge},
		{name: "the same target with other settings: it still says so",
			annotations: with(surge, "rev-b"), target: "rev-b", told: "rev-b", want: with(surge, "rev-b")},
		{name: "the target returns to the revision the group was created from: nothing is added",
			annotations: surge, target: "rev-c", told: "rev-b", want: surge},
		{name: "a group without annotations gets none", target: "rev-a", told: "rev-b"},
		{name: "an annotation that names another revision than the target the group has goes with the next write",
			annotations: with(surge, "rev-a"), target: "rev-b", told: "rev-b", want: surge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := &workloadv1alpha1.ServingGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "llama-0", Annotations: maps.Clone(tt.annotations)},
				Spec:       workloadv1alpha1.ServingGroupSpec{TargetRevision: tt.target, BaselineRevision: "rev-a"},
			}
			spec := workloadv1alpha1.ServingGroupSpec{TargetRevision: tt.told, BaselineRevision: "rev-a", RecoveryPolicy: workloadv1alpha1.NoneRestartPolicy}

			written := told(group, spec)
			if !reflect.DeepEqual(written.Spec, spec) {
				t.Errorf("spec = %+v, want %+v", written.Spec, spec)
			}
			if !maps.Equal(written.Annotations, tt.want) {
				t.Errorf("annotations = %v, want %v", written.Annotations, tt.want)
			}
			if !maps.Equal(group.Annotations, tt.annotations) || group.Spec.TargetRevision != tt.target {
				t.Errorf("the group that was read was changed: %v, target %s", group.Annotations, group.Spec.TargetRevision)
			}
		})
	}
}

// The target of a group and what the group says about where it comes from
// change in one write. Wherever a round is cut off, no group is left with
// the new target beside the annotation: a group that found the two together
// would take itself for one that was born at the new target.
func TestTheTargetAndTheAnnotationOfAGroupChangeInOneWrite(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	// The groups are created Role by Role. Then they are replaced as a whole
	// and in place: the target of a group changes in the write that starts
	// its update, and in the hand-down for the others.
	inPlace := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
			Type:                       workloadv1alpha1.ServingGroupRollingUpdate,
			UpdateMethod:               workloadv1alpha1.InPlaceUpdateMethod,
			RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt32(1))},
		}
	}
	tests := []struct {
		name string
		// then, when set, changes the ModelServing after its groups were created.
		then func(*workloadv1alpha1.ModelServing)
		// write is a write of the round that changes the target of a group.
		write string
	}{
		{name: "the settings are handed down", write: "update servinggroups default/llama-0"},
		{name: "a group is updated in place", then: inPlace, write: "update servinggroups default/llama-2"},
	}
	cut := errors.New("the round was cut off")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for stopAt := 1; ; stopAt++ {
				s := newSystem(t, modelServing(roleRolling(), threeGroups))
				s.settleContainers()
				if tt.then != nil {
					s.update(tt.then)
					s.settleContainers()
				}
				first := s.modelServing().Status.UpdateRevision
				s.update(setImage("model:v2"))
				s.cluster.ResetWrites()

				writes := 0
				s.cluster.BeforeWrite = func(apitestWrite) error {
					if writes++; writes > stopAt {
						return cut
					}
					return nil
				}
				if _, err := s.roundOfTheModelServing(); err != nil && !errors.Is(err, cut) && !errors.Is(err, ErrNotSettled) {
					t.Fatalf("stop after write %d: round: %v", stopAt, err)
				}
				s.cluster.BeforeWrite = nil

				told := 0
				for _, group := range s.groups() {
					revision, says := group.Annotations[created]
					switch target := group.Spec.TargetRevision; {
					case target == first && says && revision == first:
					case target != first && !says:
						told++
					default:
						t.Errorf("stop after write %d: group %s has the target %s and says that it was created from %q (%t); the first revision is %s",
							stopAt, group.Name, target, revision, says, first)
					}
				}
				if writes <= stopAt {
					// The round was not cut off: every write of it was made.
					var made []string
					for _, write := range s.cluster.Writes() {
						made = append(made, write.String())
					}
					if told != 3 || !slices.Contains(made, tt.write) {
						t.Errorf("the whole round told %d of 3 groups the target with the writes %v; want all three, %q among the writes", told, made, tt.write)
					}
					return
				}

				// The rounds that follow repeat what was left.
				s.settleContainers()
				for _, group := range s.groups() {
					if _, says := group.Annotations[created]; says || group.Spec.TargetRevision == first {
						t.Errorf("stop after write %d: after the next rounds group %s has the target %s and the annotations %v",
							stopAt, group.Name, group.Spec.TargetRevision, group.Annotations)
					}
				}
			}
		})
	}
}

// A group says which revision it was created from for as long as that
// revision is its target: it was born in the rollout that is under way. When
// it is told another target it stops saying so, and it never says it again,
// whatever the template returns to.
func TestAGroupThatIsToldAnotherTargetStopsSayingWhereItComesFrom(t *testing.T) {
	s := newSystem(t, modelServing(heldByARolePartition))
	s.settle()
	first := s.modelServing().Status.UpdateRevision
	names := map[string]string{first: "first"}
	if got, want := s.created(names), "0=first 1=first"; got != want {
		t.Fatalf("created: %s, want %s", got, want)
	}

	// The template changes and a group is added. The groups that were there
	// are told the new target; the new one is born at it.
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		setImage("model:v2")(ms)
		threeGroups(ms)
	})
	s.settle()
	second := s.modelServing().Status.UpdateRevision
	names[second] = "second"
	if got, want := s.created(names), "0=- 1=- 2=second"; got != want || second == first {
		t.Fatalf("after the target changed: %s, want %s", got, want)
	}

	// A setting that is no part of a revision is handed down in the same
	// kind of write. The target stays, and so does what the new group says.
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
	})
	s.settle()
	for _, group := range s.groups() {
		if group.Spec.RecoveryPolicy != workloadv1alpha1.ServingGroupRecreate {
			t.Fatalf("group %s was not told the recovery policy", group.Name)
		}
	}
	if got, want := s.created(names), "0=- 1=- 2=second"; got != want {
		t.Errorf("after another setting was handed down: %s, want %s", got, want)
	}

	// The replacement of a group that failed is born at the target of now.
	if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), "llama-0-serve-1-1", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()
	if got, want := s.created(names), "0=second 1=- 2=second"; got != want {
		t.Errorf("after a group was replaced: %s, want %s", got, want)
	}

	// Only this controller sets the annotation, and never beside another
	// target. One that somebody else put there is taken away.
	foreign := s.groups()[1]
	foreign.Annotations = map[string]string{workloadv1alpha1.CreatedRevisionAnnotationKey: first}
	if _, err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(context.Background(), foreign, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	s.settle()
	if got, want := s.created(names), "0=second 1=- 2=second"; got != want {
		t.Errorf("after an annotation was put on a group by hand: %s, want %s", got, want)
	}

	// The template moves on, and then returns to the revision two of the
	// groups were born at. They are groups that ran before by then.
	s.update(setImage("model:v3"))
	s.settle()
	if got, want := s.created(names), "0=- 1=- 2=-"; got != want {
		t.Errorf("after the target moved on: %s, want %s", got, want)
	}
	s.update(setImage("model:v2"))
	s.settle()
	if target := s.modelServing().Status.UpdateRevision; target != second {
		t.Fatalf("the template that was restored has the revision %s, want the one it had: %s", target, second)
	}
	if got, want := s.created(names), "0=- 1=- 2=-"; got != want {
		t.Errorf("after the target returned: %s, want %s", got, want)
	}
}
