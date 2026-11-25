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
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// takenOver returns the snapshot of the same cluster after an upgrade that
// went as designed: the ServingGroup resource is served, every ModelServing
// is annotated Done, every group that has Pods has its ServingGroup object,
// and that object is the controller owner of the Pods, the PodGroup and the
// Services of the group. Nothing else differs.
func takenOver(before *Snapshot) *Snapshot {
	after := &Snapshot{
		Scope:           before.Scope,
		Time:            before.Time.Add(10 * time.Minute),
		DryRunSkipped:   true,
		ServingGroupCRD: crdInstalled,
		DryRuns:         map[string]DryRunResult{},
	}
	owners := map[string]*workloadv1alpha1.ServingGroup{}
	for _, ms := range before.ModelServings {
		ms = ms.DeepCopy()
		ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionDone}
		after.ModelServings = append(after.ModelServings, ms)
		for _, pod := range before.Pods {
			group := pod.Labels[workloadv1alpha1.GroupNameLabelKey]
			key := pod.Namespace + "/" + group
			if pod.Namespace != ms.Namespace || pod.Labels[workloadv1alpha1.ModelServingNameLabelKey] != ms.Name ||
				pod.DeletionTimestamp != nil || !controlledBy(pod, ms.UID) || owners[key] != nil {
				continue
			}
			_, ordinal, _ := splitOrdinal(group)
			owners[key] = servingGroupOf(ms, ordinal, ms)
			after.ServingGroups = append(after.ServingGroups, owners[key])
		}
	}
	adopt := func(object metav1.Object) {
		owner := owners[object.GetNamespace()+"/"+object.GetLabels()[workloadv1alpha1.GroupNameLabelKey]]
		if owner != nil && metav1.GetControllerOfNoCopy(object) != nil {
			object.SetOwnerReferences([]metav1.OwnerReference{*metav1.NewControllerRef(owner, workloadv1alpha1.ServingGroupKind)})
		}
	}
	for _, pod := range before.Pods {
		pod = pod.DeepCopy()
		if pod.DeletionTimestamp == nil {
			adopt(pod)
			pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = "0"
		}
		after.Pods = append(after.Pods, pod)
	}
	for _, podGroup := range before.PodGroups {
		podGroup = podGroup.DeepCopy()
		adopt(podGroup)
		after.PodGroups = append(after.PodGroups, podGroup)
	}
	for _, service := range before.Services {
		service = service.DeepCopy()
		adopt(service)
		after.Services = append(after.Services, service)
	}
	return after
}

func podNamed(s *Snapshot, name string) *corev1.Pod {
	for _, pod := range s.Pods {
		if pod.Name == name {
			return pod
		}
	}
	return nil
}

func withoutPod(s *Snapshot, name string) {
	s.Pods = slices.DeleteFunc(s.Pods, func(pod *corev1.Pod) bool { return pod.Name == name })
}

