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

package controller_manager

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	controllerutils "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/test/e2e/utils"
)

const (
	// servingGroupTimeout bounds a wait for the ServingGroups of a ModelServing to settle.
	servingGroupTimeout = 2 * time.Minute

	controllerManagerDeployment = "kthena-controller-manager"
	controllerManagerSelector   = "app.kubernetes.io/component=kthena-controller-manager"
	// controllerManagerLease is the leader election Lease of the controller manager.
	controllerManagerLease = "lease.kthena.controller-manager"
)

// servingGroupsOf returns the ServingGroups ms controls, by name.
func servingGroupsOf(ctx context.Context, kthenaClient *clientset.Clientset, ms *workload.ModelServing) (map[string]workload.ServingGroup, error) {
	list, err := kthenaClient.WorkloadV1alpha1().ServingGroups(ms.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	groups := map[string]workload.ServingGroup{}
	for _, group := range list.Items {
		if metav1.IsControlledBy(&group, ms) {
			groups[group.Name] = group
		}
	}
	return groups, nil
}

// servingGroupSettled reports whether the status of group was computed from its current spec
// and says that the group is Ready.
func servingGroupSettled(group *workload.ServingGroup) bool {
	return group.DeletionTimestamp == nil && group.Status.ObservedGeneration == group.Generation &&
		meta.IsStatusConditionTrue(group.Status.Conditions, string(workload.ServingGroupConditionReady))
}

// describeServingGroups renders the state waitForServingGroups waits on.
func describeServingGroups(groups map[string]workload.ServingGroup) string {
	names := make([]string, 0, len(groups))
	for name := range groups {
		names = append(names, name)
	}
	sort.Strings(names)
	states := make([]string, 0, len(names))
	for _, name := range names {
		group := groups[name]
		states = append(states, fmt.Sprintf("%s(uid=%s generation=%d observed=%d ready=%t failed=%t pods=%d/%d deleting=%t)",
			name, group.UID, group.Generation, group.Status.ObservedGeneration,
			meta.IsStatusConditionTrue(group.Status.Conditions, string(workload.ServingGroupConditionReady)),
			meta.IsStatusConditionTrue(group.Status.Conditions, string(workload.ServingGroupConditionFailed)),
			group.Status.ReadyPods, group.Spec.ExpectedPods, group.DeletionTimestamp != nil))
	}
	return strings.Join(states, " ")
}

// waitForServingGroups waits until the ServingGroups ms controls are exactly the ordinals
// [0, replicas), every one of them settled, and returns them by ordinal.
func waitForServingGroups(t *testing.T, ctx context.Context, kthenaClient *clientset.Clientset, ms *workload.ModelServing, replicas int) []workload.ServingGroup {
	t.Helper()
	settled := make([]workload.ServingGroup, replicas)
	last := ""
	require.Eventually(t, func() bool {
		groups, err := servingGroupsOf(ctx, kthenaClient, ms)
		if err != nil {
			return false
		}
		if state := describeServingGroups(groups); state != last {
			t.Logf("ServingGroups of %s: %s", ms.Name, state)
			last = state
		}
		if len(groups) != replicas {
			return false
		}
		for ordinal := range replicas {
			group, ok := groups[controllerutils.GenerateServingGroupName(ms.Name, ordinal)]
			if !ok || !servingGroupSettled(&group) {
				return false
			}
			settled[ordinal] = group
		}
		return true
	}, servingGroupTimeout, time.Second, "ModelServing %s should have exactly %d Ready ServingGroups at ordinals [0, %d)", ms.Name, replicas, replicas)
	return settled
}

// podsOfServingGroup returns the Pods that carry the name of the group, sorted by name.
func podsOfServingGroup(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, groupName string) []corev1.Pod {
	t.Helper()
	pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: workload.GroupNameLabelKey + "=" + groupName})
	require.NoError(t, err, "Failed to list the Pods of ServingGroup %s", groupName)
	sort.Slice(pods.Items, func(a, b int) bool { return pods.Items[a].Name < pods.Items[b].Name })
	return pods.Items
}

