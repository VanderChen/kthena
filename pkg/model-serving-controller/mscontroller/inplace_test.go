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
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/sgcontroller"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func updatedInPlace(ms *workloadv1alpha1.ModelServing) {
	ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
}

// withEvents replaces the controllers of s by ones that record their events,
// and returns the recorder.
func (s *system) withEvents() *record.FakeRecorder {
	recorder := record.NewFakeRecorder(1000)
	now := func() time.Time { return s.cluster.Now() }
	s.ms = New(Options{Kube: s.cluster.Kube, Kthena: s.cluster.Kthena, Now: now, Recorder: recorder})
	s.sg = sgcontroller.New(sgcontroller.Options{Kube: s.cluster.Kube, Kthena: s.cluster.Kthena, PodGroups: noPodGroups{}, Now: now, Recorder: recorder})
	return recorder
}

// eventsOf takes the events recorded so far and returns the ones that have
// the reason.
func eventsOf(recorder *record.FakeRecorder, reason string) []string {
	var out []string
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, " "+reason+" ") {
				out = append(out, event)
			}
		default:
			return out
		}
	}
}

// runContainers plays the kubelet with containers: it starts the containers
// of a Pod that has none, and restarts a container whose image was changed.
// A restart takes two calls: the Pod is not ready after the first and ready,
// in a new container, after the second. It returns the number of Pods it
// changed.
func (s *system) runContainers() int {
	s.t.Helper()
	changed := 0
	for _, pod := range s.pods() {
		if pod.DeletionTimestamp != nil || s.stuck != nil && s.stuck(pod) {
			continue
		}
		ready := utils.IsPodRunningAndReady(pod)
		touched := false
		for _, container := range pod.Spec.Containers {
			var status *corev1.ContainerStatus
			for i := range pod.Status.ContainerStatuses {
				if pod.Status.ContainerStatuses[i].Name == container.Name {
					status = &pod.Status.ContainerStatuses[i]
				}
			}
			switch {
			case status == nil:
				pod.Status.ContainerStatuses = append(pod.Status.ContainerStatuses, corev1.ContainerStatus{
					Name: container.Name, Image: container.Image, ImageID: "sha256:" + container.Image,
					ContainerID: fmt.Sprintf("containerd://%s-%s-0", pod.UID, container.Name),
				})
				touched = true
			case status.Image == container.Image:
			case ready:
				// The old container is stopped.
				touched = true
			default:
				status.RestartCount++
				status.Image, status.ImageID = container.Image, "sha256:"+container.Image
				status.ContainerID = fmt.Sprintf("containerd://%s-%s-%d", pod.UID, container.Name, status.RestartCount)
				touched = true
			}
		}
		if !touched && ready {
			continue
		}
		becomes := corev1.ConditionTrue
		if touched && ready {
			becomes = corev1.ConditionFalse
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: becomes, LastTransitionTime: metav1.NewTime(s.cluster.Now())}}
		if _, err := s.cluster.Kube.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			s.t.Fatal(err)
		}
		changed++
	}
	return changed
}

// settleContainers is settle with a kubelet that restarts containers.
func (s *system) settleContainers() {
	s.t.Helper()
	for range 200 {
		changed := s.runContainers()
		s.cluster.Settle()
		if s.pass() == 0 && changed == 0 {
			return
		}
	}
	s.t.Fatalf("the ModelServing did not settle: %s", s.summary())
}

// podUIDs returns the UIDs of the Pods by name.
func (s *system) podUIDs() map[string]types.UID {
	s.t.Helper()
	uids := map[string]types.UID{}
	for _, pod := range s.pods() {
		uids[pod.Name] = pod.UID
	}
	return uids
}

