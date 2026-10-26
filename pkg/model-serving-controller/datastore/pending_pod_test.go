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

package datastore

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

func TestIsPodUnavailable(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "ms"}
	for _, groupState := range []ServingGroupStatus{ServingGroupCreating, ServingGroupRunning, ServingGroupDeleting, ServingGroupScaling, ServingGroupReadinessUnknown} {
		for _, roleState := range []RoleStatus{RoleCreating, RoleRunning, RoleDeleting, RoleReadinessUnknown} {
			t.Run(string(groupState)+"/"+string(roleState), func(t *testing.T) {
				s := New()
				s.AddServingGroupAndRole(key, "ms-0", "revision", "hash", "role", "role-0")
				require.NoError(t, s.UpdateServingGroupStatus(key, "ms-0", groupState))
				require.NoError(t, s.UpdateRoleStatus(key, "ms-0", "role", "role-0", roleState))
				require.Equal(t, groupState == ServingGroupCreating && roleState == RoleCreating, s.IsPodUnavailable(key, "ms-0", "role", "role-0", "pod"))
				s.AddRunningPodToServingGroup(key, "ms-0", "pod", "revision", "hash", "role", "role-0")
				require.False(t, s.IsPodUnavailable(key, "ms-0", "role", "role-0", "pod"))
				count, err := s.GetRunningPodNumByServingGroup(key, "ms-0")
				require.NoError(t, err)
				require.Equal(t, 1, count, "observation must not remove a Ready member")
			})
		}
	}
	s := New()
	require.False(t, s.IsPodUnavailable(key, "ms-0", "role", "role-0", "pod"))
	s.AddServingGroupAndRole(key, "ms-0", "revision", "hash", "role", "role-0")
	require.False(t, s.IsPodUnavailable(key, "missing", "role", "role-0", "pod"))
	require.False(t, s.IsPodUnavailable(key, "ms-0", "missing", "role-0", "pod"))
	require.False(t, s.IsPodUnavailable(key, "ms-0", "role", "missing", "pod"))
}
