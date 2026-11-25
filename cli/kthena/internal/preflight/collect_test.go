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
	"slices"
	"sort"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	kubefake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// fakes are the fake clientsets of a test cluster.
type fakes struct {
	kube    *kubefake.Clientset
	kthena  *kthenafake.Clientset
	volcano *volcanofake.Clientset
}

func (f fakes) clients() Clients {
	return Clients{Kube: f.kube, Kthena: f.kthena, Volcano: f.volcano}
}

// actions returns what was sent to the API server, as "verb resource".
func (f fakes) actions() []clienttesting.Action {
	return slices.Concat(f.kube.Actions(), f.kthena.Actions(), f.volcano.Actions())
}

// cluster holds the objects of a test cluster.
type cluster struct {
	kube, kthena, volcano []runtime.Object
}

func (c cluster) fakes() fakes {
	return fakes{
		kube:    kubefake.NewSimpleClientset(c.kube...),
		kthena:  kthenafake.NewSimpleClientset(c.kthena...),
		volcano: volcanofake.NewSimpleClientset(c.volcano...),
	}
}

// add puts the workloads of a ModelServing in steady state into the cluster:
// the ModelServing, the Pods of its groups, their PodGroups and Services, and
// the ControllerRevision of its template.
func (c *cluster) add(t *testing.T, ms *workloadv1alpha1.ModelServing, ordinals ...int) {
	t.Helper()
	revision := steady(t, ms)
	c.kthena = append(c.kthena, ms)
	c.kube = append(c.kube, revision)
	for _, ordinal := range ordinals {
		for _, pod := range groupPods(ms, ordinal, identityOf(revision)) {
			c.kube = append(c.kube, pod)
		}
		c.kube = append(c.kube, serviceOf(ms, ordinal))
		c.volcano = append(c.volcano, podGroupOf(ms, ordinal))
	}
}

// legacyCluster returns a cluster that runs an earlier version: a controller
// manager in kthena-system, the ModelServing default/llama with three groups
// and team-b/qwen with one, and objects that have nothing to do with them.
func legacyCluster(t *testing.T) *cluster {
	t.Helper()
	c := &cluster{}
	c.add(t, newModelServing("llama", 3), 0, 1, 2)
	qwen := newModelServing("qwen", 1)
	qwen.Namespace = "team-b"
	c.add(t, qwen, 0)

	unrelated := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "web", Labels: map[string]string{"app": "web"}}}
	otherDeployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: "kthena-router"}}
	otherConfigMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: rollbackSwitchName}}
	c.kube = append(c.kube,
		controllerDeployment(controllerNamespace),
		controllerPod(controllerNamespace, "kthena-controller-manager-5d8f7-abcde", ""),
		unrelated, otherDeployment, otherConfigMap,
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-2"}, Spec: corev1.NodeSpec{Unschedulable: true}},
		&policyv1.PodDisruptionBudget{
			ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: "llama"},
			Spec:       policyv1.PodDisruptionBudgetSpec{Selector: selecting("llama"), MinAvailable: ptr(intstr.FromInt32(4))},
		},
		&admissionregistrationv1.ValidatingWebhookConfiguration{
			ObjectMeta: metav1.ObjectMeta{Name: webhookConfigurationName},
			Webhooks:   []admissionregistrationv1.ValidatingWebhook{{Name: modelServingWebhookName, FailurePolicy: ptr(admissionregistrationv1.Fail)}},
		},
	)
	return c
}

// notServed makes a list of the resource fail the way the API server does
// when the CustomResourceDefinition is not installed.
func notServed(fake *clienttesting.Fake, resource string) {
	fake.PrependReactor("list", resource, func(clienttesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: resource}, "")
	})
}

func names[T metav1.Object](objects []T) []string {
	out := make([]string, 0, len(objects))
	for _, object := range objects {
		out = append(out, object.GetNamespace()+"/"+object.GetName())
	}
	sort.Strings(out)
	return out
}

