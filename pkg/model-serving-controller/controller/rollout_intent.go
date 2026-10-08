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
	"errors"
	"fmt"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var errStaleRolloutIntent = errors.New("ModelServing changed before applying rollout intent")

// API reads are intentional here: informer delivery can lag a completed user
// update. Status-only resourceVersion changes do not invalidate an operation.
func (c *ModelServingController) checkRolloutIntent(ctx context.Context, ms *api.ModelServing) error {
	if ms.ResourceVersion == "" {
		return nil
	} // Not an API-observed object (unit fixtures).
	current, err := c.modelServingClient.WorkloadV1alpha1().ModelServings(ms.Namespace).Get(ctx, ms.Name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if current.UID != ms.UID || current.Generation != ms.Generation || current.DeletionTimestamp != nil {
		c.enqueueModelServing(current)
		return fmt.Errorf("%w: planned UID=%s generation=%d, current UID=%s generation=%d", errStaleRolloutIntent, ms.UID, ms.Generation, current.UID, current.Generation)
	}
	return nil
}

func (c *ModelServingController) groupScaleDownPending(ms *api.ModelServing, groups []datastore.ServingGroup) (bool, error) {
	if c.podsInformer == nil || c.podsLister == nil || len(groups) <= modelServingReplicas(ms) {
		return false, nil
	}
	temporary, err := c.servingGroupSurgeNames(context.Background(), ms)
	if err != nil {
		return false, err
	}
	stable, deleting := 0, false
	for _, group := range groups {
		if temporary.Has(group.Name) {
			continue
		}
		stable++
		deleting = deleting || group.Status == datastore.ServingGroupDeleting || c.rolloutDeletionPending(ms, group.Name, "", "")
	}
	return deleting && stable > modelServingReplicas(ms), nil
}

func (c *ModelServingController) roleScaleDownPending(ms *api.ModelServing, group string, role api.Role, instances []datastore.Role) (bool, error) {
	if c.podsInformer == nil || c.podsLister == nil || len(instances) <= roleReplicas(role) {
		return false, nil
	}
	pods, err := c.surgePods(ms, surgeRole, group, role.Name)
	if err != nil {
		return false, err
	}
	temporary := markedSurgeNames(pods, surgeRole, roleReplicas(role))
	stable, deleting := 0, false
	for _, instance := range instances {
		if temporary.Has(instance.Name) {
			continue
		}
		stable++
		deleting = deleting || instance.Status == datastore.RoleDeleting || c.rolloutDeletionPending(ms, group, role.Name, instance.Name)
	}
	return deleting && stable > roleReplicas(role), nil
}

// Also fence a slot whose old Pods outlived the store/PodGroup. All physical
// members must retire before creating another incarnation at that ordinal.
func (c *ModelServingController) checkGroupCreationSlot(ms *api.ModelServing, group string) error {
	pods, err := c.getPodsByIndex(GroupNameKey, ms.Namespace+"/"+group)
	if err != nil {
		return err
	}
	for _, pod := range pods {
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) && pod.DeletionTimestamp != nil {
			c.enqueueModelServingAfter(ms, enqueueAfter)
			return fmt.Errorf("ServingGroup %s still has retiring Pod UID %s", group, pod.UID)
		}
	}
	return nil
}
