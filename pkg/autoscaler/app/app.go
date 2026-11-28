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

// Package app runs the autoscaler as a process of its own.
package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	autoscaler "github.com/volcano-sh/kthena/pkg/autoscaler/controller"
	"github.com/volcano-sh/kthena/pkg/kube"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
)

const (
	// DefaultLeaderElectionResourceName is the Lease the autoscaler elects its leader on.
	DefaultLeaderElectionResourceName = "lease.kthena.autoscaler"
	// DefaultHealthProbeBindAddress is where the health endpoint listens.
	DefaultHealthProbeBindAddress = ":8081"

	leaderElectionName = "kthena.autoscaler"
	leaseDuration      = 15 * time.Second
	renewDeadline      = 10 * time.Second
	retryPeriod        = 2 * time.Second

	inClusterNamespacePath = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
)

// Config holds what the command line configures.
type Config struct {
	Kubeconfig   string
	MasterURL    string
	KubeAPIQPS   float32
	KubeAPIBurst int
	// SyncPeriodSeconds is the reconcile interval. 0 uses the autoscaler's default.
	SyncPeriodSeconds          int
	EnableLeaderElection       bool
	LeaderElectionResourceName string
	// HealthProbeBindAddress is the address of /healthz and /readyz. Empty disables them.
	HealthProbeBindAddress string
}

// readiness tells whether this instance does what it is there for. The instance that runs the
// autoscaler is ready once its informers have synced. A standby starts no informers; it is ready
// once it has read the Lease and found another instance holding it, which shows that it reaches
// the API server and is allowed to take part in the election.
type readiness struct {
	leaderElection bool
	leading        atomic.Bool
	sawLeader      atomic.Bool
	synced         func() bool
}

// notReady returns why this instance is not ready, or "" when it is.
func (r *readiness) notReady() string {
	if r.leaderElection && !r.leading.Load() {
		if !r.sawLeader.Load() {
			return "the leader election Lease has not been read"
		}
		return ""
	}
	if !r.synced() {
		return "informers have not synced"
	}
	return ""
}

// Run starts the autoscaler and returns when ctx is done, or with an error when it cannot start.
func Run(ctx context.Context, c Config) error {
	config, err := kube.BuildConfig(c.MasterURL, c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("build client config: %w", err)
	}
	if c.KubeAPIQPS > 0 {
		config.QPS = c.KubeAPIQPS
	}
	if c.KubeAPIBurst > 0 {
		config.Burst = c.KubeAPIBurst
	}
	kubeClient, err := kubernetes.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kubernetes client: %w", err)
	}
	client, err := clientset.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("create kthena client: %w", err)
	}

	controller := autoscaler.NewAutoscaleController(kubeClient, client, c.SyncPeriodSeconds)
	if controller == nil {
		return errors.New("create autoscaler controller")
	}
	ready := &readiness{leaderElection: c.EnableLeaderElection, synced: controller.HasSynced}

	// The health endpoint is up before leader election, so that standby instances are probed too.
	if c.HealthProbeBindAddress != "" {
		listener, err := net.Listen("tcp", c.HealthProbeBindAddress)
		if err != nil {
			return fmt.Errorf("listen on health probe address %s: %w", c.HealthProbeBindAddress, err)
		}
		go serveHealth(ctx, listener, ready)
	}

	if !c.EnableLeaderElection {
		klog.Info("Starting the autoscaler without leader election")
		controller.Run(ctx)
		return nil
	}

	namespace, err := inClusterNamespace()
	if err != nil {
		return err
	}
	lock, err := newLeaseLock(kubeClient, namespace, c.LeaderElectionResourceName)
	if err != nil {
		return err
	}
	elector, err := newLeaderElector(ctx, lock, controller.Run, ready)
	if err != nil {
		return fmt.Errorf("create leader elector: %w", err)
	}
	elector.Run(ctx)
	<-ctx.Done()
	return nil
}

// newLeaderElector returns an elector that calls run while this instance holds the lock and
// keeps ready up to date.
func newLeaderElector(ctx context.Context, lock resourcelock.Interface, run func(context.Context), ready *readiness) (*leaderelection.LeaderElector, error) {
	return leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          lock,
		LeaseDuration: leaseDuration,
		RenewDeadline: renewDeadline,
		RetryPeriod:   retryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				klog.Info("Starting the autoscaler as leader")
				ready.leading.Store(true)
				run(ctx)
			},
			OnStoppedLeading: func() {
				select {
				case <-ctx.Done():
					klog.Info("leader election stopped, the autoscaler is shutting down")
				default:
					// The reconcile loop does not stop with the lease. Exit so the Pod
					// restarts and rejoins the election as a standby.
					klog.Error("leader election lost, exiting")
					klog.FlushAndExit(klog.ExitFlushTimeout, 1)
				}
			},
			OnNewLeader: func(identity string) {
				if identity == lock.Identity() {
					return
				}
				klog.Infof("Standing by, the autoscaler leader is %s", identity)
				ready.sawLeader.Store(true)
			},
		},
		Name: leaderElectionName,
	})
}

// newLeaseLock returns the Lease lock of this instance. An empty name selects
// DefaultLeaderElectionResourceName.
func newLeaseLock(client kubernetes.Interface, namespace, name string) (*resourcelock.LeaseLock, error) {
	if name == "" {
		name = DefaultLeaderElectionResourceName
	}
	hostname, err := os.Hostname()
	if err != nil {
		return nil, fmt.Errorf("get hostname: %w", err)
	}
	return &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Client:    client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: hostname + "_" + string(uuid.NewUUID()),
		},
	}, nil
}

// inClusterNamespace returns the namespace of the Pod, where the Lease lives.
func inClusterNamespace() (string, error) {
	namespace, err := os.ReadFile(inClusterNamespacePath)
	if err != nil {
		return "", fmt.Errorf("leader election needs the Pod's namespace: %w", err)
	}
	return strings.TrimSpace(string(namespace)), nil
}

// healthHandler serves /healthz, which answers as long as the process does, and /readyz.
func healthHandler(ready *readiness) http.Handler {
	respond := func(w http.ResponseWriter, status int, body string) {
		w.WriteHeader(status)
		if _, err := w.Write([]byte(body)); err != nil {
			klog.Errorf("failed to write health check response: %v", err)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		respond(w, http.StatusOK, "ok")
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if reason := ready.notReady(); reason != "" {
			respond(w, http.StatusServiceUnavailable, reason)
			return
		}
		respond(w, http.StatusOK, "ok")
	})
	return mux
}

func serveHealth(ctx context.Context, listener net.Listener, ready *readiness) {
	server := &http.Server{
		Handler:           healthHandler(ready),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	klog.Infof("Starting health endpoint on %s", listener.Addr())
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.Errorf("health endpoint failed: %v", err)
	}
}
