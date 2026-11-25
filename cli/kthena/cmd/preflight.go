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

package cmd

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	"github.com/volcano-sh/kthena/cli/kthena/internal/preflight"
	"github.com/volcano-sh/kthena/client-go/clientset/versioned"
)

var (
	preflightBaselineFile    string
	preflightCompareBaseline string
	preflightSkipDryRun      bool
)

// The process boundary of the command: where its clients come from, and how
// it ends with the exit code of a check that ran and did not come out clean.
// Tests replace both.
var (
	preflightClients = getPreflightClients
	preflightExit    = os.Exit
)

// preflightCmd represents the preflight command
var preflightCmd = &cobra.Command{
	Use:   "preflight",
	Short: "Check resources before an upgrade of Kthena",
	Long: `Check resources before an upgrade of Kthena, and verify the upgrade afterwards.

The checks read the cluster and change nothing.

Examples:
  kthena preflight model-servings --all-namespaces
  kthena preflight model-servings -A --baseline-file baseline.json
  kthena preflight model-servings --compare-baseline baseline.json`,
}

// preflightModelServingsCmd represents the preflight model-servings command
var preflightModelServingsCmd = &cobra.Command{
	Use:     "model-servings [NAME]",
	Aliases: []string{"ms", "model-serving"},
	Short:   "Check ModelServings before the upgrade to ServingGroup objects",
	Long: `Check ModelServings before the controller manager is upgraded to the version
that keeps every ServingGroup of a ModelServing in a ServingGroup object.

The earlier versions own the Pods, PodGroups and Services of a ModelServing
directly; the new version takes them over without restarting a Pod. Run this
check against the cluster while it still runs the earlier version. It reports
on the cluster (controller manager, ServingGroup CRD, existing ServingGroup
objects, cordoned nodes, rollback switch) and gives every ModelServing a verdict:

  ready      can be upgraded without disruption
  attention  the upgrade works, and something happens that you should know about
  wait       something is in progress; the finding says what to wait for
  blocked    the ModelServing would not be taken over cleanly; the finding says why

The check is read-only with one exception: every ModelServing is patched once
with a server-side dry-run that adds the annotation
modelserving.volcano.sh/servinggroup-adoption. The API server admits and
validates the patch and persists nothing. It shows whether an admission webhook
or a policy would reject the write the takeover begins with. --skip-dry-run
leaves it out.

--baseline-file records the Pods (UID, restart counts, readiness, node), the
PodGroups and headless Services (UID) and the status of every ModelServing.
After the upgrade, --compare-baseline checks the cluster against that record:
every ModelServing is taken over, every group with Pods has its ServingGroup,
no Pod, PodGroup or Service was replaced, no container restarted, and
status.availableReplicas did not drop. The scope of the comparison is the scope
of the baseline.

Exit codes:
  0  every ModelServing and the cluster are ready; or the comparison found no change
  1  the check could not run
  2  the worst verdict is wait or attention; or the comparison found only changes
     that the pre-flight check announced or a change of the spec explains
  3  a verdict is blocked; or the comparison found a change that must not happen

Examples:
  kthena preflight model-servings --all-namespaces
  kthena preflight model-servings -n production -o json
  kthena preflight model-servings my-serving -n production --skip-dry-run
  kthena preflight model-servings -A --baseline-file baseline.json
  kthena preflight model-servings --compare-baseline baseline.json`,
	Args: cobra.MaximumNArgs(1),
	RunE: runPreflightModelServings,
}

func init() {
	rootCmd.AddCommand(preflightCmd)
	preflightCmd.AddCommand(preflightModelServingsCmd)

	// Add output format flag
	preflightCmd.PersistentFlags().StringVarP(&outputFormat, "output", "o", "", "Output format (table|json)")

	// Add namespace flags
	preflightCmd.PersistentFlags().StringVarP(&getNamespace, "namespace", "n", "", "Kubernetes namespace (default: current context namespace)")
	preflightCmd.PersistentFlags().BoolVarP(&getAllNamespaces, "all-namespaces", "A", false, "Check resources across all namespaces")

	preflightModelServingsCmd.Flags().StringVar(&preflightBaselineFile, "baseline-file", "", "Write the baseline for the verification after the upgrade to this file")
	preflightModelServingsCmd.Flags().StringVar(&preflightCompareBaseline, "compare-baseline", "", "Compare the cluster with the baseline in this file instead of running the pre-flight check")
	preflightModelServingsCmd.Flags().BoolVar(&preflightSkipDryRun, "skip-dry-run", false, "Do not send the dry-run patch that tells whether the adoption annotation can be written")
}

