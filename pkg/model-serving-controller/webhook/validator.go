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
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// ModelServingValidator handles validation of ModelServing resources.
type ModelServingValidator struct {
}

func NewModelServingValidator() *ModelServingValidator {
	return &ModelServingValidator{}
}

// Handle handles admission requests for ModelServing resources
func (v *ModelServingValidator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, modelServing, err := utils.ParseModelServingFromRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Validate the ModelServing. Update validation also compares immutable fields
	// against the object from the admission request.
	oldModelServing, err := parseOldModelServingForUpdate(admissionReview)
	if err != nil {
		klog.Errorf("Failed to parse old modelServing from admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	var allowed bool
	var reason string
	if oldModelServing != nil {
		allowed, reason = v.validateModelServingUpdate(oldModelServing, modelServing)
	} else {
		allowed, reason = v.validateModelServing(modelServing)
	}

	// Create the admission response
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: allowed,
		UID:     admissionReview.Request.UID,
	}

	if !allowed {
		admissionResponse.Result = &metav1.Status{
			Message: reason,
		}
	}

	// Create the admission review response
	admissionReview.Response = &admissionResponse

	// Send the response
	if err := utils.SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send admission response: %v", err)
		http.Error(w, fmt.Sprintf("could not send response: %v", err), http.StatusInternalServerError)
		return
	}
}

func parseOldModelServingForUpdate(admissionReview *admissionv1.AdmissionReview) (*workloadv1alpha1.ModelServing, error) {
	if admissionReview.Request.Operation != admissionv1.Update {
		return nil, nil
	}
	if len(admissionReview.Request.OldObject.Raw) == 0 {
		return nil, fmt.Errorf("old modelServing is required for update validation")
	}

	oldModelServing := &workloadv1alpha1.ModelServing{}
	if err := json.Unmarshal(admissionReview.Request.OldObject.Raw, oldModelServing); err != nil {
		return nil, fmt.Errorf("failed to decode old modelServing: %v", err)
	}
	return oldModelServing, nil
}

// validateModelServing validates the ModelServing resource
func (v *ModelServingValidator) validateModelServing(modelServing *workloadv1alpha1.ModelServing) (bool, string) {
	return validateModelServingErrors(modelServing, nil)
}

func (v *ModelServingValidator) validateModelServingUpdate(oldModelServing, modelServing *workloadv1alpha1.ModelServing) (bool, string) {
	return validateModelServingErrors(modelServing, validateNetworkTopologyImmutable(oldModelServing, modelServing))
}

func validateModelServingErrors(modelServing *workloadv1alpha1.ModelServing, allErrs field.ErrorList) (bool, string) {
	allErrs = append(allErrs, validGeneratedNameLength(modelServing)...)
	allErrs = append(allErrs, validateRoleNames(modelServing)...)
	allErrs = append(allErrs, validateWorkerImages(modelServing)...)
	allErrs = append(allErrs, validatorReplicas(modelServing)...)
	allErrs = append(allErrs, validateRollingUpdateConfiguration(modelServing)...)
	allErrs = append(allErrs, validateMaxUnavailableForRoles(modelServing)...)
	allErrs = append(allErrs, validateGangPolicy(modelServing)...)
	allErrs = append(allErrs, validateTopologyAffinity(modelServing)...)
	allErrs = append(allErrs, validateWorkerReplicas(modelServing)...)
	allErrs = append(allErrs, validateRecoveryPolicyAndRolloutStrategy(modelServing)...)

	if len(allErrs) > 0 {
		var messages []string
		for _, err := range allErrs {
			messages = append(messages, fmt.Sprintf("  - %s", err.Error()))
		}
		return false, fmt.Sprintf("validation failed:\n%s", strings.Join(messages, "\n"))
	}
	return true, ""
}

func validateNetworkTopologyImmutable(oldModelServing, modelServing *workloadv1alpha1.ModelServing) field.ErrorList {
	return apivalidation.ValidateImmutableField(
		modelServing.Spec.Template.NetworkTopology,
		oldModelServing.Spec.Template.NetworkTopology,
		field.NewPath("spec").Child("template").Child("networkTopology"),
	)
}

