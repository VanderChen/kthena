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
	"encoding/json"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// roleCoordination returns the settings of a coordinated Role rollout, or nil
// when the Roles of ms are rolled independently. They only apply under
// RoleRollingUpdate.
func roleCoordination(ms *workloadv1alpha1.ModelServing) *workloadv1alpha1.RoleCoordination {
	strategy := ms.Spec.RolloutStrategy
	if strategy == nil || strategy.Type != workloadv1alpha1.RoleRollingUpdate {
		return nil
	}
	return strategy.RoleCoordination
}

// recordBaselineReplicas keeps the replica counts of the Roles of ms on the
// ControllerRevision of its target revision, in an annotation.
//
// A coordinated rollout measures the progress of a Role against the number of
// instances it had before the rollout. The replica count is not part of a
// revision: scaling does not change the identity of a revision, and the data
// of a ControllerRevision cannot be changed. So the count is recorded next to
// the revision for as long as it is the target; once another revision is the
// target, what is recorded is the count the rollout started from. An earlier
// version of the controller reads the same annotation.
//
// It returns the ControllerRevision as it is after the write.
func (c *Controller) recordBaselineReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
	if owner := metav1.GetControllerOfNoCopy(revision); owner == nil || owner.UID != ms.UID {
		return nil, fmt.Errorf("ControllerRevision %s is not controlled by ModelServing %s with UID %s", revision.Name, ms.Name, ms.UID)
	}
	replicas := make(map[string]int32, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		replicas[role.Name] = workload.RoleReplicas(role)
	}
	baseline, err := json.Marshal(replicas)
	if err != nil {
		return nil, err
	}
	if revision.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation] == string(baseline) {
		return revision, nil
	}
	updated := revision.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation] = string(baseline)
	return c.kube.AppsV1().ControllerRevisions(ms.Namespace).Update(ctx, updated, metav1.UpdateOptions{})
}

// baselineReplicas returns the replica counts recorded with a revision, by
// Role name: what recordBaselineReplicas wrote while the revision was the
// target, or, for a revision recorded before the annotation existed, the
// counts in its data. A Role without a valid count is left out; the groups do
// not start a coordinated rollout of a Role whose count they are not told.
func baselineReplicas(ms *workloadv1alpha1.ModelServing, revision *appsv1.ControllerRevision) map[string]int32 {
	if owner := metav1.GetControllerOfNoCopy(revision); owner == nil || owner.UID != ms.UID {
		return nil
	}
	recorded := map[string]int32{}
	if raw := revision.Annotations[utils.CoordinatedRoleReplicaBaselineAnnotation]; raw != "" {
		if err := json.Unmarshal([]byte(raw), &recorded); err != nil {
			return nil
		}
	} else if revision.Annotations[utils.ControllerRevisionDataVersionAnnotation] == "" {
		roles, err := utils.GetRolesFromControllerRevision(revision)
		if err != nil {
			return nil
		}
		for _, role := range roles {
			recorded[role.Name] = workload.RoleReplicas(role)
		}
	}
	for name, replicas := range recorded {
		if replicas < 0 {
			delete(recorded, name)
		}
	}
	return recorded
}

// coordinatedReplicas records the replica counts of the target revision and
// returns the ones of the baseline revision, for a ModelServing whose Roles
// are rolled together. recorded is the ControllerRevision of the target; it
// is returned as it is after the write.
func (c *Controller) coordinatedReplicas(ctx context.Context, ms *workloadv1alpha1.ModelServing, target, baseline string, recorded *appsv1.ControllerRevision) (*appsv1.ControllerRevision, map[string]int32, error) {
	if roleCoordination(ms) == nil {
		return recorded, nil, nil
	}
	recorded, err := c.recordBaselineReplicas(ctx, ms, recorded)
	if err != nil {
		return nil, nil, fmt.Errorf("record the replica counts of the Roles with revision %s: %w", target, err)
	}
	switch baseline {
	case "":
		return recorded, nil, nil
	case target:
		return recorded, baselineReplicas(ms, recorded), nil
	}
	revision, err := c.caches.ControllerRevision(ms.Namespace, utils.GenerateControllerRevisionName(ms.Name, baseline))
	if err != nil {
		// The groups wait until the count can be read.
		return recorded, nil, nil
	}
	return recorded, baselineReplicas(ms, revision), nil
}

