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
	"cmp"
	"fmt"
	"slices"

	corev1 "k8s.io/api/core/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// groupCount compares the number of groups with spec.replicas.
//
// After the takeover a group is temporary only when its object carries the
// surge annotation. Every other group is a regular one, and more regular
// groups than replicas is a scale-in, whatever the extra group was created
// for: the new controller deletes one group per surplus, and it prefers a
// group whose Pods are not all ready.
func (a *analysis) groupCount() {
	var ordinals []int
	for _, g := range a.regular {
		ordinals = append(ordinals, g.ordinal)
	}
	switch surplus := len(a.regular) - a.replicas; {
	case surplus > 0:
		candidates := slices.Clone(a.regular)
		slices.SortStableFunc(candidates, scaleInOrder)
		var names, described []string
		for _, g := range candidates[:surplus] {
			state := "all Pods ready"
			if !g.ready {
				state = "not ready"
			}
			names = append(names, g.name)
			described = append(described, fmt.Sprintf("%s (%s, deletion cost %d)", g.name, state, g.cost))
		}
		a.add(Finding{ID: FindingGroupsExceedReplicas, Level: LevelWait, changesPods: true, Objects: names,
			Message: fmt.Sprintf("%d groups without the surge mark exist (ordinals %s) and spec.replicas is %d. After the upgrade all of them are regular groups, and %s scaled in. The new controller chooses when it acts: a group whose Pods are not all ready first, then the lowest Pod deletion cost, then the highest ordinal. By the state of the Pods now it deletes %s.",
				len(a.regular), ordinalList(ordinals), a.replicas, counted(surplus, "1 group is", "%d groups are"), listed(described)),
			Action: "Let the running version finish the rollout or the scale-in before upgrading; the finding is gone when no more groups than spec.replicas remain."})
	case surplus < 0 && workload.ExpectedPods(a.in.ms) > 0:
		a.add(Finding{ID: FindingGroupsBelowReplicas, Level: LevelWait,
			Message: fmt.Sprintf("%d of the %d groups that spec.replicas asks for exist (ordinals %s). A scale-out or a recovery is in progress, or no controller is running. The new version creates the missing groups after the upgrade.",
				len(a.regular), a.replicas, ordinalList(ordinals)),
			Action: "Wait until every group exists."})
	}
	if holes := a.report.Rollout.Holes; len(holes) > 0 {
		a.add(Finding{ID: FindingOrdinalHoles, Level: LevelInfo,
			Message: fmt.Sprintf("The ordinals of the groups are not contiguous: no group at %s. A group is taken over under the ordinal in its name, and a hole below spec.replicas is filled when a group is created.",
				ordinalList(holes))})
	}
}

// scaleInOrder is the order in which the new controller picks a group to
// scale in: Pods not all ready, then the lower deletion cost, then the
// higher ordinal.
func scaleInOrder(a, b *group) int {
	rank := func(g *group) int {
		if g.ready {
			return 1
		}
		return 0
	}
	return cmp.Or(cmp.Compare(rank(a), rank(b)), cmp.Compare(a.cost, b.cost), cmp.Compare(b.ordinal, a.ordinal))
}

// surgeMarks reports where the surge marks are and what the takeover makes
// of them.
func (a *analysis) surgeMarks() {
	var surge, partial, below, beyond, ignored []string
	var roleMarked []*corev1.Pod
	for _, g := range a.groups {
		total := len(g.live) + len(g.terminating)
		switch {
		case len(g.live) == 0:
		case a.roleMode:
			if g.marked > 0 {
				ignored = append(ignored, g.name)
			}
		case g.surge:
			surge = append(surge, g.name)
			if g.marked < total {
				partial = append(partial, fmt.Sprintf("%s (%d of %d Pods)", g.name, g.marked, total))
			}
		case g.marked > 0:
			below = append(below, g.name)
		case g.ordinal >= a.replicas:
			beyond = append(beyond, g.name)
		}
		for _, pod := range g.live {
			if pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole {
				roleMarked = append(roleMarked, pod)
			}
		}
	}
	if len(surge) > 0 {
		a.add(Finding{ID: FindingSurgeGroups, Level: LevelWait, changesPods: true, Objects: surge,
			Message: fmt.Sprintf("Temporary capacity of a rolling update with maxSurge exists: %s (ordinal at or above spec.replicas %d, Pods carry the surge mark). The ServingGroup object created for such a group carries the surge annotation, and the group is deleted when the rollout ends.",
				listed(surge), a.replicas),
			Action: "Wait until the rollout has finished and the temporary groups are gone, or accept that the new version finishes it."})
	}
	if len(partial) > 0 {
		a.add(Finding{ID: FindingSurgeMarkPartial, Level: LevelAttention,
			Message: fmt.Sprintf("Only some Pods carry the surge mark in %s. One marked Pod is enough: the group is taken over as temporary capacity and is deleted when the rollout ends.",
				listed(partial))})
	}
	if len(below) > 0 {
		a.add(Finding{ID: FindingSurgeMarkBelow, Level: LevelInfo, Objects: below,
			Message: fmt.Sprintf("Pods carry the surge mark below spec.replicas %d, in %s. Such a group is a regular one: its object gets no surge annotation, and the mark is removed from the Pods after the takeover. No Pod is restarted.",
				a.replicas, listed(below))})
	}
	if len(beyond) > 0 {
		a.add(Finding{ID: FindingUnmarkedBeyond, Level: LevelInfo, Objects: beyond,
			Message: fmt.Sprintf("No Pod carries the surge mark at or above spec.replicas %d, in %s. Such a group is a regular one and counts towards spec.replicas.",
				a.replicas, listed(beyond))})
	}
	if len(ignored) > 0 {
		a.add(Finding{ID: FindingSurgeMarkIgnored, Level: LevelInfo, Objects: ignored,
			Message: fmt.Sprintf("Pods of %s carry the group-level surge mark, which has no meaning under RoleRollingUpdate. The groups are regular ones, and the mark is removed from the Pods after the takeover. No Pod is restarted.",
				listed(ignored))})
	}
	if len(roleMarked) == 0 {
		return
	}
	names := podNames(roleMarked)
	if a.roleMode {
		a.add(Finding{ID: FindingRoleSurgeInstances, Level: LevelWait, changesPods: true, Objects: names,
			Message: fmt.Sprintf("%s to temporary Role instances of a Role rolling update with maxSurge (%s). The new version continues the rollout and removes the temporary instances when it ends.",
				counted(len(names), "1 Pod belongs", "%d Pods belong"), listed(names)),
			Action: "Wait until the Role rollout has finished, or accept that the new version finishes it."})
		return
	}
	a.add(Finding{ID: FindingRoleSurgeInstances, Level: LevelAttention, changesPods: true, Objects: names,
		Message: fmt.Sprintf("%s the mark of a temporary Role instance (%s), left from a time when the ModelServing used RoleRollingUpdate. After the takeover the mark is removed, and Role instances beyond the replicas of their Role are scaled in.",
			counted(len(names), "1 Pod carries", "%d Pods carry"), listed(names))})
}
