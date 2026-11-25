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

package apitest

import (
	"errors"
	"sort"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// errNoOwnerReference reports that an object did not reference the owner the
// collector tried to release it from.
var errNoOwnerReference = errors.New("apitest: object has no such owner reference")

// settlePasses bounds Settle so that a bug in a test fixture cannot hang the
// test binary.
const settlePasses = 100

// tracked is one object currently stored in one of the trackers.
type tracked struct {
	res  *resourceInfo
	meta metav1.Object
}

// deleteLocked performs a server-side delete of a single object. It reports
// whether the object was removed from the store; an object that keeps
// finalizers (or a Pod whose termination is held) stays behind with a
// deletionTimestamp instead.
func (c *Cluster) deleteLocked(res *resourceInfo, namespace, name string, policy metav1.DeletionPropagation, gracePeriod *int64) (bool, error) {
	tracker := c.trackerFor(res.fam)
	obj, err := tracker.Get(res.gvr, namespace, name)
	if err != nil {
		return false, err
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return false, err
	}

	finalizers := m.GetFinalizers()
	switch policy {
	case metav1.DeletePropagationForeground:
		finalizers = appendFinalizer(finalizers, metav1.FinalizerDeleteDependents)
	case metav1.DeletePropagationOrphan:
		finalizers = appendFinalizer(finalizers, metav1.FinalizerOrphanDependents)
	}

	if len(finalizers) == 0 && !c.podTerminationHeldLocked(res) {
		if err := tracker.Delete(res.gvr, namespace, name); err != nil {
			return false, err
		}
		return true, nil
	}

	if m.GetDeletionTimestamp() == nil {
		deletionTimestamp := metav1.NewTime(c.Now())
		m.SetDeletionTimestamp(&deletionTimestamp)
		if res.gvr == podsGVR {
			grace := defaultPodGracePeriodSeconds
			if gracePeriod != nil {
				grace = *gracePeriod
			}
			m.SetDeletionGracePeriodSeconds(&grace)
		}
	}
	m.SetFinalizers(finalizers)
	m.SetResourceVersion(c.nextResourceVersion())
	if err := tracker.Update(res.gvr, obj, namespace); err != nil {
		return false, err
	}
	return false, nil
}

// CollectGarbage runs one pass of the garbage collector and returns the number
// of objects it deleted or released. A pass is deliberately stepwise: an owner
// that is deleting in the foreground never disappears in the same pass as its
// dependents, so tests can observe the intermediate states. Use Settle to run
// passes until the cluster is quiet.
func (c *Cluster) CollectGarbage() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.collectLocked()
}

// Settle runs CollectGarbage until a pass does nothing.
func (c *Cluster) Settle() {
	for i := 0; i < settlePasses; i++ {
		if c.CollectGarbage() == 0 {
			return
		}
	}
}

func (c *Cluster) collectLocked() int {
	collected := 0

	// Dependents whose owners are all gone are deleted. An owner that exists
	// under the same name but with a different UID counts as gone.
	for _, obj := range c.allObjectsLocked() {
		if len(obj.meta.GetOwnerReferences()) == 0 || obj.meta.GetDeletionTimestamp() != nil {
			continue
		}
		if c.anyOwnerExistsLocked(obj) {
			continue
		}
		if _, err := c.deleteLocked(obj.res, obj.meta.GetNamespace(), obj.meta.GetName(), metav1.DeletePropagationBackground, nil); err == nil {
			collected++
		}
	}

	// Owners deleting in the foreground delete their dependents first and
	// disappear once no dependent blocks them.
	for _, obj := range c.allObjectsLocked() {
		if !terminatingWith(obj.meta, metav1.FinalizerDeleteDependents) {
			continue
		}
		deleted := 0
		for _, dep := range c.dependentsOfLocked(obj.meta) {
			if dep.meta.GetDeletionTimestamp() != nil {
				continue
			}
			if _, err := c.deleteLocked(dep.res, dep.meta.GetNamespace(), dep.meta.GetName(), metav1.DeletePropagationBackground, nil); err == nil {
				deleted++
			}
		}
		if deleted > 0 {
			collected += deleted
			continue
		}
		if c.blockedByDependentsLocked(obj.meta) {
			continue
		}
		if err := c.removeFinalizerLocked(obj.res, obj.meta.GetNamespace(), obj.meta.GetName(), metav1.FinalizerDeleteDependents); err == nil {
			collected++
		}
	}

	// Owners deleting with orphan propagation release their dependents first.
	for _, obj := range c.allObjectsLocked() {
		if !terminatingWith(obj.meta, metav1.FinalizerOrphanDependents) {
			continue
		}
		released := 0
		for _, dep := range c.dependentsOfLocked(obj.meta) {
			if err := c.removeOwnerReferenceLocked(dep.res, dep.meta.GetNamespace(), dep.meta.GetName(), obj.meta.GetUID()); err == nil {
				released++
			}
		}
		if released > 0 {
			collected += released
			continue
		}
		if err := c.removeFinalizerLocked(obj.res, obj.meta.GetNamespace(), obj.meta.GetName(), metav1.FinalizerOrphanDependents); err == nil {
			collected++
		}
	}

	return collected
}

