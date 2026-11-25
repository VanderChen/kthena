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

// Package sgcontroller is the ServingGroup controller. It reconciles one
// ServingGroup object: it creates and deletes the member Pods of the group,
// keeps the ledger of its Role instances, recovers it from faults by the
// recovery policy and reports its state in the status.
//
// It decides from the spec of the group, the ControllerRevisions the spec
// names and the member Pods. It never creates or deletes a ServingGroup; that
// is the ModelServing controller.
package sgcontroller

import (
	"context"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// PodGroups is what the controller needs of the PodGroup manager.
type PodGroups interface {
	// Enabled reports whether Pods rendered from ms are scheduled through a PodGroup.
	Enabled(ms *workloadv1alpha1.ModelServing) bool
	// SyncPodGroup creates or updates the PodGroup of a group.
	SyncPodGroup(ctx context.Context, ms *workloadv1alpha1.ModelServing, owner metav1.OwnerReference, name string) (time.Duration, error)
	// AnnotatePodWithPodGroup binds a Pod to the PodGroup of its group.
	AnnotatePodWithPodGroup(pod *corev1.Pod, ms *workloadv1alpha1.ModelServing, groupName, taskName string)
	GenerateTaskName(roleName string, roleIndex int) string
}

// Controller reconciles ServingGroups.
type Controller struct {
	kube      kubernetes.Interface
	kthena    clientset.Interface
	caches    objcache.Caches
	services  corelisters.ServiceLister
	podGroups PodGroups
	registry  *plugins.Registry
	recorder  record.EventRecorder
	now       func() time.Time

	// faults remembers since when this process has seen the fault of a member.
	// It is the only input of a round that is not on an object: a new process
	// counts a full grace period again.
	faults *servinggroup.FaultTracker

	// readyNotified holds the UIDs of the Pods the OnPodReady hook was called
	// for. known holds, by the UID of a group, the UIDs of the member Pods of its
	// last round: what the process may remember something about.
	readyMu       sync.Mutex
	readyNotified sets.Set[types.UID]
	known         map[types.UID]sets.Set[types.UID]
}

// Options are the dependencies of a Controller.
type Options struct {
	Kube      kubernetes.Interface
	Kthena    clientset.Interface
	Caches    objcache.Caches
	Services  corelisters.ServiceLister
	PodGroups PodGroups
	// Registry holds the plugins; nil means plugins.DefaultRegistry.
	Registry *plugins.Registry
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
}

// New returns a Controller.
func New(options Options) *Controller {
	c := &Controller{
		kube:          options.Kube,
		kthena:        options.Kthena,
		caches:        options.Caches,
		services:      options.Services,
		podGroups:     options.PodGroups,
		registry:      options.Registry,
		recorder:      options.Recorder,
		now:           options.Now,
		faults:        servinggroup.NewFaultTracker(),
		readyNotified: sets.New[types.UID](),
		known:         map[types.UID]sets.Set[types.UID]{},
	}
	if c.registry == nil {
		c.registry = plugins.DefaultRegistry
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.recorder == nil {
		c.recorder = &record.FakeRecorder{}
	}
	return c
}

// SetCaches replaces the stores the controller reads.
func (c *Controller) SetCaches(caches objcache.Caches, services corelisters.ServiceLister) {
	c.caches, c.services = caches, services
}
