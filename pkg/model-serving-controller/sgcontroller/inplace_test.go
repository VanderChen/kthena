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
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func inPlaceStrategy(ms *workloadv1alpha1.ModelServing) {
	ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type:                    workloadv1alpha1.ServingGroupRollingUpdate,
		UpdateMethod:            workloadv1alpha1.InPlaceUpdateMethod,
		ProgressDeadlineSeconds: ptr.To[int32](600),
	}
}

// rolledByRole turns ms into one whose groups are rolled Role by Role: three
// instances of prefill, one of which may be unavailable, and one instance of
// decode.
func rolledByRole(ms *workloadv1alpha1.ModelServing) {
	ms.Spec.RolloutStrategy.Type = workloadv1alpha1.RoleRollingUpdate
	prefill := &ms.Spec.Template.Roles[0]
	prefill.Replicas, prefill.WorkerReplicas, prefill.WorkerTemplate = ptr.To[int32](3), 1, ptr.To(podTemplate("model:v1"))
	prefill.MaxUnavailable = ptr.To(intstr.FromInt32(1))
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
		Name: "decode", Replicas: ptr.To[int32](1), EntryTemplate: podTemplate("model:v1"),
	})
}

// withImage returns ms with the image of prefill changed and nothing else.
func withImage(ms *workloadv1alpha1.ModelServing, image string) *workloadv1alpha1.ModelServing {
	changed := ms.DeepCopy()
	role := &changed.Spec.Template.Roles[0]
	role.EntryTemplate.Spec.Containers[0].Image = image
	if role.WorkerTemplate != nil {
		role.WorkerTemplate.Spec.Containers[0].Image = image
	}
	return changed
}

// withEnv returns ms with the image and an environment variable of prefill changed.
func withEnv(ms *workloadv1alpha1.ModelServing, image string) *workloadv1alpha1.ModelServing {
	changed := withImage(ms, image)
	container := &changed.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0]
	container.Env = append(container.Env, corev1.EnvVar{Name: "MODE", Value: "changed"})
	return changed
}

// inPlaceHarness returns a harness whose group llama-0 is updated in place.
// The group runs rev-a (model:v1). rev-b (model:v2) and rev-c (model:v3)
// differ from it in the image alone, rev-env in more.
func inPlaceHarness(t *testing.T, mutate ...func(*workloadv1alpha1.ModelServing)) (*harness, *record.FakeRecorder) {
	t.Helper()
	ms := testModelServing(append([]func(*workloadv1alpha1.ModelServing){inPlaceStrategy}, mutate...)...)
	group := groupOf(ms, "rev-a")
	group.Spec.RolloutStrategy.UpdateMethod = ms.Spec.RolloutStrategy.UpdateMethod
	h := newHarness(t, ms, group,
		revisionOf(t, ms, "rev-a"), revisionOf(t, withImage(ms, "model:v2"), "rev-b"),
		revisionOf(t, withImage(ms, "model:v3"), "rev-c"), revisionOf(t, withEnv(ms, "model:v2"), "rev-env"))
	recorder := record.NewFakeRecorder(1000)
	h.recorder = recorder
	h.restart()
	return h, recorder
}

// eventsOf returns the events recorded since the last call.
func eventsOf(recorder *record.FakeRecorder) []string {
	var out []string
	for {
		select {
		case event := <-recorder.Events:
			out = append(out, event)
		default:
			return out
		}
	}
}

// updateEventsOf returns the events recorded since the last call, without the
// ones about the states of Role instances: an instance whose Pods are updated
// is reported as not running until they are ready again, like one whose Pods
// are created.
func updateEventsOf(recorder *record.FakeRecorder) []string {
	var out []string
	for _, event := range eventsOf(recorder) {
		if fields := strings.SplitN(event, " ", 3); len(fields) < 2 || !strings.HasPrefix(fields[1], "Role") {
			out = append(out, event)
		}
	}
	return out
}

// retarget sets the revision the group is to run: spec.revision of a group
// that is replaced as a whole, spec.targetRevision of one that is rolled Role
// by Role. That is what the ModelServing controller does to start an in-place
// update of the group.
func (h *harness) retarget(revision string) {
	h.t.Helper()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		if roleMode(group) {
			group.Spec.TargetRevision = revision
		} else {
			group.Spec.Revision = revision
		}
	})
	h.cluster.ResetWrites()
}

// run plays the kubelet for one Pod: its container runs the image of the
// spec, in a new container when the image changed, and the Pod is ready.
func (h *harness) run(name string) {
	h.t.Helper()
	pod, err := h.cluster.GetPod(testNamespace, name)
	if err != nil {
		h.t.Fatal(err)
	}
	image := pod.Spec.Containers[0].Image
	status := corev1.ContainerStatus{Name: pod.Spec.Containers[0].Name}
	if len(pod.Status.ContainerStatuses) > 0 {
		status = pod.Status.ContainerStatuses[0]
		if status.Image != image {
			status.RestartCount++
		}
	}
	status.Image, status.ImageID, status.Ready = image, "sha256:"+image, true
	status.ContainerID = fmt.Sprintf("containerd://%s-%d", pod.UID, status.RestartCount)
	h.setPodStatus(name, func(podStatus *corev1.PodStatus) {
		podStatus.Phase = corev1.PodRunning
		podStatus.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(h.cluster.Now()),
		}}
		podStatus.ContainerStatuses = []corev1.ContainerStatus{status}
	})
}

// runAll plays the kubelet for every Pod that is not terminating and does not
// run the image of its spec ready. It returns the number of Pods it changed.
func (h *harness) runAll() int {
	h.t.Helper()
	changed := 0
	for _, pod := range h.pods() {
		if pod.DeletionTimestamp != nil || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		statuses := pod.Status.ContainerStatuses
		if utils.IsPodRunningAndReady(pod) && len(statuses) > 0 && statuses[0].Image == pod.Spec.Containers[0].Image && statuses[0].ContainerID != "" {
			continue
		}
		h.run(pod.Name)
		changed++
	}
	return changed
}

// settleContainers is settle with a kubelet that restarts the containers
// whose image changed.
func (h *harness) settleContainers() {
	h.t.Helper()
	for range 40 {
		changed := h.runAll()
		h.cluster.Settle()
		if len(h.mustRound()) == 0 && changed == 0 {
			h.cluster.ResetWrites()
			return
		}
	}
	h.t.Fatalf("the group did not settle; last writes: %v", writesOf(h.cluster.Writes()))
}

