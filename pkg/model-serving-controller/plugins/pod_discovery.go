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

package plugins

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	PodDiscoveryPluginName = "pod-discovery"
	ConfigMapNameSuffix    = "-groupinfo"
	DiscoveryVolumeName    = "groupinfo-volume"
	DiscoveryFileName      = "groupinfo.config"
)

func init() {
	DefaultRegistry.Register(PodDiscoveryPluginName, NewPodDiscoveryPlugin)
}

// PodDiscoveryPlugin aggregates Pod IPs of each ServingGroup into a ConfigMap.
type PodDiscoveryConfig struct {
	AutoMountPath string `json:"autoMountPath,omitempty"`
}

type PodDiscoveryPlugin struct {
	name   string
	config PodDiscoveryConfig
}

func (p *PodDiscoveryPlugin) OnRoleSync(_ context.Context, _ *HookRequest) error {
	return nil
}

func NewPodDiscoveryPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	p := &PodDiscoveryPlugin{name: spec.Name}
	if spec.Config != nil {
		if err := DecodeJSON(spec.Config, &p.config); err != nil {
			return nil, fmt.Errorf("failed to decode pod-discovery config: %w", err)
		}
	}
	return p, nil
}

func (p *PodDiscoveryPlugin) Name() string {
	return p.name
}

// GroupInfo represents the group information stored in ConfigMap.
type GroupInfo struct {
	Group        string     `json:"group,omitempty"`
	RoleInfoList []RoleInfo `json:"roles,omitempty"`
}

// RoleInfo represents the role information.
type RoleInfo struct {
	Role             string             `json:"role,omitempty"`
	RoleInstanceList []RoleInstanceInfo `json:"roleInstanceList,omitempty"`
}

// RoleInstanceInfo represents the role instance information.
type RoleInstanceInfo struct {
	RoleInstanceName string    `json:"roleInstanceName,omitempty"`
	PodInfoList      []PodInfo `json:"podList,omitempty"`
}

// PodInfo represents the pod information.
type PodInfo struct {
	PodName string `json:"podName,omitempty"`
	PodIP   string `json:"podIP,omitempty"`
	NodeIP  string `json:"nodeIP,omitempty"`
}

// hasVolume checks if a volume with the given name already exists in a pod spec
func hasVolume(podSpec corev1.PodSpec, volumeName string) bool {
	for _, vol := range podSpec.Volumes {
		if vol.Name == volumeName {
			return true
		}
	}
	return false
}

// hasVolumeMount checks if a volume mount with the given name already exists in a container
func hasVolumeMount(container corev1.Container, volumeName string) bool {
	for _, mount := range container.VolumeMounts {
		if mount.Name == volumeName {
			return true
		}
	}
	return false
}

// addVolumeMountToContainer adds a volume mount to a container if it doesn't already exist
// Returns true if the mount was added, false if it already existed
func addVolumeMountToContainer(container *corev1.Container, mount corev1.VolumeMount) bool {
	if hasVolumeMount(*container, mount.Name) {
		return false
	}
	container.VolumeMounts = append(container.VolumeMounts, mount)
	return true
}

