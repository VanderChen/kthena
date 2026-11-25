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
	"time"

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
	ModelServings       cache.SharedIndexInformer
	ServingGroups       cache.SharedIndexInformer
	Pods                cache.SharedIndexInformer
	ControllerRevisions cache.SharedIndexInformer
}

// Runner drives a Controller from informer events. A handler only maps an
// event to the ModelServing it concerns and puts it on the queue.
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
			workqueue.TypedRateLimitingQueueConfig[string]{Name: "ModelServings"},
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

func (r *Runner) enqueue(obj any) {
	if accessor := object(obj); accessor != nil {
		r.queue.Add(accessor.GetNamespace() + "/" + accessor.GetName())
	}
}

// enqueueNamed puts on the queue the ModelServing an object names in its
// labels: a group, a Pod and a ControllerRevision all carry the name.
func (r *Runner) enqueueNamed(obj any) {
	accessor := object(obj)
	if accessor == nil {
		return
	}
	if name := accessor.GetLabels()[workloadv1alpha1.ModelServingNameLabelKey]; name != "" {
		r.queue.Add(accessor.GetNamespace() + "/" + name)
	}
}

func (r *Runner) named() cache.ResourceEventHandlerFuncs {
	return cache.ResourceEventHandlerFuncs{
		AddFunc:    r.enqueueNamed,
		UpdateFunc: func(_, current any) { r.enqueueNamed(current) },
		DeleteFunc: r.enqueueNamed,
	}
}

// Register adds the event handlers.
func (r *Runner) Register(informers Informers) error {
	handlers := []struct {
		informer cache.SharedIndexInformer
		handler  cache.ResourceEventHandler
	}{
		{informers.ModelServings, cache.ResourceEventHandlerFuncs{
			AddFunc:    r.enqueue,
			UpdateFunc: func(_, current any) { r.enqueue(current) },
			DeleteFunc: r.enqueue,
		}},
		{informers.ServingGroups, r.named()},
		// Whether the status of a group is fresh is judged against the Pod
		// cache, so a change of a Pod can change the plan without any change
		// of a group.
		{informers.Pods, r.named()},
		{informers.ControllerRevisions, r.named()},
	}
	for _, h := range handlers {
		if _, err := h.informer.AddEventHandler(h.handler); err != nil {
			return err
		}
	}
	return nil
}

// Run starts workers and blocks until ctx is done. The informers must have synced.
func (r *Runner) Run(ctx context.Context, workers int) {
	defer utilruntime.HandleCrash()
	defer r.queue.ShutDown()
	klog.InfoS("Starting the ModelServing controller", "workers", workers)
	for range workers {
		go wait.UntilWithContext(ctx, r.work, time.Second)
	}
	<-ctx.Done()
	klog.InfoS("Stopping the ModelServing controller")
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
	switch {
	case errors.Is(err, ErrNotSettled):
		// Not a failure: a group has yet to report. Its report brings the next
		// round; the growing delay is for the case that no event does.
		r.queue.AddRateLimited(key)
	case err != nil:
		klog.ErrorS(err, "Round of a ModelServing failed", "modelServing", key)
		r.queue.AddRateLimited(key)
	default:
		r.queue.Forget(key)
	}
	if after > 0 {
		r.queue.AddAfter(key, after)
	}
	return true
}
