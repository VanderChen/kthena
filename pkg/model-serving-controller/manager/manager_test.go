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
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/cache"
)

type watcherFake struct{ registered []cache.SharedIndexInformer }

func (f *watcherFake) RegisterPodGroups(informer cache.SharedIndexInformer) {
	f.registered = append(f.registered, informer)
}

func newInformer() cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(&cache.ListWatch{}, &corev1.Pod{}, 0, cache.Indexers{})
}

// With the PodGroup CustomResourceDefinition installed, the informer starts
// while the PodGroup manager is built, before the ServingGroup controller
// exists. Its events have to reach the controller all the same.
func TestPodGroupWatch(t *testing.T) {
	early, late := newInformer(), newInformer()

	watch := &podGroupWatch{}
	watch.started(nil)
	watch.started(early)
	watch.started(early)

	watcher := &watcherFake{}
	watch.bind(watcher)
	if want := []cache.SharedIndexInformer{early}; !reflect.DeepEqual(watcher.registered, want) {
		t.Fatalf("after the controller exists: %d informers are watched, want the one that started before it", len(watcher.registered))
	}

	// What the manager does after it built the controller, and an informer
	// that starts later because the definition was installed later.
	watch.started(early)
	watch.started(late)
	watch.started(late)
	if want := []cache.SharedIndexInformer{early, late}; !reflect.DeepEqual(watcher.registered, want) {
		t.Errorf("%d registrations, want each informer once", len(watcher.registered))
	}
}
