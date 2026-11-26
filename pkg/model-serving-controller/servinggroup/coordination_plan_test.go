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

package servinggroup

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// rolledTogether returns a group that rolls its Roles prefill and decode from
// rev-a to rev-b together: at most maxSkew apart, one instance of a Role
// unavailable at a time and no surge. With dependency, prefill depends on
// decode.
func rolledTogether(prefill, decode int32, maxSkew string, dependency bool, mutate ...func(*workloadv1alpha1.ServingGroup)) *workloadv1alpha1.ServingGroup {
	role := func(name string, replicas int32) workloadv1alpha1.ServingGroupRole {
		return workloadv1alpha1.ServingGroupRole{
			Name: name, Replicas: replicas, BaselineReplicas: ptr.To(replicas),
			MaxUnavailable: intOrString("1"), MaxSurge: intOrString("0"),
		}
	}
	coordination := &workloadv1alpha1.RoleCoordination{MaxSkew: intOrString(maxSkew)}
	if dependency {
		coordination.Dependencies = []workloadv1alpha1.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}}
	}
	group := &workloadv1alpha1.ServingGroup{Spec: workloadv1alpha1.ServingGroupSpec{
		TargetRevision: "rev-b", BaselineRevision: "rev-a",
		RolloutStrategy: &workloadv1alpha1.ServingGroupRolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate, RoleCoordination: coordination},
		Roles:           []workloadv1alpha1.ServingGroupRole{role("prefill", prefill), role("decode", decode)},
	}}
	for _, m := range mutate {
		m(group)
	}
	return group
}

// createdAt makes a group say that it was created new from revision, with the
// lowest instance of both Roles below the partition of its Role.
func createdAt(revision string) func(*workloadv1alpha1.ServingGroup) {
	return func(group *workloadv1alpha1.ServingGroup) {
		group.Annotations = map[string]string{workloadv1alpha1.CreatedRevisionAnnotationKey: revision}
		group.Spec.Roles[0].Partition, group.Spec.Roles[1].Partition = intOrString("1"), intOrString("1")
	}
}

// everyInstanceHeld puts both instances of both Roles below the partition.
func everyInstanceHeld(group *workloadv1alpha1.ServingGroup) {
	group.Spec.Roles[0].Partition, group.Spec.Roles[1].Partition = intOrString("2"), intOrString("2")
}

// instancesOf returns the instances of a Role, one for each word of layout in
// the order of their indices: "a" runs rev-a and "b" rev-b, "!" after it makes
// its worker not ready, "~" makes the instance one that is being drained, with
// its Pods terminating, "+" gives it the Role surge mark, "." is an instance
// that is recorded and has no Pod yet, and "-" leaves the place empty.
func instancesOf(role, layout string) []Instance {
	var instances []Instance
	for index, word := range strings.Fields(layout) {
		if word == "-" {
			continue
		}
		if word == "." {
			instances = append(instances, Instance{Role: role, Index: index, Entry: &workloadv1alpha1.RoleInstanceStatus{Role: role, Index: int32(index)}})
			continue
		}
		mutate := []func(*corev1.Pod){labelled("rev-" + word[:1])}
		if strings.HasSuffix(word, "~") {
			mutate = append(mutate, terminating)
		}
		if strings.HasSuffix(word, "+") {
			mutate = append(mutate, func(pod *corev1.Pod) {
				pod.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeRole}
			})
		}
		worker := mutate
		if strings.HasSuffix(word, "!") {
			worker = append(worker[:1:1], notReady)
		}
		inst := instanceOf(index, memberPod(role, index, 0, mutate...), memberPod(role, index, 1, worker...))
		inst.Role, inst.Entry.Role = role, role
		if strings.HasSuffix(word, "~") {
			inst.Entry.Phase = draining
		}
		instances = append(instances, inst)
	}
	return instances
}