// setCoordinationCondition sums up the CoordinatedRoleRolloutBlocked condition
// of the groups into the one of the ModelServing. When a group is blocked, the
// ModelServing reports the reason and the message of the blocked group with
// the lowest ordinal. Otherwise it reports that the rollout can make progress
// while a group says so, and that it is complete when none does.
//
// The admission webhook reads the condition to tell whether a rollout is under
// way, so it must not say "complete" too early. A group whose spec is not the
// one handed down, or that has not reported on its spec, has not said yet what
// it does with it: the rollout counts as in progress until it has. A group
// that never reported was just created, and nothing of it is rolled.
//
// A ModelServing whose Roles are rolled independently has no such condition;
// one that it carries from before is set to complete.
func setCoordinationCondition(status *workloadv1alpha1.ModelServingStatus, ms *workloadv1alpha1.ModelServing, v *view, handedDown func(*workloadv1alpha1.ServingGroup) bool) {
	conditionType := string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked)
	enabled := roleCoordination(ms) != nil
	if !enabled && meta.FindStatusCondition(status.Conditions, conditionType) == nil {
		return
	}
	condition := metav1.Condition{
		Type:               conditionType,
		Status:             metav1.ConditionFalse,
		Reason:             servinggroup.ReasonCoordinationRolloutComplete,
		Message:            "Coordinated Role rollout is complete",
		ObservedGeneration: ms.Generation,
	}
	for _, g := range v.groups {
		group := g.object
		if !enabled || g.unit.Terminating || group.Status.ObservedGeneration == 0 {
			continue
		}
		reported := meta.FindStatusCondition(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked))
		current := reported != nil && reported.ObservedGeneration == group.Generation && (handedDown == nil || handedDown(group))
		if current && reported.Status == metav1.ConditionTrue {
			condition.Status, condition.Reason = metav1.ConditionTrue, reported.Reason
			condition.Message = fmt.Sprintf("ServingGroup %s: %s", group.Name, reported.Message)
			// Keep a bounded diagnostic, independent of the number of dependencies.
			if text := []rune(condition.Message); len(text) > 512 {
				condition.Message = string(text[:509]) + "..."
			}
			break
		}
		if !current || reported.Reason == servinggroup.ReasonCoordinationProgressAvailable {
			condition.Reason, condition.Message = servinggroup.ReasonCoordinationProgressAvailable, "Coordinated Role rollout can make progress"
		}
	}
	meta.SetStatusCondition(&status.Conditions, condition)
}

// reportCoordination records an event when the CoordinatedRoleRolloutBlocked
// condition of the ModelServing changed with a status write.
func (c *Controller) reportCoordination(ms *workloadv1alpha1.ModelServing, status workloadv1alpha1.ModelServingStatus) {
	conditionType := string(workloadv1alpha1.ModelServingCoordinatedRoleRolloutBlocked)
	before, after := meta.FindStatusCondition(ms.Status.Conditions, conditionType), meta.FindStatusCondition(status.Conditions, conditionType)
	if after == nil || before != nil && before.Status == after.Status && before.Reason == after.Reason &&
		before.Message == after.Message && before.ObservedGeneration == after.ObservedGeneration {
		return
	}
	eventType := corev1.EventTypeNormal
	if after.Status == metav1.ConditionTrue {
		eventType = corev1.EventTypeWarning
	}
	c.recorder.Event(ms, eventType, after.Reason, after.Message)
}
