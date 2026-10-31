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

package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"
)

func discoveryControllerFixture(t *testing.T) (*ModelServingController, *kubefake.Clientset, *api.ModelServing, *corev1.Pod) {
	t.Helper()
	ms, pods := recoveryFixture(api.NoneRestartPolicy, 0)
	ms.Spec.Plugins = []api.PluginSpec{{Name: plugins.PodDiscoveryPluginName, Type: api.PluginTypeBuiltIn}}
	pod := pods[0]
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
	pod.Status.PodIP = "10.0.0.1"
	pod.Annotations = map[string]string{groupInstanceAnnotation: "group-instance"}
	c, kube := recoveryController(t, ms, pod)
	kube.ClearActions()
	return c, kube, ms, pod
}

func TestDiscoveryUnchangedInputsHaveNoAPIWork(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	cm, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), "ms-0"+plugins.ConfigMapNameSuffix, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, `{"prefill":["10.0.0.1"]}`, cm.Data[plugins.DiscoveryFileName])
	kube.ClearActions()
	for i := 0; i < 20; i++ {
		changed := pod.DeepCopy()
		changed.Status.Message = "different metadata/condition input"
		changed.Status.ContainerStatuses = []corev1.ContainerStatus{{RestartCount: int32(i)}}
		if i%2 == 0 {
			changed.Status.Conditions[0].Status = corev1.ConditionTrue
		}
		require.NoError(t, c.podsInformer.GetIndexer().Update(changed))
		require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	}
	require.Empty(t, kube.Actions(), "ordinary updates and Ready flips do not add Discovery API calls")
	c.retirePodDiscovery(ms)
	require.Empty(t, c.discovery.owners)
}

func TestDiscoveryPendingOutputRetriesAfterReadyWithoutHookReplay(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	fail := true
	kube.PrependReactor("list", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		if fail {
			return true, nil, apierrors.NewServiceUnavailable("unavailable")
		}
		return false, nil, nil
	})
	require.Error(t, c.maintainPodDiscovery(context.Background(), ms))
	ready := pod.DeepCopy()
	ready.Status.Conditions[0].Status = corev1.ConditionTrue
	updateRecoveryPod(t, c, ready)
	fail = false
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	_, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), "ms-0"+plugins.ConfigMapNameSuffix, metav1.GetOptions{})
	require.NoError(t, err)
	kube.ClearActions()
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	require.Empty(t, kube.Actions())
}

func TestDiscoveryReadySuccessCannotAcknowledgeNewerCacheInputs(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	ready := pod.DeepCopy()
	ready.Status.Conditions[0].Status = corev1.ConditionTrue
	updateRecoveryPod(t, c, ready)
	record := c.recordPodDiscovery(ms, ready)
	chain, err := c.buildPluginChain(ms)
	require.NoError(t, err)
	changed := pod.DeepCopy()
	changed.Status.PodIP = "10.0.0.2"
	first := true
	kube.PrependReactor("list", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		if !first {
			return false, nil, nil
		}
		first = false
		// Cache advances while the old snapshot's CM write is still in flight.
		require.NoError(t, c.podsInformer.GetIndexer().Update(changed))
		require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), changed, changed.Namespace))
		return true, &corev1.PodList{Items: []corev1.Pod{*ready}}, nil
	})
	require.NoError(t, chain.OnPodReady(context.Background(), c.discoveryRequest(ms, ready)))
	record()
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	cm, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), "ms-0"+plugins.ConfigMapNameSuffix, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, `{"prefill":["10.0.0.2"]}`, cm.Data[plugins.DiscoveryFileName])
}

func TestDiscoveryReadyOnlyAndExcludedTargetsDoNotStartIndependentWork(t *testing.T) {
	for _, kind := range []string{"ready", "target", "role", "disabled"} {
		t.Run(kind, func(t *testing.T) {
			c, kube, ms, pod := discoveryControllerFixture(t)
			switch kind {
			case "ready":
				pod.Status.Conditions[0].Status = corev1.ConditionTrue
				require.NoError(t, c.podsInformer.GetIndexer().Update(pod))
			case "target":
				ms.Spec.Plugins[0].Scope = &api.PluginScope{Target: api.PluginTargetEntry}
			case "role":
				ms.Spec.Plugins[0].Scope = &api.PluginScope{Roles: []string{"other"}}
			case "disabled":
				ms.Spec.Plugins = nil
			}
			require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
			require.Empty(t, kube.Actions())
		})
	}
}

