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

package ranktable

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// RanktableController manages ranktable generation for ModelServing
type RanktableController struct {
	kubeClient      kubernetes.Interface
	templateManager *TemplateManager
}

// NewController creates a new ranktable controller
func NewrRanktableController(kubeClient kubernetes.Interface) *RanktableController {
	return &RanktableController{
		kubeClient:      kubeClient,
		templateManager: NewTemplateManager(kubeClient),
	}
}

// NeedsRanktable checks if ModelServing requires ranktable generation
func (c *RanktableController) NeedsRanktable(ms *workloadv1alpha1.ModelServing) bool {
	if ms.Annotations == nil {
		return false
	}
	_, exists := ms.Annotations[RanktableTemplateAnnotation]
	return exists
}

// GetRanktableTemplate retrieves the ranktable template for a ModelServing
func (c *RanktableController) GetRanktableTemplate(ctx context.Context, ms *workloadv1alpha1.ModelServing) (*RanktableTemplate, error) {
	templateName := ms.Annotations[RanktableTemplateAnnotation]
	if templateName == "" {
		return nil, fmt.Errorf("ranktable template annotation is empty")
	}

	template, err := c.templateManager.GetRanktableTemplate(ctx, templateName)
	if err != nil {
		return nil, fmt.Errorf("failed to get ranktable template %s: %w", templateName, err)
	}

	// Override level if specified in annotation
	if levelOverride, ok := ms.Annotations[RanktableLevelAnnotation]; ok {
		level := RanktableLevel(levelOverride)
		if level == RoleLevelRanktable || level == GroupLevelRanktable {
			klog.V(2).Infof("Overriding ranktable level to %s for ModelServing %s/%s", level, ms.Namespace, ms.Name)
			template.Level = level
		}
	}

	klog.V(3).Infof("Successfully retrieved ranktable template %s for ModelServing %s/%s (level: %s)",
		templateName, ms.Namespace, ms.Name, template.Level)
	return template, nil
}

// EnsureRanktableConfigMaps creates empty ranktable ConfigMaps for each role/group
func (c *RanktableController) EnsureRanktableConfigMaps(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	template *RanktableTemplate,
) error {
	apiVersion := ms.APIVersion
	if apiVersion == "" {
		apiVersion = workloadv1alpha1.SchemeGroupVersion.String()
	}
	kind := ms.Kind
	if kind == "" {
		kind = "ModelServing"
	}

	ownerRefs := []metav1.OwnerReference{
		{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       ms.Name,
			UID:        ms.UID,
			Controller: func() *bool { b := true; return &b }(),
		},
	}

	klog.V(3).Infof("Ensuring ranktable ConfigMaps for ModelServing %s/%s at %s level", ms.Namespace, ms.Name, template.Level)

	// Determine which ConfigMaps to create based on ranktable level
	if template.Level == RoleLevelRanktable {
		// Create ConfigMap for each role
		for _, role := range ms.Spec.Template.Roles {
			cmName := GenerateRanktableConfigMapName(ms.Name, role.Name)
			labels := map[string]string{
				workloadv1alpha1.GroupNameLabelKey: ms.Name,
				"app.kubernetes.io/component":      "ranktable",
				"ranktable-level":                  string(RoleLevelRanktable),
			}

			klog.V(3).Infof("Ensuring ranktable ConfigMap %s for role %s in ModelServing %s/%s", cmName, role.Name, ms.Namespace, ms.Name)
			err := c.templateManager.EnsureRanktableConfigMap(
				ctx,
				ms.Namespace,
				cmName,
				ownerRefs,
				labels,
				template.Filename,
				"", // Empty content initially
			)
			if err != nil {
				return fmt.Errorf("failed to ensure ranktable ConfigMap for role %s: %w", role.Name, err)
			}
		}
	} else {
		// Create ConfigMap for each group
		for i := 0; i < int(*ms.Spec.Replicas); i++ {
			groupName := utils.GenerateServingGroupName(ms.Name, i)
			cmName := GenerateRanktableConfigMapName(ms.Name, groupName)
			labels := map[string]string{
				workloadv1alpha1.GroupNameLabelKey: groupName,
				"app.kubernetes.io/component":      "ranktable",
				"ranktable-level":                  string(GroupLevelRanktable),
			}

			klog.V(3).Infof("Ensuring ranktable ConfigMap %s for group %s in ModelServing %s/%s", cmName, groupName, ms.Namespace, ms.Name)
			err := c.templateManager.EnsureRanktableConfigMap(
				ctx,
				ms.Namespace,
				cmName,
				ownerRefs,
				labels,
				template.Filename,
				"", // Empty content initially
			)
			if err != nil {
				return fmt.Errorf("failed to ensure ranktable ConfigMap for group %s: %w", groupName, err)
			}
		}
	}

	return nil
}

