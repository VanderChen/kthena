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

package modelserving

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/volcano-sh/kthena/test/integration/modelserving/runner"
)

// TestLifecycleScenarios runs all lifecycle test cases from YAML files.
func TestLifecycleScenarios(t *testing.T) {
	testCasesDir := filepath.Join("testcases", "lifecycle")

	testCases, err := runner.LoadTestCasesFromDir(testCasesDir)
	require.NoError(t, err, "Failed to load test cases from %s", testCasesDir)

	if len(testCases) == 0 {
		t.Skipf("No test cases found in %s", testCasesDir)
		return
	}

	for _, tc := range testCases {
		tc := tc // capture range variable
		t.Run(tc.Metadata.Name, func(t *testing.T) {
			testRunner := newTestRunner()

			ctx := context.Background()
			if tc.Spec.Timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.Spec.Timeout)
				defer cancel()
			}

			err := testRunner.RunTestCase(ctx, tc)
			require.NoError(t, err, "Test case failed: %s", tc.Metadata.Description)
		})
	}
}

// TestLifecycleCreateDelete tests basic creation and deletion lifecycle.
func TestLifecycleCreateDelete(t *testing.T) {
	testCasePath := filepath.Join("testcases", "lifecycle", "01_create_delete.yaml")

	tc, err := runner.LoadTestCase(testCasePath)
	require.NoError(t, err, "Failed to load test case")

	testRunner := newTestRunner()

	ctx := context.Background()
	if tc.Spec.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, tc.Spec.Timeout)
		defer cancel()
	}

	err = testRunner.RunTestCase(ctx, tc)
	require.NoError(t, err, "Test case failed")
}
