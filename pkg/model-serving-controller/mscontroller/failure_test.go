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
	"slices"
	"testing"

	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The writes of a round are planned one after the other, each on the
// assumption that the ones before it succeeded. When one fails, the ones
// after it are not issued: the next round plans them again on what is there.
// The settings of the ModelServing are handed down to the groups all the
// same, the status is written, and the round reports the failure.
func TestAWriteThatFailsEndsTheWritesOfThePlan(t *testing.T) {
	refused := errors.New("the API server refused the write")
	tests := []struct {
		name     string
		replicas int32
		// rejected is the write that fails, skipped the one planned after it.
		rejected, skipped string
		// counts is the status the failing round writes; settled the end.
		counts, settled string
	}{
		{
			name: "a deletion", replicas: 1,
			rejected: "delete servinggroups default/llama-2", skipped: "delete servinggroups default/llama-1",
			counts:  "replicas=3 updated=3 current=3 available=3 Progressing",
			settled: "groups[0=model:v1] replicas=1 updated=1 current=1 available=1 Available",
		},
		{
			name: "a creation", replicas: 5,
			rejected: "create servinggroups default/llama-3", skipped: "create servinggroups default/llama-4",
			counts:  "replicas=3 updated=3 current=3 available=3 Progressing",
			settled: "groups[0=model:v1 1=model:v1 2=model:v1 3=model:v1 4=model:v1] replicas=5 updated=5 current=5 available=5 Available",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing())
			s.settle()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To(tt.replicas)
				ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
			})
			s.cluster.ResetWrites()
			s.cluster.BeforeWrite = func(write apitestWrite) error {
				if write.String() == tt.rejected {
					return refused
				}
				return nil
			}

			caches, snapshot := s.caches()
			s.ms.SetCaches(caches, snapshot.Listers().Services)
			if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); !errors.Is(err, refused) {
				t.Errorf("round error = %v, want the failure of the write", err)
			}
			var writes []string
			for _, write := range s.cluster.Writes() {
				writes = append(writes, write.String())
			}
			if slices.Contains(writes, tt.skipped) {
				t.Errorf("writes = %v; %q was issued after the write before it had failed", writes, tt.skipped)
			}
			for _, want := range []string{"update servinggroups default/llama-0", "update servinggroups default/llama-1", "update modelservings/status default/llama"} {
				if !slices.Contains(writes, want) {
					t.Errorf("writes = %v; want %q all the same", writes, want)
				}
			}
			ms := s.modelServing()
			if got := s.counts() + " " + s.condition(); got != tt.counts || ms.Status.ObservedGeneration != ms.Generation {
				t.Errorf("status = %s, observedGeneration %d of %d; want %s and the generation seen", got, ms.Status.ObservedGeneration, ms.Generation, tt.counts)
			}
			if got := s.images(); got != "0=model:v1 1=model:v1 2=model:v1" {
				t.Errorf("groups after the round = %q", got)
			}
			if got := s.groups()[0].Spec.RecoveryPolicy; got != workloadv1alpha1.NoneRestartPolicy {
				t.Errorf("recovery policy of a group = %q; want the setting handed down", got)
			}

			s.cluster.BeforeWrite = nil
			s.settle()
			if got := s.summary(); got != tt.settled {
				t.Errorf("after the write went through:\n got %s\nwant %s", got, tt.settled)
			}
		})
	}
}

// A round is planned on the ModelServing as it was read; its writes follow one
// by one. When the cache shows, before a write, that the ModelServing has
// changed since, the writes that are left are not issued: they were decided
// for a spec that is no more, and a group created now would come from the
// revision of before, right after the user asked for another. The next round
// plans on the ModelServing as it is.
func TestARoundStopsWritingWhenTheModelServingHasChanged(t *testing.T) {
	for _, tt := range []struct {
		name string
		// behind is how many generations the round is behind the cache.
		behind  int64
		creates int
	}{
		{name: "planned on the ModelServing the cache shows", creates: 2},
		{name: "planned on a ModelServing that has changed since", behind: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) }))
			s.settle()
			s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) })
			caches, snapshot := s.caches()
			s.ms.SetCaches(caches, snapshot.Listers().Services)

			planned := s.modelServing()
			planned.Generation -= tt.behind
			r, err := s.ms.plan(context.Background(), planned, split(planned, caches.GroupsNamedAfter(testNamespace, testName)))
			if err != nil {
				t.Fatal(err)
			}
			s.cluster.ResetWrites()
			err = s.ms.execute(context.Background(), r)
			if superseded := errors.Is(err, errSuperseded); superseded != (tt.behind > 0) {
				t.Fatalf("execute = %v, want superseded = %t", err, tt.behind > 0)
			}
			creates := 0
			for _, write := range s.cluster.Writes() {
				if write.Verb == "create" && write.Resource == "servinggroups" {
					creates++
				}
			}
			if creates != tt.creates {
				t.Errorf("%d groups created, want %d: %v", creates, tt.creates, s.cluster.Writes())
			}
		})
	}
}
