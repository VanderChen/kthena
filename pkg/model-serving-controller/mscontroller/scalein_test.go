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
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// errRevisionsUnavailable is what the API server stand-in answers during an
// outage of the ControllerRevisions.
var errRevisionsUnavailable = errors.New("injected: ControllerRevisions are unavailable")

// outage is a time during which no ControllerRevision can be written, so that
// the revision of a template that changed cannot be recorded and the rounds
// of the ModelServing have no target. Every pass in it checks what holds
// throughout: nothing is done that needs a target.
type outage struct {
	s *system
	// revisions holds, by the UID of a group, the revisions its spec named
	// when the outage began.
	revisions map[types.UID]string
	// failure is the error of the last round of the ModelServing.
	failure error
	// over ends a fault that is not a rejected write.
	over func()
}

func revisionsOf(group *workloadv1alpha1.ServingGroup) string {
	return fmt.Sprintf("revision %q, target %q, baseline %q", group.Spec.Revision, group.Spec.TargetRevision, group.Spec.BaselineRevision)
}

// outage begins a time during which every write of a ControllerRevision is
// rejected, and empties the write log.
func (s *system) outage() *outage {
	s.t.Helper()
	o := &outage{s: s, revisions: map[types.UID]string{}}
	for _, group := range s.groups() {
		o.revisions[group.UID] = revisionsOf(group)
	}
	s.cluster.BeforeWrite = func(write apitestWrite) error {
		if write.Resource == "controllerrevisions" {
			return errRevisionsUnavailable
		}
		return nil
	}
	s.cluster.ResetWrites()
	return o
}

// unlistable begins an outage during which the ControllerRevisions cannot
// even be listed: the revision of a template that did not change cannot be
// looked up either.
func (s *system) unlistable() *outage {
	s.t.Helper()
	o := s.outage()
	failing := true
	o.over = func() { failing = false }
	s.cluster.Kube.PrependReactor("list", "controllerrevisions", func(k8stesting.Action) (bool, runtime.Object, error) {
		if failing {
			return true, nil, errRevisionsUnavailable
		}
		return false, nil, nil
	})
	return o
}

// end lets the ControllerRevisions be read and written again.
func (o *outage) end() {
	o.s.cluster.BeforeWrite = nil
	if o.over != nil {
		o.over()
	}
}

// round runs one round of the ModelServing controller on a fresh cache. The
// round has to fail with the fault of the outage: that is what keeps it on
// the queue. Its error is kept in failure.
func (o *outage) round() {
	s := o.s
	s.t.Helper()
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	_, o.failure = s.ms.Reconcile(context.Background(), testNamespace, testName)
	if !errors.Is(o.failure, errTargetUnknown) || !errors.Is(o.failure, errRevisionsUnavailable) {
		s.t.Fatalf("round of the ModelServing during the outage: %v; want the revision error", o.failure)
	}
}

// pass runs one round of the ModelServing controller and one round of the
// ServingGroup controller for every group, like system.pass, and checks that
// nothing was done that needs a target. It returns the number of writes the
// controllers made.
func (o *outage) pass() int {
	s := o.s
	s.t.Helper()
	before := len(s.cluster.Writes())
	o.round()
	for _, group := range s.groups() {
		caches, snapshot := s.caches()
		s.sg.SetCaches(caches, snapshot.Listers().Services)
		if _, err := s.sg.Reconcile(context.Background(), testNamespace, group.Name); err != nil {
			s.t.Fatalf("round of ServingGroup %s: %v", group.Name, err)
		}
	}
	o.check()
	return len(s.cluster.Writes()) - before
}

// check fails the test when something was done since the outage began that
// needs a target: a group or a Pod was created, a group was told another
// revision, or the status of the ModelServing was written.
func (o *outage) check() {
	s := o.s
	s.t.Helper()
	for _, write := range s.cluster.Writes() {
		created := write.Verb == "create" && (write.Resource == "servinggroups" || write.Resource == "pods")
		reported := write.Resource == "modelservings" && (write.Subresource == "status" || write.Verb == "patch")
		if created || reported {
			s.t.Fatalf("during the outage the controllers issued: %s", write)
		}
	}
	for _, group := range s.groups() {
		before, known := o.revisions[group.UID]
		if !known {
			s.t.Fatalf("ServingGroup %s was created during the outage", group.Name)
		}
		if got := revisionsOf(group); got != before {
			s.t.Fatalf("ServingGroup %s was told other revisions during the outage: %s, before %s", group.Name, got, before)
		}
	}
}

// settle runs passes until one writes nothing, like system.settle.
func (o *outage) settle() {
	s := o.s
	s.t.Helper()
	for range 100 {
		s.kubelet()
		s.cluster.Settle()
		if o.pass() == 0 {
			return
		}
	}
	s.t.Fatalf("the ModelServing did not settle during the outage: %s", s.summary())
}

// deleted lists the groups that were deleted since the write log was emptied,
// in the order of the requests.
func (s *system) deleted() []string {
	var out []string
	for _, write := range s.cluster.Writes() {
		if write.Verb == "delete" && write.Resource == "servinggroups" {
			out = append(out, write.Name)
		}
	}
	return out
}

// podsOf returns the UIDs of the Pods of a group by name.
func (s *system) podsOf(group string) map[string]types.UID {
	s.t.Helper()
	uids := map[string]types.UID{}
	for _, pod := range s.pods() {
		if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == group {
			uids[pod.Name] = pod.UID
		}
	}
	return uids
}

