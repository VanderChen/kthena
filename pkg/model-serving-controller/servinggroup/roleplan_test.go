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
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// fakeRevisions knows the revisions by name: a revision has one worker per
// instance unless workers says otherwise; revisions in the same class render
// the same Pods. The container of a revision runs the image model:<revision>,
// and revisions with different entries in env differ in more than the image.
type fakeRevisions struct {
	workers map[string]int32
	class   map[string]string
	missing map[string]bool
	env     map[string]string
	// absent names, by revision, a Role the revision does not have.
	absent map[string]string
	// unchanged names the Roles that render the same Pods in every revision.
	unchanged map[string]bool
	// image is, by revision, the image the revision renders when it is not
	// the one named after it.
	image map[string]string
}

func (f fakeRevisions) At(revision string) (*workloadv1alpha1.ModelServing, error) {
	role, err := f.Role(revision, "serve")
	if err != nil {
		return nil, err
	}
	image, ok := f.image[revision]
	if !ok {
		image = "model:" + revision
	}
	template := workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		Name: "model", Image: image, Env: []corev1.EnvVar{{Name: "MODE", Value: f.env[revision]}},
	}}}}
	role.EntryTemplate, role.WorkerTemplate = template, template.DeepCopy()
	return &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{
		Template: workloadv1alpha1.ServingGroupTemplate{Roles: []workloadv1alpha1.Role{*role}},
	}}, nil
}

func (f fakeRevisions) Role(revision, roleName string) (*workloadv1alpha1.Role, error) {
	if revision == "" || f.missing[revision] || f.absent[revision] == roleName && roleName != "" {
		return nil, errors.New("unresolved")
	}
	workers, ok := f.workers[revision]
	if !ok {
		workers = 1
	}
	return &workloadv1alpha1.Role{Name: roleName, WorkerReplicas: workers}, nil
}

func (f fakeRevisions) Compare(revision, target string) workload.Comparison {
	return f.CompareRole(revision, target, "")
}

func (f fakeRevisions) CompareRole(revision, target, roleName string) workload.Comparison {
	if revision == "" || target == "" || f.missing[revision] || f.missing[target] {
		return workload.Unknown
	}
	if f.unchanged[roleName] {
		return workload.Equivalent
	}
	classOf := func(revision string) string {
		if class, ok := f.class[revision]; ok {
			return class
		}
		return revision
	}
	if classOf(revision) == classOf(target) {
		return workload.Equivalent
	}
	return workload.Different
}

// planInstance returns an established instance of serve with an entry Pod
// and one worker, both ready, created from revision.
func planInstance(index int, revision string, mutate ...func(*Instance)) Instance {
	inst := Instance{
		Role: "serve", Index: index,
		Entry: &workloadv1alpha1.RoleInstanceStatus{Role: "serve", Index: int32(index), Phase: established},
	}
	for ordinal := range 2 {
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:   fmt.Sprintf("%s-serve-%d-%d", testGroup, index, ordinal),
			Labels: map[string]string{workloadv1alpha1.RevisionLabelKey: revision},
		}}
		inst.Members = append(inst.Members, Member{Pod: pod, Membership: Owned, State: MemberReady, Ordinal: ordinal})
	}
	for _, m := range mutate {
		m(&inst)
	}
	return inst
}

func inPhase(phase workloadv1alpha1.RoleInstancePhase) func(*Instance) {
	return func(inst *Instance) { inst.Entry.Phase = phase }
}

func withoutPods(inst *Instance) { inst.Members = nil }

// workerOf makes the worker Pod of the instance one that was created from revision.
func workerOf(revision string) func(*Instance) {
	return func(inst *Instance) {
		inst.Members[1].Pod.Labels[workloadv1alpha1.RevisionLabelKey] = revision
	}
}

func memberNotReady(inst *Instance) { inst.Members[1].State = MemberNotReady }

// membersTerminating: every Pod of the instance is being deleted, and the
// ledger does not say that the instance is being drained.
func membersTerminating(inst *Instance) {
	now := metav1.Now()
	for i := range inst.Members {
		inst.Members[i].State = MemberTerminating
		inst.Members[i].Pod.DeletionTimestamp = &now
	}
}

func roleSurge(inst *Instance) {
	inst.Members[0].Pod.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeRole}
}

func toAdopt(inst *Instance) { inst.Members[1].Membership = Legacy }

func deletionCostOf(cost string) func(*Instance) {
	return func(inst *Instance) {
		inst.Members[0].Pod.Annotations = map[string]string{corev1.PodDeletionCost: cost}
	}
}

