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
	"bytes"
	"encoding/json"
	"fmt"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BuildRevisionConfiguration uses the same canonical scheduler/plugin inputs
// for identity, historical comparisons and immutable-field admission.
func BuildRevisionConfiguration(ms *workloadv1alpha1.ModelServing) ([]byte, error) {
	return BuildRevisionData(&workloadv1alpha1.ModelServing{Spec: workloadv1alpha1.ModelServingSpec{
		SchedulerName: ms.Spec.SchedulerName, Plugins: ms.Spec.Plugins,
	}})
}

func EqualRevisionConfiguration(left, right *workloadv1alpha1.ModelServing) (bool, error) {
	a, err := BuildRevisionConfiguration(left)
	if err != nil {
		return false, err
	}
	b, err := BuildRevisionConfiguration(right)
	return bytes.Equal(a, b), err
}

// BuildControllerRevisionData adds the full revisioned configuration while
// retaining the original Role snapshots. Their recorded replica counts are
// needed by production's pending-SG rollout guard (030); counts do not enter
// revision identity and are never overwritten by a later scale operation.
func BuildControllerRevisionData(ms *workloadv1alpha1.ModelServing, roles []workloadv1alpha1.Role) ([]byte, error) {
	historical := ms.DeepCopy()
	historical.Spec.Template.Roles = roles
	data, err := BuildRevisionData(historical)
	if err != nil {
		return nil, err
	}
	var snapshot map[string]json.RawMessage
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, err
	}
	snapshot["data"], err = json.Marshal(roles)
	if err != nil {
		return nil, err
	}
	return json.Marshal(snapshot)
}

// ModelServingForControllerRevision restores all recorded workload inputs,
// preserving current operational policies. Legacy scheduler/plugins can only
// be captured once at migration by EnsureRevisionBaseline; they cannot be
// reconstructed from Roles-only data.
func ModelServingForControllerRevision(ms *workloadv1alpha1.ModelServing, cr *appsv1.ControllerRevision) (*workloadv1alpha1.ModelServing, error) {
	if cr == nil || !metav1.IsControlledBy(cr, ms) {
		return nil, fmt.Errorf("revision is missing or has a different ModelServing owner")
	}
	roles, err := GetRolesFromControllerRevision(cr)
	if err != nil {
		return nil, err
	}
	result := ms.DeepCopy()
	if cr.Annotations[ControllerRevisionDataVersionAnnotation] == ControllerRevisionDataVersionV1 {
		result, err = ApplyRevision(ms, cr)
		if err != nil {
			return nil, err
		}
	}
	// Preserve exact historical Role templates and counts, not normalized Pod
	// fields or current replicas borrowed from the live object.
	result.Spec.Template.Roles = roles
	return result, nil
}
