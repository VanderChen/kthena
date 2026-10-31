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
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func patchContractPut(t *testing.T, c *ModelServingController, p *corev1.Pod) {
	t.Helper()
	_, err := c.kubeClientSet.CoreV1().Pods(p.Namespace).Update(context.Background(), p.DeepCopy(), metav1.UpdateOptions{})
	require.NoError(t, err)
	require.NoError(t, c.podsInformer.GetIndexer().Update(p.DeepCopy()))
}

// A current ambiguous POST receipt is existence evidence even when informer
// storage has already advanced past that Pod. It grants neither Ready credit
// nor permission to recover an unrelated request/owner/instance.
func TestPatchAmbiguousCreateCallbackReceipt(t *testing.T) {
	for _, scenario := range []string{"matching-with-empty-cache", "wrong-token", "wrong-owner", "wrong-instance", "retired-request", "new-physical-request"} {
		t.Run(scenario, func(t *testing.T) {
			ms := lifecycleMS("receipt", 1, 1, 0)
			ms.ResourceVersion = "1"
			c := lifecycleController(t, ms)
			role := ms.Spec.Template.Roles[0]
			pod := utils.GenerateEntryPod(role, ms, "receipt-0", "prefill-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
			pod.Annotations = map[string]string{groupInstanceAnnotation: "group-current", roleInstanceAnnotation: "role-current"}
			c.prepareMemberCreate(ms, pod)
			require.NotEmpty(t, pod.Annotations[creationTokenAnnotation])
			receipt := pod.DeepCopy()
			receipt.UID = "created-but-response-lost"
			receipt.ResourceVersion = "12"
			receipt.Status.Phase = corev1.PodPending
			switch scenario {
			case "wrong-token":
				receipt.Annotations[creationTokenAnnotation] = "another-request"
			case "wrong-owner":
				receipt.OwnerReferences[0].UID = "another-owner"
			case "wrong-instance":
				receipt.Annotations[roleInstanceAnnotation] = "another-instance"
			case "retired-request":
				c.forgetKnownMemberScope(ms, "receipt-0", "prefill", "prefill-0")
			case "new-physical-request":
				c.prepareMemberCreate(ms, pod)
				require.NotEqual(t, receipt.Annotations[creationTokenAnnotation], pod.Annotations[creationTokenAnnotation])
			}
			kube := c.kubeClientSet.(*kubefake.Clientset)
			kube.ClearActions()
			unlock := c.lifecycle.lock(ms.Namespace + "/" + ms.Name)
			done := make(chan struct{})
			go func() { c.updatePod(nil, receipt); close(done) }()
			select {
			case <-done:
				unlock()
			case <-time.After(time.Second):
				unlock()
				t.Fatal("Pod callback waited on the lifecycle lock")
			}
			require.Empty(t, kube.Actions(), "callback receipt uses no API")
			_, err := c.podsLister.Pods(ms.Namespace).Get(pod.Name)
			require.True(t, apierrors.IsNotFound(err), "the receipt must not rely on current cache contents")
			accepted := c.consumeMemberReceipts(ms)
			require.Equal(t, scenario == "matching-with-empty-cache", accepted)
			require.False(t, c.consumeMemberReceipts(ms), "receipt consumption is one-shot")
			roles, _ := c.store.GetRoleList(utils.GetNamespaceName(ms), "receipt-0", "prefill")
			for _, current := range roles {
				require.False(t, current.Initialized, "existence receipt must not grant Initialized")
				require.NotEqual(t, datastore.RoleRunning, current.Status, "existence receipt must not grant Ready budget")
			}
		})
	}
}

func TestPatchAmbiguousCreateUnknownBlocksNewPost(t *testing.T) {
	ms := lifecycleMS("unknown-create", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	role := ms.Spec.Template.Roles[0]
	pod := utils.GenerateEntryPod(role, ms, "unknown-create-0", "prefill-0", utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role))
	pod.Annotations = map[string]string{groupInstanceAnnotation: "group-current", roleInstanceAnnotation: "role-current"}
	c.prepareMemberCreate(ms, pod)
	token := pod.Annotations[creationTokenAnnotation]
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("ambiguous presence")
	})
	kube.ClearActions()
	require.ErrorContains(t, c.checkKnownMembersBeforeCreate(context.Background(), ms, pod), "ambiguous presence")
	for _, action := range kube.Actions() {
		require.True(t, action.Matches("get", "pods"), "unknown presence only probes; it must not mutate")
	}
	c.members.mu.Lock()
	expected := c.members.owners[memberOwner(ms)].creating[pod.Name]
	c.members.mu.Unlock()
	require.NotNil(t, expected)
	require.Equal(t, token, expected.token, "Unknown must retain the current POST expectation")
}
func patchContractSnapshot(t *testing.T, c *ModelServingController, ns string) map[string]string {
	t.Helper()
	list, err := c.kubeClientSet.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	out := map[string]string{}
	for _, p := range list.Items {
		out[p.Name] = string(p.UID)
	}
	return out
}
func patchContractRefreshCache(t *testing.T, c *ModelServingController, ns string) {
	t.Helper()
	for _, p := range c.podsInformer.GetIndexer().List() {
		require.NoError(t, c.podsInformer.GetIndexer().Delete(p))
	}
	list, err := c.kubeClientSet.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{})
	require.NoError(t, err)
	for i := range list.Items {
		require.NoError(t, c.podsInformer.GetIndexer().Add(list.Items[i].DeepCopy()))
	}
}
func patchContractDeletes(kube *kubefake.Clientset) []string {
	var names []string
	for _, a := range kube.Actions() {
		if a.Matches("delete", "pods") {
			names = append(names, a.(kubetesting.DeleteAction).GetName())
		}
	}
	sort.Strings(names)
	return names
}