func TestCollectScope(t *testing.T) {
	tests := []struct {
		name              string
		scope             Scope
		wantModelServings []string
		wantPods          int
		wantPodGroups     []string
		wantServices      int
		wantRevisions     int
		wantBudgets       int
	}{
		{
			name:              "all namespaces",
			scope:             Scope{AllNamespaces: true},
			wantModelServings: []string{"default/llama", "team-b/qwen"},
			wantPods:          8, wantServices: 4, wantRevisions: 2, wantBudgets: 1,
			wantPodGroups: []string{"default/llama-0", "default/llama-1", "default/llama-2", "team-b/qwen-0"},
		},
		{
			name:              "one namespace",
			scope:             Scope{Namespace: "team-b"},
			wantModelServings: []string{"team-b/qwen"},
			wantPods:          2, wantServices: 1, wantRevisions: 1, wantBudgets: 0,
			wantPodGroups: []string{"team-b/qwen-0"},
		},
		{
			name:              "one ModelServing",
			scope:             Scope{Namespace: testNamespace, ModelServing: "llama"},
			wantModelServings: []string{"default/llama"},
			wantPods:          6, wantServices: 3, wantRevisions: 1, wantBudgets: 1,
			wantPodGroups: []string{"default/llama-0", "default/llama-1", "default/llama-2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := legacyCluster(t).fakes()
			s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: tt.scope})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if got := names(s.ModelServings); !slices.Equal(got, tt.wantModelServings) {
				t.Errorf("ModelServings = %v, want %v", got, tt.wantModelServings)
			}
			if got := names(s.PodGroups); !slices.Equal(got, tt.wantPodGroups) {
				t.Errorf("PodGroups = %v, want %v", got, tt.wantPodGroups)
			}
			if len(s.Pods) != tt.wantPods || len(s.Services) != tt.wantServices || len(s.Revisions) != tt.wantRevisions || len(s.Budgets) != tt.wantBudgets {
				t.Errorf("Pods, Services, Revisions, Budgets = %d, %d, %d, %d, want %d, %d, %d, %d",
					len(s.Pods), len(s.Services), len(s.Revisions), len(s.Budgets), tt.wantPods, tt.wantServices, tt.wantRevisions, tt.wantBudgets)
			}
			if s.Scope != tt.scope || s.DryRunSkipped || len(s.ReadErrors) != 0 {
				t.Errorf("snapshot = scope %+v, dry-run skipped %t, read errors %v", s.Scope, s.DryRunSkipped, s.ReadErrors)
			}
			if len(s.DryRuns) != len(tt.wantModelServings) {
				t.Errorf("DryRuns = %v, want one for each ModelServing", s.DryRuns)
			}
		})
	}
}

func TestCollectOneModelServingThatDoesNotExist(t *testing.T) {
	f := legacyCluster(t).fakes()
	scope := Scope{Namespace: testNamespace, ModelServing: "missing"}
	_, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: scope})
	if err == nil || !strings.Contains(err.Error(), "failed to get ModelServing 'missing'") {
		t.Fatalf("Collect() error = %v, want the ModelServing reported as missing", err)
	}
	// A comparison has to be able to say that the ModelServing of its
	// baseline is gone.
	s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: scope, WorkloadsOnly: true})
	if err != nil || len(s.ModelServings) != 0 {
		t.Fatalf("Collect() for a comparison = %v, error = %v, want no ModelServing and no error", names(s.ModelServings), err)
	}
}

// TestCollectCluster checks the cluster-level reads: the controller manager
// is looked for in every namespace whatever the scope is, and the rollback
// switch only where a controller manager is.
func TestCollectCluster(t *testing.T) {
	c := legacyCluster(t)
	c.kube = append(c.kube,
		controllerPod("staging", "kthena-controller-manager-old", ""),
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: "staging", Name: rollbackSwitchName}},
	)
	f := c.fakes()
	s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{Namespace: "team-b"}})
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	if got := names(s.Deployments); !slices.Equal(got, []string{"kthena-system/kthena-controller-manager"}) {
		t.Errorf("Deployments = %v, want only the controller manager", got)
	}
	if got := names(s.ControllerPods); !slices.Equal(got, []string{"kthena-system/kthena-controller-manager-5d8f7-abcde", "staging/kthena-controller-manager-old"}) {
		t.Errorf("ControllerPods = %v", got)
	}
	if got := names(s.CordonedNodes); !slices.Equal(got, []string{"/node-2"}) {
		t.Errorf("CordonedNodes = %v, want only the cordoned node", got)
	}
	// default has a ConfigMap of that name and no controller manager.
	if got := names(s.RollbackSwitches); !slices.Equal(got, []string{"staging/kthena-servinggroup-rollback"}) {
		t.Errorf("RollbackSwitches = %v", got)
	}
	if s.Webhooks == nil || len(s.Webhooks.Webhooks) != 1 {
		t.Errorf("Webhooks = %+v", s.Webhooks)
	}
	if s.ServingGroupCRD != crdInstalled {
		t.Errorf("ServingGroupCRD = %q, want installed: the fake serves the resource", s.ServingGroupCRD)
	}
}

