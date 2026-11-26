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
	"reflect"
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
