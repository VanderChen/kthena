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
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// coordinated turns ms into two groups whose Roles prefill and decode are
// rolled together: two instances each, of an entry Pod and one worker, at most
// one of them unavailable, and prefill depends on decode.
func coordinated(ms *workloadv1alpha1.ModelServing) {
	ms.Spec.Replicas = ptr.To[int32](2)
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.RoleRollingUpdate,
		RoleCoordination: &workloadv1alpha1.RoleCoordination{
			MaxSkew:      ptr.To(intstr.FromString("100%")),
			Dependencies: []workloadv1alpha1.RoleRolloutDependency{{Role: "prefill", DependsOn: []string{"decode"}}},
		},
	}
	role := func(name string) workloadv1alpha1.Role {
		return workloadv1alpha1.Role{
			Name: name, Replicas: ptr.To[int32](2), WorkerReplicas: 1,
			EntryTemplate: podTemplate("model:v1"), WorkerTemplate: ptr.To(podTemplate("model:v1")),
			RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromInt32(0)),
			},
		}
	}
	ms.Spec.Template.Roles = []workloadv1alpha1.Role{role("prefill"), role("decode")}
}

// setImages changes the image of every Role.
func setImages(image string) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		for i := range ms.Spec.Template.Roles {
			role := &ms.Spec.Template.Roles[i]
			role.EntryTemplate = podTemplate(image)
			role.WorkerTemplate = ptr.To(podTemplate(image))
		}
	}
}

// coordination renders the CoordinatedRoleRolloutBlocked condition of the
// ModelServing: "<status>/<reason>", followed by the message when it is true.
func (s *system) coordination() string {
	s.t.Helper()
	ms := s.modelServing()
	condition := meta.FindStatusCondition(ms.Status.Conditions, string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked))
	switch {
	case condition == nil:
		return ""
	case condition.ObservedGeneration != ms.Generation:
		return fmt.Sprintf("observed generation %d of %d", condition.ObservedGeneration, ms.Generation)
	case condition.Status == metav1.ConditionTrue:
		return fmt.Sprintf("True/%s: %s", condition.Reason, condition.Message)
	}
	return "False/" + condition.Reason
}

// recordedReplicas returns the replica counts recorded with a revision.
func (s *system) recordedReplicas(revision string) string {
	s.t.Helper()
	cr, err := s.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Get(s.t.Context(), utils.GenerateControllerRevisionName(testName, revision), metav1.GetOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	return cr.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation]
}

// handedDown renders, for every group, the replica counts of its Roles with
// the ones of the baseline: "0[prefill=3/2 decode=2/2] 1[...]".
func (s *system) handedDown() string {
	s.t.Helper()
	var out []string
	for _, group := range s.groups() {
		var roles []string
		for _, role := range group.Spec.Roles {
			baseline := "-"
			if role.BaselineReplicas != nil {
				baseline = fmt.Sprint(*role.BaselineReplicas)
			}
			roles = append(roles, fmt.Sprintf("%s=%d/%s", role.Name, role.Replicas, baseline))
		}
		out = append(out, strings.TrimPrefix(group.Name, testName+"-")+"["+strings.Join(roles, " ")+"]")
	}
	return strings.Join(out, " ")
}

