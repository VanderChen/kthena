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
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	kthena "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Names that the chart and the controller manager fix. They are repeated
// here so that the CLI does not link the controller manager.
const (
	// controllerManagerName is the name of the controller manager Deployment
	// and the value of componentLabel on it and on its Pods.
	controllerManagerName = "kthena-controller-manager"
	componentLabel        = "app.kubernetes.io/component"
	// controllerProtocolLabel is carried by the controller manager Pods of the
	// versions that keep the state of a ModelServing in ServingGroup objects.
	controllerProtocolLabel        = "modelserving.volcano.sh/controller-protocol"
	controllerProtocolServingGroup = "servinggroup"
	// rollbackSwitchName is the ConfigMap, in the namespace of the controller
	// manager, that puts the new version into rollback mode.
	rollbackSwitchName = "kthena-servinggroup-rollback"
	// webhookConfigurationName is the ValidatingWebhookConfiguration of the chart.
	webhookConfigurationName = "kthena-controller-manager-validating-webhook"
	// modelServingWebhookName is the webhook that validates ModelServings.
	modelServingWebhookName = "validate-workload-ai-v1alpha1-modelserving.volcano.sh"
	// evictionWebhookName is the webhook that guards evictions of ModelServing Pods.
	evictionWebhookName = "eviction.modelserving.volcano.sh"
)

// States of the ServingGroup CustomResourceDefinition.
const (
	crdInstalled    = "installed"
	crdNotInstalled = "not-installed"
	crdUnknown      = "unknown"
)

// Clients are the API clients the check reads the cluster with.
type Clients struct {
	Kube    kubernetes.Interface
	Kthena  kthena.Interface
	Volcano volcano.Interface
}

// CollectOptions says what Collect reads.
type CollectOptions struct {
	Scope Scope
	// SkipDryRun leaves out the dry-run write on every ModelServing.
	SkipDryRun bool
	// WorkloadsOnly leaves out what only the pre-flight report needs: the
	// cluster-level objects, the ControllerRevisions and the dry-run. It is
	// what a comparison with a baseline reads.
	WorkloadsOnly bool
}

// What a read that may fail is about.
const (
	topicServingGroups = "servinggroups"
	topicBudgets       = "budgets"
	topicController    = "controller-manager"
	topicNodes         = "nodes"
	topicRollback      = "rollback-switch"
	topicWebhooks      = "webhooks"
)

// ReadError is a read that failed without failing the check. What depends
// on it is reported as unknown.
type ReadError struct {
	// Topic is one of the topic constants.
	Topic string
	// What names the read, as in "could not <what>".
	What  string
	Error string
}

// Snapshot is everything the check reads from the cluster.
type Snapshot struct {
	Scope Scope
	Time  time.Time
	// DryRunSkipped reports that no dry-run write was made.
	DryRunSkipped bool

	ModelServings []*workloadv1alpha1.ModelServing
	// Pods, Services, PodGroups and Revisions carry the name label of a
	// ModelServing. Budgets are all PodDisruptionBudgets of the scope.
	Pods      []*corev1.Pod
	Services  []*corev1.Service
	PodGroups []*schedulingv1beta1.PodGroup
	Revisions []*appsv1.ControllerRevision
	Budgets   []*policyv1.PodDisruptionBudget
	// ServingGroupCRD is crdInstalled, crdNotInstalled or crdUnknown.
	ServingGroupCRD string
	ServingGroups   []*workloadv1alpha1.ServingGroup

	// Deployments are the Deployments named kthena-controller-manager, and
	// ControllerPods the Pods with its component label, in every namespace.
	Deployments      []*appsv1.Deployment
	ControllerPods   []*corev1.Pod
	CordonedNodes    []*corev1.Node
	RollbackSwitches []*corev1.ConfigMap
	Webhooks         *admissionregistrationv1.ValidatingWebhookConfiguration
	// DryRuns maps namespace/name of a ModelServing to the outcome of its
	// dry-run write.
	DryRuns map[string]DryRunResult

	ReadErrors []ReadError
}

func pointers[T any](items []T) []*T {
	out := make([]*T, len(items))
	for i := range items {
		out[i] = &items[i]
	}
	return out
}

