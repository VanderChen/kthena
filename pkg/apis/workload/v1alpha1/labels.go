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

	// GroupIndexLabelKey is the pod label key for the group index (numeric ordinal).
	// This label provides compatibility with LWS's group-index label for easier migration.
	GroupIndexLabelKey = "modelserving.volcano.sh/group-index"

	// GroupHashLabelKey is the pod label key for the group unique hash identifier.
	// This label allows selecting all pods in the same group using a single label selector.
	GroupHashLabelKey = "modelserving.volcano.sh/group-hash"
)

const (
	// EntryPodNameAnnotationKey is the annotation key for worker pods to record their entry pod name.
	// This annotation provides compatibility with LWS's leader-name annotation.
	EntryPodNameAnnotationKey = "modelserving.volcano.sh/entry-name"

	// GroupSizeAnnotationKey is the annotation key for recording the group size at pod level.
	// This annotation provides compatibility with LWS's size annotation.
	GroupSizeAnnotationKey = "modelserving.volcano.sh/group-size"

	// ReplicasAnnotationKey is the annotation key for recording the total replicas at pod level.
	// This annotation provides compatibility with LWS's replicas annotation.
	ReplicasAnnotationKey = "modelserving.volcano.sh/replicas"
)
