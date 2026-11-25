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

package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/pflag"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	"github.com/volcano-sh/kthena/cli/kthena/internal/preflight"
	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// preflightCluster is a fake cluster for the preflight command: the
// controller manager of an earlier version in kthena-system and the
// ModelServing default/llama with one group of one Pod, in steady state.
type preflightCluster struct {
	kube    *kubefake.Clientset
	kthena  *kthenafake.Clientset
	volcano *volcanofake.Clientset
	ms      *workloadv1alpha1.ModelServing
	// served reports whether the ServingGroup resource is served.
	served bool
}

const (
	preflightPod     = "llama-0-leader-0-0"
	preflightGroup   = "llama-0"
	preflightManager = "kthena-controller-manager"
)

func newPreflightCluster(t *testing.T) *preflightCluster {
	t.Helper()
	ctx := context.Background()
	replicas := int32(1)
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: "llama", UID: "uid-llama", Generation: 1},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      &replicas,
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroupTemplate{Roles: []workloadv1alpha1.Role{{
				Name: "leader", Replicas: &replicas,
				EntryTemplate: workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "model", Image: "model:v1"}}}},
			}}},
		},
		Status: workloadv1alpha1.ModelServingStatus{ObservedGeneration: 1, Replicas: 1, CurrentReplicas: 1, UpdatedReplicas: 1, AvailableReplicas: 1},
	}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)}

	c := &preflightCluster{kube: kubefake.NewSimpleClientset(), volcano: volcanofake.NewSimpleClientset(), ms: ms}
	// The revision of the template, recorded the way the controller does.
	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		t.Fatalf("BuildRevisionData() error = %v", err)
	}
	revision, _, err := utils.RecordModelServingRevision(ctx, c.kube, ms, data)
	if err != nil {
		t.Fatalf("RecordModelServingRevision() error = %v", err)
	}
	identity := revision.Labels[workloadv1alpha1.RevisionLabelKey]
	ms.Status.CurrentRevision, ms.Status.UpdateRevision = identity, identity
	c.kube.ClearActions()

	groupLabels := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        preflightGroup,
	}
	podLabels := map[string]string{
		workloadv1alpha1.ModelServingNameLabelKey: ms.Name,
		workloadv1alpha1.GroupNameLabelKey:        preflightGroup,
		workloadv1alpha1.RoleLabelKey:             "leader",
		workloadv1alpha1.RoleIDKey:                "leader-0",
		workloadv1alpha1.EntryLabelKey:            utils.Entry,
		workloadv1alpha1.RevisionLabelKey:         identity,
	}
	objects := []runtime.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: ms.Namespace, Name: preflightPod, UID: "uid-pod", Labels: podLabels, OwnerReferences: owner},
			Spec:       corev1.PodSpec{NodeName: "node-1", Containers: []corev1.Container{{Name: "model", Image: "model:v1"}}},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type: corev1.PodReady, Status: corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Date(2026, 11, 25, 3, 0, 0, 0, time.UTC)),
				}},
				ContainerStatuses: []corev1.ContainerStatus{{Name: "model", Image: "model:v1", ContainerID: "containerd://1"}},
			},
		},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Namespace: ms.Namespace, Name: preflightPod, UID: "uid-service", Labels: groupLabels, OwnerReferences: owner},
			Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		},
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kthena-system", Name: preflightManager},
			Spec: appsv1.DeploymentSpec{Replicas: &replicas, Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: preflightManager, Image: "kthena-controller-manager:v1.1.0"}}},
			}},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Namespace: "kthena-system", Name: preflightManager + "-abcde",
				Labels: map[string]string{"app.kubernetes.io/component": preflightManager}},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		},
	}
	for _, object := range objects {
		if err := c.kube.Tracker().Add(object); err != nil {
			t.Fatalf("failed to add %T: %v", object, err)
		}
	}
	podGroup := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ms.Namespace, Name: preflightGroup, UID: "uid-podgroup", Labels: groupLabels, OwnerReferences: owner},
	}
	if err := c.volcano.Tracker().Add(podGroup); err != nil {
		t.Fatalf("failed to add the PodGroup: %v", err)
	}

	c.kthena = kthenafake.NewSimpleClientset(ms)
	// Before the upgrade the API server does not know the resource.
	c.kthena.PrependReactor("list", "servinggroups", func(clienttesting.Action) (bool, runtime.Object, error) {
		if c.served {
			return false, nil, nil
		}
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "servinggroups"}, "")
	})
	return c
}

