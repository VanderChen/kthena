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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// referencedRevisions returns the revisions the ModelServing and its groups
// refer to, and whether that set can be relied on: every group has reported
// on its current spec and on the Pods the cache shows, so that the revisions
// of its Pods are the ones its status lists.
func referencedRevisions(r *round, status workloadv1alpha1.ModelServingStatus) (sets.Set[string], bool) {
	referenced := sets.New(r.target, r.baseline, status.CurrentRevision, status.UpdateRevision)
	for _, g := range r.view.groups {
		if g.unit.Terminating || !g.unit.Observed || !g.unit.Fresh {
			return nil, false
		}
		spec := g.object.Spec
		referenced.Insert(spec.Revision, spec.TargetRevision, spec.BaselineRevision)
		referenced.Insert(g.object.Status.ReferencedRevisions...)
	}
	referenced.Delete("")
	return referenced, true
}

// cleanupRevisions deletes the ControllerRevisions of the ModelServing that
// nothing refers to any more. It does so only while the ModelServing is at
// rest: available, without a partition that could ask for the baseline of an
// ordinal that is empty now, and with nothing left to take over or to recover.
// It reads the cache and writes only when there is something to delete.
func (c *Controller) cleanupRevisions(ctx context.Context, r *round, status workloadv1alpha1.ModelServingStatus) {
	ms := r.ms
	if r.gated || len(r.view.input.Orphans) > 0 || r.view.spec.Partition > 0 || r.view.spec.RolePartitioned ||
		!meta.IsStatusConditionTrue(status.Conditions, string(workloadv1alpha1.ModelServingAvailable)) {
		return
	}
	for _, action := range r.actions {
		if action.Kind != grouplevel.ActionAdvanceBaseline {
			// Something is still being done, or waited for.
			return
		}
	}
	referenced, ok := referencedRevisions(r, status)
	if !ok {
		return
	}
	for _, revision := range c.caches.RevisionsLabelled(ms.Namespace, ms.Name) {
		owner := metav1.GetControllerOfNoCopy(revision)
		identity := revision.Labels[utils.ControllerRevisionRevisionLabelKey]
		if owner == nil || owner.UID != ms.UID || revision.DeletionTimestamp != nil || referenced.Has(identity) {
			continue
		}
		// Only what this controller names is deleted: a revision whose name does
		// not follow from its identity was not recorded by it.
		if identity == "" || revision.Name != utils.GenerateControllerRevisionName(ms.Name, identity) {
			continue
		}
		err := c.kube.AppsV1().ControllerRevisions(revision.Namespace).Delete(ctx, revision.Name, metav1.DeleteOptions{
			Preconditions: &metav1.Preconditions{UID: &revision.UID},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			// History that stays is harmless; the next round at rest tries again.
			klog.ErrorS(err, "Cannot delete a ControllerRevision that is not referenced any more", "controllerRevision", klog.KObj(revision))
			continue
		}
		klog.V(2).InfoS("Deleted a ControllerRevision that is not referenced any more", "controllerRevision", klog.KObj(revision), "modelServing", fmt.Sprintf("%s/%s", ms.Namespace, ms.Name))
	}
}
