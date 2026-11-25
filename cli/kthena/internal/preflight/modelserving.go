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
	"slices"
	"sort"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Identifiers of the findings about one ModelServing.
const (
	FindingDeleting              = "deleting"
	FindingTakenOver             = "taken-over"
	FindingTakeoverInProgress    = "takeover-in-progress"
	FindingRollbackInProgress    = "rollback-in-progress"
	FindingNotObserved           = "not-observed"
	FindingRevisionUnresolved    = "revision-unresolved"
	FindingRevisionIdentity      = "revision-identity"
	FindingRolloutInProgress     = "rollout-in-progress"
	FindingRevisionUnknown       = "revision-unknown"
	FindingPartitionHolds        = "partition-holds"
	FindingGroupsExceedReplicas  = "groups-exceed-replicas"
	FindingGroupsBelowReplicas   = "groups-below-replicas"
	FindingOrdinalHoles          = "ordinal-holes"
	FindingSurgeGroups           = "surge-groups"
	FindingSurgeMarkPartial      = "surge-mark-partial"
	FindingSurgeMarkBelow        = "surge-mark-below-replicas"
	FindingUnmarkedBeyond        = "unmarked-group-beyond-replicas"
	FindingSurgeMarkIgnored      = "surge-mark-ignored"
	FindingRoleSurgeInstances    = "role-surge-instances"
	FindingTerminatingPods       = "terminating-pods"
	FindingFailedPods            = "failed-pods"
	FindingRestartingPods        = "restarting-pods"
	FindingInPlaceUpdate         = "inplace-update-in-progress"
	FindingPodNotMember          = "pod-not-a-member"
	FindingPodWithoutOwner       = "pod-without-owner"
	FindingPodForeignOwner       = "pod-foreign-owner"
	FindingServingGroupNameTaken = "servinggroup-name-taken"
	FindingObjectsWithoutPods    = "objects-of-groups-without-pods"
	FindingDryRunRejected        = "dry-run-rejected"
	FindingDryRunRejectedByOwn   = "dry-run-rejected-by-kthena-webhook"
	FindingDryRunUnknown         = "dry-run-unknown"
	FindingPodsOnCordonedNodes   = "pods-on-cordoned-nodes"
	FindingDisruptionBudget      = "disruption-budget-expected-count"
)

// modelServingInput is what the analysis of one ModelServing reads. The
// objects are the ones of its namespace that carry its name.
type modelServingInput struct {
	ms        *workloadv1alpha1.ModelServing
	pods      []*corev1.Pod
	services  []*corev1.Service
	podGroups []*schedulingv1beta1.PodGroup
	revisions []*appsv1.ControllerRevision
	// groups are the ServingGroup objects that have the name of a group of ms.
	groups []*workloadv1alpha1.ServingGroup
	// budgets are the PodDisruptionBudgets of the namespace.
	budgets  []*policyv1.PodDisruptionBudget
	cordoned map[string]bool
	dryRun   DryRunResult
	now      time.Time
}

// group is one ServingGroup as it exists before the upgrade: the member Pods
// that carry its name.
type group struct {
	name    string
	ordinal int
	// live are the member Pods that are not terminating. Only they are taken over.
	live        []*corev1.Pod
	terminating []*corev1.Pod
	// marked counts the Pods, terminating ones included, with the group-level
	// surge mark.
	marked int
	// surge: the ServingGroup object created for the group carries the surge
	// annotation.
	surge bool
	// ready: the group has live Pods and all of them are running and ready.
	ready bool
	// cost is the sum of the Pod deletion cost of the live Pods.
	cost int64
	// atTarget compares the group with the target revision.
	atTarget workload.Comparison
}

// unit is what a rollout replaces: a regular group, or a Role instance under
// RoleRollingUpdate.
type unit struct {
	name string
	// held: a partition keeps the unit at its revision.
	held bool
	// atTarget compares the unit with the target revision; today with the
	// revision the running version rolls to.
	atTarget, today workload.Comparison
	// revision is the revision of a group, or a revision of a Role instance
	// that is not the target. why says why a group has no revision.
	revision, why string
}

// analysis is the state of the analysis of one ModelServing.
type analysis struct {
	in     modelServingInput
	report ModelServingReport

	roleMode bool
	replicas int
	// groups are all groups that have a member Pod, by ordinal; live the ones
	// with a Pod that is not terminating; regular the live ones that will not
	// carry the surge annotation.
	groups, live, regular []*group
	// strays are the Pods with the name label that are not members of a group.
	strays []*corev1.Pod
	// target is the revision the new controller rolls to; unresolved says why
	// it cannot be computed.
	target     target
	unresolved error
	units      []unit
	// The units by what happens to them after the upgrade: replaced, kept by a
	// partition, not comparable; behindToday are the ones to replace that the
	// running version has not brought to its own target either. why says why
	// the first unit that is not comparable is not.
	toRoll, held, unknown, behindToday []string
	why                                string
}

