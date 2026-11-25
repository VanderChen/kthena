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

// Package objcache is the view of the cluster that the ModelServing and the
// ServingGroup controller read: the stores of their shared informers, with the
// indexes both of them use.
package objcache

import (
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	// ControllerIndex indexes an object by the UID of its controller owner.
	ControllerIndex = "controller"
	// GroupIndex indexes a Pod that no ServingGroup controls by
	// <namespace>/<value of the group-name label>: the Pods a group may have to adopt.
	GroupIndex = "group"
	// ModelServingIndex indexes an object by <namespace>/<value of the ModelServing name label>.
	// A Pod is only indexed while no ServingGroup controls it.
	ModelServingIndex = "modelserving"
	// GroupObjectIndex indexes a Pod that a ServingGroup controls by
	// <namespace>/<name of that ServingGroup>: the Pods of every object that
	// has had the name of a group.
	GroupObjectIndex = "group-object"
	// OrdinalNameIndex indexes a ServingGroup named <prefix>-<ordinal> by
	// <namespace>/<prefix>: the objects that take the names of the groups of a
	// ModelServing called <prefix>, whoever made them.
	OrdinalNameIndex = "ordinal-name"
)

// Caches holds the stores the controllers read. In production each is the
// store of an informer.
type Caches struct {
	ModelServings       cache.Indexer
	ServingGroups       cache.Indexer
	Pods                cache.Indexer
	ControllerRevisions cache.Indexer
}

// AddIndexers adds the indexes the accessors below use. It must be called
// once, before the stores are read.
func (c Caches) AddIndexers() error {
	for _, add := range []struct {
		store    cache.Indexer
		indexers cache.Indexers
	}{
		{c.Pods, cache.Indexers{
			ControllerIndex: controllerIndex, GroupIndex: unadoptedGroupIndex, ModelServingIndex: unadoptedModelServingIndex,
			GroupObjectIndex: groupObjectIndex,
		}},
		{c.ServingGroups, cache.Indexers{ControllerIndex: controllerIndex, ModelServingIndex: modelServingIndex, OrdinalNameIndex: ordinalNameIndex}},
		{c.ControllerRevisions, cache.Indexers{ModelServingIndex: modelServingIndex}},
	} {
		if err := add.store.AddIndexers(add.indexers); err != nil {
			return err
		}
	}
	return nil
}

func controllerIndex(obj any) ([]string, error) {
	object, ok := obj.(metav1.Object)
	if !ok {
		return nil, nil
	}
	if owner := metav1.GetControllerOfNoCopy(object); owner != nil {
		return []string{string(owner.UID)}, nil
	}
	return nil, nil
}

// unadopted returns the Pod in obj when no ServingGroup controls it.
func unadopted(obj any) *corev1.Pod {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}
	if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.Kind == workloadv1alpha1.ServingGroupKind.Kind {
		return nil
	}
	return pod
}

func groupObjectIndex(obj any) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.Kind == workloadv1alpha1.ServingGroupKind.Kind {
		return []string{pod.Namespace + "/" + owner.Name}, nil
	}
	return nil, nil
}

func unadoptedGroupIndex(obj any) ([]string, error) {
	if pod := unadopted(obj); pod != nil && pod.Labels[workloadv1alpha1.GroupNameLabelKey] != "" {
		return []string{pod.Namespace + "/" + pod.Labels[workloadv1alpha1.GroupNameLabelKey]}, nil
	}
	return nil, nil
}

func unadoptedModelServingIndex(obj any) ([]string, error) {
	if pod := unadopted(obj); pod != nil {
		return modelServingIndex(pod)
	}
	return nil, nil
}

// OrdinalName splits a name of the form <prefix>-<ordinal>. The ordinal is
// written without a sign and without leading zeros.
func OrdinalName(name string) (prefix string, ordinal int, ok bool) {
	cut := strings.LastIndexByte(name, '-')
	if cut <= 0 {
		return "", 0, false
	}
	ordinal, err := strconv.Atoi(name[cut+1:])
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != name[cut+1:] {
		return "", 0, false
	}
	return name[:cut], ordinal, true
}

func ordinalNameIndex(obj any) ([]string, error) {
	object, ok := obj.(metav1.Object)
	if !ok {
		return nil, nil
	}
	if prefix, _, ok := OrdinalName(object.GetName()); ok {
		return []string{object.GetNamespace() + "/" + prefix}, nil
	}
	return nil, nil
}

func modelServingIndex(obj any) ([]string, error) {
	object, ok := obj.(metav1.Object)
	if !ok {
		return nil, nil
	}
	if name := object.GetLabels()[workloadv1alpha1.ModelServingNameLabelKey]; name != "" {
		return []string{object.GetNamespace() + "/" + name}, nil
	}
	return nil, nil
}

func byIndex[T any](store cache.Indexer, index, key string) []T {
	objects, err := store.ByIndex(index, key)
	if err != nil {
		// The index is added by AddIndexers; a missing one is a programming error.
		panic(fmt.Errorf("objcache: %w", err))
	}
	out := make([]T, 0, len(objects))
	for _, object := range objects {
		if typed, ok := object.(T); ok {
			out = append(out, typed)
		}
	}
	return out
}

