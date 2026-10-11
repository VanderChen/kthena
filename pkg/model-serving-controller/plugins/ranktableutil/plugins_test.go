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

package ranktableutil_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	listerv1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	podranktable "github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins/pod-ranktable"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins/ranktable"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins/ranktableutil"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

func TestBothPluginsMaintainAppliedMembers(t *testing.T) {
	for _, podIP := range []bool{false, true} {
		for _, group := range []bool{false, true} {
			t.Run(fmt.Sprintf("podIP=%v/group=%v", podIP, group), func(t *testing.T) {
				t.Setenv("POD_NAMESPACE", "templates")
				ctx := context.Background()
				role := workloadv1alpha1.Role{Name: "worker", Replicas: ptr.To[int32](1), WorkerReplicas: 1,
					EntryTemplate:  workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "a"}}}},
					WorkerTemplate: &workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "main", Image: "a"}}}},
				}
				ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: "model", Namespace: "test", UID: "owner"},
					Spec: workloadv1alpha1.ModelServingSpec{Template: workloadv1alpha1.ServingGroup{Roles: []workloadv1alpha1.Role{role}}}}
				client := fake.NewSimpleClientset()
				_, err := utils.CreateControllerRevision(ctx, client, ms, "history", []workloadv1alpha1.Role{role})
				require.NoError(t, err)
				// The latest spec is deliberately incompatible with applied W1.
				ms.Spec.Template.Roles[0].WorkerReplicas = 2
				level := "role"
				if group {
					level = "group"
				}
				index := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
				require.NoError(t, index.Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "template", Namespace: "templates"}, Data: map[string]string{
					"ranktable-level": level, "pod-parser-template": "parser", "filename": "ranktable.json", "mount-path": "/ranktable",
					"ranktable-template": `{"status":"{{ .Status }}","count":{{ .ServerCount }},"timestamp":"{{ .Timestamp }}","servers":{{ .Servers | toJson }}}`,
				}}))
				require.NoError(t, index.Add(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "parser", Namespace: "templates"}, Data: map[string]string{
					"annotation-name": "test/ranktable", "parser-template": "{{ $d := . | fromJson }}\nserverId: {{ $d.server | quote }}\ndevices: []",
				}}))
				completed, initializing := ranktable.RanktableStatusCompleted, ranktable.RanktableStatusInitializing
				factory := ranktable.NewRanktablePlugin
				if podIP {
					factory = podranktable.NewPodRanktablePlugin
					completed, initializing = podranktable.RanktableStatusCompleted, podranktable.RanktableStatusInitializing
				}
				plugin, err := factory(workloadv1alpha1.PluginSpec{Config: &apiextv1.JSON{Raw: []byte(`{"template":"template"}`)}})
				require.NoError(t, err)
				req := &plugins.HookRequest{ModelServing: ms, ServingGroup: "model-0", RoleName: "worker", RoleID: "worker-0", Role: role.DeepCopy(), KubeClient: client, ConfigMapLister: listerv1.NewConfigMapLister(index)}
				pods := []*corev1.Pod{
					utils.GenerateEntryPod(*role.DeepCopy(), ms, req.ServingGroup, req.RoleID, "history", utils.CalRoleTemplateHash(role)),
					utils.GenerateWorkerPod(*role.DeepCopy(), ms, req.ServingGroup, req.RoleID, 1, "history", utils.CalRoleTemplateHash(role)),
				}
				for i, pod := range pods {
					pod.UID = types.UID(fmt.Sprintf("pod-%d", i))
					pod.Annotations = map[string]string{ranktableutil.GroupInstance: "group-a", ranktableutil.RoleInstance: "role-a", "test/ranktable": `{"server":"shared-server"}`}
					pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, PodIP: fmt.Sprintf("192.0.2.%d", i+1), Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}
					req.Pod = pod
					require.NoError(t, plugin.OnPodCreate(ctx, req))
					require.Equal(t, "/ranktable", pod.Spec.Containers[0].VolumeMounts[0].MountPath)
					_, err := client.CoreV1().Pods(ms.Namespace).Create(ctx, pod, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				scope := ranktableutil.NewScope(req, !group, "ranktable.json")
				read := func() (*corev1.ConfigMap, string, int) {
					cm, err := client.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, scope.Name, metav1.GetOptions{})
					require.NoError(t, err)
					var data struct {
						Status string
						Count  int
					}
					require.NoError(t, json.Unmarshal([]byte(cm.Data[scope.Filename]), &data))
					return cm, data.Status, data.Count
				}
				require.NoError(t, plugin.OnPodRunning(ctx, req))
				cm, status, count := read()
				require.Equal(t, completed, status) // Running/NotReady still publishes.
				expected := 1
				if podIP {
					expected = 2
				}
				require.Equal(t, expected, count) // Annotation IDs aggregate; IP IDs differ.
				req.Pod = nil
				client.ClearActions()
				require.NoError(t, plugin.OnRoleSync(ctx, req))
				require.NoError(t, plugin.OnRoleSync(ctx, req))
				for _, action := range client.Actions() {
					if action.GetResource().Resource == "configmaps" {
						require.NotEqual(t, "update", action.GetVerb())
					}
				}
				// Parser failures and missing IP block publication with no false count.
				worker := pods[1].DeepCopy()
				worker.Annotations["test/ranktable"] = "invalid-json"
				_, err = client.CoreV1().Pods(ms.Namespace).Update(ctx, worker, metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, plugin.OnRoleSync(ctx, req))
				_, status, count = read()
				require.Equal(t, initializing, status)
				require.Zero(t, count)
				worker.Annotations["test/ranktable"] = pods[1].Annotations["test/ranktable"]
				if podIP {
					worker.Status.PodIP = ""
				}
				_, err = client.CoreV1().Pods(ms.Namespace).Update(ctx, worker, metav1.UpdateOptions{})
				require.NoError(t, err)
				if podIP {
					require.NoError(t, plugin.OnRoleSync(ctx, req))
					_, status, count = read()
					require.Equal(t, initializing, status)
					require.Zero(t, count)
				}
				_, err = client.CoreV1().Pods(ms.Namespace).Update(ctx, pods[1], metav1.UpdateOptions{})
				require.NoError(t, err)
				require.NoError(t, client.CoreV1().ConfigMaps(ms.Namespace).Delete(ctx, cm.Name, metav1.DeleteOptions{}))
				require.NoError(t, plugin.OnRoleSync(ctx, req))
				_, status, count = read()
				require.Equal(t, completed, status)
				require.Equal(t, expected, count)
				require.NoError(t, plugin.OnServingGroupDelete(ctx, req))
				for _, pod := range pods {
					require.NoError(t, client.CoreV1().Pods(ms.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}))
					req.Pod = pod
					require.NoError(t, plugin.OnPodDelete(ctx, req))
				}
				list, err := client.CoreV1().ConfigMaps(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Empty(t, list.Items)
			})
		}
	}
}
