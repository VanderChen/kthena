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

// Package mscontroller is the ModelServing controller. It reconciles one
// ModelServing: it records the revision of its template, creates and deletes
// its ServingGroup objects, rolls them from one revision to the next within
// the rollout budget, hands the settings of the ModelServing down to them and
// sums up their status.
//
// It never touches a Pod. What happens inside a group is the ServingGroup
// controller; what this controller knows about a group is the object, its
// status, and whether that status was computed from the Pods the cache shows.
package mscontroller

import (
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/record"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// Controller reconciles ModelServings.
type Controller struct {
	kube     kubernetes.Interface
	kthena   clientset.Interface
	caches   objcache.Caches
	services corelisters.ServiceLister
	registry *plugins.Registry
	recorder record.EventRecorder
	now      func() time.Time
	// volcano is nil in a cluster without Volcano.
	volcano volcano.Interface
	// rollingBack reports whether the rollback switch is set.
	rollingBack func() bool
	// rounds is held for reading by every round of the ServingGroup
	// controller (see sgcontroller.Options.Rounds).
	rounds *sync.RWMutex

	mu sync.Mutex
	// targets caches the target revision resolved for a ModelServing, by UID.
	targets map[types.UID]recorded
	// unsettled holds, by the UID of a group, when this process first saw that
	// the group had not reported; unsettledOf the groups of a ModelServing in it.
	unsettled   map[types.UID]time.Time
	unsettledOf map[types.UID]map[types.UID]bool
}

// Options are the dependencies of a Controller.
type Options struct {
	Kube     kubernetes.Interface
	Kthena   clientset.Interface
	Caches   objcache.Caches
	Services corelisters.ServiceLister
	// Registry holds the plugins; nil means plugins.DefaultRegistry.
	Registry *plugins.Registry
	Recorder record.EventRecorder
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Volcano is the client for PodGroups; nil where Volcano is not installed.
	Volcano volcano.Interface
	// RollingBack reports whether the rollback switch is set (see
	// RollbackSwitch); nil means never.
	RollingBack func() bool
	// Rounds is the lock every round of the ServingGroup controller holds for
	// reading; nil means that no such controller runs in this process.
	Rounds *sync.RWMutex
}

// New returns a Controller.
func New(options Options) *Controller {
	c := &Controller{
		kube:     options.Kube,
		kthena:   options.Kthena,
		caches:   options.Caches,
		services: options.Services,
		registry: options.Registry,
		recorder: options.Recorder,
		now:      options.Now,
		volcano:  options.Volcano,
		targets:  map[types.UID]recorded{},

		rollingBack: options.RollingBack,
		rounds:      options.Rounds,

		unsettled:   map[types.UID]time.Time{},
		unsettledOf: map[types.UID]map[types.UID]bool{},
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
	if c.rollingBack == nil {
		c.rollingBack = func() bool { return false }
	}
	if c.rounds == nil {
		c.rounds = &sync.RWMutex{}
	}
	return c
}

// SetCaches replaces the stores the controller reads.
func (c *Controller) SetCaches(caches objcache.Caches, services corelisters.ServiceLister) {
	c.caches, c.services = caches, services
}
