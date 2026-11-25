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
	"bytes"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// mixedSnapshot returns a cluster of an earlier version with three
// ModelServings: default/llama in steady state, default/qwen in a rollout on
// sparse ordinals that created its extra group below spec.replicas, and
// team-b/mistral with a Pod that is not a member of a group.
func mixedSnapshot(t *testing.T) *Snapshot {
	t.Helper()
	s := legacySnapshot(t)

	qwen := newModelServing("qwen", 2)
	old := steady(t, qwen)
	setImage(qwen, "model:v2")
	qwen.Generation, qwen.Status.ObservedGeneration = 2, 2
	updated := record(t, qwen, old)
	qwen.Status.UpdateRevision, qwen.Status.UpdatedReplicas = identityOf(updated), 1
	s.ModelServings = append(s.ModelServings, qwen)
	s.Revisions = append(s.Revisions, old, updated)
	s.Pods = append(s.Pods, groupsPods(qwen, identityOf(old), 0, 3)...)
	s.Pods = append(s.Pods, groupPods(qwen, 1, identityOf(updated), notReady)...)
	s.DryRuns["default/qwen"] = DryRunResult{Outcome: DryRunPassed}

	mistral := newModelServing("mistral", 1)
	mistral.Namespace = "team-b"
	revision := steady(t, mistral)
	stray := memberPod(mistral, 0, "leader-0", 0, identityOf(revision))
	stray.Name, stray.UID = "mistral-debug", "uid-mistral-debug"
	s.ModelServings = append(s.ModelServings, mistral)
	s.Revisions = append(s.Revisions, revision)
	s.Pods = append(append(s.Pods, groupPods(mistral, 0, identityOf(revision))...), stray)
	s.DryRuns["team-b/mistral"] = DryRunResult{Outcome: DryRunPassed}

	s.CordonedNodes = []*corev1.Node{{ObjectMeta: metav1.ObjectMeta{Name: "node-9"}, Spec: corev1.NodeSpec{Unschedulable: true}}}
	return s
}

func TestReportTable(t *testing.T) {
	report := Analyze(mixedSnapshot(t))
	var out bytes.Buffer
	if err := report.WriteTable(&out); err != nil {
		t.Fatalf("WriteTable() error = %v", err)
	}
	text := out.String()
	for _, want := range []string{
		"Scope: all namespaces   Dry-run: performed   Time: 2026-11-25T04:00:00Z",
		"CLUSTER: ready",
		"  Controller manager:    kthena-system/kthena-controller-manager",
		"    Image:               ghcr.io/volcano-sh/kthena-controller-manager:v1.1.0",
		"    Replicas:            1 (1 ready), update strategy RollingUpdate",
		"    Leader election:     off (--leader-elect not set: default of the version)",
		"    Version:             earlier version: no Pod carries the controller-protocol label",
		"  ServingGroup CRD:      not-installed",
		"  Cordoned nodes:        node-9 (0 ModelServing Pods)",
		"  Rollback switch:       none",
		"[info] servinggroup-crd-not-installed",
		"NAMESPACE   NAME      VERDICT   STRATEGY                    REPLICAS   GROUPS      AVAILABLE   PARTITION   ROLLOUT AFTER UPGRADE   DRY-RUN",
		"default     llama     ready     ServingGroupRollingUpdate   3          3 (0-2)     3/3         0           none                    passed",
		"default     qwen      wait      ServingGroupRollingUpdate   2          3 (0-1,3)   2/2         0           2 groups                passed",
		"team-b      mistral   blocked   ServingGroupRollingUpdate   1          1 (0)       1/1         0           none                    passed",
		"default/qwen: wait",
		"; 2 groups will be rolled after the upgrade",
		"  GROUP    PODS   READY   TERMINATING   REVISION",
		"[wait] groups-exceed-replicas",
		"[wait] rollout-in-progress",
		"team-b/mistral: blocked",
		"[blocked] pod-not-a-member",
		"Action: Delete these Pods, or remove their owner reference, before upgrading.",
		"Summary: 3 ModelServings: 1 ready, 0 attention, 1 wait, 1 blocked; cluster: ready",
		"Result: blocked (exit code 3)",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the table lacks %q", want)
		}
	}
	// A ModelServing without findings has no section of its own.
	if strings.Contains(text, "default/llama:") {
		t.Errorf("the table has a section for a ModelServing without findings")
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "      ") && len(line) > textWidth {
			t.Errorf("a message line is longer than %d characters: %q", textWidth, line)
		}
	}
	if t.Failed() {
		t.Logf("table:\n%s", text)
	}
}

