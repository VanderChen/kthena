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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// revisionTemplates is a read-only history snapshot for one reconciliation.
// Never cache across reconciliations or infer history from the current template.
type revisionTemplates struct {
	c                     *ModelServingController
	ms                    *workloadv1alpha1.ModelServing
	roles                 map[string][]workloadv1alpha1.Role
	errors                map[string]error
	roleHashes            map[string]string
	roleMatchesByRevision map[roleRevision]bool
}

type roleRevision struct {
	revision string
	name     string
}

type revisionTemplatesKey struct{}

func (c *ModelServingController) templates(ctx context.Context, ms *workloadv1alpha1.ModelServing) *revisionTemplates {
	if history, ok := ctx.Value(revisionTemplatesKey{}).(*revisionTemplates); ok && history.ms == ms {
		return history
	}
	return &revisionTemplates{c: c, ms: ms, roles: make(map[string][]workloadv1alpha1.Role), errors: make(map[string]error),
		roleHashes: make(map[string]string), roleMatchesByRevision: make(map[roleRevision]bool)}
}

func (h *revisionTemplates) get(ctx context.Context, revision string) ([]workloadv1alpha1.Role, error) {
	if roles, ok := h.roles[revision]; ok {
		return roles, nil
	}
	if err, ok := h.errors[revision]; ok {
		return nil, err
	}
	var err error
	var roles []workloadv1alpha1.Role
	if revision == "" || h.c.kubeClientSet == nil {
		err = fmt.Errorf("revision or Kubernetes client is missing")
	} else {
		cr, getErr := utils.GetControllerRevision(ctx, h.c.kubeClientSet, h.ms, revision)
		if getErr != nil {
			err = getErr
		} else if cr == nil {
			err = fmt.Errorf("ControllerRevision %s is missing", revision)
		} else if !metav1.IsControlledBy(cr, h.ms) {
			err = fmt.Errorf("ControllerRevision %s has a different owner", revision)
		} else {
			roles, err = utils.GetRolesFromControllerRevision(cr)
		}
		if err == nil && len(roles) == 0 {
			err = fmt.Errorf("ControllerRevision %s has no roles", revision)
		}
	}
	if err != nil {
		h.errors[revision] = err
		return nil, err
	}
	h.roles[revision] = roles
	return roles, nil
}

// desiredRevision preserves an existing identity when only its hash encoding
// differs. The hash algorithm and the set of fields participating in it stay intact.
func (h *revisionTemplates) desiredRevision(ctx context.Context, computed string) string {
	for _, revision := range []string{h.ms.Status.UpdateRevision, h.ms.Status.CurrentRevision} {
		if revision == "" {
			continue
		}
		if revision == computed {
			return computed
		}
		roles, err := h.get(ctx, revision)
		if err == nil && utils.EqualRoleTemplates(roles, h.ms.Spec.Template.Roles) {
			return revision
		}
	}
	return computed
}

func (h *revisionTemplates) roleMatches(ctx context.Context, groupRevision string, observed datastore.Role, desired workloadv1alpha1.Role) (bool, error) {
	expectedHash, ok := h.roleHashes[desired.Name]
	if !ok {
		expectedHash = utils.CalRoleTemplateHash(desired)
		h.roleHashes[desired.Name] = expectedHash
	}
	if observed.RoleTemplateHash == expectedHash {
		return true, nil
	}
	revision := observed.Revision
	if revision == "" {
		revision = groupRevision
	}
	key := roleRevision{revision: revision, name: desired.Name}
	if matches, ok := h.roleMatchesByRevision[key]; ok {
		return matches, nil
	}
	roles, err := h.get(ctx, revision)
	if err != nil {
		return false, err
	}
	for _, role := range roles {
		if role.Name == desired.Name {
			matches := utils.EqualRoleTemplate(role, desired)
			h.roleMatchesByRevision[key] = matches
			return matches, nil
		}
	}
	return false, fmt.Errorf("role %s is missing from ControllerRevision %s", desired.Name, revision)
}

func (h *revisionTemplates) groupMatches(ctx context.Context, group datastore.ServingGroup, desiredRevision string) (bool, error) {
	if group.Revision != desiredRevision {
		roles, err := h.get(ctx, group.Revision)
		if err != nil {
			return false, err
		}
		if !utils.EqualRoleTemplates(roles, h.ms.Spec.Template.Roles) {
			return false, nil
		}
	}
	// A group's revision alone cannot represent a partially updated set of Roles.
	// Check every observed Role using its own history before declaring it equivalent.
	for _, desired := range h.ms.Spec.Template.Roles {
		roles, err := h.c.store.GetRoleList(utils.GetNamespaceName(h.ms), group.Name, desired.Name)
		if err != nil {
			return false, err
		}
		for _, role := range roles {
			same, err := h.roleMatches(ctx, group.Revision, role, desired)
			if err != nil || !same {
				return same, err
			}
		}
	}
	return true, nil
}

func (c *ModelServingController) reportRevisionUnresolved(ms *workloadv1alpha1.ModelServing, group string, err error) {
	// History is read directly, without an informer. Retry even if no workload
	// event follows a transient read failure or a repaired historical revision.
	if c.workqueue != nil {
		c.enqueueModelServingAfter(ms, 5*time.Second)
	}
	klog.Warningf("Skipping template update for ModelServing %s/%s, ServingGroup %s: %v", ms.Namespace, ms.Name, group, err)
	if c.recorder != nil {
		c.recorder.Eventf(ms, corev1.EventTypeWarning, "RevisionUnresolved", "Cannot verify historical template for ServingGroup %s: %v", group, err)
	}
}
