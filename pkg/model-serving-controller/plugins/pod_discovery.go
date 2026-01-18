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
	// PodDiscoveryPluginName is the name of the pod discovery plugin.
	PodDiscoveryPluginName = "pod-discovery"
	// ConfigMapNameSuffix is the suffix for the discovery ConfigMap name.
	ConfigMapNameSuffix = "-discovery"
	// DiscoveryVolumeName is the name of the volume mounted to the pod.
	DiscoveryVolumeName = "pod-discovery-volume"
	// DiscoveryMountPath is the path where the ConfigMap is mounted.
	DiscoveryMountPath = "/etc/pod-discovery"
	// DiscoveryFileName is the key in the ConfigMap and the file name in the volume.
	DiscoveryFileName = "ips.json"
)

func init() {
	DefaultRegistry.Register(PodDiscoveryPluginName, NewPodDiscoveryPlugin)
}

// PodDiscoveryPlugin aggregates Pod IPs of each ServingGroup into a ConfigMap.
type PodDiscoveryPlugin struct {
	name string
}

// NewPodDiscoveryPlugin creates a new PodDiscoveryPlugin.
func NewPodDiscoveryPlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	return &PodDiscoveryPlugin{name: spec.Name}, nil
}

func (p *PodDiscoveryPlugin) Name() string {
	return p.name
}

func (p *PodDiscoveryPlugin) OnPodCreate(ctx context.Context, req *HookRequest) error {
	cmName := req.ServingGroup + ConfigMapNameSuffix

	// Add Volume
	volume := corev1.Volume{
		Name: DiscoveryVolumeName,
		VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{
					Name: cmName,
				},
				// Optional: true, because the ConfigMap might not exist when the first pod is created.
				// However, if we want strict ordering, it might be better to create it beforehand.
				// But plugins currently only hook into Pod lifecycle.
				// Making it optional prevents Pod creation failure if CM is missing.
				Optional: func(b bool) *bool { return &b }(true),
			},
		},
	}
	req.Pod.Spec.Volumes = append(req.Pod.Spec.Volumes, volume)

	// Add VolumeMount to all containers
	mount := corev1.VolumeMount{
		Name:      DiscoveryVolumeName,
		MountPath: DiscoveryMountPath,
		ReadOnly:  true,
	}

	for i := range req.Pod.Spec.Containers {
		req.Pod.Spec.Containers[i].VolumeMounts = append(req.Pod.Spec.Containers[i].VolumeMounts, mount)
	}
	for i := range req.Pod.Spec.InitContainers {
		req.Pod.Spec.InitContainers[i].VolumeMounts = append(req.Pod.Spec.InitContainers[i].VolumeMounts, mount)
	}

	return nil
}

func (p *PodDiscoveryPlugin) OnPodReady(ctx context.Context, req *HookRequest) error {
	if req.KubeClient == nil {
		klog.Warningf("KubeClient is nil in HookRequest for plugin %s, skipping OnPodReady", p.Name())
		return nil
	}

	namespace := req.ModelServing.Namespace
	servingGroupName := req.ServingGroup
	cmName := servingGroupName + ConfigMapNameSuffix

	// Retry loop for updating ConfigMap to handle conflicts
	return wait.PollImmediate(100*time.Millisecond, 5*time.Second, func() (bool, error) {
		// 1. List all pods in the ServingGroup
		pods, err := req.KubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
			LabelSelector: fmt.Sprintf("%s=%s", workloadv1alpha1.GroupNameLabelKey, servingGroupName),
		})
		if err != nil {
			klog.Errorf("Failed to list pods for ServingGroup %s: %v", servingGroupName, err)
			return false, err
		}

		// 2. Group IPs by Role
		// Structure: { "role1": ["ip1", "ip2"], "role2": ["ip3"] }
		roleIPs := make(map[string][]string)
		
		for _, pod := range pods.Items {
			// Check if pod is ready and has an IP
			if pod.Status.PodIP == "" {
				continue
			}
			
			// Simple check for Running/Ready. 
			// You might want stricter checks (e.g., check Ready condition), 
			// but PodIP implies network connectivity usually. 
			// Let's stick to PodIP existence + Running phase for now.
			if pod.Status.Phase != corev1.PodRunning {
				continue
			}

			roleName := utils.PodRoleName(&pod)
			if roleName == "" {
				continue
			}
			roleIPs[roleName] = append(roleIPs[roleName], pod.Status.PodIP)
		}

		// Sort IPs for deterministic output
		for _, ips := range roleIPs {
			sort.Strings(ips)
		}

		jsonData, err := json.Marshal(roleIPs)
		if err != nil {
			return false, fmt.Errorf("failed to marshal pod IPs: %v", err)
		}
		jsonString := string(jsonData)

		// 3. Get or Create ConfigMap
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
				klog.Infof("Created discovery ConfigMap %s for ServingGroup %s", cmName, servingGroupName)
				return true, nil
			}
			return false, err
		}

		// 4. Update if changed
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
		klog.Infof("Updated discovery ConfigMap %s for ServingGroup %s", cmName, servingGroupName)
		return true, nil
	})
}
