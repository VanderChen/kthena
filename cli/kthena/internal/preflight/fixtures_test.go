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

package preflight

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const testNamespace = "default"

var (
	testTime   = time.Date(2026, 11, 25, 4, 0, 0, 0, time.UTC)
	readySince = metav1.NewTime(testTime.Add(-time.Hour))
)

func ptr[T any](value T) *T { return &value }

func podTemplate(image string) workloadv1alpha1.PodTemplateSpec {
	return workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
		Containers: []corev1.Container{{Name: "model", Image: image}},
	}}
}

// newModelServing returns a ModelServing with one Role, "leader", of one
// instance with an entry Pod and one worker Pod: every group has the Pods
// <group>-leader-0-0 and <group>-leader-0-1. The running controller has
// processed its spec.
func newModelServing(name string, replicas int32) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, UID: types.UID("uid-" + name), Generation: 1},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      &replicas,
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroupTemplate{Roles: []workloadv1alpha1.Role{{
				Name: "leader", Replicas: ptr(int32(1)), WorkerReplicas: 1,
				EntryTemplate: podTemplate("model:v1"), WorkerTemplate: ptr(podTemplate("model:v1")),
			}}},
		},
		Status: workloadv1alpha1.ModelServingStatus{
			ObservedGeneration: 1, Replicas: replicas, CurrentReplicas: replicas, UpdatedReplicas: replicas, AvailableReplicas: replicas,
		},
	}
}

func setImage(ms *workloadv1alpha1.ModelServing, image string) {
	for i := range ms.Spec.Template.Roles {
		ms.Spec.Template.Roles[i].EntryTemplate = podTemplate(image)
		ms.Spec.Template.Roles[i].WorkerTemplate = ptr(podTemplate(image))
	}
}

// record records the template of ms the way the controllers do, with
// utils.RecordModelServingRevision, next to the revisions that exist. It
// returns the ControllerRevision.
func record(t *testing.T, ms *workloadv1alpha1.ModelServing, existing ...*appsv1.ControllerRevision) *appsv1.ControllerRevision {
	t.Helper()
	objects := make([]runtime.Object, 0, len(existing))
	for _, revision := range existing {
		objects = append(objects, revision.DeepCopy())
	}
	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}
	revision, _, err := utils.RecordModelServingRevision(context.Background(), kubefake.NewSimpleClientset(objects...), ms, data)
	if err != nil {
		t.Fatalf("RecordModelServingRevision() error = %v", err)
	}
	return revision
}

func identityOf(revision *appsv1.ControllerRevision) string {
	return revision.Labels[utils.ControllerRevisionRevisionLabelKey]
}

// steady makes ms a ModelServing whose rollout is complete at the revision
// of its template: the revision is recorded and the status names it.
func steady(t *testing.T, ms *workloadv1alpha1.ModelServing) *appsv1.ControllerRevision {
	t.Helper()
	revision := record(t, ms)
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = identityOf(revision), identityOf(revision)
	return revision
}

// memberPod returns a running, ready Pod of an earlier version: member of a
// Role instance of the group at ordinal, controlled by ms.
func memberPod(ms *workloadv1alpha1.ModelServing, ordinal int, roleID string, member int, revision string, mutate ...func(*corev1.Pod)) *corev1.Pod {
	group := utils.GenerateServingGroupName(ms.Name, ordinal)
	role, _, _ := splitOrdinal(roleID)
	name := utils.GeneratePodName(group, roleID, member)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
				workloadv1alpha1.GroupNameLabelKey:        group,
				workloadv1alpha1.RoleLabelKey:             role,
				workloadv1alpha1.RoleIDKey:                roleID,
				workloadv1alpha1.RevisionLabelKey:         revision,
			},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)},
		},
		Spec: corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "model", Image: "model:v1"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue, LastTransitionTime: readySince}},
			ContainerStatuses: []corev1.ContainerStatus{{
				Name: "model", Image: "model:v1", ContainerID: "containerd://" + name, ImageID: "sha256:v1",
			}},
		},
	}
	if member == 0 {
		pod.Labels[workloadv1alpha1.EntryLabelKey] = utils.Entry
	}
	for _, change := range mutate {
		change(pod)
	}
	return pod
}

// groupPods returns the two Pods of the group at ordinal.
func groupPods(ms *workloadv1alpha1.ModelServing, ordinal int, revision string, mutate ...func(*corev1.Pod)) []*corev1.Pod {
	return []*corev1.Pod{
		memberPod(ms, ordinal, "leader-0", 0, revision, mutate...),
		memberPod(ms, ordinal, "leader-0", 1, revision, mutate...),
	}
}

// groupsPods returns the Pods of the groups at the ordinals.
func groupsPods(ms *workloadv1alpha1.ModelServing, revision string, ordinals ...int) []*corev1.Pod {
	var pods []*corev1.Pod
	for _, ordinal := range ordinals {
		pods = append(pods, groupPods(ms, ordinal, revision)...)
	}
	return pods
}

func notReady(pod *corev1.Pod) {
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse, LastTransitionTime: readySince}}
}

func terminating(pod *corev1.Pod) {
	pod.DeletionTimestamp = ptr(metav1.NewTime(testTime.Add(-time.Minute)))
}

func surgeMarked(pod *corev1.Pod) {
	annotate(workloadv1alpha1.SurgeAnnotationKey, workloadv1alpha1.SurgeScopeServingGroup)(pod)
}

func annotate(key, value string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) {
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[key] = value
	}
}

func withoutOwner(pod *corev1.Pod) { pod.OwnerReferences = nil }

func onNode(node string) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.Spec.NodeName = node }
}
