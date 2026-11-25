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
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Identifiers of the findings about the cluster.
const (
	FindingScopeLimited          = "scope-limited"
	FindingReadFailed            = "read-failed"
	FindingControllerNotFound    = "controller-manager-not-found"
	FindingControllerElsewhere   = "controller-manager-in-several-namespaces"
	FindingControllerUpgraded    = "controller-manager-upgraded"
	FindingControllerUpgrading   = "controller-manager-upgrade-in-progress"
	FindingControllerNotRunning  = "controller-manager-not-running"
	FindingControllerRecreate    = "controller-manager-recreate-strategy"
	FindingLeaderElection        = "leader-election"
	FindingCRDNotInstalled       = "servinggroup-crd-not-installed"
	FindingServingGroupsExist    = "servinggroups-exist"
	FindingCordonedNodes         = "cordoned-nodes"
	FindingRollbackSwitchPresent = "rollback-switch-present"
)

// Protocols of a controller manager.
const (
	protocolLegacy       = "legacy"
	protocolServingGroup = "servinggroup"
	protocolMixed        = "mixed"
	protocolNone         = "none"
)

// analyzeCluster reports on what does not belong to one ModelServing.
func analyzeCluster(s *Snapshot) ClusterReport {
	cluster := ClusterReport{
		ControllerManagers: []ControllerManager{},
		ServingGroupCRD:    s.ServingGroupCRD,
		ServingGroups:      []ServingGroupRef{},
		CordonedNodes:      []CordonedNode{},
		RollbackSwitches:   []string{},
		Webhooks:           []Webhook{},
		EvictionWebhook:    "unknown",
		Findings:           []Finding{},
	}
	add := func(finding Finding) { cluster.Findings = append(cluster.Findings, finding) }

	if !s.Scope.AllNamespaces {
		add(Finding{ID: FindingScopeLimited, Level: LevelInfo,
			Message: fmt.Sprintf("Only %s was checked. The upgrade replaces the controller of the ModelServings of every namespace.", s.Scope),
			Action:  "Run the check with --all-namespaces before upgrading."})
	}
	for _, readError := range s.ReadErrors {
		add(Finding{ID: FindingReadFailed, Level: LevelAttention,
			Message: fmt.Sprintf("Could not %s: %s. What depends on it was not checked.", readError.What, readError.Error),
			Action:  "Run the check with permission to read these objects."})
	}

	controllerManagers(s, &cluster, add)

	switch s.ServingGroupCRD {
	case crdNotInstalled:
		add(Finding{ID: FindingCRDNotInstalled, Level: LevelInfo,
			Message: "The ServingGroup CustomResourceDefinition (servinggroups.workload.serving.volcano.sh) is not installed. Helm installs the CustomResourceDefinitions of a chart only with the first install and leaves them alone on upgrade, so it has to be applied by hand. Without it the new controller manager does not become ready and starts no controller, and the Pod of the earlier version keeps running.",
			Action:  "Before helm upgrade, apply the CustomResourceDefinitions of the target version: helm show crds <chart> --version <version> | kubectl apply --server-side -f -"})
	case crdInstalled:
		servingGroups(s, &cluster, add)
	}

	onNode := map[string]int{}
	for _, pod := range s.Pods {
		onNode[pod.Spec.NodeName]++
	}
	var cordoned []string
	for _, node := range s.CordonedNodes {
		cluster.CordonedNodes = append(cluster.CordonedNodes, CordonedNode{Name: node.Name, ModelServingPods: onNode[node.Name]})
		cordoned = append(cordoned, node.Name)
	}
	sort.Slice(cluster.CordonedNodes, func(i, j int) bool { return cluster.CordonedNodes[i].Name < cluster.CordonedNodes[j].Name })
	sort.Strings(cordoned)
	if len(cordoned) > 0 {
		add(Finding{ID: FindingCordonedNodes, Level: LevelInfo, Objects: cordoned,
			Message: fmt.Sprintf("%s cordoned (%s). A ModelServing with Pods on one of them reports it.",
				counted(len(cordoned), "1 node is", "%d nodes are"), listed(cordoned)),
			Action: "Do not drain nodes between the upgrade and its verification."})
	}

	for _, configMap := range s.RollbackSwitches {
		name := configMap.Namespace + "/" + configMap.Name
		cluster.RollbackSwitches = append(cluster.RollbackSwitches, name)
		add(Finding{ID: FindingRollbackSwitchPresent, Level: LevelBlocked, Objects: []string{name},
			Message: fmt.Sprintf("The ConfigMap %s exists. It is the switch that puts the new version into rollback mode: its controllers then take nothing over and reconcile nothing, and hand the workloads they own back to the earlier version.", name),
			Action:  "Delete the ConfigMap before upgrading."})
	}

	// Without the webhook configuration of the chart no webhook is enabled.
	if s.Webhooks != nil || !s.unread(topicWebhooks) {
		cluster.EvictionWebhook = "disabled"
	}
	if s.Webhooks != nil {
		for _, webhook := range s.Webhooks.Webhooks {
			policy := ""
			if webhook.FailurePolicy != nil {
				policy = string(*webhook.FailurePolicy)
			}
			cluster.Webhooks = append(cluster.Webhooks, Webhook{Name: webhook.Name, FailurePolicy: policy})
			if webhook.Name == evictionWebhookName {
				cluster.EvictionWebhook = "enabled"
			}
		}
	}

	sortFindings(cluster.Findings)
	cluster.Verdict = verdictOf(cluster.Findings)
	return cluster
}

