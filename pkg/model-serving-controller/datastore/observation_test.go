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
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	"testing"
)

func TestCalibrateReadyPodsPreservesOperations(t *testing.T) {
	key := types.NamespacedName{Namespace: "ns", Name: "ms"}
	for _, status := range []RoleStatus{RoleCreating, RoleRunning, RoleDeleting} {
		t.Run(string(status), func(t *testing.T) {
			s := New()
			s.BindModelServing(key, "uid")
			s.AddRunningPodToServingGroup(key, "ms-0", "ghost", "old", "hash", "role", "role-0")
			require.NoError(t, s.UpdateRoleStatus(key, "ms-0", "role", "role-0", status))
			s.CalibrateReadyPods(key, nil)
			require.Equal(t, status, s.GetRoleStatus(key, "ms-0", "role", "role-0"))
			n, err := s.GetRunningPodNumByServingGroup(key, "ms-0")
			require.NoError(t, err)
			require.Zero(t, n)
			s.CalibrateReadyPods(key, map[string][]string{"ms-0": {"live", "live"}})
			n, err = s.GetRunningPodNumByServingGroup(key, "ms-0")
			require.NoError(t, err)
			require.Equal(t, 1, n)
		})
	}
}

func TestObservationKeepsRecoveryHistoryUntilIdentityChanges(t *testing.T) {
	s := New()
	key := types.NamespacedName{Namespace: "ns", Name: "ms"}
	s.BindModelServing(key, "old")
	s.AddRole(key, "ms-0", "role", "role-0", "revision", "hash")
	require.NoError(t, s.UpdateRoleStatus(key, "ms-0", "role", "role-0", RoleRunning))
	require.NoError(t, s.UpdateRoleStatus(key, "ms-0", "role", "role-0", RoleCreating))
	s.CalibrateReadyPods(key, nil)
	roles, err := s.GetRoleList(key, "ms-0", "role")
	require.NoError(t, err)
	require.True(t, roles[0].HasRun)
	s.BindModelServing(key, "old")
	require.Equal(t, RoleCreating, s.GetRoleStatus(key, "ms-0", "role", "role-0"))
	s.BindModelServing(key, "new")
	require.Equal(t, RoleNotFound, s.GetRoleStatus(key, "ms-0", "role", "role-0"))
	require.Equal(t, []types.NamespacedName{key}, s.ListModelServings())
	s.DeleteModelServing(key)
	require.Empty(t, s.ListModelServings())
}
