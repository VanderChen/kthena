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

package test

import (
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func CreatePodGroupCRD() *apiextv1.CustomResourceDefinition {
	return CreatePodGroupCRDWithFeatures(false, false)
}

func CreatePodGroupCRDWithFeatures(hasSubGroupPolicy, hasTopologyAffinity bool) *apiextv1.CustomResourceDefinition {
	specProperties := make(map[string]apiextv1.JSONSchemaProps)
	if hasSubGroupPolicy {
		specProperties["subGroupPolicy"] = apiextv1.JSONSchemaProps{Type: "array"}
	}
	if hasTopologyAffinity {
		specProperties["topologyAffinity"] = apiextv1.JSONSchemaProps{Type: "object"}
	}

	return &apiextv1.CustomResourceDefinition{
		ObjectMeta: metav1.ObjectMeta{
			Name: "podgroups.scheduling.volcano.sh",
		},
		Spec: apiextv1.CustomResourceDefinitionSpec{
			Group: "scheduling.volcano.sh",
			Names: apiextv1.CustomResourceDefinitionNames{
				Plural:   "podgroups",
				Singular: "podgroup",
				Kind:     "PodGroup",
				ListKind: "PodGroupList",
			},
			Scope: apiextv1.ClusterScoped,
			Versions: []apiextv1.CustomResourceDefinitionVersion{
				{
					Name:    "v1beta1",
					Served:  true,
					Storage: true,
					Schema: &apiextv1.CustomResourceValidation{
						OpenAPIV3Schema: &apiextv1.JSONSchemaProps{
							Type: "object",
							Properties: map[string]apiextv1.JSONSchemaProps{
								"spec": {
									Type:       "object",
									Properties: specProperties,
								},
							},
						},
					},
				},
			},
		},
	}
}
