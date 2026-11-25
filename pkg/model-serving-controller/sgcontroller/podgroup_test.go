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
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	batchv1alpha1 "volcano.sh/apis/pkg/apis/batch/v1alpha1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
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

// managedPodGroups makes the harness run its rounds with the PodGroup manager
// of the controller manager instead of a stand-in, in a cluster whose PodGroup
// kind has subgroups. It returns the function that runs one round and returns
// its writes.
//
// The manager learns about the PodGroup kind as it does in production, from
// the CustomResourceDefinition. Its PodGroup cache is the one thing the test
// provides: before every round, the PodGroups the cluster has.
func managedPodGroups(t *testing.T, h *harness) func() []string {
	t.Helper()
	definitions := apiextfake.NewSimpleClientset()
	manager := podgroupmanager.NewManager(h.cluster.Kube, h.cluster.Volcano, definitions, nil)
	// An informer that is there keeps the manager from starting its own.
	manager.PodGroupInformer = cache.NewSharedIndexInformer(&cache.ListWatch{}, &schedulingv1beta1.PodGroup{}, 0, cache.Indexers{})
	manager.PodGroupLister = h.cluster.Snapshot().Listers().PodGroups

	crd := testhelper.CreatePodGroupCRD()
	crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties = map[string]apiextv1.JSONSchemaProps{
		"spec": {Type: "object", Properties: map[string]apiextv1.JSONSchemaProps{"subGroupPolicy": {Type: "array"}}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if _, err := definitions.ApiextensionsV1().CustomResourceDefinitions().Create(ctx, crd, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Run(ctx); err != nil {
		t.Fatal(err)
	}
	volcano := &workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{SchedulerName: "volcano"}}
	if err := wait.PollUntilContextTimeout(ctx, time.Millisecond, 10*time.Second, true, func(context.Context) (bool, error) {
		return manager.Enabled(volcano), nil
	}); err != nil {
		t.Fatalf("the PodGroup manager did not notice the PodGroup kind: %v", err)
	}

	h.controller = New(Options{
		Kube: h.cluster.Kube, Kthena: h.cluster.Kthena, PodGroups: manager,
		Now: func() time.Time { return h.cluster.Now() },
	})
	return func() []string {
		t.Helper()
		snapshot := h.cluster.Snapshot()
		manager.PodGroupLister = snapshot.Listers().PodGroups
		before := len(h.cluster.Writes())
		if _, err := h.roundOn(snapshot); err != nil {
			t.Fatalf("round: %v", err)
		}
		return writesOf(h.cluster.Writes()[before:])
	}
}

// The ServingGroup controller with the PodGroup manager of the controller
// manager: what the two make of a group is the PodGroup a scheduler reads.
// The group controls it, it is there before the first Pod, the Pods are bound
// to it, and it asks for the Pods of the group as the revision of the group
// lays them out, in the number of instances the group has now.
func TestThePodGroupOfAGroup(t *testing.T) {
	h := standard(t)
	round := managedPodGroups(t, h)
	settle := func() {
		t.Helper()
		for range 20 {
			h.readyAll()
			h.cluster.Settle()
			if len(round()) == 0 {
				return
			}
		}
		t.Fatalf("the group did not settle; last writes: %v", writesOf(h.cluster.Writes()))
	}
	podGroup := func() *schedulingv1beta1.PodGroup {
		t.Helper()
		podGroup, err := h.cluster.GetPodGroup(testNamespace, testGroup)
		if err != nil {
			t.Fatal(err)
		}
		return podGroup
	}
	// wanted is the spec of the PodGroup of a group of instances Role
	// instances, each an entry Pod and two workers.
	wanted := func(instances int32) schedulingv1beta1.PodGroupSpec {
		return schedulingv1beta1.PodGroupSpec{
			MinMember:    3 * instances,
			MinResources: &corev1.ResourceList{},
			SubGroupPolicy: []schedulingv1beta1.SubGroupPolicySpec{{
				Name: "prefill",
				LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
					workloadv1alpha1.ModelServingNameLabelKey: "llama", workloadv1alpha1.RoleLabelKey: "prefill",
				}},
				MatchLabelKeys: []string{workloadv1alpha1.RoleIDKey},
				SubGroupSize:   ptr.To[int32](3),
				MinSubGroups:   ptr.To(instances),
			}},
		}
	}

	writes := round()
	if len(writes) != 5 || writes[0] != "create podgroups llama-0" || writes[1] != "update servinggroups/status llama-0" {
		t.Fatalf("writes = %v; want the PodGroup, then the status that commits the creation, then the three Pods", writes)
	}
	settle()
	created := podGroup()
	owner := metav1.GetControllerOf(created)
	if owner == nil || owner.UID != "sg-uid" || owner.Kind != workloadv1alpha1.ServingGroupKind.Kind || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
		t.Errorf("the PodGroup is controlled by %+v, want the group, blocking its deletion", owner)
	}
	if created.Labels[workloadv1alpha1.ModelServingNameLabelKey] != "llama" || created.Labels[workloadv1alpha1.GroupNameLabelKey] != testGroup {
		t.Errorf("labels of the PodGroup: %v", created.Labels)
	}
	if !apiequality.Semantic.DeepEqual(created.Spec, wanted(1)) {
		t.Errorf("spec of the PodGroup:\n got %+v\nwant %+v", created.Spec, wanted(1))
	}
	for _, pod := range h.pods() {
		if pod.Annotations[schedulingv1beta1.KubeGroupNameAnnotationKey] != testGroup || pod.Annotations[batchv1alpha1.TaskSpecKey] != "prefill-0" ||
			pod.Spec.SchedulerName != "volcano" {
			t.Errorf("Pod %s: annotations %v, scheduler %s; want it bound to the PodGroup", pod.Name, pod.Annotations, pod.Spec.SchedulerName)
		}
	}
	if got := h.conditions(); got != "Ready=AllInstancesReady" {
		t.Errorf("conditions = %q", got)
	}

	// One more instance: the same PodGroup asks for both.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 2 })
	settle()
	if got := podGroup(); got.UID != created.UID || !apiequality.Semantic.DeepEqual(got.Spec, wanted(2)) {
		t.Errorf("the PodGroup after an instance was added: UID %s (was %s), spec %+v; want %+v", got.UID, created.UID, got.Spec, wanted(2))
	}

	// The ModelServing gets another template, with more workers and fewer
	// instances. The group still runs its revision: its PodGroup does not
	// change.
	ms, err := h.cluster.GetModelServing(testNamespace, "llama")
	if err != nil {
		t.Fatal(err)
	}
	ms.Spec.Template.Roles[0].WorkerReplicas, ms.Spec.Template.Roles[0].Replicas = 4, ptr.To[int32](1)
	if _, err := h.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Update(context.Background(), ms, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if writes := round(); len(writes) != 0 {
		t.Errorf("writes after the ModelServing changed its template: %v", writes)
	}
	if got := podGroup().Spec; !apiequality.Semantic.DeepEqual(got, wanted(2)) {
		t.Errorf("spec of the PodGroup of a group at an earlier revision:\n got %+v\nwant %+v", got, wanted(2))
	}
}

// A PodGroup under the name of the group that is not the group's is not
// taken and not changed. The group gets no Pod, says that the name is
// occupied, and comes again.
func TestAPodGroupThatIsNotOfTheGroupIsLeftAlone(t *testing.T) {
	ms := testModelServing()
	squatter := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testGroup, UID: "squatter-uid"},
		Spec:       schedulingv1beta1.PodGroupSpec{MinMember: 7},
	}
	h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), squatter)
	managedPodGroups(t, h)

	retry, err := h.round()
	if err != nil || retry <= 0 {
		t.Fatalf("round = %v, %v; want a time to come again", retry, err)
	}
	if got := h.podSummary() + " " + h.conditions(); got != " ReconcileFailed=NameOccupied" {
		t.Errorf("Pods and conditions = %q; want no Pod and the occupied name", got)
	}
	left, err := h.cluster.GetPodGroup(testNamespace, testGroup)
	if err != nil || left.UID != "squatter-uid" || len(left.OwnerReferences) != 0 || !reflect.DeepEqual(left.Spec, squatter.Spec) {
		t.Errorf("the PodGroup that holds the name: %+v, %v; want it untouched", left, err)
	}
}
