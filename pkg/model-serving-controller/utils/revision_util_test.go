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
	"hash/fnv"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

var (
	nginxPodTemplate = workloadv1alpha1.PodTemplateSpec{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "nginx",
					Image: "nginx:1.14.2",
				},
			},
		},
	}
)
var replicas int32 = 1

func TestRevision(t *testing.T) {
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	role2 := workloadv1alpha1.Role{
		Name:           "decode",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 2,
		WorkerTemplate: &nginxPodTemplate,
	}
	role3 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}

	hash1 := Revision(role1)
	hash2 := Revision(role2)
	hash3 := Revision(role3)

	if hash1 == hash2 {
		t.Errorf("Hash should be different for different objects, got %s and %s", hash1, hash3)
	}
	if hash1 != hash3 {
		t.Errorf("Hash should be equal for identical objects, got %s and %s", hash1, hash2)
	}
}

func TestDeepHashObject(t *testing.T) {
	hasher := fnv.New32()
	role1 := workloadv1alpha1.Role{
		Name:           "prefill",
		Replicas:       &replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: 0,
		WorkerTemplate: nil,
	}
	DeepHashObject(hasher, role1)
	firstHash := hasher.Sum32()

	hasher.Reset()
	DeepHashObject(hasher, role1)
	secondHash := hasher.Sum32()

	if firstHash != secondHash {
		t.Errorf("DeepHashObject should produce the same hash for the same object, got %v and %v", firstHash, secondHash)
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

func newRole(name string, replicas *int32, workerReplicas int32) workloadv1alpha1.Role {
	return workloadv1alpha1.Role{
		Name:           name,
		Replicas:       replicas,
		EntryTemplate:  nginxPodTemplate,
		WorkerReplicas: workerReplicas,
		WorkerTemplate: nil,
	}
}

func newModelServing(roles []workloadv1alpha1.Role) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		Spec: workloadv1alpha1.ModelServingSpec{
			Template: workloadv1alpha1.ServingGroup{
				Roles: roles,
			},
		},
	}
}

func TestModelServingRevision(t *testing.T) {
	tests := []struct {
		name      string
		a         *workloadv1alpha1.ModelServing
		b         *workloadv1alpha1.ModelServing
		wantEqual bool
	}{
		{
			name:      "identical roles produce equal revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			wantEqual: true,
		},
		{
			name:      "different replicas produce equal revision (replicas ignored)",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(3), 0)}),
			wantEqual: true,
		},
		{
			name:      "nil replicas equals non-nil replicas (replicas ignored)",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", nil, 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(5), 0)}),
			wantEqual: true,
		},
		{
			name:      "different role name produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("decode", int32Ptr(1), 0)}),
			wantEqual: false,
		},
		{
			name:      "different worker replicas produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 2)}),
			wantEqual: false,
		},
		{
			name:      "different number of roles produces different revision",
			a:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0)}),
			b:         newModelServing([]workloadv1alpha1.Role{newRole("prefill", int32Ptr(1), 0), newRole("decode", int32Ptr(1), 0)}),
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := ModelServingRevision(tt.a)
			gotB := ModelServingRevision(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("ModelServingRevision() equality = %v, want %v (a=%s, b=%s)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func TestMaxSurgeDoesNotChangeRevisionOrRoleTemplateHash(t *testing.T) {
	withoutSurge := newRole("decode", int32Ptr(2), 0)
	withSurge := *withoutSurge.DeepCopy()
	withSurge.MaxUnavailable = func() *intstr.IntOrString { value := intstr.FromInt(0); return &value }()
	withSurge.MaxSurge = func() *intstr.IntOrString { value := intstr.FromString("25%"); return &value }()
	withSurge.Partition = func() *intstr.IntOrString { value := intstr.FromInt(1); return &value }()

	if CalRoleTemplateHash(withoutSurge) != CalRoleTemplateHash(withSurge) {
		t.Fatal("rolling update policy must not change Role template hash")
	}
	if ModelServingRevision(newModelServing([]workloadv1alpha1.Role{withoutSurge})) !=
		ModelServingRevision(newModelServing([]workloadv1alpha1.Role{withSurge})) {
		t.Fatal("rolling update policy must not change ModelServing revision")
	}

	baseModelServing := newModelServing([]workloadv1alpha1.Role{withoutSurge})
	withGroupPolicy := baseModelServing.DeepCopy()
	withGroupPolicy.Spec.Replicas = int32Ptr(7)
	withGroupPolicy.Spec.RolloutStrategy = &workloadv1alpha1.RolloutStrategy{
		Type: workloadv1alpha1.ServingGroupRollingUpdate,
		RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
			MaxUnavailable: withSurge.MaxUnavailable,
			MaxSurge:       withSurge.MaxSurge,
			Partition:      withSurge.Partition,
		},
	}
	if ModelServingRevision(baseModelServing) != ModelServingRevision(withGroupPolicy) {
		t.Fatal("ServingGroup replica and rolling-update policy must not change ModelServing revision")
	}
}

