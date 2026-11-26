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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
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
	// a new group: nothing of the one before is left. Admission rejects
	// ServingGroupRecreate together with RoleRollingUpdate, so with the
	// webhook in place a group of this strategy does not fail; the controller
	// is tested for a ModelServing that got past it.
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

// The write that tells a group its spec changes what the group says about
// where it comes from exactly when the target it tells is another revision
// than the one the annotation names. A group that has built something, an
// instance of it was established or it has a Pod, stops saying it, whole or
// not. A group that has built nothing is still being born, and says the new
// target. The annotation is never added to a group that has none. After the
// write the annotation is gone or names the target, so the group is not
// written again for it.
//
// All of that is for a group that is rolled Role by Role. A group that is
// told that it is replaced as a whole stops saying where it comes from,
// whatever its target is.
func TestTheWriteThatTellsAGroupAnotherTarget(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	surge := map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup, "example.com/note": "kept"}
	with := func(annotations map[string]string, revision string) map[string]string {
		out := maps.Clone(annotations)
		out[created] = revision
		return out
	}
	// The group has two instances of serve. These are its ledgers.
	ledger := func(entries ...workloadv1alpha1.RoleInstanceStatus) []workloadv1alpha1.RoleInstanceStatus {
		return entries
	}
	entry := func(index int32, phase workloadv1alpha1.RoleInstancePhase, generation int64) workloadv1alpha1.RoleInstanceStatus {
		return workloadv1alpha1.RoleInstanceStatus{Role: "serve", Index: index, Phase: phase, Generation: generation}
	}
	var (
		nothing = ledger()
		// an entry that never got further than Creating.
		begun = ledger(entry(0, workloadv1alpha1.RoleInstanceCreating, 0))
		// one instance is established, the other does not exist yet.
		partly = ledger(entry(1, workloadv1alpha1.RoleInstanceEstablished, 0))
		whole  = ledger(entry(0, workloadv1alpha1.RoleInstanceEstablished, 0), entry(1, workloadv1alpha1.RoleInstanceEstablished, 0))
		// the only entry is of an instance that was lost: it is being
		// drained, or is created again after the drain.
		draining = ledger(entry(0, workloadv1alpha1.RoleInstanceDraining, 0))
		again    = ledger(entry(0, workloadv1alpha1.RoleInstanceCreating, 1))
	)
	tests := []struct {
		name        string
		annotations map[string]string
		ledger      []workloadv1alpha1.RoleInstanceStatus
		// pods is the number of Pods of the group the controller sees.
		pods int
		// asAWhole: the spec the group is told is one of ServingGroupRollingUpdate.
		asAWhole bool
		// target is the target the group has, told the one it is told.
		target, told string
		want         map[string]string
	}{
		{name: "another target, the group is whole: it stops saying where it comes from, and keeps what else it carries",
			annotations: with(surge, "rev-b"), ledger: whole, pods: 4, target: "rev-b", told: "rev-c", want: surge},
		{name: "another target, nothing of the group was established: it is still being born, and says the new target",
			annotations: with(surge, "rev-b"), ledger: nothing, target: "rev-b", told: "rev-c", want: with(surge, "rev-c")},
		{name: "another target, an entry of the group never got further than Creating: it is still being born",
			annotations: with(surge, "rev-b"), ledger: begun, target: "rev-b", told: "rev-c", want: with(surge, "rev-c")},
		{name: "another target, one instance of the group is established and the other does not exist yet: it stops saying it",
			annotations: with(surge, "rev-b"), ledger: partly, target: "rev-b", told: "rev-c", want: surge},
		{name: "another target, the only instance in the ledger is being drained: it was established, the group stops saying it",
			annotations: with(surge, "rev-b"), ledger: draining, target: "rev-b", told: "rev-c", want: surge},
		{name: "another target, the only instance in the ledger is created again after a drain: the same",
			annotations: with(surge, "rev-b"), ledger: again, target: "rev-b", told: "rev-c", want: surge},
		{name: "another target, nothing of the group was established and it has a Pod: it stops saying it",
			annotations: with(surge, "rev-b"), ledger: nothing, pods: 1, target: "rev-b", told: "rev-c", want: surge},
		{name: "the same target with other settings: it still says so",
			annotations: with(surge, "rev-b"), ledger: whole, target: "rev-b", told: "rev-b", want: with(surge, "rev-b")},
		{name: "the same target, a group that is being born: it still says so",
			annotations: with(surge, "rev-b"), ledger: nothing, target: "rev-b", told: "rev-b", want: with(surge, "rev-b")},
		{name: "the target returns to the revision the group was created from: nothing is added",
			annotations: surge, ledger: whole, target: "rev-c", told: "rev-b", want: surge},
		{name: "a group that is being born and does not say where it comes from: nothing is added",
			annotations: surge, ledger: nothing, target: "rev-b", told: "rev-c", want: surge},
		{name: "a group without annotations gets none", ledger: nothing, target: "rev-a", told: "rev-b"},
		{name: "an annotation that names another revision than the target the group has goes with the next write",
			annotations: with(surge, "rev-a"), ledger: whole, target: "rev-b", told: "rev-b", want: surge},
		{name: "and on a group that is being born it names the target after that write",
			annotations: with(surge, "rev-a"), ledger: nothing, target: "rev-b", told: "rev-b", want: with(surge, "rev-b")},
		{name: "the group is told that it is replaced as a whole: it stops saying where it comes from, although the target is the same",
			annotations: with(surge, "rev-b"), ledger: whole, asAWhole: true, target: "rev-b", told: "rev-b", want: surge},
		{name: "and a group that is being born does not say the new target there",
			annotations: with(surge, "rev-b"), ledger: nothing, asAWhole: true, target: "rev-b", told: "rev-c", want: surge},
		{name: "a group that is replaced as a whole and does not say where it comes from: nothing is added",
			annotations: surge, ledger: nothing, asAWhole: true, target: "rev-b", told: "rev-c", want: surge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byRole := &workloadv1alpha1.ServingGroupRolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
			roles := []workloadv1alpha1.ServingGroupRole{{Name: "serve", Replicas: 2}}
			group := &workloadv1alpha1.ServingGroup{
				ObjectMeta: metav1.ObjectMeta{Name: "llama-0", Annotations: maps.Clone(tt.annotations)},
				Spec:       workloadv1alpha1.ServingGroupSpec{TargetRevision: tt.target, BaselineRevision: "rev-a", RolloutStrategy: byRole, Roles: roles},
				Status:     workloadv1alpha1.ServingGroupStatus{RoleInstances: tt.ledger},
			}
			spec := workloadv1alpha1.ServingGroupSpec{
				TargetRevision: tt.told, BaselineRevision: "rev-a", RolloutStrategy: byRole, Roles: roles,
				RecoveryPolicy: workloadv1alpha1.NoneRestartPolicy,
			}
			if tt.asAWhole {
				spec.Revision = "rev-a"
				spec.RolloutStrategy = &workloadv1alpha1.ServingGroupRolloutStrategy{Type: workloadv1alpha1.ServingGroupRollingUpdate}
			}

			written := told(group, spec, tt.pods)
			if !reflect.DeepEqual(written.Spec, spec) {
				t.Errorf("spec = %+v, want %+v", written.Spec, spec)
			}
			if !maps.Equal(written.Annotations, tt.want) {
				t.Errorf("annotations = %v, want %v", written.Annotations, tt.want)
			}
			if !maps.Equal(group.Annotations, tt.annotations) || group.Spec.TargetRevision != tt.target {
				t.Errorf("the group that was read was changed: %v, target %s", group.Annotations, group.Spec.TargetRevision)
			}
			// What the hand-down asks before it writes a group whose spec is
			// as it should be: after this write the answer is no.
			if outlived(written, &spec) {
				t.Errorf("after the write what the group says still does not go with its spec: %v", written.Annotations)
			}
		})
	}
}

