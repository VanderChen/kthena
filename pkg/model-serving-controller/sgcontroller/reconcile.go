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

package sgcontroller

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
)

// Reasons of the ReconcileFailed condition and of events.
const (
	ReasonError              = "Error"
	ReasonNameOccupied       = "NameOccupied"
	ReasonOwnerNotFound      = "OwnerNotFound"
	ReasonStatusNotPersisted = "StatusNotPersisted"
)

// errNameOccupied reports that the name of a member is held by an object that
// does not belong to the group.
var errNameOccupied = errors.New("the name is occupied by an object that does not belong to the group")

// OwnerReference returns the controller reference of what a group owns. It
// blocks the deletion of the group, so that a group deleted in the foreground
// is gone only after its members are.
func OwnerReference(group *workloadv1alpha1.ServingGroup) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         workloadv1alpha1.SchemeGroupVersion.String(),
		Kind:               workloadv1alpha1.ServingGroupKind.Kind,
		Name:               group.Name,
		UID:                group.UID,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(true),
	}
}

// Reconcile runs one round for a ServingGroup. The returned duration, when
// positive, is when the next round is due even if nothing changes.
func (c *Controller) Reconcile(ctx context.Context, namespace, name string) (time.Duration, error) {
	group, err := c.caches.ServingGroup(namespace, name)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if group.DeletionTimestamp != nil {
		// The garbage collector removes the members. Nothing is created for a
		// group that is going away.
		c.forget(group.UID, nil)
		return 0, nil
	}
	parent := c.parentOf(group)
	if parent == nil {
		c.recorder.Eventf(group, corev1.EventTypeWarning, ReasonOwnerNotFound,
			"the ModelServing that controls the group does not exist; the group is left alone")
		return 0, nil
	}

	if c.rollingBack() || handingBack(parent) {
		return 0, nil
	}

	now := c.now()
	r := c.decide(observation{group: group, parent: parent, pods: c.caches.PodsOfGroup(group)}, now)
	if r.confirm {
		// What the round is about to do rests on a Pod being absent, terminating
		// or faulty. The cache may be behind: read again and decide again.
		obs, ok, err := c.readDirect(ctx, r.observation)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, nil
		}
		r = c.decide(obs, now)
	}
	return c.execute(ctx, r, now)
}

// handingBack reports whether the workloads of a ModelServing are being
// handed back to a version without group objects. The ModelServing controller
// records that on the ModelServing before it changes the owner of a Pod, and
// this controller reads it from the API server before it changes one: a Pod
// that was handed back is not adopted again.
func handingBack(parent *workloadv1alpha1.ModelServing) bool {
	return parent.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionRollingBack
}

// parentOf returns the ModelServing that controls group, from the cache, or
// nil when there is none.
func (c *Controller) parentOf(group *workloadv1alpha1.ServingGroup) *workloadv1alpha1.ModelServing {
	owner := metav1.GetControllerOfNoCopy(group)
	if owner == nil || owner.Kind != workloadv1alpha1.ModelServingKind.Kind {
		return nil
	}
	parent, err := c.caches.ModelServing(group.Namespace, owner.Name)
	if err != nil || parent.UID != owner.UID || parent.DeletionTimestamp != nil {
		return nil
	}
	return parent
}

// readDirect reads from the API server what obs holds from the cache. ok is
// false when the group, or the ModelServing that controls it, is gone, was
// replaced or is being deleted: the round ends without a write.
func (c *Controller) readDirect(ctx context.Context, obs observation) (observation, bool, error) {
	namespace := obs.group.Namespace
	group, err := c.kthena.WorkloadV1alpha1().ServingGroups(namespace).Get(ctx, obs.group.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return observation{}, false, nil
	}
	if err != nil {
		return observation{}, false, err
	}
	if group.UID != obs.group.UID || group.DeletionTimestamp != nil {
		return observation{}, false, nil
	}
	parent, err := c.kthena.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, obs.parent.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return observation{}, false, nil
	}
	if err != nil {
		return observation{}, false, err
	}
	if parent.UID != obs.parent.UID || parent.DeletionTimestamp != nil || handingBack(parent) {
		return observation{}, false, nil
	}

	selector := labels.SelectorFromSet(labels.Set{workloadv1alpha1.GroupNameLabelKey: group.Name})
	list, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return observation{}, false, err
	}
	pods := make([]*corev1.Pod, 0, len(list.Items))
	listed := sets.New[types.UID]()
	for i := range list.Items {
		pods = append(pods, &list.Items[i])
		listed.Insert(list.Items[i].UID)
	}
	// A Pod the group controls is a member whatever its labels are. One whose
	// labels were changed is not in the list; the cache knows its name.
	for _, cached := range obs.pods {
		if owner := metav1.GetControllerOfNoCopy(cached); owner == nil || owner.UID != group.UID || listed.Has(cached.UID) {
			continue
		}
		pod, err := c.kube.CoreV1().Pods(namespace).Get(ctx, cached.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return observation{}, false, err
		}
		if pod.UID == cached.UID {
			pods = append(pods, pod)
		}
	}
	return observation{group: group, parent: parent, pods: pods}, true, nil
}

