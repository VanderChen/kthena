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
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Revisions resolves the Role templates of the revisions a group refers to.
// workload.History implements it.
type Revisions interface {
	Role(revision, roleName string) (*workloadv1alpha1.Role, error)
	CompareRole(revision, target, roleName string) workload.Comparison
}

// RolePlan is what one round wants of the Role instances of a group.
type RolePlan struct {
	// Wants holds the instances that are to exist, the ones to create included.
	// An observed instance that is not in it is drained.
	Wants map[InstanceKey]*Want
	// Promote lists the instances that lose the Role surge mark: temporary
	// capacity that takes a regular place.
	Promote []InstanceKey
	// InPlace maps an instance to the revision it is updated to in place.
	InPlace map[InstanceKey]string
	// Waiting says, per Role, why a place is not filled or an instance is not
	// replaced. It is reported, not acted on.
	Waiting []string
}

// PlanRoles decides which Role instances of a group are to exist, from which
// revision each is created, and which of them a Role rolling update replaces.
//
// The Role level follows the rules of the group level, with the instances of
// one Role in one group in the place of the groups of a ModelServing: scaling
// in and out, filling holes, the rollout budget, partition, surge and its
// reclaim are the same plan. Under RoleRollingUpdate an instance is replaced
// when the template of its Role differs from the target revision. Under
// ServingGroupRollingUpdate instances are not compared: the group is replaced
// as a whole by the ModelServing controller, and the Role level only scales
// and fills.
func PlanRoles(group *workloadv1alpha1.ServingGroup, instances []Instance, revisions Revisions) (RolePlan, error) {
	plan := RolePlan{Wants: map[InstanceKey]*Want{}, InPlace: map[InstanceKey]string{}}
	byRole := map[string][]Instance{}
	for _, inst := range instances {
		if inst.Entry != nil || len(inst.Members) > 0 {
			byRole[inst.Role] = append(byRole[inst.Role], inst)
		}
	}
	strategy := group.Spec.RolloutStrategy
	roleMode := strategy != nil && strategy.Type == workloadv1alpha1.RoleRollingUpdate

	for _, role := range group.Spec.Roles {
		spec, err := roleSpec(group, role, roleMode)
		if err != nil {
			return RolePlan{}, err
		}
		p := rolePlanner{role: role.Name, spec: spec, revisions: revisions, plan: &plan}
		p.run(byRole[role.Name])
	}
	return plan, nil
}

// roleSpec resolves the settings of one Role of a group into the input of the plan.
func roleSpec(group *workloadv1alpha1.ServingGroup, role workloadv1alpha1.ServingGroupRole, roleMode bool) (grouplevel.Spec, error) {
	replicas := int(role.Replicas)
	spec := grouplevel.Spec{
		Replicas: replicas,
		// Without a rolling update of its own, a Role is not limited.
		MaxUnavailable: replicas,
		Target:         group.Spec.Revision,
		Baseline:       group.Spec.BaselineRevision,
		Mode:           grouplevel.ModeRole,
	}
	if !roleMode {
		return spec, nil
	}
	spec.Mode = grouplevel.ModeServingGroup
	spec.Target = group.Spec.TargetRevision
	scaled := func(name string, value *intstr.IntOrString, roundUp bool, into *int) error {
		if value == nil {
			return nil
		}
		resolved, err := intstr.GetScaledValueFromIntOrPercent(value, replicas, roundUp)
		if err != nil {
			return fmt.Errorf("%s of Role %s: %w", name, role.Name, err)
		}
		*into = resolved
		return nil
	}
	if err := scaled("maxUnavailable", role.MaxUnavailable, false, &spec.MaxUnavailable); err != nil {
		return spec, err
	}
	if err := scaled("maxSurge", role.MaxSurge, true, &spec.MaxSurge); err != nil {
		return spec, err
	}
	if err := scaled("partition", role.Partition, true, &spec.Partition); err != nil {
		return spec, err
	}
	return spec, nil
}

type rolePlanner struct {
	role      string
	spec      grouplevel.Spec
	revisions Revisions
	plan      *RolePlan
}

