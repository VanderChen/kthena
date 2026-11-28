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

package app

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func TestNewLeaseLock(t *testing.T) {
	tests := []struct {
		name     string
		lease    string
		expected string
	}{
		{
			name:     "empty name falls back to the autoscaler lease",
			lease:    "",
			expected: "lease.kthena.autoscaler",
		},
		{
			name:     "configured name is used as is",
			lease:    "lease.custom",
			expected: "lease.custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lock, err := newLeaseLock(fake.NewSimpleClientset(), "kthena-system", tt.lease)
			require.NoError(t, err)
			assert.Equal(t, tt.expected, lock.LeaseMeta.Name)
			assert.Equal(t, "kthena-system", lock.LeaseMeta.Namespace)
			assert.NotEmpty(t, lock.Identity())
		})
	}
}

func TestServeHealth(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		serveHealth(ctx, listener, &readiness{synced: func() bool { return true }})
		close(done)
	}()

	resp, err := http.Get("http://" + listener.Addr().String() + "/healthz")
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok", string(body))

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("health endpoint did not stop after the context was cancelled")
	}
}

func TestReadyz(t *testing.T) {
	tests := []struct {
		name           string
		leaderElection bool
		leading        bool
		sawLeader      bool
		synced         bool
		expected       int
	}{
		{name: "no leader election, informers not synced", expected: http.StatusServiceUnavailable},
		{name: "no leader election, informers synced", synced: true, expected: http.StatusOK},
		{name: "standby that has not read the Lease", leaderElection: true, expected: http.StatusServiceUnavailable},
		{name: "standby that has seen the leader starts no informers", leaderElection: true, sawLeader: true, expected: http.StatusOK},
		{name: "leader, informers not synced", leaderElection: true, leading: true, sawLeader: true, expected: http.StatusServiceUnavailable},
		{name: "leader, informers synced", leaderElection: true, leading: true, synced: true, expected: http.StatusOK},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ready := &readiness{leaderElection: tt.leaderElection, synced: func() bool { return tt.synced }}
			ready.leading.Store(tt.leading)
			ready.sawLeader.Store(tt.sawLeader)
			handler := healthHandler(ready)

			readyz := httptest.NewRecorder()
			handler.ServeHTTP(readyz, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			assert.Equal(t, tt.expected, readyz.Code)

			healthz := httptest.NewRecorder()
			handler.ServeHTTP(healthz, httptest.NewRequest(http.MethodGet, "/healthz", nil))
			assert.Equal(t, http.StatusOK, healthz.Code, "liveness does not depend on readiness")
		})
	}
}

// runElector runs an elector for the autoscaler against client until the test ends. started is
// closed when the instance starts the autoscaler.
func runElector(t *testing.T, client *fake.Clientset, synced *atomic.Bool) (ready *readiness, started chan struct{}) {
	t.Helper()
	lock, err := newLeaseLock(client, "kthena-system", "")
	require.NoError(t, err)
	ready = &readiness{leaderElection: true, synced: synced.Load}
	started = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	elector, err := newLeaderElector(ctx, lock, func(ctx context.Context) {
		close(started)
		<-ctx.Done()
	}, ready)
	require.NoError(t, err)
	done := make(chan struct{})
	go func() {
		elector.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return ready, started
}

func TestLeaderElectionReadiness(t *testing.T) {
	t.Run("the instance that takes the Lease is ready once its informers have synced", func(t *testing.T) {
		var synced atomic.Bool
		ready, started := runElector(t, fake.NewSimpleClientset(), &synced)

		select {
		case <-started:
		case <-time.After(10 * time.Second):
			t.Fatal("the autoscaler was not started")
		}
		assert.Equal(t, "informers have not synced", ready.notReady())
		synced.Store(true)
		assert.Empty(t, ready.notReady())
	})

	t.Run("a standby is ready once it has seen another instance hold the Lease", func(t *testing.T) {
		now := metav1.NewMicroTime(time.Now())
		client := fake.NewSimpleClientset(&coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: DefaultLeaderElectionResourceName, Namespace: "kthena-system"},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       ptr.To("another-instance"),
				LeaseDurationSeconds: ptr.To(int32(3600)),
				AcquireTime:          &now,
				RenewTime:            &now,
			},
		})
		var synced atomic.Bool
		ready, started := runElector(t, client, &synced)

		require.Eventually(t, func() bool { return ready.notReady() == "" }, 10*time.Second, 10*time.Millisecond)
		select {
		case <-started:
			t.Fatal("a standby must not start the autoscaler")
		default:
		}
	})

	t.Run("an instance that cannot read the Lease is not ready", func(t *testing.T) {
		client := fake.NewSimpleClientset()
		var requests atomic.Int32
		client.PrependReactor("*", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
			requests.Add(1)
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"}, DefaultLeaderElectionResourceName, nil)
		})
		var synced atomic.Bool
		synced.Store(true)
		ready, started := runElector(t, client, &synced)

		require.Eventually(t, func() bool { return requests.Load() > 0 }, 10*time.Second, 10*time.Millisecond)
		assert.Equal(t, "the leader election Lease has not been read", ready.notReady())
		select {
		case <-started:
			t.Fatal("the autoscaler must not start without the Lease")
		default:
		}
	})
}