// The target of a group, its strategy, and what the group says about where it
// comes from change in one write. Wherever a round is cut off, no group is
// left with the new target, or with the strategy of whole groups, beside the
// annotation: a group that found the two together would take itself for one
// that was born at the new target, now or on the day the strategy is changed
// back.
func TestTheTargetAndTheAnnotationOfAGroupChangeInOneWrite(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	asAWhole := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
			Type: workloadv1alpha1.ServingGroupRollingUpdate,
			RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: ptr.To(intstr.FromInt32(1)), Partition: ptr.To(intstr.FromInt32(3)),
			},
		}
	}
	// threeThatSay returns three groups that were created Role by Role and say so.
	threeThatSay := func(t *testing.T) *system {
		s := newSystem(t, modelServing(roleRolling(), threeGroups))
		s.settle()
		return s
	}
	// oneBeingBorn returns two groups that were told a target since they were
	// created, and a third that says it is new and has no member.
	oneBeingBorn := func(t *testing.T) *system {
		s, _ := aGroupBeingBorn(t)
		return s
	}
	target := func(s *system) func(*workloadv1alpha1.ServingGroup) bool {
		before := s.modelServing().Status.UpdateRevision
		return func(group *workloadv1alpha1.ServingGroup) bool { return group.Spec.TargetRevision != before }
	}
	strategy := func(*system) func(*workloadv1alpha1.ServingGroup) bool {
		return func(group *workloadv1alpha1.ServingGroup) bool {
			return group.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate
		}
	}
	tests := []struct {
		name  string
		start func(*testing.T) *system
		// change is the update of the ModelServing whose round is cut off.
		change func(*workloadv1alpha1.ModelServing)
		// written returns what tells that the round has written a group.
		written func(*system) func(*workloadv1alpha1.ServingGroup) bool
		// write is a write of the round that changes a group that says something.
		write string
	}{
		{name: "another target is handed down", start: threeThatSay, change: setImage("model:v2"), written: target,
			write: "update servinggroups default/llama-0"},
		{name: "the strategy of whole groups is handed down", start: threeThatSay, change: asAWhole, written: strategy,
			write: "update servinggroups default/llama-0"},
		// The write that gives a group without a member the revision of its
		// place is another one than the hand-down.
		{name: "a group without a member gets the revision of its place", start: oneBeingBorn, change: asAWhole, written: strategy,
			write: "update servinggroups default/llama-2"},
	}
	cut := errors.New("the round was cut off")
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for stopAt := 1; ; stopAt++ {
				s := tt.start(t)
				said := map[string]string{}
				for _, group := range s.groups() {
					if revision, says := group.Annotations[created]; says {
						said[group.Name] = revision
					}
				}
				written := tt.written(s)
				s.update(tt.change)
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

				done := 0
				for _, group := range s.groups() {
					revision, says := group.Annotations[created]
					switch {
					case written(group) && !says:
						done++
					case !written(group) && (!says || revision == said[group.Name]):
					default:
						t.Errorf("stop after write %d: group %s has the target %s and the strategy %s, and says that it was created from %q (%t); before the round it said %q",
							stopAt, group.Name, group.Spec.TargetRevision, group.Spec.RolloutStrategy.Type, revision, says, said[group.Name])
					}
				}
				if writes <= stopAt {
					// The round was not cut off: every write of it was made.
					var made []string
					for _, write := range s.cluster.Writes() {
						made = append(made, write.String())
					}
					if done != 3 || len(said) == 0 || !slices.Contains(made, tt.write) {
						t.Errorf("the whole round wrote %d of 3 groups, of which %d said something, with the writes %v; want all three, %q among the writes",
							done, len(said), made, tt.write)
					}
					return
				}

				// The rounds that follow repeat what was left.
				s.settle()
				for _, group := range s.groups() {
					if _, says := group.Annotations[created]; says || !written(group) {
						t.Errorf("stop after write %d: after the next rounds group %s has the target %s, the strategy %s and the annotations %v",
							stopAt, group.Name, group.Spec.TargetRevision, group.Spec.RolloutStrategy.Type, group.Annotations)
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
	// (Admission rejects this recovery policy under RoleRollingUpdate; see
	// TestANewGroupSaysWhichRevisionItWasCreatedFrom.)
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
	s.cluster.ResetWrites()
	s.settle()
	if got, want := s.created(names), "0=second 1=- 2=second"; got != want {
		t.Errorf("after an annotation was put on a group by hand: %s, want %s", got, want)
	}
	// It takes one write. After it nothing is left to bring in line.
	var written []string
	for _, write := range s.cluster.Writes() {
		if write.Resource == "servinggroups" && write.Subresource == "" {
			written = append(written, write.String())
		}
	}
	if !reflect.DeepEqual(written, []string{"update servinggroups default/" + foreign.Name}) {
		t.Errorf("writes of groups after the annotation was put there: %v, want one update of %s", written, foreign.Name)
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

// protected returns the UIDs of the Pods of the lowest instance of the Roles,
// the one a partition of 1 protects, in the first two groups.
func (s *system) protected(roles ...string) map[string]types.UID {
	s.t.Helper()
	uids := map[string]types.UID{}
	for _, group := range s.groups()[:2] {
		for name, uid := range s.podsOf(group.Name) {
			for _, role := range roles {
				if strings.HasPrefix(name, group.Name+"-"+role+"-0-") {
					uids[name] = uid
				}
			}
		}
	}
	return uids
}

// The partition of a Role keeps the lowest instances of a group that runs the
// baseline at the baseline. A group that is created while the rollout stands
// at the partition never ran the baseline: every instance of it, the one
// below the partition included, is created from the template of now. The
// groups that were there keep their protected instance, Pod for Pod.
func TestANewGroupIsCreatedFromTheTargetBelowARolePartition(t *testing.T) {
	const (
		created = "0[route-0=route:v1 0=model:v1 1=model:v1 2=model:v1] 1[route-0=route:v1 0=model:v1 1=model:v1 2=model:v1]"
		held    = "0[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2] 1[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2]"
		grown   = held + " 2[route-0=route:v1 0=model:v2 1=model:v2 2=model:v2]"
	)
	tests := []struct {
		name string
		// steps are the updates of the ModelServing, each settled before the next.
		steps []func(*workloadv1alpha1.ModelServing)
	}{
		{name: "the count is raised after the rollout stopped at the partition",
			steps: []func(*workloadv1alpha1.ModelServing){setImage("model:v2"), threeGroups}},
		{name: "the count is raised with the template",
			steps: []func(*workloadv1alpha1.ModelServing){func(ms *workloadv1alpha1.ModelServing) {
				setImage("model:v2")(ms)
				threeGroups(ms)
			}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(heldByARolePartition))
			s.settle()
			if got := s.instances(); got != created {
				t.Fatalf("created: %s", got)
			}
			baseline := s.modelServing().Status.CurrentRevision
			kept := s.protected("serve")
			if len(kept) != 4 {
				t.Fatalf("the protected instances have the Pods %v, want an entry and a worker in each of two groups", kept)
			}

			for i, step := range tt.steps {
				s.update(step)
				s.settle()
				if i+1 < len(tt.steps) {
					if got := s.instances(); got != held {
						t.Fatalf("at the partition: %s, want %s", got, held)
					}
				}
			}

			if got := s.instances(); got != grown {
				t.Errorf("after the count was raised:\n got %s\nwant %s", got, grown)
			}
			if after := s.protected("serve"); !reflect.DeepEqual(after, kept) {
				t.Errorf("Pods of the protected instances were replaced: %v, before %v", after, kept)
			}
			// The new group runs the target and counts as updated. The others
			// stay where the partition holds them, and so does the baseline.
			if got, want := s.counts()+" "+s.condition(), "replicas=3 updated=1 current=0 available=3 UpdateInProgress"; got != want {
				t.Errorf("status = %s, want %s", got, want)
			}
			status := s.modelServing().Status
			if status.CurrentRevision != baseline || status.UpdateRevision == baseline {
				t.Errorf("revisions: current %s, update %s, before %s; want the baseline kept", status.CurrentRevision, status.UpdateRevision, baseline)
			}
			for _, group := range s.groups() {
				if !groupReady(group) {
					t.Errorf("group %s is not ready", group.Name)
				}
			}
		})
	}
}

// A group that is deleted comes back as a new object, and that is a new group
// like one that is added: whole from the target. The other group keeps its
// Pods.
func TestAGroupThatIsDeletedUnderARolePartitionComesBackFromTheTarget(t *testing.T) {
	s := newSystem(t, modelServing(heldByARolePartition))
	s.settle()
	s.update(setImage("model:v2"))
	s.settle()
	deleted, kept := s.groups()[1], s.podsOf("llama-0")

	err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(context.Background(), deleted.Name,
		metav1.DeleteOptions{PropagationPolicy: ptr.To(metav1.DeletePropagationForeground)})
	if err != nil {
		t.Fatal(err)
	}
	s.settle()
	if again := s.groups()[1]; again.Name != deleted.Name || again.UID == deleted.UID {
		t.Fatalf("group %s has UID %s, before %s: want the same name and a new object", again.Name, again.UID, deleted.UID)
	}
	const want = "0[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2] 1[route-0=route:v1 0=model:v2 1=model:v2 2=model:v2]"
	if got := s.instances(); got != want {
		t.Errorf("after the group came back:\n got %s\nwant %s", got, want)
	}
	if after := s.podsOf("llama-0"); !reflect.DeepEqual(after, kept) {
		t.Errorf("Pods of the other group were replaced: %v, before %v", after, kept)
	}
}

// The same when the Roles of a group are rolled together. The new group is
// created whole from the target, in the order of the dependencies: the Role
// that depends on another waits for it with every instance, the one below
// its partition like the others.
func TestANewGroupOfCoordinatedRolesIsCreatedFromTheTarget(t *testing.T) {
	const (
		held  = "0[decode-0=model:v1 decode-1=model:v2 prefill-0=model:v1 prefill-1=model:v2] 1[decode-0=model:v1 decode-1=model:v2 prefill-0=model:v1 prefill-1=model:v2]"
		grown = held + " 2[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v2 prefill-1=model:v2]"
	)
	s := newSystem(t, modelServing(coordinated, func(ms *workloadv1alpha1.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromInt32(1))
		}
	}))
	s.settle()
	s.update(setImages("model:v2"))
	s.settle()
	if got := s.instances(); got != held {
		t.Fatalf("at the partitions: %s, want %s", got, held)
	}
	kept := s.protected("prefill", "decode")
	if len(kept) != 8 {
		t.Fatalf("the protected instances have the Pods %v, want an entry and a worker of each Role in each of two groups", kept)
	}

	// prefill depends on decode: no Pod of prefill is created in the new
	// group before a Pod of decode is ready there.
	s.invariant = func() {
		prefill, ready := 0, 0
		for _, pod := range s.pods() {
			if pod.Labels[workloadv1alpha1.GroupNameLabelKey] != "llama-2" {
				continue
			}
			switch pod.Labels[workloadv1alpha1.RoleLabelKey] {
			case "prefill":
				prefill++
			case "decode":
				if utils.IsPodRunningAndReady(pod) {
					ready++
				}
			}
		}
		if prefill > 0 && ready == 0 {
			t.Fatalf("the new group has %d Pods of prefill and no ready Pod of decode: %s", prefill, s.instances())
		}
	}
	s.update(threeGroups)
	s.settle()
	s.invariant = nil

	if got := s.instances(); got != grown {
		t.Errorf("after the count was raised:\n got %s\nwant %s", got, grown)
	}
	if after := s.protected("prefill", "decode"); !reflect.DeepEqual(after, kept) {
		t.Errorf("Pods of the protected instances were replaced: %v, before %v", after, kept)
	}
	if got, want := s.counts()+" "+s.coordination(), "replicas=3 updated=1 current=0 available=3 False/RolloutComplete"; got != want {
		t.Errorf("status = %s, want %s", got, want)
	}
}

// A group that ran the baseline creates the instance its Role partition
// protects from the baseline when that instance is lost. That the target is,
// once more, the revision the group was created from changes nothing: the
// group was told another target in between, and is no new group since.
func TestAGroupThatRanTheBaselineCreatesItsProtectedInstanceFromTheBaseline(t *testing.T) {
	one := ptr.To(intstr.FromInt32(1))
	// changed returns an update of the image and the partition of serve and,
	// when it is not 0, of the number of groups.
	changed := func(image string, partition *intstr.IntOrString, groups int32) func(*workloadv1alpha1.ModelServing) {
		return func(ms *workloadv1alpha1.ModelServing) {
			setImage(image)(ms)
			ms.Spec.Template.Roles[0].Partition = partition
			if groups != 0 {
				ms.Spec.Replicas = ptr.To(groups)
			}
		}
	}
	// lose deletes the Pods of the protected instance of a group.
	lose := func(t *testing.T, s *system, group string) {
		t.Helper()
		for _, name := range []string{group + "-serve-0-0", group + "-serve-0-1"} {
			if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		s.settle()
	}
	// stands compares what the groups run after a step.
	stands := func(t *testing.T, s *system, step, want string) {
		t.Helper()
		if got := s.instances(); got != want {
			t.Fatalf("%s:\n got %s\nwant %s", step, got, want)
		}
	}

	// Every group was created from v1. The rollout to v2 is complete, and is
	// then taken back with a partition: v2 is the baseline now.
	t.Run("a rollout that is complete is taken back with a Role partition", func(t *testing.T) {
		const held = "0[0=model:v2 1=model:v1 2=model:v1] 1[0=model:v2 1=model:v1 2=model:v1]"
		s := newSystem(t, modelServing(roleRolling()))
		s.settle()
		s.update(changed("model:v2", nil, 0))
		s.settle()
		s.update(changed("model:v1", one, 0))
		s.settle()
		stands(t, s, "at the partition", held)
		lose(t, s, "llama-0")
		stands(t, s, "after the protected instance of group 0 was lost", held)
	})

	// The third group is born at v2 during a canary. The canary is given up,
	// and every group runs v1; then it is tried again.
	t.Run("a canary is given up and tried again", func(t *testing.T) {
		const held = "0[0=model:v1 1=model:v2 2=model:v2] 1[0=model:v1 1=model:v2 2=model:v2] 2[0=model:v1 1=model:v2 2=model:v2]"
		s := newSystem(t, modelServing(roleRolling(), changed("model:v1", one, 0)))
		s.settle()
		s.update(changed("model:v2", one, 3))
		s.settle()
		stands(t, s, "the canary, with a group added",
			"0[0=model:v1 1=model:v2 2=model:v2] 1[0=model:v1 1=model:v2 2=model:v2] 2[0=model:v2 1=model:v2 2=model:v2]")
		s.update(changed("model:v1", nil, 0))
		s.settle()
		s.update(changed("model:v2", one, 0))
		s.settle()
		stands(t, s, "the canary again", held)
		lose(t, s, "llama-2")
		stands(t, s, "after the protected instance of group 2 was lost", held)
	})

	// The third group is born at v2 during a canary. The canary moves on to
	// v3: the group is one of an earlier revision like the others, and stays
	// one when the canary returns to v2.
	t.Run("the target moves on and returns", func(t *testing.T) {
		s := newSystem(t, modelServing(roleRolling(), changed("model:v1", one, 0)))
		s.settle()
		s.update(changed("model:v2", one, 3))
		s.settle()
		s.update(changed("model:v3", one, 0))
		s.settle()
		stands(t, s, "the target moved on",
			"0[0=model:v1 1=model:v3 2=model:v3] 1[0=model:v1 1=model:v3 2=model:v3] 2[0=model:v2 1=model:v3 2=model:v3]")
		lose(t, s, "llama-2")
		stands(t, s, "after the protected instance of group 2 was lost",
			"0[0=model:v1 1=model:v3 2=model:v3] 1[0=model:v1 1=model:v3 2=model:v3] 2[0=model:v1 1=model:v3 2=model:v3]")

		const held = "0[0=model:v1 1=model:v2 2=model:v2] 1[0=model:v1 1=model:v2 2=model:v2] 2[0=model:v1 1=model:v2 2=model:v2]"
		s.update(changed("model:v2", one, 0))
		s.settle()
		stands(t, s, "the target returned", held)
		lose(t, s, "llama-2")
		stands(t, s, "after the protected instance of group 2 was lost again", held)
	})
}

// lose deletes the Pods whose name starts with prefix: the Pods of a Role
// instance, as "llama-2-serve-0-".
func (s *system) lose(prefix string) {
	s.t.Helper()
	lost := 0
	for _, pod := range s.pods() {
		if !strings.HasPrefix(pod.Name, prefix) {
			continue
		}
		if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}); err != nil {
			s.t.Fatal(err)
		}
		lost++
	}
	if lost == 0 {
		s.t.Fatalf("no Pod is named %s*", prefix)
	}
}