// A ModelServing whose template changed while its revision cannot be recorded
// has no target, and nothing may be created or replaced for it. Its replica
// count went down in the same change, and that needs no target: the groups
// above the count are deleted, in the order of any scale-in. The group that
// stays keeps its Pods and its revision, and the status is left as it was,
// for the generation was not observed in full. Every round fails with the
// revision error, so the queue keeps trying. Once the revision can be
// recorded, the rollout happens.
func TestAScaleInIsCarriedOutWhileTheRevisionCannotBeRecorded(t *testing.T) {
	tests := []struct {
		name   string
		outage func(*system) *outage
		// costly, when set, is a group whose Pods are expensive to delete.
		costly  string
		deleted []string
		stays   string
	}{
		{name: "the revision cannot be written", outage: (*system).outage,
			deleted: []string{"llama-2", "llama-1"}, stays: "llama-0"},
		{name: "the revisions cannot be listed", outage: (*system).unlistable,
			deleted: []string{"llama-2", "llama-1"}, stays: "llama-0"},
		{name: "the group with the highest ordinal is the most expensive to delete", outage: (*system).outage, costly: "llama-2",
			deleted: []string{"llama-1", "llama-0"}, stays: "llama-2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing())
			s.settle()
			for _, pod := range s.pods() {
				if pod.Labels[workloadv1alpha1.GroupNameLabelKey] != tt.costly {
					continue
				}
				if pod.Annotations == nil {
					pod.Annotations = map[string]string{}
				}
				pod.Annotations[corev1.PodDeletionCost] = "100"
				if _, err := s.cluster.Kube.CoreV1().Pods(testNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatal(err)
				}
			}
			s.settle()
			before := s.modelServing()
			kept := s.podsOf(tt.stays)

			o := tt.outage(s)
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](1)
				setImage("model:v2")(ms)
			})
			o.pass()
			if got := s.deleted(); !reflect.DeepEqual(got, tt.deleted) {
				t.Fatalf("the first round deleted %v, want %v", got, tt.deleted)
			}
			o.settle()

			if got, want := s.images(), strings.TrimPrefix(tt.stays, testName+"-")+"=model:v1"; got != want {
				t.Fatalf("groups during the outage: %s, want %s", got, want)
			}
			if got := s.podsOf(tt.stays); !reflect.DeepEqual(got, kept) {
				t.Errorf("the Pods of the group that stays were replaced: %v, before %v", got, kept)
			}
			ms := s.modelServing()
			if !apiequality.Semantic.DeepEqual(ms.Status, before.Status) || ms.Status.ObservedGeneration >= ms.Generation {
				t.Errorf("the status was written for a generation that was not observed in full: %+v, generation %d", ms.Status, ms.Generation)
			}

			// The group that is replaced comes back at the lowest free ordinal.
			o.end()
			s.settle()
			if got, want := s.summary(), "groups[0=model:v2] replicas=1 updated=1 current=1 available=1 Available"; got != want {
				t.Fatalf("after the outage:\n got %s\nwant %s", got, want)
			}
			if ms := s.modelServing(); ms.Status.ObservedGeneration != ms.Generation {
				t.Errorf("observedGeneration = %d, generation %d", ms.Status.ObservedGeneration, ms.Generation)
			}
		})
	}
}

// Without the target nothing proves that the surge of a rollout by recreation
// is not needed any more, so its allowance is kept: groups are deleted only
// while more than replicas plus maxSurge of them exist, whether a surge group
// exists yet or not. The groups above the count go first, a group with the
// surge mark only after them. A ModelServing that is updated in place does
// not roll by surge and goes down to replicas, and one that is scaled to zero
// loses every group, the surge included.
func TestTheSurgeAllowanceIsKeptWhileTheRevisionCannotBeRecorded(t *testing.T) {
	surge := func(maxUnavailable int32) func(*workloadv1alpha1.ModelServing) {
		return rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
			config.MaxUnavailable, config.MaxSurge = ptr.To(intstr.FromInt32(maxUnavailable)), ptr.To(intstr.FromInt32(1))
		})
	}
	tests := []struct {
		name   string
		mutate []func(*workloadv1alpha1.ModelServing)
		// rolling: when the outage begins, a rollout to model:v2 waits for
		// its surge group to become ready.
		rolling bool
		// settle is system.settle unless the kubelet has to restart containers.
		settle   func(*system)
		replicas int32
		deleted  []string
		stay     string
		after    string
	}{
		{name: "a surge group exists", mutate: []func(*workloadv1alpha1.ModelServing){surge(0)}, rolling: true, replicas: 1,
			deleted: []string{"llama-2", "llama-1"}, stay: "0=model:v1 3=model:v2+",
			after: "groups[0=model:v3] replicas=1 updated=1 current=1 available=1 Available"},
		{name: "no surge group exists yet", mutate: []func(*workloadv1alpha1.ModelServing){surge(0)}, replicas: 1,
			deleted: []string{"llama-2"}, stay: "0=model:v1 1=model:v1",
			after: "groups[0=model:v3] replicas=1 updated=1 current=1 available=1 Available"},
		{name: "updated in place", mutate: []func(*workloadv1alpha1.ModelServing){surge(1), updatedInPlace}, settle: (*system).settleContainers, replicas: 1,
			deleted: []string{"llama-2", "llama-1"}, stay: "0=model:v1",
			after: "groups[0=model:v3] replicas=1 updated=1 current=1 available=1 Available"},
		{name: "scaled to zero", mutate: []func(*workloadv1alpha1.ModelServing){surge(0)}, replicas: 0,
			deleted: []string{"llama-2", "llama-1", "llama-0"}, stay: "",
			after: "groups[] replicas=0 updated=0 current=0 available=0 Available"},
		{name: "scaled to zero while a surge group exists", mutate: []func(*workloadv1alpha1.ModelServing){surge(0)}, rolling: true, replicas: 0,
			deleted: []string{"llama-2", "llama-1", "llama-0", "llama-3"}, stay: "",
			after: "groups[] replicas=0 updated=0 current=0 available=0 Available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			settle := (*system).settle
			if tt.settle != nil {
				settle = tt.settle
			}
			s := newSystem(t, modelServing(tt.mutate...))
			settle(s)
			if tt.rolling {
				s.stuck = func(pod *corev1.Pod) bool { return pod.Spec.Containers[0].Image == "model:v2" }
				s.update(setImage("model:v2"))
				settle(s)
				if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1 3=model:v2+"; got != want {
					t.Fatalf("in the middle of the rollout: %s, want %s", got, want)
				}
			}

			o := s.outage()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To(tt.replicas)
				setImage("model:v3")(ms)
			})
			o.settle()
			if got := s.deleted(); !reflect.DeepEqual(got, tt.deleted) {
				t.Errorf("deleted %v, want %v", got, tt.deleted)
			}
			if got := s.images(); got != tt.stay {
				t.Fatalf("groups during the outage: %s, want %s", got, tt.stay)
			}

			o.end()
			settle(s)
			if got := s.summary(); got != tt.after {
				t.Fatalf("after the outage:\n got %s\nwant %s", got, tt.after)
			}
		})
	}
}