// A rolling update in place patches the images of the Pods of at most
// maxUnavailable groups at a time. No Pod and no group is replaced, and the
// revision label of a Pod changes only once its group runs the new image.
func TestInPlaceRollingUpdateKeepsTheBudgetAndThePods(t *testing.T) {
	tests := []struct {
		name           string
		maxUnavailable intstr.IntOrString
		floor          int
	}{
		{name: "one unavailable", maxUnavailable: intstr.FromInt32(1), floor: 2},
		{name: "two unavailable", maxUnavailable: intstr.FromInt32(2), floor: 1},
		{name: "one in three by percentage", maxUnavailable: intstr.FromString("34%"), floor: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(updatedInPlace, rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
				config.MaxUnavailable = &tt.maxUnavailable
			})))
			recorder := s.withEvents()
			s.settleContainers()
			before := s.modelServing().Status.UpdateRevision
			pods, groups := s.podUIDs(), map[string]types.UID{}
			for _, group := range s.groups() {
				groups[group.Name] = group.UID
				if method := group.Spec.RolloutStrategy.UpdateMethod; method != workloadv1alpha1.InPlaceUpdateMethod {
					t.Fatalf("group %s is told the update method %q, want InPlace", group.Name, method)
				}
			}

			lowest := 3
			s.invariant = func() {
				ready := s.readyGroups()
				lowest = min(lowest, ready)
				if ready < tt.floor {
					t.Fatalf("%d groups are ready, the budget allows no fewer than %d: %s", ready, tt.floor, s.summary())
				}
				// A group counts as updated in the test of the product by the
				// labels and the in-place state of its Pods. Those must not say
				// that more groups are being updated than the budget allows.
				updating := map[string]bool{}
				for _, pod := range s.pods() {
					if inplace.InProgress(pod) || !utils.IsPodRunningAndReady(pod) {
						updating[pod.Labels[workloadv1alpha1.GroupNameLabelKey]] = true
					}
					target := s.modelServing().Status.UpdateRevision
					if pod.Labels[workloadv1alpha1.RevisionLabelKey] == target && target != before && pod.Spec.Containers[0].Image != "model:v2" {
						t.Fatalf("Pod %s is labelled with the new revision and runs %s", pod.Name, pod.Spec.Containers[0].Image)
					}
				}
				if len(updating) > 3-tt.floor {
					t.Fatalf("%d groups are being updated, the budget allows %d: %v", len(updating), 3-tt.floor, updating)
				}
			}
			s.update(setImage("model:v2"))
			s.settleContainers()
			s.invariant = nil

			if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
			}
			status := s.modelServing().Status
			if status.UpdateRevision == before || status.CurrentRevision != status.UpdateRevision {
				t.Errorf("revisions after the rollout: current %s, update %s, before %s", status.CurrentRevision, status.UpdateRevision, before)
			}
			if lowest != tt.floor {
				t.Errorf("the rollout never went below %d ready groups; the budget allows %d", lowest, tt.floor)
			}
			for _, group := range s.groups() {
				if group.UID != groups[group.Name] {
					t.Errorf("group %s was replaced", group.Name)
				}
				if group.Spec.Revision != status.UpdateRevision || group.Status.Revision != status.UpdateRevision || !groupReady(group) ||
					meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionRecreateRequired)) {
					t.Errorf("group %s: spec.revision %s, status.revision %s, conditions %v", group.Name, group.Spec.Revision, group.Status.Revision, group.Status.Conditions)
				}
			}
			after := s.pods()
			if len(after) != len(pods) {
				t.Errorf("%d Pods after the update, %d before", len(after), len(pods))
			}
			for _, pod := range after {
				state := inplace.StateOf(pod)
				if pod.UID != pods[pod.Name] || pod.Status.ContainerStatuses[0].RestartCount != 1 || state == nil || !state.Done ||
					pod.Labels[workloadv1alpha1.RevisionLabelKey] != status.UpdateRevision {
					t.Errorf("Pod %s: UID %s (before %s), restarts %d, in-place state %+v, revision %s", pod.Name, pod.UID, pods[pod.Name],
						pod.Status.ContainerStatuses[0].RestartCount, state, pod.Labels[workloadv1alpha1.RevisionLabelKey])
				}
			}
			if completed := eventsOf(recorder, sgcontroller.ReasonInPlaceUpdateCompleted); len(completed) != 3 {
				t.Errorf("InPlaceUpdateCompleted events = %v, want one for each group", completed)
			}
		})
	}
}