// refuse makes the cluster refuse every Pod whose name starts with one of the
// prefixes, as a quota does, until accept. A round of a ServingGroup may end
// with that.
func (s *system) refuse(prefixes ...string) {
	s.refused = errors.New("exceeded quota")
	s.cluster.BeforeWrite = func(write apitestWrite) error {
		if write.Verb != "create" || write.Resource != "pods" {
			return nil
		}
		for _, prefix := range prefixes {
			if strings.HasPrefix(write.Name, prefix) {
				return s.refused
			}
		}
		return nil
	}
}

func (s *system) accept() { s.refused, s.cluster.BeforeWrite = nil, nil }

// ledgerOf renders the ledger of a group: "route-0=Established/0 ...".
func (s *system) ledgerOf(group string) string {
	s.t.Helper()
	var entries []string
	for _, g := range s.groups() {
		if g.Name != group {
			continue
		}
		for _, entry := range g.Status.RoleInstances {
			entries = append(entries, entry.Role+"-"+strconv.Itoa(int(entry.Index))+"="+string(entry.Phase)+"/"+strconv.FormatInt(entry.Generation, 10))
		}
	}
	slices.Sort(entries)
	return strings.Join(entries, " ")
}

// aGroupBeingBorn returns a system whose ModelServing stands at the partition
// of serve between v1 and v2, with a third group whose object was just
// created: the group has had no round, and no Pod. It also returns the
// revision of v2, which the new group says it was created from.
func aGroupBeingBorn(t *testing.T) (*system, string) {
	t.Helper()
	s := newSystem(t, modelServing(heldByARolePartition))
	s.settle()
	s.update(setImage("model:v2"))
	s.settle()
	s.update(threeGroups)
	if _, err := s.roundOfTheModelServing(); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatal(err)
	}
	second := s.modelServing().Status.UpdateRevision
	born := s.groups()[2]
	if got := born.Annotations[workloadv1alpha1.CreatedRevisionAnnotationKey]; got != second || born.Status.Pods != 0 || len(s.podsOf(born.Name)) != 0 {
		t.Fatalf("the new group says %q, has committed to %d Pods and has the Pods %v; want %q and no Pod", got, born.Status.Pods, s.podsOf(born.Name), second)
	}
	return s, second
}

