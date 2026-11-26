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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
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
