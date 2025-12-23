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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

const PluginName = "ranktable"

type RanktableConfig struct {
	Template string `json:"template"`
}

type RanktablePlugin struct {
	templateManager *TemplateManager
	cfg             RanktableConfig
}

func init() {
	plugins.DefaultRegistry.Register(PluginName, NewRanktablePlugin)
}

// NewRanktablePlugin creates a new RanktablePlugin.
func NewRanktablePlugin(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
	pluginCfg := RanktableConfig{}
	if err := plugins.DecodeJSON(spec.Config, &pluginCfg); err != nil {
		return nil, fmt.Errorf("failed to decode ranktable plugin config: %w", err)
	}
	if pluginCfg.Template == "" {
		return nil, fmt.Errorf("ranktable template is required in plugin config")
	}

	return &RanktablePlugin{
		templateManager: NewTemplateManager(),
		cfg:             pluginCfg,
	}, nil
}

func (p *RanktablePlugin) Name() string {
	return PluginName
}

func (p *RanktablePlugin) OnPodCreate(ctx context.Context, req *plugins.HookRequest) error {
	ms := req.ModelServing

	template, err := p.templateManager.GetRanktableTemplate(req.ConfigMapLister, p.cfg.Template)
	if err != nil {
		return fmt.Errorf("failed to get ranktable template: %w", err)
	}

	// Override level if specified in annotation (keep this for backward compatibility or override flexibility)
	if levelOverride, ok := ms.Annotations[RanktableLevelAnnotation]; ok {
		level := RanktableLevel(levelOverride)
		if level == RoleLevelRanktable || level == GroupLevelRanktable {
			template.Level = level
		}
	}

	// Determine ConfigMap name based on level
	var cmName string
	cmLabels := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		"app.kubernetes.io/component":             "ranktable",
	}

	if template.Level == RoleLevelRanktable {
		cmName = GenerateRanktableConfigMapName(ms.Name, fmt.Sprintf("%s-%s", req.ServingGroup, req.RoleID))
		cmLabels[workloadv1alpha1.RoleLabelKey] = req.RoleName
		cmLabels[workloadv1alpha1.RoleIDKey] = req.RoleID
		cmLabels[workloadv1alpha1.GroupNameLabelKey] = req.ServingGroup
		cmLabels["ranktable-level"] = string(RoleLevelRanktable)
	} else {
		cmName = GenerateRanktableConfigMapName(ms.Name, req.ServingGroup)
		cmLabels[workloadv1alpha1.GroupNameLabelKey] = req.ServingGroup
		cmLabels["ranktable-level"] = string(GroupLevelRanktable)
	}

	// Ensure ConfigMap exists (create empty if needed)
	// We use ControllerRef to ensure garbage collection
	ownerRef := *metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))

	// Create initial ranktable content
	templateData := p.templateManager.BuildRanktableTemplateData(RanktableStatusInitializing, nil)
	ranktableJSON, err := p.templateManager.RenderRanktable(template.RanktableTemplate, templateData)
	if err != nil {
		return fmt.Errorf("failed to render initial ranktable: %w", err)
	}

	if err := p.templateManager.EnsureRanktableConfigMap(ctx, req.KubeClient, ms.Namespace, cmName, []metav1.OwnerReference{ownerRef}, cmLabels, template.Filename, ranktableJSON); err != nil {
		return err
	}

	// Inject Mount
	volumeName := "ranktable"
	volume := corev1.Volume{
		Name: volumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cmName,
				},
			},
		},
	}
	req.Pod.Spec.Volumes = append(req.Pod.Spec.Volumes, volume)

	volumeMount := corev1.VolumeMount{
		Name:      volumeName,
		MountPath: template.MountPath,
		ReadOnly:  true,
	}

	// Mount to all containers
	for i := range req.Pod.Spec.Containers {
		req.Pod.Spec.Containers[i].VolumeMounts = append(req.Pod.Spec.Containers[i].VolumeMounts, volumeMount)
	}
	// Mount to all init containers
	for i := range req.Pod.Spec.InitContainers {
		req.Pod.Spec.InitContainers[i].VolumeMounts = append(req.Pod.Spec.InitContainers[i].VolumeMounts, volumeMount)
	}

	return nil
}