// podFacts renders, for every Pod, what an in-place update changes:
// "<name suffix>=<image>/<revision label>/<in-place state>", the state being
// "-" for none, "updating:<revision>" or "done:<revision>".
func (h *harness) podFacts() string {
	h.t.Helper()
	var out []string
	for _, pod := range h.pods() {
		state := "-"
		if s := inplace.StateOf(pod); s.InProgress() {
			state = "updating:" + s.Revision
		} else if s != nil {
			state = "done:" + s.Revision
		}
		item := fmt.Sprintf("%s=%s/%s/%s", strings.TrimPrefix(pod.Name, testGroup+"-"), pod.Spec.Containers[0].Image, pod.Labels[workloadv1alpha1.RevisionLabelKey], state)
		if pod.DeletionTimestamp != nil {
			item += "!"
		}
		out = append(out, item)
	}
	return strings.Join(out, " ")
}

func (h *harness) uids() map[string]string {
	h.t.Helper()
	uids := map[string]string{}
	for _, pod := range h.pods() {
		uids[pod.Name] = string(pod.UID)
	}
	return uids
}

func hasEvent(events []string, reason string) bool {
	for _, event := range events {
		if strings.Contains(event, " "+reason+" ") {
			return true
		}
	}
	return false
}

const (
	allV1 = "prefill-0-0=model:v1/rev-a/- prefill-0-1=model:v1/rev-a/- prefill-0-2=model:v1/rev-a/-"
	// patchedV2: the images are patched; the labels are the old ones.
	patchedV2 = "prefill-0-0=model:v2/rev-a/updating:rev-b prefill-0-1=model:v2/rev-a/updating:rev-b prefill-0-2=model:v2/rev-a/updating:rev-b"
	allV2     = "prefill-0-0=model:v2/rev-b/done:rev-b prefill-0-1=model:v2/rev-b/done:rev-b prefill-0-2=model:v2/rev-b/done:rev-b"
)

var patchAll = []string{"patch pods llama-0-prefill-0-0", "patch pods llama-0-prefill-0-1", "patch pods llama-0-prefill-0-2"}

// A group that is replaced as a whole is updated in place when its revision
// is changed: the status says first that the group is not ready, then the
// images are patched; the revision labels change only when every Pod runs the
// new image in a new container and is ready. No Pod is deleted or created.
func TestAGroupIsUpdatedInPlace(t *testing.T) {
	h, recorder := inPlaceHarness(t)
	h.settleContainers()
	before := h.uids()
	if got := h.podFacts(); got != allV1 {
		t.Fatalf("created: %s", got)
	}
	updateEventsOf(recorder)

	h.retarget("rev-b")
	writes := h.mustRound()
	if want := append([]string{"update servinggroups/status llama-0"}, patchAll...); !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	if got := h.podFacts(); got != patchedV2 {
		t.Fatalf("patched: %s", got)
	}
	status := h.group().Status
	if h.isReady() || status.ObservedGeneration != 2 || status.Revision != "rev-a" {
		t.Errorf("status after the patch: ready %t, observedGeneration %d, revision %q; want not ready, 2, rev-a", h.isReady(), status.ObservedGeneration, status.Revision)
	}
	if events := updateEventsOf(recorder); len(events) != 1 || events[0] != "Normal InPlaceUpdateStarted Updating images in place in ServingGroup llama-0 to revision rev-b" {
		t.Errorf("events = %v", events)
	}

	// The Pods still report ready: the kubelet has not seen the patch. That is
	// not the update being done.
	h.cluster.ResetWrites()
	if writes := h.mustRound(); !reflect.DeepEqual(writes, []string{"update servinggroups/status llama-0"}) {
		t.Errorf("writes while the kubelet has not restarted anything: %v", writes)
	}
	if status := h.group().Status; h.isReady() || !reflect.DeepEqual(status.ReferencedRevisions, []string{"rev-a", "rev-b"}) || status.ReadyPods != 0 {
		t.Errorf("status during the update: ready %t, referenced %v, readyPods %d", h.isReady(), status.ReferencedRevisions, status.ReadyPods)
	}

	// Two of three Pods run the new image: the labels of none change.
	h.run("llama-0-prefill-0-0")
	h.run("llama-0-prefill-0-2")
	h.mustRound()
	if got := h.podFacts(); got != patchedV2 || h.isReady() {
		t.Fatalf("two of three restarted: %s, ready %t", got, h.isReady())
	}

	h.run("llama-0-prefill-0-1")
	h.cluster.ResetWrites()
	writes = h.mustRound()
	if want := append([]string{"update servinggroups/status llama-0"}, patchAll...); !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes of the round that finishes the update = %v, want %v", writes, want)
	}
	if got := h.podFacts(); got != allV2 {
		t.Fatalf("finished: %s", got)
	}
	if h.isReady() {
		t.Errorf("the group is ready in the status that was written before the labels changed")
	}
	if events := updateEventsOf(recorder); len(events) != 1 || events[0] != "Normal InPlaceUpdateCompleted In-place update of ServingGroup llama-0 to revision rev-b completed" {
		t.Errorf("events = %v", events)
	}

	h.settleContainers()
	status = h.group().Status
	if got := h.conditions(); got != "Ready=AllInstancesReady" || status.Revision != "rev-b" || !reflect.DeepEqual(status.ReferencedRevisions, []string{"rev-b"}) {
		t.Errorf("after the update: conditions %q, revision %q, referenced %v", got, status.Revision, status.ReferencedRevisions)
	}
	if got := h.ledger(); got != "prefill-0:Established/0" {
		t.Errorf("ledger = %q", got)
	}
	if after := h.uids(); !reflect.DeepEqual(after, before) {
		t.Errorf("Pods after the update %v, before %v", after, before)
	}
	for _, pod := range h.pods() {
		hash := pod.Labels[workloadv1alpha1.RoleTemplateHashLabelKey]
		want, err := utils.RoleRevisionHash(withImage(testModelServing(inPlaceStrategy), "model:v2"), "prefill")
		if err != nil || hash != want {
			t.Errorf("Pod %s: role template hash %q, want %q (%v)", pod.Name, hash, want, err)
		}
		if restarts := pod.Status.ContainerStatuses[0].RestartCount; restarts != 1 {
			t.Errorf("Pod %s restarted %d times", pod.Name, restarts)
		}
	}
}

