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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// The debug endpoint shows the ServingGroups of every ModelServing as the
// caches of the controllers hold them: what a round would read. It is what an
// operator looks at when a rollout does not move, so the fields and their
// names are part of what the controller manager serves.
func TestTheDebugEndpointShowsTheGroupsAsTheCachesHoldThem(t *testing.T) {
	const namespace = "default"
	controlledBy := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{
			APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: kind, Name: name, UID: types.UID(name + "-uid"), Controller: ptr.To(true),
		}}
	}
	group := func(ms, name string) *workloadv1alpha1.ServingGroup {
		return &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(name + "-uid"),
			Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms},
			OwnerReferences: controlledBy(workloadv1alpha1.ModelServingKind.Kind, ms),
		}}
	}
	pod := func(group, name string) *corev1.Pod {
		return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, UID: types.UID(name + "-uid"),
			Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: "llama", workloadv1alpha1.GroupNameLabelKey: group},
			OwnerReferences: controlledBy(workloadv1alpha1.ServingGroupKind.Kind, group),
		}}
	}

	// A group that has reported on the Pods it has.
	reported := group("llama", "llama-0")
	reported.Generation = 3
	reported.Spec.Revision, reported.Spec.TargetRevision = "rev-a", "rev-b"
	members := []*corev1.Pod{pod("llama-0", "llama-0-prefill-0-1"), pod("llama-0", "llama-0-prefill-0-0")}
	reported.Status = workloadv1alpha1.ServingGroupStatus{
		ObservedGeneration: 2,
		ObservedPods:       servinggroup.Digest(members),
		Conditions: []metav1.Condition{
			{Type: string(workloadv1alpha1.ServingGroupConditionReady), Status: metav1.ConditionTrue},
			{Type: string(workloadv1alpha1.ServingGroupConditionFailed), Status: metav1.ConditionFalse},
		},
		RoleInstances: []workloadv1alpha1.RoleInstanceStatus{{Role: "prefill", Index: 0, Phase: workloadv1alpha1.RoleInstanceEstablished, Generation: 1}},
	}
	// A group that is being deleted and has not reported on the Pod it has.
	terminating := group("llama", "llama-1")
	terminating.DeletionTimestamp, terminating.Finalizers = ptr.To(metav1.Now()), []string{metav1.FinalizerDeleteDependents}
	terminating.Status.ObservedPods = "of-other-pods"
	// A group of another ModelServing, and an object without a controller.
	other := group("qwen", "qwen-0")
	stray := group("llama", "llama-9")
	stray.OwnerReferences = nil

	cluster := apitest.New(reported, terminating, other, stray, members[0], members[1], pod("llama-1", "llama-1-prefill-0-0"))
	snapshot := cluster.Snapshot()
	caches := objcache.Caches{
		ModelServings: snapshot.ModelServings, ServingGroups: snapshot.ServingGroups,
		Pods: snapshot.Pods, ControllerRevisions: snapshot.ControllerRevisions,
	}
	if err := caches.AddIndexers(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&Manager{caches: caches}).RegisterDebugEndpoints(mux)

	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/debug/modelserving/cache", nil))
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("response: %d, content type %q", response.Code, response.Header().Get("Content-Type"))
	}
	const want = `{
		"default/llama": [
			{
				"name": "llama-0", "uid": "llama-0-uid", "revision": "rev-a", "targetRevision": "rev-b",
				"generation": 3, "observedGeneration": 2, "fresh": true,
				"conditions": {"Failed": "False", "Ready": "True"},
				"roleInstances": [{"role": "prefill", "index": 0, "phase": "Established", "generation": 1}],
				"pods": ["llama-0-prefill-0-0", "llama-0-prefill-0-1"]
			},
			{
				"name": "llama-1", "uid": "llama-1-uid", "generation": 1, "observedGeneration": 0, "fresh": false, "terminating": true,
				"pods": ["llama-1-prefill-0-0"]
			}
		],
		"default/qwen": [
			{"name": "qwen-0", "uid": "qwen-0-uid", "generation": 1, "observedGeneration": 0, "fresh": false}
		]
	}`
	var got, wanted any
	if err := json.Unmarshal(response.Body.Bytes(), &got); err != nil {
		t.Fatalf("the response is no JSON: %v\n%s", err, response.Body.String())
	}
	if err := json.Unmarshal([]byte(want), &wanted); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, wanted) {
		t.Errorf("the dump:\n%s\nwant:\n%s", response.Body.String(), want)
	}
}
