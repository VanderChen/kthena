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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// ServingGroupSpec is written by the ModelServing controller. It carries the
// revision of the group and the settings of the ModelServing that apply inside
// one group and are not part of a revision. Pod templates, plugins and the
// scheduler name are not copied here; they are read from the ControllerRevision
// of the revision.
//
// A revision is identified as in the status of the ModelServing and in the
// revision label of a Pod.
//
// No field has a schema default: the stored spec is exactly what the
// ModelServing controller wrote, so it can be compared with the desired spec.
type ServingGroupSpec struct {
	// Revision is the revision this group runs as a whole. It is set under
	// ServingGroupRollingUpdate and empty under RoleRollingUpdate.
	// +optional
	Revision string `json:"revision,omitempty"`

	// TargetRevision is the revision the rollout of the ModelServing moves to.
	// Under RoleRollingUpdate the Role instances of this group are rolled to it.
	// Under ServingGroupRollingUpdate a group whose revision differs from it is
	// one of an earlier revision: it starts no update of its Pods until the
	// ModelServing controller changes its revision or replaces it.
	// +optional
	TargetRevision string `json:"targetRevision,omitempty"`

	// BaselineRevision is a copy of the currentRevision of the ModelServing status.
	// Role instances protected by a Role partition are created from it. A group
	// whose annotation modelserving.volcano.sh/created-revision names the target
	// revision is the exception: it is a new group, and creates those instances
	// from the target revision like its other instances, while every Pod it
	// has is of the target revision. The ModelServing controller reads the
	// baseline back when its own status was lost.
	// +optional
	BaselineRevision string `json:"baselineRevision,omitempty"`

	// SourceGeneration is the generation of the ModelServing this spec was derived
	// from. The ModelServing controller never replaces a spec with one derived from
	// a lower generation.
	// +optional
	SourceGeneration int64 `json:"sourceGeneration,omitempty"`

	// ExpectedPods is the number of Pods of the group when it is complete. It is the
	// replica count of the scale subresource.
	// +kubebuilder:validation:Minimum=0
	ExpectedPods int32 `json:"expectedPods"`

	// RolloutStrategy carries the rollout settings of the ModelServing that apply
	// inside a group.
	// +optional
	RolloutStrategy *ServingGroupRolloutStrategy `json:"rolloutStrategy,omitempty"`

	// RecoveryPolicy is the recovery policy of the ModelServing.
	// +kubebuilder:validation:Enum={ServingGroupRecreate,RoleRecreate,None}
	// +optional
	RecoveryPolicy RecoveryPolicy `json:"recoveryPolicy,omitempty"`

	// RestartGracePeriodSeconds is the restart grace period of the ModelServing template.
	// +kubebuilder:validation:Minimum=-1
	// +optional
	RestartGracePeriodSeconds *int64 `json:"restartGracePeriodSeconds,omitempty"`

	// GangPolicy is the gang scheduling configuration of the ModelServing template.
	// +optional
	GangPolicy *ServingGroupGangPolicy `json:"gangPolicy,omitempty"`

	// NetworkTopology is the network topology configuration of the ModelServing template.
	// +optional
	NetworkTopology *NetworkTopology `json:"networkTopology,omitempty"`

	// QueueName is the scheduling queue of the PodGroup of the group.
	// +optional
	QueueName string `json:"queueName,omitempty"`

	// Roles carries, for every Role of the ModelServing template, the settings that
	// are not part of a revision.
	// +optional
	// +listType=map
	// +listMapKey=name
	Roles []ServingGroupRole `json:"roles,omitempty"`
}

// ServingGroupRolloutStrategy is the part of the ModelServing rollout strategy
// that takes effect inside one ServingGroup.
type ServingGroupRolloutStrategy struct {
	// Type is the rollout granularity of the ModelServing.
	// +kubebuilder:validation:Enum={ServingGroupRollingUpdate,RoleRollingUpdate}
	Type RolloutStrategyType `json:"type"`

	// UpdateMethod is the update method that takes effect: InPlace only when the
	// ModelServing asks for it and its rollout budget and plugins allow it.
	// +kubebuilder:validation:Enum={Recreate,InPlace}
	// +optional
	UpdateMethod UpdateMethod `json:"updateMethod,omitempty"`

	// ProgressDeadlineSeconds is the progress deadline of an in-place update.
	// +optional
	ProgressDeadlineSeconds *int32 `json:"progressDeadlineSeconds,omitempty"`

	// RoleCoordination is the cross-Role coordination configuration.
	// +optional
	RoleCoordination *RoleCoordination `json:"roleCoordination,omitempty"`
}

