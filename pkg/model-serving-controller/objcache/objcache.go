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
	ModelServingIndex = "modelserving"
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
		{c.Pods, cache.Indexers{ControllerIndex: controllerIndex, GroupIndex: unadoptedGroupIndex}},
		{c.ServingGroups, cache.Indexers{ControllerIndex: controllerIndex, ModelServingIndex: modelServingIndex}},
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

func unadoptedGroupIndex(obj any) ([]string, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, nil
	}
	group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
	if group == "" {
		return nil, nil
	}
	if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.Kind == workloadv1alpha1.ServingGroupKind.Kind {
		return nil, nil
	}
	return []string{pod.Namespace + "/" + group}, nil
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
