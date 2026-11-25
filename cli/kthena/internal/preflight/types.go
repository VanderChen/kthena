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

// Package preflight checks a cluster before the controller manager is
// upgraded to the version that keeps every ServingGroup of a ModelServing in
// a ServingGroup object, and verifies the upgrade afterwards.
//
// The earlier versions own the Pods, PodGroups and Services of a ModelServing
// directly. The new version takes them over in place. The check predicts, from
// the objects in the cluster, what the new controllers will do with every
// ModelServing once they start.
//
// Collect reads the cluster. Everything else is a function of what was read:
// Analyze turns a Snapshot into a Report, NewBaseline records what must not
// change, and Compare checks a later Snapshot against that record.
package preflight

import (
	"sort"
	"time"

	"k8s.io/apimachinery/pkg/types"
)

// FormatVersion names the JSON layout of Report, Baseline and Comparison.
// Fields are only added within one version.
const FormatVersion = "v1"

// Level says how much a finding matters for the upgrade.
type Level string

const (
	// LevelInfo describes a state that the upgrade handles without any effect
	// on the workload. It does not change the verdict.
	LevelInfo Level = "info"
	// LevelAttention: the upgrade works, and something happens that the
	// operator should know about.
	LevelAttention Level = "attention"
	// LevelWait: something is in progress. The finding says what to wait for.
	LevelWait Level = "wait"
	// LevelBlocked: the workload would not be taken over cleanly.
	LevelBlocked Level = "blocked"
)

// rank orders the levels by severity.
func (l Level) rank() int {
	switch l {
	case LevelAttention:
		return 1
	case LevelWait:
		return 2
	case LevelBlocked:
		return 3
	}
	return 0
}

// Verdict is the conclusion for one ModelServing, for the cluster, or for
// the whole report: the most severe level among the findings behind it.
type Verdict string

const (
	// VerdictReady: can be upgraded without disruption.
	VerdictReady Verdict = "ready"
	// VerdictAttention: see LevelAttention.
	VerdictAttention Verdict = "attention"
	// VerdictWait: see LevelWait.
	VerdictWait Verdict = "wait"
	// VerdictBlocked: see LevelBlocked.
	VerdictBlocked Verdict = "blocked"
)

func (v Verdict) rank() int {
	switch v {
	case VerdictAttention:
		return 1
	case VerdictWait:
		return 2
	case VerdictBlocked:
		return 3
	}
	return 0
}

func worse(a, b Verdict) Verdict {
	if b.rank() > a.rank() {
		return b
	}
	return a
}

// verdictOf returns the verdict that a list of findings amounts to.
func verdictOf(findings []Finding) Verdict {
	verdict := VerdictReady
	for _, finding := range findings {
		switch finding.Level {
		case LevelAttention:
			verdict = worse(verdict, VerdictAttention)
		case LevelWait:
			verdict = worse(verdict, VerdictWait)
		case LevelBlocked:
			verdict = worse(verdict, VerdictBlocked)
		}
	}
	return verdict
}

// sortFindings puts the most severe findings first and keeps the order of
// findings of one level.
func sortFindings(findings []Finding) {
	sort.SliceStable(findings, func(i, j int) bool { return findings[i].Level.rank() > findings[j].Level.rank() })
}

// Finding is one observation and what follows from it for the upgrade.
type Finding struct {
	// ID identifies the kind of finding. It is stable across releases.
	ID    string `json:"id"`
	Level Level  `json:"level"`
	// Message says what was observed and what happens after the upgrade.
	Message string `json:"message"`
	// Action says what to do, or what to wait for, before upgrading.
	Action string `json:"action,omitempty"`
	// Objects names the objects the finding is about.
	Objects []string `json:"objects,omitempty"`

	// changesPods: because of what was found, the controller will delete,
	// replace or restart Pods of the ModelServing by itself after the upgrade.
	changesPods bool
}

// Scope is the part of the cluster a check covers.
type Scope struct {
	// Namespace is empty when AllNamespaces is set.
	Namespace     string `json:"namespace"`
	AllNamespaces bool   `json:"allNamespaces"`
	// ModelServing limits the check to one ModelServing of Namespace.
	ModelServing string `json:"modelServing,omitempty"`
}

func (s Scope) String() string {
	switch {
	case s.AllNamespaces:
		return "all namespaces"
	case s.ModelServing != "":
		return "ModelServing " + s.Namespace + "/" + s.ModelServing
	}
	return "namespace " + s.Namespace
}

