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
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// recorded is the target revision that was resolved for one generation of a
// ModelServing. It is a cache: losing it costs one list of ControllerRevisions.
type recorded struct {
	generation int64
	revision   string
	collisions *int32
}

// targetRevision returns the identity of the revision of the template of ms,
// recording it as a ControllerRevision when no revision of ms renders the
// same Pods yet. A revision that does is reused under the identity it has: a
// template that is restored gets its old identity back, and so does one that
// an earlier version of the controller recorded in another format.
//
// The ControllerRevision is returned so that the round can read it before the
// cache shows it.
func (c *Controller) targetRevision(ctx context.Context, ms *workloadv1alpha1.ModelServing) (string, *int32, *appsv1.ControllerRevision, error) {
	c.mu.Lock()
	known, ok := c.targets[ms.UID]
	c.mu.Unlock()
	if ok && known.generation == ms.Generation {
		name := utils.GenerateControllerRevisionName(ms.Name, known.revision)
		if cr, err := c.caches.ControllerRevision(ms.Namespace, name); err == nil {
			return known.revision, known.collisions, cr, nil
		}
	}

	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		return "", nil, nil, fmt.Errorf("build the revision of the template: %w", err)
	}
	cr, collisions, err := utils.RecordModelServingRevision(ctx, c.kube, ms, data)
	if err != nil {
		return "", nil, nil, fmt.Errorf("record the revision of the template: %w", err)
	}
	revision := cr.Labels[utils.ControllerRevisionRevisionLabelKey]
	if revision == "" {
		return "", nil, nil, fmt.Errorf("ControllerRevision %s has no revision identity", cr.Name)
	}
	c.mu.Lock()
	c.targets[ms.UID] = recorded{generation: ms.Generation, revision: revision, collisions: collisions}
	c.mu.Unlock()
	return revision, collisions, cr, nil
}

// forget drops what the process remembers about a ModelServing that is gone.
func (c *Controller) forget(uid types.UID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.targets, uid)
	delete(c.unresolved, uid)
	for group := range c.unsettledOf[uid] {
		delete(c.unsettled, group)
	}
	delete(c.unsettledOf, uid)
}

// legacyBaseline returns the baseline of a ModelServing whose status has none
// and that has no group object: the revision most of the groups that exist as
// Pods without a group object run, the target left aside, or the target when
// there is no such group. That is what an earlier version would have written
// into the status with its next round; it is only missing when that version
// was stopped before it got there.
//
// Only Pods that would be taken over count, and only a revision of this
// ModelServing that can be read: Pods that a ModelServing of the same name
// left behind, which was deleted since, run revisions this one never had. A
// baseline taken from them could not be created from, and nothing below a
// partition would ever become ready.
func (c *Controller) legacyBaseline(ms *workloadv1alpha1.ModelServing, target string) string {
	parent := servinggroup.ParentOf(ms)
	history := workload.NewHistory(ms, c.caches.ControllerRevision)
	revisions := map[string]map[string]bool{}
	for _, pod := range c.caches.UnadoptedPodsOf(ms.Namespace, ms.Name) {
		group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if !servinggroup.MembershipOf(pod, group, "", parent).Adoptable() {
			continue
		}
		if revisions[group] == nil {
			revisions[group] = map[string]bool{}
		}
		revisions[group][utils.ObjectRevision(pod)] = true
	}
	count := map[string]int{}
	for _, of := range revisions {
		if len(of) != 1 {
			continue
		}
		for revision := range of {
			if revision == "" || revision == target {
				continue
			}
			if _, err := history.At(revision); err == nil {
				count[revision]++
			}
		}
	}
	baseline := target
	for revision, groups := range count {
		if baseline == target || groups > count[baseline] || groups == count[baseline] && revision < baseline {
			baseline = revision
		}
	}
	return baseline
}

// baselineRevision returns the baseline revision B: the revision the groups
// were at before the rollout that is under way, from which a group below the
// partition is created.
//
// It is status.currentRevision. When the status does not have it, it is read
// back from the copy every group carries in its spec; a ModelServing without
// a group starts at the target. "" means that it cannot be told.
func baselineRevision(ms *workloadv1alpha1.ModelServing, groups []*workloadv1alpha1.ServingGroup, target string) string {
	if ms.Status.CurrentRevision != "" {
		return ms.Status.CurrentRevision
	}
	if len(groups) == 0 {
		return target
	}
	baseline, generation := "", int64(-1)
	for _, group := range groups {
		if group.Spec.BaselineRevision != "" && group.Spec.SourceGeneration > generation {
			baseline, generation = group.Spec.BaselineRevision, group.Spec.SourceGeneration
		}
	}
	return baseline
}
