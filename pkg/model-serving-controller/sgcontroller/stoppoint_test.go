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
	"errors"
	"fmt"
	"testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
)

var errCrash = errors.New("the process died")

// stopPoints runs a scenario with a controller that dies when it is about to
// make its stopAt-th write (never for stopAt 0). A controller that remembers
// nothing takes over; with staleCache its first round reads the cache as it
// was before the round that died. It returns the state the group ends in and
// the number of writes of the controllers.
func stopPoints(t *testing.T, policy workloadv1alpha1.RecoveryPolicy, stopAt int, staleCache bool) (state string, writes int) {
	t.Helper()
	h := standard(t, recoveryPolicy(policy))
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
	settle := func() {
		t.Helper()
		for range 40 {
			h.readyAll()
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
			if len(h.cluster.Writes()) == accepted {
				return
			}
		}
		t.Fatalf("stop at write %d: the group did not settle", stopAt)
	}

	settle()
	h.lose("llama-0-prefill-0-1")
	settle()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 2, 6
	})
	settle()
	h.crash("llama-0-prefill-1-2")
	settle()
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) {
		group.Spec.Roles[0].Replicas, group.Spec.ExpectedPods = 1, 3
	})
	settle()

	status := h.group().Status
	return fmt.Sprintf("pods[%s] ledger[%s] conditions[%s] pods=%d ready=%d", h.podSummary(), h.ledger(), h.conditions(), status.Pods, status.ReadyPods), writes
}

// Whatever write a controller dies before, the one that takes over brings the
// group to the state it would have reached without the interruption: nothing
// a round needs is kept in the memory of the process.
func TestEveryStopPointEndsInTheSameState(t *testing.T) {
	for _, policy := range []workloadv1alpha1.RecoveryPolicy{workloadv1alpha1.RoleRecreate, workloadv1alpha1.NoneRestartPolicy} {
		want, total := stopPoints(t, policy, 0, false)
		if total < 10 {
			t.Fatalf("%s: the scenario made %d writes; it does not exercise the controller", policy, total)
		}
		for _, stale := range []bool{false, true} {
			for stopAt := 1; stopAt <= total; stopAt++ {
				if got, _ := stopPoints(t, policy, stopAt, stale); got != want {
					t.Errorf("%s, stop at write %d of %d, stale cache %t:\n got %s\nwant %s", policy, stopAt, total, stale, got, want)
				}
			}
		}
		t.Logf("%s: %d stop points; final state %s", policy, total, want)
	}
}