// Under RoleRollingUpdate the unit of an in-place update is a Role instance:
// the instances of a Role are patched within the budget of the Role, and the
// Pods of a Role whose template did not change are not touched.
func TestRoleInstancesAreUpdatedInPlace(t *testing.T) {
	h, recorder := inPlaceHarness(t, rolledByRole)
	h.settleContainers()
	before := h.uids()

	h.retarget("rev-b")
	writes := h.mustRound()
	want := []string{"update servinggroups/status llama-0", "patch pods llama-0-prefill-2-0", "patch pods llama-0-prefill-2-1"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want the instance with the highest index patched after the status write", writes)
	}
	if h.isReady() {
		t.Errorf("the group is ready while an instance is being updated")
	}

	// The update goes on one instance at a time: maxUnavailable is 1.
	for range 40 {
		updating := map[string]bool{}
		for _, pod := range h.pods() {
			if inplace.InProgress(pod) {
				updating[pod.Labels[workloadv1alpha1.RoleIDKey]] = true
			}
		}
		if len(updating) > 1 {
			t.Fatalf("%d instances are being updated at a time: %s", len(updating), h.podFacts())
		}
		changed := h.runAll()
		if len(h.mustRound()) == 0 && changed == 0 {
			break
		}
	}

	wantFacts := "decode-0-0=model:v1/rev-a/- " +
		"prefill-0-0=model:v2/rev-b/done:rev-b prefill-0-1=model:v2/rev-b/done:rev-b " +
		"prefill-1-0=model:v2/rev-b/done:rev-b prefill-1-1=model:v2/rev-b/done:rev-b " +
		"prefill-2-0=model:v2/rev-b/done:rev-b prefill-2-1=model:v2/rev-b/done:rev-b"
	if got := h.podFacts(); got != wantFacts {
		t.Fatalf("after the update:\n got %s\nwant %s", got, wantFacts)
	}
	if after := h.uids(); !reflect.DeepEqual(after, before) {
		t.Errorf("Pods after the update %v, before %v", after, before)
	}
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}
	decode, _ := h.cluster.GetPod(testNamespace, "llama-0-decode-0-0")
	if restarts := decode.Status.ContainerStatuses[0].RestartCount; restarts != 0 {
		t.Errorf("the Pod of the Role that did not change restarted %d times", restarts)
	}
	events := updateEventsOf(recorder)
	started, completed := 0, 0
	for _, event := range events {
		switch {
		case strings.Contains(event, " InPlaceUpdateStarted "):
			started++
		case strings.Contains(event, " InPlaceUpdateCompleted "):
			completed++
		}
	}
	if started != 3 || completed != 3 || hasEvent(events, ReasonInPlaceUpdateFallback) {
		t.Errorf("events = %v, want three updates started and completed and no fallback", events)
	}
}

// A group whose Pods cannot be patched reports that it has to be recreated.
// The condition stays, the group keeps serving as it is, and the ModelServing
// controller replaces it within the rollout budget.
func TestAGroupThatCannotBeUpdatedInPlaceAsksToBeRecreated(t *testing.T) {
	renameContainer := func(h *harness) {
		pod, _ := h.cluster.GetPod(testNamespace, "llama-0-prefill-0-1")
		pod.Spec.Containers[0].Name = "renamed"
		if _, err := h.cluster.Kube.CoreV1().Pods(testNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			h.t.Fatal(err)
		}
	}
	rewriteImage := func(h *harness) {
		pod, _ := h.cluster.GetPod(testNamespace, "llama-0-prefill-0-1")
		pod.Spec.Containers[0].Image = "model:rewritten"
		if _, err := h.cluster.Kube.CoreV1().Pods(testNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			h.t.Fatal(err)
		}
	}
	templates := func(mutate func(*corev1.PodSpec)) func(*workloadv1alpha1.ModelServing) {
		return func(ms *workloadv1alpha1.ModelServing) {
			role := &ms.Spec.Template.Roles[0]
			mutate(&role.EntryTemplate.Spec)
			mutate(&role.WorkerTemplate.Spec)
		}
	}
	tests := []struct {
		name     string
		ms       func(*workloadv1alpha1.ModelServing)
		prepare  func(*harness)
		revision string
		message  string
		// silent: the group is recreated without a fallback event.
		silent bool
	}{
		{name: "the Pods are not restarted after a failure", revision: "rev-b",
			ms:      templates(func(spec *corev1.PodSpec) { spec.RestartPolicy = corev1.RestartPolicyNever }),
			message: "pod llama-0-prefill-0-0 restartPolicy is not Always"},
		{name: "a container has a restart policy of its own", revision: "rev-b",
			ms: templates(func(spec *corev1.PodSpec) {
				spec.Containers[0].RestartPolicy = ptr.To(corev1.ContainerRestartPolicyOnFailure)
			}),
			message: "container model of pod llama-0-prefill-0-0 has restartPolicy OnFailure"},
		{name: "a container has restart policy rules", revision: "rev-b",
			ms: templates(func(spec *corev1.PodSpec) {
				spec.Containers[0].RestartPolicyRules = []corev1.ContainerRestartRule{{Action: corev1.ContainerRestartRuleActionRestart}}
			}),
			message: "container model of pod llama-0-prefill-0-0 has restartPolicyRules"},
		{name: "the revisions differ in more than images", revision: "rev-env",
			message: "changes other than container images"},
		{name: "a container was renamed after the Pod was created", revision: "rev-b", prepare: renameContainer,
			message: "pod llama-0-prefill-0-1 has no container [model] of the target template"},
		{name: "an image was rewritten after the Pod was created", revision: "rev-b", prepare: rewriteImage,
			message: "pod llama-0-prefill-0-1 runs images of containers [model] that match neither the old nor the new template"},
		{name: "the update method that takes effect is Recreate", revision: "rev-b", silent: true,
			prepare: func(h *harness) {
				h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
					group.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
				})
			},
			message: "the update method that takes effect is not InPlace"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var mutate []func(*workloadv1alpha1.ModelServing)
			if tt.ms != nil {
				mutate = append(mutate, tt.ms)
			}
			h, recorder := inPlaceHarness(t, mutate...)
			h.settleContainers()
			if tt.prepare != nil {
				tt.prepare(h)
				h.settleContainers()
			}
			before, uids := h.podFacts(), h.uids()
			updateEventsOf(recorder)

			h.retarget(tt.revision)
			if writes := h.mustRound(); !reflect.DeepEqual(writes, []string{"update servinggroups/status llama-0"}) {
				t.Fatalf("writes = %v, want the status alone", writes)
			}
			if got := h.conditions(); got != "Ready=AllInstancesReady RecreateRequired=InPlaceUpdateNotPossible" {
				t.Errorf("conditions = %q", got)
			}
			for _, condition := range h.group().Status.Conditions {
				if condition.Type == string(workloadv1alpha1.ServingGroupConditionRecreateRequired) && condition.Message != tt.message {
					t.Errorf("message = %q, want %q", condition.Message, tt.message)
				}
			}
			events := updateEventsOf(recorder)
			wantEvent := "Normal InPlaceUpdateFallback ServingGroup llama-0 is recreated instead of updated in place: " + tt.message
			if tt.silent && len(events) != 0 || !tt.silent && !reflect.DeepEqual(events, []string{wantEvent}) {
				t.Errorf("events = %v", events)
			}

			// Nothing more happens to the group, whatever its revision becomes:
			// the condition is not taken back.
			h.cluster.ResetWrites()
			h.retarget("rev-a")
			h.settleContainers()
			h.retarget(tt.revision)
			h.settleContainers()
			if got := h.conditions(); got != "Ready=AllInstancesReady RecreateRequired=InPlaceUpdateNotPossible" {
				t.Errorf("conditions later = %q", got)
			}
			if got := h.podFacts(); got != before || !reflect.DeepEqual(h.uids(), uids) {
				t.Errorf("the Pods were touched: %s, before %s", got, before)
			}
			if events := updateEventsOf(recorder); len(events) != 0 {
				t.Errorf("events later = %v", events)
			}
		})
	}
}

