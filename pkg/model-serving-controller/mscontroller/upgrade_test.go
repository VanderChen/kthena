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
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func modelServingOwner(ms *workloadv1alpha1.ModelServing) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
		Name: ms.Name, UID: ms.UID, Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

// legacyRevision returns the ControllerRevision an earlier controller recorded
// for ms under the identity revision: in the format of release 1.1, the Roles
// alone, or in the format of the controller before groups were objects.
func legacyRevision(t *testing.T, ms *workloadv1alpha1.ModelServing, revision string, release11 bool) *appsv1.ControllerRevision {
	t.Helper()
	cr := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ms.Namespace, Name: utils.GenerateControllerRevisionName(ms.Name, revision),
			Labels: map[string]string{
				utils.ControllerRevisionLabelKey:         ms.Name,
				utils.ControllerRevisionRevisionLabelKey: revision,
			},
			OwnerReferences: []metav1.OwnerReference{modelServingOwner(ms)},
		},
		Revision: 1,
	}
	var err error
	if release11 {
		cr.Data.Raw, err = json.Marshal(map[string]any{"data": ms.Spec.Template.Roles})
	} else {
		cr.Annotations = map[string]string{utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1}
		cr.Data.Raw, err = utils.BuildRevisionData(ms)
	}
	if err != nil {
		t.Fatal(err)
	}
	return cr
}

// legacyPods returns the two Pods of group ordinal as an earlier controller
// created them: controlled by the ModelServing, without an instance generation.
func legacyPods(ms *workloadv1alpha1.ModelServing, ordinal int, revision, image string, surge bool) []runtime.Object {
	var pods []runtime.Object
	group := fmt.Sprintf("%s-%d", ms.Name, ordinal)
	for member := range 2 {
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: ms.Namespace, Name: fmt.Sprintf("%s-serve-0-%d", group, member),
				Labels: map[string]string{
					workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
					workloadv1alpha1.GroupNameLabelKey:        group,
					workloadv1alpha1.RoleLabelKey:             "serve",
					workloadv1alpha1.RoleIDKey:                "serve-0",
					workloadv1alpha1.RevisionLabelKey:         revision,
				},
				OwnerReferences: []metav1.OwnerReference{modelServingOwner(ms)},
			},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: image}}},
			Status: corev1.PodStatus{
				Phase:      corev1.PodRunning,
				Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			},
		}
		if member == 0 {
			pod.Labels[workloadv1alpha1.EntryLabelKey] = utils.Entry
		}
		if surge {
			pod.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
		}
		pods = append(pods, pod)
	}
	return pods
}

