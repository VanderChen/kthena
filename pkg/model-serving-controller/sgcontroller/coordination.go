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

package sgcontroller

import (
	"cmp"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// podGroupView returns the ModelServing the PodGroup of a group whose Roles
// are rolled is built from. view renders the revision of the group; it is
// changed and returned.
//
// One subgroup of a PodGroup cannot express different worker counts within a
// Role. While instances of several revisions exist, the PodGroup takes the
// smallest layout an instance of the Role runs, so that a replacement within
// the budget can be scheduled: a gang that asks every instance for the layout
// of the target is never complete while an instance of the earlier layout
// remains, and the Pods of the replacement would never be bound. Whether an
// instance is ready is still judged by the full layout of its own revision.
//
// A Role that waits for the Roles it depends on creates no instance of the
// target. While it waits, the PodGroup keeps the minimum it asks for that Role
// now: the gang must not wait for instances that are not being created, or
// the Roles it waits for could not be scheduled either. Only a minimum that
// follows the replica count is held back; one set in gangPolicy is not.
func (c *Controller) podGroupView(r *round, view *workloadv1alpha1.ModelServing, owner metav1.OwnerReference) (*workloadv1alpha1.ModelServing, error) {
	strategy := r.group.Spec.RolloutStrategy
	if !roleMode(r.group) {
		return view, nil
	}
	// The revision each instance runs, or is created from when it does not
	// exist yet.
	type place struct {
		servinggroup.InstanceKey
		revision string
	}
	var places []place
	seen := map[servinggroup.InstanceKey]bool{}
	for _, inst := range r.instances {
		seen[inst.Key()] = true
		if inst.Phase() != workloadv1alpha1.RoleInstanceDraining && len(inst.Members) > 0 {
			places = append(places, place{inst.Key(), utils.ObjectRevision(inst.Members[0].Pod)})
		}
	}
	for key, want := range r.plan.Wants {
		if !seen[key] {
			places = append(places, place{key, want.Revision})
		}
	}
	slices.SortFunc(places, func(a, b place) int { return cmp.Compare(a.Index, b.Index) })
	for i := range view.Spec.Template.Roles {
		desired := view.Spec.Template.Roles[i]
		found := false
		for _, at := range places {
			if at.Role != desired.Name {
				continue
			}
			applied, err := r.history.Role(at.revision, desired.Name)
			if err != nil {
				return nil, fmt.Errorf("the layout of Role instance %s-%d: %w", at.Role, at.Index, err)
			}
			if !found || applied.WorkerReplicas < view.Spec.Template.Roles[i].WorkerReplicas {
				applied.Replicas = desired.Replicas
				view.Spec.Template.Roles[i] = *applied
			}
			found = true
		}
	}

	coordination := r.plan.Coordination
	if strategy.RoleCoordination == nil || coordination == nil || len(strategy.RoleCoordination.Dependencies) == 0 || view.Spec.SchedulerName != "volcano" {
		return view, nil
	}
	published := c.podGroups.PublishedMinRoleReplicas(view, owner, r.group.Name)
	for _, role := range view.Spec.Template.Roles {
		policy := view.Spec.Template.GangPolicy
		if policy != nil {
			if _, explicit := policy.MinRoleReplicas[role.Name]; explicit {
				continue
			}
		}
		minimum, known := published[role.Name]
		if !known || minimum < 0 || minimum >= workload.RoleReplicas(role) || !coordination.Deferred.Has(role.Name) {
			continue
		}
		if policy == nil {
			policy = &workloadv1alpha1.GangPolicy{}
			view.Spec.Template.GangPolicy = policy
		}
		if policy.MinRoleReplicas == nil {
			policy.MinRoleReplicas = map[string]int32{}
		}
		policy.MinRoleReplicas[role.Name] = minimum
		klog.V(4).InfoS("The gang keeps its minimum for a Role until the Roles it depends on are ready",
			"servingGroup", klog.KObj(r.group), "role", role.Name, "minimum", minimum)
	}
	return view, nil
}
