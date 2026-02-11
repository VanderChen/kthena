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
	"bytes"
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

// ChaosInjector provides methods to inject fault scenarios into pods.
type ChaosInjector struct {
	kubeClient kubernetes.Interface
	restConfig *rest.Config
	namespace  string
}

// NewChaosInjector creates a new chaos injector instance.
func NewChaosInjector(kubeClient kubernetes.Interface, restConfig *rest.Config, namespace string) *ChaosInjector {
	return &ChaosInjector{
		kubeClient: kubeClient,
		restConfig: restConfig,
		namespace:  namespace,
	}
}

// InjectPodPending makes a pod unschedulable by adding an invalid nodeSelector.
func (c *ChaosInjector) InjectPodPending(ctx context.Context, podName string) error {
	pod, err := c.kubeClient.CoreV1().Pods(c.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	// Add unschedulable nodeSelector
	if pod.Spec.NodeSelector == nil {
		pod.Spec.NodeSelector = make(map[string]string)
	}
	pod.Spec.NodeSelector["non-existent-node"] = "true"

	_, err = c.kubeClient.CoreV1().Pods(c.namespace).Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		return fmt.Errorf("failed to update pod: %w", err)
	}

	return nil
}

// InjectPodError kills the main process in a pod to trigger error state.
func (c *ChaosInjector) InjectPodError(ctx context.Context, podName, containerName string) error {
	// Execute kill command in the pod
	cmd := []string{"sh", "-c", "kill 1"}

	return c.ExecInPod(ctx, podName, containerName, cmd)
}

// InjectPodOOM triggers OOM by consuming memory in a pod.
func (c *ChaosInjector) InjectPodOOM(ctx context.Context, podName, containerName string) error {
	// Execute memory stress command
	cmd := []string{"sh", "-c", "stress --vm 1 --vm-bytes 1G --vm-hang 0"}

	return c.ExecInPod(ctx, podName, containerName, cmd)
}

// ExecInPod executes a command in a pod container.
func (c *ChaosInjector) ExecInPod(ctx context.Context, podName, containerName string, cmd []string) error {
	pod, err := c.kubeClient.CoreV1().Pods(c.namespace).Get(ctx, podName, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get pod: %w", err)
	}

	// Use first container if not specified
	if containerName == "" {
		if len(pod.Spec.Containers) == 0 {
			return fmt.Errorf("no containers found in pod")
		}
		containerName = pod.Spec.Containers[0].Name
	}

	req := c.kubeClient.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(podName).
		Namespace(c.namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: containerName,
			Command:   cmd,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(c.restConfig, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	err = exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	})

	if err != nil {
		return fmt.Errorf("failed to exec command (stdout: %s, stderr: %s): %w", stdout.String(), stderr.String(), err)
	}

	return nil
}

// DeletePod deletes a pod to test recovery.
func (c *ChaosInjector) DeletePod(ctx context.Context, podName string) error {
	err := c.kubeClient.CoreV1().Pods(c.namespace).Delete(ctx, podName, metav1.DeleteOptions{})
	if err != nil {
		return fmt.Errorf("failed to delete pod: %w", err)
	}
	return nil
}

// DeletePodsWithSelector deletes all pods matching a selector.
func (c *ChaosInjector) DeletePodsWithSelector(ctx context.Context, selector string) error {
	err := c.kubeClient.CoreV1().Pods(c.namespace).DeleteCollection(
		ctx,
		metav1.DeleteOptions{},
		metav1.ListOptions{LabelSelector: selector},
	)
	if err != nil {
		return fmt.Errorf("failed to delete pods: %w", err)
	}
	return nil
}

// InjectNetworkDelay adds network latency to a pod (requires tc tool in container).
func (c *ChaosInjector) InjectNetworkDelay(ctx context.Context, podName, containerName string, delayMs int) error {
	cmd := []string{"sh", "-c", fmt.Sprintf("tc qdisc add dev eth0 root netem delay %dms", delayMs)}
	return c.ExecInPod(ctx, podName, containerName, cmd)
}

// RemoveNetworkDelay removes network latency from a pod.
func (c *ChaosInjector) RemoveNetworkDelay(ctx context.Context, podName, containerName string) error {
	cmd := []string{"sh", "-c", "tc qdisc del dev eth0 root"}
	return c.ExecInPod(ctx, podName, containerName, cmd)
}