// InPlace takes effect only when the rollout can use it: the ModelServing
// asks for it, every plugin supports it, and where whole groups are replaced
// a group may be unavailable. Otherwise the ModelServing is rolled as with
// Recreate.
func TestTheUpdateMethodThatTakesEffect(t *testing.T) {
	plugin := func(name string) func(*workloadv1alpha1.ModelServing) {
		return func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Plugins = append(ms.Spec.Plugins, workloadv1alpha1.PluginSpec{Name: name, Type: workloadv1alpha1.PluginTypeBuiltIn})
		}
	}
	maxUnavailable := func(value string) func(*workloadv1alpha1.ModelServing) {
		return rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
			config.MaxUnavailable = ptr.To(intstr.Parse(value))
		})
	}
	tests := []struct {
		name   string
		mutate []func(*workloadv1alpha1.ModelServing)
		want   bool
		// invalid: the ModelServing cannot be reconciled at all.
		invalid bool
	}{
		{name: "Recreate"},
		{name: "no rollout strategy", mutate: []func(*workloadv1alpha1.ModelServing){func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RolloutStrategy = nil }}},
		{name: "InPlace", mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace}, want: true},
		{name: "InPlace with the default budget of one group", want: true,
			mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, func(ms *workloadv1alpha1.ModelServing) { ms.Spec.RolloutStrategy.RollingUpdateConfiguration = nil }}},
		{name: "InPlace, no group may be unavailable", mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, maxUnavailable("0")}},
		{name: "InPlace, a percentage that rounds down to no group", mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, maxUnavailable("30%")}},
		{name: "InPlace with a plugin that supports it", want: true,
			mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, plugin(plugins.LWSLabelsPluginName)}},
		{name: "InPlace with a plugin that does not support it",
			mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, plugin(plugins.LWSLabelsPluginName), plugin(plugins.HeadlessServicePluginName)}},
		{name: "InPlace with a plugin that is not registered", invalid: true,
			mutate: []func(*workloadv1alpha1.ModelServing){updatedInPlace, plugin("unknown")}},
		{name: "RoleRollingUpdate: the budget is the one of every Role, which the group applies", want: true,
			mutate: []func(*workloadv1alpha1.ModelServing){roleRolling(), updatedInPlace, func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workloadv1alpha1.RollingUpdateConfiguration{MaxUnavailable: ptr.To(intstr.FromInt32(0))}
			}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := modelServing(tt.mutate...)
			spec, err := rolloutSpec(ms, "target", "baseline")
			if err != nil {
				t.Fatal(err)
			}
			c := New(Options{})
			if got := c.updatesInPlace(ms, spec); got != tt.want {
				t.Errorf("updatesInPlace() = %t, want %t", got, tt.want)
			}
			if tt.invalid {
				return
			}

			// What the plan and the groups are told follows from it.
			s := newSystem(t, ms)
			s.pass()
			method := workloadv1alpha1.RecreateUpdateMethod
			if tt.want {
				method = workloadv1alpha1.InPlaceUpdateMethod
			}
			for _, group := range s.groups() {
				strategy := group.Spec.RolloutStrategy
				if ms.Spec.RolloutStrategy == nil {
					if strategy != nil {
						t.Errorf("group %s has the rollout strategy %+v, the ModelServing has none", group.Name, strategy)
					}
					continue
				}
				if strategy == nil || strategy.UpdateMethod != method {
					t.Errorf("group %s is told %+v, want the update method %s", group.Name, strategy, method)
				}
			}
			v := s.ms.observe(s.modelServing(), spec, nil, observed{})
			if v.inPlace != tt.want || v.spec.InPlace != (tt.want && spec.Mode == grouplevel.ModeServingGroup) {
				t.Errorf("view: inPlace %t, plan in place %t", v.inPlace, v.spec.InPlace)
			}
		})
	}
}

// Where InPlace does not take effect, the ModelServing is rolled as with
// Recreate, surge included, and no Pod is patched.
func TestInPlaceWithoutAnUnavailableBudgetUsesTheSurge(t *testing.T) {
	s := newSystem(t, modelServing(updatedInPlace, rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxUnavailable, config.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
	})))
	recorder := s.withEvents()
	s.settleContainers()
	pods := s.podUIDs()

	highest := 3
	s.invariant = func() {
		highest = max(highest, len(s.groups()))
		if ready := s.readyGroups(); ready < 3 {
			t.Fatalf("%d groups are ready, the budget allows no fewer than 3: %s", ready, s.summary())
		}
	}
	s.update(setImage("model:v2"))
	s.settleContainers()
	s.invariant = nil

	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	if highest != 4 {
		t.Errorf("the rollout had at most %d groups, want the surge group used", highest)
	}
	for _, pod := range s.pods() {
		if pod.UID == pods[pod.Name] || inplace.StateOf(pod) != nil {
			t.Errorf("Pod %s was patched, not replaced", pod.Name)
		}
	}
	if events := eventsOf(recorder, ReasonInPlaceUpdateFallback); len(events) != 0 {
		t.Errorf("fallback events = %v; a method that does not take effect is not reported group by group", events)
	}
}

