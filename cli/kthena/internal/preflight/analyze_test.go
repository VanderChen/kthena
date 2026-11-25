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
	"context"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	clienttesting "k8s.io/client-go/testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

func TestAnalyze(t *testing.T) {
	s := legacySnapshot(t)
	llama := s.ModelServings[0]

	// A second ModelServing in another namespace, in the middle of a rollout
	// and with a Pod on a cordoned node.
	qwen := newModelServing("qwen", 2)
	qwen.Namespace = "team-b"
	first := steady(t, qwen)
	setImage(qwen, "model:v2")
	second := record(t, qwen, first)
	qwen.Status.UpdateRevision = identityOf(second)
	s.ModelServings = append([]*workloadv1alpha1.ModelServing{qwen}, s.ModelServings...)
	s.Revisions = append(s.Revisions, first, second)
	s.Pods = append(s.Pods, groupPods(qwen, 0, identityOf(first), onNode("node-7"))...)
	s.Pods = append(s.Pods, groupPods(qwen, 1, identityOf(second))...)
	s.CordonedNodes = []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-7"}, Spec: corev1.NodeSpec{Unschedulable: true}}}
	s.DryRuns["team-b/qwen"] = DryRunResult{Outcome: DryRunRejected, Webhook: "policy.example.com", Message: "denied"}
	// A budget of another namespace that would select the Pods of llama by label.
	s.Budgets = []*policyv1.PodDisruptionBudget{{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-b", Name: "elsewhere"},
		Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selecting(llama.Name), MaxUnavailable: ptr(intstr.FromInt32(1))},
	}}
	// A PodGroup that carries the name of llama in another namespace.
	stray := podGroupOf(llama, 9)
	stray.Namespace = "team-b"
	s.PodGroups = append(s.PodGroups, stray)

	report := Analyze(s)

	if report.FormatVersion != FormatVersion || !report.GeneratedAt.Equal(testTime) || report.DryRun != "performed" || !report.Scope.AllNamespaces {
		t.Errorf("report = %+v", report)
	}
	if len(report.ModelServings) != 2 || report.ModelServings[0].Name != "llama" || report.ModelServings[1].Name != "qwen" {
		t.Fatalf("modelServings are not sorted by namespace and name: %+v", report.ModelServings)
	}
	if got := report.ModelServings[0]; got.Verdict != VerdictReady || len(got.Findings) != 0 {
		t.Errorf("llama = %s %v, want ready: the objects of another namespace are not its own", got.Verdict, findingLevels(got.Findings))
	}
	// The findings of a ModelServing are sorted with the most severe first.
	wantQwen := []string{FindingDryRunRejected, FindingRolloutInProgress, FindingPodsOnCordonedNodes}
	qwenReport := report.ModelServings[1]
	var gotQwen []string
	for _, finding := range qwenReport.Findings {
		gotQwen = append(gotQwen, finding.ID)
	}
	if qwenReport.Verdict != VerdictBlocked || !slices.Equal(gotQwen, wantQwen) {
		t.Errorf("qwen = %s %v, want blocked %v", qwenReport.Verdict, gotQwen, wantQwen)
	}
	if want := (Summary{ModelServings: 2, Ready: 1, Blocked: 1}); report.Summary != want {
		t.Errorf("summary = %+v, want %+v", report.Summary, want)
	}
	if report.Cluster.Verdict != VerdictReady || report.Verdict != VerdictBlocked {
		t.Errorf("cluster verdict = %s, verdict = %s, want ready and blocked", report.Cluster.Verdict, report.Verdict)
	}
}

func TestAnalyzeVerdict(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(s *Snapshot)
		want   Verdict
		code   int
	}{
		{name: "everything ready", mutate: func(*Snapshot) {}, want: VerdictReady, code: ExitReady},
		{
			name:   "a ModelServing needs attention",
			mutate: func(s *Snapshot) { withoutOwner(s.Pods[0]) },
			want:   VerdictAttention, code: ExitAttention,
		},
		{
			name:   "a ModelServing has to wait",
			mutate: func(s *Snapshot) { terminating(s.Pods[0]) },
			want:   VerdictWait, code: ExitAttention,
		},
		{
			name: "wait is worse than attention",
			mutate: func(s *Snapshot) {
				withoutOwner(s.Pods[0])
				terminating(s.Pods[2])
			},
			want: VerdictWait, code: ExitAttention,
		},
		{
			name: "the cluster is blocked although every ModelServing is ready",
			mutate: func(s *Snapshot) {
				s.RollbackSwitches = []*corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: rollbackSwitchName}}}
			},
			want: VerdictBlocked, code: ExitBlocked,
		},
		{
			name: "the dry-run was skipped",
			mutate: func(s *Snapshot) {
				s.DryRunSkipped, s.DryRuns = true, map[string]DryRunResult{}
			},
			want: VerdictReady, code: ExitReady,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := legacySnapshot(t)
			tt.mutate(s)
			report := Analyze(s)
			if report.Verdict != tt.want || report.ExitCode() != tt.code {
				t.Errorf("verdict = %s, exit code = %d, want %s, %d", report.Verdict, report.ExitCode(), tt.want, tt.code)
			}
			if s.DryRunSkipped && (report.DryRun != "skipped" || report.ModelServings[0].DryRun.Outcome != DryRunSkipped) {
				t.Errorf("dryRun = %q, %+v, want skipped", report.DryRun, report.ModelServings[0].DryRun)
			}
		})
	}
}

