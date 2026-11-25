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
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// A group is deleted as it was read, or not at all. Between the read a round
// decides on and its DELETE the group can have been replaced under its name,
// or have changed: it reported, was promoted, or was given another revision.
// The round judged neither of these. Its DELETE is refused and the group is
// judged again.
func TestAGroupIsDeletedOnlyAsItWasRead(t *testing.T) {
	const name = "llama-2"
	tests := []struct {
		name   string
		change func(*workloadv1alpha1.ServingGroup)
	}{
		{name: "another object took the name", change: func(group *workloadv1alpha1.ServingGroup) { group.UID = "another-uid" }},
		{name: "the group was written since", change: func(group *workloadv1alpha1.ServingGroup) { group.ResourceVersion += "0" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing())
			s.settle()
			s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })

			// The DELETE of the round reaches the API server after the change.
			groups := workloadv1alpha1.SchemeGroupVersion.WithResource("servinggroups")
			tracker := s.cluster.Kthena.Tracker()
			changed := false
			s.cluster.Kthena.PrependReactor("delete", "servinggroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if changed || action.(k8stesting.DeleteAction).GetName() != name {
					return false, nil, nil
				}
				changed = true
				stored, err := tracker.Get(groups, testNamespace, name)
				if err != nil {
					return true, nil, err
				}
				group := stored.(*workloadv1alpha1.ServingGroup).DeepCopy()
				tt.change(group)
				return false, nil, tracker.Update(groups, group, testNamespace)
			})

			caches, snapshot := s.caches()
			s.ms.SetCaches(caches, snapshot.Listers().Services)
			if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); !apierrors.IsConflict(err) {
				t.Errorf("round error = %v, want the conflict of a deletion that no longer applies", err)
			}
			if !changed {
				t.Fatal("the round did not delete the group")
			}
			group, err := s.cluster.GetServingGroup(testNamespace, name)
			if err != nil {
				t.Fatalf("the group as it is now: %v", err)
			}
			if group.DeletionTimestamp != nil {
				t.Fatalf("the group as it is now, UID %s, is being deleted", group.UID)
			}

			// The next round judges the group as it is, and scales in.
			s.settle()
			if got, want := s.summary(), "groups[0=model:v1 1=model:v1] replicas=2 updated=2 current=2 available=2 Available"; got != want {
				t.Errorf("after the next rounds:\n got %s\nwant %s", got, want)
			}
		})
	}
}
