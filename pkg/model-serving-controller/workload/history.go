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

// Package workload resolves the revisions of a ModelServing from its
// ControllerRevisions and builds the ModelServing a ServingGroup is rendered
// from.
package workload

import (
	"errors"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// Getter returns the ControllerRevision of that name, or a NotFound error.
type Getter func(namespace, name string) (*appsv1.ControllerRevision, error)

// UnresolvedError says that a revision cannot be read: its ControllerRevision
// is missing, is not controlled by the ModelServing, or cannot be decoded.
// Only what depends on that revision is blocked by it.
type UnresolvedError struct {
	Revision string
	Err      error
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("revision %q cannot be resolved: %v", e.Revision, e.Err)
}

func (e *UnresolvedError) Unwrap() error { return e.Err }

// IsUnresolved reports whether err is, or wraps, an UnresolvedError.
func IsUnresolved(err error) bool {
	var unresolved *UnresolvedError
	return errors.As(err, &unresolved)
}

// Comparison is the result of comparing the templates of two revisions.
type Comparison int

const (
	// Unknown: one of the revisions cannot be resolved.
	Unknown Comparison = iota
	// Equivalent: the revisions render the same Pods. Two revisions with
	// different identities can be equivalent, for instance after an upgrade that
	// changed how the identity is computed.
	Equivalent
	// Different: the revisions render different Pods.
	Different
)

func (c Comparison) String() string {
	return [...]string{"Unknown", "Equivalent", "Different"}[c]
}

// History resolves the revisions of one ModelServing during one round. What it
// resolved, and what it failed to resolve, is remembered until the round ends,
// so that every decision of the round sees the same history.
type History struct {
	ms          *workloadv1alpha1.ModelServing
	get         Getter
	snapshots   map[string]snapshot
	comparisons map[comparisonKey]Comparison
}

type snapshot struct {
	workload *workloadv1alpha1.ModelServing
	err      error
}

type comparisonKey struct{ revision, target, role string }

// NewHistory returns the history of ms. The settings of ms that are not part
// of a revision (replica counts, rollout and recovery settings) are kept in
// every revision it resolves.
func NewHistory(ms *workloadv1alpha1.ModelServing, get Getter) *History {
	return &History{ms: ms, get: get, snapshots: map[string]snapshot{}, comparisons: map[comparisonKey]Comparison{}}
}

// ModelServing returns the ModelServing the history belongs to.
func (h *History) ModelServing() *workloadv1alpha1.ModelServing { return h.ms }

// At returns the ModelServing as of a revision. The caller owns the result.
func (h *History) At(revision string) (*workloadv1alpha1.ModelServing, error) {
	workload, err := h.at(revision)
	if err != nil {
		return nil, err
	}
	return workload.DeepCopy(), nil
}

// Role returns a Role as of a revision.
func (h *History) Role(revision, roleName string) (*workloadv1alpha1.Role, error) {
	workload, err := h.at(revision)
	if err != nil {
		return nil, err
	}
	for i := range workload.Spec.Template.Roles {
		if workload.Spec.Template.Roles[i].Name == roleName {
			return workload.Spec.Template.Roles[i].DeepCopy(), nil
		}
	}
	return nil, &UnresolvedError{Revision: revision, Err: fmt.Errorf("it has no Role %s", roleName)}
}

// RoleHash returns the template hash of a Role as of a revision: the value of
// the role-template-hash label of the Pods created from it.
func (h *History) RoleHash(revision, roleName string) (string, error) {
	if _, err := h.Role(revision, roleName); err != nil {
		return "", err
	}
	workload, _ := h.at(revision)
	hash, err := utils.RoleRevisionHash(workload, roleName)
	if err != nil {
		return "", &UnresolvedError{Revision: revision, Err: err}
	}
	return hash, nil
}

// Forget drops what is remembered about a revision, so that the next call
// reads it again.
func (h *History) Forget(revision string) {
	delete(h.snapshots, revision)
	for key := range h.comparisons {
		if key.revision == revision || key.target == revision {
			delete(h.comparisons, key)
		}
	}
}

func (h *History) at(revision string) (*workloadv1alpha1.ModelServing, error) {
	if known, ok := h.snapshots[revision]; ok {
		return known.workload, known.err
	}
	workload, err := h.resolve(revision)
	if err != nil {
		err = &UnresolvedError{Revision: revision, Err: err}
	}
	h.snapshots[revision] = snapshot{workload: workload, err: err}
	return workload, err
}

func (h *History) resolve(revision string) (*workloadv1alpha1.ModelServing, error) {
	if revision == "" {
		return nil, fmt.Errorf("the revision is empty")
	}
	cr, err := h.get(h.ms.Namespace, utils.GenerateControllerRevisionName(h.ms.Name, revision))
	if apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("its ControllerRevision does not exist")
	}
	if err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOfNoCopy(cr)
	if owner == nil || owner.UID != h.ms.UID || owner.Kind != workloadv1alpha1.ModelServingKind.Kind ||
		owner.APIVersion != workloadv1alpha1.SchemeGroupVersion.String() {
		return nil, fmt.Errorf("ControllerRevision %s is not controlled by ModelServing %s/%s with UID %s", cr.Name, h.ms.Namespace, h.ms.Name, h.ms.UID)
	}
	workload, err := utils.ModelServingForControllerRevision(h.ms, cr)
	if err != nil {
		return nil, err
	}
	if len(workload.Spec.Template.Roles) == 0 {
		return nil, fmt.Errorf("ControllerRevision %s has no Role template", cr.Name)
	}
	return workload, nil
}

// Compare tells whether a ServingGroup that runs revision is at target.
func (h *History) Compare(revision, target string) Comparison {
	return h.compare(comparisonKey{revision: revision, target: target}, func(left, right *workloadv1alpha1.ModelServing) (bool, error) {
		return utils.EqualModelServingRevisions(revision, target, left, right)
	})
}

// CompareRole tells whether an instance of a Role that runs revision is at target.
func (h *History) CompareRole(revision, target, roleName string) Comparison {
	return h.compare(comparisonKey{revision: revision, target: target, role: roleName}, func(left, right *workloadv1alpha1.ModelServing) (bool, error) {
		leftHash, err := h.RoleHash(revision, roleName)
		if err != nil {
			return false, err
		}
		rightHash, err := h.RoleHash(target, roleName)
		if err != nil {
			return false, err
		}
		return utils.EqualRoleRevisions(leftHash, rightHash, left, right, roleName)
	})
}

func (h *History) compare(key comparisonKey, equal func(left, right *workloadv1alpha1.ModelServing) (bool, error)) Comparison {
	if key.revision == "" || key.target == "" {
		return Unknown
	}
	if key.revision == key.target {
		return Equivalent
	}
	if known, ok := h.comparisons[key]; ok {
		return known
	}
	result := Unknown
	left, leftErr := h.at(key.revision)
	right, rightErr := h.at(key.target)
	if leftErr == nil && rightErr == nil {
		if same, err := equal(left, right); err == nil && same {
			result = Equivalent
		} else if err == nil {
			result = Different
		}
	}
	h.comparisons[key] = result
	return result
}