// Of the writes of a plan a round without a target keeps the deletions of a
// scale-in and of surge, in the order of the plan, and of those only as many
// as there are groups above the bound. Groups that are terminating or failed
// do not count: they are on their way out.
func TestARoundWithoutATargetKeepsOnlyTheDeletionsOfAScaleIn(t *testing.T) {
	plan := []grouplevel.Action{
		{Kind: grouplevel.ActionCreate, Ordinal: 9, Reason: grouplevel.ReasonRecover},
		{Kind: grouplevel.ActionPromote, Ordinal: 0},
		{Kind: grouplevel.ActionDelete, Ordinal: 8, Reason: grouplevel.ReasonFailed},
		{Kind: grouplevel.ActionDelete, Ordinal: 5, Reason: grouplevel.ReasonScaleDown},
		{Kind: grouplevel.ActionDelete, Ordinal: 4, Reason: grouplevel.ReasonScaleDown},
		{Kind: grouplevel.ActionDelete, Ordinal: 7, Reason: grouplevel.ReasonMemberlessOutOfRange},
		{Kind: grouplevel.ActionDelete, Ordinal: 7, Reason: grouplevel.ReasonEmptyOutOfRange},
		{Kind: grouplevel.ActionRetarget, Ordinal: 1, Reason: grouplevel.ReasonMemberless},
		{Kind: grouplevel.ActionCreate, Ordinal: 2, Reason: grouplevel.ReasonRegular},
		{Kind: grouplevel.ActionCreate, Ordinal: 10, Surge: true, Reason: grouplevel.ReasonSurge},
		{Kind: grouplevel.ActionDelete, Ordinal: 6, Reason: grouplevel.ReasonReclaimSurge},
		{Kind: grouplevel.ActionDelete, Ordinal: 3, Reason: grouplevel.ReasonReplaceNotReady},
		{Kind: grouplevel.ActionDelete, Ordinal: 1, Reason: grouplevel.ReasonReplace},
		{Kind: grouplevel.ActionRetarget, Ordinal: 0, Reason: grouplevel.ReasonInPlace},
		{Kind: grouplevel.ActionAdvanceBaseline, Ordinal: -1, Reason: grouplevel.ReasonComplete},
		{Kind: grouplevel.ActionWait, Ordinal: 2, Reason: grouplevel.ReasonBaselineUnknown},
	}
	tests := []struct {
		name string
		spec grouplevel.Spec
		// live, terminating and failed are the numbers of such groups.
		live, terminating, failed int
		want                      []int
	}{
		{name: "nothing but scale-in and surge is deleted, however many groups are above the bound",
			spec: grouplevel.Spec{Replicas: 2}, live: 9, want: []int{5, 4, 6}},
		{name: "as many as are above replicas", spec: grouplevel.Spec{Replicas: 6}, live: 8, want: []int{5, 4}},
		{name: "the surge allowance is added to the bound", spec: grouplevel.Spec{Replicas: 6, MaxSurge: 1}, live: 8, want: []int{5}},
		{name: "not when groups are updated in place", spec: grouplevel.Spec{Replicas: 6, MaxSurge: 1, InPlace: true}, live: 8, want: []int{5, 4}},
		{name: "nor at zero replicas", spec: grouplevel.Spec{Replicas: 0, MaxSurge: 1}, live: 3, want: []int{5, 4, 6}},
		{name: "a group that is terminating does not count", spec: grouplevel.Spec{Replicas: 6}, live: 7, terminating: 3, want: []int{5}},
		{name: "a group that failed does not count", spec: grouplevel.Spec{Replicas: 6}, live: 7, failed: 3, want: []int{5}},
		{name: "nothing at the bound", spec: grouplevel.Spec{Replicas: 6, MaxSurge: 2}, live: 8},
		{name: "nothing below the bound", spec: grouplevel.Spec{Replicas: 6}, live: 4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &view{spec: tt.spec}
			for range tt.live {
				v.groups = append(v.groups, groupState{})
			}
			for range tt.terminating {
				v.groups = append(v.groups, groupState{unit: grouplevel.Group{Terminating: true}})
			}
			for range tt.failed {
				v.groups = append(v.groups, groupState{unit: grouplevel.Group{Failed: true}})
			}
			var got []int
			for _, action := range scaleInOnly(v, plan) {
				if action.Kind != grouplevel.ActionDelete {
					t.Errorf("kept %+v", action)
				}
				got = append(got, action.Ordinal)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("kept the deletions of %v, want %v", got, tt.want)
			}
		})
	}
}

