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
	"net/http"
	"os"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	autoscaler "github.com/volcano-sh/kthena/pkg/autoscaler/controller"
	"github.com/volcano-sh/kthena/pkg/kube"
	modelbooster "github.com/volcano-sh/kthena/pkg/model-booster-controller/controller"
	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
	modelserving "github.com/volcano-sh/kthena/pkg/model-serving-controller/controller"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/uuid"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	volcanoClientSet "volcano.sh/apis/pkg/client/clientset/versioned"
)

const (
	defaultLeaseDuration = 15 * time.Second
	defaultRenewDeadline = 10 * time.Second
	defaultRetryPeriod   = 2 * time.Second
	leaderElectionId     = "kthena.controller-manager"
	leaseName            = "lease.kthena.controller-manager"

	ModelServingController = "modelserving"
	ModelBoosterController = "modelbooster"
	AutoscalerController   = "autoscaler"
)

func SetupController(ctx context.Context, cc Config, health *HealthMonitor) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	config, err := kube.BuildConfig(cc.MasterURL, cc.Kubeconfig)
	if err != nil {
		return fmt.Errorf("build client config: %w", err)
	}
	// Set QPS and Burst if provided
	if cc.KubeAPIQPS > 0 {
		config.QPS = cc.KubeAPIQPS
	}
	if cc.KubeAPIBurst > 0 {
		config.Burst = cc.KubeAPIBurst
	}
	kubeClient := kubernetes.NewForConfigOrDie(config)
	client := clientset.NewForConfigOrDie(config)
	volcanoClient, err := volcanoClientSet.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create volcano client: %w", err)
	}
	apiextClient, err := apiextclient.NewForConfig(config)
	if err != nil {
		return fmt.Errorf("failed to create apiext client: %w", err)
	}

	var mc *modelbooster.ModelBoosterController
	var msc *modelserving.ModelServingController
	var lwsc *modelserving.LWSController
	var ac *autoscaler.AutoscaleController

	for ctrl, enable := range cc.Controllers {
		if enable {
			switch ctrl {
			case ModelBoosterController:
				mc = modelbooster.NewModelBoosterController(kubeClient, client)
			case ModelServingController:
				msc, err = modelserving.NewModelServingController(kubeClient, client, volcanoClient, apiextClient)
				if err != nil {
					return fmt.Errorf("failed to create ModelServing controller: %w", err)
				}
				lwsc, err = modelserving.InitializeLWSController(config, kubeClient, client)
				if err != nil {
					return fmt.Errorf("failed to initialize LWS controller: %w", err)
				} else if lwsc == nil {
					klog.Info("LeaderWorkerSet CRD not found, LWS support disabled")
				}
			case AutoscalerController:
				ac = autoscaler.NewAutoscaleController(kubeClient, client, cc.AutoscalingSyncPeriodSeconds)
				if ac == nil {
					return fmt.Errorf("failed to create autoscaler controller")
				}
			}
		}
	}

	startControllers := func(ctx context.Context) {
		if mc != nil {
			health.start(ctx, ModelBoosterController, func(ctx context.Context) error {
				mc.Run(ctx, cc.Workers)
				return nil
			})
			klog.Info("ModelBooster controller started")
		}
		if msc != nil {
			health.start(ctx, ModelServingController, func(ctx context.Context) error {
				msc.Run(ctx, cc.Workers)
				return nil
			})
			klog.Info("ModelServing controller started")

			if lwsc != nil {
				health.start(ctx, "lws", func(ctx context.Context) error { return lwsc.Run(ctx, 1) })
				klog.Info("ModelServing lws controller started")
			}
		}
		if ac != nil {
			health.start(ctx, AutoscalerController, func(ctx context.Context) error {
				ac.Run(ctx)
				return nil
			})
			klog.Info("Autoscaler controller started")
		}

		if cc.DebugPort > 0 {
			go func() {
				debugMux := http.ServeMux{}
				if msc != nil {
					msc.RegisterModelServingDebugEndpoints(&debugMux)
				}
				// Ensure the debug server is only accessible locally for security reasons
				debugAddr := fmt.Sprintf("localhost:%d", cc.DebugPort)
				klog.Infof("Starting debug server on %s", debugAddr)
				server := &http.Server{
					Addr:              debugAddr,
					Handler:           &debugMux,
					ReadHeaderTimeout: 5 * time.Second,
					ReadTimeout:       10 * time.Second,
					WriteTimeout:      10 * time.Second,
					IdleTimeout:       30 * time.Second,
				}
				go func() {
					<-ctx.Done()
					shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					_ = server.Shutdown(shutdownCtx)
				}()
				if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
					klog.Errorf("Debug server failed: %v", err)
				}
			}()
		}
	}

	if cc.EnableLeaderElection {
		startedLeading := func(ctx context.Context) {
			startControllers(ctx)
			klog.Info("Start as leader")
		}
		leaderElector, err := initLeaderElector(kubeClient, startedLeading)
		if err != nil {
			return fmt.Errorf("initialize leader election: %w", err)
		}
		health.start(ctx, "leader-election", func(ctx context.Context) error {
			leaderElector.Run(ctx)
			return nil
		})
	} else {
		startControllers(ctx)
		klog.Info("Started controllers without leader election")
	}
	return health.wait()
}

// initLeaderElector inits a leader elector for leader election
func initLeaderElector(kubeClient kubernetes.Interface, startedLeading func(ctx context.Context)) (*leaderelection.LeaderElector, error) {
	resourceLock, err := newResourceLock(kubeClient)
	if err != nil {
		return nil, err
	}
	leaderElector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:          resourceLock,
		LeaseDuration: defaultLeaseDuration,
		RenewDeadline: defaultRenewDeadline,
		RetryPeriod:   defaultRetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: startedLeading,
			OnStoppedLeading: func() {
				// The health monitor handles an unexpected return from Run.
				// This callback also runs during normal shutdown of a standby.
			},
		},
		ReleaseOnCancel: false,
		Name:            leaderElectionId,
	})
	if err != nil {
		return nil, err
	}
	return leaderElector, nil
}

// newResourceLock returns a lease lock which is used to elect leader
func newResourceLock(client kubernetes.Interface) (*resourcelock.LeaseLock, error) {
	namespace, err := utils.GetInClusterNameSpace()
	if err != nil {
		return nil, err
	}
	// Leader id, should be unique
	id, err := os.Hostname()
	if err != nil {
		return nil, err
	}
	id = id + "_" + string(uuid.NewUUID())
	return &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaseName,
			Namespace: namespace,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: id,
		},
	}, nil
}