// A group without a member is still being born. When the template changes
// before the group has committed to its first Pod, the group is told the new
// target and goes on saying that it is new, now of that target: it is created
// whole from the template of now. The groups that were there keep their
// protected instance.
func TestAGroupWithoutAMemberIsCreatedFromTheTargetItIsTold(t *testing.T) {
	s, second := aGroupBeingBorn(t)
	born, kept := s.groups()[2], s.protected("serve")

	s.update(setImage("model:v3"))
	s.settle()
	third := s.modelServing().Status.UpdateRevision
	const want = "0[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] 1[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] " +
		"2[route-0=route:v1 0=model:v3 1=model:v3 2=model:v3]"
	if got := s.instances(); got != want {
		t.Errorf("after the template changed:\n got %s\nwant %s", got, want)
	}
	if got, want := s.created(map[string]string{second: "second", third: "third"}), "0=- 1=- 2=third"; got != want || third == second {
		t.Errorf("the groups say %s, want %s", got, want)
	}
	if now := s.groups()[2]; now.UID != born.UID {
		t.Errorf("the new group was replaced: UID %s, before %s", now.UID, born.UID)
	}
	if after := s.protected("serve"); !reflect.DeepEqual(after, kept) {
		t.Errorf("Pods of the protected instances were replaced: %v, before %v", after, kept)
	}
}

