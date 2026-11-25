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

package mscontroller

import (
	"k8s.io/apimachinery/pkg/labels"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// statusInput is what the status of a ModelServing is computed from.
type statusInput struct {
	ms   *workloadv1alpha1.ModelServing
	view *view
	// baseline is the baseline revision after this round: the target when the
	// round found the rollout complete.
	baseline string
	// collisions is the collision count the target revision was recorded with.
	collisions *int32
	// adopting reports that workloads of an earlier version are still being
	// taken over. The counts are then kept as they were written before: the
	// groups that were just created have not reported yet, and counting them
	// would make the available replicas drop to zero and come back.
	adopting bool
	// handedDown reports whether the spec of a group is the one the round
	// hands down. Nil means that every spec is.
	handedDown func(*workloadv1alpha1.ServingGroup) bool
}

// computeStatus returns the status of the ModelServing, summed up from its
// groups. The counts and the three conditions Available, Progressing and
// UpdateInProgress mean what they meant before groups were objects.
//
//   - replicas: every group object, terminating and failed ones included.
//   - updatedReplicas: groups of the target revision that are not terminating
//     and do not have to be recreated.
//   - currentReplicas: groups of the baseline revision.
//   - availableReplicas: groups that are neither terminating nor failed and
//     whose Ready condition is true as it was last written. Whether that status
//     is observed and fresh is not asked: the number users see must not flicker
//     each time a spec is handed down.
func computeStatus(in statusInput) workloadv1alpha1.ModelServingStatus {
	ms, v := in.ms, in.view
	status := *ms.Status.DeepCopy()
	target := v.spec.Target
	replicas := v.spec.Replicas

	available, updated, current := 0, 0, 0
	progressing, updatedGroups, currentGroups := []int{}, []int{}, []int{}
	pendingSurge, outdated := false, false
	for _, g := range v.groups {
		unit := g.unit
		atTarget := g.comparison == workload.Equivalent && !unit.RecreateRequired
		if in.baseline == target && g.comparison == workload.Equivalent || in.baseline != target && g.revision == in.baseline {
			current++
		}
		if unit.Terminating {
			progressing = append(progressing, g.ordinal)
			continue
		}
		if v.spec.Mode == grouplevel.ModeServingGroup && unit.Surge {
			pendingSurge = true
		}
		if !unit.Failed && unit.Ready {
			available++
		} else {
			progressing = append(progressing, g.ordinal)
		}
		if atTarget {
			updated++
			updatedGroups = append(updatedGroups, g.ordinal)
			continue
		}
		currentGroups = append(currentGroups, g.ordinal)
		// A group below the partition stays at the baseline on purpose. Under
		// RoleRollingUpdate every group that is not at the target is being rolled,
		// also while its Roles stop at their partitions.
		if v.spec.Mode == grouplevel.ModeRole || g.ordinal >= v.spec.Partition || unit.RecreateRequired {
			outdated = true
		}
	}
	rolloutActive := pendingSurge || outdated
	progressActive := pendingSurge || len(progressing) > 0 || len(v.groups) != replicas || available != replicas

	// Groups that were just created for workloads of an earlier version have
	// not reported yet. What that version wrote stays until they have: the
	// counts, the conditions, and the generation they were written for. A
	// ModelServing whose Pods all serve is not reported as unavailable for the
	// time it takes to take them over, and a status that speaks about an
	// earlier generation does not claim to speak about this one.
	if !in.adopting {
		copied := ms.DeepCopy()
		copied.Status = status
		utils.SetConditionWithRolloutAndProgressState(copied, progressing, updatedGroups, currentGroups, rolloutActive, progressActive)
		status = copied.Status
		setCoordinationCondition(&status, ms, v, in.handedDown)

		status.Replicas = int32(len(v.groups))
		status.AvailableReplicas = int32(available)
		status.UpdatedReplicas = int32(updated)
		status.CurrentReplicas = int32(current)
		strategy := ms.Spec.RolloutStrategy
		if strategy == nil || strategy.RollingUpdateConfiguration == nil || strategy.RollingUpdateConfiguration.Partition == nil {
			// Without a partition, a rollout that is over leaves no group behind.
			if updated == replicas && available == replicas && len(v.groups) == replicas {
				status.CurrentReplicas = status.UpdatedReplicas
			}
		}
	}
	status.CurrentRevision = in.baseline
	status.UpdateRevision = target
	if in.collisions != nil && *in.collisions > 0 && (status.CollisionCount == nil || *status.CollisionCount < *in.collisions) {
		count := *in.collisions
		status.CollisionCount = &count
	}
	if !in.adopting {
		status.ObservedGeneration = ms.Generation
	}
	status.LabelSelector = scaleSelector(ms)
	return status
}

// scaleSelector is the selector the scale subresource reports. spec.replicas
// counts groups, not Pods, so the selector matches exactly one Pod per group:
// the entry Pod of the first instance of the first Role.
func scaleSelector(ms *workloadv1alpha1.ModelServing) string {
	selector := labels.Set{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.EntryLabelKey:            utils.Entry,
	}
	if len(ms.Spec.Template.Roles) > 0 {
		role := ms.Spec.Template.Roles[0].Name
		selector[workloadv1alpha1.RoleLabelKey] = role
		selector[workloadv1alpha1.RoleIDKey] = utils.GenerateRoleID(role, 0)
	}
	return selector.String()
}