// allObjectsLocked lists every modelled object across the three trackers in a
// deterministic order.
func (c *Cluster) allObjectsLocked() []tracked {
	var out []tracked
	for _, res := range modelledResources {
		list, err := c.trackerFor(res.fam).List(res.gvr, res.gvk, metav1.NamespaceAll)
		if err != nil {
			continue
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			continue
		}
		objects := make([]tracked, 0, len(items))
		for _, item := range items {
			m, err := meta.Accessor(item)
			if err != nil {
				continue
			}
			objects = append(objects, tracked{res: res, meta: m})
		}
		sort.Slice(objects, func(i, j int) bool {
			if objects[i].meta.GetNamespace() != objects[j].meta.GetNamespace() {
				return objects[i].meta.GetNamespace() < objects[j].meta.GetNamespace()
			}
			return objects[i].meta.GetName() < objects[j].meta.GetName()
		})
		out = append(out, objects...)
	}
	return out
}

// anyOwnerExistsLocked reports whether at least one owner of the object is
// still around. Owners of kinds the Cluster does not model are assumed to
// exist, so that unknown references never trigger a collection.
func (c *Cluster) anyOwnerExistsLocked(obj tracked) bool {
	for _, ref := range obj.meta.GetOwnerReferences() {
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil {
			return true
		}
		res, ok := resourceByGVK[gv.WithKind(ref.Kind)]
		if !ok {
			return true
		}
		owner, err := c.trackerFor(res.fam).Get(res.gvr, obj.meta.GetNamespace(), ref.Name)
		if err != nil {
			continue
		}
		ownerMeta, err := meta.Accessor(owner)
		if err != nil {
			continue
		}
		if ownerMeta.GetUID() == ref.UID {
			return true
		}
	}
	return false
}

// dependentsOfLocked returns the objects in the owner's namespace that
// reference it by UID.
func (c *Cluster) dependentsOfLocked(owner metav1.Object) []tracked {
	var out []tracked
	for _, obj := range c.allObjectsLocked() {
		if obj.meta.GetNamespace() != owner.GetNamespace() || obj.meta.GetUID() == owner.GetUID() {
			continue
		}
		if ownerReferenceTo(obj.meta, owner.GetUID()) != nil {
			out = append(out, obj)
		}
	}
	return out
}

// blockedByDependentsLocked reports whether a dependent with
// blockOwnerDeletion still keeps the owner alive.
func (c *Cluster) blockedByDependentsLocked(owner metav1.Object) bool {
	for _, dep := range c.dependentsOfLocked(owner) {
		ref := ownerReferenceTo(dep.meta, owner.GetUID())
		if ref != nil && ref.BlockOwnerDeletion != nil && *ref.BlockOwnerDeletion {
			return true
		}
	}
	return false
}

func (c *Cluster) removeFinalizerLocked(res *resourceInfo, namespace, name, finalizer string) error {
	tracker := c.trackerFor(res.fam)
	obj, err := tracker.Get(res.gvr, namespace, name)
	if err != nil {
		return err
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	remaining := make([]string, 0, len(m.GetFinalizers()))
	for _, f := range m.GetFinalizers() {
		if f != finalizer {
			remaining = append(remaining, f)
		}
	}
	if len(remaining) == 0 && !c.podTerminationHeldLocked(res) {
		return tracker.Delete(res.gvr, namespace, name)
	}
	m.SetFinalizers(remaining)
	m.SetResourceVersion(c.nextResourceVersion())
	return tracker.Update(res.gvr, obj, namespace)
}

func (c *Cluster) removeOwnerReferenceLocked(res *resourceInfo, namespace, name string, uid types.UID) error {
	tracker := c.trackerFor(res.fam)
	obj, err := tracker.Get(res.gvr, namespace, name)
	if err != nil {
		return err
	}
	m, err := meta.Accessor(obj)
	if err != nil {
		return err
	}
	refs := m.GetOwnerReferences()
	remaining := make([]metav1.OwnerReference, 0, len(refs))
	for _, ref := range refs {
		if ref.UID != uid {
			remaining = append(remaining, ref)
		}
	}
	if len(remaining) == len(refs) {
		return errNoOwnerReference
	}
	m.SetOwnerReferences(remaining)
	m.SetResourceVersion(c.nextResourceVersion())
	return tracker.Update(res.gvr, obj, namespace)
}

func terminatingWith(m metav1.Object, finalizer string) bool {
	if m.GetDeletionTimestamp() == nil {
		return false
	}
	for _, f := range m.GetFinalizers() {
		if f == finalizer {
			return true
		}
	}
	return false
}

func ownerReferenceTo(m metav1.Object, uid types.UID) *metav1.OwnerReference {
	refs := m.GetOwnerReferences()
	for i := range refs {
		if refs[i].UID == uid {
			return &refs[i]
		}
	}
	return nil
}

func appendFinalizer(finalizers []string, finalizer string) []string {
	for _, f := range finalizers {
		if f == finalizer {
			return finalizers
		}
	}
	return append(finalizers, finalizer)
}
