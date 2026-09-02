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

package ranktable

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

func TestOnPodRunningGeneratesRanktableBeforePodReady(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "default")

	pod := ranktableTestPod("test-ms-0", "worker", "worker-0", "pod-0")
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}
	pod.Annotations = map[string]string{PodRanktableAnnotation: `{}`}

	plugin, req, client := newRanktablePluginTest(t, RoleLevelRanktable, []*corev1.Pod{pod})
	req.Pod = pod
	require.NoError(t, plugin.OnPodRunning(context.Background(), req))

	cmName := GenerateRanktableConfigMapName(req.ModelServing.Name, req.ServingGroup+"-"+req.RoleID)
	cm, err := client.CoreV1().ConfigMaps(req.ModelServing.Namespace).Get(context.Background(), cmName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"Completed","servers":1}`, cm.Data["ranktable.json"])
}

func TestOnPodDeleteResetsRanktableWhenRoleBecomesIncomplete(t *testing.T) {
	t.Setenv("POD_NAMESPACE", "default")

	plugin, req, client := newRanktablePluginTest(t, RoleLevelRanktable, nil)
	require.NoError(t, plugin.OnPodDelete(context.Background(), req))

	cmName := GenerateRanktableConfigMapName(req.ModelServing.Name, req.ServingGroup+"-"+req.RoleID)
	cm, err := client.CoreV1().ConfigMaps(req.ModelServing.Namespace).Get(context.Background(), cmName, metav1.GetOptions{})
	require.NoError(t, err)
	assert.JSONEq(t, `{"status":"Initializing","servers":0}`, cm.Data["ranktable.json"])
}

func newRanktablePluginTest(
	t *testing.T,
	level RanktableLevel,
	pods []*corev1.Pod,
) (*RanktablePlugin, *plugins.HookRequest, *fake.Clientset) {
	t.Helper()

	parserCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-parser", Namespace: "default"},
		Data: map[string]string{
			"annotation-name": PodRanktableAnnotation,
			"parser-template": `podName: pod-0
serverId: server-0
devices:
- deviceId: "0"
  deviceIp: "192.0.2.1"`,
		},
	}
	templateCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "test-template", Namespace: "default"},
		Data: map[string]string{
			"inference-engine":    "mindie",
			"ranktable-level":     string(level),
			"pod-parser-template": parserCM.Name,
			"ranktable-template":  `{"status":"{{ .Status }}","servers":{{ .ServerCount }}}`,
			"mount-path":          "/etc/ranktable",
			"filename":            "ranktable.json",
		},
	}
	configMapIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, configMapIndexer.Add(parserCM))
	require.NoError(t, configMapIndexer.Add(templateCM))
	podIndexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	for _, pod := range pods {
		require.NoError(t, podIndexer.Add(pod))
	}

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: "test-ms", Namespace: "default", UID: types.UID("test-ms-uid")},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas: ptr.To[int32](1),
			Template: workloadv1alpha1.ServingGroup{
				Roles: []workloadv1alpha1.Role{{Name: "worker", Replicas: ptr.To[int32](1)}},
			},
		},
	}
	client := fake.NewSimpleClientset()
	return &RanktablePlugin{
		cfg:             RanktableConfig{Template: templateCM.Name},
		templateManager: NewTemplateManager(),
	}, &plugins.HookRequest{
		ModelServing:    ms,
		ServingGroup:    "test-ms-0",
		RoleName:        "worker",
		RoleID:          "worker-0",
		PodLister:       listerv1.NewPodLister(podIndexer),
		ConfigMapLister: listerv1.NewConfigMapLister(configMapIndexer),
		KubeClient:      client,
	}, client
}

func ranktableTestPod(groupName, roleName, roleID, name string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: "default",
		Labels: map[string]string{
			workloadv1alpha1.ModelServingNameLabelKey: "test-ms",
			workloadv1alpha1.GroupNameLabelKey:        groupName,
			workloadv1alpha1.RoleLabelKey:             roleName,
			workloadv1alpha1.RoleIDKey:                roleID,
		},
	}}
}
