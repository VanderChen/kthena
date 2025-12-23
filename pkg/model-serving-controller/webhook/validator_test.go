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

package webhook

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins/ranktable"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/client-go/kubernetes/fake"
)

func TestValidPodNameLength(t *testing.T) {
	replicas := int32(3)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal name length",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					ObjectMeta: v1.ObjectMeta{
						Name: "valid-name",
					},
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "name length exceeds limit",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					ObjectMeta: v1.ObjectMeta{
						Name: "this-is-a-very-long-name-that-exceeds-the-allowed-length-for-generated-name",
					},
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("metadata").Child("name"),
					"this-is-a-very-long-name-that-exceeds-the-allowed-length-for-generated-name",
					"invalid name: must be no more than 63 characters"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validGeneratedNameLength(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want[0], got[0])
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateRollingUpdateConfiguration(t *testing.T) {
	replicas := int32(3)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal rolling update configuration",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid maxUnavailable format",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.String,
									StrVal: "invalid",
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxUnavailable"),
					&intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"a valid percent string must be a numeric string followed by an ending '%' (e.g. '1%',  or '93%', regex used for validation is '[0-9]+%')",
				),
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("maxUnavailable"),
					&intstr.IntOrString{
						Type:   intstr.String,
						StrVal: "invalid",
					},
					"invalidate maxUnavailable",
				),
			},
		},
		{
			name: "both maxUnavailable and maxSurge are zero",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 0,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration"),
					"",
					"maxUnavailable cannot be 0",
				),
			},
		},
		{
			name: "valid partition - within range",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(1),
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid partition - negative value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(-1),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(-1),
					"partition must be greater than or equal to 0",
				),
			},
		},
		{
			name: "invalid partition - equal to replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(3),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(3),
					"partition must be less than replicas (3)",
				),
			},
		},
		{
			name: "invalid partition - greater than replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(5),
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration").Child("partition"),
					int32(5),
					"partition must be less than replicas (3)",
				),
			},
		},
		{
			name: "valid partition - zero value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						RolloutStrategy: &workloadv1alpha1.RolloutStrategy{
							RollingUpdateConfiguration: &workloadv1alpha1.RollingUpdateConfiguration{
								MaxUnavailable: &intstr.IntOrString{
									Type:   intstr.Int,
									IntVal: 1,
								},
								Partition: int32Ptr(0),
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateRollingUpdateConfiguration(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidatorReplicas(t *testing.T) {
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "normal replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "replicas is nil",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32PtrNil(),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("replicas"),
					int32PtrNil(),
					"replicas must be a positive integer",
				),
			},
		},
		{
			name: "replicas is less than 0",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(-1),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(2), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("replicas"),
					int32Ptr(-1),
					"replicas must be a positive integer",
				),
			},
		},
		{
			name: "role replicas is less than 0",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32Ptr(-1), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("replicas"),
					int32Ptr(-1),
					"role replicas must be a positive integer",
				),
			},
		},
		{
			name: "role replicas is nil",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role1", Replicas: int32PtrNil(), WorkerReplicas: 1},
								{Name: "role2", Replicas: int32Ptr(1), WorkerReplicas: 1},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("replicas"),
					int32PtrNil(),
					"role replicas must be a positive integer",
				),
			},
		},
		{
			name: "no role",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: int32Ptr(3),
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles"),
					[]workloadv1alpha1.Role{},
					"roles must be specified",
				),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validatorReplicas(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateGangPolicy(t *testing.T) {
	replicas := int32(3)
	roleReplicas := int32(2)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "valid minRoleReplicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": 2, // 2 (role replicas) >= 2 (min), valid
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid minRoleReplicas - role not exist",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"nonexistent": 1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("nonexistent"),
					"nonexistent",
					"role nonexistent does not exist in template.roles",
				),
			},
		},
		{
			name: "invalid minRoleReplicas - exceeds role replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": 10, // exceeds replicas 2
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("worker"),
					int32(10),
					"minRoleReplicas (10) for role worker cannot exceed replicas (2)",
				),
			},
		},
		{
			name: "invalid minRoleReplicas - negative value",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: map[string]int32{
									"worker": -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas").Key("worker"),
					int32(-1),
					"minRoleReplicas for role worker must be non-negative",
				),
			},
		},
		{
			name: "nil gang Policy",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: nil,
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "nil minRoleReplicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
							GangPolicy: &workloadv1alpha1.GangPolicy{
								MinRoleReplicas: nil,
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateGangPolicy(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateWorkerReplicas(t *testing.T) {
	replicas := int32(3)
	roleReplicas := int32(2)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "valid worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "valid zero worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: 0,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid negative worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker",
									Replicas:       &roleReplicas,
									WorkerReplicas: -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("workerReplicas"),
					int32(-1),
					"workerReplicas must be a non-negative integer",
				),
			},
		},
		{
			name: "multiple roles with one invalid worker replicas",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{
									Name:           "worker1",
									Replicas:       &roleReplicas,
									WorkerReplicas: 3,
								},
								{
									Name:           "worker2",
									Replicas:       &roleReplicas,
									WorkerReplicas: -1,
								},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(1).Child("workerReplicas"),
					int32(-1),
					"workerReplicas must be a non-negative integer",
				),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateWorkerReplicas(tt.args.ms)
			if got != nil {
				assert.EqualValues(t, tt.want, got)
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func TestValidateRoleNames(t *testing.T) {
	replicas := int32(3)
	type args struct {
		ms *workloadv1alpha1.ModelServing
	}
	tests := []struct {
		name string
		args args
		want field.ErrorList
	}{
		{
			name: "valid lowercase role name",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "prefill", Replicas: &replicas, WorkerReplicas: 2},
								{Name: "decode", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList(nil),
		},
		{
			name: "invalid uppercase role name",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "Prefill", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("name"),
					"Prefill",
					"role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character",
				),
			},
		},
		{
			name: "invalid role name starting with number",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "1role", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("name"),
					"1role",
					"role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character",
				),
			},
		},
		{
			name: "invalid role name ending with hyphen",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "role-", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(0).Child("name"),
					"role-",
					"role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character",
				),
			},
		},
		{
			name: "multiple roles with one invalid",
			args: args{
				ms: &workloadv1alpha1.ModelServing{
					Spec: workloadv1alpha1.ModelServingSpec{
						Replicas: &replicas,
						Template: workloadv1alpha1.ServingGroup{
							Roles: []workloadv1alpha1.Role{
								{Name: "prefill", Replicas: &replicas, WorkerReplicas: 2},
								{Name: "Decode", Replicas: &replicas, WorkerReplicas: 2},
							},
						},
					},
				},
			},
			want: field.ErrorList{
				field.Invalid(
					field.NewPath("spec").Child("template").Child("roles").Index(1).Child("name"),
					"Decode",
					"role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): a DNS-1035 label must consist of lower case alphanumeric characters or '-', start with an alphabetic character, and end with an alphanumeric character",
				),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateRoleNames(tt.args.ms)
			if len(got) > 0 {
				// Check that we got an error for the expected field
				assert.Equal(t, len(tt.want), len(got), "error count mismatch")
				if len(tt.want) > 0 && len(got) > 0 {
					assert.Equal(t, tt.want[0].Field, got[0].Field, "field path mismatch")
					assert.Equal(t, tt.want[0].BadValue, got[0].BadValue, "bad value mismatch")
					// Check that error message contains the expected text
					assert.Contains(t, got[0].Detail, "role name must be a valid DNS-1035 label", "error message should mention DNS-1035")
				}
			} else {
				assert.Equal(t, tt.want, got)
			}
		})
	}
}