// A coordinated rollout runs in every group; the ModelServing controller
// tells the groups the replica counts the rollout started from, and reports
// what the groups say about it.
func TestCoordinatedRoleRollout(t *testing.T) {
	s := newSystem(t, modelServing(coordinated))
	recorder := s.withEvents()
	s.settle()
	before := s.modelServing().Status.UpdateRevision
	if got := s.recordedReplicas(before); got != `{"decode":2,"prefill":2}` {
		t.Errorf("replica counts recorded with the first revision = %s", got)
	}
	if got, want := s.handedDown(), "0[prefill=2/2 decode=2/2] 1[prefill=2/2 decode=2/2]"; got != want {
		t.Errorf("handed down: %s, want %s", got, want)
	}
	if got := s.coordination(); got != "False/RolloutComplete" {
		t.Errorf("condition before the rollout = %q", got)
	}
	eventsOf(recorder, "")

	// The template of both Roles changes and prefill gets a third instance.
	// The new decode Pods do not become ready.
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		setImages("model:v2")(ms)
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
	})
	s.stuck = func(pod *corev1.Pod) bool {
		return pod.Labels[workloadv1alpha1.RoleLabelKey] == "decode" && pod.Spec.Containers[0].Image == "model:v2"
	}
	// The round that hands the change down does not say that the rollout is
	// complete: the groups have not said what they do with it.
	s.pass()
	if got := s.coordination(); got != "False/ProgressAvailable" {
		t.Errorf("condition of the round that hands the change down = %q", got)
	}
	s.settle()
	target := s.modelServing().Status.UpdateRevision
	waiting := "[decode-0=model:v1 decode-1=model:v2 prefill-0=model:v1 prefill-1=model:v1]"
	if got, want := s.instances(), "0"+waiting+" 1"+waiting; got != want {
		t.Fatalf("while decode is not ready:\n got %s\nwant %s", got, want)
	}
	blocked := "True/DependencyNotReady: ServingGroup llama-0: root Role prefill is waiting for target-version RoleRunning capacity from Roles decode"
	if got := s.coordination(); got != blocked {
		t.Errorf("condition while decode is not ready:\n got %s\nwant %s", got, blocked)
	}
	if got, want := s.handedDown(), "0[prefill=3/2 decode=2/2] 1[prefill=3/2 decode=2/2]"; got != want {
		t.Errorf("handed down: %s, want %s", got, want)
	}
	if got, kept := s.recordedReplicas(target), s.recordedReplicas(before); got != `{"decode":2,"prefill":3}` || kept != `{"decode":2,"prefill":2}` {
		t.Errorf("replica counts recorded with the target = %s, with the baseline = %s", got, kept)
	}
	if status := s.modelServing().Status; status.CurrentRevision != before || target == before {
		t.Errorf("revisions = %s, %s; started from %s", status.CurrentRevision, status.UpdateRevision, before)
	}
	events := eventsOf(recorder, "DependencyNotReady")
	if len(events) != 1 || !strings.HasPrefix(events[0], "Warning DependencyNotReady ServingGroup llama-0: root Role prefill is waiting") {
		t.Errorf("events = %q, want one warning", events)
	}
	for range 3 {
		s.pass()
	}
	if events := eventsOf(recorder, ""); len(events) != 0 {
		t.Errorf("events of rounds that changed nothing: %q", events)
	}
	// A change of the ModelServing that rolls nothing is reported with the
	// same reason, for the new generation.
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RevisionHistoryLimit = ptr.To[int32](20) })
	s.settle()
	if got, instances := s.coordination(), s.instances(); got != blocked || instances != "0"+waiting+" 1"+waiting {
		t.Errorf("after a change that rolls nothing: condition %s, instances %s", got, instances)
	}
	if events := eventsOf(recorder, "DependencyNotReady"); len(events) != 1 {
		t.Errorf("events for the new generation = %q, want one warning", events)
	}

	s.stuck = nil
	s.settle()
	done := "[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v2 prefill-1=model:v2 prefill-2=model:v2]"
	if got, want := s.instances(), "0"+done+" 1"+done; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	if got := s.coordination(); got != "False/RolloutComplete" {
		t.Errorf("condition after the rollout = %q", got)
	}
	if got, want := s.handedDown(), "0[prefill=3/3 decode=2/2] 1[prefill=3/3 decode=2/2]"; got != want {
		t.Errorf("handed down after the rollout: %s, want %s", got, want)
	}
	if status := s.modelServing().Status; status.CurrentRevision != target || status.UpdateRevision != target {
		t.Errorf("revisions after the rollout = %s, %s; want %s", status.CurrentRevision, status.UpdateRevision, target)
	}
	if got := s.counts(); got != "replicas=2 updated=2 current=2 available=2" {
		t.Errorf("counts after the rollout: %s", got)
	}
	if events := eventsOf(recorder, "RolloutComplete"); len(events) != 1 || events[0] != "Normal RolloutComplete Coordinated Role rollout is complete" {
		t.Errorf("events = %q, want one that the rollout is complete", events)
	}

	// Scaling does not make a new revision: the counts recorded with the
	// revision follow, and what the revision renders stays as it is.
	recorded, err := s.cluster.GetControllerRevision(testNamespace, utils.GenerateControllerRevisionName(testName, target))
	if err != nil {
		t.Fatal(err)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[1].Replicas = ptr.To[int32](3) })
	s.settle()
	scaled, err := s.cluster.GetControllerRevision(testNamespace, utils.GenerateControllerRevisionName(testName, target))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.recordedReplicas(target); got != `{"decode":3,"prefill":3}` || s.modelServing().Status.UpdateRevision != target ||
		string(scaled.Data.Raw) != string(recorded.Data.Raw) || scaled.Revision != recorded.Revision {
		t.Errorf("after scaling: recorded %s with revision %s, data changed = %t", got, s.modelServing().Status.UpdateRevision, string(scaled.Data.Raw) != string(recorded.Data.Raw))
	}
	if got, want := s.handedDown(), "0[prefill=3/3 decode=3/3] 1[prefill=3/3 decode=3/3]"; got != want {
		t.Errorf("handed down after scaling: %s, want %s", got, want)
	}
	if got := s.coordination(); got != "False/RolloutComplete" {
		t.Errorf("condition after scaling = %q", got)
	}
}