// Under RoleRollingUpdate an instance that cannot be updated in place is
// replaced like under Recreate, within the budget of its Role. A Role whose
// budget allows no instance to be unavailable is not updated in place at all.
func TestARoleInstanceThatCannotBeUpdatedInPlaceIsRecreated(t *testing.T) {
	tests := []struct {
		name     string
		ms       func(*workloadv1alpha1.ModelServing)
		revision string
		first    []string
		event    string
	}{
		{name: "the revisions differ in more than images", revision: "rev-env",
			first: []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-2-0", "delete pods llama-0-prefill-2-1"},
			event: "Normal InPlaceUpdateFallback Role prefill/prefill-2 in ServingGroup llama-0 is recreated instead of updated in place: changes other than container images"},
		{name: "the Role may not have an unavailable instance: its surge is used", revision: "rev-b",
			ms: func(ms *workloadv1alpha1.ModelServing) {
				role := &ms.Spec.Template.Roles[0]
				role.MaxUnavailable, role.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstr.FromInt32(1))
			},
			first: []string{"update servinggroups/status llama-0", "create pods llama-0-prefill-3-0", "create pods llama-0-prefill-3-1"}},
		{name: "the update method that takes effect is Recreate", revision: "rev-b",
			ms: func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RolloutStrategy.UpdateMethod = workloadv1alpha1.RecreateUpdateMethod
			},
			first: []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-2-0", "delete pods llama-0-prefill-2-1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := []func(*workloadv1alpha1.ModelServing){rolledByRole}
			if tt.ms != nil {
				mutate = append(mutate, tt.ms)
			}
			h, recorder := inPlaceHarness(t, mutate...)
			h.settleContainers()
			before := h.uids()
			updateEventsOf(recorder)

			h.retarget(tt.revision)
			if writes := h.mustRound(); !reflect.DeepEqual(writes, tt.first) {
				t.Fatalf("writes = %v, want %v", writes, tt.first)
			}
			h.settleContainers()

			image := map[string]string{"rev-b": "model:v2", "rev-env": "model:v2"}[tt.revision]
			for _, pod := range h.pods() {
				prefill := pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill"
				if inplace.StateOf(pod) != nil {
					t.Errorf("Pod %s was patched in place", pod.Name)
				}
				if replaced := string(pod.UID) != before[pod.Name]; replaced != prefill {
					t.Errorf("Pod %s: replaced = %t, want %t", pod.Name, replaced, prefill)
				}
				if prefill && (pod.Spec.Containers[0].Image != image || pod.Labels[workloadv1alpha1.RevisionLabelKey] != tt.revision) {
					t.Errorf("Pod %s runs %s of %s", pod.Name, pod.Spec.Containers[0].Image, pod.Labels[workloadv1alpha1.RevisionLabelKey])
				}
			}
			if got := h.conditions(); got != "Ready=AllInstancesReady" {
				t.Errorf("conditions = %q", got)
			}
			var fallbacks []string
			for _, event := range updateEventsOf(recorder) {
				if strings.Contains(event, " InPlaceUpdateFallback ") {
					fallbacks = append(fallbacks, event)
				}
			}
			switch {
			case tt.event == "" && len(fallbacks) != 0:
				t.Errorf("fallback events = %v, want none", fallbacks)
			case tt.event != "" && (len(fallbacks) != 3 || fallbacks[0] != tt.event):
				t.Errorf("fallback events = %v, want one for each instance, the first %q", fallbacks, tt.event)
			}
		})
	}
}