// assertControlledBy asserts that the only owner reference of obj names owner, of the given
// kind, as its controller and blocks the deletion of owner.
func assertControlledBy(t *testing.T, obj metav1.Object, kind schema.GroupVersionKind, owner metav1.Object) {
	t.Helper()
	refs := obj.GetOwnerReferences()
	if !assert.Len(t, refs, 1, "%s should have exactly one owner reference", obj.GetName()) {
		return
	}
	ref := refs[0]
	assert.Equal(t, kind.GroupVersion().String(), ref.APIVersion, "owner apiVersion of %s", obj.GetName())
	assert.Equal(t, kind.Kind, ref.Kind, "owner kind of %s", obj.GetName())
	assert.Equal(t, owner.GetName(), ref.Name, "owner name of %s", obj.GetName())
	assert.Equal(t, owner.GetUID(), ref.UID, "owner UID of %s", obj.GetName())
	assert.True(t, ptr.Deref(ref.Controller, false), "the owner of %s should be its controller", obj.GetName())
	assert.True(t, ptr.Deref(ref.BlockOwnerDeletion, false), "%s should block the deletion of its owner", obj.GetName())
}

// containerRuns identifies the running containers of a Pod. It changes when a container is restarted.
func containerRuns(pod corev1.Pod) []string {
	runs := make([]string, 0, len(pod.Status.ContainerStatuses))
	for _, status := range pod.Status.ContainerStatuses {
		runs = append(runs, fmt.Sprintf("%s restarts=%d id=%s", status.Name, status.RestartCount, status.ContainerID))
	}
	sort.Strings(runs)
	return runs
}

// assertPodsUntouched asserts that the Pods of a group are the ones in before, with their
// containers running as they were.
func assertPodsUntouched(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, groupName string, before []corev1.Pod) {
	t.Helper()
	after := podsOfServingGroup(t, ctx, kubeClient, groupName)
	require.Len(t, after, len(before), "ServingGroup %s should keep its Pods", groupName)
	for i, pod := range after {
		assert.Equal(t, before[i].Name, pod.Name)
		assert.Equal(t, before[i].UID, pod.UID, "Pod %s of the untouched ServingGroup %s was replaced", pod.Name, groupName)
		assert.Nil(t, pod.DeletionTimestamp, "Pod %s of the untouched ServingGroup %s is being deleted", pod.Name, groupName)
		assert.Equal(t, containerRuns(before[i]), containerRuns(pod), "Pod %s of the untouched ServingGroup %s was restarted", pod.Name, groupName)
	}
}