// A change of more than images cannot be made in place. The groups are
// replaced by deletion, within the budget, without their revision being
// changed first, and an event says why.
func TestAChangeBeyondImagesIsRolledByRecreation(t *testing.T) {
	s := newSystem(t, modelServing(updatedInPlace))
	recorder := s.withEvents()
	s.settleContainers()
	before := s.modelServing().Status.UpdateRevision
	groups := map[string]types.UID{}
	for _, group := range s.groups() {
		groups[group.Name] = group.UID
	}

	s.invariant = func() {
		if ready := s.readyGroups(); ready < 2 {
			t.Fatalf("%d groups are ready, the budget allows no fewer than 2: %s", ready, s.summary())
		}
		for _, group := range s.groups() {
			if group.UID == groups[group.Name] && group.Spec.Revision != before {
				t.Fatalf("group %s was given the revision %s although it cannot be updated in place", group.Name, group.Spec.Revision)
			}
			if meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionRecreateRequired)) {
				t.Fatalf("group %s carries RecreateRequired; the decision is made before its revision is changed", group.Name)
			}
		}
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		for _, template := range []*workloadv1alpha1.PodTemplateSpec{&ms.Spec.Template.Roles[0].EntryTemplate, ms.Spec.Template.Roles[0].WorkerTemplate} {
			template.Spec.Containers[0].Image = "model:v2"
			template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "MODE", Value: "changed"}}
		}
	})
	s.settleContainers()
	s.invariant = nil

	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	for _, group := range s.groups() {
		if group.UID == groups[group.Name] {
			t.Errorf("group %s was not replaced", group.Name)
		}
	}
	for _, pod := range s.pods() {
		if inplace.StateOf(pod) != nil {
			t.Errorf("Pod %s was patched in place", pod.Name)
		}
	}
	events := eventsOf(recorder, ReasonInPlaceUpdateFallback)
	for _, group := range []string{"llama-0", "llama-1", "llama-2"} {
		want := "Normal InPlaceUpdateFallback ServingGroup " + group + " is recreated instead of updated in place: changes other than container images"
		found := false
		for _, event := range events {
			found = found || event == want
		}
		if !found {
			t.Errorf("no event %q in %v", want, events)
		}
	}
}

// Whether a group can be updated in place is asked only where the rollout
// replaces it: a group below the partition stays, whatever changed.
func TestAChangeBeyondImagesStopsAtThePartition(t *testing.T) {
	s := newSystem(t, modelServing(updatedInPlace, rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.Partition = ptr.To(intstr.FromInt32(2))
	})))
	recorder := s.withEvents()
	s.settleContainers()
	groups := map[string]types.UID{}
	for _, group := range s.groups() {
		groups[group.Name] = group.UID
	}

	s.update(func(ms *workloadv1alpha1.ModelServing) {
		for _, template := range []*workloadv1alpha1.PodTemplateSpec{&ms.Spec.Template.Roles[0].EntryTemplate, ms.Spec.Template.Roles[0].WorkerTemplate} {
			template.Spec.Containers[0].Image = "model:v2"
			template.Spec.Containers[0].Env = []corev1.EnvVar{{Name: "MODE", Value: "changed"}}
		}
	})
	s.settleContainers()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v2] replicas=3 updated=1 current=2 available=3 Available"; got != want {
		t.Fatalf("at the partition:\n got %s\nwant %s", got, want)
	}
	for _, group := range s.groups() {
		if replaced := group.UID != groups[group.Name]; replaced != (group.Name == "llama-2") {
			t.Errorf("group %s: replaced = %t", group.Name, replaced)
		}
	}
	if events := eventsOf(recorder, ReasonInPlaceUpdateFallback); len(events) == 0 || strings.Contains(strings.Join(events, "\n"), "llama-0") || strings.Contains(strings.Join(events, "\n"), "llama-1") {
		t.Errorf("fallback events = %v, want them for llama-2 alone", events)
	}
}

