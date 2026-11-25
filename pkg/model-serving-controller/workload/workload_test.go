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

package workload

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func podTemplate(image string) workloadv1alpha1.PodTemplateSpec {
	return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: image}}}}
}

// modelServing returns a ModelServing with two Roles: prefill is an entry Pod
// with two workers, decode an entry Pod alone.
func modelServing(mutate ...func(*workloadv1alpha1.ModelServing)) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "llama", UID: types.UID("ms-uid"), Generation: 7,
			Annotations: map[string]string{schedulingv1beta1.QueueNameAnnotationKey: "gpu"},
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:       ptr.To[int32](3),
			SchedulerName:  "volcano",
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:                    workloadv1alpha1.RoleRollingUpdate,
				UpdateMethod:            workloadv1alpha1.InPlaceUpdateMethod,
				ProgressDeadlineSeconds: ptr.To[int32](600),
				RoleCoordination:        &workloadv1alpha1.RoleCoordination{MaxSkew: ptr.To(intstr.FromString("10%"))},
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt32(2)),
				},
			},
			Template: workloadv1alpha1.ServingGroupTemplate{
				RestartGracePeriodSeconds: ptr.To[int64](30),
				GangPolicy:                &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1}},
				NetworkTopology:           &workloadv1alpha1.NetworkTopology{},
				Roles: []workloadv1alpha1.Role{
					{
						Name: "prefill", Replicas: ptr.To[int32](2), WorkerReplicas: 2,
						EntryTemplate: podTemplate("model:v1"), WorkerTemplate: ptr.To(podTemplate("model:v1")),
						RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
							MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromString("50%")), Partition: ptr.To(intstr.FromInt32(1)),
						},
					},
					{Name: "decode", EntryTemplate: podTemplate("model:v1")},
				},
			},
		},
	}
	for _, m := range mutate {
		m(ms)
	}
	return ms
}

func image(role int, image string) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[role].EntryTemplate = podTemplate(image)
		if ms.Spec.Template.Roles[role].WorkerTemplate != nil {
			ms.Spec.Template.Roles[role].WorkerTemplate = ptr.To(podTemplate(image))
		}
	}
}

func controllerRevision(ms *workloadv1alpha1.ModelServing, revision string, data []byte, annotations map[string]string) *appsv1.ControllerRevision {
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: utils.GenerateControllerRevisionName(ms.Name, revision),
			Labels:      map[string]string{utils.ControllerRevisionRevisionLabelKey: revision},
			Annotations: annotations,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
				Name: ms.Name, UID: ms.UID, Controller: ptr.To(true),
			}},
		},
		Data: runtime.RawExtension{Raw: data},
	}
}

// recorded returns the ControllerRevision the current controller records for ms.
func recorded(t *testing.T, ms *workloadv1alpha1.ModelServing, revision string) *appsv1.ControllerRevision {
	t.Helper()
	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		t.Fatal(err)
	}
	return controllerRevision(ms, revision, data, map[string]string{
		utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1,
	})
}

// recordedByV110 returns the ControllerRevision release 1.1 records for ms: the
// Roles alone, wrapped, without a data version.
func recordedByV110(t *testing.T, ms *workloadv1alpha1.ModelServing, revision string) *appsv1.ControllerRevision {
	t.Helper()
	data, err := json.Marshal(map[string]any{"data": ms.Spec.Template.Roles})
	if err != nil {
		t.Fatal(err)
	}
	return controllerRevision(ms, revision, data, nil)
}

type store struct {
	revisions map[string]*appsv1.ControllerRevision
	reads     int
	err       error
}

func newStore(revisions ...*appsv1.ControllerRevision) *store {
	s := &store{revisions: map[string]*appsv1.ControllerRevision{}}
	for _, cr := range revisions {
		s.revisions[cr.Namespace+"/"+cr.Name] = cr
	}
	return s
}