// Report is the result of the pre-flight check.
type Report struct {
	FormatVersion string    `json:"formatVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Scope         Scope     `json:"scope"`
	// DryRun is "performed" or "skipped".
	DryRun string `json:"dryRun"`
	// Verdict is the most severe of the cluster verdict and the verdicts of
	// the ModelServings.
	Verdict       Verdict              `json:"verdict"`
	Summary       Summary              `json:"summary"`
	Cluster       ClusterReport        `json:"cluster"`
	ModelServings []ModelServingReport `json:"modelServings"`
}

// Summary counts the ModelServings of a report by verdict.
type Summary struct {
	ModelServings int `json:"modelServings"`
	Ready         int `json:"ready"`
	Attention     int `json:"attention"`
	Wait          int `json:"wait"`
	Blocked       int `json:"blocked"`
}

// ClusterReport holds what does not belong to one ModelServing.
type ClusterReport struct {
	Verdict Verdict `json:"verdict"`
	// ControllerManagers lists every namespace in which a controller manager
	// Deployment or controller manager Pods were found. More than one entry
	// means that two installations reconcile the same cluster.
	ControllerManagers []ControllerManager `json:"controllerManagers"`
	// ServingGroupCRD is "installed", "not-installed" or "unknown".
	ServingGroupCRD string `json:"servingGroupCRD"`
	// ServingGroups lists the ServingGroup objects that exist in the scope.
	ServingGroups []ServingGroupRef `json:"servingGroups"`
	CordonedNodes []CordonedNode    `json:"cordonedNodes"`
	// RollbackSwitches lists the rollback switch ConfigMaps that exist, as
	// namespace/name.
	RollbackSwitches []string  `json:"rollbackSwitches"`
	Webhooks         []Webhook `json:"webhooks"`
	// EvictionWebhook is "enabled", "disabled" or "unknown".
	EvictionWebhook string    `json:"evictionWebhook"`
	Findings        []Finding `json:"findings"`
}

// ControllerManager describes the controller manager found in one namespace.
type ControllerManager struct {
	Namespace string `json:"namespace"`
	// Deployment is empty when Pods were found without the Deployment.
	Deployment     string `json:"deployment,omitempty"`
	Image          string `json:"image,omitempty"`
	Replicas       int32  `json:"replicas"`
	ReadyReplicas  int32  `json:"readyReplicas"`
	UpdateStrategy string `json:"updateStrategy,omitempty"`
	// LeaderElectFlag is "true" or "false" as given on the command line, or
	// "unset".
	LeaderElectFlag string `json:"leaderElectFlag"`
	// LeaderElect is the effective setting: the flag, or the default of the
	// version when the flag is not set.
	LeaderElect bool `json:"leaderElect"`
	// Protocol tells which versions run: "legacy" when no running Pod carries
	// the controller-protocol label (not upgraded), "servinggroup" when every
	// running Pod does (upgraded), "mixed" during an upgrade, "none" when no
	// Pod runs.
	Protocol string          `json:"protocol"`
	Pods     []ControllerPod `json:"pods"`
}

// ControllerPod is one Pod of a controller manager.
type ControllerPod struct {
	Name  string `json:"name"`
	Phase string `json:"phase"`
	Node  string `json:"node,omitempty"`
	// Protocol is the value of the controller-protocol label; empty for a Pod
	// of an earlier version.
	Protocol string `json:"protocol,omitempty"`
}

// ServingGroupRef describes an existing ServingGroup object.
type ServingGroupRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	// ModelServing is the ModelServing the name of the object suggests, when a
	// ModelServing of that name exists.
	ModelServing string `json:"modelServing,omitempty"`
	// Controlled reports that ModelServing is the controller owner, by UID.
	Controlled bool `json:"controlled"`
	// Owner is the controller owner as Kind/name, or empty.
	Owner string `json:"owner,omitempty"`
}

// CordonedNode is a node that is marked unschedulable.
type CordonedNode struct {
	Name string `json:"name"`
	// ModelServingPods counts the Pods of the checked ModelServings on it.
	ModelServingPods int `json:"modelServingPods"`
}

// Webhook is one webhook of the controller manager's validating webhook
// configuration.
type Webhook struct {
	Name          string `json:"name"`
	FailurePolicy string `json:"failurePolicy"`
}

// ModelServingReport is the result for one ModelServing.
type ModelServingReport struct {
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
	Verdict    Verdict   `json:"verdict"`
	Findings   []Finding `json:"findings"`
	// PodChangesExpected lists the findings because of which the controller
	// changes Pods of this ModelServing by itself after the upgrade. When it is
	// empty the comparison with the baseline accepts no change.
	PodChangesExpected []string `json:"podChangesExpected"`
	// Adoption is the value of the adoption annotation; empty before the
	// upgrade.
	Adoption string       `json:"adoption,omitempty"`
	Rollout  RolloutInfo  `json:"rollout"`
	Revision RevisionInfo `json:"revision"`
	Groups   []GroupInfo  `json:"groups"`
	DryRun   DryRunResult `json:"dryRun"`
	Status   StatusInfo   `json:"status"`
}

// RolloutInfo describes the rollout settings and the groups that exist.
type RolloutInfo struct {
	// Strategy is ServingGroupRollingUpdate or RoleRollingUpdate.
	Strategy string `json:"strategy"`
	Replicas int    `json:"replicas"`
	// Partition is the effective partition of a ServingGroupRollingUpdate: the
	// groups with an ordinal below it are not rolled.
	Partition int `json:"partition"`
	// RolePartitions is the effective partition of each Role that has one,
	// under RoleRollingUpdate.
	RolePartitions map[string]int `json:"rolePartitions,omitempty"`
	RecoveryPolicy string         `json:"recoveryPolicy"`
	// Ordinals lists the ordinals of the groups that have a Pod that is not
	// terminating.
	Ordinals []int `json:"ordinals"`
	// Holes lists the ordinals below the highest one that have no such group.
	Holes []int `json:"holes"`
}

// RevisionInfo compares the revision the new controller will roll to with
// the revisions the Pods run.
type RevisionInfo struct {
	// Target is the identity the new controller gives the current template.
	Target string `json:"target"`
	// TargetSource is "existing" when a ControllerRevision of the ModelServing
	// renders the same Pods and its identity is reused, "new" when the
	// identity is computed from the template.
	TargetSource string `json:"targetSource"`
	// ControllerRevision is the name of the ControllerRevision of Target.
	ControllerRevision string `json:"controllerRevision"`
	// Unit is what a rollout replaces: "group", or "role-instance" under
	// RoleRollingUpdate.
	Unit string `json:"unit"`
	// Units counts the units that have a Pod that is not terminating,
	// temporary surge groups excluded.
	Units int `json:"units"`
	// ToRoll counts the units the new controller replaces after the upgrade.
	ToRoll int `json:"toRoll"`
	// HeldByPartition counts the units that are not at the target and stay as
	// they are because of a partition.
	HeldByPartition int `json:"heldByPartition"`
	// Unknown counts the units whose revision cannot be compared.
	Unknown int `json:"unknown"`
	// Statement is "no rollout after the upgrade" or "<k> <units> will be
	// rolled after the upgrade".
	Statement string `json:"statement"`
}

// GroupInfo describes one ServingGroup as it exists today: the Pods that
// carry its name.
type GroupInfo struct {
	Name    string `json:"name"`
	Ordinal int    `json:"ordinal"`
	// Pods counts the member Pods that are not terminating.
	Pods            int `json:"pods"`
	ReadyPods       int `json:"readyPods"`
	TerminatingPods int `json:"terminatingPods"`
	// Revisions lists the revisions the Pods run.
	Revisions []string `json:"revisions"`
	// AtTarget is "yes", "no" or "unknown".
	AtTarget string `json:"atTarget"`
	// SurgeMarkedPods counts the Pods, terminating ones included, that carry
	// the group-level surge mark.
	SurgeMarkedPods int `json:"surgeMarkedPods"`
	// SurgeAfterTakeover reports that the ServingGroup object created for the
	// group will carry the surge annotation: it is temporary capacity.
	SurgeAfterTakeover bool `json:"surgeAfterTakeover"`
	// Ready: the group has Pods and all of them are running and ready.
	Ready        bool  `json:"ready"`
	DeletionCost int64 `json:"deletionCost"`
}

// DryRunResult is the outcome of the dry-run write of the adoption
// annotation.
type DryRunResult struct {
	// Outcome is "passed", "rejected", "unknown" or "skipped".
	Outcome string `json:"outcome"`
	// Webhook names the admission webhook that rejected the write.
	Webhook string `json:"webhook,omitempty"`
	Message string `json:"message,omitempty"`
}

// Outcomes of the dry-run write.
const (
	DryRunPassed   = "passed"
	DryRunRejected = "rejected"
	DryRunUnknown  = "unknown"
	DryRunSkipped  = "skipped"
)

// StatusInfo is the status of a ModelServing as the running version wrote it.
type StatusInfo struct {
	ObservedGeneration int64  `json:"observedGeneration"`
	Replicas           int32  `json:"replicas"`
	CurrentReplicas    int32  `json:"currentReplicas"`
	UpdatedReplicas    int32  `json:"updatedReplicas"`
	AvailableReplicas  int32  `json:"availableReplicas"`
	CurrentRevision    string `json:"currentRevision"`
	UpdateRevision     string `json:"updateRevision"`
}

// Exit codes of the command. 1 is left to a command that could not run.
const (
	// ExitReady: every ModelServing and the cluster are ready, or a comparison
	// found no change.
	ExitReady = 0
	// ExitAttention: the worst verdict is wait or attention, or a comparison
	// found only changes that the pre-flight check announced.
	ExitAttention = 2
	// ExitBlocked: a verdict is blocked, or a comparison found a change that
	// must not happen.
	ExitBlocked = 3
)