// A replica count that went up is not carried out without a target: a group,
// or an instance of a Role, could only be created from the revision that is
// not recorded. The controllers write nothing at all. Once the revision can
// be recorded, the groups are created from it.
func TestNothingIsCreatedForARevisionThatCannotBeRecorded(t *testing.T) {
	tests := []struct {
		name string
		// base, when set, turns the ModelServing of three groups into another.
		base  func(*workloadv1alpha1.ModelServing)
		more  func(*workloadv1alpha1.ModelServing)
		after string
	}{
		{name: "more groups", more: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) },
			after: "0[0=model:v2] 1[0=model:v2] 2[0=model:v2] 3[0=model:v2] 4[0=model:v2]"},
		{name: "more instances of a Role", more: func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) },
			after: "0[0=model:v2 1=model:v2] 1[0=model:v2 1=model:v2] 2[0=model:v2 1=model:v2]"},
		{name: "more instances of a Role that is rolled by itself", base: roleRolling(),
			more:  func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](4) },
			after: "0[0=model:v2 1=model:v2 2=model:v2 3=model:v2] 1[0=model:v2 1=model:v2 2=model:v2 3=model:v2]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := tt.base
			if base == nil {
				base = func(*workloadv1alpha1.ModelServing) {}
			}
			s := newSystem(t, modelServing(base))
			s.settle()
			specs := map[string]workloadv1alpha1.ServingGroupSpec{}
			for _, group := range s.groups() {
				specs[group.Name] = group.Spec
			}
			pods := s.podUIDs()

			o := s.outage()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				tt.more(ms)
				setImage("model:v2")(ms)
			})
			o.settle()
			for _, write := range s.cluster.Writes() {
				if write.Resource != "modelservings" || write.Verb != "update" || write.Subresource != "" {
					t.Errorf("during the outage the controllers issued: %s", write)
				}
			}
			for _, group := range s.groups() {
				if !apiequality.Semantic.DeepEqual(group.Spec, specs[group.Name]) {
					t.Errorf("the spec of %s changed during the outage: %+v", group.Name, group.Spec)
				}
			}
			if got := s.podUIDs(); !reflect.DeepEqual(got, pods) {
				t.Errorf("the Pods changed during the outage: %v, before %v", got, pods)
			}
			// A round that has nothing to write does not ask the API server
			// what the cache already says.
			s.cluster.Kthena.ClearActions()
			o.round()
			if actions := s.cluster.Kthena.Actions(); len(actions) != 0 {
				t.Errorf("a round with nothing to scale in sent %d requests, the first one: %+v", len(actions), actions[0])
			}

			o.end()
			s.settle()
			if got := s.instances(); got != tt.after {
				t.Fatalf("after the outage: %s, want %s", got, tt.after)
			}
		})
	}
}

// groupUpdates counts, by group, the writes of a spec since the write log
// was emptied.
func (s *system) groupUpdates() map[string]int {
	out := map[string]int{}
	for _, write := range s.cluster.Writes() {
		if write.Verb == "update" && write.Resource == "servinggroups" && write.Subresource == "" {
			out[write.Name]++
		}
	}
	return out
}

// The count of a Role that went down is handed to the groups although the
// rest of the template cannot be: the group removes the instances above the
// count by its own rules and leaves the others alone. The number of Pods the
// group expects follows, in one write for each group that says which
// generation it comes from. A count that went up in the same change is not
// handed down, and no Role is added or removed. Once the revision can be
// recorded, the rest follows.
func TestTheCountOfARoleIsLoweredWhileTheRevisionCannotBeRecorded(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	entry := func(name string) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{Name: name, Replicas: ptr.To[int32](1), EntryTemplate: podTemplate(name + ":v1")}
	}
	others := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, entry("route"), entry("cache"))
	}
	s := newSystem(t, modelServing(two, others))
	s.settle()
	if got, want := s.handedDown(), "0[serve=2/- route=1/- cache=1/-] 1[serve=2/- route=1/- cache=1/-] 2[serve=2/- route=1/- cache=1/-]"; got != want {
		t.Fatalf("created: %s, want %s", got, want)
	}
	// stay holds the Pods of the instances that are not above the lower count.
	stay := map[string]types.UID{}
	for _, pod := range s.pods() {
		if pod.Labels[workloadv1alpha1.RoleIDKey] != "serve-1" {
			stay[pod.Name] = pod.UID
		}
	}

	// serve goes down, route goes up, cache is replaced by embed.
	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		roles := ms.Spec.Template.Roles
		roles[0].Replicas = ptr.To[int32](1)
		roles[1].Replicas = ptr.To[int32](2)
		roles[2] = entry("embed")
		setImage("model:v2")(ms)
	})
	o.settle()

	if got, want := s.handedDown(), "0[serve=1/- route=1/- cache=1/-] 1[serve=1/- route=1/- cache=1/-] 2[serve=1/- route=1/- cache=1/-]"; got != want {
		t.Fatalf("handed down during the outage: %s, want %s", got, want)
	}
	if got, want := s.groupUpdates(), map[string]int{"llama-0": 1, "llama-1": 1, "llama-2": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes of group specs during the outage: %v, want %v", got, want)
	}
	generation := s.modelServing().Generation
	for _, group := range s.groups() {
		// One instance of serve, an entry Pod and a worker, and the entry
		// Pods of route and of cache.
		if group.Spec.ExpectedPods != 4 || group.Spec.SourceGeneration != generation {
			t.Errorf("%s expects %d Pods from generation %d, want 4 from generation %d", group.Name, group.Spec.ExpectedPods, group.Spec.SourceGeneration, generation)
		}
		if !groupReady(group) {
			t.Errorf("%s is not ready with the lower count", group.Name)
		}
	}
	if got := s.podUIDs(); !reflect.DeepEqual(got, stay) {
		t.Errorf("Pods during the outage: %v, want the ones below the lower count: %v", got, stay)
	}

	o.end()
	s.settle()
	if got, want := s.handedDown(), "0[serve=1/- route=2/- embed=1/-] 1[serve=1/- route=2/- embed=1/-] 2[serve=1/- route=2/- embed=1/-]"; got != want {
		t.Fatalf("handed down after the outage: %s, want %s", got, want)
	}
	if got, want := s.counts()+" "+s.condition(), "replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("status after the outage = %s, want %s", got, want)
	}
}

