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
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// legacySnapshot returns what Collect reads, with every namespace in scope,
// from a healthy cluster that runs an earlier version: one controller
// manager, no ServingGroup CustomResourceDefinition, and the ModelServing
// default/llama in steady state with three groups.
func legacySnapshot(t *testing.T) *Snapshot {
	t.Helper()
	in := steadyInput(t, 3, 0, 1, 2)
	s := &Snapshot{
		Scope:           Scope{AllNamespaces: true},
		Time:            testTime,
		ModelServings:   []*workloadv1alpha1.ModelServing{in.ms},
		Pods:            in.pods,
		Revisions:       in.revisions,
		ServingGroupCRD: crdNotInstalled,
		Deployments:     []*appsv1.Deployment{controllerDeployment(controllerNamespace)},
		ControllerPods:  []*corev1.Pod{controllerPod(controllerNamespace, "kthena-controller-manager-5d8f7-abcde", "")},
		DryRuns:         map[string]DryRunResult{"default/llama": {Outcome: DryRunPassed}},
	}
	for ordinal := range 3 {
		s.PodGroups = append(s.PodGroups, podGroupOf(in.ms, ordinal))
		s.Services = append(s.Services, serviceOf(in.ms, ordinal))
	}
	return s
}

func TestAnalyzeCluster(t *testing.T) {
	// What a healthy cluster of an earlier version always reports.
	healthy := map[string]Level{FindingCRDNotInstalled: LevelInfo, FindingLeaderElection: LevelInfo}
	with := func(extra map[string]Level) map[string]Level {
		out := maps.Clone(healthy)
		maps.Copy(out, extra)
		return out
	}
	tests := []struct {
		name        string
		mutate      func(s *Snapshot)
		want        map[string]Level
		wantVerdict Verdict
		check       func(t *testing.T, cluster ClusterReport)
	}{
		{
			name:        "a healthy cluster of an earlier version",
			mutate:      func(*Snapshot) {},
			want:        healthy,
			wantVerdict: VerdictReady,
			check: func(t *testing.T, cluster ClusterReport) {
				if len(cluster.ControllerManagers) != 1 {
					t.Fatalf("controllerManagers = %+v, want one", cluster.ControllerManagers)
				}
				want := ControllerManager{
					Namespace: controllerNamespace, Deployment: controllerManagerName,
					Image:    "ghcr.io/volcano-sh/kthena-controller-manager:v1.1.0",
					Replicas: 1, ReadyReplicas: 1, UpdateStrategy: "RollingUpdate",
					LeaderElectFlag: "unset", LeaderElect: false, Protocol: protocolLegacy,
					Pods: []ControllerPod{{Name: "kthena-controller-manager-5d8f7-abcde", Phase: "Running", Node: "node-1"}},
				}
				if got := cluster.ControllerManagers[0]; !reflect.DeepEqual(got, want) {
					t.Errorf("controllerManager = %+v, want %+v", got, want)
				}
				if cluster.ServingGroupCRD != crdNotInstalled || cluster.EvictionWebhook != "disabled" {
					t.Errorf("servingGroupCRD = %q, evictionWebhook = %q", cluster.ServingGroupCRD, cluster.EvictionWebhook)
				}
				finding := findingByID(cluster.Findings, FindingCRDNotInstalled)
				if !strings.Contains(finding.Message, "applied by hand") || !strings.Contains(finding.Action, "kubectl apply --server-side") {
					t.Errorf("the finding does not say that the CRD is applied by hand before helm upgrade: %+v", finding)
				}
			},
		},
		{
			name:        "only one namespace was checked",
			mutate:      func(s *Snapshot) { s.Scope = Scope{Namespace: testNamespace} },
			want:        with(map[string]Level{FindingScopeLimited: LevelInfo}),
			wantVerdict: VerdictReady,
		},
		{
			name: "controller manager Pods in another namespace",
			mutate: func(s *Snapshot) {
				s.ControllerPods = append(s.ControllerPods, controllerPod("staging", "kthena-controller-manager-old", ""))
			},
			want:        with(map[string]Level{FindingControllerElsewhere: LevelBlocked}),
			wantVerdict: VerdictBlocked,
			check: func(t *testing.T, cluster ClusterReport) {
				if got := findingByID(cluster.Findings, FindingControllerElsewhere).Objects; !slices.Equal(got, []string{controllerNamespace, "staging"}) {
					t.Errorf("objects = %v", got)
				}
				if manager := cluster.ControllerManagers[1]; manager.Namespace != "staging" || manager.Deployment != "" || manager.Protocol != protocolLegacy {
					t.Errorf("controllerManagers[1] = %+v, want the Pods of staging without a Deployment", manager)
				}
			},
		},
		{
			name: "a second Deployment in another namespace",
			mutate: func(s *Snapshot) {
				s.Deployments = append(s.Deployments, controllerDeployment("staging"))
			},
			want: with(map[string]Level{
				FindingControllerElsewhere:  LevelBlocked,
				FindingControllerNotRunning: LevelAttention,
			}),
			wantVerdict: VerdictBlocked,
		},
		{
			name: "the controller manager is upgraded already",
			mutate: func(s *Snapshot) {
				s.Deployments = []*appsv1.Deployment{controllerDeployment(controllerNamespace, upgradedTemplate)}
				s.ControllerPods = []*corev1.Pod{controllerPod(controllerNamespace, "new", controllerProtocolServingGroup)}
			},
			want:        with(map[string]Level{FindingControllerUpgraded: LevelAttention}),
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, cluster ClusterReport) {
				manager := cluster.ControllerManagers[0]
				if manager.Protocol != protocolServingGroup || !manager.LeaderElect || manager.LeaderElectFlag != "unset" {
					t.Errorf("controllerManager = %+v, want an upgraded one that elects a leader by default", manager)
				}
				if message := findingByID(cluster.Findings, FindingLeaderElection).Message; !strings.Contains(message, "elects a leader by default") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "the controller manager is being upgraded",
			mutate: func(s *Snapshot) {
				s.Deployments = []*appsv1.Deployment{controllerDeployment(controllerNamespace, upgradedTemplate)}
				s.ControllerPods = append(s.ControllerPods, controllerPod(controllerNamespace, "new", controllerProtocolServingGroup))
			},
			want:        with(map[string]Level{FindingControllerUpgrading: LevelWait}),
			wantVerdict: VerdictWait,
		},
		{
			name: "a finished Pod of an earlier version does not run",
			mutate: func(s *Snapshot) {
				finished := controllerPod(controllerNamespace, "evicted", "")
				finished.Status.Phase = corev1.PodFailed
				s.Deployments = []*appsv1.Deployment{controllerDeployment(controllerNamespace, upgradedTemplate)}
				s.ControllerPods = []*corev1.Pod{finished, controllerPod(controllerNamespace, "new", controllerProtocolServingGroup)}
			},
			want:        with(map[string]Level{FindingControllerUpgraded: LevelAttention}),
			wantVerdict: VerdictAttention,
		},
		{
			name:        "no controller manager Pod is running",
			mutate:      func(s *Snapshot) { s.ControllerPods = nil },
			want:        with(map[string]Level{FindingControllerNotRunning: LevelAttention}),
			wantVerdict: VerdictAttention,
		},
		{
			name: "no controller manager is found",
			mutate: func(s *Snapshot) {
				s.Deployments, s.ControllerPods = nil, nil
			},
			want:        map[string]Level{FindingCRDNotInstalled: LevelInfo, FindingControllerNotFound: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "the controller manager could not be looked for",
			mutate: func(s *Snapshot) {
				s.Deployments, s.ControllerPods = nil, nil
				s.failed(topicController, "list the controller manager Deployments in all namespaces", errForbidden)
			},
			want:        map[string]Level{FindingCRDNotInstalled: LevelInfo, FindingReadFailed: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "the update strategy is Recreate",
			mutate: func(s *Snapshot) {
				s.Deployments[0].Spec.Strategy.Type = appsv1.RecreateDeploymentStrategyType
			},
			want:        with(map[string]Level{FindingControllerRecreate: LevelAttention}),
			wantVerdict: VerdictAttention,
		},
		{
			name: "several replicas without leader election",
			mutate: func(s *Snapshot) {
				s.Deployments[0].Spec.Replicas = ptr(int32(2))
			},
			want:        map[string]Level{FindingCRDNotInstalled: LevelInfo, FindingLeaderElection: LevelAttention},
			wantVerdict: VerdictAttention,
		},
		{
			name: "several replicas with leader election",
			mutate: func(s *Snapshot) {
				s.Deployments = []*appsv1.Deployment{controllerDeployment(controllerNamespace, withArgs("--leader-elect"))}
				s.Deployments[0].Spec.Replicas = ptr(int32(2))
			},
			want:        healthy,
			wantVerdict: VerdictReady,
		},
		{
			name: "the rollback switch exists",
			mutate: func(s *Snapshot) {
				s.RollbackSwitches = []*corev1.ConfigMap{{ObjectMeta: metav1.ObjectMeta{Namespace: controllerNamespace, Name: rollbackSwitchName}}}
			},
			want:        with(map[string]Level{FindingRollbackSwitchPresent: LevelBlocked}),
			wantVerdict: VerdictBlocked,
			check: func(t *testing.T, cluster ClusterReport) {
				if !slices.Equal(cluster.RollbackSwitches, []string{"kthena-system/kthena-servinggroup-rollback"}) {
					t.Errorf("rollbackSwitches = %v", cluster.RollbackSwitches)
				}
			},
		},
		{
			name: "cordoned nodes",
			mutate: func(s *Snapshot) {
				s.CordonedNodes = []*corev1.Node{
					{ObjectMeta: metav1.ObjectMeta{Name: "node-9"}, Spec: corev1.NodeSpec{Unschedulable: true}},
					{ObjectMeta: metav1.ObjectMeta{Name: "node-1"}, Spec: corev1.NodeSpec{Unschedulable: true}},
				}
			},
			want:        with(map[string]Level{FindingCordonedNodes: LevelInfo}),
			wantVerdict: VerdictReady,
			check: func(t *testing.T, cluster ClusterReport) {
				want := []CordonedNode{{Name: "node-1", ModelServingPods: 6}, {Name: "node-9", ModelServingPods: 0}}
				if !slices.Equal(cluster.CordonedNodes, want) {
					t.Errorf("cordonedNodes = %+v, want %+v", cluster.CordonedNodes, want)
				}
			},
		},
		{
			name: "ServingGroup objects exist",
			mutate: func(s *Snapshot) {
				ms := s.ModelServings[0]
				s.ServingGroupCRD = crdInstalled
				s.ServingGroups = []*workloadv1alpha1.ServingGroup{
					servingGroupOf(ms, 0, ms),
					servingGroupOf(ms, 1, nil),
					servingGroupOf(newModelServing("qwen", 1), 0, nil),
				}
			},
			want:        map[string]Level{FindingLeaderElection: LevelInfo, FindingServingGroupsExist: LevelInfo},
			wantVerdict: VerdictReady,
			check: func(t *testing.T, cluster ClusterReport) {
				want := []ServingGroupRef{
					{Namespace: testNamespace, Name: "llama-0", ModelServing: "llama", Controlled: true, Owner: "ModelServing/llama"},
					{Namespace: testNamespace, Name: "llama-1", ModelServing: "llama"},
					{Namespace: testNamespace, Name: "qwen-0"},
				}
				if !slices.Equal(cluster.ServingGroups, want) {
					t.Errorf("servingGroups = %+v, want %+v", cluster.ServingGroups, want)
				}
				if message := findingByID(cluster.Findings, FindingServingGroupsExist).Message; !strings.Contains(message, "3 ServingGroup objects exist: 1 controlled") {
					t.Errorf("message = %s", message)
				}
			},
		},
		{
			name: "the eviction webhook is enabled",
			mutate: func(s *Snapshot) {
				s.Webhooks = &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{Name: modelServingWebhookName, FailurePolicy: ptr(admissionregistrationv1.Fail)},
					{Name: evictionWebhookName, FailurePolicy: ptr(admissionregistrationv1.Ignore)},
				}}
			},
			want:        healthy,
			wantVerdict: VerdictReady,
			check: func(t *testing.T, cluster ClusterReport) {
				want := []Webhook{{Name: modelServingWebhookName, FailurePolicy: "Fail"}, {Name: evictionWebhookName, FailurePolicy: "Ignore"}}
				if cluster.EvictionWebhook != "enabled" || !slices.Equal(cluster.Webhooks, want) {
					t.Errorf("evictionWebhook = %q, webhooks = %+v", cluster.EvictionWebhook, cluster.Webhooks)
				}
			},
		},
		{
			name: "the eviction webhook is not enabled",
			mutate: func(s *Snapshot) {
				s.Webhooks = &admissionregistrationv1.ValidatingWebhookConfiguration{Webhooks: []admissionregistrationv1.ValidatingWebhook{
					{Name: modelServingWebhookName, FailurePolicy: ptr(admissionregistrationv1.Fail)},
				}}
			},
			want:        healthy,
			wantVerdict: VerdictReady,
			check: func(t *testing.T, cluster ClusterReport) {
				if cluster.EvictionWebhook != "disabled" {
					t.Errorf("evictionWebhook = %q, want disabled", cluster.EvictionWebhook)
				}
			},
		},
		{
			name: "the webhook configuration could not be read",
			mutate: func(s *Snapshot) {
				s.failed(topicWebhooks, "get the ValidatingWebhookConfiguration", errForbidden)
			},
			want:        with(map[string]Level{FindingReadFailed: LevelAttention}),
			wantVerdict: VerdictAttention,
			check: func(t *testing.T, cluster ClusterReport) {
				if cluster.EvictionWebhook != "unknown" {
					t.Errorf("evictionWebhook = %q, want unknown", cluster.EvictionWebhook)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := legacySnapshot(t)
			tt.mutate(s)
			cluster := analyzeCluster(s)
			if got := findingLevels(cluster.Findings); !maps.Equal(got, tt.want) {
				t.Errorf("findings = %v, want %v", got, tt.want)
			}
			if cluster.Verdict != tt.wantVerdict {
				t.Errorf("verdict = %s, want %s", cluster.Verdict, tt.wantVerdict)
			}
			for _, finding := range cluster.Findings {
				if finding.Message == "" {
					t.Errorf("finding %s has no message", finding.ID)
				}
				if (finding.Level == LevelWait || finding.Level == LevelBlocked) && finding.Action == "" {
					t.Errorf("finding %s of level %s names no action", finding.ID, finding.Level)
				}
			}
			if tt.check != nil && !t.Failed() {
				tt.check(t, cluster)
			}
		})
	}
}

func TestLeaderElectFlag(t *testing.T) {
	tests := []struct {
		name     string
		mutate   []func(*appsv1.Deployment)
		wantFlag string
		want     bool
	}{
		{name: "not set, earlier version: off", wantFlag: "unset", want: false},
		{name: "not set, new version: on", mutate: []func(*appsv1.Deployment){upgradedTemplate}, wantFlag: "unset", want: true},
		{name: "set without a value", mutate: []func(*appsv1.Deployment){withArgs("--port=8443", "--leader-elect")}, wantFlag: "true", want: true},
		{name: "set to true", mutate: []func(*appsv1.Deployment){withArgs("--leader-elect=true")}, wantFlag: "true", want: true},
		{name: "set to false on the new version", mutate: []func(*appsv1.Deployment){upgradedTemplate, withArgs("--leader-elect=false")}, wantFlag: "false", want: false},
		{name: "the last occurrence wins", mutate: []func(*appsv1.Deployment){withArgs("--leader-elect=true", "--leader-elect=false")}, wantFlag: "false", want: false},
		{name: "a value that is not a boolean is ignored", mutate: []func(*appsv1.Deployment){withArgs("--leader-elect=maybe")}, wantFlag: "unset", want: false},
		{name: "another flag with the same prefix", mutate: []func(*appsv1.Deployment){withArgs("--leader-elect-lease=30s")}, wantFlag: "unset", want: false},
		{
			name: "given in the command of the container",
			mutate: []func(*appsv1.Deployment){func(deployment *appsv1.Deployment) {
				deployment.Spec.Template.Spec.Containers[0].Command = []string{"/kthena-controller-manager", "--leader-elect"}
			}},
			wantFlag: "true", want: true,
		},
		{
			name: "read from the controller manager container, not from a sidecar",
			mutate: []func(*appsv1.Deployment){func(deployment *appsv1.Deployment) {
				sidecar := corev1.Container{Name: "proxy", Image: "proxy:1", Args: []string{"--leader-elect"}}
				deployment.Spec.Template.Spec.Containers = append([]corev1.Container{sidecar}, deployment.Spec.Template.Spec.Containers...)
			}},
			wantFlag: "unset", want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := describeControllerManager(controllerNamespace, controllerDeployment(controllerNamespace, tt.mutate...), nil)
			if manager.LeaderElectFlag != tt.wantFlag || manager.LeaderElect != tt.want {
				t.Errorf("leaderElectFlag = %q, leaderElect = %t, want %q, %t", manager.LeaderElectFlag, manager.LeaderElect, tt.wantFlag, tt.want)
			}
			if manager.Image != "ghcr.io/volcano-sh/kthena-controller-manager:v1.1.0" {
				t.Errorf("image = %q, want the image of the controller manager container", manager.Image)
			}
		})
	}
}