func TestCoordinationConditionOfTheModelServing(t *testing.T) {
	conditionType := string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked)
	// group returns a group at generation 3 that reported on that generation.
	group := func(ordinal int, reason, message string, mutate ...func(*groupState)) groupState {
		object := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{Name: fmt.Sprintf("llama-%d", ordinal), Generation: 3}}
		object.Status.ObservedGeneration = 3
		if reason != "" {
			status := metav1.ConditionTrue
			if reason == "ProgressAvailable" || reason == "RolloutComplete" {
				status = metav1.ConditionFalse
			}
			object.Status.Conditions = []metav1.Condition{{Type: conditionType, Status: status, Reason: reason, Message: message, ObservedGeneration: 3}}
		}
		g := groupState{object: object, ordinal: ordinal, unit: grouplevel.Group{Ordinal: ordinal}}
		for _, m := range mutate {
			m(&g)
		}
		return g
	}
	terminating := func(g *groupState) { g.unit.Terminating = true }
	respecified := func(g *groupState) { g.object.Generation = 4 }
	neverReported := func(g *groupState) { g.object.Status.ObservedGeneration = 0 }
	notHandedDown := func(g *groupState) { g.object.Labels = map[string]string{"spec": "behind"} }
	long := strings.Repeat("é", 600)

	tests := []struct {
		name          string
		independent   bool
		carried       *metav1.Condition
		groups        []groupState
		want, message string
	}{
		{name: "Roles rolled independently: no condition", independent: true},
		{name: "Roles rolled independently, a condition from before: complete", independent: true,
			carried: &metav1.Condition{Type: string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked), Status: metav1.ConditionTrue, Reason: "MaxSkewLimitReached"},
			groups:  []groupState{group(0, "MaxSkewLimitReached", "m")},
			want:    "False/RolloutComplete", message: "Coordinated Role rollout is complete"},
		{name: "no group", want: "False/RolloutComplete", message: "Coordinated Role rollout is complete"},
		{name: "every group is complete", groups: []groupState{group(0, "RolloutComplete", ""), group(1, "RolloutComplete", "")},
			want: "False/RolloutComplete", message: "Coordinated Role rollout is complete"},
		{name: "a group can make progress", groups: []groupState{group(0, "RolloutComplete", ""), group(1, "ProgressAvailable", "")},
			want: "False/ProgressAvailable", message: "Coordinated Role rollout can make progress"},
		{name: "the blocked group with the lowest ordinal is reported",
			groups: []groupState{group(0, "ProgressAvailable", ""), group(1, "MaxSkewLimitReached", "skew"), group(2, "DependencyNotReady", "dependency")},
			want:   "True/MaxSkewLimitReached", message: "ServingGroup llama-1: skew"},
		{name: "a blocked group is reported whatever a later group says",
			groups: []groupState{group(0, "DependencyNotReady", "dependency"), group(1, "ProgressAvailable", ""), group(2, "RolloutComplete", "", respecified)},
			want:   "True/DependencyNotReady", message: "ServingGroup llama-0: dependency"},
		{name: "a terminating group does not count", groups: []groupState{group(0, "DependencyNotReady", "dependency", terminating), group(1, "RolloutComplete", "")},
			want: "False/RolloutComplete", message: "Coordinated Role rollout is complete"},
		{name: "a group that reported on an older spec has not said that it is complete",
			groups: []groupState{group(0, "RolloutComplete", "", respecified)},
			want:   "False/ProgressAvailable", message: "Coordinated Role rollout can make progress"},
		{name: "nor that it is blocked", groups: []groupState{group(0, "DependencyNotReady", "dependency", respecified)},
			want: "False/ProgressAvailable", message: "Coordinated Role rollout can make progress"},
		{name: "a group whose spec is not the one handed down has not said that it is complete",
			groups: []groupState{group(0, "RolloutComplete", "", notHandedDown)},
			want:   "False/ProgressAvailable", message: "Coordinated Role rollout can make progress"},
		{name: "a group that says nothing about its spec is not complete", groups: []groupState{group(0, "", "")},
			want: "False/ProgressAvailable", message: "Coordinated Role rollout can make progress"},
		{name: "a group that never reported was just created", groups: []groupState{group(0, "RolloutComplete", ""), group(1, "", "", neverReported)},
			want: "False/RolloutComplete", message: "Coordinated Role rollout is complete"},
		{name: "a long message is cut", groups: []groupState{group(0, "DependencyNotReady", long)},
			want: "True/DependencyNotReady", message: ("ServingGroup llama-0: " + long)[:len("ServingGroup llama-0: ")+2*(509-len("ServingGroup llama-0: "))] + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := modelServing(coordinated)
			ms.Generation = 7
			if tt.independent {
				ms.Spec.RolloutStrategy.RoleCoordination = nil
			}
			var status workloadv1alpha1.ModelServingStatus
			if tt.carried != nil {
				status.Conditions = []metav1.Condition{*tt.carried}
			}
			handedDown := func(group *workloadv1alpha1.ServingGroup) bool { return group.Labels["spec"] != "behind" }
			setCoordinationCondition(&status, ms, &view{groups: tt.groups}, handedDown)

			condition := meta.FindStatusCondition(status.Conditions, string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked))
			if tt.want == "" {
				if condition != nil {
					t.Fatalf("condition = %+v, want none", condition)
				}
				return
			}
			if condition == nil {
				t.Fatal("no condition")
			}
			if got := string(condition.Status) + "/" + condition.Reason; got != tt.want || condition.Message != tt.message || condition.ObservedGeneration != 7 {
				t.Errorf("condition = %s %q at generation %d, want %s %q at generation 7", got, condition.Message, condition.ObservedGeneration, tt.want, tt.message)
			}
			if length := len([]rune(condition.Message)); length > 512 {
				t.Errorf("the message has %d characters", length)
			}
		})
	}
}