// controllerManagers describes the controller manager of every namespace
// that has one.
func controllerManagers(s *Snapshot, cluster *ClusterReport, add func(Finding)) {
	deployments := map[string]*appsv1.Deployment{}
	pods := map[string][]*corev1.Pod{}
	namespaces := map[string]bool{}
	for _, deployment := range s.Deployments {
		deployments[deployment.Namespace] = deployment
		namespaces[deployment.Namespace] = true
	}
	for _, pod := range s.ControllerPods {
		pods[pod.Namespace] = append(pods[pod.Namespace], pod)
		namespaces[pod.Namespace] = true
	}
	sorted := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		sorted = append(sorted, namespace)
	}
	sort.Strings(sorted)

	for _, namespace := range sorted {
		manager := describeControllerManager(namespace, deployments[namespace], pods[namespace])
		cluster.ControllerManagers = append(cluster.ControllerManagers, manager)
		where := "The controller manager in namespace " + namespace

		switch manager.Protocol {
		case protocolServingGroup:
			add(Finding{ID: FindingControllerUpgraded, Level: LevelAttention,
				Message: fmt.Sprintf("%s already keeps the state of a ModelServing in ServingGroup objects: its Pods carry the label %s. This check describes a cluster that still runs an earlier version.",
					where, controllerProtocolLabel),
				Action: "To verify an upgrade that was made, run the check with --compare-baseline."})
		case protocolMixed:
			add(Finding{ID: FindingControllerUpgrading, Level: LevelWait,
				Message: fmt.Sprintf("%s is being upgraded: Pods with and without the label %s exist. The new version starts its controllers when the Pods of the earlier version are gone.",
					where, controllerProtocolLabel),
				Action: "Wait until only Pods of one version remain."})
		case protocolNone:
			add(Finding{ID: FindingControllerNotRunning, Level: LevelAttention,
				Message: fmt.Sprintf("%s has no running Pod. Nothing reconciles ModelServings at the moment, and their status may be out of date.", where)})
		}
		if manager.UpdateStrategy == string(appsv1.RecreateDeploymentStrategyType) {
			add(Finding{ID: FindingControllerRecreate, Level: LevelAttention,
				Message: fmt.Sprintf("%s uses the update strategy Recreate: the Pod of the earlier version is stopped before the Pod of the new version is ready. In between no admission webhook of Kthena answers, and every request that a webhook with failurePolicy Fail has to admit is rejected.", where),
				Action:  "Use the update strategy RollingUpdate, which is the default of the chart."})
		}
		if manager.Deployment != "" {
			election := Finding{ID: FindingLeaderElection, Level: LevelInfo}
			switch {
			case manager.LeaderElectFlag != "unset":
				election.Message = fmt.Sprintf("%s runs with --leader-elect=%s.", where, manager.LeaderElectFlag)
			case manager.LeaderElect:
				election.Message = fmt.Sprintf("%s runs without the flag --leader-elect; this version elects a leader by default.", where)
			default:
				election.Message = fmt.Sprintf("%s runs without the flag --leader-elect; the earlier versions do not elect a leader by default. The new version does.", where)
			}
			if !manager.LeaderElect && manager.Replicas > 1 && manager.Protocol != protocolServingGroup {
				election.Level = LevelAttention
				election.Message += fmt.Sprintf(" It has %d replicas, and every one of them reconciles. The upgrade does not depend on it: the new version waits until every Pod of the earlier version is gone.", manager.Replicas)
			}
			add(election)
		}
	}

	switch {
	case len(sorted) > 1:
		add(Finding{ID: FindingControllerElsewhere, Level: LevelBlocked, Objects: sorted,
			Message: fmt.Sprintf("Controller managers exist in %d namespaces: %s. The new version waits only for the Pods of an earlier version in its own namespace. An earlier version in another namespace keeps reconciling: it takes a Pod whose owner changes for lost and deletes the other Pods of its Role instance or group.",
				len(sorted), listed(sorted)),
			Action: "Remove every installation but the one that is upgraded."})
	case len(sorted) == 0 && !s.unread(topicController):
		add(Finding{ID: FindingControllerNotFound, Level: LevelAttention,
			Message: fmt.Sprintf("No Deployment named %s and no Pod labelled %s=%s exists in any namespace. Which version reconciles the ModelServings, and where, cannot be told, and the rollback switch cannot be looked for. A controller manager that runs outside the cluster or under another label is not noticed by the new version either.",
				controllerManagerName, componentLabel, controllerManagerName),
			Action: "Make sure no controller manager of an earlier version runs when the new version starts."})
	}
}

