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
	"bytes"
	"encoding/json"
	"math"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	volcanov1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workload "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// Exercise the admission handler so update-only rules cannot accidentally be
// omitted from the endpoint used by the API server. CRD defaults are covered by
// the separate API-server verification; these objects carry explicit replicas.
func admitContract(t *testing.T, oldMS, ms *workload.ModelServing, wantField string) {
	t.Helper()
	raw, err := json.Marshal(ms)
	require.NoError(t, err)
	request := &admissionv1.AdmissionRequest{UID: "contract", Operation: admissionv1.Create, Object: runtime.RawExtension{Raw: raw}}
	if oldMS != nil {
		request.Operation = admissionv1.Update
		raw, err = json.Marshal(oldMS)
		require.NoError(t, err)
		request.OldObject = runtime.RawExtension{Raw: raw}
	}
	raw, err = json.Marshal(admissionv1.AdmissionReview{TypeMeta: metav1.TypeMeta{APIVersion: "admission.k8s.io/v1", Kind: "AdmissionReview"}, Request: request})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/validate-modelserving", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	NewModelServingValidator(nil).Handle(recorder, req)
	require.Equal(t, 200, recorder.Code, recorder.Body.String())
	var response admissionv1.AdmissionReview
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
	require.NotNil(t, response.Response)
	require.Equal(t, request.UID, response.Response.UID)
	require.Equal(t, wantField == "", response.Response.Allowed, recorder.Body.String())
	if wantField != "" {
		require.Contains(t, response.Response.Result.Message, wantField)
	}
}