// The write that tells a group another target, and a status write of the
// group, are each made on what its controller read. Both carry the
// resourceVersion of that read, so they exclude each other: the one that
// comes second meets a conflict and is decided again on what the first one
// wrote.
//
// What is decided again is whether the group has built anything. A group
// that has Pods has: it ran the target of before, and stops saying that it
// is new. A group that has committed to its Pods and has none has not: it is
// told the new target and says that it was created from it. A group of which
// an instance became established in between meets the conflict.
//
// The Pods themselves are not part of the exclusion: creating one is not a
// write of the group. The last case is a Pod that the ModelServing controller
// does not see yet.
func TestAGroupIsToldAnotherTargetOrCommitsToItsFirstPodNotBoth(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	const others = "0[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] 1[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] "
	// read is what a controller holds of the cluster at one moment.
	type read struct {
		caches   objcache.Caches
		snapshot *apitest.Snapshot
	}
	now := func(s *system) read {
		caches, snapshot := s.caches()
		return read{caches, snapshot}
	}
	roundOfTheModelServing := func(s *system, on read) error {
		s.ms.SetCaches(on.caches, on.snapshot.Listers().Services)
		_, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
		return err
	}
	roundOfTheGroup := func(s *system, on read) error {
		s.sg.SetCaches(on.caches, on.snapshot.Listers().Services)
		_, err := s.sg.Reconcile(context.Background(), testNamespace, "llama-2")
		return err
	}

	// commitsAndDies runs a round of the group that dies right after its
	// first write: the group has committed to its Pods and has none.
	commitsAndDies := func(t *testing.T, s *system) {
		t.Helper()
		writes := 0
		s.cluster.BeforeWrite = func(apitestWrite) error {
			if writes++; writes > 1 {
				return errDied
			}
			return nil
		}
		if err := roundOfTheGroup(s, now(s)); !errors.Is(err, errDied) {
			t.Fatalf("round of the group: %v, want it to die after its first write", err)
		}
		s.cluster.BeforeWrite = nil
		if group := s.groups()[2]; group.Status.Pods == 0 || len(s.podsOf(group.Name)) != 0 {
			t.Fatalf("the group has committed to %d Pods and has the Pods %v; want a commitment and no Pod", group.Status.Pods, s.podsOf(group.Name))
		}
	}

	t.Run("the group commits first", func(t *testing.T) {
		s, second := aGroupBeingBorn(t)
		s.update(setImage("model:v3"))
		before := now(s)

		if err := roundOfTheGroup(s, now(s)); err != nil {
			t.Fatalf("round of the group: %v", err)
		}
		if pods := s.groups()[2].Status.Pods; pods == 0 {
			t.Fatal("the group has not committed to its Pods")
		}
		// The ModelServing controller still holds the group as it was: no
		// member, nothing committed. Its write is rejected.
		if err := roundOfTheModelServing(s, before); !apierrors.IsConflict(err) {
			t.Fatalf("round of the ModelServing on what it read before: %v, want a conflict", err)
		}
		if group := s.groups()[2]; group.Spec.TargetRevision != second || group.Annotations[created] != second {
			t.Fatalf("after the conflict the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], second)
		}

		// Decided again, the group has Pods of v2: it ran v2, and stops
		// saying that it is new. Its protected instance stays at v2.
		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v2 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("at the end:\n got %s\nwant %s", got, want)
		}
		if revision, says := s.groups()[2].Annotations[created]; says {
			t.Errorf("at the end the group says that it was created from %q", revision)
		}
	})

	// The commitment alone excludes the other write, whether a Pod followed
	// it or not: here the round of the group dies right after it. Decided
	// again, the group has no Pod and nothing established. It is still being
	// born, is told the new target, and is created whole from it.
	t.Run("the group commits first and dies before its first Pod", func(t *testing.T) {
		s, second := aGroupBeingBorn(t)
		s.update(setImage("model:v3"))
		before := now(s)

		commitsAndDies(t, s)
		if err := roundOfTheModelServing(s, before); !apierrors.IsConflict(err) {
			t.Fatalf("round of the ModelServing on what it read before: %v, want a conflict", err)
		}
		if group := s.groups()[2]; group.Spec.TargetRevision != second || group.Annotations[created] != second {
			t.Fatalf("after the conflict the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], second)
		}

		if err := roundOfTheModelServing(s, now(s)); err != nil && !errors.Is(err, ErrNotSettled) {
			t.Fatalf("round of the ModelServing: %v", err)
		}
		third := s.modelServing().Status.UpdateRevision
		if group := s.groups()[2]; third == second || group.Spec.TargetRevision != third || group.Annotations[created] != third {
			t.Errorf("decided again, the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], third)
		}
		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v3 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("at the end:\n got %s\nwant %s", got, want)
		}
	})

	// The ModelServing controller read a group that has committed and has
	// built nothing, and would make it say the new target. The status write
	// that says an instance is established comes first.
	t.Run("an instance of the group becomes established first", func(t *testing.T) {
		s, second := aGroupBeingBorn(t)
		commitsAndDies(t, s)
		s.update(setImage("model:v3"))
		before := now(s)

		// The Pods are created, and in the round after that the instances
		// are established.
		for range 2 {
			if err := roundOfTheGroup(s, now(s)); err != nil {
				t.Fatalf("round of the group: %v", err)
			}
		}
		if got := s.ledgerOf("llama-2"); !strings.Contains(got, "Established") {
			t.Fatalf("the ledger of the group is %q, want instances that are established", got)
		}
		if err := roundOfTheModelServing(s, before); !apierrors.IsConflict(err) {
			t.Fatalf("round of the ModelServing on what it read before: %v, want a conflict", err)
		}
		if group := s.groups()[2]; group.Spec.TargetRevision != second || group.Annotations[created] != second {
			t.Fatalf("after the conflict the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], second)
		}

		// Decided again, the group ran v2: it stops saying that it is new,
		// and its protected instance comes back from the baseline.
		s.settle()
		if revision, says := s.groups()[2].Annotations[created]; says {
			t.Errorf("at the end the group says that it was created from %q", revision)
		}
		s.lose("llama-2-serve-0-")
		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("after the protected instance was lost:\n got %s\nwant %s", got, want)
		}
	})

	// Nothing excludes this one. The group has committed, and its controller
	// creates the Pods without another write of the group. The ModelServing
	// controller looked before they were there: it sees a group that has
	// built nothing, and its write is accepted. The group now says that it
	// was created from v3 and has Pods of v2. Below the partition they stay,
	// like any Pods there; and one that is lost comes back from v3.
	t.Run("the Pods of the group are created and the ModelServing controller does not see them yet", func(t *testing.T) {
		s, second := aGroupBeingBorn(t)
		commitsAndDies(t, s)
		s.update(setImage("model:v3"))
		before := now(s)

		if err := roundOfTheGroup(s, now(s)); err != nil {
			t.Fatalf("round of the group: %v", err)
		}
		if pods, ledger := s.podsOf("llama-2"), s.ledgerOf("llama-2"); len(pods) == 0 || ledger != "" {
			t.Fatalf("the group has the Pods %v and the ledger %q; want Pods and nothing established", pods, ledger)
		}
		if err := roundOfTheModelServing(s, before); err != nil && !errors.Is(err, ErrNotSettled) {
			t.Fatalf("round of the ModelServing on what it read before: %v", err)
		}
		third := s.modelServing().Status.UpdateRevision
		if group := s.groups()[2]; third == second || group.Spec.TargetRevision != third || group.Annotations[created] != third {
			t.Fatalf("the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], third)
		}

		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v2 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("at the end:\n got %s\nwant %s", got, want)
		}
		s.lose("llama-2-serve-0-")
		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v3 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("after the protected instance was lost:\n got %s\nwant %s", got, want)
		}
		if got := s.groups()[2].Annotations[created]; got != third {
			t.Errorf("at the end the group says %q, want %s", got, third)
		}
	})

	t.Run("the group is told first", func(t *testing.T) {
		s, second := aGroupBeingBorn(t)
		s.update(setImage("model:v3"))
		before := now(s)

		if err := roundOfTheModelServing(s, now(s)); err != nil && !errors.Is(err, ErrNotSettled) {
			t.Fatalf("round of the ModelServing: %v", err)
		}
		third := s.modelServing().Status.UpdateRevision
		if group := s.groups()[2]; third == second || group.Spec.TargetRevision != third || group.Annotations[created] != third {
			t.Fatalf("the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], third)
		}
		// The ServingGroup controller still holds the group with the target
		// of before. Its commitment is rejected, and no Pod is created.
		if err := roundOfTheGroup(s, before); !apierrors.IsConflict(err) {
			t.Fatalf("round of the group on what it read before: %v, want a conflict", err)
		}
		if group := s.groups()[2]; group.Status.Pods != 0 || len(s.podsOf(group.Name)) != 0 {
			t.Fatalf("after the conflict the group has committed to %d Pods and has the Pods %v", group.Status.Pods, s.podsOf(group.Name))
		}

		// Decided again, the group is born at v3.
		s.settle()
		if got, want := s.instances(), others+"2[route-0=route:v1 0=model:v3 1=model:v3 2=model:v3]"; got != want {
			t.Errorf("at the end:\n got %s\nwant %s", got, want)
		}
		if got := s.groups()[2].Annotations[created]; got != third {
			t.Errorf("at the end the group says %q, want %s", got, third)
		}
	})
}

// What a group says about where it comes from is for groups that are rolled
// Role by Role. A group that is replaced as a whole runs the revision its
// spec names, which below the partition of the groups is the baseline; an
// annotation that named the target there would be believed when the strategy
// is changed back. So the annotation goes when the strategy is changed to
// whole groups, also from a group that has no member, and it is not put back:
// after the change back every group is one that was there before, and creates
// a lost instance below a Role partition from the baseline.
func TestTheStrategyIsChangedWithAGroupThatSaysWhereItComesFrom(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	asAWhole := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
			Type: workloadv1alpha1.ServingGroupRollingUpdate,
			RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: ptr.To(intstr.FromInt32(1)), Partition: ptr.To(intstr.FromInt32(3)),
			},
		}
	}
	byRole := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
	}
	// whole returns two groups that stand at the partition between v1 and
	// v2, and a third that was added there and is whole at v2.
	whole := func(t *testing.T) *system {
		s := newSystem(t, modelServing(heldByARolePartition))
		s.settle()
		s.update(setImage("model:v2"))
		s.settle()
		s.update(threeGroups)
		s.settle()
		return s
	}
	beingBorn := func(t *testing.T) *system {
		s, _ := aGroupBeingBorn(t)
		return s
	}
	const old = "0[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2] 1[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2] "
	tests := []struct {
		name  string
		start func(*testing.T) *system
		// change is made together with the change of the strategy.
		change func(*workloadv1alpha1.ModelServing)
		// asGroups is what the groups run under the strategy of whole groups,
		// byRoles after the change back and the loss of the protected
		// instance of the second and the third group.
		asGroups, byRoles string
	}{
		{name: "a group that is whole", start: whole,
			asGroups: old + "2[route-0=route:v1 0=model:v2 1=model:v2 2=model:v2]",
			byRoles:  old + "2[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2]"},
		// Below the partition of the groups a group without a member is
		// created from the baseline.
		{name: "a group without a member", start: beingBorn,
			asGroups: old + "2[route-0=route:v1 0=model:v1 1=model:v1 2=model:v1]",
			byRoles:  old + "2[route-0=route:v1 0=model:v1 1=model:v2 2=model:v2]"},
		{name: "a group without a member, and another target with the strategy", start: beingBorn, change: setImage("model:v3"),
			asGroups: old + "2[route-0=route:v1 0=model:v1 1=model:v1 2=model:v1]",
			byRoles: "0[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] 1[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] " +
				"2[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.start(t)
			if _, says := s.groups()[2].Annotations[created]; !says {
				t.Fatal("the third group does not say where it comes from")
			}

			s.update(func(ms *workloadv1alpha1.ModelServing) {
				asAWhole(ms)
				if tt.change != nil {
					tt.change(ms)
				}
			})
			s.settle()
			if got := s.instances(); got != tt.asGroups {
				t.Fatalf("replaced as a whole:\n got %s\nwant %s", got, tt.asGroups)
			}
			if got, want := s.created(nil), "0=- 1=- 2=-"; got != want {
				t.Fatalf("replaced as a whole, the groups say %s, want %s", got, want)
			}

			// An annotation that somebody puts there is taken away, although
			// it names the target.
			foreign := s.groups()[2]
			foreign.Annotations = map[string]string{created: foreign.Spec.TargetRevision}
			if _, err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(context.Background(), foreign, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
			s.settle()
			if got, want := s.created(nil), "0=- 1=- 2=-"; got != want {
				t.Fatalf("after an annotation was put on a group by hand, the groups say %s, want %s", got, want)
			}

			s.update(byRole)
			s.settle()
			if got, want := s.created(nil), "0=- 1=- 2=-"; got != want {
				t.Errorf("rolled Role by Role again, the groups say %s, want %s", got, want)
			}
			for _, name := range []string{"llama-1-serve-0-0", "llama-1-serve-0-1", "llama-2-serve-0-0", "llama-2-serve-0-1"} {
				if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s.settle()
			if got := s.instances(); got != tt.byRoles {
				t.Errorf("after the protected instance of two groups was lost:\n got %s\nwant %s", got, tt.byRoles)
			}
		})
	}
}

