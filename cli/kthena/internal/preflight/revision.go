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

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// target is the revision the new ModelServing controller resolves the
// current template of a ModelServing to: the revision it rolls to.
type target struct {
	// identity is the value the revision label of an up-to-date Pod carries.
	identity string
	// existing reports that a ControllerRevision of the ModelServing renders
	// the same Pods as the template and that its identity is reused.
	existing bool
	// name is the name of the ControllerRevision of the target.
	name string
	// history compares revisions with the code the new controllers run.
	history *workload.History
}

// resolveTarget predicts what utils.RecordModelServingRevision returns for
// ms, without writing. The new ModelServing controller calls that function
// to obtain the target revision: a ControllerRevision of ms that renders the
// same Pods as the template is reused under the identity it has, whatever
// version recorded it; only when there is none is the identity computed from
// the template.
//
// revisions are the ControllerRevisions that carry the name label of ms.
func resolveTarget(ms *workloadv1alpha1.ModelServing, revisions []*appsv1.ControllerRevision) (target, error) {
	data, err := utils.BuildRevisionData(ms)
	if err != nil {
		return target{}, err
	}
	// A ModelServing has to pass this check of RecordModelServingRevision too.
	if len(ms.Spec.Template.Roles) == 0 {
		return target{}, fmt.Errorf("the template has no Role")
	}
	computed := utils.ModelServingRevision(ms)

	byName := map[string]*appsv1.ControllerRevision{}
	var equivalent *appsv1.ControllerRevision
	for _, revision := range revisions {
		if revision.Namespace != ms.Namespace || revision.Labels[utils.ControllerRevisionLabelKey] != ms.Name {
			continue
		}
		byName[revision.Name] = revision
		owner := metav1.GetControllerOfNoCopy(revision)
		if owner == nil || owner.UID != ms.UID {
			continue
		}
		identity := revision.Labels[utils.ControllerRevisionRevisionLabelKey]
		if identity == "" || revision.Name != utils.GenerateControllerRevisionName(ms.Name, identity) {
			continue
		}
		// It rejects a data version it does not know and an owner that is not ms.
		recorded, err := utils.ModelServingForControllerRevision(ms, revision)
		if err != nil {
			continue
		}
		same, err := utils.EqualModelServingRevisions(identity, computed, recorded, ms)
		if err != nil || !same {
			continue
		}
		if equivalent == nil || revisionLess(equivalent, revision) {
			equivalent = revision
		}
	}

	t := target{}
	if equivalent != nil {
		t.identity = equivalent.Labels[utils.ControllerRevisionRevisionLabelKey]
		t.name = equivalent.Name
		t.existing = true
	} else {
		// A name that is taken by other data is avoided by counting collisions.
		collisions := int32(0)
		if ms.Status.CollisionCount != nil {
			collisions = *ms.Status.CollisionCount
		}
		for {
			t.identity = utils.RevisionDataHash(data, &collisions)
			t.name = utils.GenerateControllerRevisionName(ms.Name, t.identity)
			if _, taken := byName[t.name]; !taken {
				break
			}
			collisions++
		}
		// The ControllerRevision the controller would create, so that the
		// history can compare the Pods with a target that is not recorded yet.
		byName[t.name] = &appsv1.ControllerRevision{
			ObjectMeta: metav1.ObjectMeta{
				Name:      t.name,
				Namespace: ms.Namespace,
				Labels: map[string]string{
					utils.ControllerRevisionLabelKey:         ms.Name,
					utils.ControllerRevisionRevisionLabelKey: t.identity,
				},
				Annotations: map[string]string{
					utils.ControllerRevisionDataVersionAnnotation: utils.ControllerRevisionDataVersionV1,
				},
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, workloadv1alpha1.ModelServingKind)},
			},
			Data: runtime.RawExtension{Raw: data},
		}
	}

	t.history = workload.NewHistory(ms, func(namespace, name string) (*appsv1.ControllerRevision, error) {
		if revision, ok := byName[name]; ok && namespace == ms.Namespace {
			return revision, nil
		}
		return nil, apierrors.NewNotFound(appsv1.Resource("controllerrevisions"), name)
	})
	return t, nil
}

// revisionLess orders ControllerRevisions like Kubernetes controller history
// does: numeric revision, creation timestamp, then name.
func revisionLess(left, right *appsv1.ControllerRevision) bool {
	if left.Revision != right.Revision {
		return left.Revision < right.Revision
	}
	if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
		return left.CreationTimestamp.Before(&right.CreationTimestamp)
	}
	return left.Name < right.Name
}

// runningRevision returns the revision a Pod counts as: the revision label,
// or the revision an unfinished in-place update is taking it to.
func runningRevision(pod *corev1.Pod) string {
	if state := inplace.StateOf(pod); state.InProgress() && state.Revision != "" {
		return state.Revision
	}
	return utils.ObjectRevision(pod)
}

// why says why a revision cannot be compared with the target.
func (t target) why(revision string) string {
	if revision == "" {
		return "a Pod has no revision label"
	}
	if _, err := t.history.At(revision); err != nil {
		return err.Error()
	}
	if _, err := t.history.At(t.identity); err != nil {
		return err.Error()
	}
	return fmt.Sprintf("revision %q cannot be compared with revision %q", revision, t.identity)
}
