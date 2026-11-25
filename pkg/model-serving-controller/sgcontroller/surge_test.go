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

package sgcontroller

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The temporary capacity of a Role rolling update is a Role instance whose
// Pods carry the Role surge mark. When the Role is given more replicas while
// the instance is there, it takes one of the new places: its Pods lose the
// mark, and they stay. With the mark left on them the instance would still
// be temporary, and be removed when the rollout ends.
func TestARoleSurgeInstanceThatTakesARegularPlaceLosesTheMark(t *testing.T) {
	rolled := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		role := &ms.Spec.Template.Roles[0]
		role.Replicas = ptr.To[int32](2)
		role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
	}
	earlier := testModelServing(rolled)
	ms := testModelServing(rolled, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate = podTemplate("model:v2")
		ms.Spec.Template.Roles[0].WorkerTemplate = ptr.To(podTemplate("model:v2"))
	})
	h := newHarness(t, ms, revisionOf(t, earlier, "rev-a"), revisionOf(t, ms, "rev-b"), groupOf(ms, "rev-a"))
	h.settle()
	// marked lists the Role instances whose Pods carry the Role surge mark,
	// and reports whether the Pods of an instance disagree.
	marked := func() string {
		byInstance := map[string]map[bool]bool{}
		for _, pod := range h.pods() {
			id := pod.Labels[workloadv1alpha1.RoleIDKey]
			if byInstance[id] == nil {
				byInstance[id] = map[bool]bool{}
			}
			byInstance[id][pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole] = true
		}
		var out []string
		for id, marks := range byInstance {
			switch {
			case len(marks) == 2:
				out = append(out, id+"?")
			case marks[true]:
				out = append(out, id)
			}
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	// The rollout begins: an instance of the target is created beside the two.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.TargetRevision = "rev-b" })
	h.mustRound()
	if got := marked(); got != "prefill-2" {
		t.Fatalf("instances with the Role surge mark = %q, want the temporary one", got)
	}
	temporary := map[string]types.UID{}
	for _, pod := range h.pods() {
		if pod.Labels[workloadv1alpha1.RoleIDKey] == "prefill-2" {
			temporary[pod.Name] = pod.UID
		}
	}

	// A third replica: the place is the one the temporary instance stands on.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 3 })
	writes := h.mustRound()
	for name := range temporary {
		if !hasWrite(writes, "patch pods "+name) {
			t.Errorf("writes = %v; want the mark taken off Pod %s", writes, name)
		}
	}
	if got := marked(); strings.Contains(got, "prefill-2") {
		t.Errorf("instances with the Role surge mark = %q; prefill-2 is a regular instance now", got)
	}

	h.settle()
	if got := marked(); got != "" {
		t.Errorf("instances with the Role surge mark after the rollout = %q", got)
	}
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/1 prefill-1:Established/1 prefill-2:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after the rollout: %q", got)
	}
	after := map[string]types.UID{}
	for _, pod := range h.pods() {
		if pod.Spec.Containers[0].Image != "model:v2" {
			t.Errorf("Pod %s runs %s after the rollout", pod.Name, pod.Spec.Containers[0].Image)
		}
		if pod.Labels[workloadv1alpha1.RoleIDKey] == "prefill-2" {
			after[pod.Name] = pod.UID
		}
	}
	if !reflect.DeepEqual(after, temporary) {
		t.Errorf("the Pods of the instance that took a regular place were replaced: %v, were %v", after, temporary)
	}
}