func TestAdmissionBudgetContract(t *testing.T) {
	integer := func(n int32) *intstr.IntOrString { return ptr.To(intstr.FromInt32(n)) }
	percent := func(s string) *intstr.IntOrString { return ptr.To(intstr.FromString(s)) }
	cases := []struct {
		name                          string
		replicas                      int32
		unavailable, surge, partition *intstr.IntOrString
		invalid                       bool
	}{
		{name: "defaults", replicas: 3},
		{name: "zero replicas with omitted budget", replicas: 0},
		{name: "zero replicas with API default", replicas: 0, unavailable: integer(1)},
		{name: "zero replicas rejects two unavailable", replicas: 0, unavailable: integer(2), invalid: true},
		{name: "zero replicas zero unavailable and omitted surge", replicas: 0, unavailable: integer(0), invalid: true},
		{name: "zero replicas percent unavailable and omitted surge", replicas: 0, unavailable: percent("25%"), invalid: true},
		{name: "zero replicas omitted unavailable and percent surge", replicas: 0, surge: percent("25%")},
		{name: "percentage rounds down using three replicas", replicas: 3, unavailable: percent("33%"), surge: integer(0), invalid: true},
		{name: "percentage rounds to one using three replicas", replicas: 3, unavailable: percent("34%"), surge: integer(0)},
		{name: "percentage rounds to one using four replicas", replicas: 4, unavailable: percent("25%"), surge: integer(0)},
		{name: "positive surge rounds up using replicas", replicas: 3, unavailable: percent("25%"), surge: percent("1%")},
		{name: "full partition omitted surge", replicas: 3, unavailable: integer(0), partition: integer(3), invalid: true},
		{name: "zero replicas integer double zero", replicas: 0, unavailable: integer(0), surge: integer(0), invalid: true},
		{name: "zero replicas percent double zero", replicas: 0, unavailable: percent("0%"), surge: percent("00%"), invalid: true},
		{name: "zero replicas mixed double zero", replicas: 0, unavailable: integer(0), surge: percent("0%"), invalid: true},
		{name: "zero replicas positive percentages", replicas: 0, unavailable: percent("25%"), surge: percent("25%"), invalid: true},
		{name: "full partition double zero", replicas: 3, unavailable: integer(0), surge: integer(0), partition: integer(3), invalid: true},
		{name: "full partition rounded zero", replicas: 3, unavailable: percent("1%"), surge: integer(0), partition: integer(3), invalid: true},
		{name: "partition equals replicas", replicas: 3, partition: integer(3)},
		{name: "partition exceeds replicas", replicas: 3, partition: integer(4), invalid: true},
		{name: "zero replicas positive partition", replicas: 0, partition: integer(1), invalid: true},
		{name: "unavailable equals replicas", replicas: 3, unavailable: integer(3)},
		{name: "unavailable exceeds replicas", replicas: 3, unavailable: integer(4), invalid: true},
		{name: "surge exceeds replicas", replicas: 3, surge: integer(8)},
		{name: "surge 101 percent", replicas: 3, surge: percent("101%")},
		{name: "surge 250 percent", replicas: 3, surge: percent("250%")},
		{name: "surge integer overflow", replicas: 3, surge: integer(math.MaxInt32), invalid: true},
		{name: "surge percent overflow", replicas: 3, surge: percent("9223372036854775807%"), invalid: true},
		{name: "surge percent parse overflow", replicas: 3, surge: percent("92233720368547758070%"), invalid: true},
		{name: "unavailable percent above 100", replicas: 3, unavailable: percent("101%"), invalid: true},
		{name: "partition percent above 100", replicas: 3, partition: percent("101%"), invalid: true},
		{name: "surge fractional percent", replicas: 3, surge: percent("1.5%"), invalid: true},
		{name: "surge numeric string", replicas: 3, surge: percent("1"), invalid: true},
		{name: "negative unavailable", replicas: 3, unavailable: integer(-1), invalid: true},
		{name: "negative surge", replicas: 3, surge: integer(-1), invalid: true},
		{name: "negative partition", replicas: 3, partition: integer(-1), invalid: true},
	}
	for _, mode := range []workload.RolloutStrategyType{workload.ServingGroupRollingUpdate, workload.RoleRollingUpdate} {
		for _, tc := range cases {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				ms := modelServingWithValidPodTemplate()
				ms.Spec.RecoveryPolicy = workload.RoleRecreate
				ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
				config := workload.RollingUpdateConfiguration{MaxUnavailable: tc.unavailable, MaxSurge: tc.surge, Partition: tc.partition}
				field := "spec.rolloutStrategy.rollingUpdateConfiguration"
				if mode == workload.RoleRollingUpdate {
					ms.Spec.Template.Roles[0].Replicas = ptr.To(tc.replicas)
					ms.Spec.Template.Roles[0].RollingUpdateConfiguration = config
					field = "spec.template.roles[0]"
				} else {
					ms.Spec.Replicas = ptr.To(tc.replicas)
					ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &config
				}
				if !tc.invalid {
					field = ""
				}
				admitContract(t, nil, ms, field)
				admitContract(t, ms.DeepCopy(), ms, field)
			})
		}
		t.Run(string(mode)+"/inactive budgets", func(t *testing.T) {
			ms := modelServingWithValidPodTemplate()
			ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
			inactive := &ms.Spec.Template.Roles[0].RollingUpdateConfiguration
			if mode == workload.RoleRollingUpdate {
				ms.Spec.RolloutStrategy.RollingUpdateConfiguration = &workload.RollingUpdateConfiguration{}
				inactive = ms.Spec.RolloutStrategy.RollingUpdateConfiguration
			}
			// Both combinations would be invalid at the active granularity.
			inactive.MaxUnavailable, inactive.MaxSurge = integer(0), integer(0)
			inactive.Partition = integer(100)
			admitContract(t, nil, ms, "")
			inactive.MaxUnavailable = integer(100)
			admitContract(t, nil, ms, "")
			// Inactive fields still have to be well-formed.
			inactive.MaxUnavailable = percent("invalid")
			admitContract(t, nil, ms, "maxUnavailable")
		})
		t.Run(string(mode)+"/scale down makes percentages both zero", func(t *testing.T) {
			old := modelServingWithValidPodTemplate()
			old.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
			config := workload.RollingUpdateConfiguration{MaxUnavailable: percent("25%"), MaxSurge: integer(0)}
			if mode == workload.RoleRollingUpdate {
				old.Spec.Template.Roles[0].Replicas = ptr.To[int32](4)
				old.Spec.Template.Roles[0].RollingUpdateConfiguration = config
			} else {
				old.Spec.Replicas = ptr.To[int32](4)
				old.Spec.RolloutStrategy.RollingUpdateConfiguration = &config
			}
			admitContract(t, nil, old, "")
			ms := old.DeepCopy()
			if mode == workload.RoleRollingUpdate {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
			} else {
				ms.Spec.Replicas = ptr.To[int32](3)
			}
			admitContract(t, old, ms, "maxUnavailable and maxSurge cannot both resolve to 0")
		})
		t.Run(string(mode)+"/scale down bounds", func(t *testing.T) {
			old := modelServingWithValidPodTemplate()
			old.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
			config := workload.RollingUpdateConfiguration{MaxUnavailable: integer(3), Partition: integer(3)}
			if mode == workload.RoleRollingUpdate {
				old.Spec.Template.Roles[0].Replicas = ptr.To[int32](3)
				old.Spec.Template.Roles[0].RollingUpdateConfiguration = config
			} else {
				old.Spec.Replicas = ptr.To[int32](3)
				old.Spec.RolloutStrategy.RollingUpdateConfiguration = &config
			}
			admitContract(t, nil, old, "")
			ms := old.DeepCopy()
			if mode == workload.RoleRollingUpdate {
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
			} else {
				ms.Spec.Replicas = ptr.To[int32](2)
			}
			admitContract(t, old, ms, "partition")
		})
	}
}

