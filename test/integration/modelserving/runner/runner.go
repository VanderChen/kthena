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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// TestRunner manages test execution and context.
type TestRunner struct {
	kubeClient   kubernetes.Interface
	kthenaClient clientset.Interface
	restConfig   *rest.Config
	namespace    string
	testID       string
	context      map[string]interface{}
	executor     *Executor
}

// NewTestRunner creates a new test runner instance.
func NewTestRunner(kubeClient kubernetes.Interface, kthenaClient clientset.Interface, restConfig *rest.Config, namespace, testID string) *TestRunner {
	runner := &TestRunner{
		kubeClient:   kubeClient,
		kthenaClient: kthenaClient,
		restConfig:   restConfig,
		namespace:    namespace,
		testID:       testID,
		context:      make(map[string]interface{}),
	}
	runner.executor = NewExecutor(kubeClient, kthenaClient, restConfig, namespace)

	// Initialize default context variables
	runner.context["TestID"] = testID
	runner.context["Namespace"] = namespace

	return runner
}

// LoadTestCasesFromDir loads all YAML test cases from a directory recursively.
func LoadTestCasesFromDir(dir string) ([]*TestCase, error) {
	var testCases []*TestCase

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories and non-YAML files
		if info.IsDir() || (!strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml")) {
			return nil
		}

		tc, err := LoadTestCase(path)
		if err != nil {
			return fmt.Errorf("failed to load test case from %s: %w", path, err)
		}

		testCases = append(testCases, tc)
		return nil
	})

	return testCases, err
}

// LoadTestCase loads a single test case from a YAML file.
func LoadTestCase(path string) (*TestCase, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	var tc TestCase
	if err := yaml.Unmarshal(data, &tc); err != nil {
		return nil, fmt.Errorf("failed to unmarshal YAML: %w", err)
	}

	return &tc, nil
}

// RunTestCase executes a complete test case including setup, steps, and cleanup.
func (r *TestRunner) RunTestCase(ctx context.Context, tc *TestCase) error {
	fmt.Printf("\n=== Running test: %s ===\n", tc.Metadata.Name)
	fmt.Printf("Description: %s\n", tc.Metadata.Description)

	// Apply timeout if specified
	if tc.Spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tc.Spec.Timeout)
		defer cancel()
	}

	// Run setup steps
	if len(tc.Spec.Setup) > 0 {
		fmt.Println("\n--- Running Setup ---")
		for i, step := range tc.Spec.Setup {
			if err := r.runStep(ctx, step, i+1, len(tc.Spec.Setup)); err != nil {
				return fmt.Errorf("setup step %d (%s) failed: %w", i+1, step.Name, err)
			}
		}
	}

	// Run test steps
	fmt.Println("\n--- Running Test Steps ---")
	testErr := r.runSteps(ctx, tc.Spec.Steps)

	// Always run cleanup
	if len(tc.Spec.Cleanup) > 0 {
		fmt.Println("\n--- Running Cleanup ---")
		for i, step := range tc.Spec.Cleanup {
			if err := r.runStep(ctx, step, i+1, len(tc.Spec.Cleanup)); err != nil {
				fmt.Printf("WARNING: cleanup step %d (%s) failed: %v\n", i+1, step.Name, err)
			}
		}
	}

	return testErr
}

// runSteps executes a list of test steps sequentially.
func (r *TestRunner) runSteps(ctx context.Context, steps []TestStep) error {
	for i, step := range steps {
		if err := r.runStep(ctx, step, i+1, len(steps)); err != nil {
			return fmt.Errorf("step %d (%s) failed: %w", i+1, step.Name, err)
		}
	}
	return nil
}

// runStep executes a single test step.
func (r *TestRunner) runStep(ctx context.Context, step TestStep, currentNum, totalNum int) error {
	fmt.Printf("[%d/%d] %s\n", currentNum, totalNum, step.Name)

	// Interpolate variables in the step
	interpolatedStep, err := r.interpolateStep(step)
	if err != nil {
		return fmt.Errorf("failed to interpolate variables: %w", err)
	}

	// Execute the step through the executor
	return r.executor.Execute(ctx, interpolatedStep, r)
}

// interpolateStep replaces template variables in a test step.
func (r *TestRunner) interpolateStep(step TestStep) (TestStep, error) {
	// Create a copy of the step
	interpolated := step

	// Interpolate string fields
	var err error
	interpolated.Name, err = r.interpolateString(step.Name)
	if err != nil {
		return step, err
	}
	interpolated.Resource, err = r.interpolateString(step.Resource)
	if err != nil {
		return step, err
	}
	interpolated.Manifest, err = r.interpolateString(step.Manifest)
	if err != nil {
		return step, err
	}
	interpolated.Selector, err = r.interpolateString(step.Selector)
	if err != nil {
		return step, err
	}
	interpolated.Condition, err = r.interpolateString(step.Condition)
	if err != nil {
		return step, err
	}
	interpolated.SaveTo, err = r.interpolateString(step.SaveTo)
	if err != nil {
		return step, err
	}

	// Interpolate patches
	for i, patch := range step.Patches {
		interpolated.Patches[i].Path, err = r.interpolateString(patch.Path)
		if err != nil {
			return step, err
		}
		// Interpolate value if it's a string
		if strVal, ok := patch.Value.(string); ok {
			interpolated.Patches[i].Value, err = r.interpolateString(strVal)
			if err != nil {
				return step, err
			}
		}
	}

	// Interpolate assertions
	for i, assertion := range step.Assertions {
		interpolated.Assertions[i].Resource, err = r.interpolateString(assertion.Resource)
		if err != nil {
			return step, err
		}
		interpolated.Assertions[i].Field, err = r.interpolateString(assertion.Field)
		if err != nil {
			return step, err
		}
		interpolated.Assertions[i].Selector, err = r.interpolateString(assertion.Selector)
		if err != nil {
			return step, err
		}
	}

	return interpolated, nil
}

// interpolateString replaces template variables in a string.
func (r *TestRunner) interpolateString(s string) (string, error) {
	if s == "" || !strings.Contains(s, "{{") {
		return s, nil
	}

	tmpl, err := template.New("interpolate").Parse(s)
	if err != nil {
		return "", fmt.Errorf("failed to parse template: %w", err)
	}

	var result strings.Builder
	if err := tmpl.Execute(&result, r.context); err != nil {
		return "", fmt.Errorf("failed to execute template: %w", err)
	}

	return result.String(), nil
}

// SetContextValue saves a value to the test context.
func (r *TestRunner) SetContextValue(key string, value interface{}) {
	r.context[key] = value
}

// GetContextValue retrieves a value from the test context.
func (r *TestRunner) GetContextValue(key string) (interface{}, bool) {
	val, ok := r.context[key]
	return val, ok
}