func get[T any](store cache.Indexer, resource schema.GroupResource, namespace, name string) (T, error) {
	var zero T
	object, exists, err := store.GetByKey(namespace + "/" + name)
	if err != nil {
		return zero, err
	}
	if !exists {
		return zero, apierrors.NewNotFound(resource, name)
	}
	typed, ok := object.(T)
	if !ok {
		return zero, fmt.Errorf("objcache: %s %s/%s has type %T", resource, namespace, name, object)
	}
	return typed, nil
}

// ModelServing returns the ModelServing of that name, or a NotFound error.
func (c Caches) ModelServing(namespace, name string) (*workloadv1alpha1.ModelServing, error) {
	return get[*workloadv1alpha1.ModelServing](c.ModelServings, workloadv1alpha1.Resource("modelservings"), namespace, name)
}

// ServingGroup returns the ServingGroup of that name, or a NotFound error.
func (c Caches) ServingGroup(namespace, name string) (*workloadv1alpha1.ServingGroup, error) {
	return get[*workloadv1alpha1.ServingGroup](c.ServingGroups, workloadv1alpha1.Resource("servinggroups"), namespace, name)
}

// ControllerRevision returns the ControllerRevision of that name, or a
// NotFound error. It is a workload.Getter.
func (c Caches) ControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error) {
	return get[*appsv1.ControllerRevision](c.ControllerRevisions, appsv1.Resource("controllerrevisions"), namespace, name)
}

// GroupsControlledBy returns the ServingGroups whose controller has that UID.
func (c Caches) GroupsControlledBy(uid types.UID) []*workloadv1alpha1.ServingGroup {
	return byIndex[*workloadv1alpha1.ServingGroup](c.ServingGroups, ControllerIndex, string(uid))
}

// GroupsLabelled returns the ServingGroups that carry the name of a
// ModelServing in their labels, whoever controls them.
func (c Caches) GroupsLabelled(namespace, modelServing string) []*workloadv1alpha1.ServingGroup {
	return byIndex[*workloadv1alpha1.ServingGroup](c.ServingGroups, ModelServingIndex, namespace+"/"+modelServing)
}

// GroupsNamedAfter returns the ServingGroups whose name is that of a group of
// the ModelServing called modelServing, <modelServing>-<ordinal>, whoever
// controls them and whatever their labels are.
func (c Caches) GroupsNamedAfter(namespace, modelServing string) []*workloadv1alpha1.ServingGroup {
	return byIndex[*workloadv1alpha1.ServingGroup](c.ServingGroups, OrdinalNameIndex, namespace+"/"+modelServing)
}

// UnadoptedPodsOf returns the Pods that carry the name of a ModelServing in
// their labels and are not controlled by a ServingGroup: Pods created before
// groups were objects, and Pods left behind without an owner.
func (c Caches) UnadoptedPodsOf(namespace, modelServing string) []*corev1.Pod {
	return byIndex[*corev1.Pod](c.Pods, ModelServingIndex, namespace+"/"+modelServing)
}

// RevisionsLabelled returns the ControllerRevisions that carry the name of a
// ModelServing in their labels, whoever controls them.
func (c Caches) RevisionsLabelled(namespace, modelServing string) []*appsv1.ControllerRevision {
	return byIndex[*appsv1.ControllerRevision](c.ControllerRevisions, ModelServingIndex, namespace+"/"+modelServing)
}

// PodsControlledBy returns the Pods whose controller has that UID.
func (c Caches) PodsControlledBy(uid types.UID) []*corev1.Pod {
	return byIndex[*corev1.Pod](c.Pods, ControllerIndex, string(uid))
}

// UnadoptedPods returns the Pods that carry the name of a group in their
// labels and are not controlled by a ServingGroup.
func (c Caches) UnadoptedPods(namespace, group string) []*corev1.Pod {
	return byIndex[*corev1.Pod](c.Pods, GroupIndex, namespace+"/"+group)
}

// PodsOfGroup returns the Pods that may be members of a group: those the
// group controls and those that carry its name without being controlled by a
// ServingGroup. Which of them are members is for servinggroup.MembershipOf to say.
func (c Caches) PodsOfGroup(group *workloadv1alpha1.ServingGroup) []*corev1.Pod {
	return append(c.PodsControlledBy(group.UID), c.UnadoptedPods(group.Namespace, group.Name)...)
}

// PodsOfEarlierObjects returns the Pods that a ServingGroup of the name of
// group with another UID controls: what is left of an object that was deleted
// without waiting for its Pods.
func (c Caches) PodsOfEarlierObjects(group *workloadv1alpha1.ServingGroup) []*corev1.Pod {
	var earlier []*corev1.Pod
	for _, pod := range byIndex[*corev1.Pod](c.Pods, GroupObjectIndex, group.Namespace+"/"+group.Name) {
		if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.UID != group.UID {
			earlier = append(earlier, pod)
		}
	}
	return earlier
}