func TestAdmissionImmutableContract(t *testing.T) {
	t.Run("gang policy transitions", func(t *testing.T) {
		policies := []*workload.GangPolicy{nil, {}, {MinRoleReplicas: map[string]int32{"inference": 1}}}
		for i, before := range policies {
			for j, after := range policies {
				old := modelServingWithValidPodTemplate()
				old.Spec.Template.GangPolicy = before
				ms := old.DeepCopy()
				ms.Spec.Template.GangPolicy = after
				field := ""
				if i != j {
					field = "gangPolicy"
				}
				admitContract(t, old, ms, field)
			}
		}
		old := modelServingWithValidPodTemplate()
		old.Spec.Template.GangPolicy = &workload.GangPolicy{}
		ms := old.DeepCopy()
		ms.Spec.Template.GangPolicy.MinRoleReplicas = map[string]int32{}
		require.NotEmpty(t, validateGangPolicyImmutable(old, ms), "an explicitly empty map differs from an absent map")
	})
	t.Run("role names", func(t *testing.T) {
		old := modelServingWithValidPodTemplate()
		second := *old.Spec.Template.Roles[0].DeepCopy()
		second.Name = "decode"
		old.Spec.Template.Roles = append(old.Spec.Template.Roles, second)
		for _, zero := range []bool{false, true} {
			if zero {
				old.Spec.Replicas = ptr.To[int32](0)
			}
			ms := old.DeepCopy()
			slices.Reverse(ms.Spec.Template.Roles)
			ms.Spec.Template.Roles[0].EntryTemplate.Spec.Containers[0].Image = "nginx:1.28"
			admitContract(t, old, ms, "")
			ms.Spec.Template.Roles[0].Name = "newname"
			admitContract(t, old, ms, "spec.template.roles")
			ms = old.DeepCopy()
			ms.Spec.Template.Roles = ms.Spec.Template.Roles[:1]
			admitContract(t, old, ms, "spec.template.roles")
			admitContract(t, ms, old, "spec.template.roles")
		}
	})
	t.Run("coordination presence and values", func(t *testing.T) {
		old := modelServingWithValidPodTemplate()
		second := *old.Spec.Template.Roles[0].DeepCopy()
		second.Name = "decode"
		old.Spec.Template.Roles = append(old.Spec.Template.Roles, second)
		old.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate}
		ms := old.DeepCopy()
		ms.Spec.RolloutStrategy.RoleCoordination = &workload.RoleCoordination{MaxSkew: ptr.To(intstr.FromString("100%"))}
		admitContract(t, old, ms, "roleCoordination")
		admitContract(t, ms, old, "roleCoordination")
		old = ms.DeepCopy()
		admitContract(t, old, ms, "")
		ms.Spec.RolloutStrategy.RoleCoordination.MaxSkew = ptr.To(intstr.FromString("50%"))
		admitContract(t, old, ms, "roleCoordination")
	})
	t.Run("coordination set ordering", func(t *testing.T) {
		old := modelServingWithValidPodTemplate()
		old.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: workload.RoleRollingUpdate, RoleCoordination: &workload.RoleCoordination{
			MaxSkew: ptr.To(intstr.FromString("25%")), Roles: []string{"a", "b", "c"},
			Dependencies: []workload.RoleRolloutDependency{{Role: "a", DependsOn: []string{"b", "c"}}, {Role: "b", DependsOn: []string{"c"}}},
		}}
		ms := old.DeepCopy()
		coord := ms.Spec.RolloutStrategy.RoleCoordination
		slices.Reverse(coord.Roles)
		slices.Reverse(coord.Dependencies[0].DependsOn)
		slices.Reverse(coord.Dependencies)
		original := ms.DeepCopy()
		require.Empty(t, validateRoleCoordinationImmutable(old, ms))
		require.Equal(t, original, ms, "validation must not mutate requests")
		coord.Dependencies[0].DependsOn = nil
		require.NotEmpty(t, validateRoleCoordinationImmutable(old, ms))
	})
}

