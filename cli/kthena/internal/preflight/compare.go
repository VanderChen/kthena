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

package preflight

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// Severities of a change found by a comparison.
const (
	// SeverityViolation: something changed that the upgrade must not change.
	SeverityViolation = "violation"
	// SeverityExpected: something changed in a ModelServing for which the
	// pre-flight check announced that the controller changes Pods by itself,
	// or whose spec was changed after the baseline. Whether the change is the
	// announced one has to be judged by a person.
	SeverityExpected = "expected"
	// SeverityInfo: an addition, or a change the takeover makes by design.
	SeverityInfo = "info"
)

// Identifiers of the changes a comparison reports.
const (
	ChangeResourceNotServed       = "servinggroup-resource-not-served"
	ChangeModelServingMissing     = "modelserving-missing"
	ChangeModelServingRecreated   = "modelserving-recreated"
	ChangeTakeoverNotDone         = "takeover-not-done"
	ChangeAvailableDropped        = "available-replicas-dropped"
	ChangeGroupWithoutObject      = "group-without-servinggroup"
	ChangeSurgeMark               = "surge-mark-changed"
	ChangePodMissing              = "pod-missing"
	ChangePodRecreated            = "pod-recreated"
	ChangePodRestarted            = "pod-restarted"
	ChangePodNotReady             = "pod-not-ready"
	ChangePodReadinessInterrupted = "pod-readiness-interrupted"
	ChangePodTerminating          = "pod-terminating"
	ChangePodGone                 = "pod-gone"
	ChangePodAdded                = "pod-added"
	ChangeObjectMissing           = "object-missing"
	ChangeObjectRecreated         = "object-recreated"
)

// Change is one difference between the baseline and the cluster.
type Change struct {
	Severity     string `json:"severity"`
	Change       string `json:"change"`
	Kind         string `json:"kind"`
	Namespace    string `json:"namespace"`
	Name         string `json:"name"`
	ModelServing string `json:"modelServing,omitempty"`
	Detail       string `json:"detail"`
}

// Comparison is the result of checking the cluster against a baseline.
type Comparison struct {
	FormatVersion     string    `json:"formatVersion"`
	ComparedAt        time.Time `json:"comparedAt"`
	BaselineCreatedAt time.Time `json:"baselineCreatedAt"`
	Scope             Scope     `json:"scope"`
	// Result is "passed" when nothing changed that the baseline records,
	// "review" when only changes of severity expected were found, "failed"
	// when something changed that must not change.
	Result  string            `json:"result"`
	Summary ComparisonSummary `json:"summary"`
	Changes []Change          `json:"changes"`
}

// ComparisonSummary counts what was compared and what was found.
type ComparisonSummary struct {
	ModelServings int `json:"modelServings"`
	Pods          int `json:"pods"`
	PodGroups     int `json:"podGroups"`
	Services      int `json:"services"`
	Violations    int `json:"violations"`
	Expected      int `json:"expected"`
	Info          int `json:"info"`
}

// Results of a comparison.
const (
	ResultPassed = "passed"
	ResultReview = "review"
	ResultFailed = "failed"
)