func TestPatchCreatedMemberRecoveryScope(t *testing.T) {
	results := map[string]any{}
	for _, scenario := range []string{"online-complete", "online-partial-ready", "online-all-pending", "online-mixed-alreadyexists", "offline-delete", "same-revision-new-instance"} {
		for _, policy := range []api.RecoveryPolicy{api.NoneRestartPolicy, api.RoleRecreate, api.ServingGroupRecreate} {
			t.Run(scenario+"/"+string(policy), func(t *testing.T) {
				ctx := context.Background()
				ms := lifecycleMS("adopt-scope", 2, 1, 0)
				ms.ResourceVersion = "1"
				ms.Spec.RecoveryPolicy = policy
				ms.Spec.Template.Roles[0].WorkerReplicas = 1
				ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
				second := *ms.Spec.Template.Roles[0].DeepCopy()
				second.Name = "decode"
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
				c := lifecycleController(t, ms)
				c.podGroupManager = &fakePodGroupManager{}
				kube := c.kubeClientSet.(*kubefake.Clientset)
				targetCreated, injected := 0, false
				var actualExisting *corev1.Pod
				var attemptedToken string
				kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					if injected {
						return false, nil, nil
					}
					p := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
					if scenario == "online-mixed-alreadyexists" && p.Name == "adopt-scope-0-prefill-0-0" {
						// Another accepted request occupies the slot after the preceding
						// LIST. The attempted POST has no UID; only its AlreadyExists
						// verification GET can supply the actual object's identity.
						require.Empty(t, p.UID)
						attemptedToken = p.Annotations[creationTokenAnnotation]
						actualExisting = p.DeepCopy()
						actualExisting.UID = "actual-already-existing-entry"
						actualExisting.ResourceVersion = "9"
						actualExisting.Status.Phase = corev1.PodPending
						actualExisting.Annotations[creationTokenAnnotation] = "prior-accepted-request-token"
						require.NoError(t, kube.Tracker().Add(actualExisting.DeepCopy()))
						targetCreated++ // request attempt; the separately recorded actual CREATE count is one
						return true, nil, apierrors.NewAlreadyExists(corev1.Resource("pods"), p.Name)
					}
					p.UID, p.ResourceVersion = types.UID("original-"+p.Name), "10"
					p.Status.Phase = corev1.PodPending
					if p.Labels[api.GroupNameLabelKey] == "adopt-scope-0" && utils.GetRoleName(p) == "prefill" {
						targetCreated++
					}
					return false, nil, nil
				})
				kube.PrependReactor("list", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					if targetCreated == 2 && !injected {
						injected = true
						return true, nil, apierrors.NewServiceUnavailable("complete CREATE validation LIST failed once")
					}
					return false, nil, nil
				})
				// Create unaffected scopes first. The final Role has both physical
				// members before validation LIST503: normally two successful POSTs,
				// or an AlreadyExists verification GET plus one POST in the mixed case.
				for g := 1; g >= 0; g-- {
					for r := len(ms.Spec.Template.Roles) - 1; r >= 0; r-- {
						role := *ms.Spec.Template.Roles[r].DeepCopy()
						err := c.CreatePodsByRole(ctx, role, ms, 0, g, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role), "")
						if g == 0 && role.Name == "prefill" {
							require.ErrorContains(t, err, "complete CREATE validation LIST failed once")
						} else {
							require.NoError(t, err)
						}
					}
				}
				require.True(t, injected)
				require.Equal(t, 2, targetCreated)
				realTargetCreates := targetCreated
				if actualExisting != nil {
					realTargetCreates--
					c.members.mu.Lock()
					recorded := c.members.owners[memberOwner(ms)].current[actualExisting.Name].DeepCopy()
					c.members.mu.Unlock()
					require.Equal(t, actualExisting.UID, recorded.UID)
					require.Equal(t, actualExisting.OwnerReferences, recorded.OwnerReferences)
					require.Equal(t, actualExisting.Annotations[roleInstanceAnnotation], recorded.Annotations[roleInstanceAnnotation])
					require.Equal(t, actualExisting.Annotations[creationTokenAnnotation], recorded.Annotations[creationTokenAnnotation])
					require.NotEqual(t, attemptedToken, recorded.Annotations[creationTokenAnnotation], "record the GET result, not the attempted POST's token")
				}
				livePods, err := kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
				require.NoError(t, err)
				require.Len(t, livePods.Items, 8)
				pods := []*corev1.Pod{}
				for i := range livePods.Items {
					p := livePods.Items[i].DeepCopy()
					pods = append(pods, p)
					require.NoError(t, c.podsInformer.GetIndexer().Add(p.DeepCopy()))
				}
				kube.ClearActions()
				key := ms.Namespace + "/" + ms.Name
				// The complete case offers a Ready callback window; partial and
				// all-Pending cases prove recovery is based on physical existence.
				for _, pod := range pods {
					pod.ResourceVersion = "20"
					pod.Status = corev1.PodStatus{Phase: corev1.PodRunning, Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
					if scenario == "online-partial-ready" && pod.Name == "adopt-scope-0-prefill-0-1" {
						pod.Status.Conditions[0].Status = corev1.ConditionFalse
					}
					if scenario == "online-all-pending" {
						pod.Status.Phase = corev1.PodPending
						pod.Status.Conditions[0].Status = corev1.ConditionFalse
					}
					patchContractPut(t, c, pod)
				}
				for _, pod := range pods {
					c.updatePod(nil, pod.DeepCopy())
				}
				for _, pod := range pods {
					pod.ResourceVersion = "30"
					pod.Status.Conditions[0].Status = corev1.ConditionFalse
					patchContractPut(t, c, pod)
				}
				for _, pod := range pods {
					c.updatePod(nil, pod.DeepCopy())
				}
				roles, _ := c.store.GetRoleList(utils.GetNamespaceName(ms), "adopt-scope-0", "prefill")
				initialized := len(roles) > 0 && roles[0].Initialized
				if len(roles) > 0 {
					require.Equal(t, datastore.RoleCreating, roles[0].Status, "current NotReady never receives Ready budget")
				}
				for _, action := range kube.Actions() {
					require.False(t, action.Matches("create", "pods"), "Ready window itself must not include additional Pod CREATE")
				}
				if scenario == "same-revision-new-instance" {
					// A whole Role incarnation changes while template revision/hash stay
					// identical. Old observed completeness must not authorize this one.
					for _, pod := range pods {
						if pod.Labels[api.GroupNameLabelKey] != "adopt-scope-0" || utils.GetRoleName(pod) != "prefill" {
							continue
						}
						old := pod.DeepCopy()
						require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(ctx, old.Name, metav1.DeleteOptions{}))
						require.NoError(t, c.podsInformer.GetIndexer().Delete(old))
						pod.UID = types.UID("new-incarnation-" + pod.Name)
						pod.Annotations[roleInstanceAnnotation] = "new-role-instance"
						pod.ResourceVersion = "40"
						_, err := kube.CoreV1().Pods(ms.Namespace).Create(ctx, pod.DeepCopy(), metav1.CreateOptions{})
						require.NoError(t, err)
						require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
						c.updatePod(nil, pod.DeepCopy())
					}
				}
				before := patchContractSnapshot(t, c, ms.Namespace)
				missingName := "adopt-scope-0-prefill-0-1"
				if scenario == "online-mixed-alreadyexists" {
					missingName = actualExisting.Name
				}
				var deleted *corev1.Pod
				for _, pod := range pods {
					if pod.Name == missingName {
						deleted = pod.DeepCopy()
					}
				}
				require.NotNil(t, deleted)
				require.NoError(t, kube.CoreV1().Pods(ms.Namespace).Delete(ctx, deleted.Name, metav1.DeleteOptions{}))
				require.NoError(t, c.podsInformer.GetIndexer().Delete(deleted))
				// The cache can lead a listener callback. No DELETE callback is invoked
				// until after a queued MS reconcile has acted on physical absence.
				if scenario == "offline-delete" {
					// The old process is gone before the loss can be delivered. Use
					// a fresh controller/store and the real startup syncAll path.
					c.workqueue.ShutDown()
					survivors, err := kube.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{})
					require.NoError(t, err)
					c = lifecycleController(t, ms)
					c.podGroupManager = &fakePodGroupManager{}
					kube = c.kubeClientSet.(*kubefake.Clientset)
					for i := range survivors.Items {
						pod := survivors.Items[i].DeepCopy()
						require.NoError(t, kube.Tracker().Add(pod.DeepCopy()))
						require.NoError(t, c.podsInformer.GetIndexer().Add(pod))
					}
					c.initialSync.Store(false)
					c.syncAll()
				}
				serial := 0
				kube.PrependReactor("create", "pods", func(a kubetesting.Action) (bool, runtime.Object, error) {
					p := a.(kubetesting.CreateAction).GetObject().(*corev1.Pod)
					if p.UID == "" {
						serial++
						p.UID = types.UID(fmt.Sprintf("replacement-%d", serial))
						p.ResourceVersion = fmt.Sprint(100 + serial)
						p.Status.Phase = corev1.PodPending
					}
					return false, nil, nil
				})
				kube.ClearActions()
				c.enqueueModelServing(ms)
				require.True(t, c.processNextWorkItem(ctx))
				firstDeletes := patchContractDeletes(kube)
				first := patchContractSnapshot(t, c, ms.Namespace)
				// Deliver updated informer storage between worker rounds. This models
				// real API results without inventing Ready for newly created members.
				for i := 0; i < 4; i++ {
					patchContractRefreshCache(t, c, ms.Namespace)
					require.NoError(t, c.syncModelServing(ctx, key))
				}
				settled := patchContractSnapshot(t, c, ms.Namespace)
				require.Len(t, settled, len(before), "recovery must refill the physical layout")
				require.NotEqual(t, before[deleted.Name], settled[deleted.Name])
				changed := []string{}
				for name, uid := range before {
					if settled[name] != uid {
						changed = append(changed, name)
					}
				}
				sort.Strings(changed)
				kube.ClearActions()
				patchContractRefreshCache(t, c, ms.Namespace)
				if scenario != "offline-delete" {
					c.deletePod(deleted)
				}
				for i := 0; i < 3; i++ {
					require.NoError(t, c.syncModelServing(ctx, key))
				}
				lateDeletes := patchContractDeletes(kube)
				afterLate := patchContractSnapshot(t, c, ms.Namespace)
				require.Empty(t, lateDeletes, "late old-UID DELETE must not evict replacement or expand recovery")
				require.Equal(t, settled, afterLate)
				row := map[string]any{"realTargetCreatesBefore503": realTargetCreates, "actualAlreadyExistsObserved": actualExisting != nil, "validation503Injected": injected, "initializedAfterReadyWindow": initialized, "firstWorkerDeletes": firstDeletes, "firstWorkerPhysicalUIDs": first, "finalChangedNames": changed, "lateDeleteActions": lateDeletes, "replacementUIDsPreservedAfterLateDelete": true, "before": before, "settled": settled}
				results[scenario+"/"+string(policy)] = row
				encoded, _ := json.Marshal(row)
				t.Logf("OBSERVATION %s/%s %s", scenario, policy, encoded)
				expectedChanged := 1
				if scenario == "online-complete" || scenario == "online-partial-ready" || scenario == "online-all-pending" || scenario == "online-mixed-alreadyexists" {
					switch policy {
					case api.RoleRecreate:
						expectedChanged = 2
					case api.ServingGroupRecreate:
						expectedChanged = 4
					}
				}
				require.Len(t, changed, expectedChanged, "confirmed online physical loss retains configured scope even before complete Ready; offline and replaced incarnations do not inherit the old scope")
				for name, uid := range before {
					if len(name) >= len("adopt-scope-1-") && name[:len("adopt-scope-1-")] == "adopt-scope-1-" {
						require.Equal(t, uid, settled[name], "unrelated SG preserved")
					}
				}
			})
		}
	}
	if output := os.Getenv("PATCH_FINAL_RECOVERY_OUTPUT"); output != "" {
		data, err := json.MarshalIndent(results, "", "  ")
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(output, append(data, '\n'), 0600))
	}
}