// An update that does not finish within the progress deadline is handled by
// the recovery policy: the Pods that did not come up on the new image count
// as faulty. A policy that never deletes a Pod leaves them alone, and the
// update still finishes when they come up.
func TestAnUpdateThatMissesItsDeadlineFollowsTheRecoveryPolicy(t *testing.T) {
	const (
		recreatedGroup = "prefill-0-0=model:v2/rev-b/- prefill-0-1=model:v2/rev-b/- prefill-0-2=model:v2/rev-b/-"
		timeout        = "Warning InPlaceUpdateTimeout In-place update of ServingGroup llama-0 exceeded progress deadline 10m0s, applying recovery policy "
	)
	tests := []struct {
		name     string
		policy   workloadv1alpha1.RecoveryPolicy
		grace    int64
		roleMode bool
		// stuck is the Pod whose new container never becomes ready.
		stuck string
		// writes are the writes of the round that finds the deadline passed.
		writes     []string
		ledger     string
		conditions string
		// kept: the Pods are left alone and the update finishes later.
		kept bool
	}{
		{name: "RoleRecreate drains the instance", policy: workloadv1alpha1.RoleRecreate, stuck: "llama-0-prefill-0-1",
			writes: []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-0-1"}, ledger: "prefill-0:Draining/0"},
		{name: "ServingGroupRecreate fails the group", policy: workloadv1alpha1.ServingGroupRecreate, stuck: "llama-0-prefill-0-1",
			writes: []string{"update servinggroups/status llama-0"}, ledger: "prefill-0:Established/0", conditions: "Failed=MemberFaulty"},
		{name: "None leaves the Pods alone", policy: workloadv1alpha1.NoneRestartPolicy, stuck: "llama-0-prefill-0-1",
			ledger: "prefill-0:Established/0", kept: true},
		{name: "a grace period of -1 leaves the Pods alone", policy: workloadv1alpha1.RoleRecreate, grace: -1, stuck: "llama-0-prefill-0-1",
			ledger: "prefill-0:Established/0", kept: true},
		{name: "RoleRecreate drains the Role instance that is rolled", policy: workloadv1alpha1.RoleRecreate, roleMode: true, stuck: "llama-0-prefill-2-1",
			writes: []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-2-1"},
			ledger: "decode-0:Established/0 prefill-0:Established/0 prefill-1:Established/0 prefill-2:Draining/0"},
		{name: "ServingGroupRecreate fails the group that is rolled Role by Role", policy: workloadv1alpha1.ServingGroupRecreate, roleMode: true, stuck: "llama-0-prefill-2-1",
			writes:     []string{"update servinggroups/status llama-0"},
			ledger:     "decode-0:Established/0 prefill-0:Established/0 prefill-1:Established/0 prefill-2:Established/0",
			conditions: "Failed=MemberFaulty"},
		{name: "None leaves the Pods of the Role instance alone", policy: workloadv1alpha1.NoneRestartPolicy, roleMode: true, stuck: "llama-0-prefill-2-1",
			ledger: "decode-0:Established/0 prefill-0:Established/0 prefill-1:Established/0 prefill-2:Established/0", kept: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mutate := []func(*workloadv1alpha1.ModelServing){func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.RecoveryPolicy, ms.Spec.Template.RestartGracePeriodSeconds = tt.policy, ptr.To(tt.grace)
			}}
			if tt.roleMode {
				mutate = append(mutate, rolledByRole)
			}
			h, recorder := inPlaceHarness(t, mutate...)
			h.settleContainers()
			h.retarget("rev-b")
			h.mustRound()
			// Every patched Pod but one comes up on the new image.
			for _, pod := range h.pods() {
				if inplace.InProgress(pod) && pod.Name != tt.stuck {
					h.run(pod.Name)
				}
			}
			patched := h.podFacts()
			updateEventsOf(recorder)

			// The round is due again when the deadline passes.
			if after, err := h.round(); err != nil || after != 600*time.Second {
				t.Fatalf("round = %v, %v; want the next round when the deadline passes, in 10m0s", after, err)
			}
			h.cluster.Advance(599 * time.Second)
			if after, err := h.round(); err != nil || after != time.Second || h.podFacts() != patched {
				t.Fatalf("a second before the deadline: round = %v, %v; Pods %s", after, err, h.podFacts())
			}
			if events := updateEventsOf(recorder); len(events) != 0 {
				t.Errorf("events before the deadline = %v", events)
			}

			h.cluster.Advance(2 * time.Second)
			h.cluster.ResetWrites()
			writes := h.mustRound()
			if !reflect.DeepEqual(writes, tt.writes) && len(writes)+len(tt.writes) > 0 {
				t.Errorf("writes after the deadline = %v, want %v", writes, tt.writes)
			}
			if got := h.ledger(); got != tt.ledger {
				t.Errorf("ledger = %q, want %q", got, tt.ledger)
			}
			if got := h.conditions(); got != tt.conditions {
				t.Errorf("conditions = %q, want %q", got, tt.conditions)
			}
			policy := string(tt.policy)
			// Next to it there may be the event of a Role instance that is drained.
			events := updateEventsOf(recorder)
			var timeouts []string
			for _, event := range events {
				if strings.Contains(event, ReasonInPlaceUpdateTimeout) {
					timeouts = append(timeouts, event)
				}
			}
			if !reflect.DeepEqual(timeouts, []string{timeout + policy}) {
				t.Errorf("events = %v, want the timeout once", events)
			}
			// The timeout is reported once, not in every round.
			h.mustRound()
			if events := updateEventsOf(recorder); hasEvent(events, ReasonInPlaceUpdateTimeout) {
				t.Errorf("the timeout was reported again: %v", events)
			}

			switch {
			case tt.kept:
				if got := h.podFacts(); got != patched {
					t.Fatalf("the Pods were touched: %s", got)
				}
				h.settleContainers()
				for _, pod := range h.pods() {
					if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" && (pod.Labels[workloadv1alpha1.RevisionLabelKey] != "rev-b" || inplace.InProgress(pod)) {
						t.Errorf("Pod %s did not finish the update once it came up: %s", pod.Name, h.podFacts())
					}
				}
				if got := h.conditions(); got != "Ready=AllInstancesReady" {
					t.Errorf("conditions at the end = %q", got)
				}
			case tt.policy == workloadv1alpha1.RoleRecreate:
				h.settleContainers()
				if got := h.conditions(); got != "Ready=AllInstancesReady" {
					t.Errorf("conditions at the end = %q", got)
				}
				if !tt.roleMode && h.podFacts() != recreatedGroup {
					t.Errorf("after the recovery: %s, want %s", h.podFacts(), recreatedGroup)
				}
				for _, pod := range h.pods() {
					if pod.Labels[workloadv1alpha1.RoleLabelKey] == "prefill" && (pod.Labels[workloadv1alpha1.RevisionLabelKey] != "rev-b" || pod.Spec.Containers[0].Image != "model:v2" || inplace.InProgress(pod)) {
						t.Errorf("Pod %s after the recovery: %s", pod.Name, h.podFacts())
					}
				}
			}
		})
	}
}

// With a grace period, a Pod that missed the deadline is given that long
// before it counts; one that comes up in the meantime is kept.
func TestAMissedDeadlineWaitsForTheGracePeriod(t *testing.T) {
	for _, recovers := range []bool{false, true} {
		h, _ := inPlaceHarness(t, func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
		})
		h.settleContainers()
		h.retarget("rev-b")
		h.mustRound()
		h.run("llama-0-prefill-0-0")
		h.run("llama-0-prefill-0-2")
		h.cluster.Advance(601 * time.Second)

		if after, err := h.round(); err != nil || after != 60*time.Second {
			t.Fatalf("round = %v, %v; want the next round when the grace period ends", after, err)
		}
		if got := h.ledger(); got != "prefill-0:Established/0" {
			t.Fatalf("ledger within the grace period = %q", got)
		}
		if recovers {
			h.run("llama-0-prefill-0-1")
		}
		h.cluster.Advance(61 * time.Second)
		h.mustRound()
		want := "prefill-0:Draining/0"
		if recovers {
			want = "prefill-0:Established/0"
		}
		if got := h.ledger(); got != want {
			t.Errorf("recovers = %t: ledger after the grace period = %q, want %q", recovers, got, want)
		}
		if recovers {
			h.settleContainers()
			if got := h.podFacts(); got != allV2 {
				t.Errorf("the update did not finish: %s", got)
			}
		}
	}
}

