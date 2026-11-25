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
	"fmt"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
)

const (
	statusSubresource = "status"

	verbCreate = "create"
	verbUpdate = "update"
	verbPatch  = "patch"
	verbDelete = "delete"

	// optimisticLockErrorMsg is the message the API server returns when a
	// write carries a stale resourceVersion.
	optimisticLockErrorMsg = "the object has been modified; please apply your changes to the latest version and try again"
)

// react handles the mutating verbs. Reads, watches and everything the Cluster
// does not model are left to the fake clientset's own reactor.
func (c *Cluster) react(fam family, action k8stesting.Action) (bool, runtime.Object, error) {
	switch a := action.(type) {
	case k8stesting.CreateActionImpl:
		switch a.GetSubresource() {
		case "":
			return c.handleCreate(fam, a)
		case statusSubresource:
			// client-go models a status create as an update of the stored object.
			return c.handleUpdate(fam, a.GetResource(), statusSubresource, a.GetNamespace(), a.GetObject())
		default:
			return false, nil, nil
		}
	case k8stesting.UpdateActionImpl:
		if sub := a.GetSubresource(); sub != "" && sub != statusSubresource {
			return false, nil, nil
		}
		return c.handleUpdate(fam, a.GetResource(), a.GetSubresource(), a.GetNamespace(), a.GetObject())
	case k8stesting.PatchActionImpl:
		if sub := a.GetSubresource(); sub != "" && sub != statusSubresource {
			return false, nil, nil
		}
		return c.handlePatch(fam, a)
	case k8stesting.DeleteActionImpl:
		if a.GetSubresource() != "" {
			return false, nil, nil
		}
		return c.handleDelete(fam, a)
	default:
		return false, nil, nil
	}
}

func (c *Cluster) handleCreate(fam family, action k8stesting.CreateActionImpl) (bool, runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gvr := action.GetResource()
	namespace := action.GetNamespace()
	obj := action.GetObject()
	res := resourceFor(fam, gvr, obj)
	tracker := c.trackerFor(fam)

	out := obj.DeepCopyObject()
	m, err := meta.Accessor(out)
	if err != nil {
		return true, nil, err
	}
	if m.GetNamespace() == "" {
		m.SetNamespace(namespace)
	}
	if m.GetName() == "" && m.GetGenerateName() != "" {
		m.SetName(m.GetGenerateName() + c.nextNameSuffix())
	}
	if m.GetName() == "" {
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("%s: name is required", res.gvr.Resource))
	}
	if _, err := tracker.Get(gvr, m.GetNamespace(), m.GetName()); err == nil {
		return true, nil, apierrors.NewAlreadyExists(res.groupResource(), m.GetName())
	} else if !apierrors.IsNotFound(err) {
		return true, nil, err
	}

	write := Write{Verb: verbCreate, Resource: res.gvr.Resource, Namespace: m.GetNamespace(), Name: m.GetName()}
	if err := c.before(write); err != nil {
		return true, nil, err
	}

	if m.GetUID() == "" {
		m.SetUID(c.nextUID())
	}
	m.SetResourceVersion(c.nextResourceVersion())
	m.SetCreationTimestamp(metav1.NewTime(c.Now()))
	m.SetGeneration(1)
	m.SetDeletionTimestamp(nil)
	m.SetDeletionGracePeriodSeconds(nil)

	if err := tracker.Create(gvr, out, m.GetNamespace()); err != nil {
		return true, nil, err
	}
	c.logLocked(res, write)
	return true, out.DeepCopyObject(), nil
}

func (c *Cluster) handleUpdate(fam family, gvr schema.GroupVersionResource, subresource, namespace string, obj runtime.Object) (bool, runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	res := resourceFor(fam, gvr, obj)
	incoming, err := meta.Accessor(obj)
	if err != nil {
		return true, nil, err
	}
	stored, err := c.trackerFor(fam).Get(gvr, namespace, incoming.GetName())
	if err != nil {
		return true, nil, err
	}
	storedMeta, err := meta.Accessor(stored)
	if err != nil {
		return true, nil, err
	}
	if err := checkOptimisticConcurrency(res, storedMeta, incoming.GetUID(), incoming.GetResourceVersion()); err != nil {
		return true, nil, err
	}
	out, err := c.applyWriteLocked(res, verbUpdate, subresource, namespace, stored, obj)
	if err != nil {
		return true, nil, err
	}
	return true, out, nil
}

