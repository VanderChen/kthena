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
	"errors"
	"testing"
	"time"

	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// syncHook is a plugin whose OnModelServingSync returns what the test says.
type syncHook struct {
	plugins.BasePlugin
	result plugins.HookResult
	// groups counts the ServingGroups that exist.
	groups func() int
	// calls holds, for every call, the number of ServingGroups at that time.
	calls []int
	// incomplete reports that a request lacked something a plugin may use.
	incomplete bool
}

func (p *syncHook) Name() string { return "sync-hook" }

func (p *syncHook) OnModelServingSync(_ context.Context, req *plugins.HookRequest) plugins.HookResult {
	p.calls = append(p.calls, p.groups())
	p.incomplete = p.incomplete || req.ModelServing == nil || req.KubeClient == nil || req.ServiceLister == nil
	return p.result
}

// hookedSystem returns a system whose ModelServing runs hook.
func hookedSystem(t *testing.T, hook *syncHook) *system {
	t.Helper()
	s := &system{t: t, registry: plugins.NewRegistry()}
	s.registry.Register("sync-hook", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return hook, nil })
	s.cluster = apitest.New(modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "sync-hook", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	}))
	s.restart()
	hook.groups = func() int { return len(s.groups()) }
	return s
}

// roundOfTheModelServing runs one round of the ModelServing controller on a
// fresh cache.
func (s *system) roundOfTheModelServing() (time.Duration, error) {
	s.t.Helper()
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	return s.ms.Reconcile(context.Background(), testNamespace, testName)
}

// The sync hook of a ModelServing is called at the start of every round,
// before a group is created. A hook that fails and does not ask to continue
// stops the round there: nothing is written. One that asks to continue does
// not stop it. When a round that goes on is asked to come again, it does.
//
// A plugin uses the hook to say "not now": a registry that is not up, a quota
// that is not granted. A round that went on would create the groups the
// plugin asked to hold back.
func TestTheSyncHookOfTheModelServing(t *testing.T) {
	syncErr := errors.New("registry init failed")
	tests := []struct {
		name    string
		result  plugins.HookResult
		stopped bool
		requeue time.Duration
	}{
		{name: "success: the round goes on"},
		{name: "success, to be called again: the round goes on and comes again", result: plugins.HookResult{RequeueAfter: 20 * time.Millisecond}, requeue: 20 * time.Millisecond},
		{name: "an error that stops the round: nothing is written", result: plugins.HookResult{Err: syncErr}, stopped: true},
		{name: "an error that does not, to be called again: the round goes on and comes again",
			result: plugins.HookResult{Err: syncErr, Continue: true, RequeueAfter: 20 * time.Millisecond}, requeue: 20 * time.Millisecond},
		{name: "an error that does not: the round goes on", result: plugins.HookResult{Err: syncErr, Continue: true}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hook := &syncHook{result: tt.result}
			s := hookedSystem(t, hook)

			after, err := s.roundOfTheModelServing()
			if len(hook.calls) != 1 || hook.calls[0] != 0 || hook.incomplete {
				t.Fatalf("the hook was called with %v groups in existence, incomplete request = %t; want one call before any group", hook.calls, hook.incomplete)
			}
			if after != tt.requeue {
				t.Errorf("the round is due again after %v, want %v", after, tt.requeue)
			}
			if tt.stopped {
				if !errors.Is(err, syncErr) {
					t.Errorf("round error = %v, want the error of the hook", err)
				}
				if writes := s.cluster.Writes(); len(writes) != 0 || s.modelServing().Status.ObservedGeneration != 0 {
					t.Errorf("a round that the hook stopped wrote %v", writes)
				}
				return
			}
			if err != nil && !errors.Is(err, ErrNotSettled) {
				t.Fatalf("round error = %v", err)
			}
			if got := s.images(); got != "0=none 1=none 2=none" {
				t.Errorf("groups after the round = %q, want the three groups created", got)
			}
			s.settle()
			if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Errorf("settled:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// A hook that stops the rounds of a ModelServing that is running stops what
// the ModelServing controller would do next: no group is created, deleted or
// given another revision, and the status keeps the generation it has seen.
// When the hook lets go, the rounds go on from there.
func TestAStoppingSyncHookHoldsARunningModelServing(t *testing.T) {
	hook := &syncHook{}
	s := hookedSystem(t, hook)
	s.settle()
	seen := s.modelServing().Status.ObservedGeneration

	hook.result = plugins.HookResult{Err: errors.New("the quota is not granted"), RequeueAfter: time.Minute}
	s.update(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.Replicas = ptr.To[int32](4)
		setImage("model:v2")(ms)
	})
	s.cluster.ResetWrites()
	for range 3 {
		if _, err := s.roundOfTheModelServing(); err == nil {
			t.Fatal("the round succeeded although the hook stops it")
		}
	}
	if writes := s.cluster.Writes(); len(writes) != 0 {
		t.Errorf("rounds that the hook stopped wrote %v", writes)
	}
	if status := s.modelServing().Status; status.ObservedGeneration != seen || s.images() != "0=model:v1 1=model:v1 2=model:v1" {
		t.Errorf("observedGeneration %d (before %d), groups %s; want nothing to have moved", status.ObservedGeneration, seen, s.images())
	}

	hook.result = plugins.HookResult{}
	s.settle()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2 3=model:v2] replicas=4 updated=4 current=4 available=4 Available"; got != want {
		t.Errorf("after the hook let go:\n got %s\nwant %s", got, want)
	}
}
