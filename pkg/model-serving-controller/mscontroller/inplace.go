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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/grouplevel"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// ReasonInPlaceUpdateFallback is the reason of the event that says why a
// group is recreated although the ModelServing asks for in-place updates.
const ReasonInPlaceUpdateFallback = "InPlaceUpdateFallback"

// updatesInPlace reports whether the update method that takes effect for ms
// is InPlace. When it is not, ms is rolled exactly as with Recreate, and no
// group is told InPlace.
//
// InPlace takes effect when ms asks for it, every plugin of its template
// supports it, and, where whole groups are replaced, the rollout may make a
// group unavailable: with no such budget an in-place update could never
// start, while recreation can use the surge. Under RoleRollingUpdate every
// Role has a budget of its own, which the group applies.
func (c *Controller) updatesInPlace(ms *workloadv1alpha1.ModelServing, spec grouplevel.Spec) bool {
	if inplace.UpdateMethodOf(ms) != workloadv1alpha1.InPlaceUpdateMethod {
		return false
	}
	if len(ms.Spec.Plugins) > 0 {
		chain, err := plugins.NewChain(c.registry, ms.Spec.Plugins)
		if err != nil {
			klog.ErrorS(err, "Cannot build the plugins; the ModelServing is updated by recreation", "modelServing", klog.KObj(ms))
			return false
		}
		if name, unsupported := chain.PluginWithoutInPlaceUpdate(); unsupported {
			klog.V(2).InfoS("A plugin does not support in-place updates; the ModelServing is updated by recreation", "modelServing", klog.KObj(ms), "plugin", name)
			return false
		}
	}
	return spec.Mode == grouplevel.ModeRole || spec.MaxUnavailable > 0
}

// inPlaceFallback returns why a group that the rollout replaces for its
// revision is recreated although groups are updated in place, or "" when it
// is not: the revision of the group and the target differ in more than the
// images of regular containers. known remembers the answer by revision.
//
// It is asked before the revision of a group is changed, so a group that
// cannot follow is deleted like under Recreate and never carries a revision
// its Pods do not run. What only the Pods can tell (a restart policy, a
// container that admission renamed) is found by the group itself, which then
// asks to be recreated.
//
// A group that the plan does not replace for its revision is not asked: a
// group below the partition must not become replaceable by the answer.
func inPlaceFallback(spec grouplevel.Spec, history *workload.History, g groupState, known map[string]string) string {
	unit := g.unit
	if !spec.InPlace || g.comparison != workload.Different || g.ordinal < spec.Partition ||
		unit.Terminating || unit.Failed || unit.Memberless || unit.RecreateRequired {
		return ""
	}
	if reason, ok := known[g.revision]; ok {
		return reason
	}
	reason := ""
	from, fromErr := history.At(g.revision)
	to, toErr := history.At(spec.Target)
	if fromErr == nil && toErr == nil {
		if only, err := inplace.OnlyImagesChanged(from, to); err != nil || !only {
			reason = "changes other than container images"
		}
	}
	known[g.revision] = reason
	return reason
}

// reportFallbacks records an event for every group that the plan of the view
// deletes in order to recreate it although groups are updated in place.
//
// The event is recorded when the deletion is planned, as it was before groups
// were objects. A round that is planned twice, on the cache and then on a
// direct read, records it twice; the recorder merges events that say the same.
func (c *Controller) reportFallbacks(ms *workloadv1alpha1.ModelServing, v *view) {
	pending := false
	for _, g := range v.groups {
		pending = pending || g.fallback != ""
	}
	// While workloads of an earlier version are taken over nothing is rolled.
	if !pending || ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] != workloadv1alpha1.ServingGroupAdoptionDone {
		return
	}
	for _, action := range grouplevel.Plan(v.input) {
		if action.Kind != grouplevel.ActionDelete || action.Reason != grouplevel.ReasonReplace && action.Reason != grouplevel.ReasonReplaceNotReady {
			continue
		}
		if g := v.group(action.Ordinal); g != nil && g.fallback != "" {
			c.recorder.Eventf(ms, corev1.EventTypeNormal, ReasonInPlaceUpdateFallback,
				"ServingGroup %s is recreated instead of updated in place: %s", g.object.Name, g.fallback)
		}
	}
}