// applyWriteLocked merges an incoming object into the stored one following the
// subresource rules, runs the BeforeWrite hook, stores the result and logs it.
func (c *Cluster) applyWriteLocked(res *resourceInfo, verb, subresource, namespace string, stored, incoming runtime.Object) (runtime.Object, error) {
	storedMeta, err := meta.Accessor(stored)
	if err != nil {
		return nil, err
	}

	var out runtime.Object
	if subresource == statusSubresource {
		// A write through status changes nothing but status.
		out = stored.DeepCopyObject()
		copyField(out, incoming, "Status")
	} else {
		out = incoming.DeepCopyObject()
		if res.hasStatus {
			// A write through the main resource never changes status.
			copyField(out, stored, "Status")
		}
	}

	outMeta, err := meta.Accessor(out)
	if err != nil {
		return nil, err
	}
	// Metadata the API server owns cannot be changed by a write.
	outMeta.SetName(storedMeta.GetName())
	outMeta.SetNamespace(storedMeta.GetNamespace())
	outMeta.SetUID(storedMeta.GetUID())
	outMeta.SetCreationTimestamp(storedMeta.GetCreationTimestamp())
	outMeta.SetDeletionTimestamp(storedMeta.GetDeletionTimestamp())
	outMeta.SetDeletionGracePeriodSeconds(storedMeta.GetDeletionGracePeriodSeconds())
	outMeta.SetGeneration(storedMeta.GetGeneration())
	if subresource == "" && res.specGeneration && !apiequality.Semantic.DeepEqual(specOf(stored), specOf(out)) {
		outMeta.SetGeneration(storedMeta.GetGeneration() + 1)
	}

	write := Write{
		Verb:        verb,
		Resource:    res.gvr.Resource,
		Subresource: subresource,
		Namespace:   namespace,
		Name:        outMeta.GetName(),
	}
	if err := c.before(write); err != nil {
		return nil, err
	}

	outMeta.SetResourceVersion(c.nextResourceVersion())
	tracker := c.trackerFor(res.fam)
	if err := tracker.Update(res.gvr, out, namespace); err != nil {
		return nil, err
	}
	// Removing the last finalizer of a terminating object completes its deletion.
	if outMeta.GetDeletionTimestamp() != nil && len(outMeta.GetFinalizers()) == 0 && !c.podTerminationHeldLocked(res) {
		if err := tracker.Delete(res.gvr, namespace, outMeta.GetName()); err != nil {
			return nil, err
		}
	}
	c.logLocked(res, write)
	return out.DeepCopyObject(), nil
}

func (c *Cluster) handleDelete(fam family, action k8stesting.DeleteActionImpl) (bool, runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gvr := action.GetResource()
	namespace := action.GetNamespace()
	name := action.GetName()

	stored, err := c.trackerFor(fam).Get(gvr, namespace, name)
	if err != nil {
		return true, nil, err
	}
	res := resourceFor(fam, gvr, stored)
	storedMeta, err := meta.Accessor(stored)
	if err != nil {
		return true, nil, err
	}

	opts := action.DeleteOptions
	if pre := opts.Preconditions; pre != nil {
		var uid types.UID
		rv := ""
		if pre.UID != nil {
			uid = *pre.UID
		}
		if pre.ResourceVersion != nil {
			rv = *pre.ResourceVersion
		}
		if err := checkOptimisticConcurrency(res, storedMeta, uid, rv); err != nil {
			return true, nil, err
		}
	}

	write := Write{Verb: verbDelete, Resource: res.gvr.Resource, Namespace: namespace, Name: name}
	if err := c.before(write); err != nil {
		return true, nil, err
	}
	if _, err := c.deleteLocked(res, namespace, name, propagationPolicy(opts), opts.GracePeriodSeconds); err != nil {
		return true, nil, err
	}
	c.logLocked(res, write)
	return true, nil, nil
}

// propagationPolicy returns the effective deletion propagation; the kinds this
// package models default to background propagation.
func propagationPolicy(opts metav1.DeleteOptions) metav1.DeletionPropagation {
	if opts.PropagationPolicy == nil {
		return metav1.DeletePropagationBackground
	}
	return *opts.PropagationPolicy
}

func (c *Cluster) before(w Write) error {
	if c.BeforeWrite == nil {
		return nil
	}
	return c.BeforeWrite(w)
}

func (c *Cluster) podTerminationHeldLocked(res *resourceInfo) bool {
	return res.gvr == podsGVR && c.holdPodTermination
}

// checkOptimisticConcurrency rejects a write whose UID or resourceVersion does
// not match the stored object. Empty values are unconditional, as in a real
// API server.
func checkOptimisticConcurrency(res *resourceInfo, stored metav1.Object, uid types.UID, resourceVersion string) error {
	if uid != "" && uid != stored.GetUID() {
		return apierrors.NewConflict(res.groupResource(), stored.GetName(),
			fmt.Errorf("precondition failed: UID in precondition: %v, UID in object meta: %v", uid, stored.GetUID()))
	}
	if resourceVersion != "" && resourceVersion != stored.GetResourceVersion() {
		return apierrors.NewConflict(res.groupResource(), stored.GetName(), errors.New(optimisticLockErrorMsg))
	}
	return nil
}

func specOf(obj runtime.Object) any {
	f := structField(obj, "Spec")
	if !f.IsValid() {
		return nil
	}
	return f.Interface()
}