// Compare checks the cluster, as read into s, against a baseline taken
// before the upgrade. It is the acceptance check of the upgrade: every
// ModelServing is taken over, every group that has Pods has its ServingGroup
// object, and no Pod, PodGroup or Service was replaced or restarted.
//
// A ModelServing is held to "nothing changes" unless the pre-flight check
// announced that the controller changes its Pods by itself, or its spec was
// changed after the baseline.
func Compare(baseline *Baseline, s *Snapshot) *Comparison {
	c := &Comparison{
		FormatVersion:     FormatVersion,
		ComparedAt:        s.Time.UTC(),
		BaselineCreatedAt: baseline.CreatedAt,
		Scope:             baseline.Scope,
		Changes:           []Change{},
		Summary: ComparisonSummary{
			ModelServings: len(baseline.ModelServings), Pods: len(baseline.Pods),
			PodGroups: len(baseline.PodGroups), Services: len(baseline.Services),
		},
	}

	modelServings := map[string]*workloadv1alpha1.ModelServing{}
	for _, ms := range s.ModelServings {
		modelServings[ms.Namespace+"/"+ms.Name] = ms
	}
	// allowed maps a ModelServing to the reason why its Pods may have changed.
	allowed := map[string]string{}
	for _, base := range baseline.ModelServings {
		key := base.Namespace + "/" + base.Name
		current := modelServings[key]
		switch {
		case current != nil && current.Generation != base.Generation:
			allowed[key] = fmt.Sprintf("the spec was changed after the baseline: generation %d, was %d", current.Generation, base.Generation)
		case len(base.PodChangesExpected) > 0:
			allowed[key] = "the pre-flight check announced changes: " + strings.Join(base.PodChangesExpected, ", ")
		}
	}
	// judge reports a change that is a violation unless the ModelServing is
	// one whose Pods may have changed.
	judge := func(change Change) {
		change.Severity = SeverityViolation
		if reason, ok := allowed[change.Namespace+"/"+change.ModelServing]; ok {
			change.Severity = SeverityExpected
			change.Detail += " (" + reason + ")"
		}
		c.Changes = append(c.Changes, change)
	}
	report := func(severity string, change Change) {
		change.Severity = severity
		c.Changes = append(c.Changes, change)
	}

	if s.ServingGroupCRD != crdInstalled {
		report(SeverityViolation, Change{Change: ChangeResourceNotServed, Kind: "CustomResourceDefinition", Name: "servinggroups.workload.serving.volcano.sh",
			Detail: "the ServingGroup resource is not served or cannot be listed: the cluster does not run the new version, or the check lacks permission"})
	}

	compareModelServings(baseline, s, modelServings, judge, report)
	comparePods(baseline, s, judge, report)
	compareObjects(baseline.PodGroups, "PodGroup", uidsOf(s.PodGroups), judge, report)
	compareObjects(baseline.Services, "Service", uidsOf(s.Services), judge, report)

	rank := map[string]int{SeverityViolation: 0, SeverityExpected: 1, SeverityInfo: 2}
	sort.SliceStable(c.Changes, func(i, j int) bool {
		a, b := c.Changes[i], c.Changes[j]
		if rank[a.Severity] != rank[b.Severity] {
			return rank[a.Severity] < rank[b.Severity]
		}
		if a.Namespace+"/"+a.ModelServing != b.Namespace+"/"+b.ModelServing {
			return a.Namespace+"/"+a.ModelServing < b.Namespace+"/"+b.ModelServing
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Name < b.Name
	})
	for _, change := range c.Changes {
		switch change.Severity {
		case SeverityViolation:
			c.Summary.Violations++
		case SeverityExpected:
			c.Summary.Expected++
		default:
			c.Summary.Info++
		}
	}
	switch {
	case c.Summary.Violations > 0:
		c.Result = ResultFailed
	case c.Summary.Expected > 0:
		c.Result = ResultReview
	default:
		c.Result = ResultPassed
	}
	return c
}

// compareModelServings checks that every ModelServing of the baseline is
// taken over: the annotation says so, every group that has Pods has its
// object, and the temporary groups are the ones the baseline names.
func compareModelServings(baseline *Baseline, s *Snapshot, modelServings map[string]*workloadv1alpha1.ModelServing, judge func(Change), report func(string, Change)) {
	// The ServingGroup objects by namespace/name.
	objects := map[string]*workloadv1alpha1.ServingGroup{}
	for _, object := range s.ServingGroups {
		objects[object.Namespace+"/"+object.Name] = object
	}
	// The groups that have a member Pod that is not terminating, by
	// ModelServing. A member is controlled by a ServingGroup, by the
	// ModelServing, or by nothing; a Pod of another owner is not one.
	groups := map[string]map[string]bool{}
	for _, pod := range s.Pods {
		name, group := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey], pod.Labels[workloadv1alpha1.GroupNameLabelKey]
		if pod.DeletionTimestamp != nil || !servinggroup.HasMemberIdentity(pod, name, group) {
			continue
		}
		key := pod.Namespace + "/" + name
		if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.Kind != workloadv1alpha1.ServingGroupKind.Kind &&
			(modelServings[key] == nil || owner.UID != modelServings[key].UID) {
			continue
		}
		if groups[key] == nil {
			groups[key] = map[string]bool{}
		}
		groups[key][group] = true
	}

	for _, base := range baseline.ModelServings {
		key := base.Namespace + "/" + base.Name
		change := Change{Kind: "ModelServing", Namespace: base.Namespace, Name: base.Name, ModelServing: base.Name}
		current := modelServings[key]
		if current == nil {
			change.Change, change.Detail = ChangeModelServingMissing, "the ModelServing does not exist any more"
			report(SeverityViolation, change)
			continue
		}
		if current.UID != base.UID {
			change.Change, change.Detail = ChangeModelServingRecreated, fmt.Sprintf("the ModelServing was deleted and created again: UID %s, was %s", current.UID, base.UID)
			report(SeverityViolation, change)
			continue
		}
		if adoption := current.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; adoption != workloadv1alpha1.ServingGroupAdoptionDone {
			change.Change = ChangeTakeoverNotDone
			change.Detail = fmt.Sprintf("the annotation %s is %q; it is Done when the takeover has finished", workloadv1alpha1.ServingGroupAdoptionAnnotationKey, adoption)
			report(SeverityViolation, change)
		}
		if current.Status.AvailableReplicas < base.Status.AvailableReplicas {
			change.Change = ChangeAvailableDropped
			change.Detail = fmt.Sprintf("status.availableReplicas is %d, was %d", current.Status.AvailableReplicas, base.Status.AvailableReplicas)
			judge(change)
		}

		if s.ServingGroupCRD != crdInstalled {
			continue
		}
		surge := map[string]bool{}
		for _, name := range base.SurgeGroups {
			surge[name] = true
		}
		names := make([]string, 0, len(groups[key]))
		for name := range groups[key] {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			change := Change{Kind: "ServingGroup", Namespace: base.Namespace, Name: name, ModelServing: base.Name}
			object := objects[base.Namespace+"/"+name]
			if object == nil || !controlledBy(object, current.UID) {
				change.Change, change.Detail = ChangeGroupWithoutObject, "Pods of this group exist and no ServingGroup object of that name is controlled by the ModelServing"
				report(SeverityViolation, change)
				continue
			}
			marked := object.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup
			switch {
			case marked && !surge[name]:
				change.Change, change.Detail = ChangeSurgeMark, "the ServingGroup carries the surge annotation, and the baseline does not name the group as temporary capacity"
				judge(change)
			case !marked && surge[name]:
				change.Change, change.Detail = ChangeSurgeMark, "the baseline names the group as temporary capacity, and the ServingGroup carries no surge annotation"
				judge(change)
			}
		}
	}
}