// getPreflightClients builds the clients the check reads the cluster with and
// returns the namespace of the current kubeconfig context.
func getPreflightClients() (clients preflight.Clients, contextNamespace string, err error) {
	kubeConfig := newKubeConfig()

	restConfig, err := kubeConfig.ClientConfig()
	if err != nil {
		return clients, "", fmt.Errorf("failed to load kubeconfig: %v", err)
	}
	// One dry-run patch is sent per ModelServing; the default client rate
	// limit would make a large cluster wait for minutes.
	restConfig.QPS, restConfig.Burst = 50, 100

	if clients.Kube, err = kubernetes.NewForConfig(restConfig); err != nil {
		return clients, "", fmt.Errorf("failed to create kubernetes client: %v", err)
	}
	if clients.Kthena, err = versioned.NewForConfig(restConfig); err != nil {
		return clients, "", fmt.Errorf("failed to create kthena client: %v", err)
	}
	if clients.Volcano, err = volcano.NewForConfig(restConfig); err != nil {
		return clients, "", fmt.Errorf("failed to create volcano client: %v", err)
	}

	contextNamespace, _, err = kubeConfig.Namespace()
	if err != nil {
		contextNamespace = "default"
	}
	return clients, contextNamespace, nil
}

func runPreflightModelServings(cmd *cobra.Command, args []string) error {
	if outputFormat != "" && outputFormat != "table" && outputFormat != "json" {
		return fmt.Errorf("unsupported output format '%s': use table or json", outputFormat)
	}
	if preflightBaselineFile != "" && preflightCompareBaseline != "" {
		return fmt.Errorf("--baseline-file and --compare-baseline cannot be used together")
	}
	if len(args) > 0 && getAllNamespaces {
		return fmt.Errorf("a ModelServing cannot be named together with --all-namespaces")
	}

	clients, contextNamespace, err := preflightClients()
	if err != nil {
		return err
	}
	scope := preflight.Scope{Namespace: resolveGetNamespace(contextNamespace), AllNamespaces: getAllNamespaces}
	if len(args) > 0 {
		scope.ModelServing = args[0]
	}
	// A scope given on the command line must be the one of the baseline.
	scopeGiven := len(args) > 0 || cmd.Flags().Changed("namespace") || cmd.Flags().Changed("all-namespaces")

	code, err := preflightModelServings(context.Background(), clients, preflightOptions{
		scope:           scope,
		scopeGiven:      scopeGiven,
		output:          outputFormat,
		baselineFile:    preflightBaselineFile,
		compareBaseline: preflightCompareBaseline,
		skipDryRun:      preflightSkipDryRun,
	}, cmd.OutOrStdout(), cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if code != preflight.ExitReady {
		preflightExit(code)
	}
	return nil
}

// preflightOptions are the flags of preflight model-servings.
type preflightOptions struct {
	scope preflight.Scope
	// scopeGiven reports that the scope was given on the command line.
	scopeGiven      bool
	output          string
	baselineFile    string
	compareBaseline string
	skipDryRun      bool
}

// preflightModelServings runs the pre-flight check, or the comparison with a
// baseline, writes the result to out and returns the exit code it stands
// for. Messages that are not part of the result go to errOut, so that JSON
// output stays parseable.
func preflightModelServings(ctx context.Context, clients preflight.Clients, opts preflightOptions, out, errOut io.Writer) (int, error) {
	if opts.compareBaseline != "" {
		baseline, err := preflight.ReadBaseline(opts.compareBaseline)
		if err != nil {
			return 0, err
		}
		if opts.scopeGiven && opts.scope != baseline.Scope {
			return 0, fmt.Errorf("the baseline was taken for %s; omit the namespace flags and the name to compare that scope", baseline.Scope)
		}
		snapshot, err := preflight.Collect(ctx, clients, preflight.CollectOptions{Scope: baseline.Scope, WorkloadsOnly: true})
		if err != nil {
			return 0, err
		}
		comparison := preflight.Compare(baseline, snapshot)
		if opts.output == "json" {
			err = preflight.WriteJSON(out, comparison)
		} else {
			err = comparison.WriteTable(out)
		}
		return comparison.ExitCode(), err
	}

	snapshot, err := preflight.Collect(ctx, clients, preflight.CollectOptions{Scope: opts.scope, SkipDryRun: opts.skipDryRun})
	if err != nil {
		return 0, err
	}
	report := preflight.Analyze(snapshot)
	if opts.output == "json" {
		err = preflight.WriteJSON(out, report)
	} else {
		err = report.WriteTable(out)
	}
	if err != nil {
		return 0, err
	}
	if opts.baselineFile != "" {
		if err := preflight.WriteBaseline(opts.baselineFile, preflight.NewBaseline(snapshot, report)); err != nil {
			return 0, err
		}
		fmt.Fprintf(errOut, "Baseline written to %s\n", opts.baselineFile)
	}
	return report.ExitCode(), nil
}