func TestReportTableWithoutModelServings(t *testing.T) {
	tests := []struct {
		name  string
		scope Scope
		want  string
	}{
		{"one namespace", Scope{Namespace: "empty"}, "No ModelServings found in namespace empty."},
		{"all namespaces", Scope{AllNamespaces: true}, "No ModelServings found across all namespaces."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := legacySnapshot(t)
			s.Scope, s.ModelServings, s.Pods = tt.scope, nil, nil
			var out bytes.Buffer
			if err := Analyze(s).WriteTable(&out); err != nil {
				t.Fatalf("WriteTable() error = %v", err)
			}
			if !strings.Contains(out.String(), tt.want) || !strings.Contains(out.String(), "Result: ready (exit code 0)") {
				t.Errorf("table:\n%s", out.String())
			}
		})
	}
}

// TestReportJSONFieldNames pins the field names of the JSON output: scripts
// read them.
func TestReportJSONFieldNames(t *testing.T) {
	s := mixedSnapshot(t)
	s.ServingGroupCRD = crdInstalled
	s.ServingGroups = []*workloadv1alpha1.ServingGroup{servingGroupOf(s.ModelServings[0], 0, nil)}
	report := decode(t, Analyze(s))

	check := func(what string, value any, want ...string) {
		t.Helper()
		if got := keysOf(t, value); !slices.Equal(got, want) {
			t.Errorf("fields of %s = %v, want %v", what, got, want)
		}
	}
	first := func(value any) any {
		t.Helper()
		list, ok := value.([]any)
		if !ok || len(list) == 0 {
			t.Fatalf("%v is not a list with an element", value)
		}
		return list[0]
	}
	check("the report", report, "cluster", "dryRun", "formatVersion", "generatedAt", "modelServings", "scope", "summary", "verdict")
	check("scope", report["scope"], "allNamespaces", "namespace")
	check("summary", report["summary"], "attention", "blocked", "modelServings", "ready", "wait")

	cluster := report["cluster"].(map[string]any)
	check("cluster", cluster, "controllerManagers", "cordonedNodes", "evictionWebhook", "findings", "rollbackSwitches",
		"servingGroupCRD", "servingGroups", "verdict", "webhooks")
	manager := first(cluster["controllerManagers"]).(map[string]any)
	check("a controller manager", manager, "deployment", "image", "leaderElect", "leaderElectFlag", "namespace", "pods",
		"protocol", "readyReplicas", "replicas", "updateStrategy")
	check("a controller manager Pod", first(manager["pods"]), "name", "node", "phase")
	check("a cordoned node", first(cluster["cordonedNodes"]), "modelServingPods", "name")
	check("a ServingGroup", first(cluster["servingGroups"]), "controlled", "modelServing", "name", "namespace")
	// The last finding is the one about cordoned nodes, which has every field.
	findings := cluster["findings"].([]any)
	check("a cluster finding", findings[len(findings)-1], "action", "id", "level", "message", "objects")

	// default/qwen is the second ModelServing.
	qwen := report["modelServings"].([]any)[1].(map[string]any)
	check("a ModelServing", qwen, "dryRun", "findings", "generation", "groups", "name", "namespace", "podChangesExpected",
		"revision", "rollout", "status", "uid", "verdict")
	check("rollout", qwen["rollout"], "holes", "ordinals", "partition", "recoveryPolicy", "replicas", "strategy")
	check("revision", qwen["revision"], "controllerRevision", "heldByPartition", "statement", "target", "targetSource",
		"toRoll", "unit", "units", "unknown")
	check("a group", first(qwen["groups"]), "atTarget", "deletionCost", "name", "ordinal", "pods", "ready", "readyPods",
		"revisions", "surgeAfterTakeover", "surgeMarkedPods", "terminatingPods")
	check("dryRun", qwen["dryRun"], "outcome")
	check("status", qwen["status"], "availableReplicas", "currentReplicas", "currentRevision", "observedGeneration",
		"replicas", "updateRevision", "updatedReplicas")
	check("a finding", first(qwen["findings"]), "action", "id", "level", "message", "objects")

	if report["verdict"] != "blocked" || qwen["verdict"] != "wait" || qwen["name"] != "qwen" {
		t.Errorf("verdict = %v, modelServings[1] = %v %v", report["verdict"], qwen["name"], qwen["verdict"])
	}
	// Placeholders in messages are written as they are, not as \u003c.
	var raw bytes.Buffer
	if err := WriteJSON(&raw, Analyze(s)); err != nil {
		t.Fatalf("WriteJSON() error = %v", err)
	}
	if !strings.Contains(raw.String(), "<group>-<role>-<instance>-<member>") {
		t.Errorf("the JSON output escapes the placeholders of a message")
	}
	// Lists are never null: a script can iterate over them without a check.
	llama := report["modelServings"].([]any)[0].(map[string]any)
	for _, field := range []string{"findings", "podChangesExpected", "groups"} {
		if _, ok := llama[field].([]any); !ok {
			t.Errorf("%s of a ModelServing without findings = %v, want a list", field, llama[field])
		}
	}
}
