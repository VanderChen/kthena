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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// existingObjects reports ServingGroup objects that hold the name of a group
// without belonging to the ModelServing, and the PodGroups and Services of
// groups that have no Pod to take over.
func (a *analysis) existingObjects() {
	ms := a.in.ms
	var taken []string
	for _, object := range a.in.groups {
		if !controlledBy(object, ms.UID) {
			taken = append(taken, object.Name)
		}
	}
	sort.Strings(taken)
	if len(taken) > 0 {
		a.add(Finding{ID: FindingServingGroupNameTaken, Level: LevelBlocked, Objects: taken,
			Message: fmt.Sprintf("%s the name of a group of this ModelServing without being controlled by it (%s). The new controller neither deletes nor adopts such an object: the group at that ordinal cannot be taken over or created.",
				counted(len(taken), "1 ServingGroup object has", "%d ServingGroup objects have"), listed(taken)),
			Action: "Delete these ServingGroup objects before upgrading."})
	}

	live := map[string]bool{}
	for _, g := range a.live {
		live[g.name] = true
	}
	var leftover []string
	for _, podGroup := range a.in.podGroups {
		if controlledBy(podGroup, ms.UID) && !live[podGroup.Labels[workloadv1alpha1.GroupNameLabelKey]] {
			leftover = append(leftover, "PodGroup "+podGroup.Name)
		}
	}
	for _, service := range a.in.services {
		if controlledBy(service, ms.UID) && !live[service.Labels[workloadv1alpha1.GroupNameLabelKey]] {
			leftover = append(leftover, "Service "+service.Name)
		}
	}
	sort.Strings(leftover)
	if len(leftover) > 0 {
		a.add(Finding{ID: FindingObjectsWithoutPods, Level: LevelInfo, Objects: leftover,
			Message: fmt.Sprintf("%s to a group without a Pod to take over (%s). The takeover leaves such an object as it is, controlled by the ModelServing. It is taken over in place when a group of that name is created; otherwise it stays until the ModelServing is deleted. No Pod is affected.",
				counted(len(leftover), "1 object belongs", "%d objects belong"), listed(leftover))})
	}
}

// dryRunResult reports a dry-run write that did not pass.
func (a *analysis) dryRunResult() {
	result := a.in.dryRun
	annotation := workloadv1alpha1.ServingGroupAdoptionAnnotationKey
	switch {
	case result.Outcome == DryRunRejected && result.Webhook == modelServingWebhookName:
		a.add(Finding{ID: FindingDryRunRejectedByOwn, Level: LevelAttention,
			Message: fmt.Sprintf("The validating webhook of the running version rejects an update that only adds the annotation %s: %s. The ModelServing does not pass the validation rules although it is running. The new version admits an update that leaves the spec unchanged, so the takeover can record its progress; a change of the spec is rejected until the object is corrected.",
				annotation, result.Message),
			Action: "Correct the spec with its next change."})
	case result.Outcome == DryRunRejected:
		by := "The API server"
		if result.Webhook != "" {
			by = fmt.Sprintf("The admission webhook %q", result.Webhook)
		}
		a.add(Finding{ID: FindingDryRunRejected, Level: LevelBlocked,
			Message: fmt.Sprintf("%s rejects an update that only adds the annotation %s: %s. The new controller records the takeover with that write. While it is rejected the ModelServing is not taken over and is neither rolled, scaled nor recovered.",
				by, annotation, result.Message),
			Action: "Make the update pass before upgrading: correct the object, or the policy that rejects it."})
	case result.Outcome == DryRunUnknown:
		a.add(Finding{ID: FindingDryRunUnknown, Level: LevelAttention,
			Message: fmt.Sprintf("Whether an update that adds the annotation %s is admitted could not be found out: %s.", annotation, result.Message),
			Action:  "Run the check with permission to patch ModelServings while the webhooks can be reached, or pass --skip-dry-run to leave this check out."})
	}
}

// environment reports what around the ModelServing changes the effect of the
// upgrade: cordoned nodes and PodDisruptionBudgets.
func (a *analysis) environment() {
	var live, cordoned []*corev1.Pod
	for _, g := range a.live {
		live = append(live, g.live...)
	}
	nodes := map[string]bool{}
	for _, pod := range live {
		if a.in.cordoned[pod.Spec.NodeName] {
			cordoned = append(cordoned, pod)
			nodes[pod.Spec.NodeName] = true
		}
	}
	if len(cordoned) > 0 {
		names := make([]string, 0, len(nodes))
		for node := range nodes {
			names = append(names, node)
		}
		sort.Strings(names)
		a.add(Finding{ID: FindingPodsOnCordonedNodes, Level: LevelAttention, Objects: podNames(cordoned),
			Message: fmt.Sprintf("%s on cordoned nodes (%s). While the controller manager is replaced nothing reconciles the ModelServing: a Pod that is lost then, for instance to a node drain, is only created again, and its Role instance or group is not recovered by recoveryPolicy.",
				counted(len(cordoned), "1 Pod runs", "%d Pods run"), listed(names)),
			Action: "Do not drain nodes between the upgrade and its verification."})
	}

	var budgets []string
	for _, budget := range a.in.budgets {
		// Only these two forms take the number of expected Pods from the scale
		// of the controller owner of the Pods.
		percentage := budget.Spec.MinAvailable != nil && budget.Spec.MinAvailable.Type == intstr.String
		if budget.Spec.MaxUnavailable == nil && !percentage {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(budget.Spec.Selector)
		if err != nil {
			continue
		}
		if slices.ContainsFunc(live, func(pod *corev1.Pod) bool { return selector.Matches(labels.Set(pod.Labels)) }) {
			budgets = append(budgets, budget.Name)
		}
	}
	sort.Strings(budgets)
	if len(budgets) > 0 {
		a.add(Finding{ID: FindingDisruptionBudget, Level: LevelAttention, Objects: budgets,
			Message: fmt.Sprintf("%s Pods of this ModelServing with maxUnavailable or a percentage (%s). Such a budget takes the number of expected Pods from the controller owner of the Pods: today the ModelServing, whose scale is the number of groups; after the upgrade the ServingGroups, whose scale is the number of Pods of the group. The number of evictions the budget allows changes with the upgrade.",
				counted(len(budgets), "1 PodDisruptionBudget selects", "%d PodDisruptionBudgets select"), listed(budgets)),
			Action: "Check the allowed disruptions of the budget after the upgrade, or express it as an absolute minAvailable."})
	}
}
