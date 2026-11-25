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
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// baselineKind marks a file as a baseline, so that another JSON file is not
// mistaken for one.
const baselineKind = "KthenaUpgradeBaseline"

// Baseline records, before the upgrade, what the upgrade must not change:
// the identity and the restarts of every Pod, the identity of every PodGroup
// and headless Service, and the status of every ModelServing.
type Baseline struct {
	Kind          string                 `json:"kind"`
	FormatVersion string                 `json:"formatVersion"`
	CreatedAt     time.Time              `json:"createdAt"`
	Scope         Scope                  `json:"scope"`
	ModelServings []BaselineModelServing `json:"modelServings"`
	Pods          []BaselinePod          `json:"pods"`
	PodGroups     []BaselineObject       `json:"podGroups"`
	Services      []BaselineObject       `json:"services"`
}

// BaselineModelServing is a ModelServing at the time of the baseline.
type BaselineModelServing struct {
	Namespace  string    `json:"namespace"`
	Name       string    `json:"name"`
	UID        types.UID `json:"uid"`
	Generation int64     `json:"generation"`
	Verdict    Verdict   `json:"verdict"`
	// PodChangesExpected lists the findings because of which the controller
	// changes Pods by itself after the upgrade. A ModelServing without any is
	// held to "nothing changes".
	PodChangesExpected []string `json:"podChangesExpected"`
	// TargetRevision is the revision the new controller rolls to.
	TargetRevision string `json:"targetRevision"`
	// SurgeGroups names the groups that are taken over as temporary capacity.
	SurgeGroups []string   `json:"surgeGroups"`
	Status      StatusInfo `json:"status"`
}

// BaselinePod is a Pod at the time of the baseline.
type BaselinePod struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	UID          types.UID `json:"uid"`
	ModelServing string    `json:"modelServing"`
	Group        string    `json:"group,omitempty"`
	Node         string    `json:"node,omitempty"`
	Phase        string    `json:"phase"`
	Revision     string    `json:"revision,omitempty"`
	Terminating  bool      `json:"terminating"`
	Ready        bool      `json:"ready"`
	// ReadyLastTransitionTime is when the Ready condition last changed.
	ReadyLastTransitionTime *time.Time          `json:"readyLastTransitionTime,omitempty"`
	Containers              []BaselineContainer `json:"containers"`
}

// BaselineContainer is the restart count of one container of a Pod.
type BaselineContainer struct {
	Name         string `json:"name"`
	Init         bool   `json:"init,omitempty"`
	RestartCount int32  `json:"restartCount"`
}

// BaselineObject is a PodGroup or a headless Service at the time of the
// baseline.
type BaselineObject struct {
	Namespace    string    `json:"namespace"`
	Name         string    `json:"name"`
	UID          types.UID `json:"uid"`
	ModelServing string    `json:"modelServing"`
	Group        string    `json:"group,omitempty"`
	// GroupHasPods reports that the group of the object had a Pod that was
	// not terminating. Only then is the object taken over with its group;
	// otherwise the takeover leaves it to the ModelServing.
	GroupHasPods bool `json:"groupHasPods"`
}

