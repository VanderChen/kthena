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

const (
	// RanktableLevelAnnotation specifies the ranktable generation level (role or group)
	RanktableLevelAnnotation = "kthena.io/ranktable-level"

	// PodRanktableAnnotation is the annotation key for pod ranktable data
	PodRanktableAnnotation = "ascend.com/ranktable"

	// RanktableTemplateNamespace is the namespace where ranktable templates are stored
	RanktableTemplateDefaultNamespace = "default"

	// RanktableConfigMapSuffix is the suffix for ranktable ConfigMap names
	RanktableConfigMapSuffix = "ranktable"

	// RanktableStatusInitializing represents the initializing status
	RanktableStatusInitializing = "Initializing"

	// RanktableStatusCompleted represents the completed status
	RanktableStatusCompleted = "Completed"
)

// RanktableLevel defines the level at which ranktable is generated
type RanktableLevel string

const (
	// RoleLevelRanktable generates ranktable at role level
	RoleLevelRanktable RanktableLevel = "role"

	// GroupLevelRanktable generates ranktable at group level
	GroupLevelRanktable RanktableLevel = "group"
)

// RanktableTemplate represents the complete ranktable template configuration
type RanktableTemplate struct {
	// InferenceEngine specifies the inference engine type (mindie, vllm-ascend)
	InferenceEngine string

	// Level specifies the ranktable generation level (role or group)
	Level RanktableLevel

	// PodParserTemplate is the Go template for parsing pod ranktable annotation
	PodParserTemplate string

	// RanktableTemplate is the Go template for generating the final ranktable JSON
	RanktableTemplate string

	// MountPath is the path where ranktable ConfigMap will be mounted
	MountPath string

	// Filename is the name of the ranktable file
	Filename string

	// PodAnnotationName is the annotation key for pod ranktable data
	// If not specified, defaults to PodRanktableAnnotation constant
	PodAnnotationName string
}

// PodRanktableData represents the ranktable information from a single pod
type PodRanktableData struct {
	PodName  string       `json:"podName"`
	ServerId string       `json:"serverId"`
	Devices  []DeviceInfo `json:"devices"`
}

// DeviceInfo represents device information without rank_id
type DeviceInfo struct {
	DeviceId string `json:"deviceId"`
	DeviceIp string `json:"deviceIp"`
}

// RanktableTemplateData represents the data used to generate role/group ranktable
type RanktableTemplateData struct {
	Status       string       `json:"status"`
	ServerCount  int          `json:"serverCount"`
	TotalDevices int          `json:"totalDevices"`
	Timestamp    string       `json:"timestamp"`
	Servers      []ServerInfo `json:"servers"`
}

// ServerInfo represents server information in the ranktable
type ServerInfo struct {
	ServerId string           `json:"serverId"`
	Devices  []DeviceWithRank `json:"devices"`
}

// DeviceWithRank represents device information with rank_id
type DeviceWithRank struct {
	DeviceId string `json:"deviceId"`
	DeviceIp string `json:"deviceIp"`
	RankId   string `json:"rankId"`
}
