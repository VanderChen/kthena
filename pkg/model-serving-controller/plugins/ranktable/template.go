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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"text/template"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/yaml"
)

// TemplateManager manages ranktable template operations
type TemplateManager struct {
	templateNamespace string
}

// NewTemplateManager creates a new TemplateManager
func NewTemplateManager() *TemplateManager {
	namespace := RanktableTemplateDefaultNamespace

	// Get namespace from environment variable if available, otherwise use "default"
	if podNamespace := os.Getenv("POD_NAMESPACE"); podNamespace != "" {
		namespace = podNamespace
	}
	return &TemplateManager{
		templateNamespace: namespace,
	}
}

// GetRanktableTemplate retrieves and parses the ranktable template from ConfigMap
func (tm *TemplateManager) GetRanktableTemplate(lister listerv1.ConfigMapLister, templateName string) (*RanktableTemplate, error) {
	// Get Role/Group Ranktable Template ConfigMap
	roleTemplateCM, err := lister.ConfigMaps(tm.templateNamespace).Get(templateName)
	if err != nil {
		return nil, fmt.Errorf("failed to get ranktable template %s in namespace %s: %w", templateName, tm.templateNamespace, err)
	}

	// Extract pod parser template name
	podParserTemplateName := roleTemplateCM.Data["pod-parser-template"]
	if podParserTemplateName == "" {
		return nil, fmt.Errorf("pod-parser-template not specified in ranktable template %s", templateName)
	}

	// Get Pod Parser Template ConfigMap
	podParserCM, err := lister.ConfigMaps(tm.templateNamespace).Get(podParserTemplateName)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod parser template %s in namespace %s: %w", podParserTemplateName, tm.templateNamespace, err)
	}

	// Build RanktableTemplate object
	level := RanktableLevel(roleTemplateCM.Data["ranktable-level"])
	if level != RoleLevelRanktable && level != GroupLevelRanktable {
		return nil, fmt.Errorf("invalid ranktable level: %s", level)
	}

	// Get pod annotation name from parser ConfigMap, default to PodRanktableAnnotation
	podAnnotationName := podParserCM.Data["annotation-name"]
	if podAnnotationName == "" {
		podAnnotationName = PodRanktableAnnotation
	}

	return &RanktableTemplate{
		InferenceEngine:   roleTemplateCM.Data["inference-engine"],
		Level:             level,
		PodParserTemplate: podParserCM.Data["parser-template"],
		RanktableTemplate: roleTemplateCM.Data["ranktable-template"],
		MountPath:         roleTemplateCM.Data["mount-path"],
		Filename:          roleTemplateCM.Data["filename"],
		PodAnnotationName: podAnnotationName,
	}, nil
}

// ParsePodRanktable parses pod ranktable annotation using the parser template
func (tm *TemplateManager) ParsePodRanktable(parserTemplate, annotation string) (*PodRanktableData, error) {
	// Parse the template
	tmpl, err := template.New("pod-parser").Funcs(template.FuncMap{
		"fromJson": func(s string) (interface{}, error) {
			var result interface{}
			if err := json.Unmarshal([]byte(s), &result); err != nil {
				return nil, err
			}
			return result, nil
		},
		"quote": func(s string) string {
			return fmt.Sprintf("%q", s)
		},
	}).Parse(parserTemplate)
	if err != nil {
		return nil, fmt.Errorf("failed to parse pod parser template: %w", err)
	}

	// Execute template with annotation data
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, annotation); err != nil {
		return nil, fmt.Errorf("failed to execute pod parser template: %w", err)
	}

	// Parse the YAML output to PodRanktableData
	var podData PodRanktableData
	if err := yaml.Unmarshal(buf.Bytes(), &podData); err != nil {
		return nil, fmt.Errorf("failed to unmarshal pod ranktable data: %w", err)
	}

	return &podData, nil
}