func (s *store) get(namespace, name string) (*appsv1.ControllerRevision, error) {
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	if cr := s.revisions[namespace+"/"+name]; cr != nil {
		return cr, nil
	}
	return nil, apierrors.NewNotFound(appsv1.Resource("controllerrevisions"), name)
}

func TestGroupSpecAndBaseCarryTheSettingsOfTheGroup(t *testing.T) {
	ms := modelServing()
	settings := Settings{
		TargetRevision: "rev-b", BaselineRevision: "rev-a", ExpectedPods: 7,
		UpdateMethod:     workloadv1alpha1.RecreateUpdateMethod,
		BaselineReplicas: map[string]int32{"prefill": 4},
	}
	spec := GroupSpec(ms, settings)

	want := workloadv1alpha1.ServingGroupSpec{
		TargetRevision: "rev-b", BaselineRevision: "rev-a", SourceGeneration: 7, ExpectedPods: 7,
		RolloutStrategy: &workloadv1alpha1.ServingGroupRolloutStrategy{
			Type:                    workloadv1alpha1.RoleRollingUpdate,
			UpdateMethod:            workloadv1alpha1.RecreateUpdateMethod,
			ProgressDeadlineSeconds: ptr.To[int32](600),
			RoleCoordination:        &workloadv1alpha1.RoleCoordination{MaxSkew: ptr.To(intstr.FromString("10%"))},
		},
		RecoveryPolicy:            workloadv1alpha1.RoleRecreate,
		RestartGracePeriodSeconds: ptr.To[int64](30),
		GangPolicy:                &workloadv1alpha1.GangPolicy{MinRoleReplicas: map[string]int32{"prefill": 1}},
		NetworkTopology:           &workloadv1alpha1.NetworkTopology{},
		QueueName:                 "gpu",
		Roles: []workloadv1alpha1.ServingGroupRole{
			{
				Name: "prefill", Replicas: 2, BaselineReplicas: ptr.To[int32](4),
				MaxUnavailable: ptr.To(intstr.FromInt32(1)), MaxSurge: ptr.To(intstr.FromString("50%")), Partition: ptr.To(intstr.FromInt32(1)),
			},
			{Name: "decode", Replicas: 1},
		},
	}
	if diff := cmp.Diff(want, spec); diff != "" {
		t.Errorf("GroupSpec() (-want +got):\n%s", diff)
	}

	// The ModelServing moved on since the spec of the group was written. The
	// view of the group does not follow it.
	newer := modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Generation = 9
		ms.Annotations = nil
		ms.Spec.RecoveryPolicy = workloadv1alpha1.ServingGroupRecreate
		ms.Spec.RolloutStrategy = nil
		ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](-1)
		ms.Spec.Template.GangPolicy = nil
		ms.Spec.Template.NetworkTopology = nil
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](5)
		ms.Spec.Template.Roles[0].RollingUpdateConfiguration = workloadv1alpha1.RollingUpdateConfiguration{}
		ms.Status.CurrentRevision = "rev-b"
		ms.Status.UpdateRevision = "rev-c"
	})
	group := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "llama-0"}, Spec: spec}
	base := Base(newer, group)

	if base.Name != "llama" || base.UID != "ms-uid" {
		t.Errorf("the view is %s with UID %s, want the identity of the ModelServing", base.Name, base.UID)
	}
	// Read back from the view, the settings are those the group was given.
	readBack := GroupSpec(base, settings)
	readBack.SourceGeneration = spec.SourceGeneration
	if diff := cmp.Diff(spec, readBack); diff != "" {
		t.Errorf("settings changed on the way through the view (-group +view):\n%s", diff)
	}
	if len(base.Spec.Template.Roles[0].EntryTemplate.Spec.Containers) != 0 {
		t.Error("the view has a Pod template before a revision is applied")
	}
	if base.Status.CurrentRevision != "rev-a" || base.Status.UpdateRevision != "rev-b" {
		t.Errorf("revisions of the view = %s, %s, want those the group was given", base.Status.CurrentRevision, base.Status.UpdateRevision)
	}
	if newer.Spec.RecoveryPolicy != workloadv1alpha1.ServingGroupRecreate || len(newer.Annotations) != 0 {
		t.Error("Base modified the ModelServing it was given")
	}
}

