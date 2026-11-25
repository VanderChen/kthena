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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
)

// hookPlugin is a plugin whose hooks fail on demand and count their calls.
type hookPlugin struct {
	syncErr, deleteErr, createErr error
	// proceed is what a failing OnModelServingSync says about continuing, and
	// retry when it asks to be called again.
	proceed bool
	retry   time.Duration
	deletes []string
}

func (p *hookPlugin) Name() string { return "hook" }
func (p *hookPlugin) OnModelServingSync(context.Context, *plugins.HookRequest) plugins.HookResult {
	return plugins.HookResult{Err: p.syncErr, Continue: p.proceed, RequeueAfter: p.retry}
}
func (p *hookPlugin) OnPodCreate(context.Context, *plugins.HookRequest) error { return p.createErr }
func (p *hookPlugin) OnPodReady(context.Context, *plugins.HookRequest) error  { return nil }
func (p *hookPlugin) OnRoleSync(context.Context, *plugins.HookRequest) error  { return nil }
func (p *hookPlugin) OnRoleDelete(_ context.Context, req *plugins.HookRequest) error {
	p.deletes = append(p.deletes, req.RoleID)
	return p.deleteErr
}

// withHooks returns a harness whose ModelServing runs the hook plugin.
func withHooks(t *testing.T, plugin *hookPlugin, mutate ...func(*workloadv1alpha1.ModelServing)) *harness {
	t.Helper()
	ms := testModelServing(append(mutate, func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "hook", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	})...)
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
	h.registry = plugins.NewRegistry()
	h.registry.Register("hook", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return plugin, nil })
	h.restart()
	return h
}

// roundWrites runs one round, which may fail, and returns its writes.
func (h *harness) roundWrites() ([]string, error) {
	h.t.Helper()
	before := len(h.cluster.Writes())
	_, err := h.round()
	return writesOf(h.cluster.Writes()[before:]), err
}

// A round that fails the way the round before it did leaves the status as it
// is. A status that changed with every failed round would start the next
// round by its own write, and the delays of the queue would never apply.
func TestARepeatedFailureWritesNothing(t *testing.T) {
	ms := testModelServing()
	squatter := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "llama-0-prefill-0-1"}}
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), squatter)

	if _, err := h.round(); !errors.Is(err, errNameOccupied) {
		t.Fatalf("round error = %v, want the occupied name", err)
	}
	// The round after it reports the member that was created before the
	// failure; from then on nothing changes.
	if _, err := h.round(); !errors.Is(err, errNameOccupied) {
		t.Fatalf("round error = %v, want the occupied name again", err)
	}
	reported := h.group().Status.DeepCopy()
	h.cluster.ResetWrites()
	for i := range 5 {
		writes, err := h.roundWrites()
		if !errors.Is(err, errNameOccupied) {
			t.Fatalf("round %d: error = %v, want the occupied name again", i, err)
		}
		if len(writes) != 0 {
			t.Fatalf("round %d wrote %v", i, writes)
		}
	}
	if got := h.group().Status; !reflect.DeepEqual(got.Conditions, reported.Conditions) {
		t.Errorf("conditions changed without a change of the failure: %v, were %v", got.Conditions, reported.Conditions)
	}

	// Once the name is free the failure is taken back.
	if err := h.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), squatter.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions after the name was freed = %q", got)
	}
}

// An instance whose delete hook fails stays drained and is not created again.
// The round has to say so by failing: no event would bring the next one.
func TestAFailingDeleteHookIsRetried(t *testing.T) {
	plugin := &hookPlugin{deleteErr: errors.New("the hook is down")}
	h := withHooks(t, plugin)
	h.settle()
	h.lose("llama-0-prefill-0-1")
	h.mustRound() // the instance is drained
	h.cluster.Settle()
	h.cluster.ResetWrites()

	if _, err := h.round(); err == nil || !strings.Contains(err.Error(), "the hook is down") {
		t.Fatalf("round error = %v, want the failure of the hook", err)
	}
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Draining/0 DrainBlocked=RoleDeleteHookFailed" {
		t.Errorf("after the hook failed: %q", got)
	}
	// It fails again, and writes nothing while it does.
	for i := range 3 {
		writes, err := h.roundWrites()
		if err == nil || len(writes) != 0 {
			t.Fatalf("round %d: error = %v, writes = %v; want the failure again and no write", i, err, writes)
		}
	}
	if got := h.podSummary(); got != "" {
		t.Errorf("Pods were created before the hook succeeded: %s", got)
	}

	plugin.deleteErr = nil
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/1 Ready=AllInstancesReady" {
		t.Errorf("after the hook succeeded: %q", got)
	}
	if len(plugin.deletes) < 5 {
		t.Errorf("the hook was called %d times, want once per round", len(plugin.deletes))
	}
}

