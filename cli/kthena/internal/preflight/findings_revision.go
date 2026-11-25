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
	"sort"
	"strconv"
	"strings"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// compareRevisions resolves the target revision and compares the units of
// the rollout with it: the regular groups, or the Role instances of every
// live group under RoleRollingUpdate.
func (a *analysis) compareRevisions() {
	info := &a.report.Revision
	info.Unit = "group"
	if a.roleMode {
		info.Unit = "role-instance"
	}
	info.Statement = "no rollout after the upgrade"

	resolved, err := resolveTarget(a.in.ms, a.in.revisions)
	if err != nil {
		a.unresolved = err
		info.Statement = "unknown: the revision of the template cannot be computed"
		return
	}
	a.target = resolved
	info.Target, info.ControllerRevision = resolved.identity, resolved.name
	info.TargetSource = "new"
	if resolved.existing {
		info.TargetSource = "existing"
	}

	history, identity := resolved.history, resolved.identity
	old := a.in.ms.Status.UpdateRevision
	if !a.roleMode {
		for _, g := range a.live {
			u := unit{name: g.name, held: g.ordinal < a.report.Rollout.Partition}
			u.revision, u.why = groupRevision(g)
			u.atTarget, u.today = history.Compare(u.revision, identity), history.Compare(u.revision, old)
			g.atTarget = u.atTarget
			// Temporary capacity is removed, not rolled.
			if !g.surge {
				a.units = append(a.units, u)
			}
		}
		a.tally()
		return
	}
	for _, g := range a.live {
		g.atTarget = workload.Equivalent
		byID := map[string]*unit{}
		var ids []string
		for _, pod := range g.live {
			role, id := pod.Labels[workloadv1alpha1.RoleLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey]
			u := byID[id]
			if u == nil {
				_, index, _ := splitOrdinal(id)
				u = &unit{name: g.name + "/" + id, held: index < a.report.Rollout.RolePartitions[role],
					atTarget: workload.Equivalent, today: workload.Equivalent}
				byID[id] = u
				ids = append(ids, id)
			}
			revision := runningRevision(pod)
			result := history.CompareRole(revision, identity, role)
			settle(&u.atTarget, result)
			settle(&u.today, history.CompareRole(revision, old, role))
			if result != workload.Equivalent {
				u.revision = revision
			}
		}
		for _, id := range ids {
			settle(&g.atTarget, byID[id].atTarget)
			a.units = append(a.units, *byID[id])
		}
	}
	a.tally()
}

// tally sorts the units by what happens to them after the upgrade and
// records the counts.
func (a *analysis) tally() {
	for _, u := range a.units {
		switch {
		case u.atTarget == workload.Equivalent:
		case u.atTarget == workload.Unknown:
			a.unknown = append(a.unknown, u.name)
			if a.why == "" {
				if a.why = u.why; a.why == "" {
					a.why = a.target.why(u.revision)
				}
			}
		case u.held:
			a.held = append(a.held, u.name)
		default:
			a.toRoll = append(a.toRoll, u.name)
			if u.today != workload.Equivalent {
				a.behindToday = append(a.behindToday, u.name)
			}
		}
	}
	info := &a.report.Revision
	info.Units, info.ToRoll, info.HeldByPartition, info.Unknown = len(a.units), len(a.toRoll), len(a.held), len(a.unknown)
	if len(a.toRoll) > 0 {
		one, many := a.unitNouns()
		info.Statement = counted(len(a.toRoll), one, many) + " will be rolled after the upgrade"
	}
}

// unitNouns returns the words for one and for several units of the rollout.
func (a *analysis) unitNouns() (one, many string) {
	if a.roleMode {
		return "1 Role instance", "%d Role instances"
	}
	return "1 group", "%d groups"
}