// A group is added while the rollout stands at the partition of a Role, and
// the template changes again while the group is being built: some of its
// Pods cannot be created for a while, as under a quota.
//
// A group that has built nothing is still being born. It is told the new
// target, goes on saying that it is new, and is created whole from the
// template of now, as upstream does.
//
// A group that has built something ran the target it was created from, and
// stops saying that it is new. What it lacks is then a place of an ordinary
// group: below the partition it is created from the baseline, a revision this
// group never ran. That is the defect this annotation is for, left in this
// one window; upstream has such an instance at the revision the group was
// created from. An instance that was begun is completed at its own revision.
func TestANewGroupIsToldAnotherTargetWhileItIsBuilt(t *testing.T) {
	const others = "0[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] 1[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3] "
	tests := []struct {
		name string
		// refused are the Pods that cannot be created, by the start of their name.
		refused []string
		// built is what the group has when the template changes.
		built string
		// says is what the group says about where it comes from afterwards.
		says string
		// runs is what the group runs once every Pod can be created.
		runs string
	}{
		{name: "no Pod can be created: the group is created whole from the new target",
			refused: []string{"llama-2-"}, built: "2[]",
			says: "third", runs: "2[route-0=route:v1 0=model:v3 1=model:v3 2=model:v3]"},
		{name: "the instance below the partition cannot be created, another instance is established: it comes from the baseline",
			refused: []string{"llama-2-serve-0-"}, built: "2[route-0=route:v1]",
			says: "-", runs: "2[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3]"},
		{name: "one Pod of the instance below the partition cannot be created: the instance is completed as it was begun",
			refused: []string{"llama-2-serve-0-1"}, built: "2[route-0=route:v1 0=model:v2]",
			says: "-", runs: "2[route-0=route:v1 0=model:v2 1=model:v3 2=model:v3]"},
		{name: "the last instance cannot be created: the instances that are established are those of any group",
			refused: []string{"llama-2-serve-2-"}, built: "2[route-0=route:v1 0=model:v2 1=model:v2]",
			says: "-", runs: "2[route-0=route:v1 0=model:v2 1=model:v3 2=model:v3]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(heldByARolePartition))
			s.settle()
			s.update(setImage("model:v2"))
			s.settle()
			names := map[string]string{s.modelServing().Status.UpdateRevision: "second"}
			kept := s.protected("serve")
			// stands compares what the new group says and runs.
			stands := func(step, says, runs string) {
				t.Helper()
				if got, want := s.created(names), "0=- 1=- 2="+says; got != want {
					t.Fatalf("%s: the groups say %s, want %s", step, got, want)
				}
				got := s.instances()
				if got = got[strings.Index(got, "2["):]; got != runs {
					t.Fatalf("%s: the new group runs\n got %s\nwant %s", step, got, runs)
				}
			}

			s.refuse(tt.refused...)
			s.update(threeGroups)
			s.settle()
			stands("the group is added", "second", tt.built)
			if pods := s.groups()[2].Status.Pods; pods == 0 {
				t.Fatal("the group has not committed to its Pods")
			}

			s.update(setImage("model:v3"))
			s.settle()
			names[s.modelServing().Status.UpdateRevision] = "third"
			stands("the template changed", tt.says, tt.built)

			s.accept()
			s.settle()
			stands("every Pod can be created", tt.says, tt.runs)
			if got := s.instances(); !strings.HasPrefix(got, others) {
				t.Errorf("the other groups run %s, want %s", got, others)
			}
			if after := s.protected("serve"); !reflect.DeepEqual(after, kept) {
				t.Errorf("Pods of the protected instances were replaced: %v, before %v", after, kept)
			}
		})
	}
}

