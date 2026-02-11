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

package runner

import (
	"time"
)

// TestCase represents a complete test scenario defined in YAML.
type TestCase struct {
	APIVersion string       `yaml:"apiVersion"`
	Kind       string       `yaml:"kind"`
	Metadata   TestMetadata `yaml:"metadata"`
	Spec       TestCaseSpec `yaml:"spec"`
}

// TestMetadata contains test identification and tagging information.
type TestMetadata struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
}

// TestCaseSpec defines the test execution plan.
type TestCaseSpec struct {
	Timeout time.Duration `yaml:"timeout"`
	Setup   []TestStep    `yaml:"setup,omitempty"`
	Steps   []TestStep    `yaml:"steps"`
	Cleanup []TestStep    `yaml:"cleanup,omitempty"`
}

// TestStep represents a single operation in the test.
type TestStep struct {
	Name     string                 `yaml:"name"`
	Action   string                 `yaml:"action"`
	Resource string                 `yaml:"resource,omitempty"`
	Manifest string                 `yaml:"manifest,omitempty"`
	Patches  []Patch                `yaml:"patches,omitempty"`
	Patch    map[string]interface{} `yaml:"patch,omitempty"`
	Selector string                 `yaml:"selector,omitempty"`
	Condition string                `yaml:"condition,omitempty"`
	Timeout  time.Duration          `yaml:"timeout,omitempty"`
	Assertions []Assertion          `yaml:"assertions,omitempty"`
	Command  []string               `yaml:"command,omitempty"`
	Query    QuerySpec              `yaml:"query,omitempty"`
	SaveTo   string                 `yaml:"saveTo,omitempty"`
}

// Patch represents a single patch operation for a resource.
type Patch struct {
	Path  string      `yaml:"path"`
	Value interface{} `yaml:"value"`
}

// Assertion defines a condition that must be true.
type Assertion struct {
	Type     string      `yaml:"type"`
	Resource string      `yaml:"resource,omitempty"`
	Field    string      `yaml:"field,omitempty"`
	Selector string      `yaml:"selector,omitempty"`
	Operator string      `yaml:"operator"`
	Value    interface{} `yaml:"value"`
	Timeout  time.Duration `yaml:"timeout,omitempty"`
}

// QuerySpec defines what data to query and save.
type QuerySpec struct {
	Type     string `yaml:"type"`
	Selector string `yaml:"selector,omitempty"`
	Field    string `yaml:"field,omitempty"`
	Index    int    `yaml:"index,omitempty"`
}
