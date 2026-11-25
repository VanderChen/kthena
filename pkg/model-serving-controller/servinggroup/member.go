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

// Package servinggroup holds the rules that apply inside one ServingGroup as
// functions of API objects: which Pods are members of a group, what state a
// member and a Role instance are in, and what the status of the group is.
// The ModelServing controller and the ServingGroup controller share them, so
// both read a group the same way.
package servinggroup

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Membership tells how a Pod relates to a ServingGroup.
type Membership int

const (
	// NotMember: the Pod is not a member of the group.
	NotMember Membership = iota
	// Owned: the controller owner of the Pod is the group.
	Owned
	// Orphan: the Pod has no controller owner and carries the instance-generation
	// label, so a ServingGroup controller managed it before. It is left behind when
	// a group is deleted with orphan propagation.
	Orphan
	// Legacy: the controller owner of the Pod is the ModelServing. The Pod was
	// created by a version without ServingGroup objects.
	Legacy
	// LegacyOrphan: the Pod has neither a controller owner nor the
	// instance-generation label, and the workloads of the ModelServing have not
	// all been taken over yet.
	LegacyOrphan
)

// Adoptable reports whether the Pod is a member that the group does not own yet.
func (m Membership) Adoptable() bool {
	return m == Orphan || m == Legacy || m == LegacyOrphan
}

// Parent is what membership depends on of the ModelServing that owns a group.
type Parent struct {
	Name string
	UID  types.UID
	// AdoptionDone reports that the adoption annotation of the ModelServing is Done.
	AdoptionDone bool
}

// ParentOf returns the Parent of the groups of ms.
func ParentOf(ms *workloadv1alpha1.ModelServing) Parent {
	return Parent{
		Name:         ms.Name,
		UID:          ms.UID,
		AdoptionDone: ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionDone,
	}
}

// MembershipOf classifies pod with respect to the group named groupName with
// UID groupUID. A Pod the group owns is a member whatever its labels and state
// are. A Pod the group does not own is a member only while it is not
// terminating and its labels and its name are those of a member of the group.
func MembershipOf(pod *corev1.Pod, groupName string, groupUID types.UID, parent Parent) Membership {
	owner := metav1.GetControllerOf(pod)
	if owner != nil && owner.UID == groupUID {
		return Owned
	}
	if pod.DeletionTimestamp != nil || !HasMemberIdentity(pod, parent.Name, groupName) {
		return NotMember
	}
	if owner != nil {
		if owner.UID == parent.UID {
			return Legacy
		}
		return NotMember
	}
	if _, ok := pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey]; ok {
		return Orphan
	}
	if !parent.AdoptionDone {
		return LegacyOrphan
	}
	return NotMember
}

// HasMemberIdentity reports whether the labels of pod name the group and a
// Role instance of it, and the name of pod is the name of a member of that
// instance: <group>-<role>-<instance>-<member>. A copy of a member Pod that
// kept the labels under another name has no member identity.
func HasMemberIdentity(pod *corev1.Pod, modelServingName, groupName string) bool {
	if pod.Labels[workloadv1alpha1.ModelServingNameLabelKey] != modelServingName ||
		pod.Labels[workloadv1alpha1.GroupNameLabelKey] != groupName {
		return false
	}
	role, roleID := pod.Labels[workloadv1alpha1.RoleLabelKey], pod.Labels[workloadv1alpha1.RoleIDKey]
	if role == "" {
		return false
	}
	if _, ok := ordinalAfter(roleID, role+"-"); !ok {
		return false
	}
	_, ok := ordinalAfter(pod.Name, groupName+"-"+roleID+"-")
	return ok
}

// ordinalAfter returns the ordinal that follows prefix in name. The ordinal
// must be written without a sign and without leading zeros.
func ordinalAfter(name, prefix string) (int, bool) {
	suffix, found := strings.CutPrefix(name, prefix)
	if !found {
		return 0, false
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil || ordinal < 0 || strconv.Itoa(ordinal) != suffix {
		return 0, false
	}
	return ordinal, true
}

// Members returns the member Pods of the group among pods, sorted by name.
func Members(pods []*corev1.Pod, groupName string, groupUID types.UID, parent Parent) []*corev1.Pod {
	var members []*corev1.Pod
	for _, pod := range pods {
		if MembershipOf(pod, groupName, groupUID, parent) != NotMember {
			members = append(members, pod)
		}
	}
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })
	return members
}

// InstanceGeneration returns the generation of the Role instance the Pod was
// created for. A Pod without the label, or with a malformed one, has generation 0.
func InstanceGeneration(pod *corev1.Pod) int64 {
	generation, err := strconv.ParseInt(pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey], 10, 64)
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

// projectedLabels are the labels the rules read.
var projectedLabels = []string{
	workloadv1alpha1.ModelServingNameLabelKey,
	workloadv1alpha1.GroupNameLabelKey,
	workloadv1alpha1.RoleLabelKey,
	workloadv1alpha1.RoleIDKey,
	workloadv1alpha1.EntryLabelKey,
	workloadv1alpha1.RevisionLabelKey,
	workloadv1alpha1.RoleTemplateHashLabelKey,
	workloadv1alpha1.InstanceGenerationLabelKey,
}

