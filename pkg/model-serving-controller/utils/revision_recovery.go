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

package utils

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// RevisionRebuiltFromGenerationAnnotation marks a ControllerRevision whose data
// was rebuilt from the live ModelServing spec because the original history was
// missing. Its value is the spec generation the data was taken from.
const RevisionRebuiltFromGenerationAnnotation = "modelserving.volcano.sh/revision-rebuilt-from-generation"

// RebuildModelServingRevision persists the current template of ms under an
// identity that live Pods already carry but no ControllerRevision describes.
// Callers must first prove that the Pods really run the current template; this
// function only makes that decision durable.
//
// The rebuilt object takes the highest numeric Revision so that
// RecordModelServingRevision prefers the running identity over the canonical
// hash it may already have persisted for the same data, mirroring how a legacy
// baseline keeps the desired identity on the Pods' label.
func RebuildModelServingRevision(ctx context.Context, client kubernetes.Interface, ms *workloadv1alpha1.ModelServing, revision string) (*appsv1.ControllerRevision, error) {
	if ms == nil || revision == "" {
		return nil, fmt.Errorf("model serving or revision is missing")
	}
	data, err := BuildRevisionData(ms)
	if err != nil {
		return nil, err
	}
	if _, err := decodeRevisionPatch(data); err != nil {
		return nil, err
	}
	name := GenerateControllerRevisionName(ms.Name, revision)
	revisions := client.AppsV1().ControllerRevisions(ms.Namespace)
	validate := func(existing *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
		if !controlledByModelServing(existing, ms) ||
			existing.Annotations[ControllerRevisionDataVersionAnnotation] != ControllerRevisionDataVersionV1 ||
			existing.Labels[ControllerRevisionRevisionLabelKey] != revision ||
			!revisionDataEqual(existing.Data.Raw, data) {
			return nil, fmt.Errorf("ControllerRevision %s already exists with a different identity or template", name)
		}
		return existing, nil
	}

	history, err := revisions.List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{ControllerRevisionLabelKey: ms.Name}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list controller revisions: %w", err)
	}
	var maxRevision int64
	for i := range history.Items {
		if controlledByModelServing(&history.Items[i], ms) && history.Items[i].Revision > maxRevision {
			maxRevision = history.Items[i].Revision
		}
	}

	rebuilt := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				ControllerRevisionLabelKey:         ms.Name,
				ControllerRevisionRevisionLabelKey: revision,
			},
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
				RevisionRebuiltFromGenerationAnnotation: strconv.FormatInt(ms.Generation, 10),
			},
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(ms)},
		},
		Revision: maxRevision + 1,
		Data:     runtime.RawExtension{Raw: data},
	}
	created, err := revisions.Create(ctx, rebuilt, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := revisions.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("get concurrently created ControllerRevision %s: %w", name, getErr)
		}
		return validate(existing)
	}
	if err != nil {
		return nil, fmt.Errorf("create ControllerRevision %s: %w", name, err)
	}
	klog.Warningf("Rebuilt missing ControllerRevision %s/%s for ModelServing %s from spec generation %d", ms.Namespace, name, ms.Name, ms.Generation)
	return created, nil
}

// RenderPodForRecovery renders the Pod the current template would create at the
// position a live Pod occupies (group, role instance, worker index), so the live
// Pod can be compared against it. It returns nil when the Pod's labels or name do
// not identify a position the template defines.
func RenderPodForRecovery(ms *workloadv1alpha1.ModelServing, pod *corev1.Pod) *corev1.Pod {
	roleName := pod.Labels[workloadv1alpha1.RoleLabelKey]
	group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
	roleID := pod.Labels[workloadv1alpha1.RoleIDKey]
	if roleName == "" || group == "" || roleID == "" {
		return nil
	}
	var role *workloadv1alpha1.Role
	for i := range ms.Spec.Template.Roles {
		if ms.Spec.Template.Roles[i].Name == roleName {
			role = ms.Spec.Template.Roles[i].DeepCopy()
			break
		}
	}
	if role == nil {
		return nil
	}
	revision := ObjectRevision(pod)
	if pod.Labels[workloadv1alpha1.EntryLabelKey] == Entry {
		return GenerateEntryPod(*role, ms, group, roleID, revision, "")
	}
	prefix := GeneratePodName(group, roleID, 0)
	prefix = prefix[:len(prefix)-1]
	if !strings.HasPrefix(pod.Name, prefix) {
		return nil
	}
	index, err := strconv.Atoi(strings.TrimPrefix(pod.Name, prefix))
	if err != nil || index < 1 || index > int(role.WorkerReplicas) || role.WorkerTemplate == nil {
		return nil
	}
	return GenerateWorkerPod(*role, ms, group, roleID, index, revision, "")
}

