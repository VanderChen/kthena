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
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// settings returns what the spec of a group at revision carries besides the
// settings copied from the ModelServing.
func (r *round) settings(revision string) workload.Settings {
	// A group is told the target under either strategy. One that is replaced
	// as a whole needs it to know whether its own revision is the one the
	// rollout moves to: only then does it bring its Pods to that revision.
	settings := workload.Settings{TargetRevision: r.target, BaselineRevision: r.baseline, UpdateMethod: workloadv1alpha1.RecreateUpdateMethod}
	if r.view.inPlace {
		// A group is told InPlace only when it takes effect.
		settings.UpdateMethod = workloadv1alpha1.InPlaceUpdateMethod
	}
	layout := revision
	if r.view.spec.Mode == grouplevel.ModeRole {
		layout = r.target
	} else {
		settings.Revision = revision
	}
	at, err := r.history.At(layout)
	if err != nil {
		// The revision of Pods that are taken back cannot be read: the group
		// reports by the layout of the target until it can.
		at, err = r.history.At(r.target)
	}
	if err == nil {
		settings.ExpectedPods = workload.ExpectedPods(at)
	}
	settings.BaselineReplicas = r.baselineReplicas
	return settings
}