// ServingGroupGangPolicy is the gang scheduling configuration of a group: what
// GangPolicy is for the template of a ModelServing, without its rules about
// updates. The ModelServing controller writes it, and whatever the
// ModelServing was allowed to change has to be writable here: a rule that
// rejected the copy would leave the group with the settings of an earlier
// generation for good.
type ServingGroupGangPolicy struct {
	// MinRoleReplicas is the minimum number of replicas of each Role that gang
	// scheduling requires. See GangPolicy.
	// +optional
	MinRoleReplicas map[string]int32 `json:"minRoleReplicas,omitempty"`
}

// ServingGroupRole carries the settings of one Role that are not part of a revision.
type ServingGroupRole struct {
	// Name of the Role.
	Name string `json:"name"`

	// Replicas is the number of instances of the Role in this group.
	// +kubebuilder:validation:Minimum=0
	Replicas int32 `json:"replicas"`

	// MaxUnavailable of the Role, used under RoleRollingUpdate.
	// +kubebuilder:validation:XIntOrString
	// +optional
	MaxUnavailable *intstr.IntOrString `json:"maxUnavailable,omitempty"`

	// MaxSurge of the Role, used under RoleRollingUpdate.
	// +kubebuilder:validation:XIntOrString
	// +optional
	MaxSurge *intstr.IntOrString `json:"maxSurge,omitempty"`

	// Partition of the Role, used under RoleRollingUpdate.
	// +kubebuilder:validation:XIntOrString
	// +optional
	Partition *intstr.IntOrString `json:"partition,omitempty"`

	// BaselineReplicas is the replica count of the Role recorded with the baseline
	// revision. Only a coordinated Role rolling update reads it.
	// +kubebuilder:validation:Minimum=0
	// +optional
	BaselineReplicas *int32 `json:"baselineReplicas,omitempty"`
}

// ServingGroupConditionType is a condition type of a ServingGroup.
type ServingGroupConditionType string

const (
	// ServingGroupConditionReady is true when every Role has its declared number of
	// instances and every instance is running and ready. A group whose Roles all
	// have zero replicas is ready.
	ServingGroupConditionReady ServingGroupConditionType = "Ready"

	// ServingGroupConditionFailed is true when the group must be deleted and
	// recreated as a whole. It never returns to false.
	ServingGroupConditionFailed ServingGroupConditionType = "Failed"

	// ServingGroupConditionDrained is meaningful together with Failed. It is true
	// when no faulty member and no terminating Pod is left in the group, so the
	// remaining members may be deleted.
	ServingGroupConditionDrained ServingGroupConditionType = "Drained"

	// ServingGroupConditionRecreateRequired is true when the group can only be
	// brought to its revision by replacing it.
	ServingGroupConditionRecreateRequired ServingGroupConditionType = "RecreateRequired"

	// ServingGroupConditionDrainBlocked is true while a Role instance cannot be
	// drained because the OnRoleDelete plugin hook keeps failing.
	ServingGroupConditionDrainBlocked ServingGroupConditionType = "DrainBlocked"

	// ServingGroupConditionReconcileFailed is true when the last reconcile of the
	// group ended with an error. Ready is still reported as observed.
	ServingGroupConditionReconcileFailed ServingGroupConditionType = "ReconcileFailed"

	// ServingGroupConditionCoordinatedRoleRolloutBlocked reports, for this group,
	// what ModelServingCoordinatedRoleRolloutBlocked reports for the ModelServing.
	ServingGroupConditionCoordinatedRoleRolloutBlocked ServingGroupConditionType = "CoordinatedRoleRolloutBlocked"
)

// RoleInstancePhase is the phase of a Role instance recorded in the ledger.
type RoleInstancePhase string

const (
	// RoleInstanceCreating is the zero value: the instance is being created. A
	// member that is missing is created, and nothing else is deleted.
	RoleInstanceCreating RoleInstancePhase = "Creating"
	// RoleInstanceEstablished: all members of the instance were observed. A member
	// that is missing or terminating was lost and is recovered by the recovery policy.
	RoleInstanceEstablished RoleInstancePhase = "Established"
	// RoleInstanceDraining: every Pod of the instance whose generation is not
	// greater than the recorded one is being deleted.
	RoleInstanceDraining RoleInstancePhase = "Draining"
)