// upgrade puts the cluster into the state an upgrade leaves it in: the
// resource is served, the ModelServing is annotated Done, and a ServingGroup
// object controls the Pod, the PodGroup and the Service of the group.
func (c *preflightCluster) upgrade(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	c.served = true

	ms, err := c.kthena.WorkloadV1alpha1().ModelServings("default").Get(ctx, "llama", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get the ModelServing: %v", err)
	}
	ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionDone}
	if _, err := c.kthena.WorkloadV1alpha1().ModelServings("default").Update(ctx, ms, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to annotate the ModelServing: %v", err)
	}
	group := &workloadv1alpha1.ServingGroup{ObjectMeta: metav1.ObjectMeta{
		Namespace: "default", Name: preflightGroup, UID: "uid-servinggroup",
		OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(c.ms, workloadv1alpha1.ModelServingKind)},
	}}
	if _, err := c.kthena.WorkloadV1alpha1().ServingGroups("default").Create(ctx, group, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create the ServingGroup: %v", err)
	}
	owner := []metav1.OwnerReference{*metav1.NewControllerRef(group, workloadv1alpha1.ServingGroupKind)}

	pod, err := c.kube.CoreV1().Pods("default").Get(ctx, preflightPod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get the Pod: %v", err)
	}
	pod.OwnerReferences = owner
	if _, err := c.kube.CoreV1().Pods("default").Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update the Pod: %v", err)
	}
	service, err := c.kube.CoreV1().Services("default").Get(ctx, preflightPod, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get the Service: %v", err)
	}
	service.OwnerReferences = owner
	if _, err := c.kube.CoreV1().Services("default").Update(ctx, service, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update the Service: %v", err)
	}
	podGroup, err := c.volcano.SchedulingV1beta1().PodGroups("default").Get(ctx, preflightGroup, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("failed to get the PodGroup: %v", err)
	}
	podGroup.OwnerReferences = owner
	if _, err := c.volcano.SchedulingV1beta1().PodGroups("default").Update(ctx, podGroup, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("failed to update the PodGroup: %v", err)
	}
	c.clearActions()
}

func (c *preflightCluster) clearActions() {
	c.kube.ClearActions()
	c.kthena.ClearActions()
	c.volcano.ClearActions()
}

// writes returns the requests that are not reads, as "verb resource".
func (c *preflightCluster) writes() []string {
	var writes []string
	for _, actions := range [][]clienttesting.Action{c.kube.Actions(), c.kthena.Actions(), c.volcano.Actions()} {
		for _, action := range actions {
			if verb := action.GetVerb(); verb != "get" && verb != "list" {
				writes = append(writes, verb+" "+action.GetResource().Resource)
			}
		}
	}
	return writes
}

type preflightRun struct {
	stdout, stderr string
	// code is the exit code the command ended the process with; 0 when it
	// returned normally.
	code int
	err  error
}

