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

package controller

import (
	"context"
	"os"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/volcano-sh/kthena/pkg/model-booster-controller/utils"
)

const (
	// ControllerProtocolLabel is carried by the Pods of the controller manager
	// and names the way its ModelServing controller keeps the state of a
	// ModelServing. ControllerProtocolServingGroup: in ServingGroup objects. A
	// Pod without the label keeps it in memory. The label names the protocol,
	// not the version: a later version that works the same way carries the
	// same value and is not kept waiting.
	ControllerProtocolLabel        = "modelserving.volcano.sh/controller-protocol"
	ControllerProtocolServingGroup = "servinggroup"

	componentLabel    = "app.kubernetes.io/component"
	componentManager  = "kthena-controller-manager"
	predecessorPeriod = 5 * time.Second
)

// A controller manager from before ServingGroup objects owns the Pods of a
// ModelServing directly and keeps what it knows about them in memory. When it
// sees a Pod change its owner it takes the Pod for lost and recovers the group
// by deleting the other Pods. It did not take part in leader election unless
// told to. So the two versions must never work at the same time, and the lease
// cannot be what keeps them apart: this process checks for itself.

// predecessors returns the names of the Pods of the controller manager in
// namespace that run and do not keep their state in ServingGroup objects.
func predecessors(ctx context.Context, kube kubernetes.Interface, namespace, self string) ([]string, error) {
	pods, err := kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: componentLabel + "=" + componentManager})
	if err != nil {
		return nil, err
	}
	var names []string
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.Name == self || pod.Labels[ControllerProtocolLabel] == ControllerProtocolServingGroup {
			continue
		}
		// A Pod that is terminating still runs until it is gone.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		names = append(names, pod.Name)
	}
	sort.Strings(names)
	return names, nil
}

// identity returns the namespace and the name of the Pod this process runs
// in. ok is false outside a cluster, where there is no predecessor to wait for.
func identity() (namespace, name string, ok bool) {
	namespace, err := utils.GetInClusterNameSpace()
	if err != nil {
		return "", "", false
	}
	name, err = os.Hostname()
	return namespace, name, err == nil
}

// waitForPredecessors blocks until no controller manager of an earlier version
// runs in the namespace of this process.
func waitForPredecessors(ctx context.Context, kube kubernetes.Interface) error {
	namespace, self, ok := identity()
	if !ok {
		klog.InfoS("Not running in a cluster; not checking for a controller manager of an earlier version")
		return nil
	}
	reported := ""
	return wait.PollUntilContextCancel(ctx, predecessorPeriod, true, func(ctx context.Context) (bool, error) {
		names, err := predecessors(ctx, kube, namespace, self)
		if err != nil {
			klog.ErrorS(err, "Cannot tell whether a controller manager of an earlier version still runs; the ModelServing controllers do not start")
			return false, nil
		}
		if len(names) == 0 {
			return true, nil
		}
		if current := strings.Join(names, ","); current != reported {
			klog.InfoS("A controller manager of an earlier version still runs; the ModelServing controllers start when it is gone", "pods", names)
			reportWaiting(ctx, kube, namespace, self, current)
			reported = current
		}
		return false, nil
	})
}

// reportWaiting records on the Pod of this process whom it waits for, for
// whoever wonders why a new controller manager does nothing.
func reportWaiting(ctx context.Context, kube kubernetes.Interface, namespace, self, predecessors string) {
	pod, err := kube.CoreV1().Pods(namespace).Get(ctx, self, metav1.GetOptions{})
	if err != nil {
		return
	}
	now := metav1.NewTime(time.Now())
	_, err = kube.CoreV1().Events(namespace).Create(ctx, &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, GenerateName: self + "."},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", APIVersion: "v1", Namespace: namespace, Name: pod.Name, UID: pod.UID},
		Type:           corev1.EventTypeNormal,
		Reason:         "WaitingForPredecessor",
		Message:        "The ModelServing controllers start when the controller manager of the earlier version is gone: " + predecessors,
		Source:         corev1.EventSource{Component: componentManager},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}, metav1.CreateOptions{})
	if err != nil {
		klog.V(2).InfoS("Cannot record an event on the Pod of this process", "err", err)
	}
}

// exitOnPredecessor ends the process when a controller manager of an earlier
// version appears while this one works, as it does when the Deployment is
// rolled back. The process starts again and waits in waitForPredecessors.
func exitOnPredecessor(ctx context.Context, kube kubernetes.Interface) {
	namespace, self, ok := identity()
	if !ok {
		return
	}
	wait.UntilWithContext(ctx, func(ctx context.Context) {
		names, err := predecessors(ctx, kube, namespace, self)
		if err != nil || len(names) == 0 {
			return
		}
		klog.ErrorS(nil, "A controller manager of an earlier version runs next to this one; exiting so that the two never work at the same time", "pods", names)
		klog.FlushAndExit(klog.ExitFlushTimeout, 1)
	}, predecessorPeriod)
}
