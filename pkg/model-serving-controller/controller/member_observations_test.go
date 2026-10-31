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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubetesting "k8s.io/client-go/testing"
)

func memberTestFixture(t *testing.T, policy api.RecoveryPolicy) (*ModelServingController, *api.ModelServing, []*corev1.Pod) {
	t.Helper()
	ms := lifecycleMS("member-observation", 1, 1, 0)
	ms.ResourceVersion, ms.Spec.RecoveryPolicy = "1", policy
	ms.Spec.Template.Roles[0].WorkerReplicas = 1
	ms.Spec.Template.Roles[0].WorkerTemplate = ms.Spec.Template.Roles[0].EntryTemplate.DeepCopy()
	other := *ms.Spec.Template.Roles[0].DeepCopy()
	other.Name = "other"
	ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, other)
	c := lifecycleController(t, ms)
	group, revision := ms.Name+"-0", utils.ModelServingRevision(ms)
	var pods []*corev1.Pod
	for _, role := range ms.Spec.Template.Roles {
		id := role.Name + "-0"
		for ordinal := 0; ordinal <= int(role.WorkerReplicas); ordinal++ {
			pod := utils.GenerateEntryPod(role, ms, group, id, revision, utils.CalRoleTemplateHash(role))
			if ordinal > 0 {
				pod = utils.GenerateWorkerPod(role, ms, group, id, ordinal, revision, utils.CalRoleTemplateHash(role))
			}
			pod.UID, pod.ResourceVersion = types.UID(pod.Name+"-uid"), "10"
			setInstanceAnnotations(pod, map[string]string{groupInstanceAnnotation: "group-original", roleInstanceAnnotation: role.Name + "-original"})
			_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Create(context.Background(), pod, metav1.CreateOptions{})
			require.NoError(t, err)
			require.NoError(t, c.podsInformer.GetIndexer().Add(pod.DeepCopy()))
			pods = append(pods, pod)
		}
		c.store.AddServingGroupAndRole(utils.GetNamespaceName(ms), group, revision, utils.CalRoleTemplateHash(role), role.Name, id)
	}
	return c, ms, pods
}

func memberTestSnapshot(t *testing.T, c *ModelServingController, ms *api.ModelServing) context.Context {
	t.Helper()
	ctx, err := c.withRolloutPodSnapshot(context.Background(), ms)
	require.NoError(t, err)
	return ctx
}

func memberTestRemove(t *testing.T, c *ModelServingController, pod *corev1.Pod) {
	t.Helper()
	require.NoError(t, c.kubeClientSet.CoreV1().Pods(pod.Namespace).Delete(context.Background(), pod.Name, metav1.DeleteOptions{}))
	require.NoError(t, c.podsInformer.GetIndexer().Delete(pod))
}

func TestMemberObservationReceiptIdentityAndRetirement(t *testing.T) {
	for _, change := range []string{"matching", "token", "owner", "group-instance", "role-instance", "revision", "layout", "legacy", "retired", "new-authoritative-uid"} {
		t.Run(change, func(t *testing.T) {
			c, ms, pods := memberTestFixture(t, api.RoleRecreate)
			intended := pods[0].DeepCopy()
			intended.UID = ""
			require.NoError(t, c.prepareMemberCreate(ms, intended))
			receipt := intended.DeepCopy()
			receipt.UID = "actual-uid"
			switch change {
			case "token":
				receipt.Annotations[creationTokenAnnotation] = "old-token"
			case "owner":
				receipt.OwnerReferences[0].UID = "old-owner"
			case "group-instance":
				receipt.Annotations[groupInstanceAnnotation] = "old-group"
			case "role-instance":
				receipt.Annotations[roleInstanceAnnotation] = "old-role"
			case "revision":
				receipt.Labels[api.RevisionLabelKey] = "old-revision"
			case "layout":
				receipt.Labels[api.RoleTemplateHashLabelKey] = "old-layout"
			case "legacy":
				delete(receipt.Annotations, creationTokenAnnotation)
			case "retired":
				c.forgetKnownMemberScope(ms, ms.Name+"-0", "prefill", "prefill-0")
				require.NoError(t, c.prepareMemberCreate(ms, intended))
			case "new-authoritative-uid":
				current := receipt.DeepCopy()
				current.UID = "replacement"
				c.observeKnownMember(ms, current)
			}
			// The payload remains admissible even after informer storage advanced
			// to absence; only the still-current POST token supplies authority.
			require.NoError(t, c.podsInformer.GetIndexer().Delete(pods[0]))
			for i := 0; i < 1000; i++ {
				c.observeCreatedPodReceipt(receipt)
			}
			require.Equal(t, change == "matching", c.consumeMemberReceipts(ms))
			state := c.members.owners[memberOwner(ms)]
			require.LessOrEqual(t, len(state.current), 1)
			require.LessOrEqual(t, len(state.creating), 1)
			if change == "matching" {
				require.Equal(t, receipt.UID, state.current[receipt.Name].UID)
			}
			if change == "new-authoritative-uid" {
				require.Equal(t, types.UID("replacement"), state.current[receipt.Name].UID)
			}
			roles, err := c.store.GetRoleList(utils.GetNamespaceName(ms), ms.Name+"-0", "prefill")
			require.NoError(t, err)
			require.False(t, roles[0].Initialized, "existence is not full-layout completion")
			require.Equal(t, datastore.RoleCreating, roles[0].Status)
		})
	}
}

