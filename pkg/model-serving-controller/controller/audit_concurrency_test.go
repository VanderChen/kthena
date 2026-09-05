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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcore "k8s.io/client-go/kubernetes/typed/core/v1"
)

type auditBlockingClient struct {
	kubernetes.Interface
	started chan struct{}
	once    sync.Once
}
type auditBlockingCore struct {
	typedcore.CoreV1Interface
	client *auditBlockingClient
}
type auditBlockingPods struct {
	typedcore.PodInterface
	client    *auditBlockingClient
	namespace string
}

func (c *auditBlockingClient) CoreV1() typedcore.CoreV1Interface {
	return &auditBlockingCore{c.Interface.CoreV1(), c}
}
func (c *auditBlockingCore) Pods(ns string) typedcore.PodInterface {
	return &auditBlockingPods{c.CoreV1Interface.Pods(ns), c.client, ns}
}
func (c *auditBlockingPods) List(ctx context.Context, opts metav1.ListOptions) (*corev1.PodList, error) {
	if c.namespace != "default" {
		return c.PodInterface.List(ctx, opts)
	}
	c.client.once.Do(func() { close(c.client.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestAuditTimeoutDoesNotHoldGlobalStateLock(t *testing.T) {
	ms, _ := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	*ms.Spec.Replicas = 0
	c, kube := auditController(t, ms)
	require.NoError(t, c.ConfigureAudit(time.Minute, 500*time.Millisecond))
	other := ms.DeepCopy()
	other.Namespace, other.UID = "other", "other-uid"
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(other.Namespace).Create(context.Background(), other, metav1.CreateOptions{})
	require.NoError(t, err)
	blocked := &auditBlockingClient{Interface: kube, started: make(chan struct{})}
	c.kubeClientSet = blocked
	c.requestAudit(utils.GetNamespaceName(ms).String(), func(r *auditRequest) { r.live = true })
	slow := make(chan error, 1)
	go func() { slow <- c.reconcileModelServing(context.Background(), utils.GetNamespaceName(ms).String()) }()
	<-blocked.started
	// A second key must get through request bookkeeping and live reads while
	// the first API request is still blocked until its own context expires.
	c.requestAudit(utils.GetNamespaceName(other).String(), func(r *auditRequest) { r.live = true })
	require.NoError(t, c.reconcileModelServing(context.Background(), utils.GetNamespaceName(other).String()))
	require.ErrorContains(t, <-slow, "context deadline exceeded")
	require.True(t, c.audit.requests[utils.GetNamespaceName(ms).String()].live)
}

func TestAuditWorkqueueSerializesSameKeyButNotDifferentKeys(t *testing.T) {
	ms, _ := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, _ := auditController(t, ms)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, release, other, again := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	active, calls := 0, 0
	overlap := false
	c.syncHandler = func(_ context.Context, key string) error {
		if key == "other/ms" {
			close(other)
			return nil
		}
		mu.Lock()
		active++
		calls++
		count := calls
		overlap = overlap || active > 1
		mu.Unlock()
		if count == 1 {
			close(first)
			<-release
		} else {
			close(again)
		}
		mu.Lock()
		active--
		mu.Unlock()
		return nil
	}
	go c.worker(ctx)
	go c.worker(ctx)
	c.requestAudit("default/ms", func(r *auditRequest) { r.live = true })
	<-first
	c.requestAudit("default/ms", func(r *auditRequest) {})
	c.requestAudit("other/ms", func(r *auditRequest) { r.live = true })
	select {
	case <-other:
	case <-time.After(3 * time.Second):
		t.Fatal("unrelated key blocked")
	}
	close(release)
	select {
	case <-again:
	case <-time.After(3 * time.Second):
		t.Fatal("dirty key was not retried")
	}
	mu.Lock()
	require.False(t, overlap)
	mu.Unlock()
}