func TestCompare(t *testing.T) {
	type found struct{ severity, change, kind, name string }
	tests := []struct {
		name string
		// before changes the cluster the baseline is taken from.
		before func(s *Snapshot)
		// after changes the cluster after an upgrade that went as designed.
		after      func(s *Snapshot)
		want       []found
		wantResult string
		check      func(t *testing.T, c *Comparison)
	}{
		{
			name:       "an upgrade that went as designed: only the owners changed",
			wantResult: ResultPassed,
			check: func(t *testing.T, c *Comparison) {
				if want := (ComparisonSummary{ModelServings: 1, Pods: 6, PodGroups: 3, Services: 3}); c.Summary != want {
					t.Errorf("summary = %+v, want %+v", c.Summary, want)
				}
			},
		},
		{
			name: "a Pod was deleted and created again",
			after: func(s *Snapshot) {
				podNamed(s, "llama-1-leader-0-0").UID = "uid-new"
			},
			want:       []found{{SeverityViolation, ChangePodRecreated, "Pod", "llama-1-leader-0-0"}},
			wantResult: ResultFailed,
			check: func(t *testing.T, c *Comparison) {
				if detail := c.Changes[0].Detail; !strings.Contains(detail, "UID uid-new, was uid-llama-1-leader-0-0") {
					t.Errorf("detail = %s", detail)
				}
			},
		},
		{
			name:       "a Pod is missing",
			after:      func(s *Snapshot) { withoutPod(s, "llama-1-leader-0-1") },
			want:       []found{{SeverityViolation, ChangePodMissing, "Pod", "llama-1-leader-0-1"}},
			wantResult: ResultFailed,
			check: func(t *testing.T, c *Comparison) {
				if detail := c.Changes[0].Detail; !strings.Contains(detail, `node "node-1"`) {
					t.Errorf("detail = %s", detail)
				}
			},
		},
		{
			name:       "the restart count of a container increased",
			before:     func(s *Snapshot) { restarted(1)(s.Pods[0]) },
			after:      func(s *Snapshot) { restarted(3)(s.Pods[0]) },
			want:       []found{{SeverityViolation, ChangePodRestarted, "Pod", "llama-0-leader-0-0"}},
			wantResult: ResultFailed,
			check: func(t *testing.T, c *Comparison) {
				if detail := c.Changes[0].Detail; !strings.Contains(detail, "model: 3, was 1") {
					t.Errorf("detail = %s", detail)
				}
			},
		},
		{
			name:       "a restart count that was not zero and did not change",
			before:     func(s *Snapshot) { restarted(4)(s.Pods[0]) },
			wantResult: ResultPassed,
		},
		{
			name:       "a Pod that was ready is not ready",
			after:      func(s *Snapshot) { notReady(s.Pods[0]) },
			want:       []found{{SeverityViolation, ChangePodNotReady, "Pod", "llama-0-leader-0-0"}},
			wantResult: ResultFailed,
		},
		{
			name: "a Pod was not ready for a time and is ready again",
			after: func(s *Snapshot) {
				s.Pods[0].Status.Conditions[0].LastTransitionTime = metav1.NewTime(testTime.Add(5 * time.Minute))
			},
			want:       []found{{SeverityViolation, ChangePodReadinessInterrupted, "Pod", "llama-0-leader-0-0"}},
			wantResult: ResultFailed,
		},
		{
			name:       "a Pod that was not ready became ready",
			before:     func(s *Snapshot) { notReady(s.Pods[0]) },
			after:      func(s *Snapshot) { s.Pods[0].Status.Conditions[0].Status = corev1.ConditionTrue },
			wantResult: ResultPassed,
		},
		{
			name:       "a Pod is being deleted",
			after:      func(s *Snapshot) { terminating(s.Pods[0]) },
			want:       []found{{SeverityViolation, ChangePodTerminating, "Pod", "llama-0-leader-0-0"}},
			wantResult: ResultFailed,
		},
		{
			name:       "a Pod that was terminating is gone",
			before:     func(s *Snapshot) { terminating(s.Pods[0]) },
			after:      func(s *Snapshot) { withoutPod(s, "llama-0-leader-0-0") },
			want:       []found{{SeverityInfo, ChangePodGone, "Pod", "llama-0-leader-0-0"}},
			wantResult: ResultPassed,
		},
		{
			name: "a Pod was created after the baseline",
			after: func(s *Snapshot) {
				s.Pods = append(s.Pods, memberPod(s.ModelServings[0], 0, "leader-1", 0, "any"))
			},
			want:       []found{{SeverityInfo, ChangePodAdded, "Pod", "llama-0-leader-1-0"}},
			wantResult: ResultPassed,
		},
		{
			name: "labelled Pods that are not members of a group need no ServingGroup object",
			before: func(s *Snapshot) {
				ms := s.ModelServings[0]
				foreign := memberPod(ms, 7, "leader-0", 0, "any", func(pod *corev1.Pod) {
					pod.OwnerReferences = []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: "other", UID: "uid-other", Controller: ptr(true)}}
				})
				debug := memberPod(ms, 8, "leader-0", 0, "any", withoutOwner)
				debug.Name, debug.UID = "llama-debug", "uid-debug"
				s.Pods = append(s.Pods, foreign, debug)
			},
			wantResult: ResultPassed,
		},
		{
			name:       "the PodGroup of a group with Pods was created again",
			after:      func(s *Snapshot) { s.PodGroups[1].UID = "uid-new" },
			want:       []found{{SeverityViolation, ChangeObjectRecreated, "PodGroup", "llama-1"}},
			wantResult: ResultFailed,
		},
		{
			name:       "the PodGroup of a group with Pods is missing",
			after:      func(s *Snapshot) { s.PodGroups = s.PodGroups[:2] },
			want:       []found{{SeverityViolation, ChangeObjectMissing, "PodGroup", "llama-2"}},
			wantResult: ResultFailed,
		},
		{
			name:       "a headless Service of a group with Pods was created again",
			after:      func(s *Snapshot) { s.Services[0].UID = "uid-new" },
			want:       []found{{SeverityViolation, ChangeObjectRecreated, "Service", "llama-0-leader-0-0"}},
			wantResult: ResultFailed,
		},
		{
			name: "the PodGroup of a group without Pods is deleted by the takeover",
			before: func(s *Snapshot) {
				s.PodGroups = append(s.PodGroups, podGroupOf(s.ModelServings[0], 5))
			},
			after: func(s *Snapshot) {
				s.PodGroups = slices.DeleteFunc(s.PodGroups, func(podGroup *schedulingv1beta1.PodGroup) bool { return podGroup.Name == "llama-5" })
			},
			want:       []found{{SeverityInfo, ChangeObjectMissing, "PodGroup", "llama-5"}},
			wantResult: ResultPassed,
		},
		{
			name:       "status.availableReplicas dropped",
			after:      func(s *Snapshot) { s.ModelServings[0].Status.AvailableReplicas = 2 },
			want:       []found{{SeverityViolation, ChangeAvailableDropped, "ModelServing", "llama"}},
			wantResult: ResultFailed,
		},
		{
			name:       "status.availableReplicas rose",
			before:     func(s *Snapshot) { s.ModelServings[0].Status.AvailableReplicas = 2 },
			after:      func(s *Snapshot) { s.ModelServings[0].Status.AvailableReplicas = 3 },
			wantResult: ResultPassed,
		},
		{
			name: "the takeover has not finished",
			after: func(s *Snapshot) {
				s.ModelServings[0].Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] = workloadv1alpha1.ServingGroupAdoptionInProgress
			},
			want:       []found{{SeverityViolation, ChangeTakeoverNotDone, "ModelServing", "llama"}},
			wantResult: ResultFailed,
		},
		{
			name: "a group with Pods has no ServingGroup object",
			after: func(s *Snapshot) {
				s.ServingGroups = slices.DeleteFunc(s.ServingGroups, func(g *workloadv1alpha1.ServingGroup) bool { return g.Name == "llama-1" })
			},
			want:       []found{{SeverityViolation, ChangeGroupWithoutObject, "ServingGroup", "llama-1"}},
			wantResult: ResultFailed,
		},
		{
			name: "the ServingGroup object of a group is controlled by something else",
			after: func(s *Snapshot) {
				s.ServingGroups[0].OwnerReferences[0].UID = "uid-other"
			},
			want:       []found{{SeverityViolation, ChangeGroupWithoutObject, "ServingGroup", "llama-0"}},
			wantResult: ResultFailed,
		},
		{
			name: "the cluster was not upgraded",
			after: func(s *Snapshot) {
				s.ServingGroupCRD, s.ServingGroups = crdNotInstalled, nil
				delete(s.ModelServings[0].Annotations, workloadv1alpha1.ServingGroupAdoptionAnnotationKey)
			},
			want: []found{
				{SeverityViolation, ChangeResourceNotServed, "CustomResourceDefinition", "servinggroups.workload.serving.volcano.sh"},
				{SeverityViolation, ChangeTakeoverNotDone, "ModelServing", "llama"},
			},
			wantResult: ResultFailed,
		},
		{
			name:       "a ModelServing does not exist any more",
			after:      func(s *Snapshot) { s.ModelServings = nil },
			want:       []found{{SeverityViolation, ChangeModelServingMissing, "ModelServing", "llama"}},
			wantResult: ResultFailed,
		},
		{
			name:       "a ModelServing was deleted and created again",
			after:      func(s *Snapshot) { s.ModelServings[0].UID = "uid-new" },
			want:       []found{{SeverityViolation, ChangeModelServingRecreated, "ModelServing", "llama"}},
			wantResult: ResultFailed,
		},
		{
			name: "a group that was regular carries the surge annotation",
			after: func(s *Snapshot) {
				s.ServingGroups[2].Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
			},
			want:       []found{{SeverityViolation, ChangeSurgeMark, "ServingGroup", "llama-2"}},
			wantResult: ResultFailed,
		},

		// Changes that the pre-flight check announced are not judged.
		{
			name: "the pre-flight check announced a rollout: replaced Pods are for a person to judge",
			before: func(s *Snapshot) {
				ms := s.ModelServings[0]
				setImage(ms, "model:v2")
				revision := record(t, ms, s.Revisions...)
				s.Revisions = append(s.Revisions, revision)
				ms.Status.UpdateRevision = identityOf(revision)
			},
			after: func(s *Snapshot) {
				podNamed(s, "llama-2-leader-0-0").UID = "uid-new"
				withoutPod(s, "llama-2-leader-0-1")
				s.ModelServings[0].Status.AvailableReplicas = 2
			},
			want: []found{
				{SeverityExpected, ChangeAvailableDropped, "ModelServing", "llama"},
				{SeverityExpected, ChangePodRecreated, "Pod", "llama-2-leader-0-0"},
				{SeverityExpected, ChangePodMissing, "Pod", "llama-2-leader-0-1"},
			},
			wantResult: ResultReview,
			check: func(t *testing.T, c *Comparison) {
				if detail := c.Changes[1].Detail; !strings.Contains(detail, "the pre-flight check announced changes: "+FindingRolloutInProgress) {
					t.Errorf("detail = %s", detail)
				}
			},
		},
		{
			name: "the temporary group of the baseline carries the surge annotation",
			before: func(s *Snapshot) {
				ms := s.ModelServings[0]
				s.Pods = append(s.Pods, groupPods(ms, 3, ms.Status.UpdateRevision, surgeMarked)...)
			},
			after: func(s *Snapshot) {
				for _, g := range s.ServingGroups {
					if g.Name == "llama-3" {
						g.Annotations = map[string]string{workloadv1alpha1.SurgeAnnotationKey: workloadv1alpha1.SurgeScopeServingGroup}
					}
				}
			},
			wantResult: ResultPassed,
		},
		{
			name: "the temporary group of the baseline lost the surge annotation",
			before: func(s *Snapshot) {
				ms := s.ModelServings[0]
				s.Pods = append(s.Pods, groupPods(ms, 3, ms.Status.UpdateRevision, surgeMarked)...)
			},
			want:       []found{{SeverityExpected, ChangeSurgeMark, "ServingGroup", "llama-3"}},
			wantResult: ResultReview,
		},
		{
			name: "the spec was changed after the baseline: replaced Pods are for a person to judge",
			after: func(s *Snapshot) {
				s.ModelServings[0].Generation = 2
				podNamed(s, "llama-2-leader-0-0").UID = "uid-new"
			},
			want:       []found{{SeverityExpected, ChangePodRecreated, "Pod", "llama-2-leader-0-0"}},
			wantResult: ResultReview,
			check: func(t *testing.T, c *Comparison) {
				if detail := c.Changes[0].Detail; !strings.Contains(detail, "the spec was changed after the baseline: generation 2, was 1") {
					t.Errorf("detail = %s", detail)
				}
			},
		},
		{
			name: "a takeover that has not finished is a violation whatever was announced",
			before: func(s *Snapshot) {
				terminating(s.Pods[5])
			},
			after: func(s *Snapshot) {
				delete(s.ModelServings[0].Annotations, workloadv1alpha1.ServingGroupAdoptionAnnotationKey)
			},
			want:       []found{{SeverityViolation, ChangeTakeoverNotDone, "ModelServing", "llama"}},
			wantResult: ResultFailed,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := legacySnapshot(t)
			if tt.before != nil {
				tt.before(before)
			}
			baseline := NewBaseline(before, Analyze(before))
			after := takenOver(before)
			if tt.after != nil {
				tt.after(after)
			}

			c := Compare(baseline, after)

			var got []found
			for _, change := range c.Changes {
				got = append(got, found{change.Severity, change.Change, change.Kind, change.Name})
				if change.Detail == "" || change.Namespace == "" && change.Kind != "CustomResourceDefinition" {
					t.Errorf("change %+v lacks a detail or a namespace", change)
				}
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("changes = %v, want %v", got, tt.want)
			}
			if c.Result != tt.wantResult {
				t.Errorf("result = %s, want %s", c.Result, tt.wantResult)
			}
			wantCode := map[string]int{ResultPassed: ExitReady, ResultReview: ExitAttention, ResultFailed: ExitBlocked}[tt.wantResult]
			if c.ExitCode() != wantCode {
				t.Errorf("exit code = %d, want %d", c.ExitCode(), wantCode)
			}
			if !c.BaselineCreatedAt.Equal(testTime) || !c.ComparedAt.Equal(testTime.Add(10*time.Minute)) || c.Scope != before.Scope {
				t.Errorf("comparison = %+v", c)
			}
			if tt.check != nil && !t.Failed() {
				tt.check(t, c)
			}
		})
	}
}