func TestMemberObservationAmbiguousCreateReceiptDuringGet(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	intended := pods[0].DeepCopy()
	intended.UID = ""
	require.NoError(t, c.prepareMemberCreate(ms, intended))
	receipt := intended.DeepCopy()
	receipt.UID = "post-succeeded-response-lost"
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("get", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		c.observeCreatedPodReceipt(receipt)
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, action.(kubetesting.GetAction).GetName())
	})
	require.ErrorIs(t, c.checkKnownMembersBeforeCreate(context.Background(), ms, intended), errMemberObservationChanged)
	require.Equal(t, receipt.UID, c.members.owners[memberOwner(ms)].current[receipt.Name].UID)
	require.Empty(t, c.members.owners[memberOwner(ms)].creating)
}

func TestMemberObservationCreateFenceUnknownAndLateReceipt(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	intended := pods[0].DeepCopy()
	intended.UID = ""
	require.NoError(t, c.prepareMemberCreate(ms, intended))
	token := intended.Annotations[creationTokenAnnotation]
	kube := c.kubeClientSet.(*kubefake.Clientset)
	kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewServiceUnavailable("GET unknown")
	})
	require.Error(t, c.checkKnownMembersBeforeCreate(context.Background(), ms, intended))
	require.Equal(t, token, c.members.owners[memberOwner(ms)].creating[intended.Name].token)
	receipt := intended.DeepCopy()
	receipt.UID = "online-uid"
	c.observeCreatedPodReceipt(receipt)
	// A receipt between the earlier check and the final POST preparation must
	// abort the new POST instead of rotating away its only existence witness.
	require.ErrorIs(t, c.prepareMemberCreate(ms, intended), errMemberObservationChanged)
	require.Equal(t, token, c.members.owners[memberOwner(ms)].creating[intended.Name].token)
}

