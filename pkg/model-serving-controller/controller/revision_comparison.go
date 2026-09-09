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
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const rolloutFromRevisionsAnnotation = "modelserving.volcano.sh/rollout-from-revisions"
const rolloutFromRolesAnnotation = "modelserving.volcano.sh/rollout-from-roles"

// revisionTemplates holds a history snapshot for one reconciliation.
// Never cache across reconciliations or infer history from the current template.
type revisionTemplates struct {
	c                       *ModelServingController
	ms                      *workloadv1alpha1.ModelServing
	roles                   map[string][]workloadv1alpha1.Role
	errors                  map[string]error
	roleHashes              map[string]string
	roleMatchesByRevision   map[roleRevision]bool
	missingRolloutRevisions map[string]bool
	missingRolloutRoles     map[string]bool
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
			err = apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "controllerrevisions"}, utils.GenerateControllerRevisionName(h.ms.Name, revision))
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

// instanceTemplate resolves the layout of an existing Role, independently of
// the desired replica count. An entry Pod anchors the identity after restart;
// a newly added worker must not promote the whole Role to its revision.
func (h *revisionTemplates) instanceTemplate(ctx context.Context, groupName, roleName string, observed datastore.Role, pods []*corev1.Pod) (workloadv1alpha1.Role, string, string, error) {
	for _, pod := range pods {
		if pod.Name == utils.GeneratePodName(groupName, observed.Name, 0) && utils.IsOwnedByModelServingWithUID(pod, h.ms.UID) {
			observed.Revision = utils.ObjectRevision(pod)
			observed.RoleTemplateHash = utils.ObjectRoleTemplateHash(pod)
			break
		}
	}
	if observed.Revision == "" {
		observed.Revision, _ = h.c.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName)
	}
	// An exact template hash proves the current template can be used even when
	// this Role kept an older group revision after a different Role was updated.
	for _, desired := range h.ms.Spec.Template.Roles {
		if desired.Name == roleName && observed.RoleTemplateHash == h.desiredRoleHash(desired) && observed.Revision != "" {
			return desired, observed.Revision, observed.RoleTemplateHash, nil
		}
	}
	roles, err := h.get(ctx, observed.Revision)
	if err != nil {
		return workloadv1alpha1.Role{}, "", "", err
	}
	for _, role := range roles {
		if role.Name == roleName {
			hash := observed.RoleTemplateHash
			if hash == "" {
				hash = utils.CalRoleTemplateHash(role)
			}
			return role, observed.Revision, hash, nil
		}
	}
	return workloadv1alpha1.Role{}, "", "", fmt.Errorf("role %s is missing from ControllerRevision %s", roleName, observed.Revision)
}

func (h *revisionTemplates) podMatchesTemplate(ctx context.Context, pod *corev1.Pod, role workloadv1alpha1.Role, revision string) (bool, error) {
	if (revision != "" && utils.ObjectRevision(pod) == revision) || utils.ObjectRoleTemplateHash(pod) == utils.CalRoleTemplateHash(role) {
		return true, nil
	}
	roles, err := h.get(ctx, utils.ObjectRevision(pod))
	if err != nil {
		return false, err
	}
	for _, historical := range roles {
		if historical.Name == role.Name {
			return utils.EqualRoleTemplate(historical, role), nil
		}
	}
	return false, fmt.Errorf("role %s is missing from Pod %s revision", role.Name, pod.Name)
}

// groupRoleTargets protects historical templates while keeping Role replica
// counts independently scalable, including inside the partition prefix.
func (h *revisionTemplates) groupRoleTargets(ctx context.Context, groupName string) ([]workloadv1alpha1.Role, error) {
	partition := h.c.getPartition(h.ms)
	if partition == 0 {
		return h.ms.Spec.Template.Roles, nil
	}
	groups, err := h.c.store.GetServingGroupByModelServing(utils.GetNamespaceName(h.ms))
	if err != nil {
		return nil, err
	}
	for i, group := range groups {
		if group.Name != groupName || i >= partition {
			continue
		}
		roles, err := h.get(ctx, group.Revision)
		if err != nil {
			return nil, err
		}
		return mergeLatestRoleReplicas(roles, h.ms.Spec.Template.Roles), nil
	}
	return h.ms.Spec.Template.Roles, nil
}