func TestBaselineReplicasOfARevision(t *testing.T) {
	ms := modelServing(coordinated)
	ms.UID = "ms-uid"
	ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
	annotated := func(value string) func(*appsv1.ControllerRevision) {
		return func(cr *appsv1.ControllerRevision) {
			cr.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation] = value
		}
	}
	tests := []struct {
		name      string
		release11 bool
		mutate    func(*appsv1.ControllerRevision)
		want      map[string]int32
	}{
		{name: "the counts recorded in the annotation", mutate: annotated(`{"prefill":3,"decode":2}`), want: map[string]int32{"prefill": 3, "decode": 2}},
		{name: "no annotation: nothing is known", want: map[string]int32{}},
		{name: "a revision of release 1.1 carries the counts in its data", release11: true, want: map[string]int32{"prefill": 5, "decode": 2}},
		{name: "the annotation of a revision of release 1.1 wins", release11: true,
			mutate: func(cr *appsv1.ControllerRevision) {
				cr.Annotations = map[string]string{utils.CoordinatedRoleReplicaBaselineAnnotation: `{"prefill":1,"decode":1}`}
			}, want: map[string]int32{"prefill": 1, "decode": 1}},
		{name: "a negative count is not a count", mutate: annotated(`{"prefill":-1,"decode":2}`), want: map[string]int32{"decode": 2}},
		{name: "an annotation that cannot be read", mutate: annotated(`{"prefill":`)},
		{name: "a revision of another ModelServing", mutate: func(cr *appsv1.ControllerRevision) {
			annotated(`{"prefill":3,"decode":2}`)(cr)
			cr.OwnerReferences[0].UID = "another"
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := legacyRevision(t, ms, "rev", tt.release11)
			if tt.mutate != nil {
				tt.mutate(cr)
			}
			if got := baselineReplicas(ms, cr); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("baseline replicas = %v, want %v", got, tt.want)
			}
		})
	}
}

// The replica counts are recorded with a revision of the ModelServing only.
func TestReplicaCountsAreNotRecordedWithARevisionOfAnotherOwner(t *testing.T) {
	ms := modelServing(coordinated)
	ms.UID = "ms-uid"
	foreign := legacyRevision(t, ms, "rev", false)
	foreign.OwnerReferences[0].UID = "another"
	s := newSystem(t, ms, foreign)
	if _, err := s.ms.recordBaselineReplicas(t.Context(), ms, foreign); err == nil {
		t.Error("the counts were recorded")
	}
	stored, err := s.cluster.GetControllerRevision(testNamespace, foreign.Name)
	if err != nil || stored.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation] != "" {
		t.Errorf("the revision afterwards: annotations %v, %v", stored.Annotations, err)
	}
}