// InjectRanktableMount injects volume mounts for ranktable
func (c *RanktableController) InjectRanktableMount(
	pod *corev1.Pod,
	template *RanktableTemplate,
	cmName string,
) {
	klog.V(3).Infof("Injecting ranktable mount (ConfigMap: %s, MountPath: %s) into pod %s/%s",
		cmName, template.MountPath, pod.Namespace, pod.Name)

	// Add Volume
	pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
		Name: "ranktable",
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cmName,
				},
			},
		},
	})

	// Add VolumeMount to all main containers
	for i := range pod.Spec.Containers {
		klog.V(4).Infof("Adding ranktable volume mount to container %s in pod %s/%s",
			pod.Spec.Containers[i].Name, pod.Namespace, pod.Name)
		pod.Spec.Containers[i].VolumeMounts = append(
			pod.Spec.Containers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "ranktable",
				MountPath: template.MountPath,
				ReadOnly:  true,
			},
		)
	}

	// Add VolumeMount to all init containers
	for i := range pod.Spec.InitContainers {
		klog.V(4).Infof("Adding ranktable volume mount to init-container %s in pod %s/%s",
			pod.Spec.InitContainers[i].Name, pod.Namespace, pod.Name)
		pod.Spec.InitContainers[i].VolumeMounts = append(
			pod.Spec.InitContainers[i].VolumeMounts,
			corev1.VolumeMount{
				Name:      "ranktable",
				MountPath: template.MountPath,
				ReadOnly:  true,
			},
		)
	}
}

// CheckPodsRanktableReady checks if all pods have ranktable annotations
func (c *RanktableController) CheckPodsRanktableReady(pods []*corev1.Pod, annotationName string) bool {
	if len(pods) == 0 {
		return false
	}

	for _, pod := range pods {
		if pod.Annotations == nil {
			klog.V(3).Infof("Pod %s/%s has no annotations, ranktable not ready", pod.Namespace, pod.Name)
			return false
		}
		annotation := pod.Annotations[annotationName]
		if annotation == "" {
			klog.V(3).Infof("Pod %s/%s does not have ranktable annotation %s yet", pod.Namespace, pod.Name, annotationName)
			return false
		}
	}

	return true
}

