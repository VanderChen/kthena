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
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

const (
	testNamespace = "default"
	testGroup     = "llama-0"
)

func podTemplate(image string) workloadv1alpha1.PodTemplateSpec {
	return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: image}}}}
}

// testModelServing returns a ModelServing with one Role, prefill: one
// instance of an entry Pod and two workers.
func testModelServing(mutate ...func(*workloadv1alpha1.ModelServing)) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "llama", UID: "ms-uid", Generation: 1},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:       ptr.To[int32](1),
			SchedulerName:  "volcano",
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
			Template: workloadv1alpha1.ServingGroupTemplate{
				RestartGracePeriodSeconds: ptr.To[int64](0),
				Roles: []workloadv1alpha1.Role{{
					Name: "prefill", Replicas: ptr.To[int32](1), WorkerReplicas: 2,
					EntryTemplate: podTemplate("model:v1"), WorkerTemplate: ptr.To(podTemplate("model:v1")),
				}},
			},
		},
	}
	for _, m := range mutate {
		m(ms)
	}
	return ms
}

// revisionOf returns the ControllerRevision that records ms as revision.
func revisionOf(t *testing.T, ms *workloadv1alpha1.ModelServing, revision string) *appsv1.ControllerRevision {
	t.Helper()
	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		t.Fatal(err)
	}
	return &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: utils.GenerateControllerRevisionName(ms.Name, revision),
			Labels: map[string]string{
				utils.ControllerRevisionLabelKey:         ms.Name,
				utils.ControllerRevisionRevisionLabelKey: revision,
			},
			Annotations: map[string]string{utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
				Name: ms.Name, UID: ms.UID, Controller: ptr.To(true),
			}},
		},
		Data: runtime.RawExtension{Raw: data},
	}
}

// groupOf returns the ServingGroup llama-0 of ms at revision.
func groupOf(ms *workloadv1alpha1.ModelServing, revision string) *workloadv1alpha1.ServingGroup {
	settings := workload.Settings{Revision: revision, ExpectedPods: workload.ExpectedPods(ms)}
	if ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
		settings = workload.Settings{TargetRevision: revision, BaselineRevision: revision, ExpectedPods: workload.ExpectedPods(ms)}
	}
	return &workloadv1alpha1.ServingGroup{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: testGroup, UID: "sg-uid", Generation: 1,
			Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
				Name: ms.Name, UID: ms.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
			}},
		},
		Spec: workload.GroupSpec(ms, settings),
	}
}

// fakePodGroups stands in for the PodGroup manager.
type fakePodGroups struct {
	enabled bool
	err     error
	synced  []string
	// writes is asked for the number of writes the cluster accepted so far.
	writes func() int
	// syncedAt holds that number for each call of SyncPodGroup.
	syncedAt []int
}

func (f *fakePodGroups) Enabled(*workloadv1alpha1.ModelServing) bool { return f.enabled }

func (f *fakePodGroups) SyncPodGroup(_ context.Context, ms *workloadv1alpha1.ModelServing, owner metav1.OwnerReference, name string) (time.Duration, error) {
	f.synced = append(f.synced, fmt.Sprintf("%s by %s for scheduler %s", name, owner.UID, ms.Spec.SchedulerName))
	if f.writes != nil {
		f.syncedAt = append(f.syncedAt, f.writes())
	}
	if f.err != nil {
		return 5 * time.Second, f.err
	}
	return 0, nil
}

func (f *fakePodGroups) AnnotatePodWithPodGroup(pod *corev1.Pod, _ *workloadv1alpha1.ModelServing, groupName, taskName string) {
	if f.enabled {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations["test/pod-group"] = groupName + "/" + taskName
	}
}

func (f *fakePodGroups) GenerateTaskName(roleName string, roleIndex int) string {
	return fmt.Sprintf("%s-%d", roleName, roleIndex)
}

// harness runs a ServingGroup controller against an apitest.Cluster. Every
// round reads a fresh snapshot of the cluster as its cache, unless the test
// hands it an older one.
type harness struct {
	t          *testing.T
	cluster    *apitest.Cluster
	controller *Controller
	podGroups  *fakePodGroups
	// recorder, when set, receives the events of the controller.
	recorder record.EventRecorder
}

func newHarness(t *testing.T, objects ...runtime.Object) *harness {
	t.Helper()
	h := &harness{t: t, cluster: apitest.New(objects...), podGroups: &fakePodGroups{}}
	h.podGroups.writes = func() int { return len(h.cluster.Writes()) }
	h.restart()
	return h
}

// restart replaces the controller by one that remembers nothing, as after a
// crash or a change of leader.
func (h *harness) restart() {
	h.controller = New(Options{
		Kube: h.cluster.Kube, Kthena: h.cluster.Kthena, PodGroups: h.podGroups, Recorder: h.recorder,
		Now: func() time.Time { return h.cluster.Now() },
	})
}

func (h *harness) useSnapshot(snapshot *apitest.Snapshot) {
	h.t.Helper()
	caches := objcache.Caches{
		ModelServings: snapshot.ModelServings, ServingGroups: snapshot.ServingGroups,
		Pods: snapshot.Pods, ControllerRevisions: snapshot.ControllerRevisions,
	}
	if err := caches.AddIndexers(); err != nil {
		h.t.Fatal(err)
	}
	h.controller.SetCaches(caches, snapshot.Listers().Services)
}

// round runs one round on a cache that is up to date.
func (h *harness) round() (time.Duration, error) {
	h.t.Helper()
	return h.roundOn(h.cluster.Snapshot())
}