func TestAdmissionTopologyContract(t *testing.T) {
	for _, rolePolicy := range []bool{false, true} {
		for _, mode := range []volcanov1beta1.NetworkTopologyMode{"", volcanov1beta1.HardNetworkTopologyMode, volcanov1beta1.SoftNetworkTopologyMode} {
			for selectors := 0; selectors < 4; selectors++ {
				ms := modelServingWithValidPodTemplate()
				ms.Spec.SchedulerName = "volcano"
				policy := &volcanov1beta1.NetworkTopologySpec{Mode: mode}
				if selectors&1 != 0 {
					policy.HighestTierAllowed = ptr.To(0)
				}
				if selectors&2 != 0 {
					policy.HighestTierName = "rack"
				}
				ms.Spec.Template.NetworkTopology = &workload.NetworkTopology{GroupPolicy: policy}
				if rolePolicy {
					ms.Spec.Template.NetworkTopology = &workload.NetworkTopology{RolePolicy: policy}
				}
				field := ""
				if selectors == 3 || selectors == 0 && mode != volcanov1beta1.SoftNetworkTopologyMode {
					field = "networkTopology"
				}
				admitContract(t, nil, ms, field)
				admitContract(t, ms.DeepCopy(), ms, field)
			}
		}
	}
	for _, kind := range []string{"groupAnti", "roleAffinity", "roleAnti"} {
		for _, required := range []bool{true, false} {
			for selectors := 0; selectors < 4; selectors++ {
				ms := modelServingWithValidPodTemplate()
				ms.Spec.SchedulerName = "volcano"
				ms.Spec.Template.Roles[0].Replicas = ptr.To[int32](2)
				second := *ms.Spec.Template.Roles[0].DeepCopy()
				second.Name = "decode"
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
				var weight, tier *int32
				name := ""
				if !required {
					weight = ptr.To[int32](50)
				}
				if selectors&1 != 0 {
					tier = ptr.To[int32](0)
				}
				if selectors&2 != 0 {
					name = "rack"
				}
				topology := &workload.NetworkTopology{}
				groupTerm := workload.ServingGroupAffinityTerm{Weight: weight, TopologyTier: tier, TopologyTierName: name}
				roleTerm := workload.RoleAffinityTerm{Roles: []string{"inference", "decode"}, Weight: weight, TopologyTier: tier, TopologyTierName: name}
				switch kind {
				case "groupAnti":
					a := &workload.ServingGroupAntiAffinity{}
					if required {
						a.Required = []workload.ServingGroupAffinityTerm{groupTerm}
					} else {
						a.Preferred = []workload.ServingGroupAffinityTerm{groupTerm}
					}
					topology.ServingGroupAntiAffinity = a
				case "roleAffinity":
					a := &workload.RoleAffinity{}
					if required {
						a.Required = []workload.RoleAffinityTerm{roleTerm}
					} else {
						a.Preferred = []workload.RoleAffinityTerm{roleTerm}
					}
					topology.RoleAffinity = a
				case "roleAnti":
					a := &workload.RoleAntiAffinity{}
					if required {
						a.Required = []workload.RoleAffinityTerm{roleTerm}
					} else {
						a.Preferred = []workload.RoleAffinityTerm{roleTerm}
					}
					topology.RoleAntiAffinity = a
				}
				ms.Spec.Template.NetworkTopology = topology
				field := ""
				if selectors == 3 || selectors == 0 {
					field = "networkTopology"
				}
				admitContract(t, nil, ms, field)
				admitContract(t, ms.DeepCopy(), ms, field)
			}
		}
	}
}

