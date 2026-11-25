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

package preflight

import (
	"slices"
	"sort"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// objectOfModelServing is an object that carries the name label of a
// ModelServing.
type objectOfModelServing interface {
	GetNamespace() string
	GetLabels() map[string]string
}

// byModelServing sorts objects by the ModelServing they are labelled with,
// keyed by namespace/name.
func byModelServing[T objectOfModelServing](objects []T) map[string][]T {
	out := map[string][]T{}
	for _, object := range objects {
		key := object.GetNamespace() + "/" + object.GetLabels()[workloadv1alpha1.ModelServingNameLabelKey]
		out[key] = append(out[key], object)
	}
	return out
}

// Analyze turns what was read into the report. It reads nothing itself.
func Analyze(s *Snapshot) *Report {
	report := &Report{
		FormatVersion: FormatVersion,
		GeneratedAt:   s.Time.UTC(),
		Scope:         s.Scope,
		DryRun:        "performed",
		Verdict:       VerdictReady,
		ModelServings: []ModelServingReport{},
	}
	if s.DryRunSkipped {
		report.DryRun = "skipped"
	}

	pods, services := byModelServing(s.Pods), byModelServing(s.Services)
	podGroups, revisions := byModelServing(s.PodGroups), byModelServing(s.Revisions)
	// A ServingGroup belongs to the ModelServing its name suggests.
	groups := map[string][]*workloadv1alpha1.ServingGroup{}
	for _, object := range s.ServingGroups {
		if prefix, _, ok := splitOrdinal(object.Name); ok {
			key := object.Namespace + "/" + prefix
			groups[key] = append(groups[key], object)
		}
	}
	cordoned := map[string]bool{}
	for _, node := range s.CordonedNodes {
		cordoned[node.Name] = true
	}

	modelServings := slices.Clone(s.ModelServings)
	sort.Slice(modelServings, func(i, j int) bool {
		if modelServings[i].Namespace != modelServings[j].Namespace {
			return modelServings[i].Namespace < modelServings[j].Namespace
		}
		return modelServings[i].Name < modelServings[j].Name
	})
	for _, ms := range modelServings {
		key := ms.Namespace + "/" + ms.Name
		in := modelServingInput{
			ms: ms, pods: pods[key], services: services[key], podGroups: podGroups[key], revisions: revisions[key],
			groups: groups[key], cordoned: cordoned, now: s.Time,
			dryRun: DryRunResult{Outcome: DryRunSkipped},
		}
		for _, budget := range s.Budgets {
			if budget.Namespace == ms.Namespace {
				in.budgets = append(in.budgets, budget)
			}
		}
		if result, ok := s.DryRuns[key]; ok {
			in.dryRun = result
		}
		result := analyzeModelServing(in)
		report.ModelServings = append(report.ModelServings, result)

		report.Summary.ModelServings++
		switch result.Verdict {
		case VerdictReady:
			report.Summary.Ready++
		case VerdictAttention:
			report.Summary.Attention++
		case VerdictWait:
			report.Summary.Wait++
		case VerdictBlocked:
			report.Summary.Blocked++
		}
		report.Verdict = worse(report.Verdict, result.Verdict)
	}

	report.Cluster = analyzeCluster(s)
	report.Verdict = worse(report.Verdict, report.Cluster.Verdict)
	return report
}

// ExitCode returns the exit code the report stands for.
func (r *Report) ExitCode() int {
	switch r.Verdict {
	case VerdictBlocked:
		return ExitBlocked
	case VerdictWait, VerdictAttention:
		return ExitAttention
	}
	return ExitReady
}
