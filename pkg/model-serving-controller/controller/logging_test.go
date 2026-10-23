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
	"errors"
	"flag"
	"strconv"
	"sync"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/klog/v2"
)

type capturedLog struct {
	Message string
	Level   int
	Err     error
	Fields  map[string]any
}

type logCapture struct {
	mu   sync.Mutex
	logs []capturedLog
}

func (c *logCapture) entries() []capturedLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]capturedLog(nil), c.logs...)
}

type captureSink struct {
	capture   *logCapture
	verbosity int
	values    []any
}

func (s *captureSink) Init(logr.RuntimeInfo)                      {}
func (s *captureSink) Enabled(level int) bool                     { return level <= s.verbosity }
func (s *captureSink) Info(level int, msg string, values ...any)  { s.append(level, nil, msg, values) }
func (s *captureSink) Error(err error, msg string, values ...any) { s.append(0, err, msg, values) }
func (s *captureSink) WithName(string) logr.LogSink               { return s }
func (s *captureSink) WithValues(values ...any) logr.LogSink {
	copy := *s
	copy.values = append(append([]any(nil), s.values...), values...)
	return &copy
}
func (s *captureSink) append(level int, err error, msg string, values []any) {
	fields := map[string]any{}
	all := append(append([]any(nil), s.values...), values...)
	for i := 0; i+1 < len(all); i += 2 {
		if key, ok := all[i].(string); ok {
			fields[key] = all[i+1]
		}
	}
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	s.capture.logs = append(s.capture.logs, capturedLog{Message: msg, Level: level, Err: err, Fields: fields})
}

// These tests must not run in parallel: klog is process-global. Restore the
// original backend after controller goroutines have stopped (LIFO cleanup).
func captureModelServingLogs(t *testing.T, verbosity int) *logCapture {
	t.Helper()
	state := klog.CaptureState()
	t.Cleanup(state.Restore)
	flags := flag.NewFlagSet("logging-test", flag.ContinueOnError)
	klog.InitFlags(flags)
	if err := flags.Set("v", strconv.Itoa(verbosity)); err != nil {
		t.Fatal(err)
	}
	capture := &logCapture{}
	klog.SetLoggerWithOptions(logr.New(&captureSink{capture: capture, verbosity: verbosity}), klog.ContextualLogger(true))
	return capture
}

func TestInputErrorIsIncludedInReconcileCompletionLog(t *testing.T) {
	ms := lifecycleMS("input-error-log", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	pod := lifecyclePod(t, c, ms, ms, 0, "stable-pod")
	c.publishPodChange(pod)
	failure := errors.New("Pod input read failed")
	c.kubeClientSet.(*kubefake.Clientset).PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, failure
	})
	logs := &logCapture{}
	ctx := klog.NewContext(context.Background(), logr.New(&captureSink{capture: logs, verbosity: 4}))
	err := c.syncModelServing(ctx, ms.Namespace+"/"+ms.Name)
	require.ErrorIs(t, err, failure)
	require.ErrorContains(t, err, "process Pod inputs")
	finished := 0
	for _, entry := range logs.entries() {
		if entry.Message == "ModelServing reconcile finished" {
			finished++
			require.Equal(t, "error", entry.Fields["result"], "deferred input errors must precede completion logging")
		}
	}
	require.Equal(t, 1, finished)
	c.inputs.mu.Lock()
	pending := len(c.inputs.owners[ownerKey(ms)].dirty)
	c.inputs.mu.Unlock()
	require.Equal(t, 1, pending, "failed input remains available for worker retry")
}