// TestCompareKeepsToTheBaseline checks that objects of a ModelServing the
// baseline does not cover are not reported.
func TestCompareKeepsToTheBaseline(t *testing.T) {
	before := legacySnapshot(t)
	baseline := NewBaseline(before, Analyze(before))
	after := takenOver(before)
	qwen := newModelServing("qwen", 1)
	after.ModelServings = append(after.ModelServings, qwen)
	after.Pods = append(after.Pods, groupPods(qwen, 0, "any")...)

	if c := Compare(baseline, after); c.Result != ResultPassed || len(c.Changes) != 0 {
		t.Errorf("comparison = %s %+v, want passed without a change", c.Result, c.Changes)
	}
}

func TestBaselineAndComparisonJSONFieldNames(t *testing.T) {
	before := legacySnapshot(t)
	baseline := NewBaseline(before, Analyze(before))
	after := takenOver(before)
	after.Pods[0].UID = "uid-new"

	check := func(what string, value any, want ...string) {
		t.Helper()
		if got := keysOf(t, value); !slices.Equal(got, want) {
			t.Errorf("fields of %s = %v, want %v", what, got, want)
		}
	}
	encoded := decode(t, baseline)
	check("the baseline", encoded, "createdAt", "formatVersion", "kind", "modelServings", "podGroups", "pods", "scope", "services")
	check("a ModelServing", encoded["modelServings"].([]any)[0], "generation", "name", "namespace", "podChangesExpected",
		"status", "surgeGroups", "targetRevision", "uid", "verdict")
	pod := encoded["pods"].([]any)[0].(map[string]any)
	check("a Pod", pod, "containers", "group", "modelServing", "name", "namespace", "node", "phase", "ready",
		"readyLastTransitionTime", "revision", "terminating", "uid")
	check("a container", pod["containers"].([]any)[0], "name", "restartCount")
	check("a PodGroup", encoded["podGroups"].([]any)[0], "group", "groupHasPods", "modelServing", "name", "namespace", "uid")
	check("a Service", encoded["services"].([]any)[0], "group", "groupHasPods", "modelServing", "name", "namespace", "uid")

	comparison := decode(t, Compare(baseline, after))
	check("the comparison", comparison, "baselineCreatedAt", "changes", "comparedAt", "formatVersion", "result", "scope", "summary")
	check("summary", comparison["summary"], "expected", "info", "modelServings", "podGroups", "pods", "services", "violations")
	check("a change", comparison["changes"].([]any)[0], "change", "detail", "kind", "modelServing", "name", "namespace", "severity")
	if comparison["result"] != "failed" {
		t.Errorf("result = %v, want failed", comparison["result"])
	}
}