// validNameLength validates the resource name generated by modelServing.
func validGeneratedNameLength(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	replicas := replicasOrDefault(ms.Spec.Replicas)
	for _, role := range ms.Spec.Template.Roles {
		roleReplicas := replicasOrDefault(role.Replicas)
		name := ms.GetName() + "-" + strconv.Itoa(int(replicas)) + "-" + role.Name + "-" + strconv.Itoa(int(roleReplicas)) + "-" + strconv.Itoa(int(role.WorkerReplicas))
		errors := apivalidation.NameIsDNS1035Label(name, false)
		if len(errors) > 0 {
			klog.Errorf("name generated by modelServing is illegal, please change ms.Name or role.Name")
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("metadata").Child("name"),
				ms.GetName(),
				fmt.Sprintf("invalid name: %s", strings.Join(errors, "; ")),
			))
		}
	}

	return allErrs
}

func replicasOrDefault(replicas *int32) int32 {
	if replicas == nil {
		return 1
	}
	return *replicas
}

// validateRoleNames validates that role names conform to DNS-1035 label format.
func validateRoleNames(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	for i, role := range ms.Spec.Template.Roles {
		errors := apivalidation.NameIsDNS1035Label(role.Name, false)
		if len(errors) > 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("name"),
				role.Name,
				fmt.Sprintf("role name must be a valid DNS-1035 label (lowercase alphanumeric characters or '-', must start with a letter): %s", strings.Join(errors, "; ")),
			))
		}
	}
	return allErrs
}

// validateRollingUpdateConfiguration is validates maxUnavailable and maxSurge in rollingUpdateConfiguration.
func validateRollingUpdateConfiguration(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	if ms.Spec.RolloutStrategy == nil || ms.Spec.RolloutStrategy.RollingUpdateConfiguration == nil {
		return allErrs
	}

	rollingUpdateConfigurationPath := field.NewPath("spec").Child("rolloutStrategy").Child("rollingUpdateConfiguration")
	if ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate {
		allErrs = append(allErrs, field.Forbidden(rollingUpdateConfigurationPath, "rollingUpdateConfiguration is only valid when rolloutStrategy.type is ServingGroupRollingUpdate"))
		return allErrs
	}

	maxUnavailable := ms.Spec.RolloutStrategy.RollingUpdateConfiguration.MaxUnavailable
	maxUnavailablePath := rollingUpdateConfigurationPath.Child("maxUnavailable")
	allErrs = append(allErrs, validateIntOrPercent(maxUnavailable, maxUnavailablePath)...)

	// Validate partition field
	if ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition != nil {
		partitionPath := rollingUpdateConfigurationPath.Child("partition")
		allErrs = append(allErrs, validateIntOrPercent(ms.Spec.RolloutStrategy.RollingUpdateConfiguration.Partition, partitionPath)...)
	}

	if ms.Spec.Replicas != nil && maxUnavailable != nil {
		replicas := int(*ms.Spec.Replicas)
		maxUnavailableValue, err := intstr.GetScaledValueFromIntOrPercent(maxUnavailable, replicas, false)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(maxUnavailablePath, maxUnavailable, fmt.Sprintf("invalid maxUnavailable: %v", err)))
		} else if maxUnavailableValue == 0 {
			allErrs = append(allErrs, field.Invalid(rollingUpdateConfigurationPath,
				"",
				"maxUnavailable cannot be 0"))
		}
	}
	return allErrs
}

func validateMaxUnavailableForRoles(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	roleRollingUpdate := ms.Spec.RolloutStrategy != nil && ms.Spec.RolloutStrategy.Type == workloadv1alpha1.RoleRollingUpdate
	for i, role := range ms.Spec.Template.Roles {
		if role.Partition == nil && role.MaxUnavailable == nil {
			continue
		}

		rollingCfgPath := field.NewPath("spec").Child("template").Child("roles").Index(i)

		if role.Partition != nil {
			fieldPath := rollingCfgPath.Child("partition")
			allErrs = append(allErrs, validateIntOrPercent(role.Partition, fieldPath)...)
			if !roleRollingUpdate {
				allErrs = append(allErrs, field.Forbidden(fieldPath, "partition is only valid when rolloutStrategy.type is RoleRollingUpdate"))
			}
		}

		if !roleRollingUpdate {
			continue
		}

		if role.MaxUnavailable != nil {
			fieldPath := rollingCfgPath.Child("maxUnavailable")
			allErrs = append(allErrs, validateIntOrPercent(role.MaxUnavailable, fieldPath)...)
			replicas := 1
			if role.Replicas != nil {
				replicas = int(*role.Replicas)
			}
			maxUnavailable, err := intstr.GetScaledValueFromIntOrPercent(role.MaxUnavailable, replicas, false)
			if err != nil {
				allErrs = append(allErrs, field.Invalid(fieldPath, role.MaxUnavailable, fmt.Sprintf("invalid maxUnavailable: %v", err)))
			} else if maxUnavailable == 0 {
				allErrs = append(allErrs, field.Invalid(fieldPath, role.MaxUnavailable, "maxUnavailable cannot be 0"))
			} else if maxUnavailable > replicas {
				allErrs = append(allErrs, field.Invalid(fieldPath, role.MaxUnavailable, fmt.Sprintf("maxUnavailable cannot be greater than replicas (%d)", replicas)))
			}
		}
	}
	return allErrs
}