// creation is the members one step creates.
type creation struct {
	step     *instanceStep
	ordinals []int
}

// execute carries out a round. The order is fixed: adopt; call the delete
// hooks; write the status, which commits the ledger, a failure, the first
// creation and that an instance is not ready while it is updated in place;
// delete; create; patch in place. A write that fails ends the round: nothing
// that was to follow it is issued.
func (c *Controller) execute(ctx context.Context, r *round, now time.Time) (time.Duration, error) {
	group := r.group
	owner := OwnerReference(group)
	c.forget(group.UID, r.members)

	if r.failure != nil {
		// What an earlier round said about the coordinated rollout stays until
		// a round can say something about it again, unless it was said about
		// another spec.
		coordination := meta.FindStatusCondition(group.Status.Conditions, string(workloadv1alpha1.ServingGroupConditionCoordinatedRoleRolloutBlocked))
		if coordination != nil && coordination.ObservedGeneration != group.Generation {
			coordination = nil
		}
		status := servinggroup.ComputeStatus(servinggroup.StatusInput{
			Group: group, Now: now, Members: r.members, Instances: r.instances,
			Ledger: group.Status.RoleInstances, LayoutUnknown: true, Revisions: r.history,
			// No plan says which update is under way. An instance with a Pod
			// that is patched and not relabelled is in the middle of one.
			Updating:     servinggroup.UnfinishedUpdates(r.instances),
			Coordination: coordination,
			Failure:      &servinggroup.ConditionChange{Reason: ReasonError, Message: r.failure.Error()},
		})
		if _, err := c.writeStatus(ctx, group, status); err != nil {
			return 0, err
		}
		return 0, r.failure
	}

	for i := range r.steps {
		for _, pod := range r.steps[i].step.Adopt {
			if err := c.adopt(ctx, pod, owner); err != nil {
				return 0, fmt.Errorf("adopt Pod %s: %w", pod.Name, err)
			}
		}
	}

	// A Role instance that is drained is forgotten, or created again, only
	// after its delete hook succeeded.
	changes := map[servinggroup.InstanceKey]servinggroup.LedgerChange{}
	drainBlocked := ""
	var creations []creation
	pending := 0
	for i := range r.steps {
		s := &r.steps[i]
		if s.step.RoleDelete {
			if err := c.roleDelete(ctx, r, s, owner); err != nil {
				klog.ErrorS(err, "Role delete hook failed", "servingGroup", klog.KObj(group), "role", s.key.Role, "index", s.key.Index)
				if drainBlocked == "" {
					drainBlocked = fmt.Sprintf("Role instance %s-%d: %v", s.key.Role, s.key.Index, err)
				}
				continue
			}
		}
		if s.step.Ledger != nil {
			changes[s.key] = *s.step.Ledger
		}
		if len(s.step.Create) > 0 {
			creations = append(creations, creation{step: s, ordinals: s.step.Create})
			pending += len(s.step.Create)
		}
	}
	ledger := servinggroup.ApplyLedger(group.Status.RoleInstances, changes)
	if len(changes) == 0 {
		ledger = group.Status.RoleInstances
	}

	// The PodGroup exists before the first member is committed to.
	var failure error
	requeue := time.Duration(0)
	if after, err := c.syncPodGroup(ctx, r, owner); err != nil {
		failure, requeue = err, after
		creations, pending = nil, 0
	}

	input := servinggroup.StatusInput{
		Group: group, Now: now, Members: r.members, Instances: r.instances,
		Ledger: ledger, Wants: r.plan.Wants, Updating: r.updating, Revisions: r.history,
		Fail: r.fail, RecreateRequired: r.recreate, DrainBlocked: drainBlocked,
		Coordination:   r.plan.Coordination.Condition(),
		PendingCreates: pending,
		// First the status says that members are being created, then the first
		// one is. The ModelServing controller changes the revision of a group
		// without a member freely; this write and that change exclude each other.
		CreationCommitted: pending > 0 && len(r.members) == 0 && group.Status.Pods == 0,
	}
	input.Failure = conditionOf(failure)
	group, err := c.writeStatus(ctx, group, servinggroup.ComputeStatus(input))
	if err != nil {
		return 0, err
	}
	input.Group = group
	c.reportInPlace(r)

	if failure == nil {
		failure = c.act(ctx, r, group, owner, creations, now)
	}
	if failure != nil {
		input.Failure = conditionOf(failure)
		if _, err := c.writeStatus(ctx, group, servinggroup.ComputeStatus(input)); err != nil {
			return 0, errors.Join(failure, err)
		}
		if requeue > 0 {
			klog.ErrorS(failure, "Round failed; retrying later", "servingGroup", klog.KObj(group), "after", requeue)
			return requeue, nil
		}
		return 0, failure
	}

	return r.wakeAfter(now), nil
}

