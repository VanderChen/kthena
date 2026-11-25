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

package servinggroup

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// DefaultProgressDeadline is how long an in-place update may take when the
// ModelServing sets no progressDeadlineSeconds.
const DefaultProgressDeadline = 1800 * time.Second

// ReasonInPlaceUpdateNotPossible is the reason of the RecreateRequired
// condition of a group whose Pods cannot be updated in place.
const ReasonInPlaceUpdateNotPossible = "InPlaceUpdateNotPossible"

// UpdatesInPlace reports whether the update method that takes effect in group
// is InPlace.
func UpdatesInPlace(group *workloadv1alpha1.ServingGroup) bool {
	strategy := group.Spec.RolloutStrategy
	return strategy != nil && strategy.UpdateMethod == workloadv1alpha1.InPlaceUpdateMethod
}

// ProgressDeadline returns how long an in-place update of group may take.
func ProgressDeadline(group *workloadv1alpha1.ServingGroup) time.Duration {
	if strategy := group.Spec.RolloutStrategy; strategy != nil && strategy.ProgressDeadlineSeconds != nil {
		return time.Duration(*strategy.ProgressDeadlineSeconds) * time.Second
	}
	return DefaultProgressDeadline
}

// updateFinished tells whether the in-place update of a Pod is done: every
// patched container runs its target image in a new instance and the Pod is
// ready. A crash on top of the restart of the update has to be recovered
// first. With recovery disabled it never is, so it must not keep a Pod that
// runs the target image from finishing.
func updateFinished(state *inplace.State, pod *corev1.Pod, recoveryEnabled bool) bool {
	if !recoveryEnabled {
		return state.OnTarget(pod)
	}
	return state.Completed(pod)
}

// InPlaceDeadlines is what the progress deadline says about the in-place
// updates of a group.
type InPlaceDeadlines struct {
	// Overdue holds the UIDs of the Pods that keep an update from finishing
	// although its deadline has passed: the Pods that were patched and do not
	// run their target images ready, and the Pods of the same unit that were
	// never patched.
	Overdue sets.Set[types.UID]
	// Next is when the next unfinished update passes its deadline; zero when
	// none is under way.
	Next time.Time
	// TimedOut is when the earliest update that passed its deadline was
	// started; zero when none has.
	TimedOut time.Time
}

// CheckInPlaceDeadlines applies the progress deadline to the in-place updates
// of the member Pods of group. The Pods that are updated together are one
// unit: the whole group when it is replaced as a whole, one Role instance
// when it is rolled Role by Role. The deadline of a unit runs from the
// earliest patch of an update that is not finished.
//
// It reads the Pods and the spec only, so a process that takes over finds the
// same deadlines.
func CheckInPlaceDeadlines(group *workloadv1alpha1.ServingGroup, members []*corev1.Pod, recoveryEnabled bool, now time.Time) InPlaceDeadlines {
	strategy := group.Spec.RolloutStrategy
	roleMode := strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate
	units := map[string][]*corev1.Pod{}
	for _, pod := range members {
		unit := ""
		if roleMode {
			unit = pod.Labels[workloadv1alpha1.RoleIDKey]
		}
		units[unit] = append(units[unit], pod)
	}

	result := InPlaceDeadlines{Overdue: sets.New[types.UID]()}
	deadline := ProgressDeadline(group)
	for _, pods := range units {
		var started time.Time
		terminating := false
		targets := sets.New[string]()
		for _, pod := range pods {
			terminating = terminating || pod.DeletionTimestamp != nil
			if state := inplace.StateOf(pod); state.InProgress() {
				targets.Insert(state.Revision)
				if started.IsZero() || state.StartedAt.Time.Before(started) {
					started = state.StartedAt.Time
				}
			}
		}
		// A unit that loses a member is recovered by the recovery policy; the
		// update is judged again once the member is gone.
		if terminating || targets.Len() == 0 {
			continue
		}
		var late []types.UID
		for _, pod := range pods {
			state := inplace.StateOf(pod)
			revision := utils.ObjectRevision(pod)
			switch {
			case state.InProgress():
				if !updateFinished(state, pod, recoveryEnabled) {
					late = append(late, pod.UID)
				}
			case !targets.Has(revision) && (roleMode || revision != group.Spec.Revision):
				late = append(late, pod.UID)
			}
		}
		if len(late) == 0 {
			continue
		}
		if expires := started.Add(deadline); expires.After(now) {
			if result.Next.IsZero() || expires.Before(result.Next) {
				result.Next = expires
			}
			continue
		}
		if result.TimedOut.IsZero() || started.Before(result.TimedOut) {
			result.TimedOut = started
		}
		result.Overdue.Insert(late...)
	}
	return result
}