// runPreflight runs "kthena preflight model-servings" with args against the
// fake cluster.
func runPreflight(t *testing.T, c *preflightCluster, args ...string) preflightRun {
	t.Helper()
	oldClients, oldExit := preflightClients, preflightExit
	t.Cleanup(func() {
		preflightClients, preflightExit = oldClients, oldExit
		rootCmd.SetOut(nil)
		rootCmd.SetErr(nil)
		rootCmd.SetArgs(nil)
	})
	// The flags keep their values from one execution to the next.
	for _, flags := range []*pflag.FlagSet{preflightCmd.PersistentFlags(), preflightModelServingsCmd.Flags()} {
		flags.VisitAll(func(flag *pflag.Flag) {
			if err := flag.Value.Set(flag.DefValue); err != nil {
				t.Fatalf("failed to reset --%s: %v", flag.Name, err)
			}
			flag.Changed = false
		})
	}

	run := preflightRun{}
	preflightClients = func() (preflight.Clients, string, error) {
		return preflight.Clients{Kube: c.kube, Kthena: c.kthena, Volcano: c.volcano}, "default", nil
	}
	preflightExit = func(code int) { run.code = code }
	var out, errOut bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errOut)
	rootCmd.SetArgs(append([]string{"preflight", "model-servings"}, args...))
	run.err = rootCmd.Execute()
	run.stdout, run.stderr = out.String(), errOut.String()
	return run
}

func TestPreflightModelServings(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(t *testing.T, c *preflightCluster)
		args   []string
		// wantCode is the exit code; wantErr a part of the error of a command
		// that could not run.
		wantCode   int
		wantErr    string
		wantStdout []string
		wantWrites []string
	}{
		{
			name:     "every ModelServing is ready: exit code 0",
			args:     []string{"--all-namespaces"},
			wantCode: 0,
			wantStdout: []string{
				"Scope: all namespaces   Dry-run: performed",
				"default     llama   ready     ServingGroupRollingUpdate   1          1 (0)    1/1         0           none                    passed",
				"Summary: 1 ModelServing: 1 ready, 0 attention, 0 wait, 0 blocked; cluster: ready",
				"Result: ready (exit code 0)",
			},
			wantWrites: []string{"patch modelservings"},
		},
		{
			name:     "the namespace of the kubeconfig context by default",
			wantCode: 0,
			wantStdout: []string{
				"Scope: namespace default",
				"[info] scope-limited",
				"llama   ready",
			},
			wantWrites: []string{"patch modelservings"},
		},
		{
			name:       "one ModelServing by name",
			args:       []string{"llama", "-n", "default"},
			wantCode:   0,
			wantStdout: []string{"Scope: ModelServing default/llama", "Result: ready (exit code 0)"},
			wantWrites: []string{"patch modelservings"},
		},
		{
			name:       "without the dry-run nothing is written",
			args:       []string{"-A", "--skip-dry-run"},
			wantCode:   0,
			wantStdout: []string{"Dry-run: skipped", "skipped\n", "Result: ready (exit code 0)"},
		},
		{
			name: "something is in progress: exit code 2",
			mutate: func(t *testing.T, c *preflightCluster) {
				pod, err := c.kube.CoreV1().Pods("default").Get(context.Background(), preflightPod, metav1.GetOptions{})
				if err != nil {
					t.Fatalf("failed to get the Pod: %v", err)
				}
				now := metav1.Now()
				pod.DeletionTimestamp = &now
				if _, err := c.kube.CoreV1().Pods("default").Update(context.Background(), pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("failed to update the Pod: %v", err)
				}
			},
			args:       []string{"-A"},
			wantCode:   2,
			wantStdout: []string{"default/llama: wait", "[wait] terminating-pods", "Result: wait (exit code 2)"},
			wantWrites: []string{"patch modelservings"},
		},
		{
			name: "the takeover would not happen: exit code 3",
			mutate: func(t *testing.T, c *preflightCluster) {
				configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "kthena-system", Name: "kthena-servinggroup-rollback"}}
				if _, err := c.kube.CoreV1().ConfigMaps("kthena-system").Create(context.Background(), configMap, metav1.CreateOptions{}); err != nil {
					t.Fatalf("failed to create the ConfigMap: %v", err)
				}
			},
			args:       []string{"-A"},
			wantCode:   3,
			wantStdout: []string{"CLUSTER: blocked", "[blocked] rollback-switch-present", "Result: blocked (exit code 3)"},
			wantWrites: []string{"patch modelservings"},
		},
		{
			name:    "an output format that is not supported",
			args:    []string{"-o", "yaml"},
			wantErr: "unsupported output format 'yaml'",
		},
		{
			name:    "a baseline cannot be written and compared in one run",
			args:    []string{"--baseline-file", "a.json", "--compare-baseline", "b.json"},
			wantErr: "--baseline-file and --compare-baseline cannot be used together",
		},
		{
			name:    "a name with all namespaces",
			args:    []string{"llama", "-A"},
			wantErr: "a ModelServing cannot be named together with --all-namespaces",
		},
		{
			name:    "a ModelServing that does not exist",
			args:    []string{"missing"},
			wantErr: "failed to get ModelServing 'missing'",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newPreflightCluster(t)
			if tt.mutate != nil {
				tt.mutate(t, c)
				c.clearActions()
			}
			run := runPreflight(t, c, tt.args...)

			if tt.wantErr != "" {
				if run.err == nil || !strings.Contains(run.err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", run.err, tt.wantErr)
				}
				if run.code != 0 {
					t.Errorf("the command ended the process with exit code %d instead of returning the error", run.code)
				}
				return
			}
			if run.err != nil {
				t.Fatalf("error = %v", run.err)
			}
			if run.code != tt.wantCode {
				t.Errorf("exit code = %d, want %d", run.code, tt.wantCode)
			}
			for _, want := range tt.wantStdout {
				if !strings.Contains(run.stdout, want) {
					t.Errorf("the output lacks %q:\n%s", want, run.stdout)
				}
			}
			if got := c.writes(); strings.Join(got, ",") != strings.Join(tt.wantWrites, ",") {
				t.Errorf("requests that are not reads = %v, want %v", got, tt.wantWrites)
			}
		})
	}
}

