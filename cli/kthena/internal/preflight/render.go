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

package preflight

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

func writeFindings(w io.Writer, findings []Finding) {
	for _, finding := range findings {
		fmt.Fprintf(w, "  [%s] %s\n", finding.Level, finding.ID)
		fmt.Fprintln(w, wrap(finding.Message, "      ", textWidth))
		if finding.Action != "" {
			fmt.Fprintln(w, wrap("Action: "+finding.Action, "      ", textWidth))
		}
	}
}

// WriteTable writes the report for a person to read.
func (r *Report) WriteTable(w io.Writer) error {
	fmt.Fprintln(w, "Pre-flight check for the upgrade to ServingGroup objects")
	fmt.Fprintf(w, "Scope: %s   Dry-run: %s   Time: %s\n\n", r.Scope, r.DryRun, r.GeneratedAt.Format(time.RFC3339))

	fmt.Fprintf(w, "CLUSTER: %s\n", r.Cluster.Verdict)
	if err := r.Cluster.writeTable(w); err != nil {
		return err
	}

	fmt.Fprintln(w, "\nMODELSERVINGS")
	if len(r.ModelServings) == 0 {
		if r.Scope.AllNamespaces {
			fmt.Fprintln(w, "No ModelServings found across all namespaces.")
		} else {
			fmt.Fprintf(w, "No ModelServings found in namespace %s.\n", r.Scope.Namespace)
		}
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		header := "NAME\tVERDICT\tSTRATEGY\tREPLICAS\tGROUPS\tAVAILABLE\tPARTITION\tROLLOUT AFTER UPGRADE\tDRY-RUN"
		if r.Scope.AllNamespaces {
			header = "NAMESPACE\t" + header
		}
		fmt.Fprintln(tw, header)
		for _, ms := range r.ModelServings {
			groups := "0"
			if n := len(ms.Rollout.Ordinals); n > 0 {
				groups = fmt.Sprintf("%d (%s)", n, ordinalRanges(ms.Rollout.Ordinals))
			}
			partition := strconv.Itoa(ms.Rollout.Partition)
			if len(ms.Rollout.RolePartitions) > 0 {
				partition = "per Role"
			}
			rollout := "none"
			switch {
			case ms.Revision.Target == "":
				rollout = "unknown"
			case ms.Revision.ToRoll > 0 && ms.Revision.Unit == "group":
				rollout = counted(ms.Revision.ToRoll, "1 group", "%d groups")
			case ms.Revision.ToRoll > 0:
				rollout = counted(ms.Revision.ToRoll, "1 Role instance", "%d Role instances")
			}
			row := fmt.Sprintf("%s\t%s\t%s\t%d\t%s\t%d/%d\t%s\t%s\t%s",
				ms.Name, ms.Verdict, ms.Rollout.Strategy, ms.Rollout.Replicas, groups,
				ms.Status.AvailableReplicas, ms.Status.Replicas, partition, rollout, ms.DryRun.Outcome)
			if r.Scope.AllNamespaces {
				row = ms.Namespace + "\t" + row
			}
			fmt.Fprintln(tw, row)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	for _, ms := range r.ModelServings {
		if len(ms.Findings) == 0 {
			continue
		}
		fmt.Fprintf(w, "\n%s/%s: %s\n", ms.Namespace, ms.Name, ms.Verdict)
		if ms.Revision.Target != "" {
			fmt.Fprintf(w, "  Target revision: %s (%s ControllerRevision %s); %s\n",
				ms.Revision.Target, ms.Revision.TargetSource, ms.Revision.ControllerRevision, ms.Revision.Statement)
		}
		if len(ms.Groups) > 0 {
			tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "  GROUP\tPODS\tREADY\tTERMINATING\tREVISION\tAT TARGET\tSURGE MARK\tAFTER TAKEOVER")
			for _, g := range ms.Groups {
				after := "regular"
				switch {
				case g.Pods == 0:
					after = "not taken over"
				case g.SurgeAfterTakeover:
					after = "surge"
				}
				fmt.Fprintf(tw, "  %s\t%d\t%d\t%d\t%s\t%s\t%d/%d\t%s\n", g.Name, g.Pods, g.ReadyPods, g.TerminatingPods,
					strings.Join(g.Revisions, ","), g.AtTarget, g.SurgeMarkedPods, g.Pods+g.TerminatingPods, after)
			}
			if err := tw.Flush(); err != nil {
				return err
			}
		}
		writeFindings(w, ms.Findings)
	}

	fmt.Fprintf(w, "\nSummary: %s: %d ready, %d attention, %d wait, %d blocked; cluster: %s\n",
		counted(r.Summary.ModelServings, "1 ModelServing", "%d ModelServings"),
		r.Summary.Ready, r.Summary.Attention, r.Summary.Wait, r.Summary.Blocked, r.Cluster.Verdict)
	_, err := fmt.Fprintf(w, "Result: %s (exit code %d)\n", r.Verdict, r.ExitCode())
	return err
}

func (c *ClusterReport) writeTable(w io.Writer) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	if len(c.ControllerManagers) == 0 {
		fmt.Fprintln(tw, "  Controller manager:\tnot found")
	}
	for _, manager := range c.ControllerManagers {
		name := manager.Namespace + "/" + manager.Deployment
		if manager.Deployment == "" {
			name = manager.Namespace + " (Pods without the Deployment)"
		}
		fmt.Fprintf(tw, "  Controller manager:\t%s\n", name)
		if manager.Deployment != "" {
			fmt.Fprintf(tw, "    Image:\t%s\n", manager.Image)
			fmt.Fprintf(tw, "    Replicas:\t%d (%d ready), update strategy %s\n", manager.Replicas, manager.ReadyReplicas, manager.UpdateStrategy)
		}
		election := "off"
		if manager.LeaderElect {
			election = "on"
		}
		if manager.LeaderElectFlag == "unset" {
			election += " (--leader-elect not set: default of the version)"
		} else {
			election += " (--leader-elect=" + manager.LeaderElectFlag + ")"
		}
		fmt.Fprintf(tw, "    Leader election:\t%s\n", election)
		version := map[string]string{
			protocolLegacy:       "earlier version: no Pod carries the controller-protocol label",
			protocolServingGroup: "upgraded: every Pod carries the controller-protocol label",
			protocolMixed:        "upgrade in progress: Pods with and without the controller-protocol label",
			protocolNone:         "no running Pod",
		}[manager.Protocol]
		fmt.Fprintf(tw, "    Version:\t%s\n", version)
	}
	fmt.Fprintf(tw, "  ServingGroup CRD:\t%s\n", c.ServingGroupCRD)
	fmt.Fprintf(tw, "  ServingGroup objects:\t%d\n", len(c.ServingGroups))
	cordoned := make([]string, 0, len(c.CordonedNodes))
	for _, node := range c.CordonedNodes {
		cordoned = append(cordoned, fmt.Sprintf("%s (%s)", node.Name, counted(node.ModelServingPods, "1 ModelServing Pod", "%d ModelServing Pods")))
	}
	fmt.Fprintf(tw, "  Cordoned nodes:\t%s\n", listed(cordoned))
	fmt.Fprintf(tw, "  Rollback switch:\t%s\n", listed(c.RollbackSwitches))
	fmt.Fprintf(tw, "  Eviction webhook:\t%s\n", c.EvictionWebhook)
	policies := make([]string, 0, len(c.Webhooks))
	for _, webhook := range c.Webhooks {
		policies = append(policies, webhook.Name+"="+webhook.FailurePolicy)
	}
	if len(policies) > 0 {
		fmt.Fprintf(tw, "  Webhook failure policies:\t%s\n", strings.Join(policies, ", "))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	writeFindings(w, c.Findings)
	return nil
}