// Collect reads the cluster. It writes nothing, except that every
// ModelServing is patched once with the dry-run option, which the API server
// validates and admits like a real write and then drops.
//
// A read the check cannot do without fails Collect. A read that only some
// findings depend on is recorded in ReadErrors.
func Collect(ctx context.Context, clients Clients, opts CollectOptions) (*Snapshot, error) {
	s := &Snapshot{
		Scope:           opts.Scope,
		Time:            time.Now(),
		DryRunSkipped:   opts.SkipDryRun || opts.WorkloadsOnly,
		ServingGroupCRD: crdUnknown,
		DryRuns:         map[string]DryRunResult{},
	}
	namespace := opts.Scope.Namespace
	if opts.Scope.AllNamespaces {
		namespace = metav1.NamespaceAll
	}
	// The objects of a ModelServing all carry its name in this label.
	selector := workloadv1alpha1.ModelServingNameLabelKey
	if opts.Scope.ModelServing != "" {
		selector += "=" + opts.Scope.ModelServing
	}
	labelled := metav1.ListOptions{LabelSelector: selector}

	if opts.Scope.ModelServing != "" {
		ms, err := clients.Kthena.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, opts.Scope.ModelServing, metav1.GetOptions{})
		switch {
		case err == nil:
			s.ModelServings = []*workloadv1alpha1.ModelServing{ms}
		case apierrors.IsNotFound(err) && opts.WorkloadsOnly:
			// The comparison reports a ModelServing of the baseline that is gone.
		default:
			return nil, fmt.Errorf("failed to get ModelServing '%s': %v", opts.Scope.ModelServing, err)
		}
	} else {
		list, err := clients.Kthena.WorkloadV1alpha1().ModelServings(namespace).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("failed to list ModelServings: %v", err)
		}
		s.ModelServings = pointers(list.Items)
	}

	pods, err := clients.Kube.CoreV1().Pods(namespace).List(ctx, labelled)
	if err != nil {
		return nil, fmt.Errorf("failed to list Pods: %v", err)
	}
	s.Pods = pointers(pods.Items)

	services, err := clients.Kube.CoreV1().Services(namespace).List(ctx, labelled)
	if err != nil {
		return nil, fmt.Errorf("failed to list Services: %v", err)
	}
	s.Services = pointers(services.Items)

	// Without Volcano there is no PodGroup resource and nothing to record.
	podGroups, err := clients.Volcano.SchedulingV1beta1().PodGroups(namespace).List(ctx, labelled)
	switch {
	case err == nil:
		s.PodGroups = pointers(podGroups.Items)
	case !apierrors.IsNotFound(err):
		return nil, fmt.Errorf("failed to list PodGroups: %v", err)
	}

	// The resource is not served before the CustomResourceDefinition is installed.
	groups, err := clients.Kthena.WorkloadV1alpha1().ServingGroups(namespace).List(ctx, metav1.ListOptions{})
	switch {
	case err == nil:
		s.ServingGroupCRD = crdInstalled
		s.ServingGroups = pointers(groups.Items)
	case apierrors.IsNotFound(err):
		s.ServingGroupCRD = crdNotInstalled
	default:
		s.failed(topicServingGroups, "list ServingGroups", err)
	}

	if opts.WorkloadsOnly {
		return s, nil
	}

	revisions, err := clients.Kube.AppsV1().ControllerRevisions(namespace).List(ctx, labelled)
	if err != nil {
		return nil, fmt.Errorf("failed to list ControllerRevisions: %v", err)
	}
	s.Revisions = pointers(revisions.Items)

	if budgets, err := clients.Kube.PolicyV1().PodDisruptionBudgets(namespace).List(ctx, metav1.ListOptions{}); err != nil {
		s.failed(topicBudgets, "list PodDisruptionBudgets", err)
	} else {
		s.Budgets = pointers(budgets.Items)
	}

	s.collectCluster(ctx, clients.Kube)

	if !opts.SkipDryRun {
		for _, ms := range s.ModelServings {
			s.DryRuns[ms.Namespace+"/"+ms.Name] = dryRun(ctx, clients.Kthena, ms)
		}
	}
	return s, nil
}

func (s *Snapshot) failed(topic, what string, err error) {
	s.ReadErrors = append(s.ReadErrors, ReadError{Topic: topic, What: what, Error: err.Error()})
}

// unread reports whether a read about topic failed.
func (s *Snapshot) unread(topic string) bool {
	for _, readError := range s.ReadErrors {
		if readError.Topic == topic {
			return true
		}
	}
	return false
}