// rollout says whether Pods are replaced after the upgrade because of their
// revision, and whether that is a rollout the running version has not
// finished or a template that the two versions identify differently.
func (a *analysis) rollout() {
	if a.unresolved != nil {
		a.add(Finding{ID: FindingRevisionUnresolved, Level: LevelBlocked,
			Message: fmt.Sprintf("The new controller cannot compute the revision of the template: %v. It reconciles nothing of this ModelServing until that is corrected.", a.unresolved),
			Action:  "Correct the spec of the ModelServing."})
		return
	}
	ms := a.in.ms
	one, many := a.unitNouns()
	toRoll, held, unknown := a.toRoll, a.held, a.unknown

	// The running version rolled to status.updateRevision. When it has seen
	// the current spec, and the new version resolves the same spec to a
	// revision that renders other Pods, the two versions disagree about the
	// identity of the template: Pods the running version considers up to date
	// are replaced.
	old := ms.Status.UpdateRevision
	disagree := ms.Status.ObservedGeneration >= ms.Generation && old != "" &&
		a.target.history.Compare(old, a.target.identity) == workload.Different
	if disagree && len(toRoll) > 0 {
		source := "computed from the template: no ControllerRevision of the ModelServing renders the same Pods"
		if a.target.existing {
			source = "ControllerRevision " + a.target.name
		}
		a.add(Finding{ID: FindingRevisionIdentity, Level: LevelAttention, changesPods: true, Objects: toRoll,
			Message: fmt.Sprintf("%s will be rolled after the upgrade although the spec was not changed (%s). The new version resolves the template to revision %s (%s), and that revision does not render the same Pods as revision %s, which the running version rolled to. The rollout follows the rolling update settings of the ModelServing.",
				counted(len(toRoll), one, many), listed(toRoll), a.target.identity, source, old),
			Action: fmt.Sprintf("Do not upgrade before this is understood: compare ControllerRevision %s with the template of the ModelServing.",
				utils.GenerateControllerRevisionName(ms.Name, old))})
	}
	rolling := toRoll
	if disagree {
		rolling = a.behindToday
	}
	if len(rolling) > 0 {
		a.add(Finding{ID: FindingRolloutInProgress, Level: LevelWait, changesPods: true, Objects: rolling,
			Message: fmt.Sprintf("A rollout is in progress: of %s, %s not yet at the target revision %s (%s). The new version continues the rollout after the upgrade and replaces them.",
				counted(len(a.units), one, many), counted(len(rolling), "1 is", "%d are"), a.target.identity, listed(rolling)),
			Action: "Wait until status.updatedReplicas equals spec.replicas, or accept that the new version finishes the rollout."})
	}
	if len(held) > 0 {
		a.add(Finding{ID: FindingPartitionHolds, Level: LevelInfo, Objects: held,
			Message: fmt.Sprintf("%s below the partition and not at the target revision (%s). The partition keeps them at the revision they run, before and after the upgrade.",
				counted(len(held), one+" is", many+" are"), listed(held))})
	}
	if len(unknown) > 0 {
		a.add(Finding{ID: FindingRevisionUnknown, Level: LevelAttention, Objects: unknown,
			Message: fmt.Sprintf("The revision of %s cannot be compared with the target revision %s (%s): %s. The new version takes them over and does not replace them on account of their revision: they stay as they are until their Pods are replaced.",
				counted(len(unknown), one, many), a.target.identity, listed(unknown), a.why),
			Action: "Restore the ControllerRevision when it is missing; otherwise delete the Pods that are not at the revision you want, so that they are created again."})
	}
}

// groupRevision returns the revision the takeover gives the ServingGroup
// object of a group: the revision an in-place update that is in progress
// leads to, else the revision label that all Pods of the group share. When
// the Pods do not agree the object gets no revision; why says so.
func groupRevision(g *group) (revision, why string) {
	labels := map[string]bool{}
	for _, pod := range g.live {
		if state := inplace.StateOf(pod); state.InProgress() && state.Revision != "" {
			return state.Revision, ""
		}
		labels[utils.ObjectRevision(pod)] = true
	}
	if len(labels) == 1 {
		for revision := range labels {
			return revision, ""
		}
	}
	quoted := make([]string, 0, len(labels))
	for label := range labels {
		quoted = append(quoted, strconv.Quote(label))
	}
	sort.Strings(quoted)
	return "", fmt.Sprintf("the Pods of the group carry the revision labels %s, and no in-place update is in progress that would say which one the group is at", strings.Join(quoted, " and "))
}

// settle folds the comparison of one Pod into the comparison of its unit. A
// unit is at the target only when every Pod of it is; one with a Pod that
// cannot be compared, and none that differs, cannot be compared.
func settle(current *workload.Comparison, next workload.Comparison) {
	if next == workload.Different || (next == workload.Unknown && *current == workload.Equivalent) {
		*current = next
	}
}