// TestServingGroupLifecycle verifies the ServingGroup objects of a ModelServing: one for every
// replica, named by its ordinal, controlled by the ModelServing and controlling its Pods; what
// their spec and status say once they are Ready; that a scale-in removes the highest ordinal;
// and that deleting the ModelServing removes the ServingGroups and the Pods.
func TestServingGroupLifecycle(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	const replicas = 3
	modelServing := createBasicModelServing("test-sg-lifecycle", replicas, 0, createRole("prefill", 1, 1), createRole("decode", 1, 0))
	t.Log("Creating ModelServing with 3 replicas of prefill (entry and worker) and decode (entry)")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)

	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")
	require.NotEmpty(t, ms.Status.UpdateRevision, "ModelServing should report its revision")

	resources, err := kubeClient.Discovery().ServerResourcesForGroupVersion(workload.SchemeGroupVersion.String())
	require.NoError(t, err, "Failed to discover the workload API")
	shortNames := map[string][]string{}
	for _, resource := range resources.APIResources {
		shortNames[resource.Name] = resource.ShortNames
	}
	assert.Equal(t, []string{"sg"}, shortNames["servinggroups"], "ServingGroup should be served with the short name sg")

	groups := waitForServingGroups(t, ctx, kthenaClient, ms, replicas)
	for ordinal, group := range groups {
		assert.Equal(t, controllerutils.GenerateServingGroupName(ms.Name, ordinal), group.Name)
		assertControlledBy(t, &group, workload.ModelServingKind, ms)
		assert.Equal(t, ms.Name, group.Labels[workload.ModelServingNameLabelKey], "ServingGroup %s should carry the name of its ModelServing", group.Name)
		assert.NotContains(t, group.Annotations, workload.SurgeAnnotationKey, "ServingGroup %s is a regular replica", group.Name)

		assert.Equal(t, ms.Status.UpdateRevision, group.Spec.Revision, "revision of ServingGroup %s", group.Name)
		assert.Equal(t, ms.Status.UpdateRevision, group.Spec.TargetRevision, "targetRevision of ServingGroup %s", group.Name)
		assert.Equal(t, int32(3), group.Spec.ExpectedPods, "expectedPods of ServingGroup %s", group.Name)
		assert.Equal(t, ms.Spec.RecoveryPolicy, group.Spec.RecoveryPolicy, "recoveryPolicy of ServingGroup %s", group.Name)
		if assert.NotNil(t, group.Spec.RolloutStrategy, "rolloutStrategy of ServingGroup %s", group.Name) {
			assert.Equal(t, workload.ServingGroupRollingUpdate, group.Spec.RolloutStrategy.Type)
		}
		roles := map[string]int32{}
		for _, role := range group.Spec.Roles {
			roles[role.Name] = role.Replicas
		}
		assert.Equal(t, map[string]int32{"prefill": 1, "decode": 1}, roles, "roles of ServingGroup %s", group.Name)

		status := group.Status
		assert.False(t, meta.IsStatusConditionTrue(status.Conditions, string(workload.ServingGroupConditionFailed)), "ServingGroup %s should not be Failed", group.Name)
		assert.Equal(t, group.Spec.Revision, status.Revision, "status.revision of ServingGroup %s", group.Name)
		assert.Equal(t, int32(3), status.Pods, "status.pods of ServingGroup %s", group.Name)
		assert.Equal(t, int32(3), status.ReadyPods, "status.readyPods of ServingGroup %s", group.Name)
		assert.Zero(t, status.FaultyMembers, "status.faultyMembers of ServingGroup %s", group.Name)
		assert.Zero(t, status.TerminatingPods, "status.terminatingPods of ServingGroup %s", group.Name)
		assert.Equal(t, workload.GroupNameLabelKey+"="+group.Name, status.LabelSelector, "status.labelSelector of ServingGroup %s", group.Name)

		ledger := map[string]workload.RoleInstanceStatus{}
		for _, instance := range status.RoleInstances {
			ledger[controllerutils.GenerateRoleID(instance.Role, int(instance.Index))] = instance
			assert.Equal(t, workload.RoleInstanceEstablished, instance.Phase, "Role instance %s-%d of ServingGroup %s", instance.Role, instance.Index, group.Name)
		}
		assert.Len(t, status.RoleInstances, 2, "status.roleInstances of ServingGroup %s", group.Name)
		assert.Contains(t, ledger, "prefill-0")
		assert.Contains(t, ledger, "decode-0")

		pods := podsOfServingGroup(t, ctx, kubeClient, group.Name)
		names := make([]string, 0, len(pods))
		for _, pod := range pods {
			names = append(names, pod.Name)
			assertControlledBy(t, &pod, workload.ServingGroupKind, &group)
			assert.Equal(t, ms.Name, pod.Labels[workload.ModelServingNameLabelKey], "Pod %s should carry the name of its ModelServing", pod.Name)
			assert.Equal(t, group.Spec.Revision, pod.Labels[workload.RevisionLabelKey], "revision of Pod %s", pod.Name)
			if instance, ok := ledger[pod.Labels[workload.RoleIDKey]]; assert.True(t, ok, "Pod %s belongs to no Role instance of the ledger", pod.Name) {
				assert.Equal(t, strconv.FormatInt(instance.Generation, 10), pod.Labels[workload.InstanceGenerationLabelKey],
					"Pod %s should carry the generation of its Role instance", pod.Name)
			}
			assert.True(t, utils.IsPodReady(pod), "Pod %s of a Ready ServingGroup should be Ready", pod.Name)
		}
		assert.ElementsMatch(t, []string{
			controllerutils.GeneratePodName(group.Name, "prefill-0", 0),
			controllerutils.GeneratePodName(group.Name, "prefill-0", 1),
			controllerutils.GeneratePodName(group.Name, "decode-0", 0),
		}, names, "Pods of ServingGroup %s", group.Name)
	}

	t.Log("Scaling in from 3 to 2 replicas: the ServingGroup with the highest ordinal goes")
	survivors := map[string][]corev1.Pod{}
	for _, group := range groups[:replicas-1] {
		survivors[group.Name] = podsOfServingGroup(t, ctx, kubeClient, group.Name)
	}
	removed := groups[replicas-1]
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.Replicas = ptr.To(int32(replicas - 1))
	})
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
	for ordinal, group := range waitForServingGroups(t, ctx, kthenaClient, ms, replicas-1) {
		assert.Equal(t, groups[ordinal].UID, group.UID, "ServingGroup %s should survive the scale-in", group.Name)
		assertPodsUntouched(t, ctx, kubeClient, group.Name, survivors[group.Name])
	}
	_, err = kthenaClient.WorkloadV1alpha1().ServingGroups(testNamespace).Get(ctx, removed.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "ServingGroup %s should be removed by the scale-in, got: %v", removed.Name, err)
	assert.Empty(t, podsOfServingGroup(t, ctx, kubeClient, removed.Name), "Pods of the removed ServingGroup %s", removed.Name)

	t.Log("Deleting the ModelServing: its ServingGroups and Pods go with it")
	err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Delete(ctx, ms.Name, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete ModelServing")
	require.Eventually(t, func() bool {
		remaining, err := servingGroupsOf(ctx, kthenaClient, ms)
		return err == nil && len(remaining) == 0
	}, servingGroupTimeout, 2*time.Second, "ServingGroups of the deleted ModelServing were not removed")
	waitForPodsGone(t, ctx, kubeClient, modelServingLabelSelector(ms.Name), servingGroupTimeout)
	_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	assert.True(t, apierrors.IsNotFound(err), "ModelServing should be deleted, got: %v", err)
}

