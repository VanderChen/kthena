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
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/yaml"
)

// Executor executes test step operations.
type Executor struct {
	kubeClient    kubernetes.Interface
	kthenaClient  clientset.Interface
	dynamicClient dynamic.Interface
	restConfig    *rest.Config
	namespace     string
}

// NewExecutor creates a new executor instance.
func NewExecutor(kubeClient kubernetes.Interface, kthenaClient clientset.Interface, restConfig *rest.Config, namespace string) *Executor {
	// Create dynamic client from the same config
	dynamicClient, _ := dynamic.NewForConfig(restConfig)

	return &Executor{
		kubeClient:    kubeClient,
		kthenaClient:  kthenaClient,
		dynamicClient: dynamicClient,
		restConfig:    restConfig,
		namespace:     namespace,
	}
}

// Execute runs a test step operation.
func (e *Executor) Execute(ctx context.Context, step TestStep, runner *TestRunner) error {
	switch step.Action {
	case "apply":
		return e.executeApply(ctx, step, runner)
	case "patch":
		return e.executePatch(ctx, step, runner)
	case "delete":
		return e.executeDelete(ctx, step, runner)
	case "wait":
		return e.executeWait(ctx, step, runner)
	case "assert":
		return e.executeAssert(ctx, step, runner)
	case "exec":
		return e.executeExec(ctx, step, runner)
	case "query":
		return e.executeQuery(ctx, step, runner)
	case "sleep":
		return e.executeSleep(ctx, step, runner)
	default:
		return fmt.Errorf("unknown action: %s", step.Action)
	}
}

// executeApply creates a resource from a manifest file with optional patches.
func (e *Executor) executeApply(ctx context.Context, step TestStep, runner *TestRunner) error {
	if step.Manifest == "" {
		return fmt.Errorf("manifest is required for apply action")
	}

	// Load the manifest from file
	data, err := e.loadManifestFile(step.Manifest)
	if err != nil {
		return fmt.Errorf("failed to load manifest: %w", err)
	}

	// Parse as unstructured object
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(data, &obj); err != nil {
		return fmt.Errorf("failed to unmarshal manifest: %w", err)
	}

	// Apply patches if specified
	if len(step.Patches) > 0 {
		if err := e.applyPatches(&obj, step.Patches); err != nil {
			return fmt.Errorf("failed to apply patches: %w", err)
		}
	}

	// Ensure namespace is set
	if obj.GetNamespace() == "" {
		obj.SetNamespace(e.namespace)
	}

	// Get GVR for the resource
	gvr, err := e.getGVR(obj.GetAPIVersion(), obj.GetKind())
	if err != nil {
		return fmt.Errorf("failed to get GVR: %w", err)
	}

	// Create the resource
	_, err = e.dynamicClient.Resource(gvr).Namespace(e.namespace).Create(ctx, &obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	fmt.Printf("  ✓ Created %s/%s\n", obj.GetKind(), obj.GetName())
	return nil
}

// executePatch updates a resource with a JSON patch.
func (e *Executor) executePatch(ctx context.Context, step TestStep, runner *TestRunner) error {
	if step.Resource == "" {
		return fmt.Errorf("resource is required for patch action")
	}

	kind, name, err := parseResource(step.Resource)
	if err != nil {
		return err
	}

	// Build patch data
	patchData, err := json.Marshal(step.Patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch: %w", err)
	}

	// Determine API version based on kind
	apiVersion := e.getAPIVersionForKind(kind)
	gvr, err := e.getGVR(apiVersion, kind)
	if err != nil {
		return fmt.Errorf("failed to get GVR: %w", err)
	}

	// Apply patch
	_, err = e.dynamicClient.Resource(gvr).Namespace(e.namespace).Patch(
		ctx, name, "application/merge-patch+json", patchData, metav1.PatchOptions{},
	)
	if err != nil {
		return fmt.Errorf("failed to patch resource: %w", err)
	}

	fmt.Printf("  ✓ Patched %s/%s\n", kind, name)
	return nil
}

// executeDelete deletes a resource.
func (e *Executor) executeDelete(ctx context.Context, step TestStep, runner *TestRunner) error {
	if step.Resource == "" {
		return fmt.Errorf("resource is required for delete action")
	}

	kind, name, err := parseResource(step.Resource)
	if err != nil {
		return err
	}

	apiVersion := e.getAPIVersionForKind(kind)
	gvr, err := e.getGVR(apiVersion, kind)
	if err != nil {
		return fmt.Errorf("failed to get GVR: %w", err)
	}

	err = e.dynamicClient.Resource(gvr).Namespace(e.namespace).Delete(ctx, name, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete resource: %w", err)
	}

	fmt.Printf("  ✓ Deleted %s/%s\n", kind, name)
	return nil
}

// executeWait waits for a condition to be met.
func (e *Executor) executeWait(ctx context.Context, step TestStep, runner *TestRunner) error {
	timeout := step.Timeout
	if timeout == 0 {
		timeout = 3 * time.Minute
	}

	kind, name, err := parseResource(step.Resource)
	if err != nil {
		return err
	}

	fmt.Printf("  Waiting for %s (timeout: %v)...\n", step.Condition, timeout)

	timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	return wait.PollUntilContextTimeout(timeoutCtx, 2*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		switch step.Condition {
		case "ready":
			return e.checkModelServingReady(ctx, name)
		case "deleted":
			return e.checkResourceDeleted(ctx, kind, name)
		case "running":
			return e.checkPodRunning(ctx, name)
		default:
			return false, fmt.Errorf("unknown wait condition: %s", step.Condition)
		}
	})
}