// PodDeviation reports the first way a live Pod fails to prove it runs the
// rendered template, or "" when it does. The container sets must be identical
// and every field the template declares must be present with the same value;
// additions by admission or plugins (extra env, volumes, tolerations, defaulted
// resources) are tolerated because they cannot hide a template change.
func PodDeviation(rendered, live *corev1.Pod) string {
	if reason := containerSetDeviation("container", rendered.Spec.Containers, live.Spec.Containers); reason != "" {
		return reason
	}
	if reason := containerSetDeviation("init container", rendered.Spec.InitContainers, live.Spec.InitContainers); reason != "" {
		return reason
	}
	for key, want := range rendered.Spec.NodeSelector {
		if got, ok := live.Spec.NodeSelector[key]; !ok || got != want {
			return fmt.Sprintf("nodeSelector %s=%q is not %q", key, got, want)
		}
	}
	if want := rendered.Spec.ServiceAccountName; want != "" && live.Spec.ServiceAccountName != want {
		return fmt.Sprintf("serviceAccountName %q is not %q", live.Spec.ServiceAccountName, want)
	}
	if want := rendered.Spec.PriorityClassName; want != "" && live.Spec.PriorityClassName != want {
		return fmt.Sprintf("priorityClassName %q is not %q", live.Spec.PriorityClassName, want)
	}
	if want := rendered.Spec.RuntimeClassName; want != nil && (live.Spec.RuntimeClassName == nil || *live.Spec.RuntimeClassName != *want) {
		return fmt.Sprintf("runtimeClassName is not %q", *want)
	}
	liveVolumes := make(map[string]corev1.Volume, len(live.Spec.Volumes))
	for _, volume := range live.Spec.Volumes {
		liveVolumes[volume.Name] = volume
	}
	for _, want := range rendered.Spec.Volumes {
		got, ok := liveVolumes[want.Name]
		if !ok {
			return fmt.Sprintf("volume %s is missing", want.Name)
		}
		if ref := volumeReference(want.VolumeSource); ref != "" && ref != volumeReference(got.VolumeSource) {
			return fmt.Sprintf("volume %s refers to %s, not %s", want.Name, volumeReference(got.VolumeSource), ref)
		}
	}
	return ""
}

func containerSetDeviation(kind string, rendered, live []corev1.Container) string {
	liveByName := make(map[string]*corev1.Container, len(live))
	for i := range live {
		liveByName[live[i].Name] = &live[i]
	}
	if len(rendered) != len(live) {
		return fmt.Sprintf("%d %ss run but the template declares %d", len(live), kind, len(rendered))
	}
	for i := range rendered {
		want := &rendered[i]
		got, ok := liveByName[want.Name]
		if !ok {
			return fmt.Sprintf("%s %s is missing", kind, want.Name)
		}
		if reason := containerDeviation(want, got); reason != "" {
			return fmt.Sprintf("%s %s %s", kind, want.Name, reason)
		}
	}
	return ""
}