func (p *PodDiscoveryPlugin) OnPodCreate(ctx context.Context, req *HookRequest) error {
	if p.config.AutoMountPath == "" {
		klog.V(4).Infof("PodDiscoveryPlugin: autoMountPath not configured, skipping mount for pod %s", req.Pod.Name)
		return nil
	}
	podRevision := utils.ObjectRevision(req.Pod)
	targetRevision := req.ModelServing.Status.UpdateRevision
	if targetRevision == "" {
		targetRevision = podRevision
		klog.V(4).Infof("PodDiscoveryPlugin: UpdateRevision is empty, using pod revision %s as target", podRevision)
	}
	if podRevision != targetRevision {
		klog.V(4).Infof("PodDiscoveryPlugin: Skipping mount for pod %s - pod revision %s does not match target revision %s",
			req.Pod.Name, podRevision, targetRevision)
		return nil
	}
	cmName := req.ServingGroup + ConfigMapNameSuffix
	klog.V(4).Infof("PodDiscoveryPlugin: Starting mount configuration for pod %s in namespace %s, ServingGroup %s",
		req.Pod.Name, req.Pod.Namespace, req.ServingGroup)
	if !hasVolume(req.Pod.Spec, DiscoveryVolumeName) {
		volume := corev1.Volume{
			Name: DiscoveryVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: cmName,
					},
					Optional: func(b bool) *bool { return &b }(true),
				},
			},
		}
		req.Pod.Spec.Volumes = append(req.Pod.Spec.Volumes, volume)
		klog.V(4).Infof("PodDiscoveryPlugin: Added volume %s referencing ConfigMap %s to pod %s",
			DiscoveryVolumeName, cmName, req.Pod.Name)
	}
	mount := corev1.VolumeMount{
		Name:      DiscoveryVolumeName,
		MountPath: p.config.AutoMountPath,
		ReadOnly:  true,
	}
	containerCount := 0
	for i := range req.Pod.Spec.Containers {
		if addVolumeMountToContainer(&req.Pod.Spec.Containers[i], mount) {
			klog.V(4).Infof("PodDiscoveryPlugin: Added volume mount to container %s in pod %s (path: %s, readonly: %v)",
				req.Pod.Spec.Containers[i].Name, req.Pod.Name, mount.MountPath, mount.ReadOnly)
			containerCount++
		} else {
			klog.V(4).Infof("PodDiscoveryPlugin: Volume mount already exists in container %s in pod %s, skipping",
				req.Pod.Spec.Containers[i].Name, req.Pod.Name)
		}
	}
	initContainerCount := 0
	for i := range req.Pod.Spec.InitContainers {
		if addVolumeMountToContainer(&req.Pod.Spec.InitContainers[i], mount) {
			klog.V(4).Infof("PodDiscoveryPlugin: Added volume mount to init container %s in pod %s (path: %s, readonly: %v)",
				req.Pod.Spec.InitContainers[i].Name, req.Pod.Name, mount.MountPath, mount.ReadOnly)
			initContainerCount++
		} else {
			klog.V(4).Infof("PodDiscoveryPlugin: Volume mount already exists in init container %s in pod %s, skipping",
				req.Pod.Spec.InitContainers[i].Name, req.Pod.Name)
		}
	}
	klog.V(4).Infof("PodDiscoveryPlugin: Completed mount configuration for pod %s - %d containers and %d init containers mounted",
		req.Pod.Name, containerCount, initContainerCount)
	return nil
}

func (p *PodDiscoveryPlugin) OnPodRunning(ctx context.Context, req *HookRequest) error {
	return p.syncConfigMap(ctx, req, false)
}

