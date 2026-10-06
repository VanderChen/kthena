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
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
	clientfake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestModelBoosterCanceledCacheSyncDoesNotStartWorkers(t *testing.T) {
	for _, beforeStart := range []bool{true, false} {
		synctest.Test(t, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := clientfake.NewSimpleClientset()
			c := NewModelBoosterController(kubefake.NewSimpleClientset(), client)
			c.workQueue.Add("default/not-started")
			if beforeStart {
				cancel()
			} else {
				client.PrependReactor("list", "modelboosters", func(k8stesting.Action) (bool, runtime.Object, error) {
					cancel()
					return true, nil, context.Canceled
				})
			}
			c.Run(ctx, 1)
			synctest.Wait()
			require.Equal(t, 1, c.workQueue.Len(), "workers must not consume items after failed cache sync")
			require.True(t, c.workQueue.ShuttingDown())
		})
	}
}