// act issues the deletions, the marks, the creations and the in-place patches
// of a round, after its status write.
func (c *Controller) act(ctx context.Context, r *round, group *workloadv1alpha1.ServingGroup, owner metav1.OwnerReference, creations []creation, now time.Time) error {
	for i := range r.steps {
		for _, pod := range r.steps[i].step.Delete {
			if err := c.deletePod(ctx, pod); err != nil {
				return fmt.Errorf("delete Pod %s: %w", pod.Name, err)
			}
		}
	}
	if err := c.promote(ctx, r); err != nil {
		return err
	}
	if err := c.syncRoles(ctx, r, owner); err != nil {
		return err
	}
	for _, creation := range creations {
		if err := c.createMembers(ctx, r, group, owner, creation); err != nil {
			return err
		}
	}
	if err := c.updateInPlace(ctx, r, now); err != nil {
		return err
	}
	return c.notifyReady(ctx, r, owner)
}

func conditionOf(err error) *servinggroup.ConditionChange {
	if err == nil {
		return nil
	}
	reason := ReasonError
	if errors.Is(err, errNameOccupied) {
		reason = ReasonNameOccupied
	}
	return &servinggroup.ConditionChange{Reason: reason, Message: err.Error()}
}

// writeStatus writes status when it differs from the one group carries. The
// write carries the resourceVersion that was read: it fails when anybody wrote
// the group since. It returns the group as it is after the write.
func (c *Controller) writeStatus(ctx context.Context, group *workloadv1alpha1.ServingGroup, status workloadv1alpha1.ServingGroupStatus) (*workloadv1alpha1.ServingGroup, error) {
	if apiequality.Semantic.DeepEqual(group.Status, status) {
		return group, nil
	}
	updated := group.DeepCopy()
	updated.Status = status
	written, err := c.kthena.WorkloadV1alpha1().ServingGroups(group.Namespace).UpdateStatus(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		return nil, fmt.Errorf("write the status of ServingGroup %s: %w", group.Name, err)
	}
	// What follows a status write relies on it. A CustomResourceDefinition that
	// is older than this controller drops the fields it does not know without
	// an error; acting on a write that was not kept would be acting without it.
	if !apiequality.Semantic.DeepEqual(written.Status.RoleInstances, status.RoleInstances) ||
		written.Status.ObservedPods != status.ObservedPods || written.Status.Pods != status.Pods {
		c.recorder.Eventf(group, corev1.EventTypeWarning, ReasonStatusNotPersisted,
			"the API server did not keep the status as written; is the ServingGroup CustomResourceDefinition older than the controller?")
		return nil, fmt.Errorf("the status of ServingGroup %s was not kept as written", group.Name)
	}
	return written, nil
}

// forget drops what the process remembers about Pods of a group that are not
// among members any more.
func (c *Controller) forget(group types.UID, members []*corev1.Pod) {
	live := sets.New[types.UID]()
	for _, pod := range members {
		live.Insert(pod.UID)
	}
	c.readyMu.Lock()
	defer c.readyMu.Unlock()
	known := c.known[group]
	c.faults.Prune(func(uid types.UID) bool { return known.Has(uid) }, live)
	for uid := range c.readyNotified {
		if known.Has(uid) && !live.Has(uid) {
			delete(c.readyNotified, uid)
		}
	}
	if live.Len() == 0 {
		delete(c.known, group)
		delete(c.timeouts, group)
	} else {
		c.known[group] = live
	}
}
