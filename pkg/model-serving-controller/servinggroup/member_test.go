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
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const (
	testMS       = "llama"
	testMSUID    = types.UID("ms-uid")
	testGroup    = "llama-0"
	testGroupUID = types.UID("sg-uid")
)

func ownerRef(kind, name string, uid types.UID) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: kind, Name: name, UID: uid,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}
}

// memberPod returns a ready member Pod <group>-<role>-<instance>-<index> owned by the test group.
func memberPod(role string, instance, index int, mutate ...func(*corev1.Pod)) *corev1.Pod {
	roleID := role + "-" + string(rune('0'+instance))
	name := testGroup + "-" + roleID + "-" + string(rune('0'+index))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: name, UID: types.UID("uid-" + name),
			Labels: map[string]string{
				workloadv1alpha1.ModelServingNameLabelKey:   testMS,
				workloadv1alpha1.GroupNameLabelKey:          testGroup,
				workloadv1alpha1.RoleLabelKey:               role,
				workloadv1alpha1.RoleIDKey:                  roleID,
				workloadv1alpha1.RevisionLabelKey:           "rev-a",
				workloadv1alpha1.RoleTemplateHashLabelKey:   "hash-a",
				workloadv1alpha1.InstanceGenerationLabelKey: "0",
			},
			OwnerReferences: []metav1.OwnerReference{ownerRef("ServingGroup", testGroup, testGroupUID)},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "model:v1"}}},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type: corev1.PodReady, Status: corev1.ConditionTrue,
				LastTransitionTime: metav1.NewTime(time.Unix(1700000000, 0)),
			}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: "model", ContainerID: "containerd://a", Image: "model:v1", ImageID: "sha256:1"}},
		},
	}
	if index == 0 {
		pod.Labels[workloadv1alpha1.EntryLabelKey] = "true"
	}
	for _, m := range mutate {
		m(pod)
	}
	return pod
}

func noOwner(pod *corev1.Pod) { pod.OwnerReferences = nil }
func terminating(pod *corev1.Pod) {
	pod.DeletionTimestamp = ptr.To(metav1.NewTime(time.Unix(1700000100, 0)))
}
func noGeneration(pod *corev1.Pod) {
	delete(pod.Labels, workloadv1alpha1.InstanceGenerationLabelKey)
}
func ownedBy(kind, name string, uid types.UID) func(*corev1.Pod) {
	return func(pod *corev1.Pod) { pod.OwnerReferences = []metav1.OwnerReference{ownerRef(kind, name, uid)} }
}

