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
	"context"
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const AppliedRoleReplicasAnnotation = "modelserving.volcano.sh/applied-role-replicas"

// GroupMembersStateName isolates operation state by the owner's immutable UID.
func GroupMembersStateName(ms *api.ModelServing) string {
	return fmt.Sprintf("modelserving-members-%x", sha256.Sum256([]byte(ms.Namespace+"/"+ms.Name+"/"+string(ms.UID))))
}

func ReadGroupMembersState(ctx context.Context, client kubernetes.Interface, ms *api.ModelServing) (*corev1.ConfigMap, error) {
	cm, err := client.CoreV1().ConfigMaps(ms.Namespace).Get(ctx, GroupMembersStateName(ms), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !IsOwnedByModelServingWithUID(cm, ms.UID) || cm.DeletionTimestamp != nil {
		return nil, fmt.Errorf("invalid member state owner or terminating state: %s", cm.Name)
	}
	return cm, nil
}
