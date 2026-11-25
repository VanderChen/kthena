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

package sgcontroller

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// headlessServices lists the Services as "<name>:<generation>", without the
// name of the group, and with ! for one that the group does not control.
func (h *harness) headlessServices() string {
	h.t.Helper()
	list, err := h.cluster.Kube.CoreV1().Services(testNamespace).List(context.Background(), metav1.ListOptions{})
	if err != nil {
		h.t.Fatal(err)
	}
	var out []string
	for _, service := range list.Items {
		item := strings.TrimPrefix(service.Name, testGroup+"-") + ":" + service.Labels[workloadv1alpha1.InstanceGenerationLabelKey]
		if owner := metav1.GetControllerOf(&service); owner == nil || owner.UID != "sg-uid" || owner.BlockOwnerDeletion == nil || !*owner.BlockOwnerDeletion {
			item += "!"
		}
		out = append(out, item)
	}
	sort.Strings(out)
	return strings.Join(out, " ")
}

var headless = uses(plugins.HeadlessServicePluginName)

// The ServingGroup controller with the headless Service plugin, as it runs
// in production: every Role instance with workers has its two Services, the
// group controls them, and they follow the instance when it is scaled away
// and when it is created again.
func TestHeadlessServicesFollowTheRoleInstances(t *testing.T) {
	h := standard(t, headless, instancesOfRole(2))
	h.settle()
	const both = "prefill-0-0-discovery:0 prefill-0-0:0 prefill-1-0-discovery:0 prefill-1-0:0"
	if got := h.headlessServices(); got != both {
		t.Fatalf("Services = %q, want %q", got, both)
	}
	entry, err := h.cluster.GetService(testNamespace, "llama-0-prefill-1-0")
	if err != nil {
		t.Fatal(err)
	}
	wantSelector := map[string]string{
		workloadv1alpha1.GroupNameLabelKey: testGroup, workloadv1alpha1.RoleLabelKey: "prefill",
		workloadv1alpha1.RoleIDKey: "prefill-1", workloadv1alpha1.EntryLabelKey: "true",
	}
	if entry.Spec.ClusterIP != corev1.ClusterIPNone || !reflect.DeepEqual(entry.Spec.Selector, wantSelector) ||
		entry.Labels[workloadv1alpha1.GroupNameLabelKey] != testGroup || entry.Labels[workloadv1alpha1.RoleLabelKey] != "prefill" ||
		entry.Labels[workloadv1alpha1.RoleIDKey] != "prefill-1" || entry.Labels[workloadv1alpha1.ModelServingNameLabelKey] != "llama" {
		t.Errorf("entry Service: clusterIP %q, selector %v, labels %v", entry.Spec.ClusterIP, entry.Spec.Selector, entry.Labels)
	}
	// The Pods are in the domain of the discovery Service of their instance
	// and know the address of their entry Pod.
	for _, pod := range h.pods() {
		instance := strings.TrimSuffix(pod.Name, pod.Name[strings.LastIndex(pod.Name, "-"):])
		address := corev1.EnvVar{Name: workloadv1alpha1.EntryAddressEnv, Value: instance + "-0." + testNamespace}
		found := false
		for _, env := range pod.Spec.Containers[0].Env {
			found = found || env == address
		}
		if pod.Spec.Hostname != pod.Name || pod.Spec.Subdomain != instance+"-0-discovery" || !found {
			t.Errorf("Pod %s: hostname %q, subdomain %q, env %v", pod.Name, pod.Spec.Hostname, pod.Spec.Subdomain, pod.Spec.Containers[0].Env)
		}
	}
	uids := map[string]string{}
	for _, name := range []string{"llama-0-prefill-0-0", "llama-0-prefill-1-0"} {
		service, _ := h.cluster.GetService(testNamespace, name)
		uids[name] = string(service.UID)
	}

	// At rest nothing is written.
	if writes := h.mustRound(); len(writes) != 0 {
		t.Errorf("writes of a round at rest: %v", writes)
	}

	// A Service that is lost comes back.
	if err := h.cluster.Kube.CoreV1().Services(testNamespace).Delete(context.Background(), "llama-0-prefill-0-0-discovery", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	h.settle()
	if got := h.headlessServices(); got != both {
		t.Errorf("Services after one was deleted = %q, want %q", got, both)
	}

	// An instance that is scaled away takes its Services with it, and no others.
	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 1 })
	h.settle()
	if got := h.headlessServices(); got != "prefill-0-0-discovery:0 prefill-0-0:0" {
		t.Errorf("Services after the scale-in = %q", got)
	}

	// While an instance is drained its Services are not made again; they are
	// removed when it is drained and made for the generation that follows.
	h.cluster.HoldPodTermination(true)
	h.lose("llama-0-prefill-0-1")
	h.cluster.FinishTermination(testNamespace, "llama-0-prefill-0-1")
	h.mustRound()
	for _, name := range []string{"llama-0-prefill-0-0", "llama-0-prefill-0-0-discovery"} {
		if err := h.cluster.Kube.CoreV1().Services(testNamespace).Delete(context.Background(), name, metav1.DeleteOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	h.mustRound()
	if got := h.ledger() + " [" + h.headlessServices() + "]"; got != "prefill-0:Draining/0 []" {
		t.Errorf("while the instance is drained: %s; want no Service made for it", got)
	}
	h.cluster.HoldPodTermination(false)
	h.finishTerminations()
	h.settle()
	if got := h.ledger() + " [" + h.headlessServices() + "]"; got != "prefill-0:Established/1 [prefill-0-0-discovery:1 prefill-0-0:1]" {
		t.Errorf("after the instance was created again: %s", got)
	}
	if service, _ := h.cluster.GetService(testNamespace, "llama-0-prefill-0-0"); string(service.UID) == uids["llama-0-prefill-0-0"] {
		t.Error("the Service of the earlier generation is still there")
	}
}

// What the controller does around the headless Service plugin when there is
// nothing to make, or the making fails.
func TestHeadlessServicesAreMadeOnlyWhereTheyBelong(t *testing.T) {
	t.Run("no plugin, no Service", func(t *testing.T) {
		h := standard(t)
		h.settle()
		if got := h.headlessServices(); got != "" {
			t.Errorf("Services without the plugin: %q", got)
		}
	})
	t.Run("a Role without workers has none", func(t *testing.T) {
		h := standard(t, headless, func(ms *workloadv1alpha1.ModelServing) {
			ms.Spec.Template.Roles[0].WorkerReplicas, ms.Spec.Template.Roles[0].WorkerTemplate = 0, nil
		})
		h.settle()
		if got := h.headlessServices() + " " + h.conditions(); got != " Ready=AllInstancesReady" {
			t.Errorf("Services and conditions of a Role without workers: %q", got)
		}
	})
	t.Run("a group that is being deleted gets none", func(t *testing.T) {
		ms := testModelServing(headless)
		group := groupOf(ms, "rev-a")
		group.DeletionTimestamp, group.Finalizers = ptr.To(metav1.Now()), []string{metav1.FinalizerDeleteDependents}
		h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), group)
		if writes := h.mustRound(); len(writes) != 0 || h.headlessServices() != "" {
			t.Errorf("writes %v, Services %q; want nothing for a group that is going", writes, h.headlessServices())
		}
	})
	// A Service that an earlier version made, with the ModelServing as its
	// controller, is taken over as it is: the name resolves throughout.
	t.Run("a Service of an earlier version is handed over", func(t *testing.T) {
		ms := testModelServing(headless)
		legacy := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: testNamespace, Name: "llama-0-prefill-0-0", UID: "legacy-uid",
				Labels: map[string]string{
					plugins.HeadlessServicePluginLabelKey:     plugins.HeadlessServicePluginName,
					workloadv1alpha1.ModelServingNameLabelKey: "llama",
					workloadv1alpha1.GroupNameLabelKey:        testGroup,
					workloadv1alpha1.RoleLabelKey:             "prefill",
					workloadv1alpha1.RoleIDKey:                "prefill-0",
				},
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: workloadv1alpha1.ModelServingKind.Kind,
					Name: "llama", UID: "ms-uid", Controller: ptr.To(true),
				}},
			},
			Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		}
		h := newHarness(t, ms, revisionOf(t, ms, "rev-a"), groupOf(ms, "rev-a"), legacy)
		h.settle()
		if got := h.headlessServices(); got != "prefill-0-0-discovery:0 prefill-0-0:0" {
			t.Errorf("Services = %q", got)
		}
		if service, _ := h.cluster.GetService(testNamespace, legacy.Name); service.UID != "legacy-uid" {
			t.Errorf("the Service of the earlier version was replaced: UID %s", service.UID)
		}
	})
	// The Services come first. A Pod without them would start with a
	// subdomain that does not resolve.
	t.Run("no Pod is created while the Services cannot be made", func(t *testing.T) {
		h := standard(t, headless)
		refused := apierrors.NewInternalError(errors.New("temporary API failure"))
		h.cluster.Kube.PrependReactor("create", "services", func(k8stesting.Action) (bool, runtime.Object, error) { return true, nil, refused })
		_, err := h.round()
		if err == nil || !strings.Contains(err.Error(), "temporary API failure") {
			t.Fatalf("round error = %v, want the failure to make the Service", err)
		}
		if got := h.podSummary() + " " + h.conditions(); got != " ReconcileFailed=Error" {
			t.Errorf("Pods and conditions = %q; want no Pod and the failure reported", got)
		}
	})
}