// The partition holds a group at its revision, and a count is no part of a
// revision: a group below the partition is lowered like the others, as it is
// when a round with a target hands the settings down. Nothing else of it
// changes. The count is not lowered in any group of a ModelServing whose
// Roles are rolled together: such a rollout measures a Role against the count
// recorded with a revision, and nothing is recorded now. The number of its
// groups goes down all the same.
func TestTheCountOfARoleIsNotLoweredEverywhere(t *testing.T) {
	t.Run("a group below the partition is lowered as well", func(t *testing.T) {
		two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
		partition := rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
			config.Partition = ptr.To(intstr.FromInt32(2))
		})
		s := newSystem(t, modelServing(two, partition))
		s.settle()
		// want holds the spec of every group as it is to be after the round:
		// the lower count, the number of Pods that goes with it, an entry Pod
		// and a worker, and the generation it comes from.
		want := map[string]workloadv1alpha1.ServingGroupSpec{}
		for _, group := range s.groups() {
			spec := *group.Spec.DeepCopy()
			spec.Roles[0].Replicas, spec.ExpectedPods = 1, 2
			want[group.Name] = spec
		}
		// stay holds the Pods of the instances that are not above the lower count.
		stay := map[string]types.UID{}
		for _, pod := range s.pods() {
			if pod.Labels[workloadv1alpha1.RoleIDKey] != "serve-1" {
				stay[pod.Name] = pod.UID
			}
		}

		o := s.outage()
		s.update(func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
			setImage("model:v2")(ms)
		})
		o.settle()
		if got, want := s.handedDown(), "0[serve=1/-] 1[serve=1/-] 2[serve=1/-]"; got != want {
			t.Fatalf("handed down during the outage: %s, want %s", got, want)
		}
		generation := s.modelServing().Generation
		for _, group := range s.groups() {
			spec := want[group.Name]
			spec.SourceGeneration = generation
			if !apiequality.Semantic.DeepEqual(group.Spec, spec) {
				t.Errorf("the spec of %s during the outage: %+v, want %+v", group.Name, group.Spec, spec)
			}
		}
		if got := s.podUIDs(); !reflect.DeepEqual(got, stay) {
			t.Errorf("Pods during the outage: %v, want the ones below the lower count: %v", got, stay)
		}

		// The partition still holds: only the group above it is rolled.
		o.end()
		s.settle()
		if got, want := s.instances(), "0[0=model:v1] 1[0=model:v1] 2[0=model:v2]"; got != want {
			t.Fatalf("instances after the outage: %s, want %s", got, want)
		}
	})

	t.Run("where the Roles are rolled together", func(t *testing.T) {
		three := func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
			ms.Spec.Template.Roles[1].Replicas = ptr.To[int32](3)
		}
		s := newSystem(t, modelServing(coordinated, three))
		s.settle()
		if got, want := s.handedDown(), "0[prefill=3/3 decode=3/3] 1[prefill=3/3 decode=3/3]"; got != want {
			t.Fatalf("created: %s, want %s", got, want)
		}
		pods := s.podsOf("llama-0")

		o := s.outage()
		s.update(func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Replicas = ptr.To[int32](1)
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			ms.Spec.Template.Roles[1].Replicas = ptr.To[int32](2)
			setImages("model:v2")(ms)
		})
		o.settle()
		if got, want := s.handedDown(), "0[prefill=3/3 decode=3/3]"; got != want {
			t.Fatalf("handed down during the outage: %s, want %s", got, want)
		}
		if got := s.groupUpdates(); len(got) != 0 {
			t.Errorf("writes of group specs during the outage: %v", got)
		}
		if got := s.podUIDs(); !reflect.DeepEqual(got, pods) {
			t.Errorf("Pods during the outage: %v, want the ones of the group that stays: %v", got, pods)
		}

		o.end()
		s.settle()
		if got, want := s.handedDown(), "0[prefill=2/2 decode=2/2]"; got != want {
			t.Fatalf("handed down after the outage: %s, want %s", got, want)
		}
		if got, want := s.instances(), "0[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v2 prefill-1=model:v2]"; got != want {
			t.Fatalf("instances after the outage: %s, want %s", got, want)
		}
	})
}

// Under RoleRollingUpdate the surge of a Role is an instance with a mark of
// its own, and the group knows the rollout it belongs to: the one to the
// target it was told last. A lower count takes the regular instances down to
// it; the surge instance stays for as long as that rollout needs it.
func TestTheSurgeOfARoleIsKeptByItsGroup(t *testing.T) {
	s := newSystem(t, modelServing(roleRolling(func(role *workloadv1alpha1.Role) {
		role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
	})))
	s.settle()
	s.stuck = func(pod *corev1.Pod) bool { return pod.Spec.Containers[0].Image == "model:v2" }
	s.update(setImage("model:v2"))
	s.settle()
	if got, want := s.instances(), "0[0=model:v1 1=model:v1 2=model:v1 3=model:v2+] 1[0=model:v1 1=model:v1 2=model:v1 3=model:v2+]"; got != want {
		t.Fatalf("in the middle of the rollout: %s, want %s", got, want)
	}

	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
		setImage("model:v3")(ms)
	})
	o.settle()
	if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-]"; got != want {
		t.Fatalf("handed down during the outage: %s, want %s", got, want)
	}
	if got, want := s.instances(), "0[0=model:v1 1=model:v1 3=model:v2+] 1[0=model:v1 1=model:v1 3=model:v2+]"; got != want {
		t.Fatalf("instances during the outage: %s, want %s", got, want)
	}

	o.end()
	s.settle()
	if got, want := s.instances(), "0[0=model:v3 1=model:v3] 1[0=model:v3 1=model:v3]"; got != want {
		t.Fatalf("instances after the outage: %s, want %s", got, want)
	}
}