func validatorReplicas(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	if ms.Spec.Replicas == nil || *ms.Spec.Replicas < 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("replicas"),
			ms.Spec.Replicas,
			"replicas must be a non-negative integer",
		))
	}

	if len(ms.Spec.Template.Roles) == 0 {
		allErrs = append(allErrs, field.Invalid(
			field.NewPath("spec").Child("template").Child("roles"),
			ms.Spec.Template.Roles,
			"roles must be specified",
		))
		return allErrs
	}

	for i, role := range ms.Spec.Template.Roles {
		if role.Replicas == nil || *role.Replicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("replicas"),
				role.Replicas,
				"role replicas must be a non-negative integer",
			))
		}
	}
	return allErrs
}

// validateGangPolicy validates the gang scheduling configuration
func validateGangPolicy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList

	if ms.Spec.Template.GangPolicy == nil || ms.Spec.Template.GangPolicy.MinRoleReplicas == nil {
		return allErrs
	}

	minRoleReplicas := ms.Spec.Template.GangPolicy.MinRoleReplicas
	minRoleReplicasPath := field.NewPath("spec").Child("template").Child("gangPolicy").Child("minRoleReplicas")

	// Create a map of role names for quick lookup
	roleMap := make(map[string]workloadv1alpha1.Role)
	for _, role := range ms.Spec.Template.Roles {
		roleMap[role.Name] = role
	}

	// Validate each minRoleReplicas entry
	for roleName, minReplicas := range minRoleReplicas {
		// Check if the role exists
		roleElement, ok := roleMap[roleName]
		if !ok {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				roleName,
				fmt.Sprintf("role %s does not exist in template.roles", roleName),
			))
			continue
		}

		// Find the role in the roleMap then check its actual replicas
		// Calculate total replicas for this role
		// minRoleReplicas is compared against the number of Role replicas
		replicas := int32(1)
		if roleElement.Replicas != nil {
			replicas = *roleElement.Replicas
		}

		// Validate minReplicas doesn't exceed total replicas
		if minReplicas > replicas {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				minReplicas,
				fmt.Sprintf("minRoleReplicas (%d) for role %s cannot exceed replicas (%d)", minReplicas, roleName, replicas),
			))
		}

		// Validate minReplicas is non-negative
		if minReplicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				minRoleReplicasPath.Key(roleName),
				minReplicas,
				fmt.Sprintf("minRoleReplicas for role %s must be non-negative", roleName),
			))
		}
	}

	return allErrs
}