// A patch that fails leaves the unit patched in part. The next round patches
// the rest, whatever else changed; when that keeps failing until the deadline,
// the recovery policy applies to the Pods that were never patched as well.
func TestAUnitThatIsPatchedInPartIsPatchedToTheEnd(t *testing.T) {
	rejectPatchOf := func(name string) func(apitest.Write) error {
		return func(write apitest.Write) error {
			if write.Verb == "patch" && write.Name == name {
				return errors.New("the patch is rejected")
			}
			return nil
		}
	}
	const half = "prefill-0-0=model:v2/rev-a/updating:rev-b prefill-0-1=model:v1/rev-a/- prefill-0-2=model:v1/rev-a/-"

	t.Run("the rest is patched by the next round", func(t *testing.T) {
		h, _ := inPlaceHarness(t)
		h.settleContainers()
		h.retarget("rev-b")
		h.cluster.BeforeWrite = rejectPatchOf("llama-0-prefill-0-1")
		if _, err := h.round(); err == nil || h.podFacts() != half {
			t.Fatalf("round error = %v, Pods %s; want the failed patch and one Pod patched", err, h.podFacts())
		}
		if h.isReady() || h.conditions() != "ReconcileFailed=Error" {
			t.Errorf("ready = %t, conditions = %q", h.isReady(), h.conditions())
		}
		// A new process: nothing but the Pods says that an update was started.
		h.restart()
		h.cluster.BeforeWrite = nil
		h.cluster.ResetWrites()
		writes := h.mustRound()
		// The last write takes back the failure the round before reported.
		want := []string{
			"update servinggroups/status llama-0", "patch pods llama-0-prefill-0-1", "patch pods llama-0-prefill-0-2",
			"update servinggroups/status llama-0",
		}
		if !reflect.DeepEqual(writes, want) || h.podFacts() != patchedV2 {
			t.Fatalf("writes = %v, Pods %s", writes, h.podFacts())
		}
		h.settleContainers()
		if got := h.podFacts(); got != allV2 {
			t.Errorf("after the update: %s", got)
		}
	})

	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.RoleRecreate, workloadv1alpha1.NoneRestartPolicy} {
		t.Run("past the deadline, "+string(policy), func(t *testing.T) {
			h, _ := inPlaceHarness(t, recoveryPolicy(policy))
			h.settleContainers()
			h.retarget("rev-b")
			h.cluster.BeforeWrite = rejectPatchOf("llama-0-prefill-0-1")
			_, _ = h.round()
			h.cluster.Advance(601 * time.Second)
			h.cluster.ResetWrites()
			_, err := h.round()

			if policy == workloadv1alpha1.NoneRestartPolicy {
				// The patch is tried again, and no Pod is deleted.
				if err == nil || h.podFacts() != half {
					t.Errorf("round error = %v, Pods %s; want the patch tried again and the Pods kept", err, h.podFacts())
				}
				return
			}
			// The Pods that were never patched are healthy. They go with the
			// instance all the same: the update cannot finish without them.
			// The last write takes back the failure of the patch that was rejected.
			want := []string{
				"update servinggroups/status llama-0", "delete pods llama-0-prefill-0-0", "delete pods llama-0-prefill-0-1", "delete pods llama-0-prefill-0-2",
				"update servinggroups/status llama-0",
			}
			if writes := writesOf(h.cluster.Writes()); err != nil || !reflect.DeepEqual(writes, want) {
				t.Fatalf("round = %v, writes = %v, want %v", err, writes, want)
			}
			h.cluster.BeforeWrite = nil
			h.settleContainers()
			if got := h.podFacts(); got != "prefill-0-0=model:v2/rev-b/- prefill-0-1=model:v2/rev-b/- prefill-0-2=model:v2/rev-b/-" {
				t.Errorf("after the recovery: %s", got)
			}
		})
	}
}

// The revision of a group can change again while its Pods are being updated.
// A new revision is patched over the unfinished update; the revision labels
// never name a revision the Pods did not run.
func TestTheTargetMovesOnDuringAnUpdate(t *testing.T) {
	h, recorder := inPlaceHarness(t)
	h.settleContainers()
	before := h.uids()
	h.retarget("rev-b")
	h.mustRound()
	h.run("llama-0-prefill-0-0")

	h.retarget("rev-c")
	writes := h.mustRound()
	if want := append([]string{"update servinggroups/status llama-0"}, patchAll...); !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	want := "prefill-0-0=model:v3/rev-a/updating:rev-c prefill-0-1=model:v3/rev-a/updating:rev-c prefill-0-2=model:v3/rev-a/updating:rev-c"
	if got := h.podFacts(); got != want {
		t.Fatalf("patched again: %s", got)
	}
	h.settleContainers()
	want = "prefill-0-0=model:v3/rev-c/done:rev-c prefill-0-1=model:v3/rev-c/done:rev-c prefill-0-2=model:v3/rev-c/done:rev-c"
	if got := h.podFacts(); got != want || !reflect.DeepEqual(h.uids(), before) || h.conditions() != "Ready=AllInstancesReady" {
		t.Fatalf("after the update: %s, conditions %q", got, h.conditions())
	}
	for _, event := range updateEventsOf(recorder) {
		if strings.Contains(event, "revision rev-b completed") {
			t.Errorf("the update that was abandoned is reported as completed: %s", event)
		}
	}

	// A crash after the update is a fault again: the restarts the two patches
	// allowed for and that did not happen are not kept as an allowance.
	h.setPodStatus("llama-0-prefill-0-1", func(status *corev1.PodStatus) {
		status.Conditions[0].Status = corev1.ConditionFalse
		status.ContainerStatuses[0].RestartCount++
	})
	h.mustRound()
	if got := h.ledger(); got != "prefill-0:Draining/0" {
		t.Errorf("ledger after a crash = %q, want the instance drained", got)
	}
}

// When the revision goes back to the one the Pods are labelled with after
// their images were patched, the Pods are up to date by their labels and run
// another image. The group asks to be recreated.
func TestARevertDuringAnUpdateRecreatesTheGroup(t *testing.T) {
	h, recorder := inPlaceHarness(t)
	h.settleContainers()
	h.retarget("rev-b")
	h.mustRound()
	updateEventsOf(recorder)

	h.retarget("rev-a")
	if writes := h.mustRound(); !reflect.DeepEqual(writes, []string{"update servinggroups/status llama-0"}) {
		t.Fatalf("writes = %v, want the status alone", writes)
	}
	if got := h.conditions(); got != "RecreateRequired=InPlaceUpdateNotPossible" || h.podFacts() != patchedV2 {
		t.Errorf("conditions = %q, Pods %s", got, h.podFacts())
	}
	if events := updateEventsOf(recorder); len(events) != 0 {
		t.Errorf("events = %v, want none: the update was abandoned, it did not fall back", events)
	}

	// A group that was retargeted and back before anything was patched has
	// nothing to undo.
	h, _ = inPlaceHarness(t)
	h.settleContainers()
	h.retarget("rev-b")
	h.retarget("rev-a")
	h.settleContainers()
	if got := h.conditions(); got != "Ready=AllInstancesReady" || h.podFacts() != allV1 {
		t.Errorf("retargeted and back: conditions %q, Pods %s", got, h.podFacts())
	}
}

// Under RoleRollingUpdate the same holds for one Role instance: it is drained
// and created again from the target.
func TestARevertDuringAnUpdateRecreatesTheRoleInstance(t *testing.T) {
	h, _ := inPlaceHarness(t, rolledByRole)
	h.settleContainers()
	before := h.uids()
	h.retarget("rev-b")
	h.mustRound()

	h.retarget("rev-a")
	writes := h.mustRound()
	want := []string{"update servinggroups/status llama-0", "delete pods llama-0-prefill-2-0", "delete pods llama-0-prefill-2-1"}
	if !reflect.DeepEqual(writes, want) {
		t.Fatalf("writes = %v, want %v", writes, want)
	}
	h.settleContainers()
	for _, pod := range h.pods() {
		recreated := strings.HasPrefix(pod.Name, "llama-0-prefill-2-")
		if replaced := string(pod.UID) != before[pod.Name]; replaced != recreated || pod.Spec.Containers[0].Image != "model:v1" || inplace.StateOf(pod) != nil {
			t.Errorf("Pod %s: replaced = %t, want %t; %s", pod.Name, replaced, recreated, h.podFacts())
		}
	}
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}
}