func TestMembershipOf(t *testing.T) {
	adopting := Parent{Name: testMS, UID: testMSUID}
	done := Parent{Name: testMS, UID: testMSUID, AdoptionDone: true}
	legacy := ownedBy("ModelServing", testMS, testMSUID)

	tests := []struct {
		name   string
		pod    *corev1.Pod
		parent Parent
		want   Membership
	}{
		{"owned", memberPod("prefill", 0, 0), done, Owned},
		{"owned and terminating", memberPod("prefill", 0, 0, terminating), done, Owned},
		{"owned whatever the labels say", memberPod("prefill", 0, 0, func(p *corev1.Pod) { p.Labels = nil }), done, Owned},
		{"owned by the previous object of the same name", memberPod("prefill", 0, 0, ownedBy("ServingGroup", testGroup, "old-sg-uid")), done, NotMember},
		{"owned by another controller", memberPod("prefill", 0, 0, ownedBy("ReplicaSet", "rs", "rs-uid")), adopting, NotMember},
		{"legacy", memberPod("prefill", 0, 1, legacy, noGeneration), adopting, Legacy},
		{"legacy after the takeover is still taken over", memberPod("prefill", 0, 1, legacy), done, Legacy},
		{"legacy and terminating", memberPod("prefill", 0, 1, legacy, terminating), adopting, NotMember},
		{"legacy of another ModelServing of the same name", memberPod("prefill", 0, 1, ownedBy("ModelServing", testMS, "other-ms-uid")), adopting, NotMember},
		{"orphan", memberPod("prefill", 1, 0, noOwner), done, Orphan},
		{"orphan and terminating", memberPod("prefill", 1, 0, noOwner, terminating), done, NotMember},
		{"orphan without generation label during the takeover", memberPod("prefill", 1, 0, noOwner, noGeneration), adopting, LegacyOrphan},
		{"orphan without generation label after the takeover", memberPod("prefill", 1, 0, noOwner, noGeneration), done, NotMember},
		{"a copy that kept the labels", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Name = "llama-0-prefill-1-0-debug" }), done, NotMember},
		{"labels of another group", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Labels[workloadv1alpha1.GroupNameLabelKey] = "llama-1" }), done, NotMember},
		{"labels of another ModelServing", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Labels[workloadv1alpha1.ModelServingNameLabelKey] = "other" }), done, NotMember},
		{"role id of another role", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Labels[workloadv1alpha1.RoleIDKey] = "decode-1" }), done, NotMember},
		{"name of another instance", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Name = "llama-0-prefill-2-0" }), done, NotMember},
		{"ordinal with a leading zero", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { p.Name = "llama-0-prefill-1-00" }), done, NotMember},
		{"no role label", memberPod("prefill", 1, 0, noOwner, func(p *corev1.Pod) { delete(p.Labels, workloadv1alpha1.RoleLabelKey) }), done, NotMember},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := MembershipOf(tt.pod, testGroup, testGroupUID, tt.parent); got != tt.want {
				t.Errorf("MembershipOf() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMembersAreSortedAndFiltered(t *testing.T) {
	pods := []*corev1.Pod{
		memberPod("prefill", 1, 0),
		memberPod("decode", 0, 0, ownedBy("ServingGroup", "llama-1", "other-sg-uid")),
		memberPod("prefill", 0, 1, noOwner),
		memberPod("prefill", 0, 0),
	}
	got := Members(pods, testGroup, testGroupUID, Parent{Name: testMS, UID: testMSUID, AdoptionDone: true})
	var names []string
	for _, pod := range got {
		names = append(names, pod.Name)
	}
	if want := "llama-0-prefill-0-0,llama-0-prefill-0-1,llama-0-prefill-1-0"; strings.Join(names, ",") != want {
		t.Errorf("Members() = %v, want %s", names, want)
	}
}

func TestInstanceGeneration(t *testing.T) {
	tests := []struct {
		label string
		set   bool
		want  int64
	}{
		{set: false, want: 0},
		{label: "0", set: true, want: 0},
		{label: "7", set: true, want: 7},
		{label: "-1", set: true, want: 0},
		{label: "x", set: true, want: 0},
	}
	for _, tt := range tests {
		pod := memberPod("prefill", 0, 0, noGeneration)
		if tt.set {
			pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = tt.label
		}
		if got := InstanceGeneration(pod); got != tt.want {
			t.Errorf("InstanceGeneration(%q, set=%t) = %d, want %d", tt.label, tt.set, got, tt.want)
		}
	}
}

func TestDigestIgnoresOrderAndUnreadFields(t *testing.T) {
	a, b := memberPod("prefill", 0, 0), memberPod("prefill", 0, 1)
	base := Digest([]*corev1.Pod{a, b})
	if !strings.HasPrefix(base, digestVersion+".") || len(base) != len(digestVersion)+1+32 {
		t.Fatalf("unexpected digest format %q", base)
	}
	if got := Digest([]*corev1.Pod{b, a}); got != base {
		t.Errorf("digest depends on the order of the Pods: %s != %s", got, base)
	}

	unread := map[string]func(*corev1.Pod){
		"resourceVersion": func(p *corev1.Pod) { p.ResourceVersion = "42" },
		"deletion cost": func(p *corev1.Pod) {
			p.Annotations = map[string]string{"controller.kubernetes.io/pod-deletion-cost": "5"}
		},
		"copy of group mark": func(p *corev1.Pod) {
			p.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
		},
		"other label":       func(p *corev1.Pod) { p.Labels["app"] = "x" },
		"condition message": func(p *corev1.Pod) { p.Status.Conditions[0].Message = "probe" },
		"other condition": func(p *corev1.Pod) {
			p.Status.Conditions = append(p.Status.Conditions, corev1.PodCondition{Type: corev1.PodScheduled, Status: corev1.ConditionTrue})
		},
		"pod IP":    func(p *corev1.Pod) { p.Status.PodIP = "10.0.0.1" },
		"node name": func(p *corev1.Pod) { p.Spec.NodeName = "node-1" },
		"non-controller owner": func(p *corev1.Pod) {
			p.OwnerReferences = append(p.OwnerReferences, metav1.OwnerReference{Kind: "ConfigMap", Name: "x", UID: "cm"})
		},
	}
	for name, mutate := range unread {
		changed := memberPod("prefill", 0, 0, mutate)
		if got := Digest([]*corev1.Pod{changed, b}); got != base {
			t.Errorf("%s: digest changed although no rule reads the field", name)
		}
	}
}

func TestDigestCoversEveryProjectedField(t *testing.T) {
	a, b := memberPod("prefill", 0, 0), memberPod("prefill", 0, 1)
	base := Digest([]*corev1.Pod{a, b})

	read := map[string]func(*corev1.Pod){
		"uid":          func(p *corev1.Pod) { p.UID = "another" },
		"deletion":     terminating,
		"phase":        func(p *corev1.Pod) { p.Status.Phase = corev1.PodFailed },
		"ready status": func(p *corev1.Pod) { p.Status.Conditions[0].Status = corev1.ConditionFalse },
		"ready transition": func(p *corev1.Pod) {
			p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(time.Unix(1700000001, 0))
		},
		"restart count": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 1 },
		"init restart count": func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
		},
		"container id":     func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ContainerID = "containerd://b" },
		"running image":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Image = "model:v2" },
		"running image id": func(p *corev1.Pod) { p.Status.ContainerStatuses[0].ImageID = "sha256:2" },
		"spec image":       func(p *corev1.Pod) { p.Spec.Containers[0].Image = "model:v2" },
		"controller owner": noOwner,
		"revision label":   func(p *corev1.Pod) { p.Labels[workloadv1alpha1.RevisionLabelKey] = "rev-b" },
		"role hash label":  func(p *corev1.Pod) { p.Labels[workloadv1alpha1.RoleTemplateHashLabelKey] = "hash-b" },
		"generation label": func(p *corev1.Pod) { p.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = "1" },
		"in-place annotation": func(p *corev1.Pod) {
			p.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: `{"revision":"rev-b"}`}
		},
		"role surge annotation": func(p *corev1.Pod) {
			p.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeRole}
		},
	}
	for name, mutate := range read {
		changed := memberPod("prefill", 0, 0, mutate)
		if got := Digest([]*corev1.Pod{changed, b}); got == base {
			t.Errorf("%s: digest did not change although a rule reads the field", name)
		}
	}

	if got := Digest([]*corev1.Pod{a}); got == base {
		t.Error("digest did not change when a Pod was removed")
	}
	if got := Digest([]*corev1.Pod{a, b, memberPod("prefill", 1, 0)}); got == base {
		t.Error("digest did not change when a Pod was added")
	}
	if Digest(nil) != Digest([]*corev1.Pod{}) {
		t.Error("digest of no Pods is not stable")
	}
}

func TestProjectDoesNotAliasThePod(t *testing.T) {
	pod := memberPod("prefill", 0, 0, terminating)
	projected := Project(pod)
	projected.Labels[workloadv1alpha1.RevisionLabelKey] = "changed"
	projected.Status.ContainerStatuses[0].RestartCount = 9
	*projected.DeletionTimestamp = metav1.NewTime(time.Unix(0, 0))
	if pod.Labels[workloadv1alpha1.RevisionLabelKey] != "rev-a" || pod.Status.ContainerStatuses[0].RestartCount != 0 ||
		pod.DeletionTimestamp.Unix() != 1700000100 {
		t.Error("Project returned data shared with its input")
	}
	if projected.ResourceVersion != "" || projected.Spec.NodeName != "" {
		t.Error("Project kept a field no rule reads")
	}
}
