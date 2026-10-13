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

package podranktable

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins/ranktableutil"
)

const PluginName = "pod-ranktable"

type RanktableConfig struct {
	Template string `json:"template"`
}

type PodRanktablePlugin struct {
	templateManager *TemplateManager
	cfg             RanktableConfig
}

func init() {
	plugins.DefaultRegistry.Register(PluginName, NewPodRanktablePlugin)
}

// NewPodRanktablePlugin creates a new PodRanktablePlugin.
func NewPodRanktablePlugin(spec workloadv1alpha1.PluginSpec) (plugins.Plugin, error) {
	pluginCfg := RanktableConfig{}
	if err := plugins.DecodeJSON(spec.Config, &pluginCfg); err != nil {
		return nil, fmt.Errorf("failed to decode pod-ranktable plugin config: %w", err)
	}
	if pluginCfg.Template == "" {
		return nil, fmt.Errorf("pod-ranktable template is required in plugin config")
	}

	return &PodRanktablePlugin{
		templateManager: NewTemplateManager(),
		cfg:             pluginCfg,
	}, nil
}

func (p *PodRanktablePlugin) Name() string {
	return PluginName
}

func (p *PodRanktablePlugin) ranktableTemplate(req *plugins.HookRequest) (*RanktableTemplate, error) {
	template, err := p.templateManager.GetRanktableTemplate(req.ConfigMapLister, p.cfg.Template)
	if err != nil {
		return nil, fmt.Errorf("failed to get ranktable template: %w", err)
	}
	if override := RanktableLevel(req.ModelServing.Annotations[RanktableLevelAnnotation]); override == RoleLevelRanktable || override == GroupLevelRanktable {
		template.Level = override
	}
	return template, nil
}

func (p *PodRanktablePlugin) OnPodCreate(ctx context.Context, req *plugins.HookRequest) error {
	template, err := p.ranktableTemplate(req)
	if err != nil {
		return err
	}
	// Device annotations may be added after this pre-API hook. Prepare and
	// mount now; annotation-based membership is resolved when syncing data.
	scope := ranktableutil.NewScope(req, template.Level == RoleLevelRanktable, template.Filename)
	if err := p.syncRanktable(ctx, req, template, ranktableutil.Prepare); err != nil {
		return err
	}
	p.injectRanktableMount(req.Pod, template, scope.Name)
	return nil
}

func (p *PodRanktablePlugin) OnPodRunning(ctx context.Context, req *plugins.HookRequest) error {
	return p.updateRanktableFromPods(ctx, req, ranktableutil.Refresh)
}

func (p *PodRanktablePlugin) OnPodReady(context.Context, *plugins.HookRequest) error {
	return nil
}

func (p *PodRanktablePlugin) OnPodDelete(ctx context.Context, req *plugins.HookRequest) error {
	return p.updateRanktableFromPods(ctx, req, ranktableutil.UpdateExisting)
}

func (p *PodRanktablePlugin) OnRoleSync(ctx context.Context, req *plugins.HookRequest) error {
	return p.updateRanktableFromPods(ctx, req, ranktableutil.Refresh)
}

func (p *PodRanktablePlugin) updateRanktableFromPods(ctx context.Context, req *plugins.HookRequest, mode ranktableutil.Mode) error {
	template, err := p.ranktableTemplate(req)
	if err != nil {
		return err
	}
	return p.syncRanktable(ctx, req, template, mode)
}

