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
	"sort"
	"sync"
	"time"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/uuid"
)

const creationTokenAnnotation = "workload.kthena.io/creation-token"

var errMemberObservationChanged = errors.New("member observation requires reconciliation before CREATE")

// Business reads/writes hold the owner's lifecycle lock. Only CREATE receipts
// are published by callbacks. This mutex never spans API calls or plugins.
type memberObservations struct {
	mu     sync.Mutex
	owners map[memberOwnerKey]*memberOwnerObservations
}

type memberOwnerKey struct {
	types.NamespacedName
	UID types.UID
}

type memberOwnerObservations struct {
	current  map[string]*corev1.Pod // metadata only; at most one UID per logical slot
	creating map[string]*memberCreateExpectation
}

type memberCreateExpectation struct {
	pod     *corev1.Pod // intended identity, never a Ready/creation-completion fact
	token   string
	receipt *corev1.Pod
}

func memberOwner(ms *api.ModelServing) memberOwnerKey {
	return memberOwnerKey{utils.GetNamespaceName(ms), ms.UID}
}

func (o *memberObservations) ownerLocked(key memberOwnerKey) *memberOwnerObservations {
	if o.owners == nil {
		o.owners = make(map[memberOwnerKey]*memberOwnerObservations)
	}
	if o.owners[key] == nil {
		o.owners[key] = &memberOwnerObservations{current: make(map[string]*corev1.Pod), creating: make(map[string]*memberCreateExpectation)}
	}
	return o.owners[key]
}

func memberIdentity(pod *corev1.Pod) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Namespace: pod.Namespace, Name: pod.Name, UID: pod.UID, ResourceVersion: pod.ResourceVersion,
		Labels: map[string]string{
			api.ModelServingNameLabelKey: pod.Labels[api.ModelServingNameLabelKey],
			api.GroupNameLabelKey:        pod.Labels[api.GroupNameLabelKey],
			api.RoleLabelKey:             utils.GetRoleName(pod), api.RoleIDKey: utils.GetRoleID(pod),
			api.RevisionLabelKey:         utils.ObjectRevision(pod),
			api.RoleTemplateHashLabelKey: utils.ObjectRoleTemplateHash(pod),
		},
		Annotations: map[string]string{
			groupInstanceAnnotation: pod.Annotations[groupInstanceAnnotation],
			roleInstanceAnnotation:  pod.Annotations[roleInstanceAnnotation],
			deletionScopeAnnotation: pod.Annotations[deletionScopeAnnotation],
			creationTokenAnnotation: pod.Annotations[creationTokenAnnotation],
		},
		OwnerReferences: append([]metav1.OwnerReference(nil), pod.OwnerReferences...),
	}}
}

func sameMemberScope(a, b *corev1.Pod) bool {
	return a.Namespace == b.Namespace && a.Name == b.Name &&
		a.Labels[api.GroupNameLabelKey] == b.Labels[api.GroupNameLabelKey] &&
		utils.GetRoleName(a) == utils.GetRoleName(b) && utils.GetRoleID(a) == utils.GetRoleID(b) &&
		a.Annotations[groupInstanceAnnotation] == b.Annotations[groupInstanceAnnotation] &&
		a.Annotations[roleInstanceAnnotation] == b.Annotations[roleInstanceAnnotation] &&
		utils.ObjectRevision(a) == utils.ObjectRevision(b) &&
		utils.ObjectRoleTemplateHash(a) == utils.ObjectRoleTemplateHash(b)
}

// observeKnownMember accepts authoritative API results, never event payloads.
func (c *ModelServingController) observeKnownMember(ms *api.ModelServing, pod *corev1.Pod) {
	if ms == nil || pod == nil || ms.UID == "" || pod.UID == "" ||
		!utils.IsOwnedByModelServingWithUID(pod, ms.UID) || pod.Labels[api.GroupNameLabelKey] == "" || utils.GetRoleID(pod) == "" {
		return
	}
	identity := memberIdentity(pod)
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.ownerLocked(memberOwner(ms))
	in.current[pod.Name] = identity
	delete(in.creating, pod.Name)
}

// prepareMemberCreate runs after plugins and immediately before POST. The
// final CREATE fence must have consumed receipts/settled an earlier request.
func (c *ModelServingController) prepareMemberCreate(ms *api.ModelServing, pod *corev1.Pod) error {
	if ms.UID == "" {
		return nil
	}
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	token := string(uuid.NewUUID())
	pod.Annotations[creationTokenAnnotation] = token
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.ownerLocked(memberOwner(ms))
	// Publication and replacement use the same short lock. A callback arriving
	// after the earlier CREATE fence cannot be erased by this new token.
	for _, expected := range in.creating {
		if expected.receipt != nil {
			return errMemberObservationChanged
		}
	}
	in.creating[pod.Name] = &memberCreateExpectation{pod: memberIdentity(pod), token: token}
	return nil
}

