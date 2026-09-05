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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestAuditConfigurationAndMetrics(t *testing.T) {
	ms, pods := auditFixture(workloadv1alpha1.NoneRestartPolicy, 0)
	c, _ := auditController(t, ms, pods...)
	require.Equal(t, 5*time.Minute, c.auditPeriod)
	require.Equal(t, 30*time.Second, c.auditTimeout)
	require.Error(t, c.ConfigureAudit(-time.Second, time.Second))
	require.Error(t, c.ConfigureAudit(time.Second, 0))
	require.NoError(t, c.ConfigureAudit(0, time.Second))
	c.runPeriodicAudit(context.Background()) // disabled ticker returns immediately
	runAudit(t, c, ms)                       // targeted live reconciliation is still enabled
	families, err := c.audit.metrics.registry.Gather()
	require.NoError(t, err)
	for _, family := range families {
		if family.GetName() == "kthena_modelserving_audit_total" || family.GetName() == "kthena_modelserving_audit_cache_drift_total" {
			require.Equal(t, float64(1), family.Metric[0].GetCounter().GetValue())
		}
	}
	mux := http.NewServeMux()
	c.RegisterModelServingDebugEndpoints(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Contains(t, w.Body.String(), "kthena_modelserving_audit_last_success_timestamp_seconds")
	require.Contains(t, w.Body.String(), "kthena_modelserving_queue_depth")
	require.NotContains(t, w.Body.String(), string(pods[0].UID))
}
