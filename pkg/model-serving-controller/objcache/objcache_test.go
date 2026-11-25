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

package objcache

import (
	"sort"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func controller(kind string, uid types.UID) []metav1.OwnerReference {
	return []metav1.OwnerReference{{Kind: kind, Name: "owner", UID: uid, Controller: ptr.To(true)}}
}

func pod(namespace, name, group string, owners []metav1.OwnerReference) *corev1.Pod {
	labels := map[string]string{}
	if group != "" {
		labels[workloadv1alpha1.GroupNameLabelKey] = group
	}
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Labels: labels, OwnerReferences: owners}}
}

func names[T metav1.Object](objects []T) string {
	var out []string
	for _, object := range objects {
		out = append(out, object.GetName())
	}
	sort.Strings(out)
	return strings.Join(out, ",")
}

func TestCaches(t *testing.T) {
	newStore := func() cache.Indexer { return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{}) }
	caches := Caches{ModelServings: newStore(), ServingGroups: newStore(), Pods: newStore(), ControllerRevisions: newStore()}

	// Indexes can be added to a store that is already filled.
	group := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "llama-0", UID: "sg-uid",
		Labels:          map[string]string{workloadv1alpha1.ModelServingNameLabelKey: "llama"},
		OwnerReferences: controller("ModelServing", "ms-uid"),
	}}
	foreign := group.DeepCopy()
	foreign.Name, foreign.UID, foreign.OwnerReferences = "llama-1", "other-sg-uid", controller("ModelServing", "previous-ms-uid")
	for _, object := range []any{group, foreign} {
		_ = caches.ServingGroups.Add(object)
	}
	notController := []metav1.OwnerReference{{Kind: "ServingGroup", Name: "llama-0", UID: "sg-uid"}}
	for _, object := range []*corev1.Pod{
		pod("default", "owned", "llama-0", controller("ServingGroup", "sg-uid")),
		pod("default", "owned-relabelled", "", controller("ServingGroup", "sg-uid")),
		pod("default", "legacy", "llama-0", controller("ModelServing", "ms-uid")),
		pod("default", "orphan", "llama-0", nil),
		pod("default", "referenced", "llama-0", notController),
		pod("default", "of-earlier-group", "llama-0", controller("ServingGroup", "old-sg-uid")),
		pod("default", "other-group", "llama-1", nil),
		pod("other", "other-namespace", "llama-0", nil),
	} {
		_ = caches.Pods.Add(object)
	}
	_ = caches.ModelServings.Add(&workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "llama"}})
	_ = caches.ControllerRevisions.Add(&appsv1.ControllerRevision{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: "llama-abc", Labels: map[string]string{workloadv1alpha1.ModelServingNameLabelKey: "llama"},
	}})
	if err := caches.AddIndexers(); err != nil {
		t.Fatal(err)
	}

	if got := names(caches.PodsControlledBy("sg-uid")); got != "owned,owned-relabelled" {
		t.Errorf("PodsControlledBy = %s", got)
	}
	// A Pod another ServingGroup controls is never one to adopt.
	if got := names(caches.UnadoptedPods("default", "llama-0")); got != "legacy,orphan,referenced" {
		t.Errorf("UnadoptedPods = %s", got)
	}
	if got := names(caches.PodsOfGroup(group)); got != "legacy,orphan,owned,owned-relabelled,referenced" {
		t.Errorf("PodsOfGroup = %s", got)
	}
	if got := names(caches.GroupsControlledBy("ms-uid")); got != "llama-0" {
		t.Errorf("GroupsControlledBy = %s", got)
	}
	if got := names(caches.GroupsLabelled("default", "llama")); got != "llama-0,llama-1" {
		t.Errorf("GroupsLabelled = %s", got)
	}
	if got := names(caches.RevisionsLabelled("default", "llama")); got != "llama-abc" {
		t.Errorf("RevisionsLabelled = %s", got)
	}

	if ms, err := caches.ModelServing("default", "llama"); err != nil || ms.Name != "llama" {
		t.Errorf("ModelServing = %v, %v", ms, err)
	}
	if sg, err := caches.ServingGroup("default", "llama-0"); err != nil || sg.UID != "sg-uid" {
		t.Errorf("ServingGroup = %v, %v", sg, err)
	}
	if cr, err := caches.ControllerRevision("default", "llama-abc"); err != nil || cr.Name != "llama-abc" {
		t.Errorf("ControllerRevision = %v, %v", cr, err)
	}
	if _, err := caches.ControllerRevision("default", "missing"); !apierrors.IsNotFound(err) {
		t.Errorf("ControllerRevision of a missing name = %v, want NotFound", err)
	}
	if _, err := caches.ServingGroup("other", "llama-0"); !apierrors.IsNotFound(err) {
		t.Errorf("ServingGroup of another namespace = %v, want NotFound", err)
	}
}