// podGroupModelServing changes only the Roles used for scheduling requirements.
// Global scheduling settings remain current. Each live Role type keeps its own
// layout, including during RoleRollingUpdate of other Role types in this group.
func (h *revisionTemplates) podGroupModelServing(ctx context.Context, groupName string) (*workloadv1alpha1.ModelServing, error) {
	if h.c.store == nil {
		return h.ms, nil
	}
	if _, exists := h.c.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName); !exists {
		return h.ms, nil
	}
	roles, err := h.groupRoleTargets(ctx, groupName)
	if err != nil {
		return nil, err
	}
	copy := h.ms.DeepCopy()
	copy.Spec.Template.Roles = nil
	for _, desired := range roles {
		instances, err := h.c.store.GetRoleList(utils.GetNamespaceName(h.ms), groupName, desired.Name)
		if err != nil {
			return nil, err
		}
		effective := desired
		found := false
		for _, instance := range instances {
			if instance.Status == datastore.RoleDeleting {
				continue
			}
			pods, err := h.c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", h.ms.Namespace, groupName, desired.Name, instance.Name))
			if err != nil {
				return nil, err
			}
			template, _, _, err := h.instanceTemplate(ctx, groupName, desired.Name, instance, pods)
			if err != nil {
				return nil, err
			}
			if found && !utils.EqualRoleTemplate(effective, template) {
				// One SubGroupPolicy cannot describe different layouts of the
				// same Role type. Retain its existing requirements until rollout
				// replaces these instances; do not invent a new scheduling layout.
				return nil, fmt.Errorf("role %s in ServingGroup %s has mixed instance templates", desired.Name, groupName)
			}
			effective, found = template, true
		}
		if !found && (h.ms.Spec.RolloutStrategy == nil || h.ms.Spec.RolloutStrategy.Type == workloadv1alpha1.ServingGroupRollingUpdate) {
			revision, _ := h.c.store.GetServingGroupRevision(utils.GetNamespaceName(h.ms), groupName)
			effective, _, _, err = h.instanceTemplate(ctx, groupName, desired.Name, datastore.Role{Revision: revision}, nil)
			if err != nil {
				return nil, err
			}
		}
		effective.Replicas = desired.Replicas
		copy.Spec.Template.Roles = append(copy.Spec.Template.Roles, *effective.DeepCopy())
	}
	return copy, nil
}

func (h *revisionTemplates) instanceMatches(ctx context.Context, group datastore.ServingGroup, observed datastore.Role, desired workloadv1alpha1.Role) (bool, error) {
	matches, err := h.roleMatches(ctx, group.Revision, observed, desired)
	if err != nil || !matches {
		return matches, err
	}
	pods, err := h.c.getPodsByIndex(RoleIDKey, fmt.Sprintf("%s/%s/%s/%s", h.ms.Namespace, group.Name, desired.Name, observed.Name))
	if err != nil {
		return false, err
	}
	for _, pod := range pods {
		matches, err := h.roleMatches(ctx, group.Revision, datastore.Role{Revision: utils.ObjectRevision(pod), RoleTemplateHash: utils.ObjectRoleTemplateHash(pod)}, desired)
		if err != nil || !matches {
			return matches, err
		}
	}
	return true, nil
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

func (h *revisionTemplates) desiredRoleHash(desired workloadv1alpha1.Role) string {
	expectedHash, ok := h.roleHashes[desired.Name]
	if !ok {
		expectedHash = utils.CalRoleTemplateHash(desired)
		h.roleHashes[desired.Name] = expectedHash
	}
	return expectedHash
}

func (h *revisionTemplates) roleMatches(ctx context.Context, groupRevision string, observed datastore.Role, desired workloadv1alpha1.Role) (bool, error) {
	expectedHash := h.desiredRoleHash(desired)
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
		if apierrors.IsNotFound(err) && h.canReplaceMissingRevision(ctx, revision) && h.missingRolloutRoles[revision+"/"+desired.Name] {
			return false, nil
		}
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
			if apierrors.IsNotFound(err) && h.canReplaceMissingRevision(ctx, group.Revision) {
				return false, nil
			}
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
			same, err := h.instanceMatches(ctx, group, role, desired)
			if err != nil || !same {
				return same, err
			}
		}
	}
	return true, nil
}