func containerDeviation(want, got *corev1.Container) string {
	if want.Image != got.Image {
		return fmt.Sprintf("runs %s but the template renders %s", got.Image, want.Image)
	}
	if !stringSlicesEqual(want.Command, got.Command) {
		return "runs a different command"
	}
	if !stringSlicesEqual(want.Args, got.Args) {
		return "runs different args"
	}
	if want.WorkingDir != "" && want.WorkingDir != got.WorkingDir {
		return "runs in a different workingDir"
	}
	liveEnv := make(map[string]corev1.EnvVar, len(got.Env))
	for _, env := range got.Env {
		liveEnv[env.Name] = env
	}
	for _, env := range want.Env {
		live, ok := liveEnv[env.Name]
		if !ok || live.Value != env.Value || !envSourcesEqual(live.ValueFrom, env.ValueFrom) {
			return fmt.Sprintf("env %s differs from the template", env.Name)
		}
	}
	for _, port := range want.Ports {
		found := false
		for _, live := range got.Ports {
			if live.ContainerPort == port.ContainerPort && effectiveProtocol(live.Protocol) == effectiveProtocol(port.Protocol) {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("does not expose port %d", port.ContainerPort)
		}
	}
	for _, mount := range want.VolumeMounts {
		found := false
		for _, live := range got.VolumeMounts {
			if live.Name == mount.Name && live.MountPath == mount.MountPath && live.SubPath == mount.SubPath && live.ReadOnly == mount.ReadOnly {
				found = true
				break
			}
		}
		if !found {
			return fmt.Sprintf("does not mount %s at %s", mount.Name, mount.MountPath)
		}
	}
	for name, quantity := range want.Resources.Limits {
		if live, ok := got.Resources.Limits[name]; !ok || live.Cmp(quantity) != 0 {
			return fmt.Sprintf("limits.%s differs from the template", name)
		}
	}
	for name, quantity := range want.Resources.Requests {
		if live, ok := got.Resources.Requests[name]; !ok || live.Cmp(quantity) != 0 {
			return fmt.Sprintf("requests.%s differs from the template", name)
		}
	}
	return ""
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// envSourcesEqual compares env sources after API-server defaulting: a live Pod's
// fieldRef carries apiVersion "v1" even when the template omitted it, and
// resourceFieldRef divisors are quantities, not strings.
func envSourcesEqual(live, want *corev1.EnvVarSource) bool {
	if live == nil || want == nil {
		return live == want
	}
	if (live.FieldRef == nil) != (want.FieldRef == nil) {
		return false
	}
	if live.FieldRef != nil && (live.FieldRef.FieldPath != want.FieldRef.FieldPath || effectiveFieldRefVersion(live.FieldRef.APIVersion) != effectiveFieldRefVersion(want.FieldRef.APIVersion)) {
		return false
	}
	if (live.ResourceFieldRef == nil) != (want.ResourceFieldRef == nil) {
		return false
	}
	if live.ResourceFieldRef != nil && (live.ResourceFieldRef.ContainerName != want.ResourceFieldRef.ContainerName || live.ResourceFieldRef.Resource != want.ResourceFieldRef.Resource || !divisorsEqual(live.ResourceFieldRef.Divisor, want.ResourceFieldRef.Divisor)) {
		return false
	}
	return apiequality.Semantic.DeepEqual(live.ConfigMapKeyRef, want.ConfigMapKeyRef) && apiequality.Semantic.DeepEqual(live.SecretKeyRef, want.SecretKeyRef)
}

func divisorsEqual(live, want resource.Quantity) bool {
	one := resource.MustParse("1")
	if live.IsZero() {
		live = one
	}
	if want.IsZero() {
		want = one
	}
	return live.Cmp(want) == 0
}

func effectiveFieldRefVersion(version string) string {
	if version == "" {
		return "v1"
	}
	return version
}

func effectiveProtocol(protocol corev1.Protocol) corev1.Protocol {
	if protocol == "" {
		return corev1.ProtocolTCP
	}
	return protocol
}

func volumeReference(source corev1.VolumeSource) string {
	switch {
	case source.ConfigMap != nil:
		return "configmap/" + source.ConfigMap.Name
	case source.Secret != nil:
		return "secret/" + source.Secret.SecretName
	case source.PersistentVolumeClaim != nil:
		return "pvc/" + source.PersistentVolumeClaim.ClaimName
	case source.HostPath != nil:
		return "hostPath/" + source.HostPath.Path
	}
	return ""
}
