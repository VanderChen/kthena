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

package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/pflag"
	"github.com/volcano-sh/kthena/pkg/autoscaler/app"
	"github.com/volcano-sh/kthena/pkg/autoscaler/util"
	"k8s.io/klog/v2"
)

func main() {
	var c app.Config
	klog.InitFlags(nil)
	// Opt into fixed stderrthreshold behavior (kubernetes/klog#212).
	if err := flag.CommandLine.Set("legacy_stderr_threshold_behavior", "false"); err != nil {
		klog.Fatalf("Failed to set legacy_stderr_threshold_behavior: %v", err)
	}
	if err := flag.CommandLine.Set("stderrthreshold", "INFO"); err != nil {
		klog.Fatalf("Failed to set stderrthreshold: %v", err)
	}
	pflag.CommandLine.AddGoFlagSet(flag.CommandLine)
	pflag.StringVar(&c.Kubeconfig, "kubeconfig", "", "kubeconfig file path")
	pflag.StringVar(&c.MasterURL, "master", "", "The address of the Kubernetes API server. Overrides any value in kubeconfig. Only required if out-of-cluster.")
	pflag.Float32Var(&c.KubeAPIQPS, "kube-api-qps", 0, "QPS to use while talking with kubernetes apiserver. If 0, use default value.")
	pflag.IntVar(&c.KubeAPIBurst, "kube-api-burst", 0, "Burst to use while talking with kubernetes apiserver. If 0, use default value.")
	pflag.IntVar(&c.SyncPeriodSeconds, "autoscaling-sync-period-seconds", util.AutoscalingSyncPeriodSeconds,
		"Reconcile interval in seconds for the autoscaler. Smaller values react faster to traffic spikes but increase API server load. 0 falls back to the default (15).")
	pflag.BoolVar(&c.EnableLeaderElection, "leader-elect", false, "Enable leader election. "+
		"Enabling this will ensure there is only one active autoscaler. Default is false.")
	pflag.StringVar(&c.LeaderElectionResourceName, "leader-elect-resource-name", app.DefaultLeaderElectionResourceName,
		"The name of the Lease used for leader election.")
	pflag.StringVar(&c.HealthProbeBindAddress, "health-probe-bind-address", app.DefaultHealthProbeBindAddress,
		"The address /healthz and /readyz are served on. Empty disables them.")
	pflag.Parse()

	pflag.CommandLine.VisitAll(func(f *pflag.Flag) {
		klog.Infof("Flag: %s, Value: %s", f.Name, f.Value.String())
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		klog.Info("Received termination, signaling shutdown")
		cancel()
	}()

	if err := app.Run(ctx, c); err != nil {
		klog.Errorf("autoscaler failed: %v", err)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}
}
