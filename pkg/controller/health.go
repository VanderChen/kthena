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
	"fmt"
	"sync"
)

// HealthMonitor supervises the lifetime of the controllers started by a manager.
// The first unexpected stop remains unhealthy until the process is restarted.
// Waiting for leadership and cache initialization are not failures.
type HealthMonitor struct {
	ctx    context.Context
	mu     sync.RWMutex
	err    error
	failed chan struct{}
}

// NewHealthMonitor uses the process context to distinguish graceful shutdown
// from cancellation of a controller's child context (for example, leader loss).
func NewHealthMonitor(ctx context.Context) *HealthMonitor {
	return &HealthMonitor{ctx: ctx, failed: make(chan struct{})}
}

// Check returns the first lifecycle failure without making any API requests.
func (h *HealthMonitor) Check() error {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.err
}

func (h *HealthMonitor) fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err == nil {
		h.err = err
		close(h.failed)
	}
}

func (h *HealthMonitor) start(ctx context.Context, name string, run func(context.Context) error) {
	// Detect cancellation even if Run is still blocked during initialization or
	// cleanup. Do not wait for Run to return before reporting a lost controller.
	stop := context.AfterFunc(ctx, func() {
		if h.ctx.Err() == nil {
			h.fail(fmt.Errorf("%s stopped: %w", name, ctx.Err()))
		}
	})
	go func() {
		defer stop()
		err := run(ctx)
		if h.ctx.Err() != nil {
			return
		}
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			err = fmt.Errorf("Run returned unexpectedly")
		}
		h.fail(fmt.Errorf("%s stopped: %w", name, err))
	}()
}

func (h *HealthMonitor) wait() error {
	select {
	case <-h.ctx.Done():
	case <-h.failed:
	}
	return h.Check()
}