// TestCollectWritesNothing checks that the only request that is not a read
// is the patch of every ModelServing, and that it carries the dry-run option.
func TestCollectWritesNothing(t *testing.T) {
	tests := []struct {
		name        string
		opts        CollectOptions
		wantPatches int
		// wantNot lists resources that must not be read.
		wantNot []string
	}{
		{name: "the pre-flight check", opts: CollectOptions{Scope: Scope{AllNamespaces: true}}, wantPatches: 2},
		{name: "without the dry-run", opts: CollectOptions{Scope: Scope{AllNamespaces: true}, SkipDryRun: true}},
		{
			name:    "for a comparison",
			opts:    CollectOptions{Scope: Scope{AllNamespaces: true}, WorkloadsOnly: true},
			wantNot: []string{"deployments", "nodes", "configmaps", "validatingwebhookconfigurations", "controllerrevisions", "poddisruptionbudgets"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := legacyCluster(t).fakes()
			s, err := Collect(context.Background(), f.clients(), tt.opts)
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			patches := 0
			for _, action := range f.actions() {
				resource := action.GetResource().Resource
				if slices.Contains(tt.wantNot, resource) {
					t.Errorf("%s %s was sent, want it left out", action.GetVerb(), resource)
				}
				switch action.GetVerb() {
				case "get", "list":
				case "patch":
					patch := action.(clienttesting.PatchAction)
					options := action.(clienttesting.PatchActionImpl).PatchOptions
					if resource != "modelservings" || action.GetSubresource() != "" {
						t.Errorf("patch of %s/%s was sent, want only ModelServings patched", resource, action.GetSubresource())
					}
					if !slices.Equal(options.DryRun, []string{metav1.DryRunAll}) {
						t.Errorf("patch of %s was sent without the dry-run option: %+v", patch.GetName(), options)
					}
					if want := `{"metadata":{"annotations":{"modelserving.volcano.sh/servinggroup-adoption":"InProgress"}}}`; string(patch.GetPatch()) != want {
						t.Errorf("patch = %s, want %s", patch.GetPatch(), want)
					}
					patches++
				default:
					t.Errorf("%s %s was sent, want only reads", action.GetVerb(), resource)
				}
			}
			if patches != tt.wantPatches {
				t.Errorf("%d patches were sent, want %d", patches, tt.wantPatches)
			}
			if wantSkipped := tt.wantPatches == 0; s.DryRunSkipped != wantSkipped || len(s.DryRuns) != tt.wantPatches {
				t.Errorf("DryRunSkipped = %t, DryRuns = %v", s.DryRunSkipped, s.DryRuns)
			}
		})
	}
}