// Without its PodGroup a round creates nothing. What it deletes it still
// deletes: a group that has failed is emptied so that it can be replaced.
func TestAFailedGroupIsDrainedWithoutItsPodGroup(t *testing.T) {
	h := standard(t, recoveryPolicy(workloadv1alpha1.ServingGroupRecreate))
	h.podGroups.enabled = true
	h.settle()
	h.lose("llama-0-prefill-0-1")
	h.mustRound() // the group fails
	h.crash("llama-0-prefill-0-2")
	h.cluster.ResetWrites()

	h.podGroups.err = podgroupmanager.ErrNameOccupied
	writes, err := h.roundWrites()
	if err != nil {
		t.Fatalf("round: %v; want a retry later, as the PodGroup manager asks", err)
	}
	if !hasWrite(writes, "delete pods llama-0-prefill-0-2") {
		t.Errorf("writes = %v, want the faulty member deleted", writes)
	}
	h.cluster.Settle()
	h.mustRound()
	if got := h.conditions(); got != "Drained=NoFaultyMember Failed=MemberLost ReconcileFailed=NameOccupied" {
		t.Errorf("conditions = %q", got)
	}
}

func hasWrite(writes []string, want string) bool {
	for _, write := range writes {
		if write == want {
			return true
		}
	}
	return false
}

// The members a group has committed to stay in its count while the cache does
// not show them, also in a round that cannot create for want of its PodGroup:
// a count of zero would tell the ModelServing controller that the group has no
// member and may be given another revision.
func TestTheCommitmentSurvivesARoundWithoutThePodGroup(t *testing.T) {
	h := standard(t)
	h.podGroups.enabled = true
	before := h.cluster.Snapshot()
	h.mustRound() // commits to three members and creates them
	if pods := h.group().Status.Pods; pods != 3 {
		t.Fatalf("pods = %d after the creation", pods)
	}

	// The group is current in the cache; the Pods are not there yet.
	stale := h.cluster.Snapshot()
	stale.Pods = before.Pods
	h.podGroups.err = errors.New("the PodGroup cannot be synced")
	if _, err := h.roundOn(stale); err != nil {
		t.Fatalf("round: %v; want a retry later", err)
	}
	if pods := h.group().Status.Pods; pods != 3 {
		t.Errorf("pods = %d, want the commitment kept", pods)
	}
	if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0" {
		t.Errorf("Pods = %q", got)
	}
}

// That an instance is drained is read from the API server. On a cache that
// does not show its Pods yet, the next generation would be committed while
// the Pods of this one exist, and they would be taken for its members.
func TestADrainDoesNotEndOnAStaleCache(t *testing.T) {
	h := standard(t)
	h.settle()
	empty := h.cluster.Snapshot() // taken now for its Pod cache, emptied below
	h.cluster.HoldPodTermination(true)
	h.lose("llama-0-prefill-0-1")
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-1")
	// The decision to drain is written, and then the process dies: the two
	// other Pods are not deleted.
	h.cluster.BeforeWrite = func(write apitest.Write) error {
		if write.Verb == "delete" {
			return errors.New("the process died")
		}
		return nil
	}
	if _, err := h.round(); err == nil {
		t.Fatal("the round was to fail at the first deletion")
	}
	h.cluster.BeforeWrite = nil
	if got := h.ledger() + " | " + h.podSummary(); got != "prefill-0:Draining/0 | prefill-0-0@0 prefill-0-2@0" {
		t.Fatalf("after the crash: %s", got)
	}

	// A new process sees the group as it is and no Pod of it.
	h.restart()
	stale := h.cluster.Snapshot()
	for _, obj := range empty.Pods.List() {
		_ = empty.Pods.Delete(obj)
	}
	stale.Pods = empty.Pods
	h.cluster.ResetWrites()
	if _, err := h.roundOn(stale); err != nil {
		t.Fatal(err)
	}
	if got := h.ledger(); got != "prefill-0:Draining/0" {
		t.Errorf("ledger = %q: the drain ended although the Pods exist", got)
	}
	// The round went on with the drain instead.
	if writes := writesOf(h.cluster.Writes()); !hasWrite(writes, "delete pods llama-0-prefill-0-0") || !hasWrite(writes, "delete pods llama-0-prefill-0-2") {
		t.Errorf("writes = %v, want the remaining Pods of the instance deleted", writes)
	}
}