// The cache can be behind the API server at every step of an update. What is
// patched is decided on a direct read.
func TestAnUpdateIsNotDecidedOnAStaleCache(t *testing.T) {
	patches := func(h *harness) []string {
		var out []string
		for _, write := range writesOf(h.cluster.Writes()) {
			if strings.HasPrefix(write, "patch pods") {
				out = append(out, write)
			}
		}
		return out
	}

	t.Run("the cache does not show the patches yet", func(t *testing.T) {
		h, _ := inPlaceHarness(t)
		h.settleContainers()
		h.retarget("rev-b")
		stale := h.cluster.Snapshot()
		h.mustRound()
		h.cluster.ResetWrites()
		if _, err := h.roundOn(stale); err != nil {
			t.Fatal(err)
		}
		if got := patches(h); len(got) != 0 || h.podFacts() != patchedV2 {
			t.Errorf("patches = %v, Pods %s", got, h.podFacts())
		}
	})

	t.Run("the cache shows a revision the group does not have any more", func(t *testing.T) {
		h, _ := inPlaceHarness(t)
		h.settleContainers()
		h.retarget("rev-b")
		stale := h.cluster.Snapshot()
		h.retarget("rev-a")
		if _, err := h.roundOn(stale); err != nil {
			t.Fatal(err)
		}
		if got := patches(h); len(got) != 0 || h.podFacts() != allV1 {
			t.Errorf("patches = %v, Pods %s", got, h.podFacts())
		}
	})

	t.Run("the cache shows a Pod ready that has crashed since", func(t *testing.T) {
		h, _ := inPlaceHarness(t, func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](60)
		})
		h.settleContainers()
		h.retarget("rev-b")
		h.mustRound()
		h.runAll()
		stale := h.cluster.Snapshot()
		h.setPodStatus("llama-0-prefill-0-1", func(status *corev1.PodStatus) {
			status.Conditions[0].Status = corev1.ConditionFalse
			status.ContainerStatuses[0].RestartCount++
		})
		h.cluster.ResetWrites()
		if _, err := h.roundOn(stale); err != nil {
			t.Fatal(err)
		}
		if got := patches(h); len(got) != 0 || h.podFacts() != patchedV2 {
			t.Errorf("the labels were changed on the word of a stale cache: patches %v, Pods %s", got, h.podFacts())
		}
	})
}

// A member that is lost during an update is handled by the recovery policy.
// The labels change only when the unit has all its members again and each of
// them runs the new image.
func TestAMemberLostDuringAnUpdate(t *testing.T) {
	tests := []struct {
		policy workloadv1alpha1.RecoveryPolicy
		want   string
	}{
		// The member is created again and takes part in the update.
		{policy: workloadv1alpha1.NoneRestartPolicy, want: allV2},
		// The instance is created again, from the revision of the group.
		{policy: workloadv1alpha1.RoleRecreate, want: "prefill-0-0=model:v2/rev-b/- prefill-0-1=model:v2/rev-b/- prefill-0-2=model:v2/rev-b/-"},
	}
	for _, tt := range tests {
		t.Run(string(tt.policy), func(t *testing.T) {
			h, _ := inPlaceHarness(t, recoveryPolicy(tt.policy))
			h.settleContainers()
			h.retarget("rev-b")
			h.mustRound()
			h.run("llama-0-prefill-0-0")
			h.run("llama-0-prefill-0-2")
			h.lose("llama-0-prefill-0-1")

			// The two that are left run the new image; that is not the unit.
			h.mustRound()
			for _, pod := range h.pods() {
				if pod.Labels[workloadv1alpha1.RevisionLabelKey] == "rev-b" && inplace.StateOf(pod) != nil {
					t.Fatalf("the labels changed while a member was missing: %s", h.podFacts())
				}
			}
			h.settleContainers()
			if got := h.podFacts(); got != tt.want || h.conditions() != "Ready=AllInstancesReady" {
				t.Errorf("after the recovery: %s, conditions %q; want %s", got, h.conditions(), tt.want)
			}
		})
	}
}

// A crash on top of the restart of the update is a fault at once: it does not
// wait for the deadline. Where no Pod is ever deleted for a fault, a Pod that
// crashed and came up on the new image finishes the update.
func TestACrashDuringAnUpdate(t *testing.T) {
	crashOnTarget := func(h *harness, ready corev1.ConditionStatus) {
		h.run("llama-0-prefill-0-1")
		h.setPodStatus("llama-0-prefill-0-1", func(status *corev1.PodStatus) {
			status.Conditions[0].Status = ready
			status.ContainerStatuses[0].RestartCount++
			status.ContainerStatuses[0].ContainerID += "-again"
		})
	}

	h, _ := inPlaceHarness(t)
	h.settleContainers()
	h.retarget("rev-b")
	h.mustRound()
	h.run("llama-0-prefill-0-0")
	h.run("llama-0-prefill-0-2")
	crashOnTarget(h, corev1.ConditionFalse)
	h.mustRound()
	if got := h.ledger(); got != "prefill-0:Draining/0" {
		t.Errorf("RoleRecreate: ledger = %q, want the instance drained", got)
	}

	h, _ = inPlaceHarness(t, recoveryPolicy(workloadv1alpha1.NoneRestartPolicy))
	h.settleContainers()
	h.retarget("rev-b")
	h.mustRound()
	h.run("llama-0-prefill-0-0")
	h.run("llama-0-prefill-0-2")
	crashOnTarget(h, corev1.ConditionTrue)
	h.mustRound()
	h.mustRound()
	if got := h.podFacts(); got != allV2 || h.conditions() != "Ready=AllInstancesReady" {
		t.Errorf("None: %s, conditions %q; want the update finished", got, h.conditions())
	}
}

// A round that cannot make a plan still reports what it sees. A group whose
// Pods are patched and not relabelled is in the middle of an update, whatever
// its Pods report: saying that it is ready would free its share of the budget.
func TestAGroupInTheMiddleOfAnUpdateIsNotReadyWithoutAPlan(t *testing.T) {
	h, _ := inPlaceHarness(t, rolledByRole)
	h.settleContainers()
	h.retarget("rev-b")
	h.mustRound()
	h.runAll()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].MaxUnavailable = ptr.To(intstr.FromString("many"))
	})

	if _, err := h.round(); err == nil {
		t.Fatal("round succeeded with a budget that cannot be resolved")
	}
	status := h.group().Status
	if h.isReady() || status.ReadyPods != 7 || h.conditions() != "ReconcileFailed=Error" {
		t.Errorf("ready = %t, readyPods = %d, conditions = %q; want a group that is not ready although its 7 Pods are", h.isReady(), status.ReadyPods, h.conditions())
	}
}

