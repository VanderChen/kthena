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
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

// The request-scoped client makes legacy collection deletion safe without
// changing the shared client or trusting a label selector as ownership proof.
type auditKubeClient struct {
	kubernetes.Interface
	controller *ModelServingController
	ms         *workloadv1alpha1.ModelServing
}
type auditCoreClient struct {
	typedcore.CoreV1Interface
	owner *auditKubeClient
}
type auditPodClient struct {
	typedcore.PodInterface
	owner     *auditKubeClient
	namespace string
}

func (c *auditKubeClient) CoreV1() typedcore.CoreV1Interface {
	return &auditCoreClient{c.Interface.CoreV1(), c}
}
func (c *auditCoreClient) Pods(namespace string) typedcore.PodInterface {
	return &auditPodClient{c.CoreV1Interface.Pods(namespace), c.owner, namespace}
}

func (c *auditPodClient) DeleteCollection(ctx context.Context, options metav1.DeleteOptions, listOptions metav1.ListOptions) error {
	if c.namespace != c.owner.ms.Namespace {
		return fmt.Errorf("Pod deletion outside ModelServing namespace")
	}
	if err := c.owner.controller.validateCurrentModelServing(ctx, c.owner.ms); err != nil {
		return err
	}
	pods, err := listAuditPages(ctx, listOptions, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Pod, string, error) {
		list, err := c.PodInterface.List(ctx, opts)
		if err != nil {
			return nil, "", err
		}
		return list.Items, list.Continue, nil
	})
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if !utils.IsOwnedByModelServingWithUID(&pod, c.owner.ms.UID) {
			continue
		}
		deleteOptions := options.DeepCopy()
		deleteOptions.Preconditions = &metav1.Preconditions{UID: &pod.UID}
		if pod.ResourceVersion != "" {
			deleteOptions.Preconditions.ResourceVersion = &pod.ResourceVersion
		}
		if err := c.PodInterface.Delete(ctx, pod.Name, *deleteOptions); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
