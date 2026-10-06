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
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	clientfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadLister "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type blockingPolicyLister struct {
	workloadLister.AutoscalingPolicyLister
	calls   atomic.Int32
	entered chan struct{}
	release chan struct{}
}

// A synced informer keeps the lifetime test independent of reflector backoff
// channels created outside synctest's virtual clock.
type syncedLifecycleInformer struct{}

func (syncedLifecycleInformer) Run(stop <-chan struct{})           { <-stop }
func (syncedLifecycleInformer) RunWithContext(ctx context.Context) { <-ctx.Done() }
func (syncedLifecycleInformer) HasSynced() bool                    { return true }
func (syncedLifecycleInformer) LastSyncResourceVersion() string    { return "" }

func (l *blockingPolicyLister) List(labels.Selector) ([]*workload.AutoscalingPolicy, error) {
	if l.calls.Add(1) == 1 {
		close(l.entered)
	}
	<-l.release
	return nil, nil
}

func TestAutoscalerRunOwnsReconcileLifetime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ac := NewAutoscaleController(kubefake.NewSimpleClientset(), clientfake.NewSimpleClientset(), 1)
		ac.autoscalingPoliciesInformer = syncedLifecycleInformer{}
		ac.modelServingInformer = syncedLifecycleInformer{}
		ac.podsInformer = syncedLifecycleInformer{}
		lister := &blockingPolicyLister{entered: make(chan struct{}), release: make(chan struct{})}
		ac.autoscalingPoliciesLister = lister
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan struct{})
		go func() { defer close(done); ac.Run(ctx) }()
		<-lister.entered
		cancel()
		synctest.Wait()
		select {
		case <-done:
			t.Error("Run returned while its reconcile loop was still active")
		default:
		}
		close(lister.release)
		<-done
		time.Sleep(3 * time.Second) // Virtual time: check multiple subsequent reconcile periods.
		require.EqualValues(t, 1, lister.calls.Load(), "no reconciles after Run exits")
	})
}

func TestAutoscalerCanceledCacheSyncDoesNotReconcile(t *testing.T) {
	for _, beforeStart := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := clientfake.NewSimpleClientset()
			ac := NewAutoscaleController(kubefake.NewSimpleClientset(), client, 1)
			lister := &blockingPolicyLister{entered: make(chan struct{}), release: make(chan struct{})}
			close(lister.release)
			ac.autoscalingPoliciesLister = lister
			if beforeStart {
				cancel()
			} else {
				client.PrependReactor("list", "autoscalingpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
					cancel()
					return true, nil, context.Canceled
				})
			}
			ac.Run(ctx)
			synctest.Wait()
			time.Sleep(2 * time.Second)
			require.Zero(t, lister.calls.Load())
		})
	}
}