// executeAssert runs all assertions in the step.
func (e *Executor) executeAssert(ctx context.Context, step TestStep, runner *TestRunner) error {
	for i, assertion := range step.Assertions {
		fmt.Printf("  Checking assertion %d/%d: %s\n", i+1, len(step.Assertions), assertion.Type)

		timeout := assertion.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}

		timeoutCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		// Poll until assertion passes or timeout
		err := wait.PollUntilContextTimeout(timeoutCtx, 1*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
			err := e.runAssertion(ctx, assertion, runner)
			if err != nil {
				// Continue polling on assertion failures
				return false, nil
			}
			return true, nil
		})

		if err != nil {
			lastErr := e.runAssertion(ctx, assertion, runner)
			return fmt.Errorf("assertion %d failed after %v: %w", i+1, timeout, lastErr)
		}

		fmt.Printf("    ✓ Assertion passed\n")
	}
	return nil
}

// runAssertion executes a single assertion.
func (e *Executor) runAssertion(ctx context.Context, assertion Assertion, runner *TestRunner) error {
	switch assertion.Type {
	case "statusField":
		return AssertStatusField(ctx, e.kthenaClient, e.namespace, assertion.Resource, assertion.Field, assertion.Operator, assertion.Value)
	case "podCount":
		return AssertPodCount(ctx, e.kubeClient, e.namespace, assertion.Selector, assertion.Operator, assertion.Value)
	case "podPhase":
		return AssertPodPhase(ctx, e.kubeClient, e.namespace, assertion.Selector, assertion.Value)
	default:
		return fmt.Errorf("unknown assertion type: %s", assertion.Type)
	}
}

// executeExec executes a command in a pod.
func (e *Executor) executeExec(ctx context.Context, step TestStep, runner *TestRunner) error {
	// This would use kubectl exec or the Kubernetes exec API
	// For now, return not implemented
	return fmt.Errorf("exec action not yet implemented")
}

// executeQuery queries a resource and saves data to context.
func (e *Executor) executeQuery(ctx context.Context, step TestStep, runner *TestRunner) error {
	if step.SaveTo == "" {
		return fmt.Errorf("saveTo is required for query action")
	}

	switch step.Query.Type {
	case "podUID":
		uid, err := e.queryPodUID(ctx, step.Query.Selector, step.Query.Index)
		if err != nil {
			return err
		}
		runner.SetContextValue(step.SaveTo, uid)
		fmt.Printf("  ✓ Saved pod UID to context: %s = %s\n", step.SaveTo, uid)
	case "podName":
		name, err := e.queryPodName(ctx, step.Query.Selector, step.Query.Index)
		if err != nil {
			return err
		}
		runner.SetContextValue(step.SaveTo, name)
		fmt.Printf("  ✓ Saved pod name to context: %s = %s\n", step.SaveTo, name)
	default:
		return fmt.Errorf("unknown query type: %s", step.Query.Type)
	}

	return nil
}

