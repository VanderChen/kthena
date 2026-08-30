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

package v1alpha1

// ServingGroupAntiAffinity defines required and preferred separation rules
// between ServingGroups of one ModelServing.
// +kubebuilder:validation:XValidation:rule="!has(self.required) || self.required.all(term, !has(term.weight))",message="weight must not be set in required terms"
// +kubebuilder:validation:XValidation:rule="!has(self.preferred) || self.preferred.all(term, has(term.weight))",message="weight must be set in preferred terms"
type ServingGroupAntiAffinity struct {
	// Required contains hard anti-affinity terms that must all be satisfied.
	// +optional
	// +listType=atomic
	Required []ServingGroupAffinityTerm `json:"required,omitempty"`

	// Preferred contains weighted soft anti-affinity terms.
	// +optional
	// +listType=atomic
	Preferred []ServingGroupAffinityTerm `json:"preferred,omitempty"`
}

// RoleAffinity defines required and preferred co-location rules between Role
// policies in one ServingGroup.
// +kubebuilder:validation:XValidation:rule="!has(self.required) || self.required.all(term, !has(term.weight))",message="weight must not be set in required terms"
// +kubebuilder:validation:XValidation:rule="!has(self.preferred) || self.preferred.all(term, has(term.weight))",message="weight must be set in preferred terms"
type RoleAffinity struct {
	// Required contains hard affinity terms that must all be satisfied.
	// +optional
	// +listType=atomic
	Required []RoleAffinityTerm `json:"required,omitempty"`

	// Preferred contains weighted soft affinity terms.
	// +optional
	// +listType=atomic
	Preferred []RoleAffinityTerm `json:"preferred,omitempty"`
}

// RoleAntiAffinity defines required and preferred separation rules between
// Role policies in one ServingGroup.
// +kubebuilder:validation:XValidation:rule="!has(self.required) || self.required.all(term, !has(term.weight))",message="weight must not be set in required terms"
// +kubebuilder:validation:XValidation:rule="!has(self.preferred) || self.preferred.all(term, has(term.weight))",message="weight must be set in preferred terms"
type RoleAntiAffinity struct {
	// Required contains hard anti-affinity terms that must all be satisfied.
	// +optional
	// +listType=atomic
	Required []RoleAffinityTerm `json:"required,omitempty"`

	// Preferred contains weighted soft anti-affinity terms.
	// +optional
	// +listType=atomic
	Preferred []RoleAffinityTerm `json:"preferred,omitempty"`
}

// ServingGroupAffinityTerm selects the topology tier used to compare peer
// ServingGroups. Exactly one topology tier field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.topologyTierName) != has(self.topologyTier)",message="exactly one of topologyTierName and topologyTier must be set"
type ServingGroupAffinityTerm struct {
	// Weight applies only to preferred terms and must be between 1 and 100.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Weight *int32 `json:"weight,omitempty"`

	// TopologyTierName refers to HyperNode.spec.tierName.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TopologyTierName string `json:"topologyTierName,omitempty"`

	// TopologyTier refers to HyperNode.spec.tier.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TopologyTier *int32 `json:"topologyTier,omitempty"`
}

// RoleAffinityTerm selects Role policy names and the topology tier used to
// compare their SubJobs. Exactly one topology tier field must be set.
// +kubebuilder:validation:XValidation:rule="has(self.topologyTierName) != has(self.topologyTier)",message="exactly one of topologyTierName and topologyTier must be set"
type RoleAffinityTerm struct {
	// Roles contains names from spec.template.roles.
	// +kubebuilder:validation:MinItems=1
	// +listType=set
	Roles []string `json:"roles"`

	// Weight applies only to preferred terms and must be between 1 and 100.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	Weight *int32 `json:"weight,omitempty"`

	// TopologyTierName refers to HyperNode.spec.tierName.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	TopologyTierName string `json:"topologyTierName,omitempty"`

	// TopologyTier refers to HyperNode.spec.tier.
	// +kubebuilder:validation:Minimum=0
	// +optional
	TopologyTier *int32 `json:"topologyTier,omitempty"`
}
