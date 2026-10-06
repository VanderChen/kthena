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
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/stretchr/testify/require"
)

func TestHealthMonitorUnexpectedExit(t *testing.T) {
	for _, name := range []string{ModelServingController, AutoscalerController, ModelBoosterController, "lws", "leader-election"} {
		for _, runErr := range []error{nil, errors.New("cache sync failed")} {
			t.Run(name+"/"+fmtError(runErr), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(context.Background())
					defer cancel()
					h := NewHealthMonitor(ctx)
					release := make(chan struct{})
					h.start(ctx, name, func(context.Context) error { <-release; return runErr })
					synctest.Wait()
					require.NoError(t, h.Check())
					close(release)
					err := h.wait()
					require.ErrorContains(t, err, name)
					if runErr != nil {
						require.ErrorIs(t, err, runErr)
					}
					cancel()
					synctest.Wait()
					require.Equal(t, err, h.Check(), "shutdown must not clear a failure")
				})
			})
		}
	}
}

func fmtError(err error) string {
	if err == nil {
		return "nil"
	}
	return "error"
}

func TestHealthMonitorCanceledControllerStillBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		root, cancel := context.WithCancel(context.Background())
		defer cancel()
		child, stop := context.WithCancel(root)
		h := NewHealthMonitor(root)
		release := make(chan struct{})
		h.start(child, ModelServingController, func(context.Context) error { <-release; return nil })
		stop()
		err := h.wait()
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorContains(t, err, ModelServingController)
		require.NoError(t, root.Err())
		close(release)
		synctest.Wait()
		require.Equal(t, err, h.Check())
	})
}

func TestHealthMonitorGracefulShutdown(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "standby", true: "active"}[active], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				h := NewHealthMonitor(ctx)
				if active {
					h.start(ctx, ModelServingController, func(ctx context.Context) error {
						<-ctx.Done()
						return ctx.Err()
					})
				}
				synctest.Wait()
				require.NoError(t, h.Check(), "standby and unregistered/disabled controllers are healthy")
				cancel()
				require.NoError(t, h.wait())
				synctest.Wait()
				require.NoError(t, h.Check())
			})
		})
	}
}

func TestHealthMonitorConcurrentFailuresAndChecks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := NewHealthMonitor(ctx)
	release := make(chan struct{})
	var checks sync.WaitGroup
	for i := 0; i < 32; i++ {
		h.start(ctx, AutoscalerController, func(context.Context) error { <-release; return nil })
		checks.Add(1)
		go func() {
			defer checks.Done()
			for j := 0; j < 100; j++ {
				_ = h.Check()
			}
		}()
	}
	close(release)
	require.ErrorContains(t, h.wait(), AutoscalerController)
	checks.Wait()
}