func TestModelServingRevisionDoesNotMutateInput(t *testing.T) {
	replicaVal := int32(3)
	ms := newModelServing([]workloadv1alpha1.Role{
		newRole("prefill", &replicaVal, 0),
		newRole("decode", &replicaVal, 2),
	})

	ModelServingRevision(ms)

	for i, role := range ms.Spec.Template.Roles {
		if role.Replicas == nil {
			t.Errorf("role[%d].Replicas was mutated to nil, expected it to be preserved", i)
			continue
		}
		if *role.Replicas != replicaVal {
			t.Errorf("role[%d].Replicas = %d, want %d", i, *role.Replicas, replicaVal)
		}
	}
}

func TestCalRoleTemplateHash(t *testing.T) {
	tests := []struct {
		name      string
		a         workloadv1alpha1.Role
		b         workloadv1alpha1.Role
		wantEqual bool
	}{
		{
			name:      "identical roles produce equal hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(1), 0),
			wantEqual: true,
		},
		{
			name:      "different replicas produce equal hash (replicas ignored)",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(4), 0),
			wantEqual: true,
		},
		{
			name:      "nil replicas equals non-nil replicas (replicas ignored)",
			a:         newRole("prefill", nil, 0),
			b:         newRole("prefill", int32Ptr(2), 0),
			wantEqual: true,
		},
		{
			name:      "different role name produces different hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("decode", int32Ptr(1), 0),
			wantEqual: false,
		},
		{
			name:      "different worker replicas produces different hash",
			a:         newRole("prefill", int32Ptr(1), 0),
			b:         newRole("prefill", int32Ptr(1), 3),
			wantEqual: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotA := CalRoleTemplateHash(tt.a)
			gotB := CalRoleTemplateHash(tt.b)
			if (gotA == gotB) != tt.wantEqual {
				t.Errorf("CalRoleTemplateHash() equality = %v, want %v (a=%s, b=%s)", gotA == gotB, tt.wantEqual, gotA, gotB)
			}
		})
	}
}

func TestCalRoleTemplateHashDoesNotMutateInput(t *testing.T) {
	replicaVal := int32(7)
	role := newRole("prefill", &replicaVal, 0)

	CalRoleTemplateHash(role)

	if role.Replicas == nil {
		t.Fatal("role.Replicas was mutated to nil, expected it to be preserved")
	}
	if *role.Replicas != replicaVal {
		t.Errorf("role.Replicas = %d, want %d", *role.Replicas, replicaVal)
	}
}

func TestEqualRoleTemplatesForRevision(t *testing.T) {
	base := []workloadv1alpha1.Role{
		newRole("prefill", int32Ptr(1), 0),
		newRole("decode", int32Ptr(2), 1),
	}
	operationalChange := make([]workloadv1alpha1.Role, len(base))
	for i := range base {
		operationalChange[i] = *base[i].DeepCopy()
	}
	operationalChange[0].Replicas = int32Ptr(4)
	maxUnavailable := intstr.FromInt(0)
	maxSurge := intstr.FromString("25%")
	partition := intstr.FromInt(1)
	operationalChange[0].MaxUnavailable = &maxUnavailable
	operationalChange[0].MaxSurge = &maxSurge
	operationalChange[0].Partition = &partition
	if !EqualRoleTemplatesForRevision(base, operationalChange) {
		t.Fatal("replica and rolling-update policy changes must be revision-equivalent")
	}

	templateChange := make([]workloadv1alpha1.Role, len(base))
	for i := range base {
		templateChange[i] = *base[i].DeepCopy()
	}
	templateChange[0].EntryTemplate.Spec.Containers[0].Image = "nginx:changed"
	if EqualRoleTemplatesForRevision(base, templateChange) {
		t.Fatal("an entry Pod template change must not be revision-equivalent")
	}

	topologyChange := make([]workloadv1alpha1.Role, len(base))
	for i := range base {
		topologyChange[i] = *base[i].DeepCopy()
	}
	topologyChange[1].WorkerReplicas++
	if EqualRoleTemplatesForRevision(base, topologyChange) {
		t.Fatal("a worker topology change must not be revision-equivalent")
	}
}
