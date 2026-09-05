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

package podgroupmanager

import (
	"context"
	"github.com/stretchr/testify/require"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/cache"
	"testing"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	pglisters "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"
)

func TestPodGroupObservationDoesNotReplaceSharedLister(t *testing.T) {
	ms := newMinimalMS("queue")
	pg := &schedulingv1beta1.PodGroup{ObjectMeta: metav1.ObjectMeta{Name: "pg", Namespace: ms.Namespace, UID: "live", OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.SchemeGroupVersion.WithKind("ModelServing"))}}}
	client := volcanofake.NewSimpleClientset(pg.DeepCopy())
	shared := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	old := pg.DeepCopy()
	old.UID = "old"
	require.NoError(t, shared.Add(old))
	m := &Manager{volcanoClient: client, PodGroupLister: pglisters.NewPodGroupLister(shared)}
	local := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	require.NoError(t, local.Add(pg))
	ctx := WithPodGroupLister(context.Background(), pglisters.NewPodGroupLister(local))
	observed, err := m.observationLister(ctx).PodGroups(ms.Namespace).Get(pg.Name)
	require.NoError(t, err)
	require.Equal(t, pg.UID, observed.UID)
	require.NoError(t, m.updatePodGroupIfNeeded(ctx, observed, ms))
	unchanged, err := m.GetPodGroupLister().PodGroups(ms.Namespace).Get(pg.Name)
	require.NoError(t, err)
	require.Equal(t, old.UID, unchanged.UID)
	require.ErrorContains(t, m.updatePodGroupIfNeeded(ctx, old, ms), "identity changed")
}
