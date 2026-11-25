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
	"reflect"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// revisionNames returns the names of the ControllerRevisions that exist.
func (s *system) revisionNames() []string {
	s.t.Helper()
	list, err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	var out []string
	for _, revision := range list.Items {
		out = append(out, revision.Name)
	}
	sort.Strings(out)
	return out
}

// unusedRevision adds a ControllerRevision that records another template of
// the ModelServing under the identity revision, changed by mutate. As it is,
// nothing refers to it and the ModelServing controls it: the cleanup deletes it.
func (s *system) unusedRevision(revision string, mutate func(*appsv1.ControllerRevision)) *appsv1.ControllerRevision {
	s.t.Helper()
	other := s.modelServing()
	setImage("model:" + revision)(other)
	cr := legacyRevision(s.t, other, revision, false)
	if mutate != nil {
		mutate(cr)
	}
	created, err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Create(context.Background(), cr, metav1.CreateOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	return created
}

// The cleanup of the revision history deletes what the ModelServing
// controller recorded for this ModelServing and nothing else. Other objects
// carry the label of the ModelServing as well: the history of a ModelServing
// of that name that was deleted and created again, and whatever a user or
// another tool keeps there. They are not this controller's to delete.
func TestTheCleanupLeavesWhatTheModelServingDidNotRecord(t *testing.T) {
	another := modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Name, ms.UID = "another", "another-uid" })
	s := newSystem(t, modelServing(), another)
	s.settle()
	target := utils.GenerateControllerRevisionName(testName, s.modelServing().Status.UpdateRevision)

	s.unusedRevision("unused", nil)
	s.unusedRevision("another-owner", func(cr *appsv1.ControllerRevision) {
		cr.OwnerReferences[0].Name, cr.OwnerReferences[0].UID = another.Name, another.UID
	})
	s.unusedRevision("no-owner", func(cr *appsv1.ControllerRevision) { cr.OwnerReferences = nil })
	s.unusedRevision("not-controlled", func(cr *appsv1.ControllerRevision) { cr.OwnerReferences[0].Controller = nil })
	s.unusedRevision("another-name", func(cr *appsv1.ControllerRevision) { cr.Name = "kept-by-someone" })
	s.unusedRevision("no-identity", func(cr *appsv1.ControllerRevision) {
		delete(cr.Labels, utils.ControllerRevisionRevisionLabelKey)
	})

	s.settle()
	want := []string{"kept-by-someone", "llama-another-owner", "llama-no-identity", "llama-no-owner", "llama-not-controlled", target}
	sort.Strings(want)
	if got := s.revisionNames(); !reflect.DeepEqual(got, want) {
		t.Errorf("ControllerRevisions after the cleanup:\n got %v\nwant %v", got, want)
	}
}

// The cleanup deletes a revision when nothing refers to it. What the Pods of
// a group refer to is in the status of the group, so nothing is deleted
// while a group has not reported on its spec and on the Pods the cache
// shows: its status may list less than its Pods run, and a Pod whose revision
// is gone can never be created again as it was.
func TestTheCleanupWaitsForEveryGroupToHaveReported(t *testing.T) {
	const unused = "llama-unused"
	round := func(s *system) {
		s.t.Helper()
		caches, snapshot := s.caches()
		s.ms.SetCaches(caches, snapshot.Listers().Services)
		if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); err != nil && !errors.Is(err, ErrNotSettled) {
			s.t.Fatal(err)
		}
	}
	kept := func(s *system, when string) {
		s.t.Helper()
		for range 3 {
			round(s)
		}
		found := false
		for _, name := range s.revisionNames() {
			found = found || name == unused
		}
		if !found || s.condition() != "Available" {
			s.t.Fatalf("%s: the revision is there = %t, the ModelServing is %s; want nothing deleted while it is available", when, found, s.condition())
		}
	}
	gone := func(s *system) {
		s.t.Helper()
		s.settle()
		for _, name := range s.revisionNames() {
			if name == unused {
				s.t.Errorf("the revision is still there after every group has reported")
			}
		}
	}

	t.Run("a Pod changed", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		// A Pod stops being ready, and its group has not looked yet.
		pod := s.pods()[2]
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(s.cluster.Now()),
		}}
		if _, err := s.cluster.Kube.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		s.unusedRevision("unused", nil)
		kept(s, "a group has not reported on its Pods")
		gone(s)
	})

	t.Run("the spec of the groups changed", func(t *testing.T) {
		s := newSystem(t, modelServing())
		s.settle()
		// A setting is handed down, and the groups have not looked yet.
		s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy })
		round(s)
		s.unusedRevision("unused", nil)
		kept(s, "the groups have not reported on their spec")
		gone(s)
	})
}
