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
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
)

// createdAtTheTarget returns a harness whose group llama-0 was created new
// while its ModelServing is rolled Role by Role from rev-a to rev-b, which
// changes the image of prefill to model:v2. prefill has three instances of an
// entry Pod and one worker, the lowest held at the baseline by the partition
// of the Role; decode has one instance and is the same in both revisions. The
// group says that it was created from rev-b.
func createdAtTheTarget(t *testing.T) *harness {
	t.Helper()
	earlier := testModelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		prefill := &ms.Spec.Template.Roles[0]
		prefill.Replicas, prefill.WorkerReplicas = ptr.To[int32](3), 1
		prefill.MaxUnavailable, prefill.Partition = ptr.To(intstr.FromInt32(1)), ptr.To(intstr.FromInt32(1))
		ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, workloadv1alpha1.Role{
			Name: "decode", Replicas: ptr.To[int32](1), EntryTemplate: podTemplate("model:v1"),
		})
	})
	ms := withImage(earlier, "model:v2")
	group := groupOf(ms, "rev-b")
	group.Spec.BaselineRevision = "rev-a"
	group.Annotations = map[string]string{workloadv1alpha1.CreatedRevisionAnnotationKey: "rev-b"}
	return newHarness(t, ms, group, revisionOf(t, earlier, "rev-a"), revisionOf(t, ms, "rev-b"))
}

// wholeAtTheTarget is what podRevisions renders for the group of
// createdAtTheTarget when every Pod of it was created from rev-b.
const wholeAtTheTarget = "decode-0-0=model:v1/rev-b " +
	"prefill-0-0=model:v2/rev-b prefill-0-1=model:v2/rev-b " +
	"prefill-1-0=model:v2/rev-b prefill-1-1=model:v2/rev-b " +
	"prefill-2-0=model:v2/rev-b prefill-2-1=model:v2/rev-b"

// podRevisions renders every Pod as "<name suffix>=<image>/<revision label>".
func (h *harness) podRevisions() string {
	h.t.Helper()
	var out []string
	for _, pod := range h.pods() {
		out = append(out, fmt.Sprintf("%s=%s/%s", strings.TrimPrefix(pod.Name, testGroup+"-"), pod.Spec.Containers[0].Image, pod.Labels[workloadv1alpha1.RevisionLabelKey]))
	}
	return strings.Join(out, " ")
}

// The partition of a Role holds the instances below it at the baseline in a
// group that ran the baseline. A group that was created at the target never
// did: its first round creates every Pod from the target, the Pods of the
// instance below the partition included.
func TestAGroupCreatedAtTheTargetCreatesEveryPodFromTheTarget(t *testing.T) {
	h := createdAtTheTarget(t)
	writes := h.mustRound()
	if len(writes) != 8 || writes[0] != "update servinggroups/status llama-0" {
		t.Fatalf("writes = %v, want the status and then seven Pods", writes)
	}
	if got := h.podRevisions(); got != wholeAtTheTarget {
		t.Fatalf("Pods after the first round:\n got %s\nwant %s", got, wholeAtTheTarget)
	}

	h.settle()
	status := h.group().Status
	if got := h.conditions(); got != "Ready=AllInstancesReady" || status.Revision != "rev-b" {
		t.Errorf("conditions = %q, revision = %q; want a ready group that reports rev-b", got, status.Revision)
	}
	if got := h.podRevisions(); got != wholeAtTheTarget {
		t.Errorf("Pods of the ready group:\n got %s\nwant %s", got, wholeAtTheTarget)
	}
}

// An instance below the partition that is lost during the rollout the group
// was created in comes back from the target as well: the group stays whole.
func TestAGroupCreatedAtTheTargetCreatesALostProtectedInstanceFromTheTarget(t *testing.T) {
	h := createdAtTheTarget(t)
	h.settle()
	before := h.uids()

	h.lose("llama-0-prefill-0-1")
	h.settle()
	if got := h.podRevisions(); got != wholeAtTheTarget {
		t.Errorf("Pods after the instance was created again:\n got %s\nwant %s", got, wholeAtTheTarget)
	}
	if got := h.ledger() + " " + h.conditions(); got != "decode-0:Established/0 prefill-0:Established/1 prefill-1:Established/0 prefill-2:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after the instance was created again: %q", got)
	}
	for name, uid := range h.uids() {
		if replaced := strings.HasPrefix(name, "llama-0-prefill-0-"); (uid != before[name]) != replaced {
			t.Errorf("Pod %s: replaced = %t, want %t", name, uid != before[name], replaced)
		}
	}
}

// createdAtTheTargetStopPoints runs the life of a group that was created at
// the target, its creation and the loss of a member of the instance below the
// partition, with a controller that dies when it is about to make its
// stopAt-th write (never for stopAt 0). A controller that remembers nothing
// takes over; with staleCache its first round reads the cache as it was
// before the round that died. It returns the Pods and the ledger the group
// ends with, and the number of writes of the controllers.
func createdAtTheTargetStopPoints(t *testing.T, stopAt int, staleCache bool) (state string, writes int) {
	t.Helper()
	h := createdAtTheTarget(t)
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
	return fmt.Sprintf("pods[%s] ledger[%s] conditions[%s]", h.podRevisions(), h.ledger(), h.conditions()), writes
}

// The revision an instance is created from is decided anew by every round,
// from the group as it is read. A controller that dies after it has written
// what it is about to create, the Pods the group is committed to or the next
// generation of an instance in the ledger, leaves the creation to the one
// that takes over. That one creates what is left from the target as well,
// below the partition like above it.
func TestAGroupCreatedAtTheTargetIsCreatedFromTheTargetAfterACrash(t *testing.T) {
	want := "pods[" + wholeAtTheTarget + "] " +
		"ledger[decode-0:Established/0 prefill-0:Established/1 prefill-1:Established/0 prefill-2:Established/0] conditions[Ready=AllInstancesReady]"
	got, total := createdAtTheTargetStopPoints(t, 0, false)
	if got != want {
		t.Fatalf("without an interruption:\n got %s\nwant %s", got, want)
	}
	if total < 12 {
		t.Fatalf("the scenario made %d writes; it does not exercise the controller", total)
	}
	for _, stale := range []bool{false, true} {
		for stopAt := 1; stopAt <= total; stopAt++ {
			if got, _ := createdAtTheTargetStopPoints(t, stopAt, stale); got != want {
				t.Errorf("stop at write %d of %d, stale cache %t:\n got %s\nwant %s", stopAt, total, stale, got, want)
			}
		}
	}
	t.Logf("%d stop points", total)
}