func (p *PodRanktablePlugin) syncRanktable(ctx context.Context, req *plugins.HookRequest, template *RanktableTemplate, mode ranktableutil.Mode) error {
	scope := ranktableutil.NewScope(req, template.Level == RoleLevelRanktable, template.Filename)
	return ranktableutil.Sync(ctx, req, scope, mode, template.PodAnnotationName, func(complete bool, pods []*corev1.Pod, timestamp string) (string, error) {
		var members []PodRanktableData
		for _, pod := range pods {

			if pod.Status.PodIP == "" {
				complete = false
				continue
			}
			annotation := pod.Annotations[template.PodAnnotationName]
			data, err := p.templateManager.ParsePodRanktable(template.PodParserTemplate, annotation, pod.Status.PodIP, pod.Name)
			if err != nil {
				klog.ErrorS(err, "Cannot parse ranktable member", "pod", pod.Name)
				complete = false
				continue
			}
			members = append(members, *data)
		}
		status := RanktableStatusCompleted
		if !complete {
			status = RanktableStatusInitializing
			members = nil
		}
		data := p.templateManager.BuildRanktableTemplateData(status, members)
		data.Timestamp = timestamp
		return p.templateManager.RenderRanktable(template.RanktableTemplate, data)
	})
}

func (p *PodRanktablePlugin) OnRoleDelete(ctx context.Context, req *plugins.HookRequest) error {
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
		cm, err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, cmName, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !metav1.IsControlledBy(cm, ms) {
			return nil
		}
		if err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).Delete(ctx, cmName, *metav1.NewPreconditionDeleteOptions(string(cm.UID))); err != nil {
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("failed to delete ranktable configmap %s: %w", cmName, err)
			}
		}
		klog.V(2).Infof("Deleted ranktable ConfigMap %s/%s", ms.Namespace, cmName)
	} else if template.Level == GroupLevelRanktable {
		// When level is group and a role is deleted, update the group-level ranktable
		// to reflect the removal of pods from that role
		klog.V(4).Infof("OnRoleDelete: Updating group-level ranktable after role %s/%s deletion", req.RoleName, req.RoleID)
		return p.updateRanktableFromPods(ctx, req, ranktableutil.UpdateExisting)
	}

	return nil
}

func (p *PodRanktablePlugin) OnServingGroupDelete(ctx context.Context, req *plugins.HookRequest) error {
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

	var deleteErr error
	for _, cm := range cms.Items {
		if !metav1.IsControlledBy(&cm, ms) {
			continue
		}
		if err := req.KubeClient.CoreV1().ConfigMaps(ms.Namespace).Delete(ctx, cm.Name, *metav1.NewPreconditionDeleteOptions(string(cm.UID))); err != nil {
			if !apierrors.IsNotFound(err) {
				deleteErr = errors.Join(deleteErr, fmt.Errorf("failed to delete ranktable configmap %s/%s: %w", ms.Namespace, cm.Name, err))
			}
		} else {
			klog.V(2).Infof("Deleted ranktable ConfigMap %s/%s", ms.Namespace, cm.Name)
		}
	}

	return deleteErr
}

func (p *PodRanktablePlugin) injectRanktableMount(pod *corev1.Pod, template *RanktableTemplate, cmName string) {
	klog.V(4).Infof("Injecting ranktable template %s/%s, comfigmap %s with mountpath %s", pod.Namespace, pod.Name, cmName, template.MountPath)
	// Check if volume already exists
	volumeExists := false
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == VolumeName {
			volumeExists = true
			break
		}
	}
	if !volumeExists {
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{
			Name: VolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cmName,
					},
				},
			},
		})
	}

	mount := corev1.VolumeMount{
		Name:      VolumeName,
		MountPath: template.MountPath,
		ReadOnly:  true,
	}

	// Helper function to check if mount exists
	hasMount := func(mounts []corev1.VolumeMount, name string) bool {
		for _, m := range mounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}

	// Add VolumeMount to main containers
	for i := range pod.Spec.Containers {
		if !hasMount(pod.Spec.Containers[i].VolumeMounts, VolumeName) {
			pod.Spec.Containers[i].VolumeMounts = append(pod.Spec.Containers[i].VolumeMounts, mount)
		}
	}

	// Add VolumeMount to main containers
	for i := range pod.Spec.InitContainers {
		if !hasMount(pod.Spec.InitContainers[i].VolumeMounts, VolumeName) {
			pod.Spec.InitContainers[i].VolumeMounts = append(pod.Spec.InitContainers[i].VolumeMounts, mount)
		}
	}
}
