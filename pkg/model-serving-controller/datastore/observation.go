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

package datastore

import "k8s.io/apimachinery/pkg/types"

func (s *store) BindModelServing(key types.NamespacedName, uid types.UID) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	if previous, exists := s.owners[key]; exists && previous != uid {
		delete(s.servingGroup, key)
	}
	s.owners[key] = uid
}

func (s *store) ListModelServings() []types.NamespacedName {
	s.mutex.RLock()
	defer s.mutex.RUnlock()
	keys := make(map[types.NamespacedName]struct{}, len(s.servingGroup)+len(s.owners))
	for key := range s.servingGroup {
		keys[key] = struct{}{}
	}
	for key := range s.owners {
		keys[key] = struct{}{}
	}
	result := make([]types.NamespacedName, 0, len(keys))
	for key := range keys {
		result = append(result, key)
	}
	return result
}

func (s *store) CalibrateReadyPods(key types.NamespacedName, readyByGroup map[string][]string) {
	s.mutex.Lock()
	defer s.mutex.Unlock()
	for name, group := range s.servingGroup[key] {
		group.runningPods = make(map[string]struct{}, len(readyByGroup[name]))
		for _, pod := range readyByGroup[name] {
			group.runningPods[pod] = struct{}{}
		}
	}
}