// describeControllerManager describes the controller manager of one
// namespace from its Deployment, which may be missing, and its Pods.
func describeControllerManager(namespace string, deployment *appsv1.Deployment, pods []*corev1.Pod) ControllerManager {
	manager := ControllerManager{Namespace: namespace, LeaderElectFlag: "unset", Pods: []ControllerPod{}}

	running, upgraded := 0, 0
	for _, pod := range pods {
		manager.Pods = append(manager.Pods, ControllerPod{
			Name: pod.Name, Phase: string(pod.Status.Phase), Node: pod.Spec.NodeName,
			Protocol: pod.Labels[controllerProtocolLabel],
		})
		// A terminating Pod runs until it is gone; a finished one does not.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		running++
		if pod.Labels[controllerProtocolLabel] == controllerProtocolServingGroup {
			upgraded++
		}
	}
	sort.Slice(manager.Pods, func(i, j int) bool { return manager.Pods[i].Name < manager.Pods[j].Name })
	switch {
	case running == 0:
		manager.Protocol = protocolNone
	case upgraded == 0:
		manager.Protocol = protocolLegacy
	case upgraded == running:
		manager.Protocol = protocolServingGroup
	default:
		manager.Protocol = protocolMixed
	}
	// The versions that work with ServingGroup objects elect a leader unless told not to.
	manager.LeaderElect = manager.Protocol == protocolServingGroup

	if deployment == nil {
		return manager
	}
	manager.Deployment = deployment.Name
	manager.Replicas = 1
	if deployment.Spec.Replicas != nil {
		manager.Replicas = *deployment.Spec.Replicas
	}
	manager.ReadyReplicas = deployment.Status.ReadyReplicas
	manager.UpdateStrategy = string(deployment.Spec.Strategy.Type)
	if manager.UpdateStrategy == "" {
		manager.UpdateStrategy = string(appsv1.RollingUpdateDeploymentStrategyType)
	}
	template := deployment.Spec.Template
	manager.LeaderElect = template.Labels[controllerProtocolLabel] == controllerProtocolServingGroup
	if len(template.Spec.Containers) == 0 {
		return manager
	}
	container := template.Spec.Containers[0]
	for _, candidate := range template.Spec.Containers {
		if candidate.Name == controllerManagerName {
			container = candidate
		}
	}
	manager.Image = container.Image
	// The last occurrence of a flag wins.
	for _, argument := range append(slices.Clone(container.Command), container.Args...) {
		name, value, hasValue := strings.Cut(argument, "=")
		if name != "--leader-elect" && name != "-leader-elect" {
			continue
		}
		enabled := true
		if hasValue {
			parsed, err := strconv.ParseBool(value)
			if err != nil {
				continue
			}
			enabled = parsed
		}
		manager.LeaderElectFlag, manager.LeaderElect = strconv.FormatBool(enabled), enabled
	}
	return manager
}

// servingGroups lists the ServingGroup objects that exist and tells whether
// each is controlled by the ModelServing its name suggests.
func servingGroups(s *Snapshot, cluster *ClusterReport, add func(Finding)) {
	modelServings := map[string]*workloadv1alpha1.ModelServing{}
	for _, ms := range s.ModelServings {
		modelServings[ms.Namespace+"/"+ms.Name] = ms
	}
	controlled, foreign, unrelated := 0, 0, 0
	for _, object := range s.ServingGroups {
		ref := ServingGroupRef{Namespace: object.Namespace, Name: object.Name}
		owner := metav1.GetControllerOfNoCopy(object)
		if owner != nil {
			ref.Owner = owner.Kind + "/" + owner.Name
		}
		prefix, _, ok := splitOrdinal(object.Name)
		ms := modelServings[object.Namespace+"/"+prefix]
		switch {
		case !ok || ms == nil:
			// With one ModelServing in scope the others are not known.
			if s.Scope.ModelServing != "" {
				continue
			}
			unrelated++
		case owner != nil && owner.UID == ms.UID:
			ref.ModelServing, ref.Controlled = ms.Name, true
			controlled++
		default:
			ref.ModelServing = ms.Name
			foreign++
		}
		cluster.ServingGroups = append(cluster.ServingGroups, ref)
	}
	sort.Slice(cluster.ServingGroups, func(i, j int) bool {
		a, b := cluster.ServingGroups[i], cluster.ServingGroups[j]
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	if total := len(cluster.ServingGroups); total > 0 {
		add(Finding{ID: FindingServingGroupsExist, Level: LevelInfo,
			Message: fmt.Sprintf("%s: %d controlled by the ModelServing its name suggests, %d that hold the name of a group of a ModelServing that does not control them (reported by that ModelServing), %d whose name is not that of a group of a checked ModelServing.",
				counted(total, "1 ServingGroup object exists", "%d ServingGroup objects exist"), controlled, foreign, unrelated)})
	}
}