// BuildRanktableTemplateData builds the template data from pod ranktables
func (tm *TemplateManager) BuildRanktableTemplateData(status string, podRanktables []PodRanktableData) RanktableTemplateData {
	// Group devices by server_id
	serverMap := make(map[string][]DeviceInfo)
	for _, podData := range podRanktables {
		serverMap[podData.ServerId] = append(serverMap[podData.ServerId], podData.Devices...)
	}

	// Build Servers list
	servers := make([]ServerInfo, 0, len(serverMap))
	globalRankId := 0
	totalDevices := 0

	// Sort server_ids to ensure deterministic output
	serverIds := make([]string, 0, len(serverMap))
	for serverId := range serverMap {
		serverIds = append(serverIds, serverId)
	}
	sort.Strings(serverIds)

	for _, serverId := range serverIds {
		devices := serverMap[serverId]

		// Sort devices by device_id (lexicographic order)
		sort.Slice(devices, func(i, j int) bool {
			return devices[i].DeviceId < devices[j].DeviceId
		})

		// Generate rank_id
		devicesWithRank := make([]DeviceWithRank, len(devices))
		for i, dev := range devices {
			devicesWithRank[i] = DeviceWithRank{
				DeviceId: dev.DeviceId,
				DeviceIp: dev.DeviceIp,
				RankId:   strconv.Itoa(globalRankId),
			}
			globalRankId++
			totalDevices++
		}

		servers = append(servers, ServerInfo{
			ServerId: serverId,
			Devices:  devicesWithRank,
		})
	}

	return RanktableTemplateData{
		Status:       status,
		ServerCount:  len(servers),
		TotalDevices: totalDevices,
		Timestamp:    time.Now().Format(time.RFC3339),
		Servers:      servers,
	}
}

// RenderRanktable renders the final ranktable JSON using the template
func (tm *TemplateManager) RenderRanktable(ranktableTemplate string, data RanktableTemplateData) (string, error) {
	// Parse the template
	tmpl, err := template.New("ranktable").Funcs(template.FuncMap{
		"quote": func(s string) string {
			return fmt.Sprintf("%q", s)
		},
		"toJson": func(v interface{}) (string, error) {
			b, err := json.Marshal(v)
			if err != nil {
				return "", err
			}
			return string(b), nil
		},
	}).Parse(ranktableTemplate)
	if err != nil {
		return "", fmt.Errorf("failed to parse ranktable template: %w", err)
	}

	// Execute template
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("failed to execute ranktable template: %w", err)
	}

	// Validate JSON output
	var jsonCheck interface{}
	if err := json.Unmarshal(buf.Bytes(), &jsonCheck); err != nil {
		klog.Errorf("Generated ranktable is not valid JSON: %s", buf.String())
		return "", fmt.Errorf("generated ranktable is not valid JSON: %w", err)
	}

	return buf.String(), nil
}

// GenerateRanktableConfigMapName generates the ConfigMap name for a role/group ranktable
func GenerateRanktableConfigMapName(msName, roleOrGroupName string) string {
	return fmt.Sprintf("%s-%s-%s", msName, roleOrGroupName, RanktableConfigMapSuffix)
}

// EnsureRanktableConfigMap creates or updates a ranktable ConfigMap
func (tm *TemplateManager) EnsureRanktableConfigMap(
	ctx context.Context,
	client kubernetes.Interface,
	namespace, name string,
	ownerReferences []metav1.OwnerReference,
	labels map[string]string,
	filename, content string,
) error {
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			OwnerReferences: ownerReferences,
			Labels:          labels,
		},
		Data: map[string]string{
			filename: content,
		},
	}

	existingCM, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			// Create new ConfigMap
			_, err = client.CoreV1().ConfigMaps(namespace).Create(ctx, cm, metav1.CreateOptions{})
			if err != nil {
				return fmt.Errorf("failed to create ranktable ConfigMap %s/%s: %w", namespace, name, err)
			}
			klog.V(2).Infof("Created ranktable ConfigMap %s/%s", namespace, name)
			return nil
		}
		return fmt.Errorf("failed to get ranktable ConfigMap %s/%s: %w", namespace, name, err)
	}

	// Check if update is needed
	if existingCM.Data != nil && existingCM.Data[filename] == content && len(existingCM.Data) == 1 {
		klog.V(4).Infof("Ranktable ConfigMap %s/%s is up to date, skipping update", namespace, name)
		return nil
	}

	// Update existing ConfigMap
	existingCM.Data = cm.Data
	_, err = client.CoreV1().ConfigMaps(namespace).Update(ctx, existingCM, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update ranktable ConfigMap %s/%s: %w", namespace, name, err)
	}
	klog.V(2).Infof("Updated ranktable ConfigMap %s/%s", namespace, name)
	return nil
}