// TestServingGroupDeletion verifies what follows when a ServingGroup object is deleted by hand,
// with each propagation policy `kubectl delete sg --cascade` offers. Deleted in the foreground
// or in the background, the group comes back under its name with new Pods. Deleted with its
// Pods orphaned, the group comes back and takes the Pods over without restarting them. The
// other ServingGroup of the ModelServing is not touched.
func TestServingGroupDeletion(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	role := createRole("prefill", 1, 1)
	role.EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	role.WorkerTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	modelServing := createBasicModelServing("test-sg-deletion", 2, 0, role)
	t.Log("Creating ModelServing with 2 replicas of an entry and a worker Pod")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	testCases := []struct {
		name        string
		propagation metav1.DeletionPropagation
		adopted     bool
	}{
		{name: "Foreground", propagation: metav1.DeletePropagationForeground},
		{name: "Background", propagation: metav1.DeletePropagationBackground},
		{name: "Orphan", propagation: metav1.DeletePropagationOrphan, adopted: true},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			groups := waitForServingGroups(t, ctx, kthenaClient, ms, 2)
			kept, deleted := groups[0], groups[1]
			keptPods := podsOfServingGroup(t, ctx, kubeClient, kept.Name)
			deletedPods := podsOfServingGroup(t, ctx, kubeClient, deleted.Name)
			require.Len(t, keptPods, 2, "Pods of ServingGroup %s", kept.Name)
			require.Len(t, deletedPods, 2, "Pods of ServingGroup %s", deleted.Name)

			t.Logf("Deleting ServingGroup %s (UID %s) with propagation policy %s", deleted.Name, deleted.UID, tc.propagation)
			err := kthenaClient.WorkloadV1alpha1().ServingGroups(testNamespace).Delete(ctx, deleted.Name, metav1.DeleteOptions{
				Preconditions:     &metav1.Preconditions{UID: &deleted.UID},
				PropagationPolicy: &tc.propagation,
			})
			require.NoError(t, err, "Failed to delete ServingGroup %s", deleted.Name)

			// The deleted object is gone, or marked as deleted, when the call returns: a
			// ServingGroup that is settled from here on is the one that replaces it.
			groups = waitForServingGroups(t, ctx, kthenaClient, ms, 2)
			recreated := groups[1]
			require.NotEqual(t, deleted.UID, recreated.UID, "ServingGroup %s should be a new object", deleted.Name)
			assertControlledBy(t, &recreated, workload.ModelServingKind, ms)
			assert.Equal(t, deleted.Spec.Revision, recreated.Spec.Revision, "ServingGroup %s should come back at its revision", deleted.Name)

			pods := podsOfServingGroup(t, ctx, kubeClient, recreated.Name)
			require.Len(t, pods, len(deletedPods), "Pods of the recreated ServingGroup %s", recreated.Name)
			for i, pod := range pods {
				before := deletedPods[i]
				require.Equal(t, before.Name, pod.Name)
				assertControlledBy(t, &pod, workload.ServingGroupKind, &recreated)
				assert.True(t, utils.IsPodReady(pod), "Pod %s should be Ready", pod.Name)
				if !tc.adopted {
					assert.NotEqual(t, before.UID, pod.UID, "Pod %s should be created anew for the recreated ServingGroup", pod.Name)
					continue
				}
				assert.Equal(t, before.UID, pod.UID, "Pod %s should be adopted, not replaced", pod.Name)
				assert.Equal(t, containerRuns(before), containerRuns(pod), "Pod %s should not be restarted by the adoption", pod.Name)
				assert.Equal(t, before.Labels[workload.InstanceGenerationLabelKey], pod.Labels[workload.InstanceGenerationLabelKey],
					"Pod %s should keep the generation of its Role instance", pod.Name)
			}

			assert.Equal(t, kept.UID, groups[0].UID, "ServingGroup %s should not be replaced", kept.Name)
			assertPodsUntouched(t, ctx, kubeClient, kept.Name, keptPods)
			utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
		})
	}
}