func TestMemberObservationMissingRecoversWithoutReady(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate, api.NoneRestartPolicy} {
		for _, evidence := range []string{"online", "offline", "unknown", "replacement", "new-instance"} {
			t.Run(fmt.Sprintf("%s/%s", policy, evidence), func(t *testing.T) {
				c, ms, pods := memberTestFixture(t, policy)
				if evidence != "offline" {
					for i := 0; i < 3; i++ {
						handled, err := c.processKnownMemberLosses(memberTestSnapshot(t, c, ms), ms)
						require.NoError(t, err)
						require.False(t, handled)
					}
				}
				memberTestRemove(t, c, pods[0])
				kube := c.kubeClientSet.(*kubefake.Clientset)
				if evidence == "replacement" {
					replacement := pods[0].DeepCopy()
					replacement.UID = "new-uid"
					_, err := kube.CoreV1().Pods(ms.Namespace).Create(context.Background(), replacement, metav1.CreateOptions{})
					require.NoError(t, err)
				}
				if evidence == "new-instance" {
					sibling := pods[1].DeepCopy()
					sibling.Annotations[roleInstanceAnnotation] = "new-incarnation"
					_, err := kube.CoreV1().Pods(ms.Namespace).Update(context.Background(), sibling, metav1.UpdateOptions{})
					require.NoError(t, err)
				}
				if evidence == "unknown" {
					kube.PrependReactor("get", "pods", func(kubetesting.Action) (bool, runtime.Object, error) {
						return true, nil, apierrors.NewServiceUnavailable("unknown")
					})
				}
				ctx := memberTestSnapshot(t, c, ms)
				kube.ClearActions()
				handled, err := c.processKnownMemberLosses(ctx, ms)
				if evidence == "unknown" && policy != api.NoneRestartPolicy {
					require.Error(t, err)
				} else {
					require.NoError(t, err)
				}
				wantRecovery := evidence == "online" && policy != api.NoneRestartPolicy
				require.Equal(t, wantRecovery, handled)
				for _, pod := range pods[1:] {
					wantDelete := wantRecovery && (policy == api.ServingGroupRecreate || utils.GetRoleName(pod) == "prefill")
					require.Equal(t, wantDelete, lifecycleDeletionMatches(c, pod), pod.Name)
				}
			})
		}
	}
}

func TestMemberObservationRecoveryFreezesUIDsBefore404(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	handled, err := c.processKnownMemberLosses(memberTestSnapshot(t, c, ms), ms)
	require.NoError(t, err)
	require.False(t, handled)
	memberTestRemove(t, c, pods[0])
	ctx := memberTestSnapshot(t, c, ms)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	replaced := false
	kube.PrependReactor("get", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		if action.(kubetesting.GetAction).GetName() != pods[0].Name || replaced {
			return false, nil, nil
		}
		replaced = true
		replacement := pods[1].DeepCopy()
		replacement.UID = "later-new-uid"
		require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pods[0].Name)
	})
	kube.ClearActions()
	handled, err = c.processKnownMemberLosses(ctx, ms)
	require.NoError(t, err)
	require.True(t, handled)
	require.False(t, lifecycleDeletionMatches(c, pods[1]), "later replacement must not be selected by a post-404 LIST")
	current, err := kube.CoreV1().Pods(ms.Namespace).Get(context.Background(), pods[1].Name, metav1.GetOptions{})
	require.NoError(t, err)
	require.Equal(t, types.UID("later-new-uid"), current.UID)
}

func TestMemberObservationEntireUnregisteredScopeRetiresBeforeCreate(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.RoleRecreate, api.ServingGroupRecreate} {
		t.Run(string(policy), func(t *testing.T) {
			c, ms, pods := memberTestFixture(t, policy)
			for _, pod := range pods {
				c.observeKnownMember(ms, pod)
				if policy == api.ServingGroupRecreate || utils.GetRoleName(pod) == "prefill" {
					memberTestRemove(t, c, pod)
				}
			}
			c.store.DeleteRole(utils.GetNamespaceName(ms), ms.Name+"-0", "prefill", "prefill-0")
			if policy == api.ServingGroupRecreate {
				c.store.DeleteServingGroup(utils.GetNamespaceName(ms), ms.Name+"-0")
			}
			handled, err := c.processKnownMemberLosses(memberTestSnapshot(t, c, ms), ms)
			require.NoError(t, err)
			require.True(t, handled, "caller must stop before choosing a fresh template")
			require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), ms.Name+"-0", "prefill", "prefill-0"))
			for _, pod := range c.members.owners[memberOwner(ms)].current {
				require.NotEqual(t, "prefill", utils.GetRoleName(pod))
			}
		})
	}
}

