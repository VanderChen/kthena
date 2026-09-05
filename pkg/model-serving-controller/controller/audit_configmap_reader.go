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

package controller

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	corelisters "k8s.io/client-go/listers/core/v1"
)

// Plugin templates may live outside the workload namespace and need not carry
// child labels. Resolve explicitly requested ConfigMaps, never globally scan.
type auditConfigMapLister struct {
	ctx    context.Context
	client kubernetes.Interface
}
type auditConfigMapNamespaceLister struct {
	*auditConfigMapLister
	namespace string
}

func (l *auditConfigMapLister) List(labels.Selector) ([]*corev1.ConfigMap, error) {
	return nil, fmt.Errorf("a namespace is required for plugin ConfigMap listing")
}
func (l *auditConfigMapLister) ConfigMaps(namespace string) corelisters.ConfigMapNamespaceLister {
	return &auditConfigMapNamespaceLister{l, namespace}
}
func (l *auditConfigMapNamespaceLister) Get(name string) (*corev1.ConfigMap, error) {
	return l.client.CoreV1().ConfigMaps(l.namespace).Get(l.ctx, name, metav1.GetOptions{})
}
func (l *auditConfigMapNamespaceLister) List(selector labels.Selector) ([]*corev1.ConfigMap, error) {
	if l.namespace == "" {
		return nil, fmt.Errorf("a namespace is required for plugin ConfigMap listing")
	}
	items, err := listAuditPages(l.ctx, metav1.ListOptions{LabelSelector: selector.String()}, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.ConfigMap, string, error) {
		list, err := l.client.CoreV1().ConfigMaps(l.namespace).List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return list.Items, list.Continue, nil
	})
	if err != nil {
		return nil, err
	}
	result := make([]*corev1.ConfigMap, 0, len(items))
	for i := range items {
		result = append(result, &items[i])
	}
	return result, nil
}