// TestServingGroupRecreateReplacesGroupObject verifies whole-group recovery on the ServingGroup
// object: under ServingGroupRecreate the loss of one Pod marks the group Failed, the object is
// deleted in the foreground and a new object takes its name, and with it its ordinal, with new
// Pods. The other ServingGroup of the ModelServing is not touched.
func TestServingGroupRecreateReplacesGroupObject(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)

	prefill, decode := createRole("prefill", 1, 0), createRole("decode", 1, 0)
	prefill.EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	decode.EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	modelServing := createBasicModelServing("test-sg-recreate-object", 2, 0, prefill, decode)
	modelServing.Spec.RecoveryPolicy = workload.ServingGroupRecreate
	t.Log("Creating ModelServing with ServingGroupRecreate policy, 2 replicas and 2 roles")
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")

	groups := waitForServingGroups(t, ctx, kthenaClient, ms, 2)
	kept, failed := groups[0], groups[1]
	keptPods := podsOfServingGroup(t, ctx, kubeClient, kept.Name)
	failedPods := podsOfServingGroup(t, ctx, kubeClient, failed.Name)
	require.Len(t, keptPods, 2, "Pods of ServingGroup %s", kept.Name)
	require.Len(t, failedPods, 2, "Pods of ServingGroup %s", failed.Name)

	// Every change of the object named after the group, from the state that was just read.
	watcher, err := kthenaClient.WorkloadV1alpha1().ServingGroups(testNamespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   "metadata.name=" + failed.Name,
		ResourceVersion: failed.ResourceVersion,
	})
	require.NoError(t, err, "Failed to watch ServingGroup %s", failed.Name)
	defer watcher.Stop()

	victim := failedPods[0]
	t.Logf("Deleting Pod %s of ServingGroup %s (UID %s) to trigger ServingGroupRecreate", victim.Name, failed.Name, failed.UID)
	err = kubeClient.CoreV1().Pods(testNamespace).Delete(ctx, victim.Name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &victim.UID}})
	require.NoError(t, err, "Failed to delete Pod %s", victim.Name)

	sawFailed, sawForegroundDeletion, sawDeleted := false, false, false
	var replacement types.UID
	timeout := time.After(servingGroupTimeout)
	for replacement == "" {
		select {
		case event, ok := <-watcher.ResultChan():
			require.True(t, ok, "the watch of ServingGroup %s ended before the group was replaced", failed.Name)
			group, isGroup := event.Object.(*workload.ServingGroup)
			require.True(t, isGroup, "unexpected %s event while watching ServingGroup %s: %v", event.Type, failed.Name, event.Object)
			if group.UID != failed.UID {
				require.Equal(t, watch.Added, event.Type, "first event of the object that replaces ServingGroup %s", failed.Name)
				require.True(t, sawDeleted, "ServingGroup %s was created again before the failed object was gone", failed.Name)
				replacement = group.UID
				continue
			}
			sawFailed = sawFailed || meta.IsStatusConditionTrue(group.Status.Conditions, string(workload.ServingGroupConditionFailed))
			if group.DeletionTimestamp != nil && !sawForegroundDeletion {
				require.True(t, sawFailed, "ServingGroup %s should be marked Failed before it is deleted", failed.Name)
				require.Contains(t, group.Finalizers, metav1.FinalizerDeleteDependents, "ServingGroup %s should be deleted in the foreground", failed.Name)
				sawForegroundDeletion = true
				t.Logf("ServingGroup %s (UID %s) is Failed and deleted in the foreground", failed.Name, failed.UID)
			}
			if event.Type == watch.Deleted {
				require.True(t, sawForegroundDeletion, "ServingGroup %s should be deleted by a foreground deletion", failed.Name)
				sawDeleted = true
			}
		case <-timeout:
			require.FailNow(t, "ServingGroup was not replaced after a Pod was deleted under ServingGroupRecreate",
				"ServingGroup %s: failed=%t foregroundDeletion=%t deleted=%t", failed.Name, sawFailed, sawForegroundDeletion, sawDeleted)
		}
	}
	t.Logf("ServingGroup %s was created again with UID %s", failed.Name, replacement)

	groups = waitForServingGroups(t, ctx, kthenaClient, ms, 2)
	recreated := groups[1]
	assert.Equal(t, replacement, recreated.UID, "the object that took the name %s should be the one that became Ready", failed.Name)
	assertControlledBy(t, &recreated, workload.ModelServingKind, ms)
	assert.Equal(t, failed.Spec.Revision, recreated.Spec.Revision, "ServingGroup %s should come back at its revision", failed.Name)

	pods := podsOfServingGroup(t, ctx, kubeClient, recreated.Name)
	require.Len(t, pods, len(failedPods), "Pods of the recreated ServingGroup %s", recreated.Name)
	for i, pod := range pods {
		require.Equal(t, failedPods[i].Name, pod.Name)
		assert.NotEqual(t, failedPods[i].UID, pod.UID, "Pod %s should be created anew for the recreated ServingGroup", pod.Name)
		assertControlledBy(t, &pod, workload.ServingGroupKind, &recreated)
		assert.True(t, utils.IsPodReady(pod), "Pod %s should be Ready", pod.Name)
	}

	assert.Equal(t, kept.UID, groups[0].UID, "ServingGroup %s should not be replaced", kept.Name)
	assertPodsUntouched(t, ctx, kubeClient, kept.Name, keptPods)
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)
}

