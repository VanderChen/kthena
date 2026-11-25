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
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// hookRecorder is a plugin that writes every hook it is called with into a
// log: "<plugin> sync prefill-0@1" for a Role instance and its generation,
// "<plugin> create llama-0-prefill-0-0@1" for a Pod. Several recorders can
// share one log.
type hookRecorder struct {
	plugins.BasePlugin
	name string
	log  *[]string
	// syncErr is what OnRoleSync returns.
	syncErr error
	// onCreate, when set, is called by OnPodCreate.
	onCreate func(*plugins.HookRequest)
	// synced holds the last request of OnRoleSync for each Role instance.
	synced map[string]plugins.HookRequest
}

func newRecorder(name string, log *[]string) *hookRecorder {
	return &hookRecorder{name: name, log: log, synced: map[string]plugins.HookRequest{}}
}

func (p *hookRecorder) record(hook, subject string, generation int64) {
	*p.log = append(*p.log, fmt.Sprintf("%s %s %s@%d", p.name, hook, subject, generation))
}

func (p *hookRecorder) Name() string { return p.name }

func (p *hookRecorder) OnPodCreate(_ context.Context, req *plugins.HookRequest) error {
	p.record("create", req.Pod.Name, req.Generation)
	if p.onCreate != nil {
		p.onCreate(req)
	}
	return nil
}

func (p *hookRecorder) OnPodReady(_ context.Context, req *plugins.HookRequest) error {
	p.record("ready", req.Pod.Name, req.Generation)
	return nil
}

func (p *hookRecorder) OnRoleSync(_ context.Context, req *plugins.HookRequest) error {
	p.record("sync", req.RoleID, req.Generation)
	p.synced[req.RoleID] = *req
	return p.syncErr
}

func (p *hookRecorder) OnRoleDelete(_ context.Context, req *plugins.HookRequest) error {
	p.record("delete", req.RoleID, req.Generation)
	return nil
}

// hooked returns a harness whose registry holds the recorders.
func hooked(t *testing.T, objects []runtime.Object, recorders ...*hookRecorder) *harness {
	t.Helper()
	h := newHarness(t, objects...)
	h.registry = plugins.NewRegistry()
	for _, recorder := range recorders {
		h.registry.Register(recorder.name, func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return recorder, nil })
	}
	h.restart()
	return h
}

// uses makes the ModelServing run the plugins of these names.
func uses(names ...string) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Plugins = nil
		for _, name := range names {
			ms.Spec.Plugins = append(ms.Spec.Plugins, workloadv1alpha1.PluginSpec{Name: name, Type: workloadv1alpha1.PluginTypeBuiltIn})
		}
	}
}

func instancesOfRole(count int32) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].Replicas = ptr.To(count) }
}

// hooksOf returns the entries of the log about one hook, without the name of
// the hook, and empties the log: "rec prefill-0@0 rec prefill-1@0".
func hooksOf(log *[]string, hook string) string {
	var out []string
	for _, entry := range *log {
		if fields := strings.Fields(entry); fields[1] == hook {
			out = append(out, fields[0]+" "+fields[2])
		}
	}
	*log = nil
	return strings.Join(out, " ")
}

// finishTerminations removes every Pod that is terminating.
func (h *harness) finishTerminations() {
	h.t.Helper()
	for _, pod := range h.pods() {
		if pod.DeletionTimestamp != nil {
			if err := h.cluster.FinishTermination(testNamespace, pod.Name); err != nil {
				h.t.Fatal(err)
			}
		}
	}
}