// A Pod the group controls that lost the labels of a member is in no cache.
// Creating the member fails on its name. That is reported; taking the Pod for
// the member would leave the instance waiting for a Pod that never shows up.
func TestAMemberWithoutItsLabelsIsReported(t *testing.T) {
	h := standard(t, recoveryPolicy(workloadv1alpha1.NoneRestartPolicy))
	h.settle()
	pod, err := h.cluster.GetPod(testNamespace, "llama-0-prefill-0-1")
	if err != nil {
		t.Fatal(err)
	}
	delete(pod.Labels, workloadv1alpha1.GroupNameLabelKey)
	if _, err := h.cluster.Kube.CoreV1().Pods(testNamespace).Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	// What the filtered informer and the list by label show.
	visible := h.cluster.Snapshot()
	for _, obj := range visible.Pods.List() {
		if obj.(*corev1.Pod).Name == pod.Name {
			_ = visible.Pods.Delete(obj)
		}
	}

	_, err = h.roundOn(visible)
	if !errors.Is(err, errNameOccupied) || !strings.Contains(err.Error(), "labels of a member") {
		t.Fatalf("round error = %v, want the Pod without its labels reported", err)
	}
	if got := h.conditions(); got != "ReconcileFailed=NameOccupied" {
		t.Errorf("conditions = %q", got)
	}
}

// A ServingGroup under a name that is not <ModelServing>-<ordinal> is not a
// group of the ModelServing it points at, and gets no Pods.
func TestAGroupUnderAnotherNameIsLeftAlone(t *testing.T) {
	ms := testModelServing()
	group := groupOf(ms, "rev-a")
	group.Name = "llama-extra"
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), group)
	h.useSnapshot(h.cluster.Snapshot())
	if _, err := h.controller.Reconcile(context.Background(), testNamespace, group.Name); err != nil {
		t.Fatal(err)
	}
	if writes := writesOf(h.cluster.Writes()); len(writes) != 0 {
		t.Errorf("writes = %v", writes)
	}
}

// A hook of the ModelServing that fails and does not ask to continue stops
// the groups as well: nothing is created, deleted or patched.
func TestABlockingModelServingHookStopsTheGroup(t *testing.T) {
	plugin := &hookPlugin{syncErr: errors.New("not now")}
	h := withHooks(t, plugin)

	writes, err := h.roundWrites()
	if err == nil || !strings.Contains(err.Error(), "not now") {
		t.Fatalf("round error = %v, want the failure of the hook", err)
	}
	if want := []string{"update servinggroups/status llama-0"}; !reflect.DeepEqual(writes, want) || h.conditions() != "ReconcileFailed=Error" {
		t.Errorf("writes = %v, conditions = %q; want the failure reported and nothing else", writes, h.conditions())
	}
	if writes, _ := h.roundWrites(); len(writes) != 0 {
		t.Errorf("the same failure again wrote %v", writes)
	}

	// A hook that fails and asks to continue does not stop anything.
	plugin.proceed = true
	h.settle()
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}

	// Neither does a group that exists lose anything to a blocking hook.
	plugin.proceed = false
	h.lose("llama-0-prefill-0-1")
	if writes, err := h.roundWrites(); err == nil || hasWrite(writes, "delete pods llama-0-prefill-0-0") {
		t.Errorf("round error = %v, writes = %v; want no deletion while the hook blocks", err, writes)
	}
}