func TestComparisonTable(t *testing.T) {
	before := legacySnapshot(t)
	baseline := NewBaseline(before, Analyze(before))

	t.Run("nothing changed", func(t *testing.T) {
		var out bytes.Buffer
		if err := Compare(baseline, takenOver(before)).WriteTable(&out); err != nil {
			t.Fatalf("WriteTable() error = %v", err)
		}
		for _, want := range []string{
			"Scope: all namespaces   Baseline: 2026-11-25T04:00:00Z   Time: 2026-11-25T04:10:00Z",
			"Compared: 1 ModelServing, 6 Pods, 3 PodGroups, 3 Services",
			"Nothing that the baseline records has changed.",
			"Result: passed (exit code 0)",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the table lacks %q:\n%s", want, out.String())
			}
		}
	})
	t.Run("a Pod was replaced", func(t *testing.T) {
		after := takenOver(before)
		after.Pods[0].UID = "uid-new"
		var out bytes.Buffer
		if err := Compare(baseline, after).WriteTable(&out); err != nil {
			t.Fatalf("WriteTable() error = %v", err)
		}
		for _, want := range []string{
			"SEVERITY    CHANGE          KIND   NAMESPACE   NAME                 DETAIL",
			"violation   pod-recreated   Pod    default     llama-0-leader-0-0   the Pod was deleted and created again: UID uid-new, was uid-llama-0-leader-0-0",
			"Summary: 1 change that must not happen, 0 changes that the pre-flight check announced or a change of the spec explains, 0 notes",
			"Result: failed (exit code 3)",
		} {
			if !strings.Contains(out.String(), want) {
				t.Errorf("the table lacks %q:\n%s", want, out.String())
			}
		}
	})
}

// TestCompareWithABaselineReadBack checks that a baseline that went through
// a file is accepted like the one it was written from.
func TestCompareWithABaselineReadBack(t *testing.T) {
	before := legacySnapshot(t)
	restarted(2)(before.Pods[0])
	terminating(before.Pods[5])
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := WriteBaseline(path, NewBaseline(before, Analyze(before))); err != nil {
		t.Fatalf("WriteBaseline() error = %v", err)
	}
	baseline, err := ReadBaseline(path)
	if err != nil {
		t.Fatalf("ReadBaseline() error = %v", err)
	}
	if c := Compare(baseline, takenOver(before)); c.Result != ResultPassed || len(c.Changes) != 0 {
		t.Errorf("comparison = %s %+v, want passed without a change", c.Result, c.Changes)
	}
}
