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
	"reflect"
	"sort"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The event of a ServingGroup starts a round of every ModelServing it can
// concern: the one it names, and the one whose ordinal its name occupies.
func TestServingGroupEventsReachTheModelServing(t *testing.T) {
	group := func(name string, labels map[string]string) *workloadv1alpha1.ServingGroup {
		return &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: name, Labels: labels}}
	}
	named := func(ms string) map[string]string {
		return map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms}
	}
	tests := []struct {
		name  string
		event any
		want  []string
	}{
		{name: "a group of the ModelServing", event: group("llama-2", named("llama")), want: []string{"default/llama"}},
		{name: "an object that holds the name of a group", event: group("llama-2", nil), want: []string{"default/llama"}},
		{name: "a group under a name that is also a group name of another", event: group("llama-2-0", named("llama-2")),
			want: []string{"default/llama-2"}},
		{name: "labelled for one, named after another", event: group("llama-2", named("qwen")), want: []string{"default/llama", "default/qwen"}},
		{name: "neither", event: group("llama", nil)},
		{name: "the last state of a deleted object",
			event: cache.DeletedFinalStateUnknown{Key: "default/llama-0", Obj: group("llama-0", nil)}, want: []string{"default/llama"}},
	}
	for _, tt := range tests {
		r := NewRunner(New(Options{}))
		r.enqueueGroup(tt.event)
		var got []string
		for r.queue.Len() > 0 {
			key, _ := r.queue.Get()
			got = append(got, key)
			r.queue.Done(key)
		}
		sort.Strings(got)
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("%s: queued %v, want %v", tt.name, got, tt.want)
		}
		r.queue.ShutDown()
	}
}