// The Role sync hook is how a plugin keeps what belongs to a Role instance:
// the headless Services, for one. It is called in every round for each
// instance that is to exist, from the round that creates its first Pod on,
// with the generation of the instance. It is not called for an instance that
// is going, and not while one is drained to be created again: a plugin would
// make again what the delete hook is about to remove.
func TestTheRoleSyncHookIsForTheInstancesThatAreNotDrained(t *testing.T) {
	var log []string
	recorder := newRecorder("rec", &log)
	ms := testModelServing(instancesOfRole(2), uses("rec"))
	h := hooked(t, []runtime.Object{ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a")}, recorder)
	synced := func(when, want string) {
		t.Helper()
		h.mustRound()
		if got := hooksOf(&log, "sync"); got != want {
			t.Fatalf("%s: synced %q, want %q", when, got, want)
		}
	}

	synced("the round that creates the Pods", "rec prefill-0@0 rec prefill-1@0")
	h.settle()
	log = nil
	synced("at rest", "rec prefill-0@0 rec prefill-1@0")
	req := recorder.synced["prefill-1"]
	if req.ServingGroup != testGroup || req.RoleName != "prefill" || req.RoleIndex != 1 || req.KubeClient == nil ||
		req.Owner == nil || req.Owner.UID != "sg-uid" || req.Owner.Kind != workloadv1alpha1.ServingGroupKind.Kind ||
		req.Role == nil || req.Role.WorkerReplicas != 2 || req.ModelServing == nil || req.ModelServing.Name != "llama" {
		t.Errorf("request of the sync hook: %+v", req)
	}

	// An instance that is scaled away is not synced from the round that
	// decides so, while its Pods terminate and after.
	h.cluster.HoldPodTermination(true)
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 1 })
	synced("the round that scales in", "rec prefill-0@0")
	synced("while the instance terminates", "rec prefill-0@0")
	h.finishTerminations()
	synced("the round that forgets the instance", "rec prefill-0@0")
	if got := h.ledger(); got != "prefill-0:Established/0" {
		t.Fatalf("ledger after the scale-in = %q", got)
	}

	// An instance that lost a member is drained and created again.
	h.lose("llama-0-prefill-0-1")
	h.finishTerminations()
	h.mustRound()
	if got := h.ledger(); got != "prefill-0:Draining/0" {
		t.Fatalf("ledger after a member was lost = %q", got)
	}
	log = nil
	synced("while the instance is drained", "")
	h.finishTerminations()
	h.mustRound()
	if got := h.podSummary(); got != "prefill-0-0@1 prefill-0-1@1 prefill-0-2@1" {
		t.Fatalf("Pods after the instance was created again = %q", got)
	}
	log = nil
	synced("the instance at its next generation", "rec prefill-0@1")
}

// A Role sync hook that fails fails the round, and not before the round has
// deleted what it decided to delete. A scale-in, or the recovery of an
// instance, goes on while the hook keeps failing: a Role that cannot be synced
// must still be able to shrink, and shrinking it can be what makes the hook
// succeed.
func TestAFailingRoleSyncHookDoesNotHoldBackTheDeletions(t *testing.T) {
	var log []string
	recorder := newRecorder("rec", &log)
	ms := testModelServing(instancesOfRole(2), uses("rec"))
	h := hooked(t, []runtime.Object{ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a")}, recorder)
	h.settle()
	recorder.syncErr = errors.New("MigrationRequired: a worker still uses the earlier subdomain")

	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 1 })
	writes, err := h.roundWrites()
	if err == nil || !strings.Contains(err.Error(), "MigrationRequired") {
		t.Fatalf("round error = %v, want the failure of the hook", err)
	}
	for _, name := range []string{"llama-0-prefill-1-0", "llama-0-prefill-1-1", "llama-0-prefill-1-2"} {
		if !hasWrite(writes, "delete pods "+name) {
			t.Errorf("writes = %v; Pod %s was not deleted", writes, name)
		}
	}
	if got := h.conditions(); got != "ReconcileFailed=Error" {
		t.Errorf("conditions = %q, want the failure reported", got)
	}

	// The instance is forgotten once its Pods are gone, hook or no hook.
	log = nil
	if _, err := h.round(); err == nil {
		t.Fatal("the round succeeded although the hook fails")
	}
	if got, deleted := h.ledger(), hooksOf(&log, "delete"); got != "prefill-0:Established/0" || deleted != "rec prefill-1@0" {
		t.Errorf("ledger = %q, delete hooks %q; want the instance forgotten after its delete hook", got, deleted)
	}

	// A member is lost: the rest of its instance is deleted all the same.
	h.lose("llama-0-prefill-0-1")
	writes, err = h.roundWrites()
	if err == nil || !hasWrite(writes, "delete pods llama-0-prefill-0-0") || !hasWrite(writes, "delete pods llama-0-prefill-0-2") {
		t.Errorf("writes = %v, error %v; want the instance drained and the failure of the hook", writes, err)
	}

	recorder.syncErr = nil
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/1 Ready=AllInstancesReady" {
		t.Errorf("after the hook succeeded: %q", got)
	}
}