func TestMemberObservationRealCreateResponseLost(t *testing.T) {
	ms := lifecycleMS("lost-response", 1, 1, 0)
	ms.ResourceVersion = "1"
	c := lifecycleController(t, ms)
	kube := c.kubeClientSet.(*kubefake.Clientset)
	var created *corev1.Pod
	kube.PrependReactor("create", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
		created = action.(kubetesting.CreateAction).GetObject().(*corev1.Pod).DeepCopy()
		created.UID = "server-assigned-uid"
		require.NotEmpty(t, created.Annotations[creationTokenAnnotation])
		c.observeCreatedPodReceipt(created)
		// The server accepted POST, notification was delivered, and an external
		// deletion completed before the client recovered its response.
		return true, nil, apierrors.NewTimeoutError("response lost", 0)
	})
	role := ms.Spec.Template.Roles[0]
	revision := utils.ModelServingRevision(ms)
	require.Error(t, c.CreatePodsByRole(context.Background(), role, ms, 0, 0, utils.ModelServingRevision(ms), utils.CalRoleTemplateHash(role), ""))
	require.NotNil(t, created)
	matches, err := c.revisionHistory(context.Background(), ms).podMatchesTemplate(context.Background(), created, role, revision)
	require.NoError(t, err)
	require.True(t, matches, "runtime token does not change template identity")
	withoutToken := created.DeepCopy()
	delete(withoutToken.Annotations, creationTokenAnnotation)
	withoutTokenMatches, err := c.revisionHistory(context.Background(), ms).podMatchesTemplate(context.Background(), withoutToken, role, revision)
	require.NoError(t, err)
	require.Equal(t, matches, withoutTokenMatches)
	require.True(t, c.consumeMemberReceipts(ms))
	require.Equal(t, created.UID, c.members.owners[memberOwner(ms)].current[created.Name].UID)
	handled, err := c.processKnownMemberLosses(memberTestSnapshot(t, c, ms), ms)
	require.NoError(t, err)
	require.True(t, handled)
	require.Equal(t, datastore.RoleNotFound, c.store.GetRoleStatus(utils.GetNamespaceName(ms), ms.Name+"-0", role.Name, role.Name+"-0"))
}

func TestMemberObservationRealDeleteUnknownAndFrozenSelection(t *testing.T) {
	for _, mode := range []string{"unknown", "replacement-after-404"} {
		t.Run(mode, func(t *testing.T) {
			c, ms, pods := memberTestFixture(t, api.RoleRecreate)
			memberTestRemove(t, c, pods[0])
			kube := c.kubeClientSet.(*kubefake.Clientset)
			injected := false
			kube.PrependReactor("get", "pods", func(action kubetesting.Action) (bool, runtime.Object, error) {
				if action.(kubetesting.GetAction).GetName() != pods[0].Name || injected {
					return false, nil, nil
				}
				injected = true
				if mode == "unknown" {
					return true, nil, apierrors.NewServiceUnavailable("physical loss is not confirmed")
				}
				replacement := pods[1].DeepCopy()
				replacement.UID = "new-after-404"
				require.NoError(t, kube.Tracker().Update(corev1.SchemeGroupVersion.WithResource("pods"), replacement, ms.Namespace))
				return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pods[0].Name)
			})
			stale, siblings, err := c.deletionObservation(context.Background(), ms, pods[0])
			if mode == "unknown" {
				require.Error(t, err)
				require.False(t, stale)
				return
			}
			require.NoError(t, err)
			require.False(t, stale)
			ctx := withObservedPodDeletion(context.Background(), ms, pods[0], siblings)
			kube.ClearActions()
			require.NoError(t, c.DeleteRole(ctx, ms, ms.Name+"-0", "prefill", "prefill-0"))
			require.False(t, lifecycleDeletionMatches(c, pods[1]))
		})
	}
}

func TestMemberObservationReceiptRetirementConcurrent(t *testing.T) {
	c, ms, pods := memberTestFixture(t, api.RoleRecreate)
	intended := pods[0].DeepCopy()
	intended.UID = ""
	require.NoError(t, c.prepareMemberCreate(ms, intended))
	receipt := intended.DeepCopy()
	receipt.UID = "retired-online-uid"
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			c.observeCreatedPodReceipt(receipt)
		}
	}()
	go func() { defer wg.Done(); c.forgetKnownMemberOwner(utils.GetNamespaceName(ms), "") }()
	wg.Wait()
	require.False(t, c.consumeMemberReceipts(ms))
	require.Empty(t, c.members.owners)
}