// validateTopologyAffinity validates required/preferred topology terms and the
// Role references that cannot be expressed completely in the CRD schema.
func validateTopologyAffinity(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	networkTopology := ms.Spec.Template.NetworkTopology
	if networkTopology == nil ||
		(networkTopology.ServingGroupAntiAffinity == nil && networkTopology.RoleAffinity == nil && networkTopology.RoleAntiAffinity == nil) {
		return allErrs
	}

	topologyPath := field.NewPath("spec").Child("template").Child("networkTopology")
	if ms.Spec.SchedulerName != "volcano" {
		allErrs = append(allErrs, field.Invalid(
			topologyPath,
			networkTopology,
			"networkTopology affinity rules require schedulerName to be volcano",
		))
	}

	hasTerms := false
	if antiAffinity := networkTopology.ServingGroupAntiAffinity; antiAffinity != nil {
		path := topologyPath.Child("servingGroupAntiAffinity")
		hasTerms = hasTerms || len(antiAffinity.Required) > 0 || len(antiAffinity.Preferred) > 0
		for i := range antiAffinity.Required {
			termPath := path.Child("required").Index(i)
			allErrs = append(allErrs, validateTopologyTerm(
				antiAffinity.Required[i].Weight,
				antiAffinity.Required[i].TopologyTierName,
				antiAffinity.Required[i].TopologyTier,
				true,
				termPath,
			)...)
		}
		for i := range antiAffinity.Preferred {
			termPath := path.Child("preferred").Index(i)
			allErrs = append(allErrs, validateTopologyTerm(
				antiAffinity.Preferred[i].Weight,
				antiAffinity.Preferred[i].TopologyTierName,
				antiAffinity.Preferred[i].TopologyTier,
				false,
				termPath,
			)...)
		}
	}

	roleReplicas := make(map[string]int32, len(ms.Spec.Template.Roles))
	for _, role := range ms.Spec.Template.Roles {
		roleReplicas[role.Name] = replicasOrDefault(role.Replicas)
	}

	if affinity := networkTopology.RoleAffinity; affinity != nil {
		path := topologyPath.Child("roleAffinity")
		hasTerms = hasTerms || len(affinity.Required) > 0 || len(affinity.Preferred) > 0
		allErrs = append(allErrs, validateRoleAffinityTerms(affinity.Required, roleReplicas, true, true, path.Child("required"))...)
		allErrs = append(allErrs, validateRoleAffinityTerms(affinity.Preferred, roleReplicas, false, true, path.Child("preferred"))...)
	}

	if antiAffinity := networkTopology.RoleAntiAffinity; antiAffinity != nil {
		path := topologyPath.Child("roleAntiAffinity")
		hasTerms = hasTerms || len(antiAffinity.Required) > 0 || len(antiAffinity.Preferred) > 0
		allErrs = append(allErrs, validateRoleAffinityTerms(antiAffinity.Required, roleReplicas, true, false, path.Child("required"))...)
		allErrs = append(allErrs, validateRoleAffinityTerms(antiAffinity.Preferred, roleReplicas, false, false, path.Child("preferred"))...)
	}

	if !hasTerms {
		allErrs = append(allErrs, field.Required(topologyPath, "at least one topology affinity term must be configured"))
	}

	allErrs = append(allErrs, validateRequiredRoleTopologyConflicts(networkTopology, roleReplicas, topologyPath)...)
	return allErrs
}

func validateTopologyTerm(weight *int32, topologyTierName string, topologyTier *int32, required bool, termPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	if (topologyTierName != "") == (topologyTier != nil) {
		allErrs = append(allErrs, field.Invalid(
			termPath,
			"",
			"exactly one of topologyTierName and topologyTier must be set",
		))
	}
	if topologyTier != nil && *topologyTier < 0 {
		allErrs = append(allErrs, field.Invalid(termPath.Child("topologyTier"), *topologyTier, "must be greater than or equal to 0"))
	}

	if required {
		if weight != nil {
			allErrs = append(allErrs, field.Forbidden(termPath.Child("weight"), "weight must not be set in a required term"))
		}
		return allErrs
	}

	if weight == nil {
		allErrs = append(allErrs, field.Required(termPath.Child("weight"), "weight is required in a preferred term"))
	} else if *weight < 1 || *weight > 100 {
		allErrs = append(allErrs, field.Invalid(termPath.Child("weight"), *weight, "must be between 1 and 100"))
	}
	return allErrs
}

func validateRoleAffinityTerms(
	terms []workloadv1alpha1.RoleAffinityTerm,
	roleReplicas map[string]int32,
	required bool,
	affinity bool,
	termsPath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList

	for i := range terms {
		term := &terms[i]
		termPath := termsPath.Index(i)
		allErrs = append(allErrs, validateTopologyTerm(
			term.Weight,
			term.TopologyTierName,
			term.TopologyTier,
			required,
			termPath,
		)...)

		minimumRoles := 1
		if affinity {
			minimumRoles = 2
		}
		if len(term.Roles) < minimumRoles {
			allErrs = append(allErrs, field.Invalid(
				termPath.Child("roles"),
				term.Roles,
				fmt.Sprintf("must contain at least %d distinct Role name(s)", minimumRoles),
			))
		}

		seen := make(map[string]struct{}, len(term.Roles))
		for roleIndex, roleName := range term.Roles {
			rolePath := termPath.Child("roles").Index(roleIndex)
			if _, exists := roleReplicas[roleName]; !exists {
				allErrs = append(allErrs, field.NotFound(rolePath, roleName))
			}
			if _, exists := seen[roleName]; exists {
				allErrs = append(allErrs, field.Duplicate(rolePath, roleName))
				continue
			}
			seen[roleName] = struct{}{}
		}
	}

	return allErrs
}