// What only the Pods can tell is found by the group after its revision was
// changed: it asks to be recreated, and is replaced within the budget.
func TestAGroupThatAsksToBeRecreatedIsReplaced(t *testing.T) {
	never := func(ms *workloadv1alpha1.ModelServing) {
		role := &ms.Spec.Template.Roles[0]
		role.EntryTemplate.Spec.RestartPolicy, role.WorkerTemplate.Spec.RestartPolicy = corev1.RestartPolicyNever, corev1.RestartPolicyNever
	}
	s := newSystem(t, modelServing(updatedInPlace, never))
	recorder := s.withEvents()
	s.settleContainers()
	groups := map[string]types.UID{}
	for _, group := range s.groups() {
		groups[group.Name] = group.UID
	}

	asked := map[string]bool{}
	s.invariant = func() {
		if ready := s.readyGroups(); ready < 2 {
			t.Fatalf("%d groups are ready, the budget allows no fewer than 2: %s", ready, s.summary())
		}
		for _, group := range s.groups() {
			if meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionRecreateRequired)) {
				asked[group.Name] = true
			}
		}
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "model:v2"
		ms.Spec.Template.Roles[0].WorkerTemplate.Spec.Containers[0].Image = "model:v2"
	})
	s.settleContainers()
	s.invariant = nil

	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	for _, group := range s.groups() {
		if group.UID == groups[group.Name] || !asked[group.Name] {
			t.Errorf("group %s: replaced = %t, asked to be recreated = %t; want both", group.Name, group.UID != groups[group.Name], asked[group.Name])
		}
		if meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionRecreateRequired)) {
			t.Errorf("the replacement of group %s asks to be recreated", group.Name)
		}
	}
	if events := eventsOf(recorder, ReasonInPlaceUpdateFallback); len(events) != 3 || !strings.Contains(events[0], "restartPolicy is not Always") {
		t.Errorf("fallback events = %v, want one for each group", events)
	}
}

// The partition holds for an in-place update as for any other.
func TestAnInPlaceUpdateStopsAtThePartition(t *testing.T) {
	s := newSystem(t, modelServing(updatedInPlace, rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.Partition = ptr.To(intstr.FromInt32(2))
	})))
	s.settleContainers()
	pods := s.podUIDs()

	s.update(setImage("model:v2"))
	s.settleContainers()
	if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v2] replicas=3 updated=1 current=2 available=3 Available"; got != want {
		t.Fatalf("at the partition:\n got %s\nwant %s", got, want)
	}
	for _, pod := range s.pods() {
		if patched := inplace.StateOf(pod) != nil; patched != (pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-2") {
			t.Errorf("Pod %s: patched = %t", pod.Name, patched)
		}
	}

	s.update(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.Partition = nil }))
	s.settleContainers()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("without a partition:\n got %s\nwant %s", got, want)
	}
	if after := s.podUIDs(); len(after) != len(pods) {
		t.Errorf("%d Pods, before %d", len(after), len(pods))
	}
	for name, uid := range s.podUIDs() {
		if uid != pods[name] {
			t.Errorf("Pod %s was replaced", name)
		}
	}
}

