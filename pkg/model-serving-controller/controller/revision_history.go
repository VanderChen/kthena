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

package controller

import (
	"context"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// modelServingForRevision restores immutable global workload inputs without
// replacing the current replica, rollout or recovery policies.
func (c *ModelServingController) modelServingForRevision(ctx context.Context, ms *workloadv1alpha1.ModelServing, revision string) (*workloadv1alpha1.ModelServing, error) {
	cr, err := utils.GetControllerRevision(ctx, c.kubeClientSet, ms, revision)
	if err != nil {
		return nil, err
	}
	if cr == nil {
		return ms, nil
	} // New target creation persists history at reconcile entry.
	baseline, err := utils.EnsureRevisionBaseline(ctx, c.kubeClientSet, ms, cr)
	if err != nil {
		return nil, err
	}
	historical, err := utils.ModelServingForControllerRevision(ms, baseline)
	if err != nil {
		return nil, err
	}
	result := ms.DeepCopy()
	result.Spec.SchedulerName = historical.Spec.SchedulerName
	result.Spec.Plugins = historical.Spec.Plugins
	return result, nil
}
