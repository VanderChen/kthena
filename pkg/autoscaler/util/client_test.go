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

package util

import (
	"context"
	"testing"
	"time"

	clientsetfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadlisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

type fakePodLister struct {
	pods []*corev1.Pod
}

func (f *fakePodLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.pods, nil
}

func (f *fakePodLister) Pods(namespace string) listerv1.PodNamespaceLister {
	return &fakePodNamespaceLister{pods: f.pods}
}

type fakePodNamespaceLister struct {
	pods []*corev1.Pod
}

func (f *fakePodNamespaceLister) List(selector labels.Selector) ([]*corev1.Pod, error) {
	return f.pods, nil
}

func (f *fakePodNamespaceLister) Get(name string) (*corev1.Pod, error) {
	for _, p := range f.pods {
		if p.Name == name {
			return p, nil
		}
	}
	return nil, nil
}

func TestGetRoleName(t *testing.T) {
	ref := &corev1.ObjectReference{Name: "role/sub"}
	role, sub, err := GetRoleName(ref)
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if role != "role" || sub != "sub" {
		t.Fatalf("wrong role parsing")
	}
}

func TestGetRoleNameInvalid(t *testing.T) {
	ref := &corev1.ObjectReference{Name: "invalid"}
	_, _, err := GetRoleName(ref)
	if err == nil {
		t.Fatalf("expected error")
	}
}

func TestGetTargetLabels(t *testing.T) {
	target := &workload.Target{}
	target.TargetRef.Name = "model1"
	target.TargetRef.Kind = workload.ModelServingKind.Kind

	selector, err := GetTargetLabels(target)
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if selector == nil {
		t.Fatalf("selector is nil")
	}
}

func TestGetMetricPods(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod1",
			Namespace: "default",
			Labels: map[string]string{
				workload.ModelServingNameLabelKey: "model1",
				workload.EntryLabelKey:            Entry,
			},
		},
	}

	lister := &fakePodLister{
		pods: []*corev1.Pod{pod},
	}

	target := &workload.Target{}
	target.TargetRef.Name = "model1"
	target.TargetRef.Kind = workload.ModelServingKind.Kind

	pods, err := GetMetricPods(lister, "default", target)
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if len(pods) != 1 {
		t.Fatalf("expected 1 pod")
	}
}

func TestUpdateModelServing(t *testing.T) {
	client := clientsetfake.NewSimpleClientset()

	model := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model1",
			Namespace: "default",
		},
	}

	_, err := client.WorkloadV1alpha1().
		ModelServings("default").
		Create(context.Background(), model, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create failed")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err = UpdateModelServing(ctx, client, model)
	if err != nil {
		t.Fatalf("update failed")
	}
}

func TestGetModelServingTarget(t *testing.T) {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{})
	model := &workload.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "model1",
			Namespace: "default",
		},
	}
	indexer.Add(model)

	lister := workloadlisters.NewModelServingLister(indexer)

	result, err := GetModelServingTarget(lister, "default", "model1")
	if err != nil {
		t.Fatalf("unexpected error")
	}
	if result.Name != "model1" {
		t.Fatalf("wrong model serving")
	}
}
