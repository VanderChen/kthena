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

const (
	// ModelServingNameLabelKey is the pod label key for the model serving name.
	ModelServingNameLabelKey = "modelserving.volcano.sh/name"
	// GroupNameLabelKey is the pod label key for the group name.
	GroupNameLabelKey = "modelserving.volcano.sh/group-name"
	// RoleLabelKey is the pod label key for the role.
	RoleLabelKey = "modelserving.volcano.sh/role"
	// RoleIDKey is the pod label key for the role serial number.
	RoleIDKey = "modelserving.volcano.sh/role-id"
	// EntryLabelKey is the entry pod label key.
	EntryLabelKey = "modelserving.volcano.sh/entry"

	// RevisionLabelKey is the revision label for the model serving.
	RevisionLabelKey = "modelserving.volcano.sh/revision"
	// RoleTemplateHashLabelKey is the revision label for the role, used for RoleRollingUpdate strategy.
	RoleTemplateHashLabelKey = "modelserving.volcano.sh/role-template-hash"

	// InPlaceUpdateStateAnnotationKey is the pod annotation that records in-place update state.
	InPlaceUpdateStateAnnotationKey = "modelserving.volcano.sh/inplace-update-state"

	// InstanceGenerationLabelKey is the pod label key for the generation of the Role
	// instance the Pod was created for. It matches the generation recorded for the
	// instance in the ServingGroup status. A Pod without the label has generation 0.
	InstanceGenerationLabelKey = "modelserving.volcano.sh/instance-generation"

	// SurgeAnnotationKey marks temporary capacity created by a rolling update with
	// maxSurge. On a ServingGroup the value is SurgeScopeServingGroup: the annotation
	// is set when the group is created, removed when the group becomes a regular
	// replica and never added later. On a Pod the value is SurgeScopeRole for a
	// temporary Role instance, or SurgeScopeServingGroup as a copy of the mark of
	// its ServingGroup.
	SurgeAnnotationKey = "modelserving.volcano.sh/surge"
	// SurgeScopeServingGroup is the value of SurgeAnnotationKey for a temporary ServingGroup.
	SurgeScopeServingGroup = "serving-group"
	// SurgeScopeRole is the value of SurgeAnnotationKey for a temporary Role instance.
	SurgeScopeRole = "role"

	// CreatedRevisionAnnotationKey is the ServingGroup annotation that names
	// the revision a group was created from. It says that the group was born
	// in the rollout to that revision, and it stays for as long as that
	// revision is the target of the group. The ModelServing controller sets it
	// when it creates a new group under RoleRollingUpdate. In the write that
	// tells the group another target it removes it; from a group that has no
	// member yet, which is still being born, it does not, and makes it name
	// the new target instead. It is never added to an object that has none,
	// and an object created for Pods that exist already does not carry it. A
	// group that carries it creates every Role instance from that revision,
	// the ones below the partition of a Role included. The annotation exists
	// under RoleRollingUpdate alone: it is removed in the write that tells
	// the group that it is replaced as a whole.
	CreatedRevisionAnnotationKey = "modelserving.volcano.sh/created-revision"

	// ServingGroupAdoptionAnnotationKey is the ModelServing annotation that records
	// how far the workloads created by a version without ServingGroup objects have
	// been taken over. An absent annotation means that it was not checked yet.
	ServingGroupAdoptionAnnotationKey = "modelserving.volcano.sh/servinggroup-adoption"
	// ServingGroupAdoptionInProgress: objects owned directly by the ModelServing
	// remain. The ModelServing controller makes no group-level change.
	ServingGroupAdoptionInProgress = "InProgress"
	// ServingGroupAdoptionDone: every Pod, PodGroup and Service of the ModelServing
	// is owned by a ServingGroup.
	ServingGroupAdoptionDone = "Done"
	// ServingGroupAdoptionRollingBack: the objects are being handed back to the
	// ModelServing so that a version without ServingGroup objects can take over.
	ServingGroupAdoptionRollingBack = "RollingBack"
)
