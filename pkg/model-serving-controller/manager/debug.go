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

package manager

import (
	"encoding/json"
	"net/http"
	"sort"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// groupDump is what the debug endpoint shows of one ServingGroup: what the
// controllers read of it, as their caches have it.
type groupDump struct {
	Name               string `json:"name"`
	UID                string `json:"uid"`
	Revision           string `json:"revision,omitempty"`
	TargetRevision     string `json:"targetRevision,omitempty"`
	Generation         int64  `json:"generation"`
	ObservedGeneration int64  `json:"observedGeneration"`
	// Fresh reports whether the status was computed from the Pods the cache holds.
	Fresh         bool                                  `json:"fresh"`
	Terminating   bool                                  `json:"terminating,omitempty"`
	Conditions    map[string]string                     `json:"conditions,omitempty"`
	RoleInstances []workloadv1alpha1.RoleInstanceStatus `json:"roleInstances,omitempty"`
	Pods          []string                              `json:"pods,omitempty"`
}

// RegisterDebugEndpoints registers /debug/modelserving/cache, which dumps the
// ServingGroups of every ModelServing as the caches of the controllers hold
// them. The controllers keep no state of their own to show.
func (m *Manager) RegisterDebugEndpoints(mux *http.ServeMux) {
	mux.HandleFunc("/debug/modelserving/cache", func(w http.ResponseWriter, _ *http.Request) {
		dump := map[string][]groupDump{}
		for _, obj := range m.caches.ServingGroups.List() {
			group, ok := obj.(*workloadv1alpha1.ServingGroup)
			if !ok {
				continue
			}
			owner := metav1.GetControllerOfNoCopy(group)
			if owner == nil {
				continue
			}
			item := groupDump{
				Name: group.Name, UID: string(group.UID),
				Revision: group.Spec.Revision, TargetRevision: group.Spec.TargetRevision,
				Generation: group.Generation, ObservedGeneration: group.Status.ObservedGeneration,
				Terminating:   group.DeletionTimestamp != nil,
				Conditions:    map[string]string{},
				RoleInstances: group.Status.RoleInstances,
			}
			for _, condition := range group.Status.Conditions {
				item.Conditions[condition.Type] = string(condition.Status)
			}
			members := servinggroup.Members(m.caches.PodsOfGroup(group), group.Name, group.UID, servinggroup.Parent{Name: owner.Name, UID: owner.UID, AdoptionDone: true})
			item.Fresh = group.Status.ObservedPods == servinggroup.Digest(members)
			for _, pod := range members {
				item.Pods = append(item.Pods, pod.Name)
			}
			key := group.Namespace + "/" + owner.Name
			dump[key] = append(dump[key], item)
		}
		for _, groups := range dump {
			sort.Slice(groups, func(a, b int) bool { return groups[a].Name < groups[b].Name })
		}
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(dump); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	})
}