// syncConfigMap keeps the production Running-hook behavior and revision gate. The independent
// maintenance entry additionally refuses to take over another owner's output.
func (p *PodDiscoveryPlugin) syncConfigMap(ctx context.Context, req *HookRequest, requireOwner bool) error {
	if requireOwner && (req.ModelServing == nil || req.ModelServing.UID == "") {
		return fmt.Errorf("cannot maintain discovery ConfigMap without a ModelServing UID")
	}
	if req.KubeClient == nil {
		klog.Warningf("KubeClient is nil in HookRequest for plugin %s, skipping OnPodRunning", p.Name())
		return nil
	}
	// Only apply to new groups (where pod revision equals UpdateRevision)
	if req.ModelServing.Status.UpdateRevision == "" {
		// No update revision yet, skip
		klog.V(4).Infof("PodDiscoveryPlugin: Skipping ConfigMap update for pod %s - UpdateRevision is empty", req.Pod.Name)
		return nil
	}
	podRevision := utils.ObjectRevision(req.Pod)
	if podRevision != req.ModelServing.Status.UpdateRevision {
		// This is an old group, skip
		klog.V(4).Infof("PodDiscoveryPlugin: Skipping ConfigMap update for pod %s - pod revision %s does not match UpdateRevision %s",
			req.Pod.Name, podRevision, req.ModelServing.Status.UpdateRevision)
		return nil
	}
	namespace := req.ModelServing.Namespace
	servingGroupName := req.ServingGroup
	cmName := servingGroupName + ConfigMapNameSuffix
	klog.V(4).Infof("PodDiscoveryPlugin: Starting ConfigMap update for pod %s in ServingGroup %s", req.Pod.Name, servingGroupName)
	// 使用 PollUntilContextTimeout 替换 PollImmediate
	// 100ms: interval, 5s: timeout, true: immediate (立即执行第一次检测)
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		// 1. List all pods in the ServingGroup
		pods, err := req.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", workloadv1alpha1.GroupNameLabelKey, servingGroupName),
		})
		if err != nil {
			return false, err
		}
		// 2. Build GroupInfo structure
		groupInfo := GroupInfo{
			Group: servingGroupName,
		}
		// Map to store pods by role and roleID (statefulSet)
		// role -> roleID -> []PodInfo
		roleStatefulSetPods := make(map[string]map[string][]PodInfo)
		for _, pod := range pods.Items {
			// Check if pod is ready and has an IP
			if pod.Status.PodIP == "" {
				continue
			}
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}
			roleName := utils.GetRoleName(&pod)
			if roleName == "" {
				continue
			}
			roleID := utils.GetRoleID(&pod)
			if roleID == "" {
				continue
			}
			// Initialize map for this role if not exists
			if roleStatefulSetPods[roleName] == nil {
				roleStatefulSetPods[roleName] = make(map[string][]PodInfo)
			}
			// Add pod info to the map
			podInfo := PodInfo{
				PodName: pod.Name,
				PodIP:   pod.Status.PodIP,
				NodeIP:  pod.Status.HostIP,
			}
			roleStatefulSetPods[roleName][roleID] = append(roleStatefulSetPods[roleName][roleID], podInfo)
		}
		// 3. Build RoleInfoList from the map
		for roleName, statefulSetMap := range roleStatefulSetPods {
			roleInfo := RoleInfo{
				Role: roleName,
			}
			// Sort statefulSet names for deterministic output
			statefulSetNames := make([]string, 0, len(statefulSetMap))
			for statefulSetName := range statefulSetMap {
				statefulSetNames = append(statefulSetNames, statefulSetName)
			}
			sort.Strings(statefulSetNames)
			// Build RoleInstanceList
			for _, statefulSetName := range statefulSetNames {
				podList := statefulSetMap[statefulSetName]
				// Sort pods by name for deterministic output
				sort.Slice(podList, func(i, j int) bool {
					return podList[i].PodName < podList[j].PodName
				})
				roleInstanceInfo := RoleInstanceInfo{
					RoleInstanceName: statefulSetName,
					PodInfoList:      podList,
				}
				roleInfo.RoleInstanceList = append(roleInfo.RoleInstanceList, roleInstanceInfo)
			}
			groupInfo.RoleInfoList = append(groupInfo.RoleInfoList, roleInfo)
		}
		// Sort roles by name for deterministic output
		sort.Slice(groupInfo.RoleInfoList, func(i, j int) bool {
			return groupInfo.RoleInfoList[i].Role < groupInfo.RoleInfoList[j].Role
		})
		jsonData, err := json.Marshal(groupInfo)
		if err != nil {
			return false, fmt.Errorf("failed to marshal group info: %v", err)
		}
		jsonString := string(jsonData)
		// 4. Get or Create ConfigMap
		cm, err := req.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				// Create
				newCM := &corev1.ConfigMap{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cmName,
						Namespace: namespace,
						Labels: map[string]string{
							workloadv1alpha1.GroupNameLabelKey: servingGroupName,
							"app.kubernetes.io/managed-by":     "model-serving-controller",
						},
						OwnerReferences: []metav1.OwnerReference{
							*metav1.NewControllerRef(req.ModelServing, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing")),
						},
					},
					Data: map[string]string{
						DiscoveryFileName: jsonString,
					},
				}
				_, err = req.KubeClient.CoreV1().ConfigMaps(namespace).Create(ctx, newCM, metav1.CreateOptions{})
				if err != nil {
					if errors.IsAlreadyExists(err) {
						// Race condition: created by another thread/pod, retry to update
						return false, nil
					}
					return false, err
				}
				klog.V(4).InfoS("Created groupinfo ConfigMap", "modelServing", klog.KObj(req.ModelServing), "servingGroup", servingGroupName, "configMap", cmName)
				return true, nil
			}
			return false, err
		}

		// Check the actual object on every attempt, including after AlreadyExists
		// or Conflict. Update retains this GET's UID and resourceVersion so a
		// same-name replacement cannot receive an update intended for the old CM.
		if requireOwner && !metav1.IsControlledBy(cm, req.ModelServing) {
			return false, fmt.Errorf("discovery ConfigMap %s/%s is not controlled by ModelServing UID %s", namespace, cmName, req.ModelServing.UID)
		}

		// 5. Update if changed
		if cm.Data == nil {
			cm.Data = make(map[string]string)
		}
		if cm.Data[DiscoveryFileName] == jsonString {
			// No change
			return true, nil
		}
		cm.Data[DiscoveryFileName] = jsonString
		_, err = req.KubeClient.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			if errors.IsConflict(err) {
				// Optimistic locking conflict, retry
				return false, nil
			}
			return false, err
		}
		klog.V(4).InfoS("Updated groupinfo ConfigMap", "modelServing", klog.KObj(req.ModelServing), "servingGroup", servingGroupName, "configMap", cmName)
		return true, nil
	})
}

