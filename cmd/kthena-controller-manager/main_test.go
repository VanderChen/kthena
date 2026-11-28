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

package main

import (
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/pflag"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/volcano-sh/kthena/pkg/controller"
)

func TestParseControllers(t *testing.T) {
	tests := []struct {
		name     string
		input    []string
		expected map[string]bool
	}{
		{
			name:  "wildcard_only",
			input: []string{"*"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "wildcard_with_other_controllers",
			input: []string{"*", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "single_controller_modelserving",
			input: []string{"modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "single_controller_modelbooster",
			input: []string{"modelbooster"},
			expected: map[string]bool{
				controller.ModelBoosterController: true,
			},
		},
		{
			name:     "single_controller_autoscaler",
			input:    []string{"autoscaler"},
			expected: map[string]bool{},
		},
		{
			name:  "multiple_controllers",
			input: []string{"modelserving", "modelbooster"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "all_controllers_explicit",
			input: []string{"modelserving", "modelbooster", "autoscaler"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "controllers_with_spaces",
			input: []string{" modelserving ", " modelbooster "},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "invalid_controller_name",
			input: []string{"invalid"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "mixed_valid_and_invalid_controllers",
			input: []string{"modelserving", "invalid"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "only_commas",
			input: []string{",,"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "invalid_with_no_valid_controllers",
			input: []string{"invalid1", "invalid2"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "duplicate_valid_controllers",
			input: []string{"modelserving", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "invalid_controller",
			input: []string{"*modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		// The autoscaler is not a controller of this binary any more. Its name still
		// selects what it selected, so that a list which names it does not fall back to
		// everything. The workload chart mirrors the cases below in
		// charts/kthena/charts/workload/templates/_helpers.tpl to decide whether to
		// deploy kthena-autoscaler. Change both together.
		{
			name:  "wildcard_with_exclusion",
			input: []string{"*", "-autoscaler"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "exclusion_before_wildcard",
			input: []string{"-autoscaler", "*"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "exclusion_before_explicit_controller",
			input: []string{"-autoscaler", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:  "exclusion_alone_enables_all",
			input: []string{"-autoscaler"},
			expected: map[string]bool{
				controller.ModelServingController: true,
				controller.ModelBoosterController: true,
			},
		},
		{
			name:  "first_token_wins_exclusion",
			input: []string{"-autoscaler", "+autoscaler", "modelserving"},
			expected: map[string]bool{
				controller.ModelServingController: true,
			},
		},
		{
			name:     "first_token_wins_inclusion",
			input:    []string{"+autoscaler", "-autoscaler"},
			expected: map[string]bool{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseControllers(tt.input)
			assert.Equal(t, tt.expected, result, "parseControllers(%q) result mismatch", tt.input)
		})
	}
}

// The workload chart works out from --controllers whether a release ran the autoscaler,
// and can only do that for the names it knows.
func TestChartHelperKnowsEveryController(t *testing.T) {
	helper, err := os.ReadFile(filepath.Join("..", "..", "charts", "kthena", "charts", "workload", "templates", "_helpers.tpl"))
	require.NoError(t, err)

	match := regexp.MustCompile(`(?s)define "kthena\.workload\.controllerNames" -}}\s*(\S+)\s*{{- end`).FindSubmatch(helper)
	require.NotNil(t, match, "kthena.workload.controllerNames not found in the workload chart helpers")

	expected := []string{controller.AutoscalerController}
	for name := range parseControllers([]string{"*"}) {
		expected = append(expected, name)
	}
	assert.ElementsMatch(t, expected, strings.Split(string(match[1]), ","))
}

// A command line written for a release whose controller-manager ran the autoscaler must still parse.
func TestDeprecatedFlagsAreAccepted(t *testing.T) {
	for _, args := range [][]string{
		{"--autoscaling-sync-period-seconds=30"},
		{"--autoscaling-sync-period-seconds", "30"},
	} {
		fs := pflag.NewFlagSet("kthena-controller-manager", pflag.ContinueOnError)
		fs.SetOutput(io.Discard)
		require.NoError(t, addDeprecatedFlags(fs))
		assert.NoError(t, fs.Parse(args), "%v", args)
		assert.Empty(t, fs.Args(), "%v", args)
	}
}