func TestCollectDryRun(t *testing.T) {
	denied := func(webhook, message string) error {
		return apierrors.NewBadRequest(`admission webhook "` + webhook + `" denied the request: ` + message)
	}
	tests := []struct {
		name string
		// reply is what the API server answers the patch with.
		reply error
		// adoption is the adoption annotation the ModelServing carries.
		adoption    string
		want        DryRunResult
		wantPatches int
	}{
		{name: "admitted", want: DryRunResult{Outcome: DryRunPassed}, wantPatches: 1},
		{
			name:  "denied by an admission webhook",
			reply: denied("policy.example.com", "annotations are locked"),
			want:  DryRunResult{Outcome: DryRunRejected, Webhook: "policy.example.com", Message: "annotations are locked"}, wantPatches: 1,
		},
		{
			name:  "denied by the ModelServing webhook of the running version",
			reply: denied(modelServingWebhookName, "spec.replicas: Invalid value"),
			want:  DryRunResult{Outcome: DryRunRejected, Webhook: modelServingWebhookName, Message: "spec.replicas: Invalid value"}, wantPatches: 1,
		},
		{
			name: "rejected by the validation of the API server",
			reply: apierrors.NewInvalid(workloadv1alpha1.ModelServingKind.GroupKind(), "llama",
				field.ErrorList{field.Invalid(field.NewPath("spec", "replicas"), -1, "must be positive")}),
			want: DryRunResult{Outcome: DryRunRejected, Message: `ModelServing.workload.serving.volcano.sh "llama" is invalid: spec.replicas: Invalid value: -1: must be positive`}, wantPatches: 1,
		},
		{
			name:  "no permission to patch says nothing about the object",
			reply: apierrors.NewForbidden(schema.GroupResource{Resource: "modelservings"}, "llama", errors.New("User cannot patch")),
			want:  DryRunResult{Outcome: DryRunUnknown, Message: `modelservings "llama" is forbidden: User cannot patch`}, wantPatches: 1,
		},
		{
			name:  "a webhook that cannot be reached says nothing about the object",
			reply: apierrors.NewInternalError(errors.New(`failed calling webhook "` + modelServingWebhookName + `": connection refused`)),
			want: DryRunResult{Outcome: DryRunUnknown,
				Message: `Internal error occurred: failed calling webhook "` + modelServingWebhookName + `": connection refused`}, wantPatches: 1,
		},
		{
			name:     "a ModelServing that has the annotation is not patched",
			adoption: workloadv1alpha1.ServingGroupAdoptionDone,
			want:     DryRunResult{Outcome: DryRunSkipped, Message: `the adoption annotation is already "Done"`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ms := newModelServing("llama", 1)
			if tt.adoption != "" {
				ms.Annotations = map[string]string{workloadv1alpha1.ServingGroupAdoptionAnnotationKey: tt.adoption}
			}
			c := &cluster{}
			c.add(t, ms, 0)
			f := c.fakes()
			patches := 0
			f.kthena.PrependReactor("patch", "modelservings", func(clienttesting.Action) (bool, runtime.Object, error) {
				patches++
				return true, ms, tt.reply
			})

			s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{Namespace: testNamespace}})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if got := s.DryRuns["default/llama"]; got != tt.want {
				t.Errorf("dry-run = %+v, want %+v", got, tt.want)
			}
			if patches != tt.wantPatches {
				t.Errorf("%d patches were sent, want %d", patches, tt.wantPatches)
			}
		})
	}
}

func TestCollectResourcesThatAreNotServed(t *testing.T) {
	t.Run("without the ServingGroup CustomResourceDefinition", func(t *testing.T) {
		f := legacyCluster(t).fakes()
		notServed(&f.kthena.Fake, "servinggroups")
		s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if s.ServingGroupCRD != crdNotInstalled || len(s.ReadErrors) != 0 {
			t.Errorf("ServingGroupCRD = %q, read errors = %v, want not-installed", s.ServingGroupCRD, s.ReadErrors)
		}
	})
	t.Run("with the ServingGroup CustomResourceDefinition", func(t *testing.T) {
		c := legacyCluster(t)
		c.kthena = append(c.kthena, servingGroupOf(newModelServing("llama", 3), 0, nil))
		f := c.fakes()
		s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if s.ServingGroupCRD != crdInstalled || len(s.ServingGroups) != 1 {
			t.Errorf("ServingGroupCRD = %q, ServingGroups = %v, want installed with one object", s.ServingGroupCRD, names(s.ServingGroups))
		}
	})
	t.Run("without Volcano there are no PodGroups", func(t *testing.T) {
		f := legacyCluster(t).fakes()
		notServed(&f.volcano.Fake, "podgroups")
		s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
		if err != nil {
			t.Fatalf("Collect() error = %v", err)
		}
		if len(s.PodGroups) != 0 || len(s.Pods) != 8 {
			t.Errorf("PodGroups = %v, Pods = %d, want no PodGroups and 8 Pods", names(s.PodGroups), len(s.Pods))
		}
	})
}

