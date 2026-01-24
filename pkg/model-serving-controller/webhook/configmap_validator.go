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
	"fmt"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	versioned "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/ranktable"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// ConfigMapValidator handles validation of ConfigMap resources.
type ConfigMapValidator struct {
	kubeClient   kubernetes.Interface
	kthenaClient versioned.Interface
}

func NewConfigMapValidator(kubeClient kubernetes.Interface, kthenaClient versioned.Interface) *ConfigMapValidator {
	return &ConfigMapValidator{
		kubeClient:   kubeClient,
		kthenaClient: kthenaClient,
	}
}

// Handle handles admission requests for ConfigMap resources
func (v *ConfigMapValidator) Handle(w http.ResponseWriter, r *http.Request) {
	// Parse the admission request
	admissionReview, cm, err := utils.ParseConfigMapFromRequest(r)
	if err != nil {
		klog.Errorf("Failed to parse admission request: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// We only care about ranktable related configmaps, which are usually in specific namespace
	// But since we might not restrict namespace strictly in code (webhook config does),
	// we just process valid requests.
	// The RanktableTemplateNamespace is where templates are stored.
	if cm.Namespace != ranktable.RanktableTemplateNamespace {
		// If not in kthena-system, we simply allow it,
		// UNLESS users are storing templates elsewhere.
		// The controller assumes templates are in RanktableTemplateNamespace.
		// So it is safe to ignore other namespaces.
		allow(w, admissionReview)
		return
	}

	// Validate the ConfigMap
	allowed, reason := v.validateConfigMap(r.Context(), cm, admissionReview.Request.Operation)

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

func allow(w http.ResponseWriter, admissionReview *admissionv1.AdmissionReview) {
	admissionResponse := admissionv1.AdmissionResponse{
		Allowed: true,
		UID:     admissionReview.Request.UID,
	}
	admissionReview.Response = &admissionResponse
	if err := utils.SendAdmissionResponse(w, admissionReview); err != nil {
		klog.Errorf("Failed to send allow response: %v", err)
	}
}

// validateConfigMap checks if the ConfigMap is used by any ModelServing
func (v *ConfigMapValidator) validateConfigMap(ctx context.Context, cm *corev1.ConfigMap, operation admissionv1.Operation) (bool, string) {
	// We only care about DELETE and UPDATE
	if operation != admissionv1.Delete && operation != admissionv1.Update {
		return true, ""
	}

	// List all ModelServings
	// TODO: Optimization - maybe cache ModelServings or use index if performance becomes an issue.
	// For now, listing is acceptable as this is an infrequent operation (deleting/updating configmaps in system ns).
	msList, err := v.kthenaClient.WorkloadV1alpha1().ModelServings("").List(ctx, metav1.ListOptions{})
	if err != nil {
		// If we can't list ModelServings, we should probably fail open or closed?
		// Failing closed (deny) is safer for protection, but might block legit ops if API server is flaky.
		// Given this is a protection feature, logging error and allowing might be better to avoid deadlock,
		// OR blocking to be safe. Let's block to be safe with a clear message.
		klog.Errorf("Failed to list ModelServings for ConfigMap validation: %v", err)
		return false, fmt.Sprintf("Failed to list ModelServings to verify usage: %v", err)
	}

	for _, ms := range msList.Items {
		templateName := ms.Annotations[ranktable.RanktableTemplateAnnotation]
		if templateName == "" {
			continue
		}

		// Check 1: Is this ConfigMap the Ranktable Template itself?
		if templateName == cm.Name {
			return false, fmt.Sprintf("ConfigMap '%s' is in use as a Ranktable Template by ModelServing '%s/%s'", cm.Name, ms.Namespace, ms.Name)
		}

		// Check 2: Is this ConfigMap the Pod Parser Template used by the Ranktable Template?
		// We need to fetch the Ranktable Template ConfigMap to find out which parser it uses.
		// We can try to optimize by not fetching if we are not modifying a potential parser.
		// But "potential parser" is just a ConfigMap.
		// Note: If the user modifies the Ranktable Template (Check 1 matched), we blocked it.
		// If we are here, the CM being modified is NOT the Ranktable Template used by this MS.
		// It MIGHT be the Parser.

		// Fetch the Ranktable Template to see its parser.
		// We use a separate context or the same one.
		templateCM, err := v.kubeClient.CoreV1().ConfigMaps(ranktable.RanktableTemplateNamespace).Get(ctx, templateName, metav1.GetOptions{})
		if err != nil {
			// If the template referenced by MS doesn't exist, then this MS is broken or misconfigured.
			// We can ignore this MS for the purpose of protecting *other* things,
			// OR we assume it might be what we are deleting? But we checked name match above.
			// If we can't find the template, we can't check its parser.
			klog.Warningf("ModelServing '%s/%s' references non-existent template '%s'", ms.Namespace, ms.Name, templateName)
			continue
		}

		parserName := templateCM.Data["pod-parser-template"]
		if parserName == cm.Name {
			return false, fmt.Sprintf("ConfigMap '%s' is in use as a Pod Parser Template (via '%s') by ModelServing '%s/%s'", cm.Name, templateName, ms.Namespace, ms.Name)
		}
	}

	return true, ""
}