// NewBaseline records the state a pre-flight report was made from.
func NewBaseline(s *Snapshot, report *Report) *Baseline {
	baseline := &Baseline{
		Kind:          baselineKind,
		FormatVersion: FormatVersion,
		CreatedAt:     s.Time.UTC(),
		Scope:         s.Scope,
		ModelServings: []BaselineModelServing{},
		Pods:          []BaselinePod{},
		PodGroups:     []BaselineObject{},
		Services:      []BaselineObject{},
	}
	known := map[string]bool{}
	groupHasPods := map[string]bool{}
	for _, ms := range report.ModelServings {
		known[ms.Namespace+"/"+ms.Name] = true
		entry := BaselineModelServing{
			Namespace: ms.Namespace, Name: ms.Name, UID: ms.UID, Generation: ms.Generation,
			Verdict: ms.Verdict, PodChangesExpected: ms.PodChangesExpected,
			TargetRevision: ms.Revision.Target, SurgeGroups: []string{}, Status: ms.Status,
		}
		for _, g := range ms.Groups {
			groupHasPods[ms.Namespace+"/"+g.Name] = g.Pods > 0
			if g.SurgeAfterTakeover {
				entry.SurgeGroups = append(entry.SurgeGroups, g.Name)
			}
		}
		baseline.ModelServings = append(baseline.ModelServings, entry)
	}

	for _, pod := range s.Pods {
		name := pod.Labels[workloadv1alpha1.ModelServingNameLabelKey]
		if !known[pod.Namespace+"/"+name] {
			continue
		}
		baseline.Pods = append(baseline.Pods, baselinePod(pod))
	}
	sort.Slice(baseline.Pods, func(i, j int) bool {
		a, b := baseline.Pods[i], baseline.Pods[j]
		return a.Namespace+"/"+a.Name < b.Namespace+"/"+b.Name
	})

	object := func(o metav1.Object) (BaselineObject, bool) {
		name := o.GetLabels()[workloadv1alpha1.ModelServingNameLabelKey]
		group := o.GetLabels()[workloadv1alpha1.GroupNameLabelKey]
		return BaselineObject{
			Namespace: o.GetNamespace(), Name: o.GetName(), UID: o.GetUID(), ModelServing: name, Group: group,
			GroupHasPods: groupHasPods[o.GetNamespace()+"/"+group],
		}, known[o.GetNamespace()+"/"+name]
	}
	for _, podGroup := range s.PodGroups {
		if entry, ok := object(podGroup); ok {
			baseline.PodGroups = append(baseline.PodGroups, entry)
		}
	}
	for _, service := range s.Services {
		if entry, ok := object(service); ok {
			baseline.Services = append(baseline.Services, entry)
		}
	}
	for _, objects := range [][]BaselineObject{baseline.PodGroups, baseline.Services} {
		sort.Slice(objects, func(i, j int) bool {
			return objects[i].Namespace+"/"+objects[i].Name < objects[j].Namespace+"/"+objects[j].Name
		})
	}
	return baseline
}

func baselinePod(pod *corev1.Pod) BaselinePod {
	entry := BaselinePod{
		Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID,
		ModelServing: pod.Labels[workloadv1alpha1.ModelServingNameLabelKey],
		Group:        pod.Labels[workloadv1alpha1.GroupNameLabelKey],
		Node:         pod.Spec.NodeName, Phase: string(pod.Status.Phase),
		Revision:    pod.Labels[workloadv1alpha1.RevisionLabelKey],
		Terminating: pod.DeletionTimestamp != nil,
		Containers:  []BaselineContainer{},
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			entry.Ready = condition.Status == corev1.ConditionTrue
			if !condition.LastTransitionTime.IsZero() {
				since := condition.LastTransitionTime.UTC()
				entry.ReadyLastTransitionTime = &since
			}
		}
	}
	for _, status := range pod.Status.InitContainerStatuses {
		entry.Containers = append(entry.Containers, BaselineContainer{Name: status.Name, Init: true, RestartCount: status.RestartCount})
	}
	for _, status := range pod.Status.ContainerStatuses {
		entry.Containers = append(entry.Containers, BaselineContainer{Name: status.Name, RestartCount: status.RestartCount})
	}
	return entry
}

// WriteBaseline writes the baseline to a file as indented JSON.
func WriteBaseline(path string, baseline *Baseline) error {
	var data bytes.Buffer
	if err := WriteJSON(&data, baseline); err != nil {
		return fmt.Errorf("failed to encode the baseline: %v", err)
	}
	if err := os.WriteFile(path, data.Bytes(), 0o644); err != nil {
		return fmt.Errorf("failed to write the baseline: %v", err)
	}
	return nil
}

// ReadBaseline reads a baseline that WriteBaseline wrote.
func ReadBaseline(path string) (*Baseline, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read the baseline: %v", err)
	}
	baseline := &Baseline{}
	if err := json.Unmarshal(data, baseline); err != nil {
		return nil, fmt.Errorf("failed to decode the baseline %s: %v", path, err)
	}
	if baseline.Kind != baselineKind {
		return nil, fmt.Errorf("%s is not a baseline written with --baseline-file", path)
	}
	if baseline.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("the baseline %s has format version %q; this version of kthena reads %q", path, baseline.FormatVersion, FormatVersion)
	}
	return baseline, nil
}