func int32Ptr(i int32) *int32 {
	return &i
}

func int32PtrNil() *int32 {
	return nil
}

func TestValidateRanktablePlugin(t *testing.T) {
	// Setup fake client
	kubeClient := fake.NewSimpleClientset()
	v := NewModelServingValidator(kubeClient)

	// Create a valid template ConfigMap in default namespace
	templateName := "valid-template"
	_, err := kubeClient.CoreV1().ConfigMaps("default").Create(context.Background(), &corev1.ConfigMap{
		ObjectMeta: v1.ObjectMeta{
			Name:      templateName,
			Namespace: "default",
		},
	}, v1.CreateOptions{})
	assert.NoError(t, err)

	// Helper to create JSON config
	createConfig := func(template string) *apiextensionsv1.JSON {
		cfg := ranktable.RanktableConfig{
			Template: template,
		}
		bytes, _ := json.Marshal(cfg)
		return &apiextensionsv1.JSON{Raw: bytes}
	}

	tests := []struct {
		name          string
		ms            *workloadv1alpha1.ModelServing
		setupEnv      func()
		teardownEnv   func()
		expectedError bool
		errorMsg      string
	}{
		{
			name: "valid ranktable plugin config",
			ms: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Plugins: []workloadv1alpha1.PluginSpec{
						{
							Name:   ranktable.PluginName,
							Config: createConfig(templateName),
						},
					},
				},
			},
			expectedError: false,
		},
		{
			name: "missing template config",
			ms: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Plugins: []workloadv1alpha1.PluginSpec{
						{
							Name:   ranktable.PluginName,
							Config: createConfig(""),
						},
					},
				},
			},
			expectedError: true,
			errorMsg:      "ranktable template is required",
		},
		{
			name: "non-existent template configmap",
			ms: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Plugins: []workloadv1alpha1.PluginSpec{
						{
							Name:   ranktable.PluginName,
							Config: createConfig("non-existent-template"),
						},
					},
				},
			},
			expectedError: true,
			errorMsg:      "ranktable template ConfigMap 'non-existent-template' not found",
		},
		{
			name: "valid template in custom namespace",
			ms: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Plugins: []workloadv1alpha1.PluginSpec{
						{
							Name:   ranktable.PluginName,
							Config: createConfig("custom-template"),
						},
					},
				},
			},
			setupEnv: func() {
				os.Setenv("POD_NAMESPACE", "custom-ns")
				_, _ = kubeClient.CoreV1().ConfigMaps("custom-ns").Create(context.Background(), &corev1.ConfigMap{
					ObjectMeta: v1.ObjectMeta{
						Name:      "custom-template",
						Namespace: "custom-ns",
					},
				}, v1.CreateOptions{})
			},
			teardownEnv: func() {
				os.Unsetenv("POD_NAMESPACE")
				_ = kubeClient.CoreV1().ConfigMaps("custom-ns").Delete(context.Background(), "custom-template", v1.DeleteOptions{})
			},
			expectedError: false,
		},
		{
			name: "missing template in custom namespace",
			ms: &workloadv1alpha1.ModelServing{
				Spec: workloadv1alpha1.ModelServingSpec{
					Plugins: []workloadv1alpha1.PluginSpec{
						{
							Name:   ranktable.PluginName,
							Config: createConfig("missing-custom-template"),
						},
					},
				},
			},
			setupEnv: func() {
				os.Setenv("POD_NAMESPACE", "custom-ns")
			},
			teardownEnv: func() {
				os.Unsetenv("POD_NAMESPACE")
			},
			expectedError: true,
			errorMsg:      "ranktable template ConfigMap 'missing-custom-template' not found in namespace 'custom-ns'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.setupEnv != nil {
				tt.setupEnv()
			}
			if tt.teardownEnv != nil {
				defer tt.teardownEnv()
			}

			errs := v.validateRanktablePlugin(context.Background(), tt.ms)
			if tt.expectedError {
				assert.NotEmpty(t, errs)
				found := false
				for _, err := range errs {
					if err.Detail != "" && contains(err.Detail, tt.errorMsg) {
						found = true
						break
					}
				}
				// If detail check failed, check string representation or fallback
				if !found {
					// re-check roughly
					for _, err := range errs {
						if contains(err.Error(), tt.errorMsg) {
							found = true
							break
						}
					}
				}
				assert.True(t, found, "Expected error message '%s' not found in %v", tt.errorMsg, errs)
			} else {
				assert.Empty(t, errs)
			}
		})
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && s[0:len(substr)] == substr || (len(s) > len(substr) && contains(s[1:], substr))
}