func (p *PodDiscoveryPlugin) OnPodReady(_ context.Context, _ *HookRequest) error {
	return nil
}

func (p *PodDiscoveryPlugin) OnPodDelete(ctx context.Context, req *HookRequest) error {
	if req.KubeClient == nil {
		klog.Warningf("PodDiscoveryPlugin: KubeClient is nil in HookRequest, skipping OnPodDelete")
		return nil
	}
	namespace := req.ModelServing.Namespace
	servingGroupName := req.ServingGroup
	cmName := servingGroupName + ConfigMapNameSuffix
	klog.V(4).Infof("PodDiscoveryPlugin: Starting ConfigMap update for pod deletion %s in ServingGroup %s", req.Pod.Name, servingGroupName)
	// 使用 PollUntilContextTimeout 替换 PollImmediate
	// 100ms: interval, 5s: timeout, true: immediate (立即执行第一次检测)
	return wait.PollUntilContextTimeout(ctx, 100*time.Millisecond, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		cm, err := req.KubeClient.CoreV1().ConfigMaps(namespace).Get(ctx, cmName, metav1.GetOptions{})
		if err != nil {
			if errors.IsNotFound(err) {
				klog.V(4).Infof("PodDiscoveryPlugin: ConfigMap %s not found, nothing to remove", cmName)
				return true, nil
			}
			return false, err
		}
		jsonStr := cm.Data[DiscoveryFileName]
		if jsonStr == "" {
			klog.V(4).Infof("PodDiscoveryPlugin: ConfigMap %s has no data, nothing to remove", cmName)
			return true, nil
		}
		var groupInfo GroupInfo
		if err := json.Unmarshal([]byte(jsonStr), &groupInfo); err != nil {
			klog.Errorf("PodDiscoveryPlugin: Failed to unmarshal ConfigMap data: %v", err)
			return true, nil
		}
		deletedPodName := req.Pod.Name
		found := false
		for i := range groupInfo.RoleInfoList {
			roleInfo := &groupInfo.RoleInfoList[i]
			for j := range roleInfo.RoleInstanceList {
				ri := &roleInfo.RoleInstanceList[j]
				for k := len(ri.PodInfoList) - 1; k >= 0; k-- {
					if ri.PodInfoList[k].PodName == deletedPodName {
						ri.PodInfoList = append(ri.PodInfoList[:k], ri.PodInfoList[k+1:]...)
						found = true
					}
				}
			}
		}
		if !found {
			klog.V(4).Infof("PodDiscoveryPlugin: Pod %s not found in ConfigMap %s, nothing to remove", deletedPodName, cmName)
			return true, nil
		}
		newData, err := json.Marshal(groupInfo)
		if err != nil {
			return false, fmt.Errorf("failed to marshal group info: %v", err)
		}
		cm.Data[DiscoveryFileName] = string(newData)
		_, err = req.KubeClient.CoreV1().ConfigMaps(namespace).Update(ctx, cm, metav1.UpdateOptions{})
		if err != nil {
			if errors.IsConflict(err) {
				// 乐观锁冲突，重试
				return false, nil
			}
			return false, err
		}
		klog.V(4).Infof("PodDiscoveryPlugin: Removed pod %s from ConfigMap %s", deletedPodName, cmName)
		return true, nil
	})
}

func (p *PodDiscoveryPlugin) OnRoleDelete(_ context.Context, _ *HookRequest) error {
	return nil
}

func (p *PodDiscoveryPlugin) OnServingGroupDelete(ctx context.Context, req *HookRequest) error {
	if req.KubeClient == nil {
		klog.Warningf("PodDiscoveryPlugin: KubeClient is nil in HookRequest, skipping OnServingGroupDelete")
		return nil
	}
	namespace := req.ModelServing.Namespace
	cmName := req.ServingGroup + ConfigMapNameSuffix
	err := req.KubeClient.CoreV1().ConfigMaps(namespace).Delete(ctx, cmName, metav1.DeleteOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			klog.V(4).Infof("PodDiscoveryPlugin: ConfigMap %s not found, nothing to delete", cmName)
			return nil
		}
		return fmt.Errorf("failed to delete ConfigMap %s: %w", cmName, err)
	}
	klog.V(4).Infof("PodDiscoveryPlugin: Deleted ConfigMap %s for ServingGroup %s", cmName, req.ServingGroup)
	return nil
}