func (a *analysis) add(finding Finding) {
	a.report.Findings = append(a.report.Findings, finding)
}

// analyzeModelServing predicts what the new controllers do with one
// ModelServing when they take it over in the state it is in.
func analyzeModelServing(in modelServingInput) ModelServingReport {
	ms := in.ms
	a := &analysis{in: in, replicas: 1}
	if ms.Spec.Replicas != nil {
		a.replicas = int(*ms.Spec.Replicas)
	}
	strategy := ms.Spec.RolloutStrategy
	a.roleMode = strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate

	a.report = ModelServingReport{
		Namespace:  ms.Namespace,
		Name:       ms.Name,
		UID:        ms.UID,
		Generation: ms.Generation,
		Adoption:   ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey],
		DryRun:     in.dryRun,
		Status: StatusInfo{
			ObservedGeneration: ms.Status.ObservedGeneration,
			Replicas:           ms.Status.Replicas,
			CurrentReplicas:    ms.Status.CurrentReplicas,
			UpdatedReplicas:    ms.Status.UpdatedReplicas,
			AvailableReplicas:  ms.Status.AvailableReplicas,
			CurrentRevision:    ms.Status.CurrentRevision,
			UpdateRevision:     ms.Status.UpdateRevision,
		},
		Findings:           []Finding{},
		PodChangesExpected: []string{},
		Groups:             []GroupInfo{},
	}
	a.rolloutInfo()
	a.sortPods()

	switch {
	case ms.DeletionTimestamp != nil:
		a.add(Finding{ID: FindingDeleting, Level: LevelWait, changesPods: true,
			Message: "The ModelServing is being deleted.",
			Action:  "Wait until it is gone."})
	case a.takeoverStarted():
	default:
		a.observed()
	}

	a.groupInfo()
	sortFindings(a.report.Findings)
	for _, finding := range a.report.Findings {
		if finding.changesPods {
			a.report.PodChangesExpected = append(a.report.PodChangesExpected, finding.ID)
		}
	}
	a.report.Verdict = verdictOf(a.report.Findings)
	return a.report
}

// rolloutInfo records the rollout settings.
func (a *analysis) rolloutInfo() {
	ms := a.in.ms
	info := RolloutInfo{
		Strategy:       string(workloadv1alpha1.ServingGroupRollingUpdate),
		Replicas:       a.replicas,
		RecoveryPolicy: string(ms.Spec.RecoveryPolicy),
		Ordinals:       []int{},
		Holes:          []int{},
	}
	if info.RecoveryPolicy == "" {
		info.RecoveryPolicy = string(workloadv1alpha1.RoleRecreate)
	}
	// A partition is rounded up, as the new controller does.
	scaled := func(value *intstr.IntOrString, total int) int {
		if value == nil {
			return 0
		}
		resolved, err := intstr.GetScaledValueFromIntOrPercent(value, total, true)
		if err != nil {
			return 0
		}
		return resolved
	}
	if a.roleMode {
		info.Strategy = string(workloadv1alpha1.RoleRollingUpdate)
		for _, role := range ms.Spec.Template.Roles {
			if partition := scaled(role.Partition, int(workload.RoleReplicas(role))); partition > 0 {
				if info.RolePartitions == nil {
					info.RolePartitions = map[string]int{}
				}
				info.RolePartitions[role.Name] = partition
			}
		}
	} else if strategy := ms.Spec.RolloutStrategy; strategy != nil && strategy.RollingUpdateConfiguration != nil {
		info.Partition = scaled(strategy.RollingUpdateConfiguration.Partition, a.replicas)
	}
	a.report.Rollout = info
}