// changesOf renders what a plan changes: the instances it does not want any
// more, and after "+" the ones it wants that do not exist, with the revision
// they are created from.
func changesOf(plan RolePlan, instances []Instance) string {
	existing := sets.New[InstanceKey]()
	var removed, created []string
	for _, inst := range instances {
		existing.Insert(inst.Key())
		if plan.Wants[inst.Key()] == nil {
			removed = append(removed, fmt.Sprintf("%s-%d", inst.Role, inst.Index))
		}
	}
	for key, want := range plan.Wants {
		if !existing.Has(key) {
			created = append(created, fmt.Sprintf("+%s-%d@%s", key.Role, key.Index, want.Revision))
		}
	}
	sort.Strings(removed)
	sort.Strings(created)
	return strings.Join(append(removed, created...), " ")
}

func TestPlanRolesRolledTogether(t *testing.T) {
	sameTemplate := fakeRevisions{class: map[string]string{"rev-a": "same", "rev-b": "same"}}
	tests := []struct {
		name            string
		group           *workloadv1alpha1.ServingGroup
		prefill, decode string
		// other holds the instances of a third Role, other.
		other     string
		revisions fakeRevisions
		// changes is what changesOf renders.
		changes string
		// condition is "<status>/<reason>" of the condition the plan reports.
		// It describes the group as the plan found it: a Role that can still
		// start an instance is not blocked.
		condition string
		// deferred lists the Roles that may not start the target yet.
		deferred []string
		err      string
	}{
		{name: "a Role waits for the Role it depends on: only that one starts",
			group: rolledTogether(2, 2, "100%", true), prefill: "a a", decode: "a a",
			changes: "decode-1", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "target capacity that is not ready admits nothing",
			group: rolledTogether(2, 2, "100%", true), prefill: "a a", decode: "a b!",
			condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "ready target capacity admits the caller; the last old instance of the dependency stays",
			group: rolledTogether(2, 2, "100%", true), prefill: "a a", decode: "a b",
			changes: "prefill-1", condition: "True/OldVersionDependencyPresent"},
		{name: "the last old instance of the dependency stays, whichever it is",
			group: rolledTogether(2, 2, "100%", true), prefill: "a a", decode: "b a",
			changes: "prefill-1", condition: "True/OldVersionDependencyPresent"},
		{name: "the last old instance of the dependency stays while an old caller remains",
			group: rolledTogether(2, 2, "100%", true), prefill: "a b", decode: "a b",
			changes: "prefill-0", condition: "True/OldVersionDependencyPresent"},
		{name: "no old caller is left: the dependency finishes",
			group: rolledTogether(2, 2, "100%", true), prefill: "b b", decode: "a b",
			changes: "decode-0", condition: "False/ProgressAvailable"},
		{name: "a missing instance of a Role that waits is not created at the target",
			group: rolledTogether(2, 2, "100%", true), prefill: "a -", decode: "a a",
			changes: "decode-1", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "added capacity of a Role that waits is not created",
			group:   rolledTogether(2, 2, "100%", true, func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 4 }),
			prefill: "a a", decode: "a a",
			changes: "decode-1", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "added capacity is created once the dependency is ready",
			group:   rolledTogether(2, 2, "100%", true, func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 4 }),
			prefill: "a a", decode: "a b",
			changes: "+prefill-2@rev-b +prefill-3@rev-b", condition: "True/OldVersionDependencyPresent"},
		{name: "added capacity is not limited by maxSkew, and uses up what the old instances may start",
			group: rolledTogether(4, 4, "25%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Replicas, group.Spec.Roles[1].Replicas = 8, 8
			}),
			prefill: "a a a a", decode: "a a a a",
			changes:   "+decode-4@rev-b +decode-5@rev-b +decode-6@rev-b +decode-7@rev-b +prefill-4@rev-b +prefill-5@rev-b +prefill-6@rev-b +prefill-7@rev-b",
			condition: "False/ProgressAvailable"},
		{name: "nor is it with a budget for every instance to be unavailable",
			group: rolledTogether(4, 4, "25%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Replicas, group.Spec.Roles[1].Replicas = 8, 8
				group.Spec.Roles[0].MaxUnavailable, group.Spec.Roles[1].MaxUnavailable = intOrString("8"), intOrString("8")
			}),
			prefill: "a a a a", decode: "a a a a",
			changes:   "+decode-4@rev-b +decode-5@rev-b +decode-6@rev-b +decode-7@rev-b +prefill-4@rev-b +prefill-5@rev-b +prefill-6@rev-b +prefill-7@rev-b",
			condition: "False/ProgressAvailable"},
		{name: "three of eight are ready: five may have started, one old instance follows the four that were added",
			group: rolledTogether(4, 4, "25%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Replicas, group.Spec.Roles[1].Replicas = 8, 8
				group.Spec.Roles[0].MaxUnavailable, group.Spec.Roles[1].MaxUnavailable = intOrString("8"), intOrString("8")
			}),
			prefill: "a a a a b b b b!", decode: "a a a a b b b b!",
			changes: "decode-3 prefill-3", condition: "False/ProgressAvailable"},

		{name: "a place of the baseline that is empty is a started instance, and filling it is not another one",
			group: rolledTogether(4, 4, "50%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].MaxUnavailable = intOrString("2")
			}),
			prefill: "a a - a", decode: "a a a a",
			changes: "decode-3 prefill-3 +prefill-2@rev-b", condition: "False/ProgressAvailable"},
		{name: "maxSkew: rounding up admits one instance of each Role",
			group: rolledTogether(4, 2, "25%", false), prefill: "a a a a", decode: "a a",
			changes: "decode-1 prefill-3", condition: "False/ProgressAvailable"},
		{name: "maxSkew: the slowest Role has nothing ready, nothing more starts",
			group: rolledTogether(4, 2, "25%", false), prefill: "a a a b", decode: "a b!",
			condition: "True/MaxSkewLimitReached"},
		{name: "maxSkew: a quarter and a half are ready, half of each Role may have started",
			group: rolledTogether(4, 2, "25%", false), prefill: "a a a b", decode: "a b",
			changes: "prefill-2", condition: "True/MaxSkewLimitReached"},
		{name: "maxSkew: half of both are ready, three quarters may have started",
			group: rolledTogether(4, 2, "25%", false), prefill: "a a b b", decode: "a b",
			changes: "decode-0 prefill-1", condition: "False/ProgressAvailable"},
		{name: "maxSkew: an instance of the target below the old ones is a started instance",
			group: rolledTogether(10, 10, "10%", false), prefill: "b a a a a a a a a a", decode: "a a a a a a a a a a",
			changes: "decode-9", condition: "True/MaxSkewLimitReached"},
		{name: "an instance that is going is a started instance, not a ready one",
			group: rolledTogether(2, 2, "100%", true), prefill: "a a", decode: "a a~",
			condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "an instance without a Pod below the partition will run the baseline: it is not a started one",
			group: rolledTogether(4, 4, "25%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Partition, group.Spec.Roles[1].Partition = intOrString("1"), intOrString("1")
				group.Spec.Roles[1].MaxUnavailable = intOrString("2")
			}),
			prefill: "a a a a", decode: ". a a a", changes: "decode-3 prefill-3", condition: "False/ProgressAvailable"},
		{name: "an instance with the surge mark within the replica count is a regular one, held back like the others",
			group: rolledTogether(2, 2, "50%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].MaxUnavailable = intOrString("2")
			}),
			prefill: "b! a+", decode: "a a", changes: "decode-1", condition: "True/MaxSkewLimitReached"},
		{name: "every instance runs the target",
			group: rolledTogether(4, 2, "25%", false), prefill: "b b b b", decode: "b b",
			condition: "False/RolloutComplete"},

		// A group that was born in the rollout never ran the baseline: the
		// places below the partitions are filled from the target, and wait for
		// the Roles they depend on like every instance of the target.
		{name: "a group created at the target: the Role that is depended on is created whole from the target, its caller waits",
			group: rolledTogether(2, 2, "100%", true, createdAt("rev-b")), prefill: "- -", decode: "- -",
			changes: "+decode-0@rev-b +decode-1@rev-b", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "a group created at the target: the caller follows, whole from the target",
			group: rolledTogether(2, 2, "100%", true, createdAt("rev-b")), prefill: "- -", decode: "b b",
			changes: "+prefill-0@rev-b +prefill-1@rev-b", condition: "False/ProgressAvailable"},
		{name: "a group created at the target: an instance without a Pod below the partition will run the target, it is a started one and does not wait again",
			group: rolledTogether(1, 1, "100%", true, createdAt("rev-b")), prefill: ".", decode: "b!",
			condition: "False/RolloutComplete"},
		// The open point of a group that was told another target while its Pods
		// were created: it says rev-b, and decode is of rev-c. Taken at its
		// word, prefill would be of the target and wait for decode, which the
		// partition holds at rev-c for good.
		{name: "a group that says the target and has decode of another revision is not taken at its word: prefill is of the baseline and does not wait",
			group: rolledTogether(2, 2, "100%", true, createdAt("rev-b"), everyInstanceHeld), prefill: "- -", decode: "c c",
			changes: "+prefill-0@rev-a +prefill-1@rev-a", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "an annotation that names another revision than the target counts for nothing: the baseline below the partitions, which does not wait",
			group: rolledTogether(2, 2, "100%", true, createdAt("rev-a")), prefill: "- -", decode: "- -",
			changes: "+decode-0@rev-a +decode-1@rev-b +prefill-0@rev-a", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},

		{name: "the partition of a Role holds",
			group: rolledTogether(4, 2, "100%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Partition, group.Spec.Roles[1].Partition = intOrString("2"), intOrString("1")
			}),
			prefill: "a a b b", decode: "a b", condition: "False/RolloutComplete"},
		{name: "partitions that no common share of the Roles gives stop the rollout",
			group: rolledTogether(4, 4, "100%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].Partition, group.Spec.Roles[1].Partition = intOrString("1"), intOrString("3")
			}),
			prefill: "a a a a", decode: "a a a a", condition: "True/IncompatiblePartitions"},

		{name: "a surge instance of the target stays until the place it stands in for is filled",
			group: rolledTogether(1, 1, "100%", false, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.Roles[0].MaxUnavailable, group.Spec.Roles[0].MaxSurge = intOrString("0"), intOrString("1")
			}),
			prefill: "- b+", decode: "b", changes: "+prefill-0@rev-b", condition: "False/ProgressAvailable"},
		{name: "without a surge budget the surge instance goes first",
			group:   rolledTogether(1, 1, "100%", false),
			prefill: "- b+", decode: "b", changes: "prefill-1", condition: "False/ProgressAvailable"},
		{name: "a Role whose template did not change has nothing to roll and holds nothing back",
			group: rolledTogether(2, 2, "100%", true), revisions: fakeRevisions{unchanged: map[string]bool{"prefill": true}},
			prefill: "a a", decode: "a b", changes: "decode-0", condition: "False/ProgressAvailable"},
		{name: "a Role that is not named does not count, whatever state it is in",
			group: rolledTogether(3, 3, "20%", true, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"prefill", "decode"}
				group.Spec.Roles = append(group.Spec.Roles, workloadv1alpha1.ServingGroupRole{Name: "other", Replicas: 3})
			}),
			prefill: "a a a", decode: "a a a", other: "b! b! b!",
			changes: "decode-2", condition: "True/DependencyNotReady", deferred: []string{"prefill"}},
		{name: "a Role that is not named is rolled within its own budget",
			group: rolledTogether(2, 2, "100%", true, func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.RolloutStrategy.RoleCoordination.Roles = []string{"decode"}
				group.Spec.RolloutStrategy.RoleCoordination.Dependencies = nil
			}),
			prefill: "a a", decode: "a a", changes: "decode-1 prefill-1", condition: "False/ProgressAvailable"},
		{name: "the revisions render the same Pods: there is nothing to coordinate, and creation does not wait",
			group: rolledTogether(2, 2, "100%", true), revisions: sameTemplate, prefill: "a -", decode: "a -",
			changes: "+decode-1@rev-b +prefill-1@rev-b", condition: "False/RolloutComplete"},
		{name: "a Role the baseline did not have starts from nothing",
			group:     rolledTogether(2, 2, "100%", false, func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].BaselineReplicas = nil }),
			revisions: fakeRevisions{absent: map[string]string{"rev-a": "prefill"}}, prefill: "- -", decode: "a a",
			changes: "decode-1 +prefill-0@rev-b +prefill-1@rev-b", condition: "False/ProgressAvailable"},

		{name: "the baseline revision cannot be read: nothing is planned",
			group: rolledTogether(2, 2, "100%", true), revisions: fakeRevisions{missing: map[string]bool{"rev-a": true}},
			prefill: "a a", decode: "a a", err: `the baseline revision "rev-a" cannot be compared with the target revision "rev-b"`},
		{name: "the replica count of the baseline is not told: nothing is planned",
			group:   rolledTogether(2, 2, "100%", true, func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[1].BaselineReplicas = nil }),
			prefill: "a a", decode: "a a", err: "the replica count of Role decode at the baseline revision rev-a is not known"},
		{name: "maxSkew cannot be read: nothing is planned",
			group:   rolledTogether(2, 2, "a lot", true),
			prefill: "a a", decode: "a a", err: "coordinated Role rollout:"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			instances := append(instancesOf("prefill", tt.prefill), instancesOf("decode", tt.decode)...)
			instances = append(instances, instancesOf("other", tt.other)...)
			plan, err := PlanRoles(tt.group, instances, tt.revisions)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("error = %v, want one that says %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := changesOf(plan, instances); got != tt.changes {
				t.Errorf("changes = %q, want %q", got, tt.changes)
			}
			condition := plan.Coordination.Condition()
			if got := string(condition.Status) + "/" + condition.Reason; got != tt.condition {
				t.Errorf("condition = %s (%s), want %s", got, condition.Message, tt.condition)
			}
			if got := sets.List(plan.Coordination.Deferred); !sets.New(got...).Equal(sets.New(tt.deferred...)) {
				t.Errorf("Roles that may not start = %v, want %v", got, tt.deferred)
			}
		})
	}
}

// A group whose Roles are rolled independently says nothing about a
// coordinated rollout.
func TestPlanRolesWithoutCoordinationReportsNothing(t *testing.T) {
	for name, mutate := range map[string]func(*workloadv1alpha1.ServingGroup){
		"no roleCoordination": func(group *workloadv1alpha1.ServingGroup) { group.Spec.RolloutStrategy.RoleCoordination = nil },
		"the group is replaced as a whole": func(group *workloadv1alpha1.ServingGroup) {
			group.Spec.RolloutStrategy.Type = workloadv1alpha1.ServingGroupRollingUpdate
			group.Spec.Revision = "rev-a"
		},
	} {
		t.Run(name, func(t *testing.T) {
			instances := append(instancesOf("prefill", "a a"), instancesOf("decode", "a a")...)
			plan, err := PlanRoles(rolledTogether(2, 2, "100%", true, mutate), instances, fakeRevisions{})
			if err != nil {
				t.Fatal(err)
			}
			if plan.Coordination != nil || plan.Coordination.Condition() != nil {
				t.Errorf("coordination = %+v, want none", plan.Coordination)
			}
		})
	}
}