// The number of Pods a group expects is computed for the layout of the
// revision the group runs. Where that revision cannot be read the number
// cannot be told, and the count of the Role is left as it is rather than
// written with a number of Pods that does not go with it. The other groups
// are lowered, and the round says which one was not.
func TestTheCountOfARoleIsNotLoweredWhereTheRevisionOfTheGroupCannotBeRead(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	s := newSystem(t, modelServing(two))
	s.settle()
	first := s.modelServing().Status.UpdateRevision
	// A rollout stops after its first group: llama-2 runs the target, the
	// others still run the revision before it.
	s.stuck = func(pod *corev1.Pod) bool { return pod.Spec.Containers[0].Image == "model:v2" }
	s.update(setImage("model:v2"))
	s.settle()
	if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v2"; got != want {
		t.Fatalf("in the middle of the rollout: %s, want %s", got, want)
	}
	name := utils.GenerateControllerRevisionName(testName, first)
	if err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}

	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
		setImage("model:v3")(ms)
	})
	o.settle()
	if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-] 2[serve=1/-]"; got != want {
		t.Fatalf("handed down during the outage: %s, want %s", got, want)
	}
	for _, group := range s.groups() {
		if want := map[string]int32{"llama-0": 4, "llama-1": 4, "llama-2": 2}[group.Name]; group.Spec.ExpectedPods != want {
			t.Errorf("%s expects %d Pods, want %d", group.Name, group.Spec.ExpectedPods, want)
		}
	}
	o.round()
	for _, group := range []string{"llama-0", "llama-1"} {
		if !strings.Contains(o.failure.Error(), "lower the Role counts of ServingGroup "+group) {
			t.Errorf("the round does not say that %s was left as it is: %v", group, o.failure)
		}
	}
}

// The last target that was recorded stands in for the one that is missing. A
// ModelServing whose status names none, or names one that cannot be read, has
// no stand-in: nothing is known about what its groups were created from, and
// they are left to a round that has a target.
func TestNothingIsScaledInWithoutARecordedTarget(t *testing.T) {
	tests := []struct {
		name string
		lose func(*testing.T, *system)
	}{
		{name: "the status names no target", lose: func(t *testing.T, s *system) {
			ms := s.modelServing()
			ms.Status.UpdateRevision = ""
			if _, err := s.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).UpdateStatus(context.Background(), ms, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "the revision of the last target is gone", lose: func(t *testing.T, s *system) {
			name := utils.GenerateControllerRevisionName(testName, s.modelServing().Status.UpdateRevision)
			if err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
			s := newSystem(t, modelServing(two))
			s.settle()
			tt.lose(t, s)

			o := s.outage()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](1)
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
				setImage("model:v2")(ms)
			})
			o.settle()
			if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-] 2[serve=2/-]"; got != want {
				t.Fatalf("groups during the outage: %s, want %s", got, want)
			}
			if got := s.deleted(); len(got) != 0 {
				t.Errorf("deleted %v", got)
			}

			// A round with a target carries out both decreases.
			o.end()
			s.settle()
			if got, want := s.handedDown(), "0[serve=1/-]"; got != want {
				t.Fatalf("groups after the outage: %s, want %s", got, want)
			}
		})
	}
}