// The template can change again while groups are being updated, and both
// controllers can be replaced at any time. The budget holds throughout, and
// no Pod is replaced as long as only images change.
func TestTheTemplateChangesDuringAnInPlaceRollout(t *testing.T) {
	tests := []struct {
		name string
		next string
		// replaced: groups whose Pods were patched for the abandoned revision
		// are recreated.
		replaced bool
	}{
		{name: "to another image", next: "model:v3"},
		{name: "back to the image the Pods were created with", next: "model:v1", replaced: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(updatedInPlace))
			s.settleContainers()
			pods := s.podUIDs()
			s.invariant = func() {
				if ready := s.readyGroups(); ready < 2 {
					t.Fatalf("%d groups are ready, the budget allows no fewer than 2: %s", ready, s.summary())
				}
			}

			s.update(setImage("model:v2"))
			// The first group is patched and its containers are stopped; then
			// the template changes.
			s.pass()
			s.runContainers()
			s.restart()
			updating := 0
			for _, pod := range s.pods() {
				if inplace.InProgress(pod) && !utils.IsPodRunningAndReady(pod) && pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-2" {
					updating++
				}
			}
			if got := s.images(); got != "0=model:v1 1=model:v1 2=model:v2" || updating != 2 {
				t.Fatalf("in the middle of the rollout: %s, %d Pods of llama-2 are being updated", got, updating)
			}
			// The new containers of the group do not come up for a while, so
			// the change finds the group in the middle of its update.
			s.stuck = func(pod *corev1.Pod) bool { return pod.Labels[workloadv1alpha1.GroupNameLabelKey] == "llama-2" }
			s.update(setImage(tt.next))
			for range 10 {
				s.cluster.Settle()
				s.restart()
				s.pass()
			}
			s.stuck = nil
			for range 200 {
				changed := s.runContainers()
				s.cluster.Settle()
				s.restart()
				if s.pass() == 0 && changed == 0 {
					break
				}
			}
			s.invariant = nil

			image := tt.next
			if got, want := s.summary(), fmt.Sprintf("groups[0=%[1]s 1=%[1]s 2=%[1]s] replicas=3 updated=3 current=3 available=3 Available", image); got != want {
				t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
			}
			for name, uid := range s.podUIDs() {
				replaced := uid != pods[name]
				if want := tt.replaced && strings.HasPrefix(name, "llama-2-"); replaced != want {
					t.Errorf("Pod %s: replaced = %t, want %t", name, replaced, want)
				}
			}
			for _, pod := range s.pods() {
				if inplace.InProgress(pod) || pod.Labels[workloadv1alpha1.RevisionLabelKey] != s.modelServing().Status.UpdateRevision {
					t.Errorf("Pod %s: revision %s, in-place state %+v", pod.Name, pod.Labels[workloadv1alpha1.RevisionLabelKey], inplace.StateOf(pod))
				}
			}
		})
	}
}

// Under RoleRollingUpdate every group updates its Role instances in place
// itself, within the budget of the Role. No group and no Pod is replaced.
func TestRoleRollingUpdateInPlace(t *testing.T) {
	s := newSystem(t, modelServing(roleRolling(), updatedInPlace))
	s.settleContainers()
	pods, groups := s.podUIDs(), map[string]types.UID{}
	for _, group := range s.groups() {
		groups[group.Name] = group.UID
	}
	baseline := s.modelServing().Status.CurrentRevision

	lowest := 3
	s.invariant = func() {
		ready := s.readyInstances()
		lowest = min(lowest, ready)
		if ready < 2 {
			t.Fatalf("a group has %d ready instances, the budget allows no fewer than 2: %s", ready, s.instances())
		}
		for _, group := range s.groups() {
			updating := map[string]bool{}
			for _, pod := range s.pods() {
				if pod.Labels[workloadv1alpha1.GroupNameLabelKey] == group.Name && inplace.InProgress(pod) {
					updating[pod.Labels[workloadv1alpha1.RoleIDKey]] = true
				}
			}
			if len(updating) > 1 {
				t.Fatalf("group %s updates %d instances at a time, the budget allows 1", group.Name, len(updating))
			}
		}
	}
	s.update(setImage("model:v2"))
	s.settleContainers()
	s.invariant = nil

	if got, want := s.instances(), "0[0=model:v2 1=model:v2 2=model:v2] 1[0=model:v2 1=model:v2 2=model:v2]"; got != want {
		t.Fatalf("rolled: %s", got)
	}
	if got, want := s.counts()+" "+s.condition(), "replicas=2 updated=2 current=2 available=2 Available"; got != want {
		t.Fatalf("status after the rollout = %s, want %s", got, want)
	}
	if status := s.modelServing().Status; status.CurrentRevision != status.UpdateRevision || status.CurrentRevision == baseline {
		t.Errorf("revisions after the rollout: %s, %s; before %s", status.CurrentRevision, status.UpdateRevision, baseline)
	}
	if lowest != 2 {
		t.Errorf("the rollout never went below %d ready instances; the budget allows 2", lowest)
	}
	for _, group := range s.groups() {
		if group.UID != groups[group.Name] {
			t.Errorf("group %s was replaced", group.Name)
		}
	}
	for name, uid := range s.podUIDs() {
		if uid != pods[name] {
			t.Errorf("Pod %s was replaced", name)
		}
	}
	for _, pod := range s.pods() {
		if state := inplace.StateOf(pod); state == nil || !state.Done || pod.Status.ContainerStatuses[0].RestartCount != 1 {
			t.Errorf("Pod %s: in-place state %+v, restarts %d", pod.Name, state, pod.Status.ContainerStatuses[0].RestartCount)
		}
	}
}
