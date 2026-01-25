package podgroupmanager

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	testhelper "github.com/volcano-sh/kthena/pkg/model-serving-controller/utils/test"
	apiextfake "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
)

func TestCreateOrUpdatePodGroup_Terminating(t *testing.T) {
	store := datastore.New()

	// Create a PodGroup that is terminating
	pg := &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "test-group",
			Namespace:         "default",
			DeletionTimestamp: &metav1.Time{Time: time.Now()},
		},
	}

	volcanoClient := volcanofake.NewSimpleClientset(pg)
	apiextClient := apiextfake.NewSimpleClientset(testhelper.CreatePodGroupCRD())
	manager := NewManager(nil, volcanoClient, apiextClient, store)
	manager.hasPodGroupCRD.Store(true)

	ms := &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-model",
			Namespace: "default",
		},
		Spec: workloadv1alpha1.ModelServingSpec{
			SchedulerName: "volcano",
			Template: workloadv1alpha1.ServingGroup{
				GangPolicy: &workloadv1alpha1.GangPolicy{},
			},
		},
	}

	err := manager.CreateOrUpdatePodGroup(context.TODO(), ms, "test-group")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "terminating")
}