// comparePods checks that every Pod of the baseline is the same Pod, has not
// restarted and has stayed ready.
func comparePods(baseline *Baseline, s *Snapshot, judge func(Change), report func(string, Change)) {
	current := map[string]*corev1.Pod{}
	for _, pod := range s.Pods {
		current[pod.Namespace+"/"+pod.Name] = pod
	}
	recorded := map[string]bool{}
	checked := map[string]bool{}
	for _, ms := range baseline.ModelServings {
		checked[ms.Namespace+"/"+ms.Name] = true
	}

	for _, base := range baseline.Pods {
		key := base.Namespace + "/" + base.Name
		recorded[key] = true
		change := Change{Kind: "Pod", Namespace: base.Namespace, Name: base.Name, ModelServing: base.ModelServing}
		pod := current[key]
		switch {
		case pod == nil && base.Terminating:
			change.Change, change.Detail = ChangePodGone, "the Pod was terminating at the time of the baseline and is gone"
			report(SeverityInfo, change)
			continue
		case pod == nil:
			change.Change, change.Detail = ChangePodMissing, fmt.Sprintf("the Pod does not exist any more; it ran on node %q", base.Node)
			judge(change)
			continue
		case pod.UID != base.UID && base.Terminating:
			change.Change, change.Detail = ChangePodGone, "the Pod was terminating at the time of the baseline and was created again"
			report(SeverityInfo, change)
			continue
		case pod.UID != base.UID:
			change.Change, change.Detail = ChangePodRecreated, fmt.Sprintf("the Pod was deleted and created again: UID %s, was %s", pod.UID, base.UID)
			judge(change)
			continue
		}

		now := baselinePod(pod)
		restarts := map[string]int32{}
		for _, container := range now.Containers {
			restarts[fmt.Sprintf("%t/%s", container.Init, container.Name)] = container.RestartCount
		}
		var restarted []string
		for _, container := range base.Containers {
			if count := restarts[fmt.Sprintf("%t/%s", container.Init, container.Name)]; count > container.RestartCount {
				restarted = append(restarted, fmt.Sprintf("%s: %d, was %d", container.Name, count, container.RestartCount))
			}
		}
		if len(restarted) > 0 {
			change.Change, change.Detail = ChangePodRestarted, "the restart count of a container increased ("+strings.Join(restarted, "; ")+")"
			judge(change)
		}
		switch {
		case now.Terminating && !base.Terminating:
			change.Change, change.Detail = ChangePodTerminating, "the Pod is being deleted"
			judge(change)
		case base.Ready && !now.Ready:
			change.Change, change.Detail = ChangePodNotReady, "the Pod was ready at the time of the baseline and is not ready"
			judge(change)
		case base.Ready && base.ReadyLastTransitionTime != nil && now.ReadyLastTransitionTime != nil &&
			!base.ReadyLastTransitionTime.Equal(*now.ReadyLastTransitionTime):
			change.Change = ChangePodReadinessInterrupted
			change.Detail = fmt.Sprintf("the Pod was not ready for a time after the baseline: its Ready condition last changed at %s, was %s",
				now.ReadyLastTransitionTime.Format(time.RFC3339), base.ReadyLastTransitionTime.Format(time.RFC3339))
			judge(change)
		}
	}

	for _, pod := range s.Pods {
		name := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]
		if recorded[pod.Namespace+"/"+pod.Name] || !checked[pod.Namespace+"/"+name] {
			continue
		}
		report(SeverityInfo, Change{Change: ChangePodAdded, Kind: "Pod", Namespace: pod.Namespace, Name: pod.Name, ModelServing: name,
			Detail: "the Pod was created after the baseline"})
	}
}

