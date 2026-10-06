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
	"encoding/json"
	"fmt"
	"hash"
	"hash/fnv"
	"sort"

	"k8s.io/apimachinery/pkg/util/dump"
	"k8s.io/apimachinery/pkg/util/rand"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// roleTemplateRevision contains only fields that define the workload rendered
// for one Role. Replica counts and rolling-update policy control how an existing
// workload is managed; they do not define the workload template itself.
//
// Keep this explicit projection instead of hashing workloadv1alpha1.Role
// directly. Adding another operational field to the public Role API must not
// silently change every existing workload's revision.
type roleTemplateRevision struct {
	Name           string                            `json:"name"`
	EntryTemplate  workloadv1alpha1.PodTemplateSpec  `json:"entryTemplate"`
	WorkerReplicas int32                             `json:"workerReplicas"`
	WorkerTemplate *workloadv1alpha1.PodTemplateSpec `json:"workerTemplate,omitempty"`
}

// Revision calculates the revision of an object using FNV hashing.
func Revision(obj interface{}) string {
	hasher := fnv.New32()
	DeepHashObject(hasher, obj)
	return rand.SafeEncodeString(fmt.Sprint(hasher.Sum32()))
}

// serializedRevision hashes the API representation, so newly added omitted fields
// do not change identity merely because the Go type changed.
func serializedRevision(value interface{}) string {
	data, err := json.Marshal(value)
	if err != nil {
		return Revision(value)
	}
	return RevisionDataHash(data, nil)
}

// DeepHashObject writes specified object to hash using the spew library
// which follows pointers and prints actual values of the nested objects
// ensuring the hash does not change when a pointer changes.
func DeepHashObject(hasher hash.Hash, objectToWrite interface{}) {
	hasher.Reset()
	fmt.Fprintf(hasher, "%v", dump.ForHash(objectToWrite))
}

func projectRoleTemplateForRevision(role workloadv1alpha1.Role) roleTemplateRevision {
	return roleTemplateRevision{
		Name:           role.Name,
		EntryTemplate:  *role.EntryTemplate.DeepCopy(),
		WorkerReplicas: role.WorkerReplicas,
		WorkerTemplate: role.WorkerTemplate.DeepCopy(),
	}
}

func projectRoleTemplatesForRevision(roles []workloadv1alpha1.Role) []roleTemplateRevision {
	projected := make([]roleTemplateRevision, len(roles))
	for i := range roles {
		projected[i] = projectRoleTemplateForRevision(roles[i])
	}
	sort.Slice(projected, func(i, j int) bool { return projected[i].Name < projected[j].Name })
	return projected
}

// ModelServingRevision calculates the revision of a ModelServing object.
func ModelServingRevision(ms *workloadv1alpha1.ModelServing) string {
	configuration, err := BuildRevisionConfiguration(ms)
	if err != nil {
		return ""
	}
	return serializedRevision(struct {
		Roles         []roleTemplateRevision `json:"roles"`
		Configuration json.RawMessage        `json:"configuration"`
	}{projectRoleTemplatesForRevision(ms.Spec.Template.Roles), configuration})
}

// CalRoleTemplateHash calculates the revision hash for a Role template.
func CalRoleTemplateHash(role workloadv1alpha1.Role) string {
	return serializedRevision(projectRoleTemplateForRevision(role))
}
