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
	"os"
	"testing"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// The identity of a revision is on every Pod of a running ModelServing. A
// controller that computes another identity for the same template takes all
// those Pods for outdated and replaces them: an upgrade of the controller
// would restart every workload.
//
// testdata/release-1.1-revisions.json holds ModelServing specs as the API
// server stored them and the identity that the controller manager of
// release-1.1 (commit 24f0e136) gave them on a cluster. This build has to
// give them the same.
func TestRevisionIdentityIsTheOneOfRelease11(t *testing.T) {
	raw, err := os.ReadFile("testdata/release-1.1-revisions.json")
	if err != nil {
		t.Fatal(err)
	}
	var samples []struct {
		Name     string                            `json:"name"`
		Revision string                            `json:"revision"`
		Spec     workloadv1alpha1.ModelServingSpec `json:"spec"`
	}
	if err := json.Unmarshal(raw, &samples); err != nil {
		t.Fatal(err)
	}
	if len(samples) == 0 {
		t.Fatal("no samples")
	}
	for _, sample := range samples {
		ms := &workloadv1alpha1.ModelServing{Spec: sample.Spec}
		if got := ModelServingRevision(ms); got != sample.Revision {
			t.Errorf("%s: revision = %s, release-1.1 computed %s", sample.Name, got, sample.Revision)
		}
	}
}
