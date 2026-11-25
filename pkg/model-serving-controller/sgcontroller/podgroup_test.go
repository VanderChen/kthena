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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func (h *harness) deleteRevision(revision string) {
	h.t.Helper()
	name := utils.GenerateControllerRevisionName("llama", revision)
	if err := h.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

// The PodGroup of a group says what the Pods of the group need: how many,
// in subgroups of which size, with which resources. That is read from the
// revision the group runs. While the revision cannot be resolved the PodGroup
// is left as it is. Built from the template the ModelServing has now, it would
// ask the scheduler for a gang the Pods of the group never form.
func TestThePodGroupIsLeftAloneWhileTheRevisionCannotBeResolved(t *testing.T) {
	t.Run("a group that has no Pod yet", func(t *testing.T) {
		ms := testModelServing()
		h := newHarness(t, ms, groupOf(ms, "rev-a"))
		h.podGroups.enabled = true
		h.mustRound()
		h.mustRound()
		if len(h.podGroups.synced) != 0 {
			t.Fatalf("the PodGroup was synced without the revision of the group: %v", h.podGroups.synced)
		}
		// The ControllerRevision appears: the PodGroup comes first, then the Pods.
		if _, err := h.cluster.Kube.AppsV1().ControllerRevisions(testNamespace).Create(context.Background(), revisionOf(t, ms, "rev-a"), metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		h.settle()
		if len(h.podGroups.synced) == 0 || h.conditions() != "Ready=AllInstancesReady" {
			t.Errorf("after the revision appeared: PodGroup synced %d times, conditions %q", len(h.podGroups.synced), h.conditions())
		}
	})

	// The group runs, its revision is lost, and the ModelServing has another
	// template by now, with more workers.
	t.Run("a group that runs", func(t *testing.T) {
		h := standard(t)
		h.podGroups.enabled = true
		h.settle()
		synced := len(h.podGroups.synced)
		if synced == 0 || h.podGroups.views[synced-1].Spec.Template.Roles[0].WorkerReplicas != 2 {
			t.Fatalf("the PodGroup of the running group was synced %d times", synced)
		}

		h.deleteRevision("rev-a")
		ms, err := h.cluster.GetModelServing(testNamespace, "llama")
		if err != nil {
			t.Fatal(err)
		}
		ms.Spec.Template.Roles[0].WorkerReplicas = 5
		if _, err := h.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Update(context.Background(), ms, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			h.mustRound()
		}
		if len(h.podGroups.synced) != synced {
			t.Errorf("the PodGroup was synced %d more times without the revision of the group, the last time for %d workers",
				len(h.podGroups.synced)-synced, h.podGroups.views[len(h.podGroups.views)-1].Spec.Template.Roles[0].WorkerReplicas)
		}
		if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0" {
			t.Errorf("Pods = %q", got)
		}
	})
}

// Rolled Role by Role, the PodGroup takes the layout of every instance from
// the revision the instance runs. When one of them cannot be read the
// PodGroup is not written, the round fails and says why, and nothing is
// deleted: a PodGroup built without that instance could ask for more than the
// instance has, and its Pods would never be scheduled again.
func TestThePodGroupIsNotWrittenWithoutTheLayoutOfAnInstance(t *testing.T) {
	// Two instances; the partition of the Role keeps both at rev-a.
	rolled := func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{Type: workloadv1alpha1.RoleRollingUpdate}
		ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
		ms.Spec.Template.Roles[0].Partition = ptr.To(intstr.FromInt32(2))
	}
	earlier := testModelServing(rolled)
	ms := testModelServing(rolled, func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Template.Roles[0].WorkerReplicas = 3 })
	group := groupOf(ms, "rev-b")
	group.Spec.BaselineRevision = "rev-a"
	h := newHarness(t, ms, revisionOf(t, earlier, "rev-a"), revisionOf(t, ms, "rev-b"), group)
	h.podGroups.enabled = true
	h.settle()
	if got := h.podSummary(); got != "prefill-0-0@0 prefill-0-1@0 prefill-0-2@0 prefill-1-0@0 prefill-1-1@0 prefill-1-2@0" {
		t.Fatalf("Pods = %q; want two instances of the layout of rev-a", got)
	}
	synced := len(h.podGroups.synced)
	if synced == 0 || h.podGroups.views[synced-1].Spec.Template.Roles[0].WorkerReplicas != 2 {
		t.Fatalf("the PodGroup was synced %d times; want it built from the layout of rev-a", synced)
	}

	h.deleteRevision("rev-a")
	h.cluster.ResetWrites()
	for range 3 {
		writes, err := h.roundWrites()
		if err == nil || !strings.Contains(err.Error(), "rev-a") {
			t.Fatalf("round error = %v, want the revision that cannot be read", err)
		}
		for _, write := range writes {
			if !strings.HasPrefix(write, "update servinggroups/status") {
				t.Errorf("a round without the layout of an instance issued %q", write)
			}
		}
	}
	if len(h.podGroups.synced) != synced {
		t.Errorf("the PodGroup was synced %d more times without the layout of an instance", len(h.podGroups.synced)-synced)
	}
}