// An upgrade in the middle of a coordinated rollout: the groups go on from
// where the earlier version stopped. The instances that run the target
// already are not replaced, and the replica counts the earlier version
// recorded with the revisions are the ones the groups are told.
func TestACoordinatedRolloutUnderWayIsTakenOver(t *testing.T) {
	single := func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](1) }
	old := modelServing(coordinated, single)
	old.UID = "ms-uid"
	ms := modelServing(coordinated, single, setImages("model:v2"))
	ms.UID = "ms-uid"
	carried := metav1.Condition{
		Type: string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked), Status: metav1.ConditionTrue,
		Reason: "MaxSkewLimitReached", Message: "ServingGroup llama-0: written by the earlier version",
	}
	ms.Status = workloadv1alpha1.ModelServingStatus{
		Replicas: 1, CurrentReplicas: 1, CurrentRevision: "rev-old", UpdateRevision: "rev-new",
		Conditions: []metav1.Condition{carried},
	}
	recorded := func(cr *appsv1.ControllerRevision) *appsv1.ControllerRevision {
		cr.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation] = `{"decode":2,"prefill":2}`
		return cr
	}
	objects := []runtime.Object{ms, recorded(legacyRevision(t, old, "rev-old", false)), recorded(legacyRevision(t, ms, "rev-new", false))}
	// The earlier version replaced decode-1 and was waiting for it.
	for _, instance := range []struct {
		role            string
		ordinal         int
		revision, image string
	}{
		{"prefill", 0, "rev-old", "model:v1"}, {"prefill", 1, "rev-old", "model:v1"},
		{"decode", 0, "rev-old", "model:v1"}, {"decode", 1, "rev-new", "model:v2"},
	} {
		for _, object := range legacyPods(ms, 0, instance.revision, instance.image, false) {
			pod := object.(*corev1.Pod)
			id := utils.GenerateRoleID(instance.role, instance.ordinal)
			pod.Name = strings.Replace(pod.Name, "serve-0", id, 1)
			pod.Labels[workloadv1alpha1.RoleLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey] = instance.role, id
			objects = append(objects, pod)
		}
	}
	s := newSystem(t, objects...)
	uids := map[string]types.UID{}
	for _, pod := range s.pods() {
		uids[pod.Name] = pod.UID
	}

	// Until the take-over is done nothing is replaced, and what the earlier
	// version said about the rollout stays.
	for range 10 {
		s.kubelet()
		s.cluster.Settle()
		if s.modelServing().Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionDone {
			break
		}
		if got := meta.FindStatusCondition(s.modelServing().Status.Conditions, carried.Type); got == nil || got.Message != carried.Message {
			t.Fatalf("condition during the take-over = %+v", got)
		}
		s.pass()
	}
	for _, pod := range s.pods() {
		if pod.UID != uids[pod.Name] || pod.DeletionTimestamp != nil {
			t.Fatalf("Pod %s was replaced during the take-over", pod.Name)
		}
	}
	if got, want := s.handedDown(), "0[prefill=2/2 decode=2/2]"; got != want {
		t.Fatalf("handed down: %s, want %s", got, want)
	}

	// At most one instance of a Role is away at a time, as before.
	s.invariant = func() {
		ready := map[string]int{}
		for _, pod := range s.pods() {
			if pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod) {
				ready[pod.Labels[workloadv1alpha1.RoleIDKey]]++
			}
		}
		byRole := map[string]int{}
		for id, pods := range ready {
			if pods == 2 {
				byRole[strings.Split(id, "-")[0]]++
			}
		}
		if byRole["prefill"] < 1 || byRole["decode"] < 1 {
			t.Fatalf("ready instances by Role: %v; %s", byRole, s.instances())
		}
	}
	s.settle()
	if got, want := s.instances(), "0[decode-0=model:v2 decode-1=model:v2 prefill-0=model:v2 prefill-1=model:v2]"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	if got := s.coordination(); got != "False/RolloutComplete" {
		t.Errorf("condition after the rollout = %q", got)
	}
	if status := s.modelServing().Status; status.CurrentRevision != "rev-new" || status.UpdateRevision != "rev-new" {
		t.Errorf("revisions = %s, %s", status.CurrentRevision, status.UpdateRevision)
	}
	for _, pod := range s.pods() {
		if strings.Contains(pod.Name, "decode-1") && pod.UID != uids[pod.Name] {
			t.Errorf("Pod %s ran the target already and was replaced", pod.Name)
		}
	}
	var annotation map[string]int32
	if err := json.Unmarshal([]byte(s.recordedReplicas("rev-new")), &annotation); err != nil || !reflect.DeepEqual(annotation, map[string]int32{"prefill": 2, "decode": 2}) {
		t.Errorf("replica counts recorded with the target = %v, %v", annotation, err)
	}
}