// TestAnalyzeOneModelServing checks that with one ModelServing in scope the
// ServingGroup objects of the others are not reported as unrelated.
func TestAnalyzeOneModelServing(t *testing.T) {
	s := legacySnapshot(t)
	llama := s.ModelServings[0]
	s.Scope = Scope{Namespace: testNamespace, ModelServing: llama.Name}
	s.ServingGroupCRD = crdInstalled
	s.ServingGroups = []*workloadv1alpha1.ServingGroup{servingGroupOf(newModelServing("qwen", 1), 0, nil)}

	report := Analyze(s)
	if len(report.Cluster.ServingGroups) != 0 {
		t.Errorf("servingGroups = %+v, want none", report.Cluster.ServingGroups)
	}
	if finding := findingByID(report.Cluster.Findings, FindingScopeLimited); finding == nil || !strings.Contains(finding.Message, "ModelServing default/llama") {
		t.Errorf("the scope is not reported: %+v", report.Cluster.Findings)
	}
}

// clusterOf puts the objects of the analysis of one ModelServing into fake
// clientsets, as the cluster of an earlier version that they describe.
func clusterOf(t *testing.T, in *modelServingInput) fakes {
	t.Helper()
	c := cluster{kthena: []runtime.Object{in.ms}}
	for _, pod := range in.pods {
		c.kube = append(c.kube, pod)
	}
	for _, service := range in.services {
		c.kube = append(c.kube, service)
	}
	for _, revision := range in.revisions {
		c.kube = append(c.kube, revision)
	}
	for _, budget := range in.budgets {
		c.kube = append(c.kube, budget)
	}
	for node := range in.cordoned {
		c.kube = append(c.kube, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: node}, Spec: corev1.NodeSpec{Unschedulable: true}})
	}
	for _, podGroup := range in.podGroups {
		c.volcano = append(c.volcano, podGroup)
	}
	for _, group := range in.groups {
		c.kthena = append(c.kthena, group)
	}
	f := c.fakes()
	if len(in.groups) == 0 {
		notServed(&f.kthena.Fake, "servinggroups")
	}
	// The API server answers the dry-run write the way the case says.
	f.kthena.PrependReactor("patch", "modelservings", func(clienttesting.Action) (bool, runtime.Object, error) {
		switch result := in.dryRun; {
		case result.Outcome == DryRunRejected && result.Webhook != "":
			return true, nil, apierrors.NewBadRequest(`admission webhook "` + result.Webhook + `" denied the request: ` + result.Message)
		case result.Outcome == DryRunRejected:
			return true, nil, apierrors.NewInvalid(workloadv1alpha1.ModelServingKind.GroupKind(), in.ms.Name, nil)
		case result.Outcome == DryRunUnknown:
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "modelservings"}, in.ms.Name, errors.New(result.Message))
		}
		return true, in.ms, nil
	})
	return f
}

// TestFindingsThroughClients runs every case of the analysis of a
// ModelServing once more with the objects read through the clients: what
// Collect reads and Analyze hands on must lead to the same findings.
func TestFindingsThroughClients(t *testing.T) {
	cases := slices.Concat(stateCases(), revisionCases(), groupCases(), podStateCases(), strayPodCases(),
		existingObjectCases(), dryRunCases(), environmentCases())
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			in := tt.input(t)
			f := clusterOf(t, in)
			s, err := Collect(context.Background(), f.clients(), CollectOptions{
				Scope:      Scope{Namespace: in.ms.Namespace},
				SkipDryRun: in.dryRun.Outcome == DryRunSkipped,
			})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			report := Analyze(s)
			if len(report.ModelServings) != 1 {
				t.Fatalf("modelServings = %+v, want one", report.ModelServings)
			}
			got := report.ModelServings[0]
			if levels := findingLevels(got.Findings); !maps.Equal(levels, tt.want) {
				t.Errorf("findings = %v, want %v", levels, tt.want)
			}
			if got.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %s, want %s", got.Verdict, tt.wantVerdict)
			}
			if want := worse(tt.wantVerdict, report.Cluster.Verdict); report.Verdict != want || report.ExitCode() != map[Verdict]int{
				VerdictReady: ExitReady, VerdictAttention: ExitAttention, VerdictWait: ExitAttention, VerdictBlocked: ExitBlocked}[want] {
				t.Errorf("verdict of the report = %s with exit code %d, want %s", report.Verdict, report.ExitCode(), want)
			}
		})
	}
}

// TestAnalyzeReportsReadFailures checks that a read that Collect could not
// make ends up in the report, and that the check does not pass because of it.
func TestAnalyzeReportsReadFailures(t *testing.T) {
	f := legacyCluster(t).fakes()
	f.kube.PrependReactor("list", "nodes", func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "nodes"}, "", errors.New("not allowed"))
	})
	s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	report := Analyze(s)
	finding := findingByID(report.Cluster.Findings, FindingReadFailed)
	if finding == nil || finding.Level != LevelAttention || !strings.Contains(finding.Message, "list the cordoned nodes") || !strings.Contains(finding.Message, "not allowed") {
		t.Errorf("the failed read is not reported: %+v", report.Cluster.Findings)
	}
	if report.Verdict != VerdictAttention || report.ExitCode() != ExitAttention {
		t.Errorf("verdict = %s, exit code = %d, want attention", report.Verdict, report.ExitCode())
	}
}
