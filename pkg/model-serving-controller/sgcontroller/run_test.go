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
	"reflect"
	"sort"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// handlerKeeper is an informer that keeps the handlers it is given, so that
// a test can deliver an event to them.
type handlerKeeper struct {
	cache.SharedIndexInformer
	handlers []cache.ResourceEventHandler
}

func (k *handlerKeeper) AddEventHandler(handler cache.ResourceEventHandler) (cache.ResourceEventHandlerRegistration, error) {
	k.handlers = append(k.handlers, handler)
	return nil, nil
}

func (k *handlerKeeper) added(obj any) {
	for _, handler := range k.handlers {
		handler.OnAdd(obj, false)
	}
}

func (k *handlerKeeper) updated(old, current any) {
	for _, handler := range k.handlers {
		handler.OnUpdate(old, current)
	}
}

func (k *handlerKeeper) deleted(obj any) {
	for _, handler := range k.handlers {
		handler.OnDelete(obj)
	}
}

// An event starts the round of the group it concerns, and of no other. For
// what a group owns that is the group that controls the object. A Pod, a
// Service or a PodGroup that no ServingGroup controls concerns the group its
// labels name: it may be one the group has to take over, from an earlier
// version or after it was released. A ControllerRevision that appears
// concerns every group of its ModelServing: a revision that could not be
// resolved may be resolved now. A group that misses an event waits for the
// next one, which may never come.
func TestEventsReachTheGroupTheyConcern(t *testing.T) {
	controlledBy := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: name, UID: types.UID(name + "-uid"), Controller: ptr.To(true)}}
	}
	meta := func(name, group string, owners []metav1.OwnerReference) metav1.ObjectMeta {
		object := metav1.ObjectMeta{Namespace: testNamespace, Name: name, OwnerReferences: owners}
		if group != "" {
			object.Labels = map[string]string{workloadv1alpha1.GroupNameLabelKey: group}
		}
		return object
	}
	pod := func(name, group string, owners []metav1.OwnerReference) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: meta(name, group, owners)}
	}
	groupOfName := func(ms, name string) *workloadv1alpha1.ServingGroup {
		return &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace, Name: name, UID: types.UID(name + "-uid"),
			Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms},
		}}
	}
	revision := func(ms string) *appsv1.ControllerRevision {
		return &appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{
			Namespace: testNamespace, Name: ms + "-rev", Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms},
		}}
	}
	modelServing := func(mutate func(*workloadv1alpha1.ModelServing)) *workloadv1alpha1.ModelServing {
		ms := testModelServing()
		if mutate != nil {
			mutate(ms)
		}
		return ms
	}
	byGroup, byModelServing := "ServingGroup", "ModelServing"

	type informers struct{ groups, modelServings, pods, services, revisions, podGroups *handlerKeeper }
	tests := []struct {
		name  string
		event func(informers)
		want  []string
	}{
		// What a group owns.
		{name: "a Pod a group controls", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.pods.added(pod("llama-0-prefill-0-0", "llama-0", controlledBy(byGroup, "llama-0")))
			}},
		{name: "a Pod a group controls, whatever its label names", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.pods.added(pod("llama-0-prefill-0-0", "llama-7", controlledBy(byGroup, "llama-0")))
			}},
		{name: "a Pod without a controller that names a group: one to adopt", want: []string{"default/llama-1"},
			event: func(i informers) { i.pods.added(pod("llama-1-prefill-0-0", "llama-1", nil)) }},
		{name: "a Pod the ModelServing controls that names a group: one to take over", want: []string{"default/llama-1"},
			event: func(i informers) {
				i.pods.added(pod("llama-1-prefill-0-0", "llama-1", controlledBy(byModelServing, "llama")))
			}},
		{name: "a Pod that names no group and has no controller",
			event: func(i informers) { i.pods.added(pod("stranger", "", nil)) }},
		{name: "a Pod that changes: its group", want: []string{"default/llama-0"},
			event: func(i informers) {
				before := pod("llama-0-prefill-0-0", "llama-0", controlledBy(byGroup, "llama-0"))
				i.pods.updated(before, before.DeepCopy())
			}},
		{name: "a Pod that changes its controller: the group it left and the group it joins", want: []string{"default/llama-0", "default/llama-1"},
			event: func(i informers) {
				i.pods.updated(pod("p", "llama-0", controlledBy(byGroup, "llama-0")), pod("p", "llama-0", controlledBy(byGroup, "llama-1")))
			}},
		{name: "a Pod that is released: the group that had it", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.pods.updated(pod("p", "", controlledBy(byGroup, "llama-0")), pod("p", "", nil))
			}},
		{name: "a Pod that is deleted", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.pods.deleted(pod("llama-0-prefill-0-0", "llama-0", controlledBy(byGroup, "llama-0")))
			}},
		{name: "the last state of a Pod whose deletion was missed", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.pods.deleted(cache.DeletedFinalStateUnknown{Key: "default/p", Obj: pod("p", "llama-0", controlledBy(byGroup, "llama-0"))})
			}},
		{name: "something that is no object",
			event: func(i informers) {
				i.pods.added("not an object")
				i.pods.deleted(cache.DeletedFinalStateUnknown{Key: "default/p", Obj: "not an object"})
			}},
		{name: "a Service a group controls", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.services.deleted(&corev1.Service{ObjectMeta: meta("llama-0-prefill-0-0", "llama-0", controlledBy(byGroup, "llama-0"))})
			}},
		{name: "a Service of an earlier version: one to hand over", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.services.added(&corev1.Service{ObjectMeta: meta("llama-0-prefill-0-0", "llama-0", controlledBy(byModelServing, "llama"))})
			}},
		{name: "the PodGroup of a group is deleted: the group makes it again", want: []string{"default/llama-0"},
			event: func(i informers) {
				i.podGroups.deleted(&schedulingv1beta1.PodGroup{ObjectMeta: meta("llama-0", "llama-0", controlledBy(byGroup, "llama-0"))})
			}},
		{name: "a PodGroup of an earlier version: one to hand over", want: []string{"default/llama-1"},
			event: func(i informers) {
				i.podGroups.added(&schedulingv1beta1.PodGroup{ObjectMeta: meta("llama-1", "llama-1", controlledBy(byModelServing, "llama"))})
			}},

		// The group itself.
		{name: "a group that appears", want: []string{"default/llama-1"},
			event: func(i informers) { i.groups.added(groupOfName("llama", "llama-1")) }},
		{name: "a group that changes", want: []string{"default/llama-1"},
			event: func(i informers) { i.groups.updated(groupOfName("llama", "llama-1"), groupOfName("llama", "llama-1")) }},
		{name: "a group that is deleted", want: []string{"default/llama-1"},
			event: func(i informers) {
				i.groups.deleted(cache.DeletedFinalStateUnknown{Key: "default/llama-1", Obj: groupOfName("llama", "llama-1")})
			}},

		// What every group of a ModelServing reads.
		{name: "a ControllerRevision appears: every group of its ModelServing", want: []string{"default/llama-0", "default/llama-1"},
			event: func(i informers) { i.revisions.added(revision("llama")) }},
		{name: "a ControllerRevision of another ModelServing appears", want: []string{"default/qwen-0"},
			event: func(i informers) { i.revisions.added(revision("qwen")) }},
		{name: "a ControllerRevision changes or goes",
			event: func(i informers) {
				i.revisions.updated(revision("llama"), revision("llama"))
				i.revisions.deleted(revision("llama"))
			}},
		{name: "the recovery policy of the ModelServing changes: every group of it", want: []string{"default/llama-0", "default/llama-1"},
			event: func(i informers) {
				i.modelServings.updated(modelServing(nil), modelServing(recoveryPolicy(workloadv1alpha1.NoneRestartPolicy)))
			}},
		{name: "the grace period of the ModelServing changes: every group of it", want: []string{"default/llama-0", "default/llama-1"},
			event: func(i informers) {
				i.modelServings.updated(modelServing(nil), modelServing(func(ms *workloadv1alpha1.ModelServing) {
					ms.Spec.Template.RestartGracePeriodSeconds = ptr.To[int64](30)
				}))
			}},
		{name: "the take-over of the ModelServing moves on: every group of it", want: []string{"default/llama-0", "default/llama-1"},
			event: func(i informers) {
				i.modelServings.updated(modelServing(nil), modelServing(func(ms *workloadv1alpha1.ModelServing) {
					ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionDone}
				}))
			}},
		{name: "the ModelServing changes what reaches a group through its spec",
			event: func(i informers) {
				i.modelServings.added(modelServing(nil))
				i.modelServings.updated(modelServing(nil), modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) }))
				i.modelServings.deleted(modelServing(nil))
			}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, groupOfName("llama", "llama-0"), groupOfName("llama", "llama-1"), groupOfName("qwen", "qwen-0"))
			h.useSnapshot(h.cluster.Snapshot())
			runner := NewRunner(h.controller)
			defer runner.queue.ShutDown()
			i := informers{&handlerKeeper{}, &handlerKeeper{}, &handlerKeeper{}, &handlerKeeper{}, &handlerKeeper{}, &handlerKeeper{}}
			if err := runner.Register(Informers{
				ServingGroups: i.groups, ModelServings: i.modelServings, Pods: i.pods, Services: i.services, ControllerRevisions: i.revisions,
			}); err != nil {
				t.Fatal(err)
			}
			runner.RegisterPodGroups(i.podGroups)

			tt.event(i)
			var got []string
			for runner.queue.Len() > 0 {
				key, _ := runner.queue.Get()
				got = append(got, key)
				runner.queue.Done(key)
			}
			sort.Strings(got)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("queued %v, want %v", got, tt.want)
			}
		})
	}
}
