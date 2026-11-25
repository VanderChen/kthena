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
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// roleRolling turns ms into two groups of three instances of the Role, rolled
// Role by Role.
func roleRolling(mutate ...func(*workloadv1alpha1.Role)) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](2)
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		role := &ms.Spec.Template.Roles[0]
		role.Replicas = ptr.To[int32](3)
		role.MaxUnavailable = ptr.To(intstr.FromInt32(1))
		for _, m := range mutate {
			m(role)
		}
	}
}

// instances lists, per group, the image of every Role instance:
// "0[0=model:v1 1=model:v2] 1[...]". An instance with the Role surge mark is
// followed by "+".
func (s *system) instances() string {
	s.t.Helper()
	type instance struct {
		images map[string]bool
		surge  bool
	}
	byGroup := map[string]map[string]*instance{}
	for _, pod := range s.pods() {
		group, id := pod.Labels[workloadv1alpha1.GroupNameLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey]
		if byGroup[group] == nil {
			byGroup[group] = map[string]*instance{}
		}
		if byGroup[group][id] == nil {
			byGroup[group][id] = &instance{images: map[string]bool{}}
		}
		byGroup[group][id].images[pod.Spec.Containers[0].Image] = true
		byGroup[group][id].surge = byGroup[group][id].surge || pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole
	}
	var out []string
	for _, group := range s.groups() {
		var ids []string
		for id := range byGroup[group.Name] {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		var items []string
		for _, id := range ids {
			inst := byGroup[group.Name][id]
			image := "mixed"
			if len(inst.images) == 1 {
				for only := range inst.images {
					image = only
				}
			}
			item := strings.TrimPrefix(id, "serve-") + "=" + image
			if inst.surge {
				item += "+"
			}
			items = append(items, item)
		}
		out = append(out, strings.TrimPrefix(group.Name, testName+"-")+"["+strings.Join(items, " ")+"]")
	}
	return strings.Join(out, " ")
}

// readyInstances returns the lowest number of ready Role instances any group has.
func (s *system) readyInstances() int {
	s.t.Helper()
	ready := map[string]map[string]int{}
	for _, group := range s.groups() {
		ready[group.Name] = map[string]int{}
	}
	for _, pod := range s.pods() {
		if pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod) && ready[pod.Labels[workloadv1alpha1.GroupNameLabelKey]] != nil {
			ready[pod.Labels[workloadv1alpha1.GroupNameLabelKey]][pod.Labels[workloadv1alpha1.RoleIDKey]]++
		}
	}
	lowest := -1
	for _, instances := range ready {
		count := 0
		for _, pods := range instances {
			if pods == 2 {
				count++
			}
		}
		if lowest < 0 || count < lowest {
			lowest = count
		}
	}
	return lowest
}