// A Pod is created as the create hooks left it. The hooks are called for
// every Pod, the entry Pod first, with the Pod, its Role and whether it is the
// entry Pod. Without the hooks a Pod has no hostname and subdomain for its
// headless Service, and the workers cannot find the entry Pod.
func TestAPodIsCreatedAsTheCreateHooksLeftIt(t *testing.T) {
	var log []string
	recorder := newRecorder("rec", &log)
	recorder.onCreate = func(req *plugins.HookRequest) {
		req.Pod.Labels["test/role-id"] = req.RoleID
		req.Pod.Spec.Hostname = req.Pod.Name
		req.Pod.Spec.Subdomain = "set-by-the-hook"
		req.Pod.Spec.Containers[0].Env = append(req.Pod.Spec.Containers[0].Env, corev1.EnvVar{Name: "WORKERS", Value: fmt.Sprint(req.Role.WorkerReplicas)})
		if req.IsEntry {
			req.Pod.Labels["test/entry"] = "yes"
		}
	}
	ms := testModelServing(uses("rec"))
	h := hooked(t, []runtime.Object{ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a")}, recorder)

	h.mustRound()
	if got, want := hooksOf(&log, "create"), "rec llama-0-prefill-0-0@0 rec llama-0-prefill-0-1@0 rec llama-0-prefill-0-2@0"; got != want {
		t.Errorf("create hooks = %q, want %q", got, want)
	}
	pods := h.pods()
	if len(pods) != 3 {
		t.Fatalf("Pods = %q", h.podSummary())
	}
	for _, pod := range pods {
		entry := ""
		if pod.Name == "llama-0-prefill-0-0" {
			entry = "yes"
		}
		env := pod.Spec.Containers[0].Env
		if pod.Labels["test/role-id"] != "prefill-0" || pod.Labels["test/entry"] != entry || pod.Spec.Hostname != pod.Name ||
			pod.Spec.Subdomain != "set-by-the-hook" || len(env) == 0 || env[len(env)-1] != (corev1.EnvVar{Name: "WORKERS", Value: "2"}) {
			t.Errorf("Pod %s as created: labels %v, hostname %q, subdomain %q, env %v", pod.Name, pod.Labels, pod.Spec.Hostname, pod.Spec.Subdomain, env)
		}
	}
}

// The ready hook is called once for every Pod, when the controller first sees
// it ready, and for a Pod that replaces it. A new process calls it again: what
// was called is remembered in the process alone, and the hooks bear that.
func TestTheReadyHookIsCalledOnceForEveryPod(t *testing.T) {
	var log []string
	recorder := newRecorder("rec", &log)
	ms := testModelServing(uses("rec"), recoveryPolicy(workloadv1alpha1.NoneRestartPolicy))
	h := hooked(t, []runtime.Object{ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a")}, recorder)
	const all = "rec llama-0-prefill-0-0@0 rec llama-0-prefill-0-1@0 rec llama-0-prefill-0-2@0"

	h.mustRound()
	h.mustRound()
	if got := hooksOf(&log, "ready"); got != "" {
		t.Fatalf("ready hooks before a Pod is ready: %q", got)
	}
	h.ready("llama-0-prefill-0-1")
	h.mustRound()
	if got := hooksOf(&log, "ready"); got != "rec llama-0-prefill-0-1@0" {
		t.Fatalf("ready hooks after one Pod became ready: %q", got)
	}
	h.settle()
	if got := hooksOf(&log, "ready"); got != "rec llama-0-prefill-0-0@0 rec llama-0-prefill-0-2@0" {
		t.Fatalf("ready hooks after the others became ready: %q", got)
	}
	for range 3 {
		h.mustRound()
	}
	if got := hooksOf(&log, "ready"); got != "" {
		t.Errorf("ready hooks in rounds without a change: %q", got)
	}

	// A Pod that stops being ready and is ready again is the same Pod.
	h.crash("llama-0-prefill-0-2")
	h.mustRound()
	h.settle()
	if got := hooksOf(&log, "ready"); got != "" {
		t.Errorf("ready hooks for a Pod that was ready before: %q", got)
	}
	// A Pod that takes the place of one that is lost is another.
	h.lose("llama-0-prefill-0-1")
	h.settle()
	if got := hooksOf(&log, "ready"); got != "rec llama-0-prefill-0-1@0" {
		t.Errorf("ready hooks after a Pod was replaced: %q", got)
	}

	h.restart()
	h.mustRound()
	if got := hooksOf(&log, "ready"); got != all {
		t.Errorf("ready hooks of a new process: %q, want %q", got, all)
	}
}

// The Pods of a revision are created as that revision says: with its
// scheduler, and through its plugins and no others. The ModelServing may have
// changed both since. A group that stays at an earlier revision, below a
// partition for instance, would otherwise be repaired with the plugins of a
// template it does not run.
func TestAnInstanceGoesThroughThePluginsOfItsRevision(t *testing.T) {
	earlier := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.SchedulerName = "earlier-scheduler"
		uses("earlier")(ms)
	}
	later := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.SchedulerName = "later-scheduler"
		ms.Spec.Template.Roles[0].EntryTemplate = podTemplate("model:v2")
		ms.Spec.Template.Roles[0].WorkerTemplate = ptr.To(podTemplate("model:v2"))
		uses("later")(ms)
	}
	// schedulers lists the Role instances with the scheduler of their Pods.
	schedulers := func(h *harness) string {
		byInstance := map[string]string{}
		for _, pod := range h.pods() {
			id := pod.Labels[workloadv1alpha1.RoleIDKey]
			if seen, ok := byInstance[id]; ok && seen != pod.Spec.SchedulerName {
				byInstance[id] = "mixed"
				continue
			}
			byInstance[id] = pod.Spec.SchedulerName
		}
		var out []string
		for id, scheduler := range byInstance {
			out = append(out, id+"="+scheduler)
		}
		sort.Strings(out)
		return strings.Join(out, " ")
	}

	t.Run("a group at an earlier revision", func(t *testing.T) {
		var log []string
		ms := testModelServing(later)
		h := hooked(t, []runtime.Object{ms, revisionOf(t, testModelServing(earlier), "rev-a"), revisionOf(t, ms, "rev-b"), groupOf(ms, "rev-a")},
			newRecorder("earlier", &log), newRecorder("later", &log))
		h.settle()

		if got := schedulers(h); got != "prefill-0=earlier-scheduler" {
			t.Errorf("schedulers = %q", got)
		}
		hooks := strings.Join(log, "\n")
		if strings.Contains(hooks, "later ") {
			t.Errorf("a plugin the revision of the group does not have was called:\n%s", hooks)
		}
		for _, hook := range []string{"earlier sync prefill-0@0", "earlier create llama-0-prefill-0-0@0", "earlier ready llama-0-prefill-0-0@0"} {
			if !strings.Contains(hooks, hook) {
				t.Errorf("the hooks have no %q:\n%s", hook, hooks)
			}
		}

		// A lost member is created again the same way.
		log = nil
		h.lose("llama-0-prefill-0-2")
		h.settle()
		hooks = strings.Join(log, "\n")
		if strings.Contains(hooks, "later ") || !strings.Contains(hooks, "earlier create llama-0-prefill-0-2@1") || schedulers(h) != "prefill-0=earlier-scheduler" {
			t.Errorf("after the recovery: schedulers %q, hooks:\n%s", schedulers(h), hooks)
		}
	})

	// Rolled Role by Role, the instances of one group run different
	// revisions: here the one below the partition of the Role is created from
	// the baseline, the other from the target.
	t.Run("instances of two revisions in one group", func(t *testing.T) {
		var log []string
		rolled := func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
			ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			ms.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(1))
		}
		ms := testModelServing(later, rolled)
		group := groupOf(ms, "rev-b")
		group.Spec.BaselineRevision = "rev-a"
		h := hooked(t, []runtime.Object{ms, revisionOf(t, testModelServing(earlier, rolled), "rev-a"), revisionOf(t, ms, "rev-b"), group},
			newRecorder("earlier", &log), newRecorder("later", &log))
		h.settle()

		if got := schedulers(h); got != "prefill-0=earlier-scheduler prefill-1=later-scheduler" {
			t.Errorf("schedulers = %q", got)
		}
		hooks := strings.Join(log, "\n")
		for _, hook := range []string{
			"earlier sync prefill-0@0", "earlier create llama-0-prefill-0-0@0", "earlier ready llama-0-prefill-0-0@0",
			"later sync prefill-1@0", "later create llama-0-prefill-1-0@0", "later ready llama-0-prefill-1-0@0",
		} {
			if !strings.Contains(hooks, hook) {
				t.Errorf("the hooks have no %q:\n%s", hook, hooks)
			}
		}
		for _, hook := range []string{"earlier sync prefill-1@", "earlier create llama-0-prefill-1-", "earlier ready llama-0-prefill-1-",
			"later sync prefill-0@", "later create llama-0-prefill-0-", "later ready llama-0-prefill-0-"} {
			if strings.Contains(hooks, hook) {
				t.Errorf("an instance went through the plugin of the other revision (%q):\n%s", hook, hooks)
			}
		}
	})
}
