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

package workload

import (
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Settings is what the spec of a ServingGroup carries besides the settings
// copied from the ModelServing.
type Settings struct {
	// Revision, TargetRevision and BaselineRevision as in ServingGroupSpec.
	Revision, TargetRevision, BaselineRevision string
	// UpdateMethod is the update method that takes effect.
	UpdateMethod workloadv1alpha1.UpdateMethod
	// ExpectedPods is the number of Pods of the complete group.
	ExpectedPods int32
	// BaselineReplicas holds, by Role name, the replica count recorded with the
	// baseline revision. Only a coordinated Role rolling update needs it.
	BaselineReplicas map[string]int32
}

// GroupSpec returns the spec of a ServingGroup of ms: the settings of ms that
// apply inside one group and are not part of a revision, and settings.
func GroupSpec(ms *workloadv1alpha1.ModelServing, settings Settings) workloadv1alpha1.ServingGroupSpec {
	spec := workloadv1alpha1.ServingGroupSpec{
		Revision:                  settings.Revision,
		TargetRevision:            settings.TargetRevision,
		BaselineRevision:          settings.BaselineRevision,
		SourceGeneration:          ms.Generation,
		ExpectedPods:              settings.ExpectedPods,
		RecoveryPolicy:            ms.Spec.RecoveryPolicy,
		RestartGracePeriodSeconds: copyOf(ms.Spec.Template.RestartGracePeriodSeconds),
		GangPolicy:                ms.Spec.Template.GangPolicy.DeepCopy(),
		NetworkTopology:           ms.Spec.Template.NetworkTopology.DeepCopy(),
		QueueName:                 ms.Annotations[schedulingv1beta1.QueueNameAnnotationKey],
	}
	if strategy := ms.Spec.RolloutStrategy; strategy != nil {
		spec.RolloutStrategy = &workloadv1alpha1.ServingGroupRolloutStrategy{
			Type:                    strategy.Type,
			UpdateMethod:            settings.UpdateMethod,
			ProgressDeadlineSeconds: copyOf(strategy.ProgressDeadlineSeconds),
			RoleCoordination:        strategy.RoleCoordination.DeepCopy(),
		}
	}
	for _, role := range ms.Spec.Template.Roles {
		groupRole := workloadv1alpha1.ServingGroupRole{
			Name:           role.Name,
			Replicas:       RoleReplicas(role),
			MaxUnavailable: copyOf(role.MaxUnavailable),
			MaxSurge:       copyOf(role.MaxSurge),
			Partition:      copyOf(role.Partition),
		}
		if replicas, ok := settings.BaselineReplicas[role.Name]; ok {
			groupRole.BaselineReplicas = &replicas
		}
		spec.Roles = append(spec.Roles, groupRole)
	}
	return spec
}

// Base returns the ModelServing a group is rendered from, before a revision
// is applied to it: the identity of parent with the settings of the group.
// History.At applies a revision. Decisions about a group are made on this
// view, so they depend on the spec of the group and on the ControllerRevision
// and not on a newer spec of parent.
//
// Three things are still taken from parent: its identity; the scheduler name
// and plugins of a revision recorded before these became part of a revision;
// and its metadata, for plugins that read it.
func Base(parent *workloadv1alpha1.ModelServing, group *workloadv1alpha1.ServingGroup) *workloadv1alpha1.ModelServing {
	base := parent.DeepCopy()
	spec := &base.Spec
	spec.RecoveryPolicy = group.Spec.RecoveryPolicy
	spec.Template.RestartGracePeriodSeconds = copyOf(group.Spec.RestartGracePeriodSeconds)
	spec.Template.GangPolicy = group.Spec.GangPolicy.DeepCopy()
	spec.Template.NetworkTopology = group.Spec.NetworkTopology.DeepCopy()

	spec.RolloutStrategy = nil
	if strategy := group.Spec.RolloutStrategy; strategy != nil {
		spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
			Type:                    strategy.Type,
			UpdateMethod:            strategy.UpdateMethod,
			ProgressDeadlineSeconds: copyOf(strategy.ProgressDeadlineSeconds),
			RoleCoordination:        strategy.RoleCoordination.DeepCopy(),
		}
	}

	spec.Template.Roles = nil
	for _, role := range group.Spec.Roles {
		replicas := role.Replicas
		spec.Template.Roles = append(spec.Template.Roles, workloadv1alpha1.Role{
			Name:     role.Name,
			Replicas: &replicas,
			RollingUpdateConfiguration: workloadv1alpha1.RollingUpdateConfiguration{
				MaxUnavailable: copyOf(role.MaxUnavailable),
				MaxSurge:       copyOf(role.MaxSurge),
				Partition:      copyOf(role.Partition),
			},
		})
	}

	if group.Spec.QueueName != "" {
		if base.Annotations == nil {
			base.Annotations = map[string]string{}
		}
		base.Annotations[schedulingv1beta1.QueueNameAnnotationKey] = group.Spec.QueueName
	} else {
		delete(base.Annotations, schedulingv1beta1.QueueNameAnnotationKey)
	}
	// The status of parent can be ahead of the spec of the group. What the group
	// was told replaces it.
	base.Status = workloadv1alpha1.ModelServingStatus{
		CurrentRevision: group.Spec.BaselineRevision,
		UpdateRevision:  group.Spec.TargetRevision,
	}
	if group.Spec.Revision != "" {
		base.Status.UpdateRevision = group.Spec.Revision
	}
	return base
}

// RoleReplicas returns the number of instances of a Role in one group.
func RoleReplicas(role workloadv1alpha1.Role) int32 {
	if role.Replicas == nil {
		return 1
	}
	return *role.Replicas
}

// ExpectedPods returns the number of Pods of one complete group of ms.
func ExpectedPods(ms *workloadv1alpha1.ModelServing) int32 {
	pods := int32(0)
	for _, role := range ms.Spec.Template.Roles {
		pods += RoleReplicas(role) * (1 + role.WorkerReplicas)
	}
	return pods
}

func copyOf[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copied := *value
	return &copied
}