// validateRequiredRoleTopologyConflicts rejects hard constraints whose tier
// relationship and Role pairs make them impossible to satisfy statically.
func validateRequiredRoleTopologyConflicts(
	networkTopology *workloadv1alpha1.NetworkTopology,
	roleReplicas map[string]int32,
	topologyPath *field.Path,
) field.ErrorList {
	var allErrs field.ErrorList
	if networkTopology.RoleAffinity == nil || networkTopology.RoleAntiAffinity == nil {
		return allErrs
	}

	for antiIndex, antiTerm := range networkTopology.RoleAntiAffinity.Required {
		for affinityIndex, affinityTerm := range networkTopology.RoleAffinity.Required {
			if !topologyTiersConflict(affinityTerm, antiTerm) || !roleTermsConflict(affinityTerm.Roles, antiTerm.Roles, roleReplicas) {
				continue
			}
			allErrs = append(allErrs, field.Invalid(
				topologyPath.Child("roleAntiAffinity").Child("required").Index(antiIndex),
				antiTerm,
				fmt.Sprintf("conflicts with roleAffinity.required[%d] at a broader anti-affinity tier", affinityIndex),
			))
		}
	}
	return allErrs
}

func topologyTiersConflict(affinityTerm, antiAffinityTerm workloadv1alpha1.RoleAffinityTerm) bool {
	if affinityTerm.TopologyTier != nil && antiAffinityTerm.TopologyTier != nil {
		return *affinityTerm.TopologyTier < *antiAffinityTerm.TopologyTier
	}
	return false
}

func roleTermsConflict(affinityRoles, antiAffinityRoles []string, roleReplicas map[string]int32) bool {
	affinitySet := make(map[string]struct{}, len(affinityRoles))
	for _, roleName := range affinityRoles {
		affinitySet[roleName] = struct{}{}
	}

	if len(antiAffinityRoles) == 1 {
		roleName := antiAffinityRoles[0]
		_, included := affinitySet[roleName]
		return included && roleReplicas[roleName] > 1
	}

	sharedRolesWithReplicas := 0
	seen := make(map[string]struct{}, len(antiAffinityRoles))
	for _, roleName := range antiAffinityRoles {
		if _, duplicate := seen[roleName]; duplicate {
			continue
		}
		seen[roleName] = struct{}{}
		if _, included := affinitySet[roleName]; included && roleReplicas[roleName] > 0 {
			sharedRolesWithReplicas++
		}
	}
	return sharedRolesWithReplicas >= 2
}

// validateWorkerReplicas validates worker replicas in roles
func validateWorkerReplicas(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList

	for i, role := range ms.Spec.Template.Roles {
		// WorkerReplicas must be non-negative
		if role.WorkerReplicas < 0 {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerReplicas"),
				role.WorkerReplicas,
				"workerReplicas must be a non-negative integer",
			))
		}
		if role.WorkerReplicas > 0 && role.WorkerTemplate == nil {
			allErrs = append(allErrs, field.Required(
				field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerTemplate"),
				"workerTemplate is required when workerReplicas is greater than 0",
			))
		}
	}
	return allErrs
}

func validateIntOrPercent(value *intstr.IntOrString, fieldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value == nil {
		return allErrs
	}
	switch value.Type {
	case intstr.String:
		for _, msg := range validation.IsValidPercent(value.StrVal) {
			allErrs = append(allErrs, field.Invalid(fieldPath, value, msg))
		}
		if len(allErrs) > 0 {
			break
		}
		// Strip trailing '%' and parse; IsValidPercent already ensures the format is valid.
		percentStr := strings.TrimSuffix(value.StrVal, "%")
		percent, err := strconv.Atoi(percentStr)
		if err != nil || percent < 0 || percent > 100 {
			allErrs = append(allErrs, field.Invalid(fieldPath, value, "must be a valid percent value (0-100)"))
		}
	case intstr.Int:
		allErrs = append(allErrs, validateNonnegativeField(int64(value.IntValue()), fieldPath)...)
	default:
		allErrs = append(allErrs, field.Invalid(fieldPath, value, "must be an int or percent"))
	}
	return allErrs
}