// A matching token proves which still-pending POST produced the notification.
// A callback cannot create an expectation or replace an authoritative UID.
func (c *ModelServingController) observeCreatedPodReceipt(pod *corev1.Pod) {
	if pod == nil || pod.UID == "" || pod.Annotations[creationTokenAnnotation] == "" {
		return
	}
	name := pod.Labels[api.ModelServingNameLabelKey]
	if name == "" {
		return
	}
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "ModelServing" || ref.APIVersion != api.SchemeGroupVersion.String() {
			continue
		}
		key := memberOwnerKey{types.NamespacedName{Namespace: pod.Namespace, Name: name}, ref.UID}
		o := &c.members
		o.mu.Lock()
		in := o.owners[key]
		if in != nil {
			expected := in.creating[pod.Name]
			if expected != nil && expected.token == pod.Annotations[creationTokenAnnotation] && sameMemberScope(expected.pod, pod) &&
				(in.current[pod.Name] == nil || in.current[pod.Name].UID == pod.UID) &&
				(expected.receipt == nil || expected.receipt.UID == pod.UID) {
				expected.receipt = memberIdentity(pod)
			}
		}
		o.mu.Unlock()
		return
	}
}

func (c *ModelServingController) consumeMemberReceipts(ms *api.ModelServing) bool {
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.owners[memberOwner(ms)]
	changed := false
	if in == nil {
		return false
	}
	for name, expected := range in.creating {
		if expected.receipt == nil {
			continue
		}
		if current := in.current[name]; current == nil || current.UID == expected.receipt.UID {
			in.current[name] = expected.receipt
			changed = true
		}
		delete(in.creating, name)
	}
	return changed
}

func (c *ModelServingController) forgetKnownMemberOwner(name types.NamespacedName, keepUID types.UID) {
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	for key := range o.owners {
		if key.NamespacedName == name && (keepUID == "" || key.UID != keepUID) {
			delete(o.owners, key)
		}
	}
}

func memberInScope(pod *corev1.Pod, group, role, instance string) bool {
	return pod.Labels[api.GroupNameLabelKey] == group &&
		(role == "" || utils.GetRoleName(pod) == role && utils.GetRoleID(pod) == instance)
}

func (c *ModelServingController) forgetKnownMemberScope(ms *api.ModelServing, group, role, instance string) {
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	in := o.owners[memberOwner(ms)]
	if in == nil {
		return
	}
	for name, pod := range in.current {
		if memberInScope(pod, group, role, instance) {
			delete(in.current, name)
		}
	}
	for name, expected := range in.creating {
		if memberInScope(expected.pod, group, role, instance) {
			delete(in.creating, name)
		}
	}
}

func (c *ModelServingController) forgetKnownMember(ms *api.ModelServing, pod *corev1.Pod) {
	o := &c.members
	o.mu.Lock()
	defer o.mu.Unlock()
	if in := o.owners[memberOwner(ms)]; in != nil {
		if current := in.current[pod.Name]; current != nil && current.UID == pod.UID {
			delete(in.current, pod.Name)
		}
	}
}

type memberPresence int

const (
	memberUnknown memberPresence = iota
	memberPresent
	memberReplaced
	memberAbsent
)

func (c *ModelServingController) probeMember(ctx context.Context, ms *api.ModelServing, pod *corev1.Pod) (memberPresence, *corev1.Pod, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	current, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return memberAbsent, nil, nil
	}
	if err != nil {
		return memberUnknown, nil, err
	}
	if current.UID != pod.UID {
		return memberReplaced, current, nil
	}
	return memberPresent, current, nil
}

// A new slot is local. An ambiguous POST or a previously observed slot needs a
// GET before retrying CREATE, so an unprocessed physical loss cannot be erased.
func (c *ModelServingController) checkKnownMembersBeforeCreate(ctx context.Context, ms *api.ModelServing, pod *corev1.Pod) error {
	if c.consumeMemberReceipts(ms) {
		return errMemberObservationChanged
	}
	o := &c.members
	o.mu.Lock()
	var pending *memberCreateExpectation
	var known *corev1.Pod
	if in := o.owners[memberOwner(ms)]; in != nil {
		pending = in.creating[pod.Name]
		known = in.current[pod.Name]
	}
	o.mu.Unlock()
	if pending == nil && known == nil {
		return nil
	}
	identity := known
	if pending != nil {
		identity = pending.pod
	}
	state, current, err := c.probeMember(ctx, ms, identity)
	if err != nil {
		return err
	}
	if state == memberPresent || state == memberReplaced {
		if state == memberReplaced && known != nil {
			c.forgetKnownMember(ms, known)
		}
		c.observeKnownMember(ms, current)
		return nil
	}
	if known != nil {
		return errMemberObservationChanged
	}
	// Publication racing the GET must be consumed before changing tokens.
	o.mu.Lock()
	defer o.mu.Unlock()
	if in := o.owners[memberOwner(ms)]; in != nil && in.creating[pod.Name] == pending {
		if pending.receipt != nil {
			in.current[pod.Name] = pending.receipt
			delete(in.creating, pod.Name)
			return errMemberObservationChanged
		}
		delete(in.creating, pod.Name)
	}
	return nil
}

