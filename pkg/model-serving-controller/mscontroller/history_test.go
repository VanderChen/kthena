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
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// revisions returns the identities of the ControllerRevisions that exist.
func (s *system) revisions() []string {
	s.t.Helper()
	list, err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	var out []string
	for _, revision := range list.Items {
		out = append(out, revision.Labels[utils.ControllerRevisionRevisionLabelKey])
	}
	sort.Strings(out)
	return out
}

// History is kept while anything refers to it, and no longer.
func TestRevisionHistoryIsCleanedUpAtRest(t *testing.T) {
	s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.Partition = ptr.To(intstr.FromInt32(1))
	})))
	s.settle()
	first := s.modelServing().Status.UpdateRevision
	if got := s.revisions(); len(got) != 1 || got[0] != first {
		t.Fatalf("revisions after creation = %v, want %s", got, first)
	}

	// While the rollout runs, every Pod of the old revision keeps it alive.
	s.invariant = func() {
		revisions := s.revisions()
		for _, pod := range s.pods() {
			found := false
			for _, revision := range revisions {
				found = found || revision == pod.Labels[workloadv1alpha1.RevisionLabelKey]
			}
			if !found {
				t.Fatalf("the revision %s of Pod %s was deleted; revisions %v", pod.Labels[workloadv1alpha1.RevisionLabelKey], pod.Name, revisions)
			}
		}
	}
	s.update(setImage("model:v2"))
	s.settle()
	second := s.modelServing().Status.UpdateRevision

	// At the partition the baseline is what a protected group is created from.
	if got := s.revisions(); len(got) != 2 {
		t.Fatalf("revisions at the partition = %v, want both %s and %s", got, first, second)
	}

	// A third template before the second is complete: the second has no group
	// left once the third is rolled out, the first still has the protected one.
	s.update(setImage("model:v3"))
	s.settle()
	third := s.modelServing().Status.UpdateRevision
	if got := s.revisions(); len(got) != 3 {
		t.Fatalf("revisions with a partition = %v; nothing is deleted while a partition is set", got)
	}

	s.update(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.Partition = nil }))
	s.settle()
	s.invariant = nil
	if got := s.revisions(); len(got) != 1 || got[0] != third {
		t.Fatalf("revisions at rest = %v, want only %s", got, third)
	}
	if got, want := s.summary(), "groups[0=model:v3 1=model:v3 2=model:v3] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("at rest:\n got %s\nwant %s", got, want)
	}

	// Going back to a template that was used before records it anew; the
	// revision that is left behind goes.
	s.update(setImage("model:v1"))
	s.settle()
	if got := s.revisions(); len(got) != 1 || got[0] == third {
		t.Fatalf("revisions after going back = %v", got)
	}
}