// inPlaceStopPoints runs an in-place update, with a change of mind in the
// middle, with a controller that dies when it is about to make its stopAt-th
// write (never for stopAt 0). See stopPoints.
func inPlaceStopPoints(t *testing.T, mutate func(*workloadv1alpha1.ModelServing), stopAt int, staleCache bool) (state string, writes int) {
	t.Helper()
	var mutations []func(*workloadv1alpha1.ModelServing)
	if mutate != nil {
		mutations = append(mutations, mutate)
	}
	h, _ := inPlaceHarness(t, mutations...)
	inRound, dead := false, false
	h.cluster.BeforeWrite = func(apitest.Write) error {
		if !inRound {
			return nil
		}
		if dead {
			return errCrash
		}
		writes++
		if writes == stopAt {
			dead = true
			return errCrash
		}
		return nil
	}
	// settle runs rounds until one writes nothing. With kubelet set, the
	// containers whose image changed are restarted in between.
	settle := func(kubelet bool) {
		t.Helper()
		for range 60 {
			changed := 0
			if kubelet {
				changed = h.runAll()
			}
			h.cluster.Settle()
			before := h.cluster.Snapshot()
			accepted := len(h.cluster.Writes())
			inRound = true
			_, err := h.round()
			inRound = false
			if dead {
				dead = false
				h.restart()
				if staleCache {
					inRound = true
					_, _ = h.roundOn(before)
					inRound = false
				}
				continue
			}
			if err != nil {
				t.Fatalf("stop at write %d: round: %v", stopAt, err)
			}
			if len(h.cluster.Writes()) == accepted && changed == 0 {
				return
			}
		}
		t.Fatalf("stop at write %d: the group did not settle", stopAt)
	}

	settle(true)
	uids := h.uids()
	// The images are patched for rev-b; before a container restarts the
	// revision becomes rev-c.
	h.retarget("rev-b")
	settle(false)
	h.retarget("rev-c")
	settle(true)

	if after := h.uids(); !reflect.DeepEqual(after, uids) {
		t.Errorf("stop at write %d: Pods %v, before the update %v", stopAt, after, uids)
	}
	status := h.group().Status
	return fmt.Sprintf("pods[%s] ledger[%s] conditions[%s] revision=%s ready=%d", h.podFacts(), h.ledger(), h.conditions(), status.Revision, status.ReadyPods), writes
}

// Whatever write a controller dies before, the one that takes over finishes
// the in-place update: what was patched, for which revision and since when is
// read from the Pods.
func TestEveryStopPointOfAnInPlaceUpdateEndsInTheSameState(t *testing.T) {
	for name, mutate := range map[string]func(*workloadv1alpha1.ModelServing){"the group as a whole": nil, "Role by Role": rolledByRole} {
		want, total := inPlaceStopPoints(t, mutate, 0, false)
		if total < 10 || !strings.Contains(want, "/rev-c/done:rev-c") || strings.Contains(want, "updating") {
			t.Fatalf("%s: the scenario made %d writes and ended in %s; it does not exercise the update", name, total, want)
		}
		for _, stale := range []bool{false, true} {
			for stopAt := 1; stopAt <= total; stopAt++ {
				if got, _ := inPlaceStopPoints(t, mutate, stopAt, stale); got != want {
					t.Errorf("%s, stop at write %d of %d, stale cache %t:\n got %s\nwant %s", name, stopAt, total, stale, got, want)
				}
			}
		}
		t.Logf("%s: %d stop points; final state %s", name, total, want)
	}
}

// A group whose revision is not the one the rollout moves to is a group of an
// earlier revision: the ModelServing controller moves it to the target, or
// replaces it, within the budget of the rollout, or protects it. Until then
// the group starts nothing and does not ask to be recreated, whatever
// revisions its Pods run. A Pod whose images were patched finishes its own
// update and gets the label of what it runs.
func TestAGroupThatIsNotAtTheTargetLeavesItsPodsAlone(t *testing.T) {
	for _, method := range []workloadv1alpha1.UpdateMethod{workloadv1alpha1.InPlaceUpdateMethod, workloadv1alpha1.RecreateUpdateMethod} {
		t.Run(string(method), func(t *testing.T) {
			h, recorder := inPlaceHarness(t)
			h.settleContainers()
			before := h.uids()
			// One Pod of three is patched for rev-b when the target moves on.
			h.retarget("rev-b")
			h.cluster.BeforeWrite = func(write apitest.Write) error {
				if write.Verb == "patch" && write.Name == "llama-0-prefill-0-1" {
					return errors.New("the patch is rejected")
				}
				return nil
			}
			if _, err := h.round(); err == nil {
				t.Fatal("the round with a rejected patch succeeded")
			}
			h.cluster.BeforeWrite = nil
			h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
				group.Spec.TargetRevision = "rev-c"
				group.Spec.RolloutStrategy.UpdateMethod = method
			})
			h.cluster.ResetWrites()
			updateEventsOf(recorder)

			writes := h.mustRound()
			for _, write := range writes {
				if !strings.HasPrefix(write, "update servinggroups/status ") {
					t.Fatalf("writes = %v, want the status alone: the patched Pod has not restarted", writes)
				}
			}
			h.settleContainers()
			const held = "prefill-0-0=model:v2/rev-b/done:rev-b prefill-0-1=model:v1/rev-a/- prefill-0-2=model:v1/rev-a/-"
			if got := h.podFacts(); got != held || !reflect.DeepEqual(h.uids(), before) {
				t.Fatalf("Pods = %s, want %s and no Pod replaced", got, held)
			}
			if got := h.conditions(); got != "Ready=AllInstancesReady" {
				t.Errorf("conditions = %q, want the group ready and not asking to be recreated", got)
			}
			if events := updateEventsOf(recorder); hasEvent(events, ReasonInPlaceUpdateCompleted) || hasEvent(events, ReasonInPlaceUpdateFallback) {
				t.Errorf("events = %v, want none about an update of the group", events)
			}

			if method != workloadv1alpha1.InPlaceUpdateMethod {
				return
			}
			// The ModelServing controller moves the group to the target.
			h.retarget("rev-c")
			h.settleContainers()
			const moved = "prefill-0-0=model:v3/rev-c/done:rev-c prefill-0-1=model:v3/rev-c/done:rev-c prefill-0-2=model:v3/rev-c/done:rev-c"
			if got := h.podFacts(); got != moved || !reflect.DeepEqual(h.uids(), before) || h.conditions() != "Ready=AllInstancesReady" {
				t.Errorf("after the move: %s, conditions %q", got, h.conditions())
			}
		})
	}
}