// What an earlier version of the controller left behind is taken over without
// a Pod being created, deleted or restarted, whichever format its revision
// history has.
func TestWorkloadsOfAnEarlierVersionAreTakenOver(t *testing.T) {
	for name, release11 := range map[string]bool{"release 1.1": true, "the controller before group objects": false} {
		t.Run(name, func(t *testing.T) {
			ms := modelServing()
			ms.UID = "ms-uid"
			ms.Status = workloadv1alpha1.ModelServingStatus{
				Replicas: 3, UpdatedReplicas: 3, CurrentReplicas: 3, AvailableReplicas: 3,
				CurrentRevision: "old-identity", UpdateRevision: "old-identity",
			}
			objects := []runtime.Object{ms, legacyRevision(t, ms, "old-identity", release11)}
			for ordinal := range 3 {
				objects = append(objects, legacyPods(ms, ordinal, "old-identity", "model:v1", false)...)
			}
			s := newSystem(t, objects...)
			uids := map[string]types.UID{}
			for _, pod := range s.pods() {
				uids[pod.Name] = pod.UID
			}

			// While the workloads are taken over, the counts users see do not move.
			s.invariant = func() {
				if got := s.counts(); got != "replicas=3 updated=3 current=3 available=3" {
					t.Fatalf("during the take-over: %s", got)
				}
			}
			s.settle()
			s.invariant = nil

			for _, write := range s.cluster.Writes() {
				if write.Resource == "pods" && write.Verb != "patch" || write.Resource == "controllerrevisions" && write.Verb == "create" {
					t.Errorf("the take-over issued: %s", write)
				}
			}
			if got, want := s.summary(), "groups[0=model:v1 1=model:v1 2=model:v1] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Fatalf("after the take-over:\n got %s\nwant %s", got, want)
			}
			taken := s.modelServing()
			if got := taken.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; got != workloadv1alpha1.ServingGroupAdoptionDone {
				t.Errorf("adoption annotation = %q, want Done", got)
			}
			if taken.Status.UpdateRevision != "old-identity" || taken.Status.CurrentRevision != "old-identity" {
				t.Errorf("revisions = %s, %s; want the identity the earlier version recorded", taken.Status.CurrentRevision, taken.Status.UpdateRevision)
			}
			groups := map[string]types.UID{}
			for _, group := range s.groups() {
				groups[group.Name] = group.UID
				if group.Spec.Revision != "old-identity" || !groupReady(group) {
					t.Errorf("group %s: revision %s, ready %t", group.Name, group.Spec.Revision, groupReady(group))
				}
			}
			for _, pod := range s.pods() {
				owner := metav1.GetControllerOf(pod)
				if pod.UID != uids[pod.Name] || owner == nil || owner.UID != groups[pod.Labels[workloadv1alpha1.GroupNameLabelKey]] {
					t.Errorf("Pod %s: same Pod = %t, controller %+v", pod.Name, pod.UID == uids[pod.Name], owner)
				}
			}

			// The ModelServing is then rolled like any other.
			s.update(setImage("model:v2"))
			s.settle()
			if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
				t.Fatalf("rolled after the take-over:\n got %s\nwant %s", got, want)
			}
		})
	}
}

// An upgrade in the middle of a rolling update with surge: the surge group is
// recognised by the mark on its Pods and the rollout goes on from where it was.
func TestARolloutUnderWayIsTakenOver(t *testing.T) {
	old := modelServing()
	old.UID = "ms-uid"
	ms := modelServing(setImage("model:v2"), rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxSurge = ptr.To(intstrOne)
	}))
	ms.UID = "ms-uid"
	ms.Status = workloadv1alpha1.ModelServingStatus{
		Replicas: 4, UpdatedReplicas: 2, CurrentReplicas: 2, AvailableReplicas: 4,
		CurrentRevision: "rev-old", UpdateRevision: "rev-new",
	}
	objects := []runtime.Object{ms, legacyRevision(t, old, "rev-old", false), legacyRevision(t, ms, "rev-new", false)}
	objects = append(objects, legacyPods(ms, 0, "rev-old", "model:v1", false)...)
	objects = append(objects, legacyPods(ms, 1, "rev-old", "model:v1", false)...)
	objects = append(objects, legacyPods(ms, 2, "rev-new", "model:v2", false)...)
	objects = append(objects, legacyPods(ms, 3, "rev-new", "model:v2", true)...)
	s := newSystem(t, objects...)

	// The first passes only take over: until the take-over is recorded as
	// done, no group is deleted and none is created beyond the four that exist
	// as Pods.
	taken := ""
	for range 10 {
		s.kubelet()
		s.cluster.Settle()
		taken = s.images()
		s.pass()
		if s.modelServing().Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionDone {
			break
		}
	}
	if want := "0=model:v1 1=model:v1 2=model:v2 3=model:v2+"; taken != want {
		t.Fatalf("taken over: %s, want %s", taken, want)
	}

	s.invariant = func() {
		if ready := s.readyGroups(); ready < 2 || len(s.groups()) > 4 {
			t.Fatalf("%d groups ready, %d exist: %s", ready, len(s.groups()), s.summary())
		}
	}
	s.settle()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
	if status := s.modelServing().Status; status.CurrentRevision != "rev-new" || status.UpdateRevision != "rev-new" {
		t.Errorf("revisions = %s, %s", status.CurrentRevision, status.UpdateRevision)
	}
	for _, write := range s.cluster.Writes() {
		if strings.HasPrefix(write.Name, "llama-2-") && write.Resource == "pods" && write.Verb != "patch" {
			t.Errorf("the group that was at the target already was touched: %s", write)
		}
	}
}
