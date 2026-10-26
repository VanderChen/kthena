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

// Package diagnostics provides temporary, grep-friendly ModelServing timings.
package diagnostics

import (
	"path/filepath"
	"runtime"
	"sync/atomic"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const Marker = "[MS-PENDING-DEBUG]"

var sequence atomic.Uint64

// Span holds only this call's diagnostic fields; it does not track queue state.
// IDs are process-local. An end event means the call returned, not that it succeeded.
type Span struct {
	started time.Time
	fields  []interface{}
}

// Begin emits before work starts, so an unfinished operation remains visible.
// V(4) enables all new diagnostics without changing the controller's defaults.
func Begin(operation string, fields ...interface{}) *Span {
	if !klog.V(4).Enabled() {
		return nil
	}
	s := &Span{started: time.Now()}
	s.fields = append([]interface{}{"operation", operation, "span", sequence.Add(1)}, fields...)
	s.Event("begin")
	return s
}

// Event adds a checkpoint with elapsed time since Begin, including on early exits.
func (s *Span) Event(event string, fields ...interface{}) {
	if s == nil {
		return
	}
	values := make([]interface{}, 0, len(s.fields)+len(fields)+4)
	values = append(values, s.fields...)
	values = append(values, "event", event, "elapsedMs", float64(time.Since(s.started))/float64(time.Millisecond))
	values = append(values, fields...)
	klog.V(4).InfoS(Marker, values...)
}

func (s *Span) End() {
	s.Event("end")
}

func ModelServing(operation string, ms *workloadv1alpha1.ModelServing) *Span {
	if ms == nil || !klog.V(4).Enabled() {
		return nil
	}
	return Begin(operation, "modelServing", klog.KObj(ms), "uid", ms.UID,
		"generation", ms.Generation, "resourceVersion", ms.ResourceVersion)
}

func Pod(operation string, pod *corev1.Pod) *Span {
	if pod == nil || !klog.V(4).Enabled() {
		return nil
	}
	fields := []interface{}{"pod", klog.KObj(pod), "uid", pod.UID,
		"resourceVersion", pod.ResourceVersion, "phase", pod.Status.Phase,
		"modelServing", klog.KRef(pod.Namespace, pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]),
		"node", pod.Spec.NodeName, "terminating", pod.DeletionTimestamp != nil}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodScheduled || condition.Type == corev1.PodReady {
			fields = append(fields, string(condition.Type), condition.Status, string(condition.Type)+"Reason", condition.Reason)
		}
	}
	return Begin(operation, fields...)
}

// Enqueue records a request, not a promise of a distinct queue item. Depth is an
// instantaneous ready-queue observation; delayed, dirty and in-flight keys differ.
func Enqueue(key, mode string, delay time.Duration, depth int) {
	if !klog.V(4).Enabled() {
		return
	}
	_, file, line, _ := runtime.Caller(2)
	klog.V(4).InfoS(Marker, "operation", "enqueue", "key", key, "mode", mode,
		"delayMs", float64(delay)/float64(time.Millisecond), "queueDepth", depth,
		"source", filepath.Base(file), "line", line)
}

// Lock records the caller of lifecycleLocks.lock, without changing lock scope.
func Lock(key string) *Span {
	if !klog.V(4).Enabled() {
		return nil
	}
	_, file, line, _ := runtime.Caller(2)
	return Begin("lifecycle.lock", "key", key, "source", filepath.Base(file), "line", line)
}
