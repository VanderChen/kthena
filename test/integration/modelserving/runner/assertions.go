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
	"reflect"
	"strings"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// AssertStatusField checks a status field of a ModelServing resource.
func AssertStatusField(ctx context.Context, kthenaClient clientset.Interface, namespace, resource, field, operator string, expected interface{}) error {
	kind, name, err := parseResource(resource)
	if err != nil {
		return err
	}

	if strings.ToLower(kind) != "modelserving" {
		return fmt.Errorf("statusField assertion only supports ModelServing resources")
	}

	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get ModelServing: %w", err)
	}

	// Extract field value using reflection
	actual, err := getFieldValue(ms.Status, field)
	if err != nil {
		return fmt.Errorf("failed to get field %s: %w", field, err)
	}

	// Compare values
	return compareValues(actual, operator, expected)
}

// AssertPodCount checks the number of pods matching a selector.
func AssertPodCount(ctx context.Context, kubeClient kubernetes.Interface, namespace, selector, operator string, expected interface{}) error {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	actual := len(pods.Items)
	expectedInt, ok := toInt(expected)
	if !ok {
		return fmt.Errorf("expected value must be an integer for podCount assertion")
	}

	return compareInts(actual, operator, expectedInt)
}

// AssertPodPhase checks that all pods matching a selector are in a specific phase.
func AssertPodPhase(ctx context.Context, kubeClient kubernetes.Interface, namespace, selector string, expectedPhase interface{}) error {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return fmt.Errorf("failed to list pods: %w", err)
	}

	if len(pods.Items) == 0 {
		return fmt.Errorf("no pods found matching selector: %s", selector)
	}

	expectedPhaseStr, ok := expectedPhase.(string)
	if !ok {
		return fmt.Errorf("expected phase must be a string")
	}

	for _, pod := range pods.Items {
		if string(pod.Status.Phase) != expectedPhaseStr {
			return fmt.Errorf("pod %s is in phase %s, expected %s", pod.Name, pod.Status.Phase, expectedPhaseStr)
		}
	}

	return nil
}

// AssertPodRecreated checks if a pod has been recreated (UID changed).
func AssertPodRecreated(ctx context.Context, kubeClient kubernetes.Interface, namespace, name, oldUID string) error {
	pod, err := kubeClient.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	if string(pod.UID) == oldUID {
		return fmt.Errorf("pod UID has not changed (pod not recreated)")
	}

	return nil
}

// AssertEventExists checks if a specific event exists.
func AssertEventExists(ctx context.Context, kubeClient kubernetes.Interface, namespace, involvedObjectName, reason string) error {
	events, err := kubeClient.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list events: %w", err)
	}

	for _, event := range events.Items {
		if event.InvolvedObject.Name == involvedObjectName && event.Reason == reason {
			return nil
		}
	}

	return fmt.Errorf("event not found: involvedObject=%s, reason=%s", involvedObjectName, reason)
}

// Helper functions

func getFieldValue(status interface{}, field string) (interface{}, error) {
	// Use reflection to navigate nested fields
	v := reflect.ValueOf(status)
	parts := strings.Split(field, ".")

	for _, part := range parts {
		// Handle "status." prefix
		if part == "status" {
			continue
		}

		// Get field by name (case-insensitive)
		v = getFieldByName(v, part)
		if !v.IsValid() {
			return nil, fmt.Errorf("field not found: %s", part)
		}
	}

	return v.Interface(), nil
}

func getFieldByName(v reflect.Value, name string) reflect.Value {
	// Dereference pointer if needed
	if v.Kind() == reflect.Ptr {
		v = v.Elem()
	}

	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}

	// Try exact match first
	field := v.FieldByName(name)
	if field.IsValid() {
		return field
	}

	// Try case-insensitive match
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		if strings.EqualFold(t.Field(i).Name, name) {
			return v.Field(i)
		}
	}

	return reflect.Value{}
}

func compareValues(actual interface{}, operator string, expected interface{}) error {
	switch operator {
	case "equals", "==":
		if !reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("expected %v, got %v", expected, actual)
		}
	case "notEquals", "!=":
		if reflect.DeepEqual(actual, expected) {
			return fmt.Errorf("expected not %v, but got %v", expected, actual)
		}
	case "greaterThan", ">":
		actualInt, ok1 := toInt(actual)
		expectedInt, ok2 := toInt(expected)
		if !ok1 || !ok2 {
			return fmt.Errorf("values must be integers for comparison")
		}
		if actualInt <= expectedInt {
			return fmt.Errorf("expected > %d, got %d", expectedInt, actualInt)
		}
	case "greaterThanOrEqual", ">=":
		actualInt, ok1 := toInt(actual)
		expectedInt, ok2 := toInt(expected)
		if !ok1 || !ok2 {
			return fmt.Errorf("values must be integers for comparison")
		}
		if actualInt < expectedInt {
			return fmt.Errorf("expected >= %d, got %d", expectedInt, actualInt)
		}
	case "lessThan", "<":
		actualInt, ok1 := toInt(actual)
		expectedInt, ok2 := toInt(expected)
		if !ok1 || !ok2 {
			return fmt.Errorf("values must be integers for comparison")
		}
		if actualInt >= expectedInt {
			return fmt.Errorf("expected < %d, got %d", expectedInt, actualInt)
		}
	case "lessThanOrEqual", "<=":
		actualInt, ok1 := toInt(actual)
		expectedInt, ok2 := toInt(expected)
		if !ok1 || !ok2 {
			return fmt.Errorf("values must be integers for comparison")
		}
		if actualInt > expectedInt {
			return fmt.Errorf("expected <= %d, got %d", expectedInt, actualInt)
		}
	default:
		return fmt.Errorf("unknown operator: %s", operator)
	}
	return nil
}

func compareInts(actual int, operator string, expected int) error {
	switch operator {
	case "equals", "==":
		if actual != expected {
			return fmt.Errorf("expected %d, got %d", expected, actual)
		}
	case "notEquals", "!=":
		if actual == expected {
			return fmt.Errorf("expected not %d, but got %d", expected, actual)
		}
	case "greaterThan", ">":
		if actual <= expected {
			return fmt.Errorf("expected > %d, got %d", expected, actual)
		}
	case "greaterThanOrEqual", ">=":
		if actual < expected {
			return fmt.Errorf("expected >= %d, got %d", expected, actual)
		}
	case "lessThan", "<":
		if actual >= expected {
			return fmt.Errorf("expected < %d, got %d", expected, actual)
		}
	case "lessThanOrEqual", "<=":
		if actual > expected {
			return fmt.Errorf("expected <= %d, got %d", expected, actual)
		}
	default:
		return fmt.Errorf("unknown operator: %s", operator)
	}
	return nil
}

func toInt(v interface{}) (int, bool) {
	switch val := v.(type) {
	case int:
		return val, true
	case int32:
		return int(val), true
	case int64:
		return int(val), true
	case float64:
		return int(val), true
	default:
		return 0, false
	}
}

// GetPodsByLabel returns all pods matching a label selector.
func GetPodsByLabel(ctx context.Context, kubeClient kubernetes.Interface, namespace, selector string) ([]corev1.Pod, error) {
	pods, err := kubeClient.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
	})
	if err != nil {
		return nil, err
	}
	return pods.Items, nil
}
