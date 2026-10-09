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
	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
)

// Completion is recorded only on surviving Pods after checking the full
// historical layout. A wholly absent Role after restart is simply refilled.
func (c *ModelServingController) validateRoleCreation(ctx context.Context, ms *api.ModelServing, group, role, instance string, pods []*corev1.Pod) error {
	observed := datastore.Role{Name: instance, Revision: utils.ObjectRevision(pods[0]), RoleTemplateHash: utils.ObjectRoleTemplateHash(pods[0])}
	template, revision, hash, err := c.roleTemplateForInstance(ctx, ms, group, role, observed, pods)
	if err != nil {
		return err
	}
	groupInstance, roleInstance := pods[0].Annotations[groupInstanceAnnotation], pods[0].Annotations[roleInstanceAnnotation]
	names := map[string]bool{}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil ||
			pod.Annotations[groupInstanceAnnotation] != groupInstance || pod.Annotations[roleInstanceAnnotation] != roleInstance ||
			utils.ObjectRevision(pod) != revision || (utils.ObjectRoleTemplateHash(pod) != "" && utils.ObjectRoleTemplateHash(pod) != hash) {
			return fmt.Errorf("cannot mark incomplete or mixed Role %s/%s", group, instance)
		}
		names[pod.Name] = true
	}
	if len(names) != 1+int(template.WorkerReplicas) {
		return fmt.Errorf("cannot mark incomplete Role %s/%s", group, instance)
	}
	for i := 0; i <= int(template.WorkerReplicas); i++ {
		if !names[utils.GeneratePodName(group, instance, i)] {
			return fmt.Errorf("cannot mark absent member of Role %s/%s", group, instance)
		}
	}
	return nil
}