// A group that was whole and has lost every Pod has none, like a group that
// is being born. Its ledger tells the two apart: its instances were
// established. Told another target while its Pods cannot be created again,
// it stops saying that it is new, and the instance below the partition comes
// back from the baseline.
func TestAGroupThatWasWholeAndLostEveryPodIsNotBeingBorn(t *testing.T) {
	s := newSystem(t, modelServing(heldByARolePartition))
	s.settle()
	s.update(setImage("model:v2"))
	s.settle()
	names := map[string]string{s.modelServing().Status.UpdateRevision: "second"}
	s.update(threeGroups)
	s.settle()
	runs := func(step, want string) {
		t.Helper()
		got := s.instances()
		if got = got[strings.Index(got, "2["):]; got != want {
			t.Fatalf("%s: the new group runs\n got %s\nwant %s", step, got, want)
		}
	}
	runs("the group is whole", "2[route-0=route:v1 0=model:v2 1=model:v2 2=model:v2]")

	s.refuse("llama-2-")
	s.lose("llama-2-")
	s.settle()
	runs("every Pod was lost", "2[]")
	if got, want := s.ledgerOf("llama-2"), "route-0=Creating/1 serve-0=Creating/1 serve-1=Creating/1 serve-2=Creating/1"; got != want {
		t.Fatalf("the ledger of the group is %s, want %s", got, want)
	}
	if got, want := s.created(names), "0=- 1=- 2=second"; got != want {
		t.Fatalf("the groups say %s, want %s", got, want)
	}

	s.update(setImage("model:v3"))
	s.settle()
	if got, want := s.created(names), "0=- 1=- 2=-"; got != want {
		t.Fatalf("after the template changed the groups say %s, want %s", got, want)
	}
	s.accept()
	s.settle()
	runs("the Pods can be created again", "2[route-0=route:v1 0=model:v1 1=model:v3 2=model:v3]")
}

// The same for Roles that are rolled together. prefill depends on decode, so
// a new group has decode first and prefill only when decode is ready, which
// for a model server is minutes: a group of coordinated Roles is built in
// two steps even when nothing fails. When the template changes in between,
// the group has built decode: it stops saying that it is new, and the
// instances of prefill are places of an ordinary group. Below the partition
// they are created from the baseline, and at once, since an instance of the
// baseline waits for nothing; above it they are instances of the new target
// and wait for decode. The group is complete in every case, also when every
// instance is below the partition, which is how a staged rollout begins.
//
// Upstream creates the whole group at once, so prefill exists at the revision
// the group was created from before the template changes.
func TestANewGroupOfCoordinatedRolesIsToldAnotherTargetWhileItIsBuilt(t *testing.T) {
	decodeOfTheNewGroup := func(pod *corev1.Pod) bool {
		return pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-2" && pod.Labels[workloadv1alpha1.RoleLabelKey] == "decode"
	}
	tests := []struct {
		name      string
		replicas  int32
		partition string
		// before runs before the group is added and after runs once it has
		// come to rest; built is what the group has when the template changes.
		before, after func(*system)
		built         string
		// changed is what the group runs after the template changed, while
		// decode is not ready, and runs what it runs at the end.
		changed, runs string
	}{
		{name: "decode is not ready, an instance is above the partition",
			replicas: 2, partition: "50%", before: func(s *system) { s.stuck = decodeOfTheNewGroup },
			built:   "2[decode-0=model:v2 decode-1=model:v2]",
			changed: "2[decode-0=model:v2 decode-1=model:v3 prefill-0=model:v1]",
			runs:    "2[decode-0=model:v2 decode-1=model:v3 prefill-0=model:v1 prefill-1=model:v3]"},
		{name: "decode is not ready, every instance is below the partition",
			replicas: 2, partition: "100%", before: func(s *system) { s.stuck = decodeOfTheNewGroup },
			built:   "2[decode-0=model:v2 decode-1=model:v2]",
			changed: "2[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v1 prefill-1=model:v1]",
			runs:    "2[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v1 prefill-1=model:v1]"},
		// The group has one Pod and no instance that is established.
		{name: "a Pod of decode cannot be created, every instance is below the partition",
			replicas: 1, partition: "100%", before: func(s *system) { s.refuse("llama-2-decode-0-1") },
			built:   "2[decode-0=model:v2]",
			changed: "2[decode-0=model:v2]",
			runs:    "2[decode-0=model:v2 prefill-0=model:v1]"},
		// The group was whole. It loses every Pod while decode cannot get
		// ready: decode comes back, prefill waits for it, and the entry of
		// its instance is gone from the ledger. The entry of decode is there.
		{name: "a group that was whole lost its Pods and waits to start prefill again",
			replicas: 1, partition: "100%", after: func(s *system) {
				s.stuck = decodeOfTheNewGroup
				s.lose("llama-2-decode-")
				s.lose("llama-2-prefill-")
				s.settle()
				if got, want := s.ledgerOf("llama-2"), "decode-0=Established/1"; got != want {
					s.t.Fatalf("the ledger of the group is %s, want %s", got, want)
				}
			},
			built:   "2[decode-0=model:v2]",
			changed: "2[decode-0=model:v2 prefill-0=model:v1]",
			runs:    "2[decode-0=model:v2 prefill-0=model:v1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(coordinated, func(ms *workloadv1alpha1.ModelServing) {
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].Replicas = ptr.To(tt.replicas)
					ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromString(tt.partition))
				}
			}))
			s.settle()
			s.update(setImages("model:v2"))
			s.settle()
			names := map[string]string{s.modelServing().Status.UpdateRevision: "second"}
			stands := func(step, says, runs string) {
				t.Helper()
				if got, want := s.created(names), "0=- 1=- 2="+says; got != want {
					t.Fatalf("%s: the groups say %s, want %s", step, got, want)
				}
				got := s.instances()
				if got = got[strings.Index(got, "2["):]; got != runs {
					t.Fatalf("%s: the new group runs\n got %s\nwant %s", step, got, runs)
				}
			}

			if tt.before != nil {
				tt.before(s)
			}
			s.update(threeGroups)
			s.settle()
			if tt.after != nil {
				tt.after(s)
			}
			stands("the group is added", "second", tt.built)

			s.update(setImages("model:v3"))
			s.settle()
			stands("the template changed", "-", tt.changed)

			s.stuck = nil
			s.accept()
			s.settle()
			stands("decode is ready", "-", tt.runs)
			if got, want := s.counts(), "available=3"; !strings.HasSuffix(got, want) {
				t.Errorf("the ModelServing reports %s, want %s", got, want)
			}
		})
	}
}