func (p *RanktablePlugin) OnPodReady(ctx context.Context, req *plugins.HookRequest) error {
	ms := req.ModelServing

	template, err := p.templateManager.GetRanktableTemplate(req.ConfigMapLister, p.cfg.Template)
	if err != nil {
		return fmt.Errorf("failed to get ranktable template: %w", err)
	}

	// Override level if specified in annotation
	if levelOverride, ok := ms.Annotations[RanktableLevelAnnotation]; ok {
		level := RanktableLevel(levelOverride)
		if level == RoleLevelRanktable || level == GroupLevelRanktable {
			template.Level = level
		}
	}

	// List pods for the scope to check readiness
	labelSet := labels.Set{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        req.ServingGroup,
	}

	if template.Level == RoleLevelRanktable {
		labelSet[workloadv1alpha1.RoleIDKey] = req.RoleID
	}

	pods, err := req.PodLister.Pods(ms.Namespace).List(labelSet.AsSelector())
	if err != nil {
		return err
	}

	klog.V(4).Infof("Found %d pods for ranktable generation (Level: %s, RoleID: %s)", len(pods), template.Level, req.RoleID)

	// Check readiness and collect data
	allReady := true
	var podRanktables []PodRanktableData
	activePods := 0

	for _, pod := range pods {
		isRunning := pod.Status.Phase == corev1.PodRunning
		klog.V(4).Infof("Checking pod %s: Phase=%s, DeletionTimestamp=%v, Running=%v", pod.Name, pod.Status.Phase, pod.DeletionTimestamp, isRunning)

		// Skip pods that are deleting
		if pod.DeletionTimestamp != nil {
			continue
		}
		activePods++

		// Check if pod is running
		if !isRunning {
			allReady = false
			klog.V(4).Infof("Pod %s/%s is not running, waiting for ranktable", pod.Namespace, pod.Name)
			continue
		}

		ann, ok := pod.Annotations[template.PodAnnotationName]
		if !ok {
			allReady = false
			klog.V(4).Infof("Pod %s/%s missing ranktable annotation %s", pod.Namespace, pod.Name, template.PodAnnotationName)
			continue
		}

		// Parse
		data, err := p.templateManager.ParsePodRanktable(template.PodParserTemplate, ann)
		if err != nil {
			klog.Errorf("Failed to parse annotation for pod %s: %v", pod.Name, err)
			allReady = false
			continue
		}
		podRanktables = append(podRanktables, *data)
	}

	status := RanktableStatusCompleted
	if !allReady || len(podRanktables) == 0 {
		status = RanktableStatusInitializing
		podRanktables = nil // Force empty data if not ready
		klog.V(4).Infof("Ranktable status set to Initializing. allReady: %v, podRanktables count: %d", allReady, len(podRanktables))
	} else {
		// Double check if we have enough pods
		if template.Level == RoleLevelRanktable {
			var roleReplicas int32
			found := false
			for _, r := range ms.Spec.Template.Roles {
				if r.Name == req.RoleName {
					// For a specific RoleID (Role Instance), expected count is 1 (entry) + WorkerReplicas
					roleReplicas = 1 + r.WorkerReplicas
					found = true
					break
				}
			}
			klog.V(4).Infof("Role %s/%s check: activePods=%d, roleReplicas=%d, foundRole=%v", ms.Name, req.RoleID, activePods, roleReplicas, found)
			if found && int32(activePods) < roleReplicas {
				allReady = false
				status = RanktableStatusInitializing
				podRanktables = nil
				klog.V(4).Infof("Role %s/%s (Group: %s) has %d active pods, expected %d. Setting status to Initializing.", ms.Name, req.RoleID, req.ServingGroup, activePods, roleReplicas)
			}
		}
	}

	templateData := p.templateManager.BuildRanktableTemplateData(status, podRanktables)
	ranktableJSON, err := p.templateManager.RenderRanktable(template.RanktableTemplate, templateData)
	if err != nil {
		return fmt.Errorf("failed to render ranktable: %w", err)
	}

	// Update ConfigMap
	var cmName string
	cmLabels := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		"app.kubernetes.io/component":             "ranktable",
	}

	if template.Level == RoleLevelRanktable {
		cmName = GenerateRanktableConfigMapName(ms.Name, fmt.Sprintf("%s-%s", req.ServingGroup, req.RoleID))
		cmLabels[workloadv1alpha1.RoleLabelKey] = req.RoleName
		cmLabels[workloadv1alpha1.RoleIDKey] = req.RoleID
		cmLabels[workloadv1alpha1.GroupNameLabelKey] = req.ServingGroup
		cmLabels["ranktable-level"] = string(RoleLevelRanktable)
	} else {
		cmName = GenerateRanktableConfigMapName(ms.Name, req.ServingGroup)
		cmLabels[workloadv1alpha1.GroupNameLabelKey] = req.ServingGroup
		cmLabels["ranktable-level"] = string(GroupLevelRanktable)
	}

	ownerRef := *metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))

	return p.templateManager.EnsureRanktableConfigMap(ctx, req.KubeClient, ms.Namespace, cmName, []metav1.OwnerReference{ownerRef}, cmLabels, template.Filename, ranktableJSON)
}