func secondMemberAt(revision string) func(*Instance) {
	return func(inst *Instance) { inst.Members[1].Pod.Labels[workloadv1alpha1.RevisionLabelKey] = revision }
}

// planGroup returns a group with the Role serve. With a Role given, the group
// is rolled Role by Role from rev-a to rev-b.
func planGroup(replicas int32, role *workloadv1alpha1.ServingGroupRole) *workloadv1alpha1.ServingGroup {
	group := &workloadv1alpha1.ServingGroup{Spec: workloadv1alpha1.ServingGroupSpec{Revision: "rev-a"}}
	if role == nil {
		group.Spec.Roles = []workloadv1alpha1.ServingGroupRole{{Name: "serve", Replicas: replicas}}
		return group
	}
	role.Name, role.Replicas = "serve", replicas
	group.Spec = workloadv1alpha1.ServingGroupSpec{
		TargetRevision: "rev-b", BaselineRevision: "rev-a",
		RolloutStrategy: &workloadv1alpha1.ServingGroupRolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate},
		Roles:           []workloadv1alpha1.ServingGroupRole{*role},
	}
	return group
}

// wantsOf renders the instances a plan wants as "<index>@<revision>", with
// "+" for surge and "?" for an unknown layout: "0@rev-a 1@rev-b+".
func wantsOf(plan RolePlan) string {
	var keys []InstanceKey
	for key := range plan.Wants {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].Role != keys[b].Role {
			return keys[a].Role < keys[b].Role
		}
		return keys[a].Index < keys[b].Index
	})
	var out []string
	for _, key := range keys {
		want := plan.Wants[key]
		item := fmt.Sprintf("%d@%s", key.Index, want.Revision)
		if key.Role != "serve" {
			item = key.Role + "-" + item
		}
		if want.Surge {
			item += "+"
		}
		if !want.LayoutKnown {
			item += "?"
		}
		out = append(out, item)
	}
	return strings.Join(out, " ")
}

func intOrString(value string) *intstr.IntOrString {
	parsed := intstr.Parse(value)
	return &parsed
}