// GenerateAndUpdateRanktables generates ranktables and updates ConfigMaps
func (c *RanktableController) GenerateAndUpdateRanktables(
	ctx context.Context,
	ms *workloadv1alpha1.ModelServing,
	template *RanktableTemplate,
	pods []*corev1.Pod,
) error {
	// Group pods by role or group based on template level
	podGroups := c.groupPods(ms, pods, template.Level)
	klog.V(3).Infof("Processing ranktable generation for ModelServing %s/%s, found %d groups",
		ms.Namespace, ms.Name, len(podGroups))

	for groupName, groupPods := range podGroups {
		var ranktableJSON string
		status := "updating"

		klog.V(3).Infof("Checking ranktable readiness for group %s with %d pods", groupName, len(groupPods))

		// Check if pods are ready (have ranktable annotations)
		if c.CheckPodsRanktableReady(groupPods, template.PodAnnotationName) {
			status = "completed"
			klog.V(2).Infof("All pods in group %s are ready with annotations, generating ranktable", groupName)

			// Parse each pod's ranktable annotation
			podRanktables := make([]PodRanktableData, 0, len(groupPods))
			for _, pod := range groupPods {
				annotation := pod.Annotations[template.PodAnnotationName]
				if annotation == "" {
					klog.Warningf("Skipping pod %s/%s without ranktable annotation %s during generation",
						pod.Namespace, pod.Name, template.PodAnnotationName)
					continue
				}

				podData, err := c.templateManager.ParsePodRanktable(template.PodParserTemplate, annotation)
				if err != nil {
					klog.Errorf("Failed to parse ranktable for pod %s/%s: %v", pod.Namespace, pod.Name, err)
					continue
				}
				podRanktables = append(podRanktables, *podData)
			}

			if len(podRanktables) > 0 {
				klog.V(3).Infof("Building ranktable from %d pods for group %s", len(podRanktables), groupName)
				// Build ranktable template data
				templateData := c.templateManager.BuildRanktableTemplateData(status, podRanktables)

				// Render ranktable JSON
				var err error
				ranktableJSON, err = c.templateManager.RenderRanktable(template.RanktableTemplate, templateData)
				if err != nil {
					return fmt.Errorf("failed to render ranktable for group %s: %w", groupName, err)
				}
				klog.V(3).Infof("Successfully rendered ranktable JSON for group %s (size: %d bytes)",
					groupName, len(ranktableJSON))
			} else {
				klog.Warningf("No valid pod ranktables found for group %s after parsing", groupName)
			}
		} else {
			// Not ready, clear content
			ranktableJSON = ""
			klog.V(2).Infof("Pods in group %s are not ready (waiting for annotations), clearing ranktable content", groupName)
		}

		// Update ConfigMap
		cmName := GenerateRanktableConfigMapName(ms.Name, groupName)

		apiVersion := ms.APIVersion
		if apiVersion == "" {
			apiVersion = workloadv1alpha1.SchemeGroupVersion.String()
		}
		kind := ms.Kind
		if kind == "" {
			kind = "ModelServing"
		}

		ownerRef := metav1.OwnerReference{
			APIVersion: apiVersion,
			Kind:       kind,
			Name:       ms.Name,
			UID:        ms.UID,
			Controller: func() *bool { b := true; return &b }(),
		}

		labels := map[string]string{
			workloadv1alpha1.GroupNameLabelKey: groupName,
			"app.kubernetes.io/component":      "ranktable",
			"ranktable-level":                  string(template.Level),
		}

		klog.V(3).Infof("Updating ranktable ConfigMap %s for group %s", cmName, groupName)
		err := c.templateManager.EnsureRanktableConfigMap(
			ctx,
			ms.Namespace,
			cmName,
			[]metav1.OwnerReference{ownerRef},
			labels,
			template.Filename,
			ranktableJSON,
		)
		if err != nil {
			return fmt.Errorf("failed to update ranktable ConfigMap for group %s: %w", groupName, err)
		}

		klog.V(2).Infof("Successfully updated ranktable for group %s with status %s", groupName, status)
	}

	return nil
}

// groupPods groups pods by role or group name based on the ranktable level
func (c *RanktableController) groupPods(
	ms *workloadv1alpha1.ModelServing,
	pods []*corev1.Pod,
	level RanktableLevel,
) map[string][]*corev1.Pod {
	podGroups := make(map[string][]*corev1.Pod)

	for _, pod := range pods {
		var groupKey string
		if level == RoleLevelRanktable {
			// Group by role name
			groupKey = utils.PodRoleName(pod)
		} else {
			// Group by serving group name
			_, groupKey, _ = utils.GetModelServingAndGroupByLabel(pod.Labels)
		}

		if groupKey == "" {
			klog.V(4).Infof("Pod %s/%s has no group key, skipping", pod.Namespace, pod.Name)
			continue
		}

		podGroups[groupKey] = append(podGroups[groupKey], pod)
	}

	return podGroups
}

// GetRanktableConfigMapName returns the ConfigMap name for a pod's ranktable
func (c *RanktableController) GetRanktableConfigMapName(
	ms *workloadv1alpha1.ModelServing,
	pod *corev1.Pod,
	level RanktableLevel,
) string {
	var groupName string
	if level == RoleLevelRanktable {
		groupName = utils.PodRoleName(pod)
	} else {
		_, groupName, _ = utils.GetModelServingAndGroupByLabel(pod.Labels)
	}

	return GenerateRanktableConfigMapName(ms.Name, groupName)
}

// ShouldRequeue determines if reconciliation should be requeued
func (c *RanktableController) ShouldRequeue(pods []*corev1.Pod, annotationName string) (bool, time.Duration) {
	if !c.CheckPodsRanktableReady(pods, annotationName) {
		// Requeue after 5 seconds to check again
		return true, 5 * time.Second
	}
	return false, 0
}