func memberScopeStillDesired(ms *api.ModelServing, pod *corev1.Pod) bool {
	if modelServingReplicas(ms) == 0 {
		return false
	}
	for _, role := range ms.Spec.Template.Roles {
		if role.Name == utils.GetRoleName(pod) {
			return roleReplicas(role) > 0
		}
	}
	return false
}

// processKnownMemberLosses runs with the lifecycle lock, after the normal live
// rollout snapshot and before any CREATE. It never authenticates a loss from
// a selector omission alone. Selection is frozen BEFORE the confirming GET.
func (c *ModelServingController) processKnownMemberLosses(ctx context.Context, ms *api.ModelServing) (bool, error) {
	c.consumeMemberReceipts(ms)
	o := &c.members
	o.mu.Lock()
	var known []*corev1.Pod
	if in := o.owners[memberOwner(ms)]; in != nil {
		for _, pod := range in.current {
			known = append(known, pod)
		}
	}
	o.mu.Unlock()
	sort.Slice(known, func(i, j int) bool { return known[i].Name < known[j].Name })
	var live []*corev1.Pod
	if snapshot, ok := ctx.Value(rolloutPodSnapshotKey{}).(*rolloutPodSnapshot); ok && snapshot.ownerUID == ms.UID && !snapshot.dirty {
		for _, pods := range snapshot.roles {
			live = append(live, pods...)
		}
	} else {
		// Production objects always have a resourceVersion. Existing direct
		// unit callers without API identity do not own a live snapshot.
		if ms.ResourceVersion == "" {
			return false, nil
		}
		return false, fmt.Errorf("known-member recovery requires the current rollout Pod snapshot")
	}
	byName := make(map[string]*corev1.Pod, len(live))
	for _, pod := range live {
		byName[pod.Name] = pod
	}
	for _, pod := range known {
		group, role, instance := pod.Labels[api.GroupNameLabelKey], utils.GetRoleName(pod), utils.GetRoleID(pod)
		if current := byName[pod.Name]; current != nil {
			c.observeKnownMember(ms, current)
			continue
		}
		if !memberScopeStillDesired(ms, pod) {
			c.forgetKnownMember(ms, pod)
			continue
		}
		if c.podDeletionPlanCoversPod(ms, pod) || c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), group) == datastore.ServingGroupDeleting ||
			c.store.GetRoleStatus(utils.GetNamespaceName(ms), group, role, instance) == datastore.RoleDeleting {
			continue
		}
		if mark := pod.Annotations[deletionScopeAnnotation]; mark == deleteRoleScope || mark == deleteGroupScope {
			c.forgetKnownMember(ms, pod)
			continue
		}
		if ms.Spec.RecoveryPolicy == api.NoneRestartPolicy {
			c.forgetKnownMember(ms, pod)
			continue
		}
		stale := false
		selected := []corev1.Pod{*pod}
		for _, sibling := range live {
			if sibling.Labels[api.GroupNameLabelKey] != group {
				continue
			}
			if sibling.Annotations[groupInstanceAnnotation] != pod.Annotations[groupInstanceAnnotation] ||
				(utils.GetRoleName(sibling) == role && utils.GetRoleID(sibling) == instance && sibling.Annotations[roleInstanceAnnotation] != pod.Annotations[roleInstanceAnnotation]) {
				stale = true
				break
			}
			if ms.Spec.RecoveryPolicy == api.ServingGroupRecreate || utils.GetRoleName(sibling) == role && utils.GetRoleID(sibling) == instance {
				selected = append(selected, *sibling.DeepCopy())
			}
		}
		if stale {
			c.forgetKnownMember(ms, pod)
			continue
		}
		state, current, err := c.probeMember(ctx, ms, pod)
		if err != nil {
			return false, err
		}
		if state != memberAbsent {
			if state == memberReplaced {
				c.forgetKnownMember(ms, pod)
			}
			c.observeKnownMember(ms, current)
			continue
		}
		siblings := make([]*corev1.Pod, 0, len(selected)-1)
		for i := 1; i < len(selected); i++ {
			siblings = append(siblings, &selected[i])
		}
		ctx = withObservedPodDeletion(ctx, ms, pod, siblings)
		var recoveryErr error
		if ms.Spec.RecoveryPolicy == api.ServingGroupRecreate {
			recoveryErr = c.deleteServingGroup(ctx, ms, group)
		} else {
			recoveryErr = c.DeleteRole(ctx, ms, group, role, instance)
		}
		c.enqueueModelServingAfter(ms, enqueueAfter)
		return true, recoveryErr
	}
	// These are existing authoritative observations, not new API calls.
	for _, pod := range live {
		c.observeKnownMember(ms, pod)
	}
	return false, nil
}
