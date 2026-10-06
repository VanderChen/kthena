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

package utils

import (
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
)

// EqualRoleTemplatesForRevision is the authoritative comparison for the current
// Roles-only ControllerRevision format. Hashes identify persisted snapshots;
// a hash difference alone is not evidence of a workload template change.
// Both comparison and hashing use the projection in revision_util.go, so adding
// a revisioned field changes all SG, Role, recovery and admission callers together.
// Replica counts, rollout controls and Role ordering are intentionally excluded.
func EqualRoleTemplatesForRevision(left, right []workloadv1alpha1.Role) bool {
	return apiequality.Semantic.DeepEqual(
		projectRoleTemplatesForRevision(left),
		projectRoleTemplatesForRevision(right),
	)
}

// EqualRoleTemplateForRevision compares one Role with the same field projection
// used by CalRoleTemplateHash and the complete ControllerRevision comparator.
func EqualRoleTemplateForRevision(left, right workloadv1alpha1.Role) bool {
	return apiequality.Semantic.DeepEqual(
		projectRoleTemplateForRevision(left),
		projectRoleTemplateForRevision(right),
	)
}