// The phases of a Role instance are reported on the ModelServing, as a
// version without group objects did.
func TestRoleInstancePhasesAreReported(t *testing.T) {
	ms := testModelServing()
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
	recorder := record.NewFakeRecorder(100)
	h.recorder = recorder
	h.restart()

	h.settle()
	h.lose("llama-0-prefill-0-1")
	h.settle()
	var got []string
	for _, event := range eventsOf(recorder) {
		if strings.Contains(event, " Role") {
			got = append(got, event)
		}
	}
	want := []string{
		"Normal RoleCreating Role prefill/prefill-0 in ServingGroup llama-0 is now Creating",
		"Normal RoleRunning Role prefill/prefill-0 in ServingGroup llama-0 is now Running",
		"Normal RoleDeleting Role prefill/prefill-0 in ServingGroup llama-0 is now Deleting",
		"Normal RoleCreating Role prefill/prefill-0 in ServingGroup llama-0 is now Creating",
		"Normal RoleRunning Role prefill/prefill-0 in ServingGroup llama-0 is now Running",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("events:\n got %q\nwant %q", got, want)
	}
}

// What the process remembers about a group is dropped with it, whichever way
// it went: a group deleted in the background is never seen terminating.
func TestAGroupThatIsGoneIsForgotten(t *testing.T) {
	h := standard(t)
	h.settle()
	c := h.controller
	if len(c.known) != 1 || c.readyNotified.Len() != 3 {
		t.Fatalf("before: known = %d, notified = %d", len(c.known), c.readyNotified.Len())
	}

	runner := NewRunner(c)
	defer runner.queue.ShutDown()
	runner.forgetGroup(h.group())
	if len(c.known) != 0 || c.readyNotified.Len() != 0 {
		t.Errorf("after: known = %d, notified = %d", len(c.known), c.readyNotified.Len())
	}
}

// Every group is given a round when the rollback switch is removed.
func TestEnqueueAll(t *testing.T) {
	h := standard(t)
	h.useSnapshot(h.cluster.Snapshot())
	runner := NewRunner(h.controller)
	defer runner.queue.ShutDown()
	runner.EnqueueAll()
	if runner.queue.Len() != 1 {
		t.Fatalf("%d keys are queued, want the one group", runner.queue.Len())
	}
	if key, _ := runner.queue.Get(); key != testNamespace+"/"+testGroup {
		t.Errorf("queued %q", key)
	}
}

// roleEventsOf returns the events about the states of Role instances that
// were recorded since the last call.
func roleEventsOf(recorder *record.FakeRecorder) []string {
	var out []string
	for _, event := range eventsOf(recorder) {
		if fields := strings.SplitN(event, " ", 3); len(fields) == 3 && strings.HasPrefix(fields[1], "Role") {
			out = append(out, fields[1])
		}
	}
	return out
}

// A Role instance is reported as Running when every Pod of it is ready, not
// when its Pods exist, and as Creating again when a Pod stops being ready. A
// state is reported when it is entered, not in every round that finds it. A
// new process does not report the instances it finds running again.
func TestRoleStatesFollowTheReadinessOfThePods(t *testing.T) {
	ms := testModelServing()
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
	recorder := record.NewFakeRecorder(100)
	h.recorder = recorder
	h.restart()
	expect := func(step string, want ...string) {
		t.Helper()
		if got := roleEventsOf(recorder); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: events %q, want %q", step, got, want)
		}
	}
	rounds := func() {
		t.Helper()
		for range 3 {
			h.cluster.Settle()
			h.mustRound()
		}
	}

	rounds()
	expect("the Pods exist and are not ready", "RoleCreating")
	h.readyAll()
	rounds()
	expect("every Pod is ready", "RoleRunning")
	h.setPodStatus("llama-0-prefill-0-1", func(status *corev1.PodStatus) { status.Conditions[0].Status = corev1.ConditionFalse })
	rounds()
	expect("a Pod is not ready any more", "RoleCreating")
	h.ready("llama-0-prefill-0-1")
	rounds()
	expect("ready again", "RoleRunning")

	h.restart()
	rounds()
	expect("a new process")
}