// collectCluster reads what does not belong to one ModelServing. The
// controller manager is looked for in every namespace: an installation in a
// namespace nobody expects is exactly what has to be found.
func (s *Snapshot) collectCluster(ctx context.Context, kube kubernetes.Interface) {
	namespaces := map[string]bool{}

	deployments, err := kube.AppsV1().Deployments(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		FieldSelector: "metadata.name=" + controllerManagerName,
	})
	if err != nil {
		s.failed(topicController, "list the controller manager Deployments in all namespaces", err)
	} else {
		for i := range deployments.Items {
			if deployment := &deployments.Items[i]; deployment.Name == controllerManagerName {
				s.Deployments = append(s.Deployments, deployment)
				namespaces[deployment.Namespace] = true
			}
		}
	}

	pods, err := kube.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
		LabelSelector: componentLabel + "=" + controllerManagerName,
	})
	if err != nil {
		s.failed(topicController, "list the controller manager Pods in all namespaces", err)
	} else {
		s.ControllerPods = pointers(pods.Items)
		for _, pod := range s.ControllerPods {
			namespaces[pod.Namespace] = true
		}
	}

	nodes, err := kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{FieldSelector: "spec.unschedulable=true"})
	if err != nil {
		s.failed(topicNodes, "list the cordoned nodes", err)
	} else {
		for i := range nodes.Items {
			if node := &nodes.Items[i]; node.Spec.Unschedulable {
				s.CordonedNodes = append(s.CordonedNodes, node)
			}
		}
	}

	sorted := make([]string, 0, len(namespaces))
	for namespace := range namespaces {
		sorted = append(sorted, namespace)
	}
	sort.Strings(sorted)
	for _, namespace := range sorted {
		configMap, err := kube.CoreV1().ConfigMaps(namespace).Get(ctx, rollbackSwitchName, metav1.GetOptions{})
		switch {
		case err == nil:
			s.RollbackSwitches = append(s.RollbackSwitches, configMap)
		case !apierrors.IsNotFound(err):
			s.failed(topicRollback, fmt.Sprintf("get the rollback switch ConfigMap %s/%s", namespace, rollbackSwitchName), err)
		}
	}

	webhooks, err := kube.AdmissionregistrationV1().ValidatingWebhookConfigurations().Get(ctx, webhookConfigurationName, metav1.GetOptions{})
	switch {
	case err == nil:
		s.Webhooks = webhooks
	case !apierrors.IsNotFound(err):
		s.failed(topicWebhooks, "get the ValidatingWebhookConfiguration "+webhookConfigurationName, err)
	}
}

// dryRun sends the write the new ModelServing controller starts the takeover
// with: a merge patch that sets the adoption annotation. With the dry-run
// option the API server runs admission and validation and persists nothing.
//
// A ModelServing that already has the annotation was taken over, or is being
// taken over, by the new version; that write has happened.
func dryRun(ctx context.Context, client kthena.Interface, ms *workloadv1alpha1.ModelServing) DryRunResult {
	if value, ok := ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; ok {
		return DryRunResult{Outcome: DryRunSkipped, Message: fmt.Sprintf("the adoption annotation is already %q", value)}
	}
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{
		"annotations": map[string]string{
			workloadv1alpha1.ServingGroupAdoptionAnnotationKey: workloadv1alpha1.ServingGroupAdoptionInProgress,
		},
	}})
	if err != nil {
		return DryRunResult{Outcome: DryRunUnknown, Message: err.Error()}
	}
	_, err = client.WorkloadV1alpha1().ModelServings(ms.Namespace).Patch(ctx, ms.Name, types.MergePatchType, patch,
		metav1.PatchOptions{DryRun: []string{metav1.DryRunAll}})
	return classifyDryRun(err)
}

// deniedByWebhook matches the message the API server returns when an
// admission webhook denies a request.
var deniedByWebhook = regexp.MustCompile(`admission webhook "([^"]+)" denied the request: (.*)`)

// classifyDryRun tells a rejection of the write from a failure to find out.
// A rejection comes from admission or from validation and would meet the
// controller's write as well. Anything else (no permission to patch, a
// webhook that cannot be reached or does not support dry-run) says nothing
// about the object.
func classifyDryRun(err error) DryRunResult {
	if err == nil {
		return DryRunResult{Outcome: DryRunPassed}
	}
	if match := deniedByWebhook.FindStringSubmatch(err.Error()); match != nil {
		return DryRunResult{Outcome: DryRunRejected, Webhook: match[1], Message: match[2]}
	}
	if apierrors.IsInvalid(err) {
		return DryRunResult{Outcome: DryRunRejected, Message: err.Error()}
	}
	return DryRunResult{Outcome: DryRunUnknown, Message: err.Error()}
}