// Whether workloads of an earlier version are still being taken over is
// decided, and recorded, by a round that has a target. Until one has said
// that the take-over is done, a round without a target scales nothing: the
// groups do not tell yet what exists. The same holds while a hand-back that
// was abandoned is not taken over again.
func TestNothingIsScaledInWhileWorkloadsAreTakenOver(t *testing.T) {
	for _, state := range []string{"", workloadv1alpha1.ServingGroupAdoptionInProgress, workloadv1alpha1.ServingGroupAdoptionRollingBack} {
		t.Run("adoption state "+state, func(t *testing.T) {
			two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
			s := newSystem(t, modelServing(two))
			s.settle()

			o := s.outage()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				delete(ms.Annotations, workloadv1alpha1.ServingGroupAdoptionAnnotationKey)
				if state != "" {
					ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] = state
				}
				ms.Spec.Replicas = ptr.To[int32](1)
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
				setImage("model:v2")(ms)
			})
			o.settle()
			if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-] 2[serve=2/-]"; got != want {
				t.Fatalf("groups during the outage: %s, want %s", got, want)
			}
			if got := s.deleted(); len(got) != 0 {
				t.Errorf("deleted %v", got)
			}

			o.end()
			s.settle()
			if got, want := s.summary(), "groups[0=model:v2] replicas=1 updated=1 current=1 available=1 Available"; got != want {
				t.Fatalf("after the outage:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// As in any round, nothing is deleted or changed on the word of the cache:
// the ModelServing and its groups are read from the API server, and the round
// is decided again on what was read. Here the cache still shows counts that
// were taken back.
func TestAStaleCacheDoesNotScaleInWithoutATarget(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	s := newSystem(t, modelServing(two))
	s.settle()

	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
		setImage("model:v2")(ms)
	})
	stale, snapshot := s.caches()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](3)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
	})
	s.cluster.ResetWrites()

	s.ms.SetCaches(stale, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); !errors.Is(err, errRevisionsUnavailable) {
		t.Fatalf("round on a cache that was behind: %v; want the revision error", err)
	}
	if writes := s.cluster.Writes(); len(writes) != 0 {
		t.Errorf("a round on a cache that was behind issued: %v", writes)
	}
	if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-] 2[serve=2/-]"; got != want {
		t.Errorf("groups = %s, want %s", got, want)
	}

	// The same round on a cache that is up to date has nothing to do either;
	// on the counts the stale cache showed it has.
	o.pass()
	if writes := s.cluster.Writes(); len(writes) != 0 {
		t.Errorf("a round that has nothing to scale in issued: %v", writes)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) })
	o.pass()
	if got, want := s.deleted(), []string{"llama-2", "llama-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

// That the revision cannot be recorded is reported on the ModelServing, with
// the error, once for a generation: not in every round that finds it so, and
// again when the ModelServing was changed and it still cannot be.
func TestARevisionThatCannotBeRecordedIsReportedOnceForAGeneration(t *testing.T) {
	s := newSystem(t, modelServing())
	recorder := s.withEvents()
	s.settle()
	if got := eventsOf(recorder, ReasonRevisionUnresolved); len(got) != 0 {
		t.Fatalf("reported without an outage: %v", got)
	}

	o := s.outage()
	s.update(setImage("model:v2"))
	for range 3 {
		o.pass()
	}
	stale, snapshot := s.caches()
	got := eventsOf(recorder, ReasonRevisionUnresolved)
	if len(got) != 1 || !strings.HasPrefix(got[0], corev1.EventTypeWarning+" ") || !strings.Contains(got[0], errRevisionsUnavailable.Error()) {
		t.Fatalf("events of three rounds for one generation: %q, want one warning that names the error", got)
	}

	s.update(setImage("model:v3"))
	for range 3 {
		o.pass()
	}
	if got := eventsOf(recorder, ReasonRevisionUnresolved); len(got) != 1 {
		t.Fatalf("events of three rounds for the next generation: %q, want one", got)
	}
	// A cache that is behind shows the generation that was reported first.
	s.ms.SetCaches(stale, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); !errors.Is(err, errRevisionsUnavailable) {
		t.Fatalf("round on a cache that was behind: %v; want the revision error", err)
	}
	if got := eventsOf(recorder, ReasonRevisionUnresolved); len(got) != 0 {
		t.Fatalf("a generation that was reported before is reported again: %q", got)
	}

	o.end()
	s.settle()
	if got := eventsOf(recorder, ReasonRevisionUnresolved); len(got) != 0 {
		t.Errorf("reported after the outage: %q", got)
	}
	if got, want := s.summary(), "groups[0=model:v3 1=model:v3 2=model:v3] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the outage:\n got %s\nwant %s", got, want)
	}
}

// A change that lowers the number of groups and the count of a Role is
// carried out in one round: the groups above the number are deleted, and the
// group that stays is told the lower count. A group that is deleted, in this
// round or an earlier one, is not written to.
func TestGroupsAndRoleCountsGoDownTogether(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	s := newSystem(t, modelServing(two))
	s.settle()

	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
		setImage("model:v2")(ms)
	})
	// The first round fails with the revision error and with nothing else.
	o.round()
	if strings.Contains(o.failure.Error(), "ServingGroup") {
		t.Errorf("the round failed at a group: %v", o.failure)
	}
	// The groups it deleted still exist, until their Pods are gone.
	if got, want := s.images(), "0=model:v1 1=model:v1! 2=model:v1!"; got != want {
		t.Fatalf("after the first round: %s, want %s", got, want)
	}
	o.pass()
	if got, want := s.deleted(), []string{"llama-2", "llama-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
	if got, want := s.groupUpdates(), map[string]int{"llama-0": 1}; !reflect.DeepEqual(got, want) {
		t.Errorf("writes of group specs: %v, want %v", got, want)
	}

	o.settle()
	if got, want := s.instances(), "0[0=model:v1]"; got != want {
		t.Fatalf("instances during the outage: %s, want %s", got, want)
	}
	o.end()
	s.settle()
	if got, want := s.instances(), "0[0=model:v2]"; got != want {
		t.Fatalf("instances after the outage: %s, want %s", got, want)
	}
}

// What fails while the counts are taken down is returned together with the
// revision error. A deletion that fails ends the deletions of the round: the
// ones after it were chosen on the assumption that it succeeded. The counts
// of the Roles are handed down all the same.
func TestAScaleInThatFailsIsReturnedWithTheRevisionError(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	s := newSystem(t, modelServing(two))
	s.settle()

	o := s.outage()
	errRefused := errors.New("injected: ServingGroups cannot be changed")
	var refused []string
	revisions := s.cluster.BeforeWrite
	s.cluster.BeforeWrite = func(write apitestWrite) error {
		if write.Resource == "servinggroups" && write.Subresource == "" {
			refused = append(refused, write.String())
			return errRefused
		}
		return revisions(write)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
		setImage("model:v2")(ms)
	})
	o.round()
	if !errors.Is(o.failure, errRefused) {
		t.Errorf("the round did not return the failure of the scale-in: %v", o.failure)
	}
	for _, part := range []string{"delete ServingGroup llama-2", "lower the Role counts of ServingGroup llama-0"} {
		if !strings.Contains(o.failure.Error(), part) {
			t.Errorf("the error does not say %q: %v", part, o.failure)
		}
	}
	if want := []string{"delete servinggroups default/llama-2", "update servinggroups default/llama-0"}; !reflect.DeepEqual(refused, want) {
		t.Errorf("the round asked for %v, want %v", refused, want)
	}

	s.cluster.BeforeWrite = revisions
	o.settle()
	if got, want := s.instances(), "0[0=model:v1]"; got != want {
		t.Fatalf("instances once groups can be changed: %s, want %s", got, want)
	}
}

