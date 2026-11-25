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
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Informers are the informers whose events start a round.
type Informers struct {
	ServingGroups       cache.SharedIndexInformer
	ModelServings       cache.SharedIndexInformer
	Pods                cache.SharedIndexInformer
	Services            cache.SharedIndexInformer
	ControllerRevisions cache.SharedIndexInformer
}

// Runner drives a Controller from informer events. A handler only maps an
// event to the groups it concerns and puts them on the queue: it reads no
// state and calls no API.
type Runner struct {
	controller *Controller
	queue      workqueue.TypedRateLimitingInterface[string]
}

// NewRunner returns a Runner for controller.
func NewRunner(controller *Controller) *Runner {
	return &Runner{
		controller: controller,
		queue: workqueue.NewTypedRateLimitingQueueWithConfig(
			workqueue.DefaultTypedControllerRateLimiter[string](),
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ServingGroups"},
		),
	}
}

func object(obj any) metav1.Object {
	if tombstone, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		obj = tombstone.Obj
	}
	accessor, _ := obj.(metav1.Object)
	return accessor
}

// enqueueOwner puts on the queue the group that controls an object, or, for
// an object no ServingGroup controls, the group its labels name: such a Pod
// may be one the group has to adopt.
func (r *Runner) enqueueOwner(obj any) {
	accessor := object(obj)
	if accessor == nil {
		return
	}
	if owner := metav1.GetControllerOfNoCopy(accessor); owner != nil && owner.Kind == workloadv1alpha1.ServingGroupKind.Kind {
		r.queue.Add(accessor.GetNamespace() + "/" + owner.Name)
		return
	}
	if group := accessor.GetLabels()[workloadv1alpha1.GroupNameLabelKey]; group != "" {
		r.queue.Add(accessor.GetNamespace() + "/" + group)
	}
}

// owned are the handlers of an object that a group controls. An update whose
// owner changed concerns the old owner and the new one.
func (r *Runner) owned() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    r.enqueueOwner,
		UpdateFunc: func(old, current any) { r.enqueueOwner(old); r.enqueueOwner(current) },
		DeleteFunc: r.enqueueOwner,
	}
}

func (r *Runner) enqueueGroup(obj any) {
	if accessor := object(obj); accessor != nil {
		r.queue.Add(accessor.GetNamespace() + "/" + accessor.GetName())
	}
}

// enqueueGroupsOf puts on the queue every group that names a ModelServing.
func (r *Runner) enqueueGroupsOf(namespace, modelServing string) {
	for _, group := range r.controller.caches.GroupsLabelled(namespace, modelServing) {
		r.queue.Add(group.Namespace + "/" + group.Name)
	}
}

// forgetGroup drops what the process remembers about a group that is gone.
// No round will see the object again to forget it by: a group deleted in the
// background is gone at once.
func (r *Runner) forgetGroup(obj any) {
	if accessor := object(obj); accessor != nil {
		r.controller.forget(accessor.GetUID(), nil)
	}
}

// Register adds the event handlers.
func (r *Runner) Register(informers Informers) error {
	handlers := []struct {
		informer cache.SharedIndexInformer
		handler  cache.ResourceEventHandler
	}{
		{informers.ServingGroups, cache.ResourceEventHandlerFuncs{
			AddFunc:    r.enqueueGroup,
			UpdateFunc: func(_, current any) { r.enqueueGroup(current) },
			DeleteFunc: func(obj any) {
				r.forgetGroup(obj)
				r.enqueueGroup(obj)
			},
		}},
		{informers.Pods, r.owned()},
		{informers.Services, r.owned()},
		// A revision that could not be resolved may be resolved now.
		{informers.ControllerRevisions, cache.ResourceEventHandlerFuncs{
			AddFunc: func(obj any) {
				if accessor := object(obj); accessor != nil {
					r.enqueueGroupsOf(accessor.GetNamespace(), accessor.GetLabels()[workloadv1alpha1.ModelServingNameLabelKey])
				}
			},
		}},
		// The recovery settings of the ModelServing take effect in its groups
		// before they reach the spec of a group.
		{informers.ModelServings, cache.ResourceEventHandlerFuncs{
			UpdateFunc: func(old, current any) {
				before, ok := old.(*workloadv1alpha1.ModelServing)
				after, ok2 := current.(*workloadv1alpha1.ModelServing)
				if !ok || !ok2 {
					return
				}
				if before.Spec.RecoveryPolicy != after.Spec.RecoveryPolicy ||
					!reflect.DeepEqual(before.Spec.Template.RestartGracePeriodSeconds, after.Spec.Template.RestartGracePeriodSeconds) ||
					before.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] != after.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] {
					r.enqueueGroupsOf(after.Namespace, after.Name)
				}
			},
		}},
	}
	for _, h := range handlers {
		if _, err := h.informer.AddEventHandler(h.handler); err != nil {
			return err
		}
	}
	return nil
}

// RegisterPodGroups adds the event handlers of the PodGroup informer, which
// exists only while the PodGroup CustomResourceDefinition is installed.
func (r *Runner) RegisterPodGroups(informer cache.SharedIndexInformer) {
	if _, err := informer.AddEventHandler(r.owned()); err != nil {
		klog.ErrorS(err, "Cannot watch PodGroups for the ServingGroup controller")
	}
}

// Run starts workers and blocks until ctx is done. The informers must have synced.
func (r *Runner) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer r.queue.ShutDown()
	klog.InfoS("Starting the ServingGroup controller", "workers", workers)
	for range workers {
		go wait.UntilWithContext(ctx, r.work, time.Second)
	}
	<-ctx.Done()
	klog.InfoS("Stopping the ServingGroup controller")
}

func (r *Runner) work(ctx context.Context) {
	for r.next(ctx) {
	}
}

func (r *Runner) next(ctx context.Context) bool {
	key, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(key)

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		r.queue.Forget(key)
		return true
	}
	after, err := r.controller.Reconcile(ctx, namespace, name)
	if apierrors.IsConflict(err) {
		// The object changed since it was read: the precondition of a write did
		// its work. The round is decided again on the object as it is now.
		klog.V(2).InfoS("An object changed since it was read; the round is decided again", "servingGroup", key, "err", err)
		r.queue.AddRateLimited(key)
		return true
	}
	if err != nil {
		klog.ErrorS(err, "Round of a ServingGroup failed", "servingGroup", key)
		r.queue.AddRateLimited(key)
		return true
	}
	r.queue.Forget(key)
	if after > 0 {
		r.queue.AddAfter(key, after)
	}
	return true
}