func TestAtAppliesARevisionToTheSettingsOfTheGroup(t *testing.T) {
	ms := modelServing()
	old := modelServing(image(0, "model:v0"), func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.SchedulerName = "default-scheduler"
		ms.Spec.Template.Roles[0].WorkerReplicas = 1
		// What a revision records about replicas is not used.
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](9)
	})
	group := &workloadv1alpha1.ServingGroup{Spec: GroupSpec(ms, Settings{})}
	revisions := newStore(recorded(t, old, "rev-a"), recordedByV110(t, old, "rev-0"))

	for _, revision := range []string{"rev-a", "rev-0"} {
		view, err := NewHistory(Base(ms, group), revisions.get).At(revision)
		if err != nil {
			t.Fatalf("At(%s): %v", revision, err)
		}
		prefill := view.Spec.Template.Roles[0]
		if prefill.Name != "prefill" || prefill.EntryTemplate.Spec.Containers[0].Image != "model:v0" || prefill.WorkerReplicas != 1 {
			t.Errorf("%s: prefill = %s, image %s, %d workers, want the template of the revision",
				revision, prefill.Name, prefill.EntryTemplate.Spec.Containers[0].Image, prefill.WorkerReplicas)
		}
		if *prefill.Replicas != 2 || prefill.Partition.IntValue() != 1 {
			t.Errorf("%s: prefill has %d replicas and partition %v, want those of the group", revision, *prefill.Replicas, prefill.Partition)
		}
		if got := ExpectedPods(view); got != 2*2+1 {
			t.Errorf("%s: ExpectedPods() = %d, want 5", revision, got)
		}
	}

	// A revision recorded by release 1.1 holds the Roles alone: the scheduler
	// name is the one of the ModelServing.
	history := NewHistory(Base(ms, group), revisions.get)
	current, _ := history.At("rev-a")
	legacy, _ := history.At("rev-0")
	if current.Spec.SchedulerName != "default-scheduler" || legacy.Spec.SchedulerName != "volcano" {
		t.Errorf("scheduler names = %s, %s, want default-scheduler, volcano", current.Spec.SchedulerName, legacy.Spec.SchedulerName)
	}
	current.Spec.Template.Roles[0].Name = "changed"
	if again, _ := history.At("rev-a"); again.Spec.Template.Roles[0].Name != "prefill" {
		t.Error("At returned an object it keeps")
	}
}

func TestUnresolvedRevisions(t *testing.T) {
	ms := modelServing()
	other := modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.UID = "another-uid" })
	garbage := recorded(t, ms, "rev-garbage")
	garbage.Data.Raw = []byte(`{"spec":`)
	noRoles := recorded(t, modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles = nil }), "rev-empty")
	revisions := newStore(recorded(t, ms, "rev-a"), recorded(t, other, "rev-foreign"), garbage, noRoles)
	history := NewHistory(ms, revisions.get)

	for _, revision := range []string{"", "rev-missing", "rev-foreign", "rev-garbage", "rev-empty"} {
		_, err := history.At(revision)
		var unresolved *UnresolvedError
		if !errors.As(err, &unresolved) || unresolved.Revision != revision || !IsUnresolved(err) {
			t.Errorf("At(%q) = %v, want an UnresolvedError for that revision", revision, err)
		}
	}
	if _, err := history.Role("rev-a", "embed"); !IsUnresolved(err) {
		t.Errorf("Role of a revision that does not have it = %v, want an UnresolvedError", err)
	}
	if IsUnresolved(errors.New("other")) || IsUnresolved(nil) {
		t.Error("IsUnresolved is true for an unrelated error")
	}

	// A round asks once. The next round, or Forget, asks again.
	reads := revisions.reads
	history.At("rev-missing")
	history.At("rev-a")
	if revisions.reads != reads {
		t.Errorf("a revision was read %d more times within one round", revisions.reads-reads)
	}
	revisions.revisions["default/llama-rev-missing"] = recorded(t, ms, "rev-missing")
	if _, err := history.At("rev-missing"); err == nil {
		t.Error("a revision that could not be resolved changed within one round")
	}
	history.Forget("rev-missing")
	if _, err := history.At("rev-missing"); err != nil {
		t.Errorf("At after Forget: %v", err)
	}

	// An error that is not NotFound is not hidden.
	revisions.err = errors.New("apiserver is away")
	if _, err := NewHistory(ms, revisions.get).At("rev-a"); !IsUnresolved(err) || !errors.Is(err, revisions.err) {
		t.Errorf("At() = %v, want the error of the read", err)
	}
}

