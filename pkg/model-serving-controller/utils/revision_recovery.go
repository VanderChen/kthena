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
	"context"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// RevisionRebuiltFromGenerationAnnotation marks a ControllerRevision whose data
// was rebuilt from the live ModelServing spec because the original history was
// missing. Its value is the spec generation the data was taken from.
const RevisionRebuiltFromGenerationAnnotation = "modelserving.volcano.sh/revision-rebuilt-from-generation"

// RebuildModelServingRevision persists the current template of ms under an
// identity that live Pods already carry but no ControllerRevision describes.
// Callers must first prove that the Pods really run the current template; this
// function only makes that decision durable.
//
// The rebuilt object takes the highest numeric Revision so that
// RecordModelServingRevision prefers the running identity over the canonical
// hash it may already have persisted for the same data, mirroring how a legacy
// baseline keeps the desired identity on the Pods' label.
func RebuildModelServingRevision(ctx context.Context, client kubernetes.Interface, ms *workloadv1alpha1.ModelServing, revision string) (*appsv1.ControllerRevision, error) {
	if ms == nil || revision == "" {
		return nil, fmt.Errorf("model serving or revision is missing")
	}
	data, err := BuildRevisionData(ms)
	if err != nil {
		return nil, err
	}
	if _, err := decodeRevisionPatch(data); err != nil {
		return nil, err
	}
	name := GenerateControllerRevisionName(ms.Name, revision)
	revisions := client.AppsV1().ControllerRevisions(ms.Namespace)
	validate := func(existing *appsv1.ControllerRevision) (*appsv1.ControllerRevision, error) {
		if !controlledByModelServing(existing, ms) ||
			existing.Annotations[ControllerRevisionDataVersionAnnotation] != ControllerRevisionDataVersionV1 ||
			existing.Labels[ControllerRevisionRevisionLabelKey] != revision ||
			!revisionDataEqual(existing.Data.Raw, data) {
			return nil, fmt.Errorf("ControllerRevision %s already exists with a different identity or template", name)
		}
		return existing, nil
	}

	history, err := revisions.List(ctx, metav1.ListOptions{
		LabelSelector: labels.SelectorFromSet(map[string]string{ControllerRevisionLabelKey: ms.Name}).String(),
	})
	if err != nil {
		return nil, fmt.Errorf("list controller revisions: %w", err)
	}
	var maxRevision int64
	for i := range history.Items {
		if controlledByModelServing(&history.Items[i], ms) && history.Items[i].Revision > maxRevision {
			maxRevision = history.Items[i].Revision
		}
	}

	rebuilt := &appsv1.ControllerRevision{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ms.Namespace,
			Labels: map[string]string{
				ControllerRevisionLabelKey:         ms.Name,
				ControllerRevisionRevisionLabelKey: revision,
			},
			Annotations: map[string]string{
				ControllerRevisionDataVersionAnnotation: ControllerRevisionDataVersionV1,
				RevisionRebuiltFromGenerationAnnotation: strconv.FormatInt(ms.Generation, 10),
			},
			OwnerReferences: []metav1.OwnerReference{newModelServingOwnerRef(ms)},
		},
		Revision: maxRevision + 1,
		Data:     runtime.RawExtension{Raw: data},
	}
	created, err := revisions.Create(ctx, rebuilt, metav1.CreateOptions{})
	if apierrors.IsAlreadyExists(err) {
		existing, getErr := revisions.Get(ctx, name, metav1.GetOptions{})
		if getErr != nil {
			return nil, fmt.Errorf("get concurrently created ControllerRevision %s: %w", name, getErr)
		}
		return validate(existing)
	}
	if err != nil {
		return nil, fmt.Errorf("create ControllerRevision %s: %w", name, err)
	}
	klog.Warningf("Rebuilt missing ControllerRevision %s/%s for ModelServing %s from spec generation %d", ms.Namespace, name, ms.Name, ms.Generation)
	return created, nil
}