// scaleControllerManager sets the number of controller manager replicas and waits until exactly
// that many Pods exist and all of them are ready.
func scaleControllerManager(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, replicas int32) {
	t.Helper()
	deployments := kubeClient.AppsV1().Deployments(kthenaNamespace)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		scale, err := deployments.GetScale(ctx, controllerManagerDeployment, metav1.GetOptions{})
		if err != nil {
			return err
		}
		scale.Spec.Replicas = replicas
		_, err = deployments.UpdateScale(ctx, controllerManagerDeployment, scale, metav1.UpdateOptions{})
		return err
	})
	require.NoError(t, err, "Failed to scale the controller manager to %d replicas", replicas)
	require.Eventually(t, func() bool {
		deployment, err := deployments.Get(ctx, controllerManagerDeployment, metav1.GetOptions{})
		if err != nil || deployment.Status.ObservedGeneration < deployment.Generation ||
			deployment.Status.Replicas != replicas || deployment.Status.ReadyReplicas != replicas {
			return false
		}
		pods, err := kubeClient.CoreV1().Pods(kthenaNamespace).List(ctx, metav1.ListOptions{LabelSelector: controllerManagerSelector})
		return err == nil && len(pods.Items) == int(replicas)
	}, 3*time.Minute, 2*time.Second, "controller manager did not reach %d ready replicas", replicas)
}

// waitForControllerManagerLeader waits until a ready controller manager Pod other than
// previous holds the leader election Lease, and returns its name.
func waitForControllerManagerLeader(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, previous string) string {
	t.Helper()
	leader := ""
	require.Eventually(t, func() bool {
		lease, err := kubeClient.CoordinationV1().Leases(kthenaNamespace).Get(ctx, controllerManagerLease, metav1.GetOptions{})
		if err != nil {
			return false
		}
		// The identity of a holder is <Pod name>_<UUID>.
		holder, _, _ := strings.Cut(ptr.Deref(lease.Spec.HolderIdentity, ""), "_")
		if holder == "" || holder == previous {
			return false
		}
		pod, err := kubeClient.CoreV1().Pods(kthenaNamespace).Get(ctx, holder, metav1.GetOptions{})
		if err != nil || !utils.IsPodReady(*pod) {
			return false
		}
		leader = holder
		return true
	}, 2*time.Minute, time.Second, "no ready controller manager Pod other than %q holds the Lease %s", previous, controllerManagerLease)
	return leader
}

// trackUnavailableServingGroups watches the Pods of a ModelServing whose ServingGroups have one
// Pod each, and records the largest number of the ordinals [0, replicas) that were without a
// Ready Pod at the same time. A group whose Pod is gone counts, as does one whose Pod is not
// Ready or is being deleted. The returned function stops the watch and returns the peak.
func trackUnavailableServingGroups(t *testing.T, ctx context.Context, kubeClient *kubernetes.Clientset, msName string, replicas int) func() int {
	t.Helper()
	selector := modelServingLabelSelector(msName)
	list, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	require.NoError(t, err, "Failed to list the Pods of ModelServing %s", msName)
	pods := map[string]corev1.Pod{}
	for _, pod := range list.Items {
		pods[pod.Name] = pod
	}
	unavailable := func() int {
		available := map[string]bool{}
		for _, pod := range pods {
			if utils.IsPodReady(pod) {
				available[pod.Labels[workload.GroupNameLabelKey]] = true
			}
		}
		count := 0
		for ordinal := range replicas {
			if !available[controllerutils.GenerateServingGroupName(msName, ordinal)] {
				count++
			}
		}
		return count
	}

	// The watch starts where the list ended, so no change of a Pod is missed.
	watchCtx, cancel := context.WithCancel(ctx)
	watcher, err := kubeClient.CoreV1().Pods(testNamespace).Watch(watchCtx, metav1.ListOptions{LabelSelector: selector, ResourceVersion: list.ResourceVersion})
	if err != nil {
		cancel()
		require.NoError(t, err, "Failed to watch the Pods of ModelServing %s", msName)
	}

	var mu sync.Mutex
	peak := unavailable()
	var watchErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		for event := range watcher.ResultChan() {
			pod, ok := event.Object.(*corev1.Pod)
			if !ok {
				if watchCtx.Err() == nil {
					mu.Lock()
					watchErr = fmt.Errorf("unexpected %s event: %v", event.Type, event.Object)
					mu.Unlock()
				}
				return
			}
			if event.Type == watch.Deleted {
				delete(pods, pod.Name)
			} else {
				pods[pod.Name] = *pod
			}
			count := unavailable()
			mu.Lock()
			peak = max(peak, count)
			mu.Unlock()
		}
		if watchCtx.Err() == nil {
			mu.Lock()
			watchErr = fmt.Errorf("watch closed before the tracker was stopped")
			mu.Unlock()
		}
	}()

	var once sync.Once
	stop := func() int {
		once.Do(func() {
			cancel()
			watcher.Stop()
			<-done
		})
		mu.Lock()
		defer mu.Unlock()
		// A broken watch means the peak may be incomplete, so it cannot prove the budget held.
		require.NoError(t, watchErr, "the unavailable ServingGroup tracker lost its watch")
		return peak
	}
	t.Cleanup(func() { stop() })
	return stop
}