func (p *RanktablePlugin) OnRoleDelete(ctx context.Context, req *plugins.HookRequest) error {
	ms := req.ModelServing

	template, err := p.templateManager.GetRanktableTemplate(req.ConfigMapLister, p.cfg.Template)
	if err != nil {
		return fmt.Errorf("failed to get ranktable template: %w", err)
	}

	// Override level if specified in annotation
	if levelOverride, ok := ms.Annotations[RanktableLevelAnnotation]; ok {
		level := RanktableLevel(levelOverride)
		if level == RoleLevelRanktable || level == GroupLevelRanktable {
			template.Level = level
		}
	}

	if template.Level == RoleLevelRanktable {
		cmName := GenerateRanktableConfigMapName(ms.Name, fmt.Sprintf("%s-%s", req.ServingGroup, req.RoleID))
		if err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).Delete(ctx, cmName, metav1.DeleteOptions{}); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete ranktable configmap %s: %w", cmName, err)
			}
		}
		klog.V(2).Infof("Deleted ranktable ConfigMap %s/%s", ms.Namespace, cmName)
	}

	return nil
}

func (p *RanktablePlugin) OnServingGroupDelete(ctx context.Context, req *plugins.HookRequest) error {
	ms := req.ModelServing

	// Delete all ranktable ConfigMaps for this group using labels.
	// This covers both group-level and role-level ranktables associated with this group.
	selector := labels.SelectorFromSet(map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        req.ServingGroup,
		"app.kubernetes.io/component":             "ranktable",
	})

	// List ConfigMaps first because DeleteCollection is often restricted in RBAC (delete vs deletecollection)
	cms, err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return fmt.Errorf("failed to list ranktable configmaps for group %s: %w", req.ServingGroup, err)
	}

	for _, cm := range cms.Items {
		if err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}); err != nil {
			if !apierrors.IsNotFound(err) {
				klog.Errorf("failed to delete ranktable configmap %s/%s: %v", ms.Namespace, cm.Name, err)
			}
		} else {
			klog.V(2).Infof("Deleted ranktable ConfigMap %s/%s", ms.Namespace, cm.Name)
		}
	}

	return nil
}