// A round reads the ModelServing from the API server before it writes, and
// plans again on what it read. The ModelServing it reads may be a later one
// than the cache shows, whose revision cannot be recorded: the round is then
// without a target as well, and takes the counts down that the later one
// declares. Here the cache still shows a scale-out.
func TestARoundThatLosesItsTargetOnTheDirectReadScalesIn(t *testing.T) {
	s := newSystem(t, modelServing())
	s.settle()
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) })
	stale, snapshot := s.caches()

	o := s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1)
		setImage("model:v2")(ms)
	})
	s.ms.SetCaches(stale, snapshot.Listers().Services)
	_, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
	if !errors.Is(err, errTargetUnknown) || !errors.Is(err, errRevisionsUnavailable) {
		t.Fatalf("round on a cache that was behind: %v; want the revision error", err)
	}
	o.check()
	if got, want := s.deleted(), []string{"llama-2", "llama-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("deleted %v, want %v", got, want)
	}
}

// Only a round without a target is cut down to a scale-in. A plan that fails
// for another reason ends the round as before, with nothing written: here the
// revision exists, and what cannot be written is the count of the Roles that
// a coordinated rollout records with it.
func TestAPlanThatFailsOtherwiseScalesNothingIn(t *testing.T) {
	s := newSystem(t, modelServing(coordinated))
	s.settle()

	s.outage()
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](1)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
	})
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	_, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
	if !errors.Is(err, errRevisionsUnavailable) || errors.Is(err, errTargetUnknown) {
		t.Fatalf("round whose plan cannot record the counts of the Roles: %v; want that error, and not that the target is unknown", err)
	}
	for _, write := range s.cluster.Writes() {
		if write.Resource != "modelservings" || write.Verb != "update" || write.Subresource != "" {
			t.Errorf("the round issued: %s", write)
		}
	}
}

// A group is written with the resourceVersion it was read with, so a group
// that someone else changed in between is not overwritten. The write is
// rejected and the round fails with the conflict, which makes the runner plan
// it again; the other groups are written all the same.
func TestALowerCountDoesNotOverwriteAGroupThatChanged(t *testing.T) {
	two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
	s := newSystem(t, modelServing(two))
	s.settle()

	o := s.outage()
	// The first group the round writes is changed between its read and its write.
	resource := workloadv1alpha1.SchemeGroupVersion.WithResource("servinggroups")
	changed := ""
	s.cluster.Kthena.PrependReactor("update", "servinggroups", func(action k8stesting.Action) (bool, runtime.Object, error) {
		update := action.(k8stesting.UpdateAction)
		if changed != "" || update.GetSubresource() != "" {
			return false, nil, nil
		}
		changed = update.GetObject().(*workloadv1alpha1.ServingGroup).Name
		stored, err := s.cluster.Kthena.Tracker().Get(resource, testNamespace, changed)
		if err != nil {
			return true, nil, err
		}
		group := stored.(*workloadv1alpha1.ServingGroup).DeepCopy()
		group.ResourceVersion += "0"
		return false, nil, s.cluster.Kthena.Tracker().Update(resource, group, testNamespace)
	})
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
		setImage("model:v2")(ms)
	})
	o.round()
	if !apierrors.IsConflict(o.failure) {
		t.Fatalf("round that wrote a group that had changed: %v; want a conflict", o.failure)
	}
	if got, want := s.handedDown(), "0[serve=2/-] 1[serve=1/-] 2[serve=1/-]"; changed != "llama-0" || got != want {
		t.Fatalf("after a round in which %s had changed: %s, want %s", changed, got, want)
	}

	o.settle()
	if got, want := s.handedDown(), "0[serve=1/-] 1[serve=1/-] 2[serve=1/-]"; got != want {
		t.Fatalf("handed down during the outage: %s, want %s", got, want)
	}
}

// How many groups may stay follows from the rollout settings. Where they
// cannot be resolved there is no bound to scale in to: nothing is deleted or
// lowered, and the round says what is wrong. An admission webhook rejects such
// settings; the controller does not rely on it.
func TestNothingIsScaledInOnSettingsThatCannotBeResolved(t *testing.T) {
	for name, maxSurge := range map[string]intstr.IntOrString{
		"maxSurge is no number":  intstr.FromString("some"),
		"maxSurge is below zero": intstr.FromInt32(-1),
	} {
		t.Run(name, func(t *testing.T) {
			two := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2) }
			s := newSystem(t, modelServing(two))
			s.settle()

			o := s.outage()
			s.update(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Replicas = ptr.To[int32](1)
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](1)
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxSurge = &maxSurge
				setImage("model:v2")(ms)
			})
			o.round()
			if !strings.Contains(o.failure.Error(), "maxSurge") {
				t.Errorf("the round does not say that maxSurge cannot be resolved: %v", o.failure)
			}
			if got := s.deleted(); len(got) != 0 {
				t.Errorf("deleted %v", got)
			}
			if got, want := s.handedDown(), "0[serve=2/-] 1[serve=2/-] 2[serve=2/-]"; got != want {
				t.Errorf("groups = %s, want %s", got, want)
			}

			s.update(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.MaxSurge = nil }))
			o.settle()
			if got, want := s.handedDown(), "0[serve=1/-]"; got != want {
				t.Fatalf("groups once the settings can be resolved: %s, want %s", got, want)
			}
		})
	}
}