// RoleInstanceStatus is the ledger entry of one Role instance. Unlike the rest
// of the status it cannot be rebuilt from the Pods: it records what the
// controller has committed to.
type RoleInstanceStatus struct {
	// Role is the name of the Role.
	Role string `json:"role"`

	// Index is the ordinal of the instance within its Role.
	// +kubebuilder:validation:Minimum=0
	Index int32 `json:"index"`

	// Phase is Creating, Established or Draining. An absent phase means Creating.
	// +kubebuilder:validation:Enum={Creating,Established,Draining}
	// +optional
	Phase RoleInstancePhase `json:"phase,omitempty"`

	// Generation numbers the successive sets of Pods of the instance. A Pod carries
	// the generation it was created for in the instance-generation label.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Generation int64 `json:"generation,omitempty"`
}

// ServingGroupStatus is written by the ServingGroup controller.
type ServingGroupStatus struct {
	// ObservedGeneration is the generation of the spec the decisions behind this
	// status were made from. While it is lower than metadata.generation the
	// ModelServing controller does not rely on the conditions.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedPods is a digest of the member Pods this status was computed from.
	// The ModelServing controller computes the same digest from its own view of the
	// Pods; while the two differ it does not rely on the conditions.
	// +optional
	ObservedPods string `json:"observedPods,omitempty"`

	// Conditions of the group. See ServingGroupConditionType.
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// Revision is the revision that every Pod of the group runs, read from the
	// Pods and not from the spec. A Pod runs the revision of its label, and
	// every revision in which its Role renders the same Pods: a rollout that
	// replaces Role by Role does not touch the Pods of a Role it does not
	// change. It is empty when the Pods run different revisions or the group
	// has no Pod.
	// +optional
	Revision string `json:"revision,omitempty"`

	// ReferencedRevisions lists every revision referenced by a Pod of the group,
	// including terminating Pods. ControllerRevisions listed here are retained.
	// +optional
	// +listType=set
	ReferencedRevisions []string `json:"referencedRevisions,omitempty"`

	// Pods is the number of Pods of the group, including terminating Pods and Pods
	// the controller has decided to create but not created yet. A group with zero
	// Pods here and no member Pod has no member: its revision can still be changed
	// without replacing anything.
	Pods int32 `json:"pods"`

	// ReadyPods is the number of member Pods that are running and ready. For display.
	ReadyPods int32 `json:"readyPods"`

	// FaultyMembers is the number of member Pods that are in a terminal phase or in
	// a restart failure that counts as a fault. For display.
	FaultyMembers int32 `json:"faultyMembers"`

	// TerminatingPods is the number of member Pods that are being deleted. For display.
	TerminatingPods int32 `json:"terminatingPods"`

	// LabelSelector selects the Pods of the group. It is the selector of the scale subresource.
	// +optional
	LabelSelector string `json:"labelSelector,omitempty"`

	// RoleInstances is the ledger: one entry for every Role instance the controller
	// has started to create and not finished removing.
	// +optional
	// +listType=map
	// +listMapKey=role
	// +listMapKey=index
	RoleInstances []RoleInstanceStatus `json:"roleInstances,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.expectedPods,statuspath=.status.pods,selectorpath=.status.labelSelector
// +kubebuilder:resource:shortName=sg
// +kubebuilder:storageversion
// +genclient
// +kubebuilder:printcolumn:name="Revision",type="string",JSONPath=".status.revision",description="Revision that every Pod of the group runs"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status",description="Whether every Role instance of the group is ready"
// +kubebuilder:printcolumn:name="Pods",type="integer",JSONPath=".status.readyPods",description="Number of ready Pods"
// +kubebuilder:printcolumn:name="Expected",type="integer",JSONPath=".spec.expectedPods",description="Number of Pods of a complete group"
// +kubebuilder:printcolumn:name="Failed",type="string",JSONPath=".status.conditions[?(@.type=='Failed')].status",priority=1,description="Whether the group is to be replaced as a whole"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// ServingGroup is one replica of a ModelServing. It is named
// <ModelServing name>-<ordinal>, is owned by the ModelServing and owns the Pods,
// the PodGroup and the headless Services of the group. ServingGroups are created,
// updated and deleted by the ModelServing controller; a manual change of the spec
// is overwritten.
type ServingGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServingGroupSpec   `json:"spec,omitempty"`
	Status            ServingGroupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ServingGroupList contains a list of ServingGroup
type ServingGroupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ServingGroup `json:"items"`
}