func TestDiscoveryFailureIsLocalToChangedScope(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	second := pod.DeepCopy()
	second.Name = "second"
	second.UID = "second"
	second.Labels[api.GroupNameLabelKey] = "ms-1"
	second.Annotations[groupInstanceAnnotation] = "second-instance"
	second.Status.PodIP = "10.0.0.2"
	require.NoError(t, c.podsInformer.GetIndexer().Add(second))
	require.NoError(t, kube.Tracker().Add(second))
	fail := true
	kube.PrependReactor("create", "configmaps", func(a kubetesting.Action) (bool, runtime.Object, error) {
		cm := a.(kubetesting.CreateAction).GetObject().(*corev1.ConfigMap)
		if fail && cm.Name == "ms-0"+plugins.ConfigMapNameSuffix {
			return true, nil, apierrors.NewServiceUnavailable("group 0 unavailable")
		}
		return false, nil, nil
	})
	require.Error(t, c.maintainPodDiscovery(context.Background(), ms))
	_, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), "ms-1"+plugins.ConfigMapNameSuffix, metav1.GetOptions{})
	require.NoError(t, err, "a failed scope cannot prevent another scope's output")
	fail = false
	kube.ClearActions()
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	lists := 0
	for _, a := range kube.Actions() {
		if a.Matches("list", "pods") {
			lists++
		}
	}
	require.Equal(t, 1, lists, "retry only the failed output, not every scope")
	require.NoError(t, c.podsInformer.GetIndexer().Delete(second))
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	require.Len(t, c.discovery.owners[ownerKey(ms)], 1, "retired scope fingerprint must be released")
}

func TestDiscoveryPhysicalDeletionChangesFingerprintAfterTermination(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	other := pod.DeepCopy()
	other.Name, other.UID, other.Status.PodIP = "other", "other", "10.0.0.2"
	require.NoError(t, c.podsInformer.GetIndexer().Add(other))
	require.NoError(t, kube.Tracker().Add(other))
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	terminating := other.DeepCopy()
	now := metav1.Now()
	terminating.DeletionTimestamp = &now
	require.NoError(t, c.podsInformer.GetIndexer().Update(terminating))
	require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), terminating, other.Namespace))
	// Existing Discovery includes all Running IPs, including Terminating. Its
	// input fingerprint must retain this member until it physically disappears.
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(terminating))
	require.NoError(t, kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), other.Namespace, other.Name))
	require.NoError(t, c.maintainPodDiscovery(context.Background(), ms))
	cm, err := kube.CoreV1().ConfigMaps(ms.Namespace).Get(context.Background(), "ms-0"+plugins.ConfigMapNameSuffix, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, `{"prefill":["10.0.0.1"]}`, cm.Data[plugins.DiscoveryFileName])
}

func TestDiscoveryFailureDoesNotBlockOrdinaryReplicaCreation(t *testing.T) {
	c, kube, ms, pod := discoveryControllerFixture(t)
	ms.Spec.Replicas = ptr.To[int32](2)
	require.NoError(t, c.modelServingsInformer.GetIndexer().Update(ms))
	_, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Update(context.Background(), ms, metav1.UpdateOptions{})
	require.NoError(t, err)
	// Only the Discovery helper's group-local list fails. The original full
	// reconciliation view and create APIs remain available.
	kube.PrependReactor("list", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
		selector := a.(kubetesting.ListAction).GetListRestrictions().Labels.String()
		if selector == api.GroupNameLabelKey+"=ms-0" {
			return true, nil, apierrors.NewServiceUnavailable("discovery-only list failure")
		}
		return false, nil, nil
	})
	c.publishPodChange(pod)
	err = c.syncModelServing(context.Background(), ms.Namespace+"/"+ms.Name)
	require.Error(t, err, "pending output must still schedule a retry")
	created := false
	for _, action := range kube.Actions() {
		if action.Matches("create", "pods") && action.(kubetesting.CreateAction).GetObject().(*corev1.Pod).Labels[api.GroupNameLabelKey] == "ms-1" {
			created = true
		}
	}
	require.True(t, created, "derived Discovery output failure must not newly block ordinary replica creation")
}