// executeSleep pauses execution for the specified timeout.
func (e *Executor) executeSleep(ctx context.Context, step TestStep, runner *TestRunner) error {
	duration := step.Timeout
	if duration == 0 {
		duration = 1 * time.Second
	}
	fmt.Printf("  Sleeping for %v...\n", duration)
	time.Sleep(duration)
	return nil
}

// Helper methods

func (e *Executor) loadManifestFile(path string) ([]byte, error) {
	// Get project root (test/integration/modelserving is 3 levels deep)
	_, filename, _, _ := runtime.Caller(0)
	projectRoot := filepath.Join(filepath.Dir(filename), "..", "..", "..", "..")
	absPath := filepath.Join(projectRoot, path)

	return os.ReadFile(absPath)
}

func (e *Executor) applyPatches(obj *unstructured.Unstructured, patches []Patch) error {
	for _, patch := range patches {
		if err := unstructured.SetNestedField(obj.Object, patch.Value, strings.Split(patch.Path, ".")...); err != nil {
			return fmt.Errorf("failed to apply patch %s: %w", patch.Path, err)
		}
	}
	return nil
}

func (e *Executor) getGVR(apiVersion, kind string) (schema.GroupVersionResource, error) {
	// Parse apiVersion into group and version
	parts := strings.Split(apiVersion, "/")
	var group, version string
	if len(parts) == 1 {
		version = parts[0]
	} else {
		group = parts[0]
		version = parts[1]
	}

	// Map kinds to resources
	resource := strings.ToLower(kind) + "s"

	return schema.GroupVersionResource{
		Group:    group,
		Version:  version,
		Resource: resource,
	}, nil
}

func (e *Executor) getAPIVersionForKind(kind string) string {
	switch strings.ToLower(kind) {
	case "modelserving":
		return "workload.serving.volcano.sh/v1alpha1"
	case "pod":
		return "v1"
	case "configmap", "service":
		return "v1"
	default:
		return "v1"
	}
}

func (e *Executor) checkModelServingReady(ctx context.Context, name string) (bool, error) {
	ms, err := e.kthenaClient.WorkloadV1alpha1().ModelServings(e.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}

	expectedReplicas := int32(1)
	if ms.Spec.Replicas != nil {
		expectedReplicas = *ms.Spec.Replicas
	}

	return ms.Status.AvailableReplicas >= expectedReplicas, nil
}

func (e *Executor) checkResourceDeleted(ctx context.Context, kind, name string) (bool, error) {
	apiVersion := e.getAPIVersionForKind(kind)
	gvr, err := e.getGVR(apiVersion, kind)
	if err != nil {
		return false, err
	}

	_, err = e.dynamicClient.Resource(gvr).Namespace(e.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		// Resource not found means it's deleted
		return true, nil
	}
	return false, nil
}

func (e *Executor) checkPodRunning(ctx context.Context, name string) (bool, error) {
	pod, err := e.kubeClient.CoreV1().Pods(e.namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, err
	}
	return pod.Status.Phase == corev1.PodRunning, nil
}

func (e *Executor) queryPodUID(ctx context.Context, selector string, index int) (string, error) {
	pods, err := e.kubeClient.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) <= index {
		return "", fmt.Errorf("pod index %d out of range (found %d pods)", index, len(pods.Items))
	}

	return string(pods.Items[index].UID), nil
}

func (e *Executor) queryPodName(ctx context.Context, selector string, index int) (string, error) {
	pods, err := e.kubeClient.CoreV1().Pods(e.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return "", fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) <= index {
		return "", fmt.Errorf("pod index %d out of range (found %d pods)", index, len(pods.Items))
	}

	return pods.Items[index].Name, nil
}

func parseResource(resource string) (kind string, name string, err error) {
	parts := strings.Split(resource, "/")
	if len(parts) != 2 {
		return "", "", fmt.Errorf("invalid resource format (expected kind/name): %s", resource)
	}
	return parts[0], parts[1], nil
}
