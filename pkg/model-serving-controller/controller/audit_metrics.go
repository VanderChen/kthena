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
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Controller-local registry avoids global registration collisions. Labels are
// bounded categories, never ModelServing names, Pod identities or error text.
type auditMetrics struct {
	registry    *prometheus.Registry
	rounds      *prometheus.CounterVec
	duration    prometheus.Histogram
	lastSuccess prometheus.Gauge
	drift       prometheus.Counter
	triggers    *prometheus.CounterVec
	reads       *prometheus.CounterVec
	pendingAge  prometheus.Histogram
}

func newAuditMetrics() *auditMetrics {
	const namespace = "kthena_modelserving"
	m := &auditMetrics{
		registry:    prometheus.NewRegistry(),
		rounds:      prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "audit_total", Help: "Live ModelServing reconciliations by result."}, []string{"result"}),
		duration:    prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Name: "audit_duration_seconds", Help: "Live observation plus reconciliation latency.", Buckets: prometheus.ExponentialBuckets(0.01, 4, 8)}),
		lastSuccess: prometheus.NewGauge(prometheus.GaugeOpts{Namespace: namespace, Name: "audit_last_success_timestamp_seconds", Help: "Unix time of the most recent successful live reconciliation."}),
		drift:       prometheus.NewCounter(prometheus.CounterOpts{Namespace: namespace, Name: "audit_cache_drift_total", Help: "Live audits that found cache object/version drift."}),
		triggers:    prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "observation_enqueues_total", Help: "Business notifications enqueued, before workqueue deduplication."}, []string{"mode"}),
		reads:       prometheus.NewCounterVec(prometheus.CounterOpts{Namespace: namespace, Name: "audit_observation_reads_total", Help: "Direct API reads constructing scoped observations, including pagination (not all reconcile API traffic)."}, []string{"resource"}),
		pendingAge:  prometheus.NewHistogram(prometheus.HistogramOpts{Namespace: namespace, Name: "audit_pending_operation_age_seconds", Help: "Age of unresolved in-memory recovery/cleanup observed at reconciliation.", Buckets: prometheus.ExponentialBuckets(1, 4, 8)}),
	}
	m.registry.MustRegister(m.rounds, m.duration, m.lastSuccess, m.drift, m.triggers, m.reads, m.pendingAge)
	return m
}

func (c *ModelServingController) registerAuditMetrics(mux *http.ServeMux) {
	m := c.audit.metrics
	m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "kthena_modelserving_queue_depth", Help: "Current workqueue depth."}, func() float64 { return float64(c.workqueue.Len()) }))
	mux.Handle("/metrics", promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}))
}

func (m *auditMetrics) finish(start time.Time, err error, state *servingAuditState) {
	result := "success"
	if err != nil {
		result = "error"
	} else {
		m.lastSuccess.SetToCurrentTime()
	}
	m.rounds.WithLabelValues(result).Inc()
	m.duration.Observe(time.Since(start).Seconds())
	if state == nil {
		return
	}
	if len(state.roleDeletes)+len(state.groupDeletes)+len(state.grace)+len(state.deleted) > 0 {
		if state.pendingSince.IsZero() {
			state.pendingSince = start
		}
		m.pendingAge.Observe(time.Since(state.pendingSince).Seconds())
	} else {
		state.pendingSince = time.Time{}
	}
}