func validateNonnegativeField(value int64, fldPath *field.Path) field.ErrorList {
	allErrs := field.ErrorList{}
	if value < 0 {
		allErrs = append(allErrs, field.Invalid(fldPath, value, "must be a non-negative integer"))
	}
	return allErrs
}

// validateWorkerImages validates the image of entryPod and workerPod.
func validateWorkerImages(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	for i, role := range ms.Spec.Template.Roles {
		// validate entryPod image
		for j, container := range role.EntryTemplate.Spec.Containers {
			if container.Image != "" {
				if err := validateImageField(container.Image); err != nil {
					allErrs = append(allErrs, field.Invalid(
						field.NewPath("spec").Child("template").Child("roles").Index(i).Child("entryTemplate").Child("spec").Child("containers").Index(j).Child("image"),
						container.Image,
						fmt.Sprintf("invalid container image reference: %v", err),
					))
				}
			}
		}

		// validate workerPods image
		if role.WorkerTemplate != nil {
			for j, container := range role.WorkerTemplate.Spec.Containers {
				if container.Image != "" {
					if err := validateImageField(container.Image); err != nil {
						allErrs = append(allErrs, field.Invalid(
							field.NewPath("spec").Child("template").Child("roles").Index(i).Child("workerTemplate").Child("spec").Child("containers").Index(j).Child("image"),
							container.Image,
							fmt.Sprintf("invalid container image reference: %v", err),
						))
					}
				}
			}
		}
	}
	return allErrs
}

// validateImageField checks if a container image string is a valid Docker reference.
func validateImageField(image string) error {
	if image == "" {
		// Optional: return the error if you want to require the image field
		return nil
	}

	// Simple validation: check if image contains at least one character and no spaces
	if strings.TrimSpace(image) == "" {
		return fmt.Errorf("image cannot be empty or whitespace only")
	}

	if strings.Contains(image, " ") {
		return fmt.Errorf("image cannot contain spaces")
	}

	return nil
}

func validateRecoveryPolicyAndRolloutStrategy(ms *workloadv1alpha1.ModelServing) field.ErrorList {
	var allErrs field.ErrorList
	// Effective defaults:
	// - recoveryPolicy: RoleRecreate
	// - rolloutStrategy.type: ServingGroupRollingUpdate
	// Required one-to-one mapping not be:
	// - ServingGroupRecreate <-> roleRollingUpdate
	effectiveRecoveryPolicy := ms.Spec.RecoveryPolicy
	if effectiveRecoveryPolicy == "" {
		effectiveRecoveryPolicy = workloadv1alpha1.RoleRecreate
	}

	effectiveRolloutType := workloadv1alpha1.ServingGroupRollingUpdate
	if ms.Spec.RolloutStrategy != nil {
		effectiveRolloutType = ms.Spec.RolloutStrategy.Type
		if effectiveRolloutType == "" {
			effectiveRolloutType = workloadv1alpha1.ServingGroupRollingUpdate
		}
	}

	unMatched := (effectiveRecoveryPolicy == workloadv1alpha1.ServingGroupRecreate && effectiveRolloutType == workloadv1alpha1.RoleRollingUpdate)

	if unMatched {
		// Point to the explicitly specified field when possible.
		errPath := field.NewPath("spec").Child("rolloutStrategy").Child("type")
		errValue := any(effectiveRolloutType)
		if ms.Spec.RolloutStrategy == nil {
			errPath = field.NewPath("spec").Child("recoveryPolicy")
			errValue = effectiveRecoveryPolicy
		}

		allErrs = append(allErrs, field.Invalid(
			errPath,
			errValue,
			fmt.Sprintf(
				"incompatible recoveryPolicy and rolloutStrategy.type after applying defaults: recoveryPolicy=%s, rolloutStrategy.type=%s; valid pairs: (%s,%s) or (%s,%s)",
				effectiveRecoveryPolicy,
				effectiveRolloutType,
				workloadv1alpha1.ServingGroupRecreate,
				workloadv1alpha1.ServingGroupRollingUpdate,
				workloadv1alpha1.RoleRecreate,
				workloadv1alpha1.RoleRollingUpdate,
			),
		))
	}

	return allErrs
}
