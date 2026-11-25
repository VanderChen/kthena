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
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/sgcontroller"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	testNamespace = "default"
	testName      = "llama"
)

func podTemplate(image string) workloadv1alpha1.PodTemplateSpec {
	return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: image}}}}
}

// modelServing returns a ModelServing of three groups. Each group has one
// instance of one Role: an entry Pod and one worker.
func modelServing(mutate ...func(*workloadv1alpha1.ModelServing)) *workloadv1alpha1.ModelServing {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: testName},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:       ptr.To[int32](3),
			SchedulerName:  "default-scheduler",
			RecoveryPolicy: workloadv1alpha1.RoleRecreate,
			RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
				Type:         workloadv1alpha1.ServingGroupRollingUpdate,
				UpdateMethod: workloadv1alpha1.RecreateUpdateMethod,
				RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
					MaxUnavailable: ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: workloadv1alpha1.ServingGroupTemplate{
				RestartGracePeriodSeconds: ptr.To[int64](0),
				Roles: []workloadv1alpha1.Role{{
					Name: "serve", Replicas: ptr.To[int32](1), WorkerReplicas: 1,
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

func rollout(mutate func(*workloadv1alpha1.RollingUpdateConfiguration)) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) { mutate(ms.Spec.RolloutStrategy.RollingUpdateConfiguration) }
}

// noPodGroups is the PodGroup manager of a cluster without Volcano.
type noPodGroups struct{}

func (noPodGroups) Enabled(*workloadv1alpha1.ModelServing) bool { return false }
func (noPodGroups) SyncPodGroup(context.Context, *workloadv1alpha1.ModelServing, metav1.OwnerReference, string) (time.Duration, error) {
	return 0, nil
}
func (noPodGroups) AnnotatePodWithPodGroup(*corev1.Pod, *workloadv1alpha1.ModelServing, string, string) {
}
func (noPodGroups) GenerateTaskName(roleName string, roleIndex int) string {
	return fmt.Sprintf("%s-%d", roleName, roleIndex)
}

// system runs the ModelServing controller and the ServingGroup controller
// against one apitest.Cluster, a round at a time, with the kubelet and the
// garbage collector played by the test.
type system struct {
	t       *testing.T
	cluster *apitest.Cluster
	ms      *Controller
	sg      *sgcontroller.Controller
	// invariant, when set, is checked after every pass.
	invariant func()
}

func newSystem(t *testing.T, objects ...runtime.Object) *system {
	t.Helper()
	s := &system{t: t, cluster: apitest.New(objects...)}
	s.restart()
	return s
}

// restart replaces both controllers by ones that remember nothing.
func (s *system) restart() {
	now := func() time.Time { return s.cluster.Now() }
	s.ms = New(Options{Kube: s.cluster.Kube, Kthena: s.cluster.Kthena, Now: now})
	s.sg = sgcontroller.New(sgcontroller.Options{Kube: s.cluster.Kube, Kthena: s.cluster.Kthena, PodGroups: noPodGroups{}, Now: now})
}

func (s *system) caches() (objcache.Caches, *apitest.Snapshot) {
	s.t.Helper()
	snapshot := s.cluster.Snapshot()
	caches := objcache.Caches{
		ModelServings: snapshot.ModelServings, ServingGroups: snapshot.ServingGroups,
		Pods: snapshot.Pods, ControllerRevisions: snapshot.ControllerRevisions,
	}
	if err := caches.AddIndexers(); err != nil {
		s.t.Fatal(err)
	}
	return caches, snapshot
}

// pass runs one round of the ModelServing controller and one round of the
// ServingGroup controller for every group, each on a fresh cache. It returns
// the number of writes the controllers made.
func (s *system) pass() int {
	s.t.Helper()
	before := len(s.cluster.Writes())
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	if _, err := s.ms.Reconcile(context.Background(), testNamespace, testName); err != nil && !errors.Is(err, ErrNotSettled) {
		s.t.Fatalf("round of the ModelServing: %v", err)
	}
	for _, group := range s.groups() {
		caches, snapshot := s.caches()
		s.sg.SetCaches(caches, snapshot.Listers().Services)
		if _, err := s.sg.Reconcile(context.Background(), testNamespace, group.Name); err != nil {
			s.t.Fatalf("round of ServingGroup %s: %v", group.Name, err)
		}
	}
	if s.invariant != nil {
		s.invariant()
	}
	return len(s.cluster.Writes()) - before
}

// settle runs passes until one writes nothing. Between passes every Pod that
// is not terminating becomes ready and the garbage collector runs.
func (s *system) settle() {
	s.t.Helper()
	for range 100 {
		s.kubelet()
		s.cluster.Settle()
		if s.pass() == 0 {
			return
		}
	}
	s.t.Fatalf("the ModelServing did not settle: %s", s.summary())
}

// kubelet makes every Pod ready that is not terminating and not ready.
func (s *system) kubelet() {
	s.t.Helper()
	for _, pod := range s.pods() {
		if pod.DeletionTimestamp != nil || utils.IsPodRunningAndReady(pod) {
			continue
		}
		pod.Status.Phase = corev1.PodRunning
		pod.Status.Conditions = []corev1.PodCondition{{
			Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: metav1.NewTime(s.cluster.Now()),
		}}
		if _, err := s.cluster.Kube.CoreV1().Pods(testNamespace).UpdateStatus(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
			s.t.Fatal(err)
		}
	}
}

func (s *system) modelServing() *workloadv1alpha1.ModelServing {
	s.t.Helper()
	ms, err := s.cluster.GetModelServing(testNamespace, testName)
	if err != nil {
		s.t.Fatal(err)
	}
	return ms
}

func (s *system) update(mutate func(*workloadv1alpha1.ModelServing)) {
	s.t.Helper()
	ms := s.modelServing()
	mutate(ms)
	if _, err := s.cluster.Kthena.WorkloadV1alpha1().ModelServings(testNamespace).Update(context.Background(), ms, metav1.UpdateOptions{}); err != nil {
		s.t.Fatal(err)
	}
}

func setImage(image string) func(*workloadv1alpha1.ModelServing) {
	return func(ms *workloadv1alpha1.ModelServing) {
		role := &ms.Spec.Template.Roles[0]
		role.EntryTemplate = podTemplate(image)
		role.WorkerTemplate = ptr.To(podTemplate(image))
	}
}

func (s *system) groups() []*workloadv1alpha1.ServingGroup {
	s.t.Helper()
	list, err := s.cluster.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	groups := make([]*workloadv1alpha1.ServingGroup, 0, len(list.Items))
	for i := range list.Items {
		groups = append(groups, &list.Items[i])
	}
	sort.Slice(groups, func(a, b int) bool { return groups[a].Name < groups[b].Name })
	return groups
}

func (s *system) pods() []*corev1.Pod {
	s.t.Helper()
	list, err := s.cluster.Kube.CoreV1().Pods(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		s.t.Fatal(err)
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
	}
	sort.Slice(pods, func(a, b int) bool { return pods[a].Name < pods[b].Name })
	return pods
}

// readyGroups counts the groups whose Pods are all there, ready and not terminating.
func (s *system) readyGroups() int {
	s.t.Helper()
	ready := map[string]int{}
	for _, pod := range s.pods() {
		if pod.DeletionTimestamp == nil && utils.IsPodRunningAndReady(pod) {
			ready[pod.Labels[workloadv1alpha1.GroupNameLabelKey]]++
		}
	}
	count := 0
	for _, pods := range ready {
		if pods == 2 {
			count++
		}
	}
	return count
}

// images lists the groups with the image their Pods run: "0=model:v1 1=model:v2".
// A group whose Pods disagree shows "mixed", one without Pods "none"; a group
// with the surge mark is followed by "+", one that is terminating by "!".
func (s *system) images() string {
	s.t.Helper()
	byGroup := map[string]map[string]bool{}
	for _, pod := range s.pods() {
		group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if byGroup[group] == nil {
			byGroup[group] = map[string]bool{}
		}
		byGroup[group][pod.Spec.Containers[0].Image] = true
	}
	var out []string
	for _, group := range s.groups() {
		image := "none"
		switch len(byGroup[group.Name]) {
		case 0:
		case 1:
			for only := range byGroup[group.Name] {
				image = only
			}
		default:
			image = "mixed"
		}
		item := strings.TrimPrefix(group.Name, testName+"-") + "=" + image
		if group.Annotations[workloadv1alpha1.SurgeAnnotationKey] != "" {
			item += "+"
		}
		if group.DeletionTimestamp != nil {
			item += "!"
		}
		out = append(out, item)
	}
	return strings.Join(out, " ")
}

// counts renders the counts of the status: "replicas=3 updated=3 current=3 available=3".
func (s *system) counts() string {
	s.t.Helper()
	status := s.modelServing().Status
	return fmt.Sprintf("replicas=%d updated=%d current=%d available=%d", status.Replicas, status.UpdatedReplicas, status.CurrentReplicas, status.AvailableReplicas)
}

// condition returns the one of Available, Progressing and UpdateInProgress that is true.
func (s *system) condition() string {
	s.t.Helper()
	var out []string
	for _, condition := range s.modelServing().Status.Conditions {
		if condition.Status == metav1.ConditionTrue {
			out = append(out, condition.Type)
		}
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func (s *system) summary() string {
	s.t.Helper()
	return fmt.Sprintf("groups[%s] %s %s", s.images(), s.counts(), s.condition())
}

func groupReady(group *workloadv1alpha1.ServingGroup) bool {
	return meta.IsStatusConditionTrue(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionReady))
}

var intstrOne = intstr.FromInt32(1)