func TestPlanRolesOfAGroupThatIsReplacedAsAWhole(t *testing.T) {
	tests := []struct {
		name      string
		replicas  int32
		instances []Instance
		revisions fakeRevisions
		want      string
		promote   int
	}{
		{name: "nothing exists: every instance is created from the revision of the group",
			replicas: 2, want: "0@rev-a 1@rev-a"},
		{name: "what exists stays, whatever revision it is",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-old")}, want: "0@rev-a 1@rev-old"},
		{name: "a hole is filled",
			replicas: 3, instances: []Instance{planInstance(0, "rev-a"), planInstance(2, "rev-a")}, want: "0@rev-a 1@rev-a 2@rev-a"},
		{name: "scaling in drops the highest index",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a"), planInstance(2, "rev-a")}, want: "0@rev-a 1@rev-a"},
		{name: "scaling in drops an instance that is not ready first",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a", memberNotReady), planInstance(1, "rev-a"), planInstance(2, "rev-a")}, want: "1@rev-a 2@rev-a"},
		{name: "then the one that costs least to delete",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a", deletionCostOf("5")), planInstance(1, "rev-a", deletionCostOf("-5")), planInstance(2, "rev-a", deletionCostOf("5"))}, want: "0@rev-a 2@rev-a"},
		{name: "an instance drained for recovery is wanted again",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a", inPhase(draining)), planInstance(1, "rev-a")}, want: "0@rev-a 1@rev-a"},
		{name: "an instance drained by scaling in is not",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a"), planInstance(2, "rev-a", inPhase(draining))}, want: "0@rev-a 1@rev-a"},
		{name: "while one drains, its place is not given to a new index",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a", inPhase(draining), withoutPods)}, want: "0@rev-a 1@rev-a"},
		{name: "an entry without Pods is created from the revision of the group",
			replicas: 1, instances: []Instance{planInstance(0, "rev-old", inPhase(creating), withoutPods)}, want: "0@rev-a"},
		{name: "an instance of a Role the group does not declare is not wanted",
			replicas: 1, instances: []Instance{planInstance(0, "rev-a"), {Role: "gone", Index: 0, Members: planInstance(0, "rev-a").Members}}, want: "0@rev-a"},
		{name: "a template that cannot be resolved: the layout is unknown",
			replicas: 1, revisions: fakeRevisions{missing: map[string]bool{"rev-a": true}}, want: "0@rev-a?"},
		{name: "a Role surge mark left behind is removed",
			replicas: 2, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a", roleSurge)}, want: "0@rev-a 1@rev-a", promote: 1},
		{name: "while an instance has a member to adopt, it is neither counted nor chosen",
			replicas: 1, instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a", toAdopt)}, want: "0@rev-a 1@rev-a"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan, err := PlanRoles(planGroup(tt.replicas, nil), tt.instances, tt.revisions)
			if err != nil {
				t.Fatal(err)
			}
			if got := wantsOf(plan); got != tt.want {
				t.Errorf("wants = %q, want %q", got, tt.want)
			}
			if len(plan.Promote) != tt.promote || len(plan.InPlace) != 0 {
				t.Errorf("promote = %v, in place = %v", plan.Promote, plan.InPlace)
			}
		})
	}
}

func TestPlanRolesOfAGroupThatIsRolledRoleByRole(t *testing.T) {
	old3 := func() []Instance {
		return []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a"), planInstance(2, "rev-a")}
	}
	tests := []struct {
		name      string
		role      workloadv1alpha1.ServingGroupRole
		instances []Instance
		revisions fakeRevisions
		want      string
		waiting   bool
	}{
		{name: "at most maxUnavailable instances are replaced at a time, the highest index first",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1")}, instances: old3(), want: "0@rev-a 1@rev-a"},
		{name: "without maxUnavailable a Role is not limited",
			instances: old3(), want: ""},
		{name: "a percentage of the replicas, rounded down",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("67%")}, instances: old3(), want: "0@rev-a"},
		{name: "the instance that is being replaced is created again from the target",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a"), planInstance(2, "rev-a", inPhase(draining))},
			want:      "0@rev-a 1@rev-a 2@rev-b"},
		{name: "the next one waits until the replacement is ready",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-a"), planInstance(1, "rev-a"), planInstance(2, "rev-b", memberNotReady)},
			want:      "0@rev-a 1@rev-a 2@rev-b"},
		{name: "an instance that is not ready is replaced without waiting",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-a", memberNotReady), planInstance(1, "rev-a"), planInstance(2, "rev-a")},
			want:      "1@rev-a 2@rev-a"},
		{name: "instances below the partition stay",
			role: workloadv1alpha1.ServingGroupRole{Partition: intOrString("2")}, instances: old3(), want: "0@rev-a 1@rev-a"},
		{name: "a percentage partition is rounded up",
			role: workloadv1alpha1.ServingGroupRole{Partition: intOrString("34%")}, instances: old3(), want: "0@rev-a 1@rev-a"},
		{name: "a missing instance below the partition is created from the baseline",
			role:      workloadv1alpha1.ServingGroupRole{Partition: intOrString("2")},
			instances: []Instance{planInstance(1, "rev-a"), planInstance(2, "rev-b")}, want: "0@rev-a 1@rev-a 2@rev-b"},
		{name: "the baseline cannot be told: nothing is created below the partition",
			role:      workloadv1alpha1.ServingGroupRole{Partition: intOrString("2")},
			instances: []Instance{planInstance(1, "rev-a"), planInstance(2, "rev-b")}, want: "1@rev-a 2@rev-b", waiting: true},
		{name: "surge: temporary capacity is created first, marked",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("0"), MaxSurge: intOrString("1")},
			instances: old3(), want: "0@rev-a 1@rev-a 2@rev-a 3@rev-b+"},
		{name: "surge: once it is ready an old instance goes",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("0"), MaxSurge: intOrString("1")},
			instances: append(old3(), planInstance(3, "rev-b", roleSurge)), want: "0@rev-a 1@rev-a 3@rev-b+"},
		{name: "surge: the temporary instance goes when nothing is left to replace",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("0"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-b"), planInstance(3, "rev-b", roleSurge)},
			want:      "0@rev-b 1@rev-b 2@rev-b"},
		// The temporary instance stands in for a place until three other
		// instances have a Pod, whether it is ready or not.
		{name: "surge: the temporary instance stays while the last old instance is being drained",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-a", inPhase(draining)),
				planInstance(3, "rev-b", roleSurge, memberNotReady)},
			want: "0@rev-b 1@rev-b 2@rev-b 3@rev-b+"},
		{name: "surge: a ready one stays as well",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-a", inPhase(draining)),
				planInstance(3, "rev-b", roleSurge)},
			want: "0@rev-b 1@rev-b 2@rev-b 3@rev-b+"},
		{name: "surge: it stays while the replacement is in the ledger and has no Pod",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-b", inPhase(creating), withoutPods),
				planInstance(3, "rev-b", roleSurge, memberNotReady)},
			want: "0@rev-b 1@rev-b 2@rev-b 3@rev-b+"},
		{name: "surge: it stays while every Pod of the replacement is terminating",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-b", membersTerminating),
				planInstance(3, "rev-b", roleSurge, memberNotReady)},
			want: "0@rev-b 1@rev-b 2@rev-b 3@rev-b+"},
		{name: "surge: it goes once the replacement has a Pod, ready or not",
			role: workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-b", memberNotReady),
				planInstance(3, "rev-b", roleSurge, memberNotReady)},
			want: "0@rev-b 1@rev-b 2@rev-b"},
		{name: "surge: with maxSurge lowered to 0 there is no room for the missing place until the temporary instance has gone",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), MaxSurge: intOrString("0")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(3, "rev-b", roleSurge)},
			want:      "0@rev-b 1@rev-b"},
		{name: "a revision that renders the same Pods as the target is not replaced",
			instances: old3(), revisions: fakeRevisions{class: map[string]string{"rev-a": "same", "rev-b": "same"}}, want: "0@rev-a 1@rev-a 2@rev-a"},
		{name: "a revision that cannot be resolved is not replaced",
			instances: old3(), revisions: fakeRevisions{missing: map[string]bool{"rev-a": true}}, want: "0@rev-a? 1@rev-a? 2@rev-a?"},
		{name: "an instance with a member still at the old revision is not at the target",
			role:      workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b"), planInstance(1, "rev-b"), planInstance(2, "rev-b", secondMemberAt("rev-a"))},
			want:      "0@rev-b 1@rev-b"},
		{name: "an entry without Pods gets the revision of its place",
			role:      workloadv1alpha1.ServingGroupRole{Partition: intOrString("1")},
			instances: []Instance{planInstance(0, "rev-b", inPhase(creating), withoutPods), planInstance(1, "rev-a", inPhase(creating), withoutPods), planInstance(2, "rev-b")},
			want:      "0@rev-a 1@rev-b 2@rev-b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := tt.role
			group := planGroup(3, &role)
			if tt.waiting {
				group.Spec.BaselineRevision = ""
			}
			plan, err := PlanRoles(group, tt.instances, tt.revisions)
			if err != nil {
				t.Fatal(err)
			}
			if got := wantsOf(plan); got != tt.want {
				t.Errorf("wants = %q, want %q", got, tt.want)
			}
			if (len(plan.Waiting) > 0) != tt.waiting {
				t.Errorf("waiting = %v, want waiting = %t", plan.Waiting, tt.waiting)
			}
		})
	}
}