// TestServingGroupControllerFailoverDuringRollingUpdate verifies that a rolling update is
// carried on by another controller manager when the leading one is lost half way. The update
// is held after its first ServingGroup by a readiness gate, so the leader is deleted while the
// whole maxUnavailable budget is in use: the successor must neither replace a second group
// before the first is Ready nor leave the update unfinished.
func TestServingGroupControllerFailoverDuringRollingUpdate(t *testing.T) {
	ctx, kthenaClient, kubeClient := setupControllerManagerE2ETest(t)
	waitForWebhookReady(t, ctx, kthenaClient, testNamespace)

	const (
		replicas       = 4
		maxUnavailable = 1
	)
	gate := corev1.PodConditionType("kthena.e2e/failover-ready")
	releaseGate := []byte(fmt.Sprintf(`{"status":{"conditions":[{"type":"%s","status":"True"}]}}`, gate))
	selector := modelServingLabelSelector("test-sg-failover")

	// Registered first, so it runs after the ModelServing is deleted: the tests that follow
	// find one controller manager, leading and answering the webhooks.
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		scaleControllerManager(t, cleanupCtx, kubeClient, 1)
		waitForControllerManagerLeader(t, cleanupCtx, kubeClient, "")
		waitForWebhookReady(t, cleanupCtx, kthenaClient, testNamespace)
	})
	t.Log("Scaling the controller manager to 2 replicas")
	scaleControllerManager(t, ctx, kubeClient, 2)

	modelServing := createBasicModelServing("test-sg-failover", replicas, 0)
	modelServing.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable = ptr.To(intstr.FromInt(maxUnavailable))
	modelServing.Spec.Template.Roles[0].EntryTemplate.Spec.TerminationGracePeriodSeconds = ptr.To(int64(1))
	t.Logf("Creating ModelServing with %d replicas and maxUnavailable=%d", replicas, maxUnavailable)
	createAndWaitForModelServing(t, ctx, kthenaClient, modelServing)
	ms, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, modelServing.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")
	waitForServingGroups(t, ctx, kthenaClient, ms, replicas)
	stopTracking := trackUnavailableServingGroups(t, ctx, kubeClient, ms.Name, replicas)

	// held lists the Pods and reports whether the update stands at its first group: one Pod
	// of the new template, running behind its gate, and every other group Ready on the old
	// template.
	held := func() (map[string]types.UID, bool, error) {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return nil, false, err
		}
		uids := map[string]types.UID{}
		replaced := 0
		for _, pod := range pods.Items {
			uids[pod.Name] = pod.UID
			switch {
			case pod.DeletionTimestamp != nil:
				return uids, false, nil
			case len(pod.Spec.ReadinessGates) == 0:
				if !utils.IsPodReady(pod) {
					return uids, false, nil
				}
			case pod.Status.Phase != corev1.PodRunning || utils.IsPodReady(pod):
				return uids, false, nil
			default:
				replaced++
			}
		}
		return uids, len(pods.Items) == replicas && replaced == maxUnavailable, nil
	}

	t.Log("Starting a rolling update whose Pods wait behind a readiness gate")
	updateModelServingWithRetry(t, ctx, kthenaClient, ms.Name, func(current *workload.ModelServing) {
		current.Spec.Template.Roles[0].EntryTemplate.Spec.ReadinessGates = []corev1.PodReadinessGate{{ConditionType: gate}}
	})
	var heldPods map[string]types.UID
	require.Eventually(t, func() bool {
		pods, ok, err := held()
		heldPods = pods
		return err == nil && ok
	}, servingGroupTimeout, time.Second, "the rolling update should replace one ServingGroup and wait for it")

	leader := waitForControllerManagerLeader(t, ctx, kubeClient, "")
	t.Logf("Deleting the leading controller manager Pod %s in the middle of the rolling update", leader)
	err = kubeClient.CoreV1().Pods(kthenaNamespace).Delete(ctx, leader, metav1.DeleteOptions{})
	require.NoError(t, err, "Failed to delete controller manager Pod %s", leader)
	successor := waitForControllerManagerLeader(t, ctx, kubeClient, leader)
	t.Logf("Controller manager Pod %s leads now", successor)

	// A change that is no part of a revision makes the successor decide a round for this
	// ModelServing. Its status tells when that round is over.
	var generation int64
	require.Eventually(t, func() bool {
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			current, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
			if err != nil {
				return err
			}
			current.Spec.RevisionHistoryLimit = ptr.To(ptr.Deref(current.Spec.RevisionHistoryLimit, 10) + 1)
			generation = current.Generation + 1
			_, err = kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Update(ctx, current, metav1.UpdateOptions{})
			return err
		})
		if err != nil {
			t.Logf("Updating ModelServing %s after the failover, retrying: %v", ms.Name, err)
		}
		return err == nil
	}, time.Minute, 2*time.Second, "the ModelServing could not be updated after the failover")
	require.Eventually(t, func() bool {
		current, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
		return err == nil && current.Status.ObservedGeneration >= generation
	}, servingGroupTimeout, time.Second, "the new leader should reconcile the ModelServing")

	t.Log("The new leader has reconciled the ModelServing; the update must still wait for its first ServingGroup")
	require.Never(t, func() bool {
		pods, ok, err := held()
		if err != nil {
			// Not an observation of the Pods: the next one decides.
			return false
		}
		if !ok {
			return true
		}
		for name, uid := range heldPods {
			if pods[name] != uid {
				return true
			}
		}
		return false
	}, 5*time.Second, 500*time.Millisecond, "the new leader went on although the maxUnavailable budget was in use")

	t.Log("Releasing the readiness gate of every replaced Pod until the rolling update is complete")
	require.Eventually(t, func() bool {
		pods, err := kubeClient.CoreV1().Pods(testNamespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return false
		}
		complete := len(pods.Items) == replicas
		for _, pod := range pods.Items {
			if pod.DeletionTimestamp != nil || len(pod.Spec.ReadinessGates) == 0 || pod.Status.Phase != corev1.PodRunning {
				complete = false
				continue
			}
			released := false
			for _, condition := range pod.Status.Conditions {
				released = released || (condition.Type == gate && condition.Status == corev1.ConditionTrue)
			}
			if !released {
				if _, err := kubeClient.CoreV1().Pods(testNamespace).Patch(ctx, pod.Name, types.StrategicMergePatchType, releaseGate, metav1.PatchOptions{}, "status"); err != nil {
					t.Logf("Releasing the readiness gate of Pod %s, retrying: %v", pod.Name, err)
				}
			}
			complete = complete && utils.IsPodReady(pod)
		}
		return complete
	}, 3*time.Minute, time.Second, "the rolling update should be completed by the new leader")
	utils.WaitForModelServingReady(t, ctx, kthenaClient, testNamespace, ms.Name)

	peak := stopTracking()
	t.Logf("Largest number of ServingGroups unavailable at the same time: %d", peak)
	assert.LessOrEqual(t, peak, maxUnavailable, "more ServingGroups were unavailable at the same time than maxUnavailable allows")
	assert.Positive(t, peak, "the rolling update should have replaced ServingGroups")

	final, err := kthenaClient.WorkloadV1alpha1().ModelServings(testNamespace).Get(ctx, ms.Name, metav1.GetOptions{})
	require.NoError(t, err, "Failed to get ModelServing")
	assert.Equal(t, int32(replicas), final.Status.UpdatedReplicas, "every ServingGroup should be updated")
	assert.NotEqual(t, ms.Status.UpdateRevision, final.Status.UpdateRevision, "the rolling update should have a new revision")
	for _, group := range waitForServingGroups(t, ctx, kthenaClient, ms, replicas) {
		assert.Equal(t, final.Status.UpdateRevision, group.Spec.Revision, "revision of ServingGroup %s", group.Name)
	}
}