// A group of coordinated Roles that was created at the target loses
// instances of the Role that depends on another, while that other Role is
// not ready. With RoleRecreate the instances are drained and created anew,
// from the target. Whether they wait depends on what is left of the Role.
// With an instance of it left, or with a place above its partition, the Role
// goes on, and the instances are created again in the next round. With no
// instance left and every place below the partition, the Role starts the
// target anew, and waits until the Role it depends on is ready. A group that
// ran the baseline creates them again from the baseline and never waits.
func TestALostInstanceOfANewGroupOfCoordinatedRolesWaitsOnlyWhenItsRoleStartsAnew(t *testing.T) {
	tests := []struct {
		name      string
		partition string
		// group loses the Pods whose names start with lose.
		group, lose string
		// image is what the instances of the group run.
		image string
		waits bool
	}{
		{name: "a place above the partition, every instance lost", partition: "50%", group: "llama-2", lose: "llama-2-prefill-", image: "model:v2"},
		{name: "every place below the partition, one instance lost", partition: "100%", group: "llama-2", lose: "llama-2-prefill-0-", image: "model:v2"},
		{name: "every place below the partition, every instance lost", partition: "100%", group: "llama-2", lose: "llama-2-prefill-", image: "model:v2", waits: true},
		{name: "the same in a group that ran the baseline", partition: "100%", group: "llama-1", lose: "llama-1-prefill-", image: "model:v1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(coordinated, func(ms *workloadv1alpha1.ModelServing) {
				for i := range ms.Spec.Template.Roles {
					ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromString(tt.partition))
				}
			}))
			s.settle()
			s.update(setImages("model:v2"))
			s.settle()
			s.update(threeGroups)
			s.settle()
			runs := func(step, want string) {
				t.Helper()
				ordinal := strings.TrimPrefix(tt.group, testName+"-")
				got := s.instances()
				got = got[strings.Index(got, ordinal+"["):]
				if got = got[:strings.Index(got, "]")+1]; got != ordinal+"["+want+"]" {
					t.Fatalf("%s: the group runs\n got %s\nwant %s[%s]", step, got, ordinal, want)
				}
			}
			decode := "decode-0=" + tt.image + " decode-1=" + tt.image
			whole := decode + " prefill-0=" + tt.image + " prefill-1=" + tt.image
			runs("at the start", whole)

			// decode is lost and comes back, and does not get ready.
			s.stuck = func(pod *corev1.Pod) bool {
				return pod.Labels[workloadv1alpha1.GroupNameLabelKey] == tt.group && pod.Labels[workloadv1alpha1.RoleLabelKey] == "decode"
			}
			s.lose(tt.group + "-decode-")
			s.settle()
			runs("decode is not ready", whole)

			s.lose(tt.lose)
			s.settle()
			if tt.waits {
				runs("prefill was lost, decode is not ready", decode)
			} else {
				runs("prefill was lost, decode is not ready", whole)
			}
			s.stuck = nil
			s.settle()
			runs("decode is ready", whole)
		})
	}
}

// An open point, written down as it is today. It is not what should happen.
//
// Nothing excludes a Pod that is created after the ModelServing controller
// looked: creating a Pod is not a write of the group. A group that has
// committed, and whose controller creates its Pods at that moment, can be
// told that it was created from the new target and have Pods of the one
// before (the last case of
// TestAGroupIsToldAnotherTargetOrCommitsToItsFirstPodNotBoth). Those Pods are
// held below the partition of their Role like any others.
//
// Roles that are rolled together begin a staged rollout with every instance
// below the partition. Then decode stays at the target of before in such a
// group, and prefill, which does not exist yet, is of the new target and
// waits for capacity of the new target: it is not created. The group is not
// complete until the partitions are lowered, the template returns, or an
// instance of decode is lost and comes back from the new target.
//
// The window is the time between the creation of the Pods and the moment the
// ModelServing controller sees them.
func TestANewGroupThatWasToldTheTargetWhileItsPodsWereCreatedWaits(t *testing.T) {
	const created = workloadv1alpha1.CreatedRevisionAnnotationKey
	s := newSystem(t, modelServing(coordinated, func(ms *workloadv1alpha1.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromString("100%"))
		}
	}))
	s.settle()
	s.update(setImages("model:v2"))
	s.settle()
	runs := func(step, want string) {
		t.Helper()
		got := s.instances()
		if got = got[strings.Index(got, "2["):]; got != want {
			t.Fatalf("%s: the new group runs\n got %s\nwant %s", step, got, want)
		}
	}
	roundOfTheGroup := func() error {
		caches, snapshot := s.caches()
		s.sg.SetCaches(caches, snapshot.Listers().Services)
		_, err := s.sg.Reconcile(context.Background(), testNamespace, "llama-2")
		return err
	}

	// The group is created, commits to its Pods, and its controller dies.
	s.update(threeGroups)
	if _, err := s.roundOfTheModelServing(); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatal(err)
	}
	writes := 0
	s.cluster.BeforeWrite = func(apitestWrite) error {
		if writes++; writes > 1 {
			return errDied
		}
		return nil
	}
	if err := roundOfTheGroup(); !errors.Is(err, errDied) {
		t.Fatalf("round of the group: %v, want it to die after its first write", err)
	}
	s.cluster.BeforeWrite = nil

	// The template changes. The ModelServing controller looks; then the
	// Pods of decode are created, from v2; then it writes.
	s.update(setImages("model:v3"))
	caches, snapshot := s.caches()
	if err := roundOfTheGroup(); err != nil {
		t.Fatalf("round of the group: %v", err)
	}
	runs("the Pods of decode are created", "2[decode-0=model:v2 decode-1=model:v2]")
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); err != nil && !errors.Is(err, ErrNotSettled) {
		t.Fatalf("round of the ModelServing on what it read before: %v", err)
	}
	third := s.modelServing().Status.UpdateRevision
	if group := s.groups()[2]; group.Spec.TargetRevision != third || group.Annotations[created] != third {
		t.Fatalf("the group has the target %s and says %q; want %s for both", group.Spec.TargetRevision, group.Annotations[created], third)
	}

	s.settle()
	runs("at rest", "2[decode-0=model:v2 decode-1=model:v2]")
	condition := meta.FindStatusCondition(s.groups()[2].Status.Conditions, string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked))
	if condition == nil || condition.Status != metav1.ConditionTrue || condition.Reason != "DependencyNotReady" {
		t.Errorf("the new group reports %+v, want it blocked by DependencyNotReady", condition)
	}
	if got, want := s.counts(), "available=2"; !strings.HasSuffix(got, want) {
		t.Errorf("the ModelServing reports %s, want %s", got, want)
	}

	s.update(func(ms *workloadv1alpha1.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			ms.Spec.Template.Roles[i].Partition = ptr.To(intstr.FromString("50%"))
		}
	})
	s.settle()
	runs("the partitions are lowered", "2[decode-0=model:v2 decode-1=model:v3 prefill-0=model:v3 prefill-1=model:v3]")
}
