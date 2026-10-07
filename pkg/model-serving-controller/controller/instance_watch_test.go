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
	"time"

	"github.com/stretchr/testify/require"
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
)

func TestInstanceLifecycleDeletedWatchCannotEvictCurrentIdentity(t *testing.T) {
	for _, policy := range []api.RecoveryPolicy{api.ServingGroupRecreate, api.RoleRecreate} {
		t.Run(string(policy), func(t *testing.T) { verifyDeletedWatchCannotEvictCurrentIdentity(t, policy) })
	}
}

func verifyDeletedWatchCannotEvictCurrentIdentity(t *testing.T, policy api.RecoveryPolicy) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	oldMS := lifecycleMS("watch-uid", 1, 1, 0)
	ms := oldMS.DeepCopy()
	ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "test:v2"
	ms.ResourceVersion = "1"
	ms.Spec.RecoveryPolicy = policy
	c := lifecycleController(t, ms, oldMS)
	current := lifecyclePod(t, c, ms, ms, 0, "current-healthy")
	current.ResourceVersion = "20"
	current.Annotations = map[string]string{groupInstanceAnnotation: "same-group", roleInstanceAnnotation: "new-role"}
	_, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Update(ctx, current, metav1.UpdateOptions{})
	require.NoError(t, err)
	oldEntry := current.DeepCopy()
	oldEntry.UID = "old-entry"
	oldEntry.ResourceVersion = "10"
	oldEntry.Annotations[roleInstanceAnnotation] = "old-role"
	oldEntry.Labels[api.RevisionLabelKey] = utils.ModelServingRevision(oldMS)
	oldEntry.Labels[api.RoleTemplateHashLabelKey] = utils.CalRoleTemplateHash(oldMS.Spec.Template.Roles[0])
	oldEntry.Spec.Containers[0].Image = "test:v1"
	oldWorker := oldEntry.DeepCopy()
	oldWorker.Name = "watch-uid-0-prefill-0-1"
	oldWorker.UID = "old-worker"
	events := watch.NewRaceFreeFake()
	defer events.Stop()
	informer := cache.NewSharedIndexInformer(&cache.ListWatch{
		ListFunc: func(metav1.ListOptions) (runtime.Object, error) {
			return &corev1.PodList{ListMeta: metav1.ListMeta{ResourceVersion: "20"}, Items: []corev1.Pod{*current}}, nil
		},
		WatchFunc: func(metav1.ListOptions) (watch.Interface, error) { return events, nil },
	}, &corev1.Pod{}, 0, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc, GroupNameKey: utils.GroupNameIndexFunc, RoleIDKey: utils.RoleIDIndexFunc})
	completed := make(chan struct{}, 4)
	_, err = informer.AddEventHandler(cache.ResourceEventHandlerFuncs{UpdateFunc: func(old, new interface{}) { c.updatePod(old, new); completed <- struct{}{} }, DeleteFunc: func(obj interface{}) { c.deletePod(obj); completed <- struct{}{} }})
	require.NoError(t, err)
	c.podsInformer = informer
	c.podsLister = corelisters.NewPodLister(informer.GetIndexer())
	go informer.Run(ctx.Done())
	require.True(t, cache.WaitForCacheSync(ctx.Done(), informer.HasSynced))
	// A stale Ready frame must not overwrite identity or misclassify the live
	// target as an old version, even though the informer cache accepted it.
	events.Modify(oldEntry)
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("update callback not delivered")
	}
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.NoError(t, c.manageRollingUpdate(ctx, ms, utils.ModelServingRevision(ms), nil))
	require.False(t, lifecycleDeletionMatches(c, current))
	for _, old := range []*corev1.Pod{oldEntry, oldWorker, oldEntry, oldWorker} {
		events.Delete(old)
		select {
		case <-completed:
		case <-time.After(5 * time.Second):
			t.Fatal("delete callback not delivered")
		}
	}
	require.False(t, lifecycleDeletionMatches(c, current), "a stale Watch deletion must not delete the live replacement")
	// SharedIndexInformer applies Delete by namespace/name before calling us.
	// The API snapshot must preserve physical Ready even when that cache is empty.
	_, exists, err := informer.GetIndexer().GetByKey(ms.Namespace + "/" + current.Name)
	require.NoError(t, err)
	require.False(t, exists)
	require.NoError(t, c.refreshRolloutAvailability(ctx, ms))
	require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), "watch-uid-0"))
	require.NoError(t, c.manageRoleReplicasPerGroup(ctx, ms, "watch-uid-0", ms.Spec.Template.Roles[0], 0, utils.ModelServingRevision(ms), nil, true))
	require.False(t, lifecycleDeletionMatches(c, current), "missing cache entries must not trigger recovery of a physically complete healthy Role")
}