// Project returns a copy of pod reduced to the fields the rules read: identity,
// deletion, the labels and annotations that carry state, the controller owner,
// container images, phase, readiness and container restarts.
//
// The state of a member is decided from the projection and from nothing else
// of the Pod, and Digest covers exactly the projection. A status computed from
// some Pods is therefore out of date exactly when Digest of those Pods changed.
// Fields that other components write and no rule reads (the deletion cost, the
// messages of conditions, the resourceVersion) are not part of it.
func Project(pod *corev1.Pod) *corev1.Pod {
	out := &corev1.Pod{}
	out.Namespace, out.Name, out.UID = pod.Namespace, pod.Name, pod.UID
	out.DeletionTimestamp = pod.DeletionTimestamp.DeepCopy()

	for _, key := range projectedLabels {
		if value, ok := pod.Labels[key]; ok {
			if out.Labels == nil {
				out.Labels = map[string]string{}
			}
			out.Labels[key] = value
		}
	}
	if value, ok := pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey]; ok {
		out.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: value}
	}
	// The copy of the group's mark is an output. Only the mark of a temporary Role
	// instance is state.
	if pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeRole {
		if out.Annotations == nil {
			out.Annotations = map[string]string{}
		}
		out.Annotations[workloadv1alpha1.SurgeAnnotationKey] = workloadv1alpha1.SurgeScopeRole
	}
	if owner := metav1.GetControllerOf(pod); owner != nil {
		controller := true
		out.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: owner.APIVersion, Kind: owner.Kind, Name: owner.Name, UID: owner.UID, Controller: &controller,
		}}
	}

	for _, container := range pod.Spec.Containers {
		out.Spec.Containers = append(out.Spec.Containers, corev1.Container{Name: container.Name, Image: container.Image})
	}

	out.Status.Phase = pod.Status.Phase
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			out.Status.Conditions = []corev1.PodCondition{{
				Type: condition.Type, Status: condition.Status, LastTransitionTime: condition.LastTransitionTime,
			}}
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		out.Status.InitContainerStatuses = append(out.Status.InitContainerStatuses,
			corev1.ContainerStatus{Name: status.Name, RestartCount: status.RestartCount})
	}
	for _, status := range pod.Status.ContainerStatuses {
		out.Status.ContainerStatuses = append(out.Status.ContainerStatuses, corev1.ContainerStatus{
			Name: status.Name, RestartCount: status.RestartCount,
			ContainerID: status.ContainerID, Image: status.Image, ImageID: status.ImageID,
		})
	}
	return out
}

// digestVersion names the encoding of Digest. A status written with another
// encoding never compares equal, so it is computed again instead of trusted.
const digestVersion = "v1"

// digestEntry is the canonical form of one member Pod in a digest.
type digestEntry struct {
	UID         types.UID                `json:"uid"`
	Name        string                   `json:"name"`
	Deleting    bool                     `json:"deleting,omitempty"`
	Labels      map[string]string        `json:"labels,omitempty"`
	Annotations map[string]string        `json:"annotations,omitempty"`
	Owner       types.UID                `json:"owner,omitempty"`
	Images      []string                 `json:"images,omitempty"`
	Phase       corev1.PodPhase          `json:"phase,omitempty"`
	Ready       corev1.ConditionStatus   `json:"ready,omitempty"`
	ReadySince  int64                    `json:"readySince,omitempty"`
	Init        []digestContainerRestart `json:"init,omitempty"`
	Containers  []digestContainerStatus  `json:"containers,omitempty"`
}

type digestContainerRestart struct {
	Name     string `json:"name"`
	Restarts int32  `json:"restarts,omitempty"`
}

type digestContainerStatus struct {
	Name        string `json:"name"`
	Restarts    int32  `json:"restarts,omitempty"`
	ContainerID string `json:"containerID,omitempty"`
	Image       string `json:"image,omitempty"`
	ImageID     string `json:"imageID,omitempty"`
}

// Digest returns a digest of the projections of pods. It does not depend on
// the order of pods. Two sets of Pods have the same digest exactly when every
// rule reads them the same way (up to hash collisions; the digest has 128 bits).
func Digest(pods []*corev1.Pod) string {
	entries := make([]digestEntry, 0, len(pods))
	for _, pod := range pods {
		p := Project(pod)
		entry := digestEntry{
			UID: p.UID, Name: p.Name, Deleting: p.DeletionTimestamp != nil,
			Labels: p.Labels, Annotations: p.Annotations, Phase: p.Status.Phase,
		}
		if len(p.OwnerReferences) > 0 {
			entry.Owner = p.OwnerReferences[0].UID
		}
		for _, container := range p.Spec.Containers {
			entry.Images = append(entry.Images, container.Name+"="+container.Image)
		}
		for _, condition := range p.Status.Conditions {
			entry.Ready, entry.ReadySince = condition.Status, condition.LastTransitionTime.Unix()
		}
		for _, status := range p.Status.InitContainerStatuses {
			entry.Init = append(entry.Init, digestContainerRestart{Name: status.Name, Restarts: status.RestartCount})
		}
		for _, status := range p.Status.ContainerStatuses {
			entry.Containers = append(entry.Containers, digestContainerStatus{
				Name: status.Name, Restarts: status.RestartCount,
				ContainerID: status.ContainerID, Image: status.Image, ImageID: status.ImageID,
			})
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Name != entries[j].Name {
			return entries[i].Name < entries[j].Name
		}
		return entries[i].UID < entries[j].UID
	})
	// Marshal cannot fail: the entries hold only strings, numbers and maps of strings.
	encoded, _ := json.Marshal(entries)
	sum := sha256.Sum256(encoded)
	return digestVersion + "." + hex.EncodeToString(sum[:16])
}
