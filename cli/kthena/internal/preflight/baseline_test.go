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
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestNewBaseline(t *testing.T) {
	s := legacySnapshot(t)
	ms := s.ModelServings[0]
	restarted(2)(s.Pods[0])
	s.Pods[0].Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
	terminating(s.Pods[5])
	// A PodGroup of a group without Pods, and the objects of a ModelServing
	// that is not in scope.
	s.PodGroups = append(s.PodGroups, podGroupOf(ms, 5), podGroupOf(newModelServing("qwen", 1), 0))
	s.Pods = append(s.Pods, groupPods(newModelServing("qwen", 1), 0, "any")...)

	baseline := NewBaseline(s, Analyze(s))

	if baseline.Kind != baselineKind || baseline.FormatVersion != FormatVersion || !baseline.CreatedAt.Equal(testTime) || baseline.Scope != s.Scope {
		t.Errorf("baseline = %+v", baseline)
	}
	wantModelServing := BaselineModelServing{
		Namespace: testNamespace, Name: "llama", UID: "uid-llama", Generation: 1,
		Verdict: VerdictWait, PodChangesExpected: []string{FindingTerminatingPods},
		TargetRevision: ms.Status.UpdateRevision, SurgeGroups: []string{},
		Status: StatusInfo{ObservedGeneration: 1, Replicas: 3, CurrentReplicas: 3, UpdatedReplicas: 3, AvailableReplicas: 3,
			CurrentRevision: ms.Status.CurrentRevision, UpdateRevision: ms.Status.UpdateRevision},
	}
	if len(baseline.ModelServings) != 1 || !reflect.DeepEqual(baseline.ModelServings[0], wantModelServing) {
		t.Errorf("modelServings = %+v, want %+v", baseline.ModelServings, wantModelServing)
	}

	if len(baseline.Pods) != 6 {
		t.Fatalf("pods = %d, want the 6 Pods of llama", len(baseline.Pods))
	}
	since := readySince.UTC()
	wantPod := BaselinePod{
		Namespace: testNamespace, Name: "llama-0-leader-0-0", UID: "uid-llama-0-leader-0-0",
		ModelServing: "llama", Group: "llama-0", Node: "node-1", Phase: "Running", Revision: ms.Status.UpdateRevision,
		Ready: true, ReadyLastTransitionTime: &since,
		Containers: []BaselineContainer{{Name: "init", Init: true, RestartCount: 1}, {Name: "model", RestartCount: 2}},
	}
	if !reflect.DeepEqual(baseline.Pods[0], wantPod) {
		t.Errorf("pods[0] = %+v, want %+v", baseline.Pods[0], wantPod)
	}
	if last := baseline.Pods[5]; last.Name != "llama-2-leader-0-1" || !last.Terminating {
		t.Errorf("pods[5] = %+v, want the terminating Pod, sorted last", last)
	}

	wantPodGroups := []BaselineObject{
		{Namespace: testNamespace, Name: "llama-0", UID: "uid-podgroup-llama-0", ModelServing: "llama", Group: "llama-0", GroupHasPods: true},
		{Namespace: testNamespace, Name: "llama-1", UID: "uid-podgroup-llama-1", ModelServing: "llama", Group: "llama-1", GroupHasPods: true},
		{Namespace: testNamespace, Name: "llama-2", UID: "uid-podgroup-llama-2", ModelServing: "llama", Group: "llama-2", GroupHasPods: true},
		{Namespace: testNamespace, Name: "llama-5", UID: "uid-podgroup-llama-5", ModelServing: "llama", Group: "llama-5", GroupHasPods: false},
	}
	if !reflect.DeepEqual(baseline.PodGroups, wantPodGroups) {
		t.Errorf("podGroups = %+v, want %+v", baseline.PodGroups, wantPodGroups)
	}
	if len(baseline.Services) != 3 || baseline.Services[0].Name != "llama-0-leader-0-0" || baseline.Services[0].UID != "uid-service-llama-0-leader-0-0" || !baseline.Services[0].GroupHasPods {
		t.Errorf("services = %+v", baseline.Services)
	}
}

func TestNewBaselineRecordsSurgeGroups(t *testing.T) {
	s := legacySnapshot(t)
	ms := s.ModelServings[0]
	s.Pods = append(s.Pods, groupPods(ms, 3, ms.Status.UpdateRevision, surgeMarked)...)
	baseline := NewBaseline(s, Analyze(s))
	if got := baseline.ModelServings[0].SurgeGroups; !slices.Equal(got, []string{"llama-3"}) {
		t.Errorf("surgeGroups = %v, want llama-3", got)
	}
}

func TestBaselineRoundTrip(t *testing.T) {
	s := legacySnapshot(t)
	restarted(2)(s.Pods[0])
	terminating(s.Pods[5])
	want := NewBaseline(s, Analyze(s))

	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := WriteBaseline(path, want); err != nil {
		t.Fatalf("WriteBaseline() error = %v", err)
	}
	got, err := ReadBaseline(path)
	if err != nil {
		t.Fatalf("ReadBaseline() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the baseline read back differs:\n got %+v\nwant %+v", got, want)
	}
}

func TestReadBaselineRejectsOtherFiles(t *testing.T) {
	tests := []struct {
		name     string
		content  string
		wantText string
	}{
		{"not JSON", "namespace: default\n", "failed to decode the baseline"},
		{"JSON that is not a baseline", `{"formatVersion":"v1","verdict":"ready"}`, "is not a baseline written with --baseline-file"},
		{"a baseline of another format version", `{"kind":"KthenaUpgradeBaseline","formatVersion":"v0"}`, `has format version "v0"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "baseline.json")
			if err := os.WriteFile(path, []byte(tt.content), 0o600); err != nil {
				t.Fatalf("failed to write the file: %v", err)
			}
			if _, err := ReadBaseline(path); err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("ReadBaseline() error = %v, want %q", err, tt.wantText)
			}
		})
	}
	if _, err := ReadBaseline(filepath.Join(t.TempDir(), "missing.json")); err == nil || !strings.Contains(err.Error(), "failed to read the baseline") {
		t.Errorf("ReadBaseline() error = %v for a file that does not exist", err)
	}
}