// The delete hook asks for what belongs to a Role instance to be deleted. It
// does not wait for it to be gone, and neither does the group: a Service that
// a finalizer holds must not keep a Role from shrinking.
func TestADrainDoesNotWaitForTheServicesToBeGone(t *testing.T) {
	h := standard(t, headless, instancesOfRole(2))
	h.settle()
	// The API server accepts the deletion of a Service and keeps the object.
	var asked []string
	h.cluster.Kube.PrependReactor("delete", "services", func(action k8stesting.Action) (bool, runtime.Object, error) {
		asked = append(asked, action.(k8stesting.DeleteAction).GetName())
		return true, nil, nil
	})

	h.updateGroup(func(group *workloadv1alpha1.ServingGroup) { group.Spec.Roles[0].Replicas = 1 })
	h.settle()
	if got := h.ledger() + " " + h.conditions(); got != "prefill-0:Established/0 Ready=AllInstancesReady" {
		t.Errorf("after the scale-in: %q; want the instance forgotten", got)
	}
	sort.Strings(asked)
	if want := []string{"llama-0-prefill-1-0", "llama-0-prefill-1-0-discovery"}; !reflect.DeepEqual(asked, want) {
		t.Errorf("Services asked to be deleted: %v, want %v, each once", asked, want)
	}
	if got := h.headlessServices(); !strings.Contains(got, "prefill-1-0:0") {
		t.Errorf("Services = %q; the test wants the Service to be still there", got)
	}
}