// A group that was born in the rollout that is under way says which revision
// it was created from, and that revision is its target. The partition of a
// Role holds the instances below it at the baseline in a group that ran the
// baseline; this group never did, and creates every instance from the target.
//
// It is taken at its word only while every Pod it has is of the target. A
// group that says the target and has a Pod of another revision was not
// created from the target, whatever it says: it is planned like any group.
func TestPlanRolesOfAGroupThatWasCreatedAtTheTarget(t *testing.T) {
	tests := []struct {
		name string
		// created is the revision the group says it was created from, "" when
		// it does not say; target is the one it is rolled to. The baseline is rev-a.
		created, target string
		noBaseline      bool
		revisions       fakeRevisions
		instances       []Instance
		want            string
	}{
		{name: "nothing exists: every instance is created from the target, the one below the partition included",
			created: "rev-b", target: "rev-b", want: "0@rev-b 1@rev-b 2@rev-b"},
		{name: "a missing instance below the partition is created from the target",
			created: "rev-b", target: "rev-b",
			instances: []Instance{planInstance(1, "rev-b"), planInstance(2, "rev-b")}, want: "0@rev-b 1@rev-b 2@rev-b"},
		{name: "an entry without Pods below the partition gets the target",
			created: "rev-b", target: "rev-b",
			instances: []Instance{planInstance(0, "rev-b", inPhase(creating), withoutPods), planInstance(1, "rev-b"), planInstance(2, "rev-b")},
			want:      "0@rev-b 1@rev-b 2@rev-b"},
		{name: "the baseline cannot be told: nothing of the group needs it",
			created: "rev-b", target: "rev-b", noBaseline: true,
			instances: []Instance{planInstance(1, "rev-b"), planInstance(2, "rev-b")}, want: "0@rev-b 1@rev-b 2@rev-b"},
		{name: "the group was told another target and does not say where it comes from: the baseline",
			target:    "rev-c",
			instances: []Instance{planInstance(1, "rev-c"), planInstance(2, "rev-c")}, want: "0@rev-a 1@rev-c 2@rev-c"},
		{name: "an annotation that names another revision than the target counts for nothing: the baseline",
			created: "rev-b", target: "rev-c",
			instances: []Instance{planInstance(1, "rev-c"), planInstance(2, "rev-c")}, want: "0@rev-a 1@rev-c 2@rev-c"},

		// The group says rev-c, and an instance of it was created from rev-b.
		{name: "the group has an instance of another revision: the missing instance below the partition is of the baseline",
			created: "rev-c", target: "rev-c",
			instances: []Instance{planInstance(1, "rev-b"), planInstance(2, "rev-c")}, want: "0@rev-a 1@rev-b 2@rev-c"},
		{name: "one Pod of another revision is enough",
			created: "rev-c", target: "rev-c",
			instances: []Instance{planInstance(1, "rev-c", workerOf("rev-b")), planInstance(2, "rev-c")}, want: "0@rev-a 1@rev-c 2@rev-c"},
		{name: "the other revision renders the same Pods for the Role: the group is taken at its word",
			created: "rev-c", target: "rev-c", revisions: fakeRevisions{class: map[string]string{"rev-b": "same", "rev-c": "same"}},
			instances: []Instance{planInstance(1, "rev-b"), planInstance(2, "rev-c")}, want: "0@rev-c 1@rev-b 2@rev-c"},
		{name: "the other revision cannot be read: the Pod is not known to be of the target, the baseline",
			created: "rev-c", target: "rev-c", revisions: fakeRevisions{missing: map[string]bool{"rev-b": true}},
			instances: []Instance{planInstance(1, "rev-b"), planInstance(2, "rev-c")}, want: "0@rev-a 1@rev-b? 2@rev-c"},
		{name: "the target cannot be read: the Pods of the group carry the target itself, which needs no comparison",
			created: "rev-c", target: "rev-c", revisions: fakeRevisions{missing: map[string]bool{"rev-c": true}},
			instances: []Instance{planInstance(1, "rev-c"), planInstance(2, "rev-c")}, want: "0@rev-c? 1@rev-c? 2@rev-c?"},
		{name: "the Pods of the other revision are terminating: the group does not hold them, and is taken at its word",
			created: "rev-c", target: "rev-c",
			instances: []Instance{planInstance(1, "rev-b", membersTerminating), planInstance(2, "rev-c")}, want: "0@rev-c 1@rev-b 2@rev-c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			group := planGroup(3, &workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("1"), Partition: intOrString("1")})
			if tt.created != "" {
				group.Annotations = map[string]string{workloadv1alpha1.CreatedRevisionAnnotationKey: tt.created}
			}
			group.Spec.TargetRevision = tt.target
			if tt.noBaseline {
				group.Spec.BaselineRevision = ""
			}
			plan, err := PlanRoles(group, tt.instances, tt.revisions)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Waiting) > 0 {
				t.Errorf("waiting = %v, want nothing to wait for", plan.Waiting)
			}
			if got := wantsOf(plan); got != tt.want {
				t.Errorf("wants = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPlanRolesLayoutAndErrors(t *testing.T) {
	// The layout of an instance is the one of the revision it was created from.
	revisions := fakeRevisions{workers: map[string]int32{"rev-a": 1, "rev-b": 3}}
	role := workloadv1alpha1.ServingGroupRole{Partition: intOrString("1")}
	plan, err := PlanRoles(planGroup(2, &role), []Instance{planInstance(0, "rev-a")}, revisions)
	if err != nil {
		t.Fatal(err)
	}
	if kept, created := plan.Wants[InstanceKey{Role: "serve", Index: 0}], plan.Wants[InstanceKey{Role: "serve", Index: 1}]; kept == nil || created == nil || kept.Workers != 1 || created.Workers != 3 {
		t.Errorf("layouts = %+v, %+v; want one worker for the instance of rev-a and three for the one of rev-b", kept, created)
	}

	bad := workloadv1alpha1.ServingGroupRole{MaxUnavailable: intOrString("many")}
	if _, err := PlanRoles(planGroup(2, &bad), nil, fakeRevisions{}); err == nil || !strings.Contains(err.Error(), "maxUnavailable of Role serve") {
		t.Errorf("PlanRoles() error = %v, want the setting that cannot be resolved", err)
	}
}