// Under RoleRollingUpdate no group is replaced: every group rolls its Role
// instances itself, within the budget of the Role.
func TestRoleRollingUpdate(t *testing.T) {
	const v1 = "0[0=model:v1 1=model:v1 2=model:v1] 1[0=model:v1 1=model:v1 2=model:v1]"
	const v2 = "0[0=model:v2 1=model:v2 2=model:v2] 1[0=model:v2 1=model:v2 2=model:v2]"
	tests := []struct {
		name            string
		role            func(*workloadv1alpha1.Role)
		floor           int
		stop, afterStop string
	}{
		{name: "one unavailable", role: func(*workloadv1alpha1.Role) {}, floor: 2},
		{name: "surge only", floor: 3, role: func(role *workloadv1alpha1.Role) {
			role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
		}},
		{name: "partition", floor: 2,
			role: func(role *workloadv1alpha1.Role) { role.Partition = ptr.To(intstr.FromInt32(2)) },
			stop: "0[0=model:v1 1=model:v1 2=model:v2] 1[0=model:v1 1=model:v1 2=model:v2]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(roleRolling(tt.role)))
			s.settle()
			if got := s.instances(); got != v1 {
				t.Fatalf("created: %s", got)
			}
			if got, want := s.counts()+" "+s.condition(), "replicas=2 updated=2 current=2 available=2 Available"; got != want {
				t.Fatalf("status after creation = %s, want %s", got, want)
			}
			groups := map[string]types.UID{}
			for _, group := range s.groups() {
				groups[group.Name] = group.UID
			}
			baseline := s.modelServing().Status.CurrentRevision

			s.invariant = func() {
				if ready := s.readyInstances(); ready < tt.floor {
					t.Fatalf("a group has %d ready instances, the budget allows no fewer than %d: %s", ready, tt.floor, s.instances())
				}
			}
			s.update(setImage("model:v2"))
			s.settle()

			if tt.stop != "" {
				// At the partition: the baseline stays, and so does the rollout.
				if got := s.instances(); got != tt.stop {
					t.Fatalf("at the partition: %s, want %s", got, tt.stop)
				}
				status := s.modelServing().Status
				if status.CurrentRevision != baseline || status.UpdateRevision == baseline || status.UpdatedReplicas != 0 {
					t.Errorf("status at the partition: %+v", status)
				}
				s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Partition = nil })
				s.settle()
			}
			s.invariant = nil

			if got := s.instances(); got != v2 {
				t.Fatalf("rolled: %s", got)
			}
			if got, want := s.counts()+" "+s.condition(), "replicas=2 updated=2 current=2 available=2 Available"; got != want {
				t.Fatalf("status after the rollout = %s, want %s", got, want)
			}
			if status := s.modelServing().Status; status.CurrentRevision != status.UpdateRevision || status.CurrentRevision == baseline {
				t.Errorf("revisions after the rollout: %s, %s; before %s", status.CurrentRevision, status.UpdateRevision, baseline)
			}
			for _, group := range s.groups() {
				if group.UID != groups[group.Name] {
					t.Errorf("group %s was replaced by a Role rolling update", group.Name)
				}
				if group.Spec.Revision != "" || group.Spec.TargetRevision != s.modelServing().Status.UpdateRevision {
					t.Errorf("group %s: revision %q, targetRevision %q", group.Name, group.Spec.Revision, group.Spec.TargetRevision)
				}
			}
		})
	}
}

// A rollout that changes one Role of several leaves the Pods of the other
// Roles alone, their revision labels included. A group still reports the new
// revision once the changed Role is rolled, so the rollout ends and the
// baseline advances; the next rollout, of another Role, starts from there.
func TestRoleRollingUpdateOfOneRoleOfSeveral(t *testing.T) {
	router := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
			Name: "route", Replicas: ptr.To[int32](1), EntryTemplate: podTemplate("route:v1"),
		})
	}
	s := newSystem(t, modelServing(roleRolling(), router))
	s.settle()
	const done = "replicas=2 updated=2 current=2 available=2 Available"
	if got := s.counts() + " " + s.condition(); got != done {
		t.Fatalf("status after creation = %s, want %s", got, done)
	}
	// uids returns the UID and the revision label of every Pod of a Role.
	uids := func(role string) map[string]string {
		out := map[string]string{}
		for _, pod := range s.pods() {
			if pod.Labels[workloadv1alpha1.RoleLabelKey] == role {
				out[pod.Name] = string(pod.UID) + " " + pod.Labels[workloadv1alpha1.RevisionLabelKey]
			}
		}
		return out
	}
	rolled := func(step, untouched string, before map[string]string, baseline string) string {
		t.Helper()
		if got := s.counts() + " " + s.condition(); got != done {
			t.Fatalf("%s: status = %s, want %s", step, got, done)
		}
		status := s.modelServing().Status
		if status.CurrentRevision != status.UpdateRevision || status.CurrentRevision == baseline {
			t.Fatalf("%s: revisions %s, %s; before %s", step, status.CurrentRevision, status.UpdateRevision, baseline)
		}
		if after := uids(untouched); len(after) == 0 || !reflect.DeepEqual(after, before) {
			t.Errorf("%s: the Pods of %s changed: %v, before %v", step, untouched, after, before)
		}
		for _, group := range s.groups() {
			if group.Status.Revision != status.UpdateRevision {
				t.Errorf("%s: group %s reports revision %q, want %q", step, group.Name, group.Status.Revision, status.UpdateRevision)
			}
		}
		return status.CurrentRevision
	}

	first := s.modelServing().Status.CurrentRevision
	routers := uids("route")
	s.update(setImage("model:v2"))
	s.settle()
	second := rolled("serve rolled", "route", routers, first)

	servers := uids("serve")
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[1].EntryTemplate = podTemplate("route:v2")
	})
	s.settle()
	rolled("route rolled", "serve", servers, second)
	for name, pod := range uids("route") {
		if routers[name] == pod {
			t.Errorf("Pod %s of route was not replaced", name)
		}
	}
}