func (p *rolePlanner) run(instances []Instance) {
	units := make([]grouplevel.Group, 0, len(instances))
	settled := make([]grouplevel.Group, 0, len(instances))
	awaiting := map[int]grouplevel.Orphans{}
	own := map[int]string{}
	for _, inst := range instances {
		unit, revision, adoptable := p.unit(inst)
		units = append(units, unit)
		own[inst.Index] = revision
		if adoptable {
			awaiting[inst.Index] = grouplevel.Orphans{}
		}
		if !unit.Terminating {
			settled = append(settled, unit)
		}
	}

	removed := map[int]bool{}
	promoted := map[int]bool{}
	retargeted := map[int]string{}
	for _, action := range grouplevel.Plan(grouplevel.Input{Spec: p.spec, Groups: units, Orphans: awaiting}) {
		key := InstanceKey{Role: p.role, Index: action.Ordinal}
		switch action.Kind {
		case grouplevel.ActionCreate:
			p.want(key, action.Revision, action.Surge)
		case grouplevel.ActionDelete:
			removed[action.Ordinal] = true
		case grouplevel.ActionPromote:
			p.plan.Promote = append(p.plan.Promote, key)
			promoted[action.Ordinal] = true
		case grouplevel.ActionRetarget:
			if action.Reason == grouplevel.ReasonInPlace {
				p.plan.InPlace[key] = action.Revision
			} else {
				retargeted[action.Ordinal] = action.Revision
			}
		case grouplevel.ActionWait:
			p.plan.Waiting = append(p.plan.Waiting, fmt.Sprintf("Role %s, instance %d: %s", p.role, action.Ordinal, action.Reason))
		}
	}

	// An instance that is being drained is wanted when its place is filled
	// again once it is gone. The plan that tells is the one without it.
	refill := map[int]grouplevel.Action{}
	for _, action := range grouplevel.Plan(grouplevel.Input{Spec: p.spec, Groups: settled, Orphans: awaiting}) {
		if action.Kind == grouplevel.ActionCreate {
			refill[action.Ordinal] = action
		}
	}

	for _, unit := range units {
		key := InstanceKey{Role: p.role, Index: unit.Ordinal}
		switch {
		case removed[unit.Ordinal]:
		case unit.Terminating:
			if action, ok := refill[unit.Ordinal]; ok {
				p.want(key, action.Revision, action.Surge)
			}
		case unit.Memberless:
			revision, ok := retargeted[unit.Ordinal]
			if !ok && p.spec.Mode == grouplevel.ModeRole {
				revision = p.spec.Target
			}
			p.want(key, revision, unit.Surge && !promoted[unit.Ordinal])
		default:
			p.want(key, own[unit.Ordinal], unit.Surge && !promoted[unit.Ordinal])
		}
	}
}

// want records that an instance is to exist, created from revision.
func (p *rolePlanner) want(key InstanceKey, revision string, surge bool) {
	want := &Want{Revision: revision, Surge: surge}
	if role, err := p.revisions.Role(revision, p.role); err == nil {
		want.Workers, want.LayoutKnown = role.WorkerReplicas, true
	}
	p.plan.Wants[key] = want
}

// unit describes one Role instance to the plan. revision is the revision its
// members were created from; adoptable reports that it has members the group
// still has to adopt.
func (p *rolePlanner) unit(inst Instance) (unit grouplevel.Group, revision string, adoptable bool) {
	unit = grouplevel.Group{
		Ordinal:     inst.Index,
		UID:         types.UID(utils.GenerateRoleID(inst.Role, inst.Index)),
		Terminating: inst.Phase() == workloadv1alpha1.RoleInstanceDraining,
		Observed:    true,
		Fresh:       true,
		Memberless:  true,
		PodsReady:   true,
	}
	for _, member := range inst.Members {
		if member.Membership != Owned {
			adoptable = true
			continue
		}
		unit.Memberless = false
		unit.Surge = unit.Surge || member.Pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole
		unit.PodsReady = unit.PodsReady && member.State == MemberReady
		unit.DeletionCost += deletionCost(member.Pod)

		podRevision := utils.ObjectRevision(member.Pod)
		if revision == "" {
			revision = podRevision
		}
		if p.spec.Mode != grouplevel.ModeServingGroup {
			unit.Revision = revision
			continue
		}
		// The instance is at the target only when every member is. A template
		// that renders the same Pods as the target is the target, under whatever
		// identity it was recorded.
		switch p.revisions.CompareRole(podRevision, p.spec.Target, p.role) {
		case workload.Equivalent:
			if unit.Revision == "" {
				unit.Revision = p.spec.Target
			}
		case workload.Different:
			if unit.Revision == "" || unit.Revision == p.spec.Target {
				unit.Revision = podRevision
			}
		default:
			unit.RevisionUnknown = true
			unit.Revision = podRevision
		}
	}
	unit.PodsReady = unit.PodsReady && !unit.Memberless
	if role, err := p.revisions.Role(revision, p.role); err == nil {
		unit.Ready = inst.Ready(role.WorkerReplicas)
	}
	return unit, revision, adoptable
}

func deletionCost(pod *corev1.Pod) int64 {
	cost, err := strconv.ParseInt(pod.Annotations[corev1.PodDeletionCost], 10, 64)
	if err != nil {
		return 0
	}
	return cost
}