// An instance of which nothing exists and nothing is created is not reported
// as being created.
func TestNoRoleIsReportedAsCreatedWhileNothingIsCreated(t *testing.T) {
	ms := testModelServing()
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"))
	recorder := record.NewFakeRecorder(100)
	h.recorder = recorder
	h.restart()
	h.podGroups.enabled, h.podGroups.err = true, errors.New("the PodGroup cannot be synced")
	for range 3 {
		_, _ = h.round()
	}
	if got := roleEventsOf(recorder); len(got) != 0 || h.podSummary() != "" {
		t.Fatalf("events %q, Pods %q; want neither without the PodGroup", got, h.podSummary())
	}
	h.podGroups.err = nil
	h.mustRound()
	if got := roleEventsOf(recorder); !reflect.DeepEqual(got, []string{"RoleCreating"}) {
		t.Errorf("events %q, want the instance reported when its Pods are created", got)
	}
}

// A group whose ModelServing is being deleted, or is gone, is deleted by the
// garbage collector: that is no fault of the group and is not reported as
// one. A group that will stay without Pods is.
func TestOnlyAGroupThatStaysWithoutAnOwnerIsReported(t *testing.T) {
	ms := testModelServing()
	gone := groupOf(ms, "rev-a")
	gone.OwnerReferences[0].UID = "previous-ms-uid"
	stray := groupOf(ms, "rev-a")
	stray.Name = "llama-extra"
	unowned := groupOf(ms, "rev-a")
	unowned.OwnerReferences = nil
	deleting := testModelServing()
	deleting.DeletionTimestamp, deleting.Finalizers = ptr.To(metav1.Now()), []string{metav1.FinalizerDeleteDependents}

	for _, tt := range []struct {
		name   string
		ms     *workloadv1alpha1.ModelServing
		group  *workloadv1alpha1.ServingGroup
		events int
	}{
		{name: "the ModelServing is gone", ms: ms, group: gone},
		{name: "the ModelServing is being deleted", ms: deleting, group: groupOf(ms, "rev-a")},
		{name: "the name is not one of a group of the ModelServing", ms: ms, group: stray, events: 1},
		{name: "nothing controls the group", ms: ms, group: unowned, events: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, tt.ms, revisionOf(t, ms, "rev-a"), tt.group)
			recorder := record.NewFakeRecorder(10)
			h.recorder = recorder
			h.restart()
			h.useSnapshot(h.cluster.Snapshot())
			if _, err := h.controller.Reconcile(context.Background(), testNamespace, tt.group.Name); err != nil {
				t.Fatal(err)
			}
			events := eventsOf(recorder)
			if len(events) != tt.events || tt.events == 1 && !strings.HasPrefix(events[0], "Warning OwnerNotFound ") {
				t.Errorf("events = %q, want %d", events, tt.events)
			}
			if writes := writesOf(h.cluster.Writes()); len(writes) != 0 {
				t.Errorf("writes = %v", writes)
			}
		})
	}
}

// The delete hooks of the Role instances are called for a group that is
// deleted together with its members. A group that is deleted to release its
// members, which is how they are handed back to a version without group
// objects, keeps what belongs to them: the hooks would delete it.
func TestTheDeleteHooksAreForAGroupThatIsDeletedWithItsMembers(t *testing.T) {
	for _, tt := range []struct {
		name        string
		finalizer   string
		handingBack bool
		called      bool
	}{
		{name: "deleted in the foreground", finalizer: metav1.FinalizerDeleteDependents, called: true},
		{name: "deleted to release its members", finalizer: metav1.FinalizerOrphanDependents},
		{name: "deleted in the foreground while its workloads are handed back", finalizer: metav1.FinalizerDeleteDependents, handingBack: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			plugin := &hookPlugin{}
			ms := testModelServing(func(ms *workloadv1alpha1.ModelServing) {
				ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "hook", Type: workloadv1alpha1.PluginTypeBuiltIn}}
				if tt.handingBack {
					ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionRollingBack}
				}
			})
			group := groupOf(ms, "rev-a")
			group.DeletionTimestamp, group.Finalizers = ptr.To(metav1.Now()), []string{tt.finalizer}
			group.Status.RoleInstances = []workloadv1alpha1.RoleInstanceStatus{{Role: "prefill", Index: 0, Phase: workloadv1alpha1.RoleInstanceEstablished}}
			h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), group)
			h.registry = plugins.NewRegistry()
			h.registry.Register("hook", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return plugin, nil })
			h.restart()
			h.mustRound()
			if called := len(plugin.deletes) > 0; called != tt.called {
				t.Errorf("delete hooks called for %v, want called = %t", plugin.deletes, tt.called)
			}
		})
	}
}