// roundOn runs one round on the given cache.
func (h *harness) roundOn(snapshot *apitest.Snapshot) (time.Duration, error) {
	h.t.Helper()
	h.useSnapshot(snapshot)
	return h.controller.Reconcile(context.Background(), testNamespace, testGroup)
}

// mustRound runs one round that has to succeed and returns the writes it made.
func (h *harness) mustRound() []string {
	h.t.Helper()
	before := len(h.cluster.Writes())
	if _, err := h.round(); err != nil {
		h.t.Fatalf("round: %v", err)
	}
	return writesOf(h.cluster.Writes()[before:])
}

// settle runs rounds until one writes nothing, making every Pod that is not
// terminating ready in between as a kubelet would, and finishing the garbage
// collection.
func (h *harness) settle() {
	h.t.Helper()
	for range 20 {
		h.readyAll()
		h.cluster.Settle()
		if len(h.mustRound()) == 0 {
			return
		}
	}
	h.t.Fatalf("the group did not settle; last writes: %v", writesOf(h.cluster.Writes()))
}

func writesOf(writes []apitest.Write) []string {
	out := make([]string, 0, len(writes))
	for _, write := range writes {
		resource := write.Resource
		if write.Subresource != "" {
			resource += "/" + write.Subresource
		}
		out = append(out, write.Verb+" "+resource+" "+write.Name)
	}
	return out
}

func (h *harness) group() *workloadv1alpha1.ServingGroup {
	h.t.Helper()
	group, err := h.cluster.GetServingGroup(testNamespace, testGroup)
	if err != nil {
		h.t.Fatal(err)
	}
	return group
}

func (h *harness) pods() []*corev1.Pod {
	h.t.Helper()
	list, err := h.cluster.Kube.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	sort.Slice(pods, func(a, b int) bool { return pods[a].Name < pods[b].Name })
	return pods
}

// podSummary lists the Pods as <name suffix>@<generation>, with ! for a Pod
// that is terminating: "prefill-0-0@0 prefill-0-1@0!".
func (h *harness) podSummary() string {
	h.t.Helper()
	var out []string
	for _, pod := range h.pods() {
		item := strings.TrimPrefix(pod.Name, testGroup+"-") + "@" + pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey]
		if pod.DeletionTimestamp != nil {
			item += "!"
		}
		out = append(out, item)
	}
	return strings.Join(out, " ")
}

// ledger renders the ledger as "prefill-0:Established/0 ...".
func (h *harness) ledger() string {
	h.t.Helper()
	var out []string
	for _, entry := range h.group().Status.RoleInstances {
		out = append(out, fmt.Sprintf("%s-%d:%s/%d", entry.Role, entry.Index, entry.Phase, entry.Generation))
	}
	return strings.Join(out, " ")
}

// conditions renders the conditions that are true as "Failed=MemberLost Ready=AllInstancesReady".
func (h *harness) conditions() string {
	h.t.Helper()
	var out []string
	for _, condition := range h.group().Status.Conditions {
		if condition.Status == metav1.ConditionTrue {
			out = append(out, condition.Type+"="+condition.Reason)
		}
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

func (h *harness) isReady() bool {
	h.t.Helper()
	return meta.IsStatusConditionTrue(h.group().Status.Conditions, string(workloadv1alpha1.ServingGroupConditionReady))
}

// setPodStatus writes the status of a Pod as a kubelet would.
func (h *harness) setPodStatus(name string, mutate func(*corev1.PodStatus)) {
	h.t.Helper()
	pod, err := h.cluster.GetPod(testNamespace, name)
	if err != nil {
		h.t.Fatal(err)
	}
	mutate(&pod.Status)
	if _, err := h.cluster.Kube.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) ready(name string) {
	h.t.Helper()
	h.setPodStatus(name, func(status *corev1.PodStatus) {
		status.Phase = corev1.PodRunning
		status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(h.cluster.Now()),
		}}
		status.ContainerStatuses = []corev1.ContainerStatus{{Name: "model", Ready: true, Image: "model:v1"}}
	})
}

// readyAll makes every Pod ready that is not terminating and not ready yet.
func (h *harness) readyAll() {
	h.t.Helper()
	for _, pod := range h.pods() {
		if pod.DeletionTimestamp == nil && !utils.IsPodRunningAndReady(pod) && pod.Status.Phase != corev1.PodFailed {
			h.ready(pod.Name)
		}
	}
}

// crash makes the container of a Pod restart: the Pod is not ready and has a
// restart nobody planned.
func (h *harness) crash(name string) {
	h.t.Helper()
	h.setPodStatus(name, func(status *corev1.PodStatus) {
		status.Phase = corev1.PodRunning
		status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: metav1.NewTime(h.cluster.Now()),
		}}
		status.ContainerStatuses = []corev1.ContainerStatus{{Name: "model", RestartCount: 1, Image: "model:v1"}}
	})
}

// lose deletes a Pod behind the back of the controller, as a node failure or
// a user would.
func (h *harness) lose(name string) {
	h.t.Helper()
	if err := h.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
		h.t.Fatal(err)
	}
	h.cluster.ResetWrites()
}

func (h *harness) updateGroup(mutate func(*workloadv1alpha1.ServingGroup)) {
	h.t.Helper()
	group := h.group()
	mutate(group)
	if _, err := h.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(context.Background(), group, metav1.UpdateOptions{}); err != nil {
		h.t.Fatal(err)
	}
}

func uidOf(pods []*corev1.Pod, suffix string) types.UID {
	for _, pod := range pods {
		if pod.Name == testGroup+"-"+suffix {
			return pod.UID
		}
	}
	return ""
}