// InPlaceUnit is a set of Role instances whose Pods move to a revision
// together: every instance of a group that is replaced as a whole, one
// instance of a group that is rolled Role by Role. The revision label of its
// Pods changes only when all of them run the revision, so a unit never
// reports mixed revisions as updated.
type InPlaceUnit struct {
	// Role is the Role of the instance; "" when the unit is the whole group.
	// What may differ between two revisions is then judged for every Role.
	Role string
	// Instances are the Role instances of the unit.
	Instances []Instance
	// Target is the revision the unit is updated to.
	Target string
	// Allowed reports that Pods may be patched: the update method that takes
	// effect is InPlace. An update whose Pods are all patched already is
	// brought to its end either way.
	Allowed bool
	// Settled reports that every instance of the unit is established, wanted
	// and has all the members of its layout. The labels are not changed while
	// a member may still be on its way into the cache: it would be left on the
	// old revision.
	Settled bool
	// RecoveryEnabled is false when a failed or restarting Pod is never deleted.
	RecoveryEnabled bool
}

// ImagePatch is the in-place update of one Pod: the images its containers get.
// A Pod whose containers run the target images already is patched as well, with
// no image: it takes part in the update and its labels change with the others.
type ImagePatch struct {
	Pod    *corev1.Pod
	Images map[string]string
}

// InPlaceStep is what one round does about the in-place update of a unit.
type InPlaceStep struct {
	// Revision is the revision the unit is updated to.
	Revision string
	// Active reports that the unit has an update to make or to finish. Its
	// instances are not ready until the labels of its Pods have changed.
	Active bool
	// Patch lists the Pods to patch.
	Patch []ImagePatch
	// Complete lists the Pods whose revision label changes. It is set only when
	// every Pod of the unit runs the revision and is ready.
	Complete []*corev1.Pod
	// Obstacle, when not empty, says why the unit cannot be updated in place.
	// The unit has to be recreated.
	Obstacle string
	// Fallback reports that the obstacle was found on a unit the rollout
	// chose to update in place, which is reported as a fallback to recreation.
	// An update that the spec turned away from is abandoned without that.
	Fallback bool
	// Waiting, when not empty, says what the update waits for.
	Waiting string
}

// PlanInPlace decides what to do about the in-place update of a unit. It
// depends on the Pods, their in-place state and the revisions alone, so a
// process that takes over continues where the previous one stopped: a unit
// that is patched in part is patched to the end, one whose target moved on is
// patched again, and one that is finished gets its labels.
func PlanInPlace(unit InPlaceUnit, revisions Revisions) InPlaceStep {
	step := InPlaceStep{Revision: unit.Target}
	var members, patched []Member
	adoptable, terminating := false, false
	// stale: a Pod is being updated to another revision. unpatched: a Pod of
	// another revision is not being updated. reverted: the unit went back to
	// the revision a Pod is labelled with after its images were patched away.
	stale, unpatched, reverted := false, false, false
	// Of the unpatched Pods: differs, one whose Role the target renders
	// differently; unread, one for which that cannot be told. A Pod that is
	// neither is what the target renders, under the label of another revision:
	// a rollout that replaces Role by Role leaves the Roles it does not
	// change alone, labels included.
	differs, unread := false, ""
	for _, inst := range unit.Instances {
		for _, member := range inst.Members {
			if member.Membership != Owned {
				adoptable = true
				continue
			}
			members = append(members, member)
			terminating = terminating || member.State == MemberTerminating
			state := inplace.StateOf(member.Pod)
			revision := utils.ObjectRevision(member.Pod)
			switch {
			case state.InProgress() && state.Revision == unit.Target:
				patched = append(patched, member)
			case state.InProgress():
				stale = true
				reverted = reverted || revision == unit.Target
			case revision != unit.Target:
				unpatched = true
				switch revisions.CompareRole(revision, unit.Target, member.Pod.Labels[workloadv1alpha1.RoleLabelKey]) {
				case workload.Different:
					differs = true
				case workload.Unknown:
					unread = fmt.Sprintf("revision %q of Pod %s, which cannot be resolved", revision, member.Pod.Name)
				}
			}
		}
	}
	if len(patched) == 0 && !stale && !unpatched {
		return step
	}
	step.Active = true
	switch {
	case adoptable:
		step.Waiting = "members that are not adopted yet"
		return step
	case terminating:
		step.Waiting = "a terminating member"
		return step
	case reverted:
		// By their labels the Pods are up to date, so no rollout would ever
		// patch them back.
		step.Obstacle = fmt.Sprintf("the revision went back to %s after the images of its Pods were patched for another revision", unit.Target)
		return step
	}

	if stale || unpatched {
		if !unit.Allowed {
			switch {
			case stale || differs:
				step.Obstacle = "the update method that takes effect is not InPlace"
			case unread != "":
				// A revision that cannot be read is no reason to recreate.
				step.Waiting = unread
			default:
				// Every Pod is what the target renders. Nothing is to be done
				// about them, and no label is changed without an update.
				return InPlaceStep{Revision: unit.Target}
			}
			return step
		}
		step.Patch, step.Obstacle, step.Waiting = checkInPlace(members, unit.Role, unit.Target, revisions)
		step.Fallback = step.Obstacle != ""
		return step
	}

	for _, member := range patched {
		if !updateFinished(inplace.StateOf(member.Pod), member.Pod, unit.RecoveryEnabled) {
			step.Waiting = fmt.Sprintf("Pod %s to run its target images and be ready", member.Pod.Name)
			return step
		}
	}
	if !unit.Settled {
		step.Waiting = "every member of the unit to be there"
		return step
	}
	step.Complete = pods(patched)
	return step
}