func TestPreflightModelServingsJSON(t *testing.T) {
	c := newPreflightCluster(t)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	run := runPreflight(t, c, "-A", "-o", "json", "--baseline-file", baseline)
	if run.err != nil || run.code != 0 {
		t.Fatalf("error = %v, exit code = %d", run.err, run.code)
	}
	// What is not part of the report goes to standard error.
	var report preflight.Report
	if err := json.Unmarshal([]byte(run.stdout), &report); err != nil {
		t.Fatalf("standard output is not JSON: %v\n%s", err, run.stdout)
	}
	if report.Verdict != preflight.VerdictReady || len(report.ModelServings) != 1 || report.ModelServings[0].Name != "llama" ||
		report.ModelServings[0].DryRun.Outcome != preflight.DryRunPassed {
		t.Errorf("report = %+v", report)
	}
	if !strings.Contains(run.stderr, "Baseline written to "+baseline) {
		t.Errorf("standard error = %q, want the baseline announced", run.stderr)
	}
	if _, err := preflight.ReadBaseline(baseline); err != nil {
		t.Errorf("the baseline cannot be read back: %v", err)
	}
}

// TestPreflightCompareBaseline goes through the upgrade: a baseline before
// it, and comparisons before the takeover, after it, and after a Pod was lost.
func TestPreflightCompareBaseline(t *testing.T) {
	c := newPreflightCluster(t)
	baseline := filepath.Join(t.TempDir(), "baseline.json")
	if run := runPreflight(t, c, "-A", "--baseline-file", baseline); run.err != nil || run.code != 0 {
		t.Fatalf("writing the baseline: error = %v, exit code = %d\n%s", run.err, run.code, run.stdout)
	}
	if _, err := os.Stat(baseline); err != nil {
		t.Fatalf("the baseline was not written: %v", err)
	}
	c.clearActions()

	steps := []struct {
		name       string
		prepare    func(t *testing.T)
		args       []string
		wantCode   int
		wantErr    string
		wantStdout []string
	}{
		{
			name:     "before the upgrade: nothing is taken over",
			args:     []string{"--compare-baseline", baseline},
			wantCode: 3,
			wantStdout: []string{
				"Scope: all namespaces",
				"servinggroup-resource-not-served",
				"takeover-not-done",
				"Result: failed (exit code 3)",
			},
		},
		{
			name:       "after the upgrade: nothing changed but the owners",
			prepare:    func(t *testing.T) { c.upgrade(t) },
			args:       []string{"--compare-baseline", baseline},
			wantCode:   0,
			wantStdout: []string{"Compared: 1 ModelServing, 1 Pod, 1 PodGroup, 1 Service", "Nothing that the baseline records has changed.", "Result: passed (exit code 0)"},
		},
		{
			name:       "the scope of the baseline, given again",
			args:       []string{"--compare-baseline", baseline, "-A", "-o", "json"},
			wantCode:   0,
			wantStdout: []string{`"result": "passed"`, `"changes": []`},
		},
		{
			name:    "another scope than the one of the baseline",
			args:    []string{"--compare-baseline", baseline, "-n", "default"},
			wantErr: "the baseline was taken for all namespaces",
		},
		{
			name: "a Pod was lost",
			prepare: func(t *testing.T) {
				if err := c.kube.CoreV1().Pods("default").Delete(context.Background(), preflightPod, metav1.DeleteOptions{}); err != nil {
					t.Fatalf("failed to delete the Pod: %v", err)
				}
				c.clearActions()
			},
			args:       []string{"--compare-baseline", baseline},
			wantCode:   3,
			wantStdout: []string{"violation   pod-missing", "Result: failed (exit code 3)"},
		},
		{
			name:    "a file that is not a baseline",
			args:    []string{"--compare-baseline", filepath.Join(t.TempDir(), "missing.json")},
			wantErr: "failed to read the baseline",
		},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) {
			if step.prepare != nil {
				step.prepare(t)
			}
			run := runPreflight(t, c, step.args...)
			if step.wantErr != "" {
				if run.err == nil || !strings.Contains(run.err.Error(), step.wantErr) {
					t.Fatalf("error = %v, want %q", run.err, step.wantErr)
				}
				return
			}
			if run.err != nil {
				t.Fatalf("error = %v", run.err)
			}
			if run.code != step.wantCode {
				t.Errorf("exit code = %d, want %d\n%s", run.code, step.wantCode, run.stdout)
			}
			for _, want := range step.wantStdout {
				if !strings.Contains(run.stdout, want) {
					t.Errorf("the output lacks %q:\n%s", want, run.stdout)
				}
			}
			// A comparison only reads.
			if writes := c.writes(); len(writes) != 0 {
				t.Errorf("the comparison sent %v", writes)
			}
		})
	}
}

func TestPreflightCommandTree(t *testing.T) {
	for _, path := range [][]string{{"preflight", "model-servings"}, {"preflight", "ms"}, {"preflight", "model-serving"}} {
		found, _, err := rootCmd.Find(path)
		if err != nil || found != preflightModelServingsCmd {
			t.Errorf("kthena %s resolves to %v (%v), want the model-servings check", strings.Join(path, " "), found, err)
		}
	}
	for name, shorthand := range map[string]string{"namespace": "n", "all-namespaces": "A", "output": "o"} {
		flag := preflightCmd.PersistentFlags().Lookup(name)
		if flag == nil || flag.Shorthand != shorthand {
			t.Errorf("flag --%s/-%s is not defined like on kthena get: %+v", name, shorthand, flag)
		}
	}
	for _, name := range []string{"baseline-file", "compare-baseline", "skip-dry-run"} {
		if preflightModelServingsCmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s is not defined", name)
		}
	}
}