// uidsOf maps namespace/name to the UID of every object.
func uidsOf[T metav1.Object](objects []T) map[string]types.UID {
	out := make(map[string]types.UID, len(objects))
	for _, object := range objects {
		out[object.GetNamespace()+"/"+object.GetName()] = object.GetUID()
	}
	return out
}

// compareObjects checks that every PodGroup or Service of the baseline is
// the same object. One whose group had no Pod to take over is deleted by the
// takeover and created again with the group; that is reported, not judged.
func compareObjects(objects []BaselineObject, kind string, current map[string]types.UID, judge func(Change), report func(string, Change)) {
	for _, base := range objects {
		change := Change{Kind: kind, Namespace: base.Namespace, Name: base.Name, ModelServing: base.ModelServing}
		uid, exists := current[base.Namespace+"/"+base.Name]
		switch {
		case exists && uid == base.UID:
			continue
		case !exists:
			change.Change, change.Detail = ChangeObjectMissing, fmt.Sprintf("the %s does not exist any more", kind)
		default:
			change.Change, change.Detail = ChangeObjectRecreated, fmt.Sprintf("the %s was deleted and created again: UID %s, was %s", kind, uid, base.UID)
		}
		if !base.GroupHasPods {
			change.Detail += "; its group had no Pod to take over, and the takeover deletes such an object"
			report(SeverityInfo, change)
			continue
		}
		judge(change)
	}
}

// ExitCode returns the exit code the comparison stands for.
func (c *Comparison) ExitCode() int {
	switch c.Result {
	case ResultFailed:
		return ExitBlocked
	case ResultReview:
		return ExitAttention
	}
	return ExitReady
}

// WriteTable writes the comparison for a person to read.
func (c *Comparison) WriteTable(w io.Writer) error {
	fmt.Fprintln(w, "Verification of the upgrade to ServingGroup objects")
	fmt.Fprintf(w, "Scope: %s   Baseline: %s   Time: %s\n", c.Scope, c.BaselineCreatedAt.Format(time.RFC3339), c.ComparedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "Compared: %s, %s, %s, %s\n\n",
		counted(c.Summary.ModelServings, "1 ModelServing", "%d ModelServings"), counted(c.Summary.Pods, "1 Pod", "%d Pods"),
		counted(c.Summary.PodGroups, "1 PodGroup", "%d PodGroups"), counted(c.Summary.Services, "1 Service", "%d Services"))

	if len(c.Changes) == 0 {
		fmt.Fprintln(w, "Nothing that the baseline records has changed.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		fmt.Fprintln(tw, "SEVERITY\tCHANGE\tKIND\tNAMESPACE\tNAME\tDETAIL")
		for _, change := range c.Changes {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", change.Severity, change.Change, change.Kind, change.Namespace, change.Name, change.Detail)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	fmt.Fprintf(w, "\nSummary: %s that must not happen, %s that the pre-flight check announced or a change of the spec explains, %s\n",
		counted(c.Summary.Violations, "1 change", "%d changes"), counted(c.Summary.Expected, "1 change", "%d changes"),
		counted(c.Summary.Info, "1 note", "%d notes"))
	_, err := fmt.Fprintf(w, "Result: %s (exit code %d)\n", c.Result, c.ExitCode())
	return err
}
