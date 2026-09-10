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

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
)

func (h *revisionHistory) podMatchesTemplate(ctx context.Context, pod *corev1.Pod, role workloadv1alpha1.Role, revision string) (bool, error) {
	if (revision != "" && utils.ObjectRevision(pod) == revision) || utils.ObjectRoleTemplateHash(pod) == utils.CalRoleTemplateHash(role) {
		return true, nil
	}
	roles, err := h.roles(ctx, utils.ObjectRevision(pod))
	if err != nil {
		return false, err
	}
	for _, historical := range roles {
		if historical.Name == role.Name {
			return utils.EqualRoleTemplatesForRevision([]workloadv1alpha1.Role{historical}, []workloadv1alpha1.Role{role}), nil
		}
	}
	return false, fmt.Errorf("role %s is missing from Pod %s revision", role.Name, pod.Name)
}