// Record only missing revisions observed when a persisted previous target proves
// a real template change. Save before advancing status so retries and restarts
// retain that decision. Never fabricate the missing revision's template.
func (h *revisionTemplates) recordMissingRevisionRollout(ctx context.Context, target string) error {
	previous := h.ms.Status.UpdateRevision
	if previous == "" || previous == target {
		return nil
	}
	oldRoles, err := h.get(ctx, previous)
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if utils.EqualRoleTemplates(oldRoles, h.ms.Spec.Template.Roles) {
		return nil
	}
	groups, err := h.c.store.GetServingGroupByModelServing(utils.GetNamespaceName(h.ms))
	if errors.Is(err, datastore.ErrServingGroupNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	missing := map[string]bool{}
	seen := map[string]bool{}
	for _, group := range groups {
		revisions := []string{group.Revision}
		roles, err := h.c.store.GetRolesByGroup(utils.GetNamespaceName(h.ms), group.Name)
		if err != nil {
			return err
		}
		for _, instances := range roles {
			for _, role := range instances {
				revisions = append(revisions, role.Revision)
			}
		}
		for _, revision := range revisions {
			if revision != "" && !seen[revision] {
				seen[revision] = true
				// Only absence needs a recovery decision. Leave validation of an
				// existing snapshot to its group, after partition is applied.
				cr, err := utils.GetControllerRevision(ctx, h.c.kubeClientSet, h.ms, revision)
				if err != nil {
					return err
				}
				if cr == nil {
					missing[revision] = true
				}
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	cr, err := utils.CreateControllerRevision(ctx, h.c.kubeClientSet, h.ms, target, h.ms.Spec.Template.Roles)
	if err != nil {
		return err
	}
	// A revision-level decision is sufficient for ServingGroup rollout, but
	// Role rollout may only replace roles whose templates actually changed.
	roleDecisions := map[string]bool{}
	for _, entry := range strings.Split(cr.Annotations[rolloutFromRolesAnnotation], ",") {
		if entry != "" {
			roleDecisions[entry] = true
		}
	}
	for _, desired := range h.ms.Spec.Template.Roles {
		unchanged := false
		for _, old := range oldRoles {
			if old.Name == desired.Name && utils.EqualRoleTemplate(old, desired) {
				unchanged = true
				break
			}
		}
		if !unchanged {
			for revision := range missing {
				roleDecisions[revision+"/"+desired.Name] = true
			}
		}
	}
	roleEntries := make([]string, 0, len(roleDecisions))
	for entry := range roleDecisions {
		roleEntries = append(roleEntries, entry)
	}
	slices.Sort(roleEntries)
	roleValue := strings.Join(roleEntries, ",")
	for _, revision := range strings.Split(cr.Annotations[rolloutFromRevisionsAnnotation], ",") {
		if revision != "" {
			missing[revision] = true
		}
	}
	revisions := make([]string, 0, len(missing))
	for revision := range missing {
		revisions = append(revisions, revision)
	}
	slices.Sort(revisions)
	value := strings.Join(revisions, ",")
	if cr.Annotations[rolloutFromRevisionsAnnotation] == value && cr.Annotations[rolloutFromRolesAnnotation] == roleValue {
		return nil
	}
	if cr.Annotations == nil {
		cr.Annotations = map[string]string{}
	}
	cr.Annotations[rolloutFromRevisionsAnnotation] = value
	cr.Annotations[rolloutFromRolesAnnotation] = roleValue
	_, err = h.c.kubeClientSet.AppsV1().ControllerRevisions(h.ms.Namespace).Update(ctx, cr, metav1.UpdateOptions{})
	return err
}

func (h *revisionTemplates) canReplaceMissingRevision(ctx context.Context, revision string) bool {
	if h.missingRolloutRevisions == nil {
		h.missingRolloutRevisions = map[string]bool{}
		h.missingRolloutRoles = map[string]bool{}
		target := h.desiredRevision(ctx, utils.Revision(utils.RemoveRoleReplicasForRevision(h.ms).Spec.Template.Roles))
		cr, err := utils.GetControllerRevision(ctx, h.c.kubeClientSet, h.ms, target)
		if err != nil || cr == nil || !metav1.IsControlledBy(cr, h.ms) {
			return false
		}
		roles, err := utils.GetRolesFromControllerRevision(cr)
		if err != nil || !utils.EqualRoleTemplates(roles, h.ms.Spec.Template.Roles) {
			return false
		}
		for _, previous := range strings.Split(cr.Annotations[rolloutFromRevisionsAnnotation], ",") {
			h.missingRolloutRevisions[previous] = true
		}
		for _, entry := range strings.Split(cr.Annotations[rolloutFromRolesAnnotation], ",") {
			h.missingRolloutRoles[entry] = true
		}
	}
	return h.missingRolloutRevisions[revision]
}

func (c *ModelServingController) reportRevisionUnresolved(ms *workloadv1alpha1.ModelServing, group string, err error) {
	// History is read directly, without an informer. Retry even if no workload
	// event follows a transient read failure or a repaired historical revision.
	if c.workqueue != nil {
		c.enqueueModelServingAfter(ms, 5*time.Second)
		klog.V(4).Infof("Requeued ModelServing %s/%s after 5s to retry unresolved history for ServingGroup %s", ms.Namespace, ms.Name, group)
	}
	klog.Warningf("Skipping template update for ModelServing %s/%s, ServingGroup %s: %v", ms.Namespace, ms.Name, group, err)
	if c.recorder != nil {
		c.recorder.Eventf(ms, corev1.EventTypeWarning, "RevisionUnresolved", "Cannot verify historical template for ServingGroup %s: %v", group, err)
	}
}
