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

package mscontroller

import (
	"testing"

	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The partition of a rollout is how many groups, or how many instances of a
// Role, stay at the baseline. Given as a percentage it is rounded up: half of
// three groups is two groups that stay, and one percent of ten is one. Rounded
// down, a canary of "all but ten percent" would update everything.
func TestAPercentagePartitionIsRoundedUp(t *testing.T) {
	groups := []struct {
		name      string
		replicas  int32
		partition *intstr.IntOrString
		want      int
	}{
		{name: "no partition is 0", replicas: 5},
		{name: "a number is taken as it is", replicas: 5, partition: ptr.To(intstr.FromInt32(3)), want: 3},
		{name: "50% of 3 is rounded up to 2", replicas: 3, partition: ptr.To(intstr.FromString("50%")), want: 2},
		{name: "50% of 4 is 2", replicas: 4, partition: ptr.To(intstr.FromString("50%")), want: 2},
		{name: "1% of 10 is rounded up to 1", replicas: 10, partition: ptr.To(intstr.FromString("1%")), want: 1},
		{name: "100% of 5 is 5", replicas: 5, partition: ptr.To(intstr.FromString("100%")), want: 5},
		{name: "0% of 5 is 0", replicas: 5, partition: ptr.To(intstr.FromString("0%"))},
	}
	for _, tt := range groups {
		t.Run("groups: "+tt.name, func(t *testing.T) {
			ms := modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To(tt.replicas) },
				rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.Partition = tt.partition }))
			spec, err := rolloutSpec(ms, "target", "baseline")
			if err != nil || spec.Partition != tt.want {
				t.Errorf("partition = %d, %v; want %d", spec.Partition, err, tt.want)
			}
		})
	}

	// Under RoleRollingUpdate the ModelServing controller needs to know one
	// thing about the partitions of the Roles: whether any instance is held
	// back. While one is, the rollout is not complete and the baseline stays.
	roles := []struct {
		name      string
		replicas  int32
		partition *intstr.IntOrString
		want      bool
	}{
		{name: "no partition holds nothing back", replicas: 3},
		{name: "0% of 3 holds nothing back", replicas: 3, partition: ptr.To(intstr.FromString("0%"))},
		{name: "10% of 3 is rounded up to 1", replicas: 3, partition: ptr.To(intstr.FromString("10%")), want: true},
		{name: "1% of 10 is rounded up to 1", replicas: 10, partition: ptr.To(intstr.FromString("1%")), want: true},
		{name: "a number", replicas: 3, partition: ptr.To(intstr.FromInt32(1)), want: true},
	}
	for _, tt := range roles {
		t.Run("instances of a Role: "+tt.name, func(t *testing.T) {
			ms := modelServing(roleRolling(func(role *workloadv1alpha1.Role) {
				role.Replicas, role.Partition = ptr.To(tt.replicas), tt.partition
			}))
			spec, err := rolloutSpec(ms, "target", "baseline")
			if err != nil || spec.RolePartitioned != tt.want {
				t.Errorf("an instance is held back = %t, %v; want %t", spec.RolePartitioned, err, tt.want)
			}
		})
	}
}

// The same, from the template to the Pods: a rollout with a partition of 50%
// leaves two of three at the image they run, groups or instances of a Role,
// and does not advance the baseline.
func TestARolloutStopsAtAPercentagePartition(t *testing.T) {
	half := ptr.To(intstr.FromString("50%"))
	tests := []struct {
		name   string
		mutate func(*workloadv1alpha1.ModelServing)
		// state renders what runs where.
		state func(*system) string
		want  string
	}{
		{
			name:   "groups",
			mutate: rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) { config.Partition = half }),
			state:  (*system).images,
			want:   "0=model:v1 1=model:v1 2=model:v2",
		},
		{
			name:   "instances of a Role",
			mutate: roleRolling(func(role *workloadv1alpha1.Role) { role.Partition = half }),
			state:  (*system).instances,
			want:   "0[0=model:v1 1=model:v1 2=model:v2] 1[0=model:v1 1=model:v1 2=model:v2]",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newSystem(t, modelServing(tt.mutate))
			s.settle()
			baseline := s.modelServing().Status.CurrentRevision

			s.update(setImage("model:v2"))
			s.settle()
			if got := tt.state(s); got != tt.want {
				t.Errorf("at the partition: %s, want %s", got, tt.want)
			}
			if status := s.modelServing().Status; status.CurrentRevision != baseline || status.UpdateRevision == baseline {
				t.Errorf("revisions at the partition: current %s, update %s, before %s; want the baseline kept", status.CurrentRevision, status.UpdateRevision, baseline)
			}
		})
	}
}