func TestAdmissionRecoveryContract(t *testing.T) {
	for _, mode := range []workload.RolloutStrategyType{workload.ServingGroupRollingUpdate, workload.RoleRollingUpdate} {
		for _, recovery := range []workload.RecoveryPolicy{workload.ServingGroupRecreate, workload.RoleRecreate, workload.NoneRestartPolicy} {
			ms := modelServingWithValidPodTemplate()
			ms.Spec.RolloutStrategy = &workload.RolloutStrategy{Type: mode}
			ms.Spec.RecoveryPolicy = recovery
			field := ""
			if mode == workload.RoleRollingUpdate && recovery == workload.ServingGroupRecreate {
				field = "rolloutStrategy.type"
			}
			admitContract(t, nil, ms, field)
			admitContract(t, ms.DeepCopy(), ms, field)
		}
	}
}

// Range checks must reject invalid input even when the handler is called without
// CRD schema validation. Exercise both admission operations and valid boundaries.
func TestAdmissionRangeContract(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(*workload.ModelServing)
		wantField string
	}{
		{"revision history zero", func(ms *workload.ModelServing) { ms.Spec.RevisionHistoryLimit = ptr.To[int32](0) }, ""},
		{"revision history negative", func(ms *workload.ModelServing) { ms.Spec.RevisionHistoryLimit = ptr.To[int32](-1) }, "spec.revisionHistoryLimit"},
		{"role name twelve characters", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Name = strings.Repeat("a", 12) }, ""},
		{"role name thirteen characters", func(ms *workload.ModelServing) { ms.Spec.Template.Roles[0].Name = strings.Repeat("a", 13) }, "spec.template.roles[0].name"},
		{"empty roles", func(ms *workload.ModelServing) { ms.Spec.Template.Roles = nil }, "spec.template.roles"},
		{"four roles", func(ms *workload.ModelServing) {
			for _, name := range []string{"second", "third", "fourth"} {
				role := *ms.Spec.Template.Roles[0].DeepCopy()
				role.Name = name
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, role)
			}
		}, ""},
		{"five roles", func(ms *workload.ModelServing) {
			for _, name := range []string{"second", "third", "fourth", "fifth"} {
				role := *ms.Spec.Template.Roles[0].DeepCopy()
				role.Name = name
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, role)
			}
		}, "spec.template.roles"},
		{"duplicate role names", func(ms *workload.ModelServing) {
			ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, *ms.Spec.Template.Roles[0].DeepCopy())
		}, "spec.template.roles[1].name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := modelServingWithValidPodTemplate()
			tc.mutate(ms)
			admitContract(t, nil, ms, tc.wantField)
			admitContract(t, ms.DeepCopy(), ms, tc.wantField)
		})
	}
	for _, boundary := range []string{"groupPolicy", "rolePolicy"} {
		for _, tc := range []struct {
			name      string
			policy    volcanov1beta1.NetworkTopologySpec
			wantField string
		}{
			{"zero tier", volcanov1beta1.NetworkTopologySpec{HighestTierAllowed: ptr.To(0)}, ""},
			{"negative tier", volcanov1beta1.NetworkTopologySpec{HighestTierAllowed: ptr.To(-1)}, "highestTierAllowed"},
			{"253 characters", volcanov1beta1.NetworkTopologySpec{HighestTierName: strings.Repeat("界", 253)}, ""},
			{"254 characters", volcanov1beta1.NetworkTopologySpec{HighestTierName: strings.Repeat("界", 254)}, "highestTierName"},
		} {
			t.Run(boundary+"/"+tc.name, func(t *testing.T) {
				ms := modelServingWithValidPodTemplate()
				ms.Spec.Template.NetworkTopology = &workload.NetworkTopology{}
				if boundary == "groupPolicy" {
					ms.Spec.Template.NetworkTopology.GroupPolicy = &tc.policy
				} else {
					ms.Spec.Template.NetworkTopology.RolePolicy = &tc.policy
				}
				admitContract(t, nil, ms, tc.wantField)
				admitContract(t, ms.DeepCopy(), ms, tc.wantField)
			})
		}
	}
	for _, kind := range []string{"servingGroupAntiAffinity", "roleAffinity", "roleAntiAffinity"} {
		for _, tc := range []struct {
			name      string
			term      workload.ServingGroupAffinityTerm
			wantField string
		}{
			{"zero tier", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](1), TopologyTier: ptr.To[int32](0)}, ""},
			{"negative tier", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](1), TopologyTier: ptr.To[int32](-1)}, "topologyTier"},
			{"253 characters", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](100), TopologyTierName: strings.Repeat("界", 253)}, ""},
			{"254 characters", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](100), TopologyTierName: strings.Repeat("界", 254)}, "topologyTierName"},
			{"zero weight", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](0), TopologyTier: ptr.To[int32](0)}, "weight"},
			{"excessive weight", workload.ServingGroupAffinityTerm{Weight: ptr.To[int32](101), TopologyTier: ptr.To[int32](0)}, "weight"},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				ms := modelServingWithValidPodTemplate()
				ms.Spec.SchedulerName = "volcano"
				second := *ms.Spec.Template.Roles[0].DeepCopy()
				second.Name = "second"
				ms.Spec.Template.Roles = append(ms.Spec.Template.Roles, second)
				topology := &workload.NetworkTopology{}
				roleTerm := workload.RoleAffinityTerm{Roles: []string{ms.Spec.Template.Roles[0].Name, "second"}, Weight: tc.term.Weight, TopologyTier: tc.term.TopologyTier, TopologyTierName: tc.term.TopologyTierName}
				switch kind {
				case "servingGroupAntiAffinity":
					topology.ServingGroupAntiAffinity = &workload.ServingGroupAntiAffinity{Preferred: []workload.ServingGroupAffinityTerm{tc.term}}
				case "roleAffinity":
					topology.RoleAffinity = &workload.RoleAffinity{Preferred: []workload.RoleAffinityTerm{roleTerm}}
				case "roleAntiAffinity":
					topology.RoleAntiAffinity = &workload.RoleAntiAffinity{Preferred: []workload.RoleAffinityTerm{roleTerm}}
				}
				ms.Spec.Template.NetworkTopology = topology
				admitContract(t, nil, ms, tc.wantField)
				admitContract(t, ms.DeepCopy(), ms, tc.wantField)
			})
		}
	}
}