func TestCollectReadFailures(t *testing.T) {
	forbidden := func(fake *clienttesting.Fake, verb, resource string) {
		fake.PrependReactor(verb, resource, func(clienttesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: resource}, "", errors.New("not allowed"))
		})
	}
	// A read the verdicts depend on fails the check.
	fatal := []struct {
		name     string
		forbid   func(f fakes)
		wantText string
	}{
		{"ModelServings", func(f fakes) { forbidden(&f.kthena.Fake, "list", "modelservings") }, "failed to list ModelServings"},
		{"Pods", func(f fakes) { forbidden(&f.kube.Fake, "list", "pods") }, "failed to list Pods"},
		{"Services", func(f fakes) { forbidden(&f.kube.Fake, "list", "services") }, "failed to list Services"},
		{"PodGroups", func(f fakes) { forbidden(&f.volcano.Fake, "list", "podgroups") }, "failed to list PodGroups"},
		{"ControllerRevisions", func(f fakes) { forbidden(&f.kube.Fake, "list", "controllerrevisions") }, "failed to list ControllerRevisions"},
	}
	for _, tt := range fatal {
		t.Run(tt.name+" cannot be read: the check fails", func(t *testing.T) {
			f := legacyCluster(t).fakes()
			tt.forbid(f)
			_, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Fatalf("Collect() error = %v, want %q", err, tt.wantText)
			}
		})
	}

	// A read only some findings depend on is recorded.
	recorded := []struct {
		name   string
		forbid func(f fakes)
		topic  string
	}{
		{"ServingGroups", func(f fakes) { forbidden(&f.kthena.Fake, "list", "servinggroups") }, topicServingGroups},
		{"PodDisruptionBudgets", func(f fakes) { forbidden(&f.kube.Fake, "list", "poddisruptionbudgets") }, topicBudgets},
		{"Deployments", func(f fakes) { forbidden(&f.kube.Fake, "list", "deployments") }, topicController},
		{"nodes", func(f fakes) { forbidden(&f.kube.Fake, "list", "nodes") }, topicNodes},
		{"the rollback switch", func(f fakes) { forbidden(&f.kube.Fake, "get", "configmaps") }, topicRollback},
		{"the webhook configuration", func(f fakes) { forbidden(&f.kube.Fake, "get", "validatingwebhookconfigurations") }, topicWebhooks},
	}
	for _, tt := range recorded {
		t.Run(tt.name+" cannot be read: recorded", func(t *testing.T) {
			f := legacyCluster(t).fakes()
			tt.forbid(f)
			s, err := Collect(context.Background(), f.clients(), CollectOptions{Scope: Scope{AllNamespaces: true}})
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if len(s.ReadErrors) != 1 || s.ReadErrors[0].Topic != tt.topic || !s.unread(tt.topic) {
				t.Fatalf("read errors = %+v, want one about %s", s.ReadErrors, tt.topic)
			}
			if failure := s.ReadErrors[0]; failure.What == "" || !strings.Contains(failure.Error, "not allowed") {
				t.Errorf("read error = %+v, want what was read and why it failed", failure)
			}
			if tt.topic == topicServingGroups && s.ServingGroupCRD != crdUnknown {
				t.Errorf("ServingGroupCRD = %q, want unknown", s.ServingGroupCRD)
			}
		})
	}
}

func TestClassifyDryRun(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want DryRunResult
	}{
		{name: "no error", want: DryRunResult{Outcome: DryRunPassed}},
		{
			name: "a denial with a status the webhook chose",
			err:  apierrors.NewForbidden(schema.GroupResource{Resource: "modelservings"}, "llama", errors.New(`admission webhook "gatekeeper" denied the request: no`)),
			want: DryRunResult{Outcome: DryRunRejected, Webhook: "gatekeeper", Message: "no"},
		},
		{
			name: "a webhook that does not support dry-run is not a denial",
			err:  apierrors.NewBadRequest(`admission webhook "legacy.example.com" does not support dry run`),
			want: DryRunResult{Outcome: DryRunUnknown, Message: `admission webhook "legacy.example.com" does not support dry run`},
		},
		{
			name: "the ModelServing was deleted meanwhile",
			err:  apierrors.NewNotFound(schema.GroupResource{Resource: "modelservings"}, "llama"),
			want: DryRunResult{Outcome: DryRunUnknown, Message: `modelservings "llama" not found`},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyDryRun(tt.err); got != tt.want {
				t.Errorf("classifyDryRun() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