func TestCompare(t *testing.T) {
	ms := modelServing()
	prefillChanged := modelServing(image(0, "model:v2"))
	revisions := newStore(
		recorded(t, ms, "rev-a"),
		recorded(t, ms, "rev-a-again"),
		recordedByV110(t, ms, "rev-v110"),
		recorded(t, prefillChanged, "rev-b"),
	)
	history := NewHistory(ms, revisions.get)

	tests := []struct {
		revision, target      string
		group, prefill, decod Comparison
	}{
		{"rev-a", "rev-a", Equivalent, Equivalent, Equivalent},
		{"rev-missing", "rev-missing", Equivalent, Equivalent, Equivalent},
		{"rev-a", "rev-a-again", Equivalent, Equivalent, Equivalent},
		{"rev-v110", "rev-a", Equivalent, Equivalent, Equivalent},
		{"rev-a", "rev-b", Different, Different, Equivalent},
		{"rev-b", "rev-a", Different, Different, Equivalent},
		{"rev-v110", "rev-b", Different, Different, Equivalent},
		{"rev-missing", "rev-a", Unknown, Unknown, Unknown},
		{"rev-a", "rev-missing", Unknown, Unknown, Unknown},
		{"", "rev-a", Unknown, Unknown, Unknown},
		{"rev-a", "", Unknown, Unknown, Unknown},
	}
	for _, tt := range tests {
		// Twice: the second answer is the remembered one.
		for range 2 {
			if got := history.Compare(tt.revision, tt.target); got != tt.group {
				t.Errorf("Compare(%q, %q) = %s, want %s", tt.revision, tt.target, got, tt.group)
			}
			if got := history.CompareRole(tt.revision, tt.target, "prefill"); got != tt.prefill {
				t.Errorf("CompareRole(%q, %q, prefill) = %s, want %s", tt.revision, tt.target, got, tt.prefill)
			}
			if got := history.CompareRole(tt.revision, tt.target, "decode"); got != tt.decod {
				t.Errorf("CompareRole(%q, %q, decode) = %s, want %s", tt.revision, tt.target, got, tt.decod)
			}
		}
	}
	if got := history.CompareRole("rev-a", "rev-b", "embed"); got != Unknown {
		t.Errorf("CompareRole of a Role neither revision has = %s, want Unknown", got)
	}

	// The hash of a Role is the one its Pods are labelled with.
	want, err := utils.RoleRevisionHash(prefillChanged, "prefill")
	if err != nil {
		t.Fatal(err)
	}
	if got, err := history.RoleHash("rev-b", "prefill"); err != nil || got != want {
		t.Errorf("RoleHash() = %s, %v, want %s", got, err, want)
	}
	if same, _ := history.RoleHash("rev-a", "prefill"); same == want {
		t.Error("two different templates of a Role have the same hash")
	}
}
