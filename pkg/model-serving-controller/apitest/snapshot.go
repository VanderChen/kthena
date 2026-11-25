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
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	appslisters "k8s.io/client-go/listers/apps/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	schedulinglisters "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"

	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
)

// Snapshot is the content of the cluster at one point in time, in the shape an
// informer cache has. A Snapshot is independent of the Cluster: later writes
// never change it, so a test can hand a controller a deliberately stale cache
// by keeping an older Snapshot.
type Snapshot struct {
	Pods                cache.Indexer
	Services            cache.Indexer
	ConfigMaps          cache.Indexer
	ControllerRevisions cache.Indexer
	ModelServings       cache.Indexer
	ServingGroups       cache.Indexer
	PodGroups           cache.Indexer
}

// Listers are typed listers backed by a Snapshot, ready to be injected into a
// controller.
type Listers struct {
	Pods                corelisters.PodLister
	Services            corelisters.ServiceLister
	ConfigMaps          corelisters.ConfigMapLister
	ControllerRevisions appslisters.ControllerRevisionLister
	ModelServings       workloadlisters.ModelServingLister
	ServingGroups       workloadlisters.ServingGroupLister
	PodGroups           schedulinglisters.PodGroupLister
}

// Snapshot returns the current content of the cluster as informer-style
// stores holding deep copies.
func (c *Cluster) Snapshot() *Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()

	indexers := map[string]cache.Indexer{}
	for _, res := range modelledResources {
		indexer := newIndexer()
		indexers[res.gvr.Resource] = indexer
		list, err := c.trackerFor(res.fam).List(res.gvr, res.gvk, metav1.NamespaceAll)
		if err != nil {
			continue
		}
		items, err := meta.ExtractList(list)
		if err != nil {
			continue
		}
		for _, item := range items {
			if err := indexer.Add(item.DeepCopyObject()); err != nil {
				panic(fmt.Errorf("apitest: cannot index %s: %w", res.gvr.Resource, err))
			}
		}
	}

	return &Snapshot{
		Pods:                indexers["pods"],
		Services:            indexers["services"],
		ConfigMaps:          indexers["configmaps"],
		ControllerRevisions: indexers["controllerrevisions"],
		ModelServings:       indexers["modelservings"],
		ServingGroups:       indexers["servinggroups"],
		PodGroups:           indexers["podgroups"],
	}
}

// Listers returns typed listers over the snapshot.
func (s *Snapshot) Listers() Listers {
	return Listers{
		Pods:                corelisters.NewPodLister(s.Pods),
		Services:            corelisters.NewServiceLister(s.Services),
		ConfigMaps:          corelisters.NewConfigMapLister(s.ConfigMaps),
		ControllerRevisions: appslisters.NewControllerRevisionLister(s.ControllerRevisions),
		ModelServings:       workloadlisters.NewModelServingLister(s.ModelServings),
		ServingGroups:       workloadlisters.NewServingGroupLister(s.ServingGroups),
		PodGroups:           schedulinglisters.NewPodGroupLister(s.PodGroups),
	}
}

func newIndexer() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc,
	})
}