// sortPods builds the groups from the Pods that are members of one and
// collects the Pods that are not.
//
// A Pod is a member of a group when its labels name the group and a Role
// instance of it, its name is that of a member of the instance
// (<group>-<role>-<instance>-<member>), and the ModelServing controls it or
// nothing does. That is the rule the new controllers adopt Pods by.
func (a *analysis) sortPods() {
	ms := a.in.ms
	// A Pod that a ServingGroup of ms controls was taken over already.
	own := map[string]bool{string(ms.UID): true}
	for _, object := range a.in.groups {
		if controlledBy(object, ms.UID) {
			own[string(object.UID)] = true
		}
	}
	byOrdinal := map[int]*group{}
	pods := slices.Clone(a.in.pods)
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	for _, pod := range pods {
		groupName := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		prefix, ordinal, ok := splitOrdinal(groupName)
		named := ok && prefix == ms.Name && servinggroup.HasMemberIdentity(pod, ms.Name, groupName)
		owner := metav1.GetControllerOfNoCopy(pod)
		if !named || (owner != nil && !own[string(owner.UID)]) {
			a.strays = append(a.strays, pod)
			continue
		}
		g := byOrdinal[ordinal]
		if g == nil {
			g = &group{name: groupName, ordinal: ordinal}
			byOrdinal[ordinal] = g
			a.groups = append(a.groups, g)
		}
		if pod.DeletionTimestamp != nil {
			g.terminating = append(g.terminating, pod)
		} else {
			g.live = append(g.live, pod)
		}
		if pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup {
			g.marked++
		}
	}
	sort.Slice(a.groups, func(i, j int) bool { return a.groups[i].ordinal < a.groups[j].ordinal })

	present := map[int]bool{}
	for _, g := range a.groups {
		// The rule by which the takeover decides the surge annotation, once:
		// the ordinal is not below spec.replicas and at least one Pod carries
		// the mark. Under RoleRollingUpdate no group is temporary.
		g.surge = !a.roleMode && g.ordinal >= a.replicas && g.marked > 0
		if len(g.live) == 0 {
			continue
		}
		g.ready = true
		for _, pod := range g.live {
			g.ready = g.ready && utils.IsPodRunningAndReady(pod)
			if cost, err := strconv.ParseInt(pod.Annotations[corev1.PodDeletionCost], 10, 64); err == nil {
				g.cost += cost
			}
		}
		a.live = append(a.live, g)
		if !g.surge {
			a.regular = append(a.regular, g)
		}
		present[g.ordinal] = true
		a.report.Rollout.Ordinals = append(a.report.Rollout.Ordinals, g.ordinal)
	}
	if n := len(a.live); n > 0 {
		for ordinal := 0; ordinal < a.live[n-1].ordinal; ordinal++ {
			if !present[ordinal] {
				a.report.Rollout.Holes = append(a.report.Rollout.Holes, ordinal)
			}
		}
	}
}

// takeoverStarted reports, with a finding, that the new version has started
// on this ModelServing. What the check predicts has then happened already.
func (a *analysis) takeoverStarted() bool {
	switch a.report.Adoption {
	case "":
		return false
	case workloadv1alpha1.ServingGroupAdoptionDone:
		a.add(Finding{ID: FindingTakenOver, Level: LevelInfo,
			Message: "The ModelServing was taken over by the new version already (adoption annotation Done). The pre-upgrade findings do not apply."})
	case workloadv1alpha1.ServingGroupAdoptionRollingBack:
		a.add(Finding{ID: FindingRollbackInProgress, Level: LevelWait, changesPods: true,
			Message: "The workloads of the ModelServing are being handed back to the earlier version (adoption annotation RollingBack).",
			Action:  "Wait until the rollback has removed the annotation, or delete the rollback switch to abandon the rollback."})
	default:
		a.add(Finding{ID: FindingTakeoverInProgress, Level: LevelWait, changesPods: true,
			Message: fmt.Sprintf("The new version is taking over the ModelServing (adoption annotation %s).", a.report.Adoption),
			Action:  "Wait until the annotation is Done."})
	}
	return true
}

// observed reports a spec the running controller has not processed.
func (a *analysis) observed() {
	ms := a.in.ms
	if ms.Status.ObservedGeneration >= ms.Generation {
		return
	}
	a.add(Finding{ID: FindingNotObserved, Level: LevelWait, changesPods: true,
		Message: fmt.Sprintf("The running controller has processed generation %d of the spec; the current generation is %d. What it still has to do for the change is not visible yet.",
			ms.Status.ObservedGeneration, ms.Generation),
		Action: "Wait until status.observedGeneration equals metadata.generation."})
}

// groupInfo records the groups in the report.
func (a *analysis) groupInfo() {
	for _, g := range a.groups {
		info := GroupInfo{
			Name: g.name, Ordinal: g.ordinal, Pods: len(g.live), TerminatingPods: len(g.terminating),
			SurgeMarkedPods: g.marked, SurgeAfterTakeover: g.surge && len(g.live) > 0,
			Ready: g.ready, DeletionCost: g.cost, Revisions: []string{}, AtTarget: "unknown",
		}
		seen := map[string]bool{}
		for _, pod := range g.live {
			if utils.IsPodRunningAndReady(pod) {
				info.ReadyPods++
			}
			if revision := runningRevision(pod); !seen[revision] {
				seen[revision] = true
				info.Revisions = append(info.Revisions, revision)
			}
		}
		sort.Strings(info.Revisions)
		switch g.atTarget {
		case workload.Equivalent:
			info.AtTarget = "yes"
		case workload.Different:
			info.AtTarget = "no"
		}
		a.report.Groups = append(a.report.Groups, info)
	}
}
