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
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
)

func TestUpdatePodGroupIfNeeded_Conflict(t *testing.T) {
	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-ms",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				GangPolicy: &workloadv1alpha1.GangPolicy{},
			},
		},
	}

	pg := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "test-ms-0",
			Namespace:       "default",
			ResourceVersion: "1",
		},
		Spec: schedulingv1beta1.PodGroupSpec{
			MinMember: 1,
		},
	}

	// Create fake client
	fakeVolcanoClient := volcanofake.NewSimpleClientset(pg)
	store := datastore.New()
	apiextfake := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())

	manager := NewManager(nil, fakeVolcanoClient, apiextfake, store)
	manager.hasPodGroupCRD.Store(true)

	// Inject reactor to simulate conflict
	conflictCount := 0
	fakeVolcanoClient.PrependReactor("update", "podgroups", func(action k8stesting.Action) (handled bool, ret runtime.Object, err error) {
		if conflictCount == 0 {
			conflictCount++
			return true, nil, apierrors.NewConflict(schema.GroupResource{Group: "scheduling.volcano.sh", Resource: "podgroups"}, "test-ms-0", fmt.Errorf("conflict"))
		}
		return false, nil, nil
	})

	// ms has empty roles, so calculated minMember will be 0.
	// pg has MinMember 1.
	// So 0 != 1, update should happen.

	err := manager.updatePodGroupIfNeeded(context.Background(), pg, ms)
	assert.NoError(t, err)
	assert.Equal(t, 1, conflictCount, "Should have retried once")

	// Verify final state
	updatedPG, err := fakeVolcanoClient.SchedulingV1beta1().PodGroups("default").Get(context.Background(), "test-ms-0", metav1.GetOptions{})
	assert.NoError(t, err)
	assert.Equal(t, int32(0), updatedPG.Spec.MinMember)
}