// A round that cannot be planned keeps the number of members the group
// committed to: the cache may not show yet the Pods the round before created,
// and a count of zero would tell the ModelServing controller that the group
// has no member.
func TestTheCommitmentSurvivesARoundThatCannotBePlanned(t *testing.T) {
	plugin := &hookPlugin{}
	h := withHooks(t, plugin)
	h.mustRound()
	if got := h.group().Status.Pods; got != 3 || len(h.pods()) != 3 {
		t.Fatalf("after the round that creates: status.pods = %d, %d Pods", got, len(h.pods()))
	}
	// What a cache shows that has the status and not the Pods yet.
	behind := h.cluster.Snapshot()
	for _, obj := range behind.Pods.List() {
		_ = behind.Pods.Delete(obj)
	}
	plugin.syncErr = errors.New("not now")
	if _, err := h.roundOn(behind); err == nil {
		t.Fatal("the round with a blocking hook succeeded")
	}
	if got := h.group().Status.Pods; got != 3 || h.conditions() != "ReconcileFailed=Error" {
		t.Errorf("status.pods = %d, conditions = %q; want the 3 members still counted", got, h.conditions())
	}
}

// A hook that stops the round and says when to ask again is asked again
// then. The delays of the queue grow with every failure: the group would act
// long after the hook lets it.
func TestABlockingHookSaysWhenToAskAgain(t *testing.T) {
	plugin := &hookPlugin{syncErr: errors.New("not now"), retry: 30 * time.Second}
	h := withHooks(t, plugin)
	after, err := h.round()
	if err != nil || after != 30*time.Second {
		t.Fatalf("round = %v, %v; want the next round in 30s and no error for the queue to back off on", after, err)
	}
	if got := h.conditions(); got != "ReconcileFailed=Error" || h.podSummary() != "" {
		t.Errorf("conditions = %q, Pods = %q; want the failure reported and nothing created", got, h.podSummary())
	}
}

// What a version without group objects reported on the ModelServing as
// warnings is still reported there: a plugin that refuses a Pod, and a
// revision that cannot be resolved, once.
func TestWarningsAreRecordedOnTheModelServing(t *testing.T) {
	warnings := func(recorder *record.FakeRecorder) (out []string) {
		for _, event := range eventsOf(recorder) {
			if fields := strings.SplitN(event, " ", 3); fields[0] == "Warning" {
				out = append(out, fields[1])
			}
		}
		return out
	}
	t.Run("a plugin refuses a Pod", func(t *testing.T) {
		h := withHooks(t, &hookPlugin{createErr: errors.New("no")})
		recorder := record.NewFakeRecorder(10)
		h.recorder = recorder
		h.restart()
		if _, err := h.round(); err == nil {
			t.Fatal("the round succeeded")
		}
		if got := warnings(recorder); !reflect.DeepEqual(got, []string{ReasonPluginError}) {
			t.Errorf("warnings = %q", got)
		}
	})
	t.Run("the revision of the group cannot be resolved", func(t *testing.T) {
		ms := testModelServing()
		h := newHarness(t, ms, groupOf(ms, "rev-a"))
		recorder := record.NewFakeRecorder(10)
		h.recorder = recorder
		h.restart()
		h.mustRound()
		h.mustRound()
		if got := warnings(recorder); !reflect.DeepEqual(got, []string{ReasonRevisionUnresolved}) {
			t.Errorf("warnings = %q, want the revision reported once", got)
		}
	})
}