// checkInPlace tells whether members can be brought to target by patching
// images, and which Pods have to be patched with which images. role is the
// Role of the members, or "" for the members of a whole group.
//
// obstacle says why they cannot. waiting says what keeps the question open: a
// revision that cannot be read is no reason to recreate anything.
func checkInPlace(members []Member, role, target string, revisions Revisions) (patches []ImagePatch, obstacle, waiting string) {
	views := map[string]*workloadv1alpha1.ModelServing{}
	view := func(revision string) (*workloadv1alpha1.ModelServing, error) {
		if known, ok := views[revision]; ok {
			return known, nil
		}
		resolved, err := revisions.At(revision)
		if err != nil {
			return nil, err
		}
		views[revision] = resolved
		return resolved, nil
	}
	targetView, err := view(target)
	if err != nil {
		return nil, "", fmt.Sprintf("revision %s, which cannot be resolved", target)
	}
	imagesOnly := map[string]bool{}
	for _, member := range members {
		pod := member.Pod
		if reason := restartPolicyBlocksInPlace(pod); reason != "" {
			return nil, reason, ""
		}
		source := utils.ObjectRevision(pod)
		if source == target {
			continue
		}
		if missing := inplace.MissingContainers(targetView, pod); len(missing) > 0 {
			return nil, fmt.Sprintf("pod %s has no container %v of the target template", pod.Name, missing), ""
		}
		sourceView, err := view(source)
		if err != nil {
			return nil, "", fmt.Sprintf("revision %q of Pod %s, which cannot be resolved", source, pod.Name)
		}
		same, known := imagesOnly[source]
		if !known {
			if role == "" {
				only, err := inplace.OnlyImagesChanged(sourceView, targetView)
				same = err == nil && only
			} else {
				same = inplace.OnlyRoleImagesChanged(sourceView, targetView, role)
			}
			imagesOnly[source] = same
		}
		if !same {
			return nil, "changes other than container images", ""
		}
		images, drift := inplace.ImagesToPatch(pod, inplace.TargetImages(targetView, pod), inplace.TargetImages(sourceView, pod))
		if len(drift) > 0 {
			return nil, fmt.Sprintf("pod %s runs images of containers %v that match neither the old nor the new template", pod.Name, drift), ""
		}
		if state := inplace.StateOf(pod); state.InProgress() && state.Revision == target {
			continue
		}
		patches = append(patches, ImagePatch{Pod: pod, Images: images})
	}
	return patches, "", ""
}

// restartPolicyBlocksInPlace returns why the kubelet may not restart a
// container after its image changed, or "" when it always does. An empty
// restartPolicy is the API default, Always.
func restartPolicyBlocksInPlace(pod *corev1.Pod) string {
	if pod.Spec.RestartPolicy != corev1.RestartPolicyAlways && pod.Spec.RestartPolicy != "" {
		return fmt.Sprintf("pod %s restartPolicy is not Always", pod.Name)
	}
	for _, container := range pod.Spec.Containers {
		if container.RestartPolicy != nil && *container.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			return fmt.Sprintf("container %s of pod %s has restartPolicy %s", container.Name, pod.Name, *container.RestartPolicy)
		}
		if len(container.RestartPolicyRules) > 0 {
			return fmt.Sprintf("container %s of pod %s has restartPolicyRules", container.Name, pod.Name)
		}
	}
	return ""
}

// unfinishedUpdate returns the revision the members of an instance are being
// updated to in place: the one of the update that was started last, or "" when
// no member has an unfinished update.
func unfinishedUpdate(members []Member) string {
	revision, started := "", time.Time{}
	for _, member := range members {
		state := inplace.StateOf(member.Pod)
		if state.InProgress() && (revision == "" || state.StartedAt.Time.After(started)) {
			revision, started = state.Revision, state.StartedAt.Time
		}
	}
	return revision
}

// UnfinishedUpdates returns the instances that have a member whose in-place
// update is not finished: its images were patched and its revision label has
// not changed yet.
func UnfinishedUpdates(instances []Instance) map[InstanceKey]bool {
	var updating map[InstanceKey]bool
	for _, inst := range instances {
		if unfinishedUpdate(inst.Members) == "" {
			continue
		}
		if updating == nil {
			updating = map[InstanceKey]bool{}
		}
		updating[inst.Key()] = true
	}
	return updating
}

// Settled reports whether the instance is wanted, established and has exactly
// the members of its layout, all of them owned by the group and none
// terminating.
func (i Instance) Settled(want *Want) bool {
	if want == nil || !want.LayoutKnown || i.Phase() != workloadv1alpha1.RoleInstanceEstablished || len(i.Members) != int(want.Workers)+1 {
		return false
	}
	for _, member := range i.Members {
		if member.Membership != Owned {
			return false
		}
	}
	missing, terminating := missingOrdinals(i.Members, want.Workers)
	return len(missing) == 0 && !terminating
}
