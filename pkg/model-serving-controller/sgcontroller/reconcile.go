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
	"slices"
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
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/workload"
)

// Reasons of the ReconcileFailed condition and of events.
const (
	ReasonError              = "Error"
	ReasonNameOccupied       = "NameOccupied"
	ReasonOwnerNotFound      = "OwnerNotFound"
	ReasonStatusNotPersisted = "StatusNotPersisted"
	// ReasonPluginError and ReasonRevisionUnresolved are recorded on the
	// ModelServing, where a version without group objects recorded them: a
	// plugin refused a Pod that was to be created, and the revision of a group
	// cannot be resolved.
	ReasonPluginError        = "PluginError"
	ReasonRevisionUnresolved = "RevisionUnresolved"
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
	// Held to the end of the round; the rollback switch is read under it.
	c.rounds.RLock()
	defer c.rounds.RUnlock()

	group, err := c.caches.ServingGroup(namespace, name)
	if apierrors.IsNotFound(err) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// A round that is under way when the group goes away would remember it
	// for good: the event that drops what is remembered has passed by then.
	defer func() {
		if current, err := c.caches.ServingGroup(namespace, name); apierrors.IsNotFound(err) || err == nil && current.UID != group.UID {
			c.drop(group.UID)
		}
	}()
	if group.DeletionTimestamp != nil {
		// The garbage collector removes the members. Nothing is created for a
		// group that is going away.
		c.drop(group.UID)
		c.farewell(ctx, group)
		return 0, nil
	}
	parent, stray := c.parentOf(group)
	if parent == nil {
		if stray != "" {
			c.recorder.Eventf(group, corev1.EventTypeWarning, ReasonOwnerNotFound, "%s; the group is left alone", stray)
		}
		return 0, nil
	}

	if c.rollingBack() || handingBack(parent) {
		return 0, nil
	}

	now := c.now()
	// The Pods of an earlier object of the group are no members. They are
	// part of what the round is decided on: nothing is created beside them.
	pods := append(c.caches.PodsOfGroup(group), c.caches.PodsOfEarlierObjects(group)...)
	r := c.decide(observation{group: group, parent: parent, pods: pods}, now)
	if retry, err := c.syncPlugins(ctx, parent); err != nil {
		// A hook of the ModelServing said that nothing may be done for it:
		// the round creates, deletes and patches nothing, and says why.
		r.failure, r.failureRetry = err, retry
		return c.execute(ctx, r, now)
	}
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

// syncPlugins calls the OnModelServingSync hooks of the ModelServing. Its own
// controller calls them as well, before it creates or deletes a group; a hook
// that fails without asking to continue has to stop what happens inside the
// groups that exist, too. The duration is when such a hook asked to be called
// again; zero when it did not say.
func (c *Controller) syncPlugins(ctx context.Context, parent *workloadv1alpha1.ModelServing) (time.Duration, error) {
	chain, err := c.chain(parent)
	if err != nil {
		return 0, fmt.Errorf("build the plugins: %w", err)
	}
	if chain == nil {
		return 0, nil
	}
	result := chain.OnModelServingSync(ctx, &plugins.HookRequest{ModelServing: parent, KubeClient: c.kube, ServiceLister: c.services})
	if result.Err == nil {
		return 0, nil
	}
	if result.Continue {
		klog.V(2).InfoS("A plugin failed; the round continues", "modelServing", klog.KObj(parent), "err", result.Err)
		return 0, nil
	}
	return max(result.RequeueAfter, 0), fmt.Errorf("plugins of ModelServing %s: %w", parent.Name, result.Err)
}

// farewell calls the OnRoleDelete hooks for the Role instances of a group
// that is being deleted, once its Pods are gone. It is an attempt and no more:
// the object can be gone before the round that would call them. What a plugin
// creates for a group has to be owned by the group, so that it goes with it.
//
// The hooks are for a group whose members are deleted with it. A group that
// is deleted to release its members, as the hand-back to a version without
// group objects does, keeps what belongs to them: the hooks of the headless
// Services would delete Services that were just handed back.
func (c *Controller) farewell(ctx context.Context, group *workloadv1alpha1.ServingGroup) {
	if !slices.Contains(group.Finalizers, metav1.FinalizerDeleteDependents) || c.rollingBack() {
		return
	}
	parent, _ := c.parentOf(group)
	if parent == nil || handingBack(parent) || len(group.Status.RoleInstances) == 0 || len(c.caches.PodsOfGroup(group)) > 0 {
		return
	}
	history := workload.NewHistory(workload.Base(parent, group), c.caches.ControllerRevision)
	view, err := history.At(groupRevision(group))
	if err != nil {
		view = history.ModelServing()
	}
	chain, err := plugins.NewRoleDeleteChain(c.registry, view.Spec.Plugins)
	if err != nil || chain == nil {
		return
	}
	owner := OwnerReference(group)
	for _, entry := range group.Status.RoleInstances {
		key := servinggroup.InstanceKey{Role: entry.Role, Index: int(entry.Index)}
		if err := chain.OnRoleDelete(ctx, c.hookRequest(view, group.Name, key, owner, entry.Generation)); err != nil {
			klog.V(2).InfoS("Role delete hook failed for a group that is being deleted", "servingGroup", klog.KObj(group),
				"role", entry.Role, "index", entry.Index, "err", err)
		}
	}
}

// Role states, as the events on the ModelServing name them.
const (
	roleCreating = "Creating"
	roleRunning  = "Running"
	roleDeleting = "Deleting"
)

// reportRoles records, on the ModelServing, that a Role instance entered a
// state. The events are the ones a version without group objects recorded:
// Creating while its Pods are created or not all ready, Running when every
// Pod of it is ready, Creating again when one stops being ready, Deleting
// when it is drained.
//
// What was reported last is remembered in the process and nowhere else. A new
// process takes an instance it finds running for one that was reported: it
// does not report every instance of every group again.
//
// ledger is the ledger as the round wrote it; creating holds the instances
// the round creates members for.
func (c *Controller) reportRoles(r *round, ledger []workloadv1alpha1.RoleInstanceStatus, creating sets.Set[servinggroup.InstanceKey]) {
	phases := map[servinggroup.InstanceKey]workloadv1alpha1.RoleInstancePhase{}
	for _, entry := range ledger {
		phases[servinggroup.InstanceKey{Role: entry.Role, Index: int(entry.Index)}] = entry.Phase
	}
	states := map[servinggroup.InstanceKey]string{}
	for i := range r.steps {
		s := &r.steps[i]
		phase, recorded := phases[s.key]
		switch {
		case recorded && phase == workloadv1alpha1.RoleInstanceDraining:
			states[s.key] = roleDeleting
		case recorded && s.want != nil && s.want.LayoutKnown && s.inst.Ready(s.want.Workers):
			states[s.key] = roleRunning
		case recorded || len(s.inst.Members) > 0 || creating.Has(s.key):
			states[s.key] = roleCreating
		}
	}

	c.readyMu.Lock()
	reported := c.roleStates[r.group.UID]
	c.roleStates[r.group.UID] = states
	c.readyMu.Unlock()
	for i := range r.steps {
		key := r.steps[i].key
		state, known := states[key]
		before, seen := reported[key]
		if !known || state == before || state == roleRunning && !seen {
			continue
		}
		c.recorder.Eventf(r.parent, corev1.EventTypeNormal, "Role"+state, "Role %s/%s in ServingGroup %s is now %s",
			key.Role, utils.GenerateRoleID(key.Role, key.Index), r.group.Name, state)
	}
}

// reportUnresolved records, on the ModelServing, that the revision of the
// group cannot be resolved: nothing is created from it until it can be. It is
// recorded once for a revision, not in every round that finds it so.
func (c *Controller) reportUnresolved(r *round) {
	revision := groupRevision(r.group)
	_, err := r.history.At(revision)
	c.readyMu.Lock()
	reported, known := c.unresolved[r.group.UID]
	if err == nil {
		delete(c.unresolved, r.group.UID)
	} else {
		c.unresolved[r.group.UID] = revision
	}
	c.readyMu.Unlock()
	if err != nil && !(known && reported == revision) {
		c.recorder.Eventf(r.parent, corev1.EventTypeWarning, ReasonRevisionUnresolved,
			"Cannot resolve revision %q of ServingGroup %s; template-driven creation and deletion are paused for it: %v", revision, r.group.Name, err)
	}
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
// nil when there is none. stray then says what is wrong with a group that
// will stay as it is; it is empty for a group whose ModelServing is being
// deleted or is gone, which the garbage collector deletes.
func (c *Controller) parentOf(group *workloadv1alpha1.ServingGroup) (parent *workloadv1alpha1.ModelServing, stray string) {
	owner := metav1.GetControllerOfNoCopy(group)
	if owner == nil || owner.Kind != workloadv1alpha1.ModelServingKind.Kind {
		return nil, "no ModelServing controls the group"
	}
	// A ModelServing only knows the groups that carry its name and an
	// ordinal. An object under another name that points at it is not counted,
	// rolled or deleted by it, and gets no Pods either.
	if prefix, _, ok := objcache.OrdinalName(group.Name); !ok || prefix != owner.Name {
		return nil, fmt.Sprintf("the name of the group is not of the form %s-<ordinal>, which ModelServing %s takes its groups by", owner.Name, owner.Name)
	}
	parent, err := c.caches.ModelServing(group.Namespace, owner.Name)
	if err != nil || parent.UID != owner.UID || parent.DeletionTimestamp != nil {
		return nil, ""
	}
	return parent, ""
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
			// No plan says what is still to be created. The count the group
			// committed to is kept: members that the cache does not show yet
			// may be among it, and a count of zero would say that the group
			// has none.
			PendingCreates: max(0, int(group.Status.Pods)-len(r.members)),
			Failure:        &servinggroup.ConditionChange{Reason: ReasonError, Message: r.failure.Error()},
		})
		if _, err := c.writeStatus(ctx, group, status); err != nil {
			return 0, err
		}
		if r.failureRetry > 0 {
			// The hook said when it is worth asking again. The delays of the
			// queue grow with every failure; the groups would act long after
			// the hook lets them.
			klog.ErrorS(r.failure, "Round failed; retrying later", "servingGroup", klog.KObj(group), "after", r.failureRetry)
			return r.failureRetry, nil
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
	var hookFailure error
	var creations []creation
	pending := 0
	for i := range r.steps {
		s := &r.steps[i]
		if s.step.RoleDelete {
			if err := c.roleDelete(ctx, r, s, owner); err != nil {
				klog.ErrorS(err, "Role delete hook failed", "servingGroup", klog.KObj(group), "role", s.key.Role, "index", s.key.Index)
				if drainBlocked == "" {
					drainBlocked = fmt.Sprintf("Role instance %s-%d: %v", s.key.Role, s.key.Index, err)
					hookFailure = fmt.Errorf("delete hook of Role instance %s-%d: %w", s.key.Role, s.key.Index, err)
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

	// The PodGroup exists before the first member is committed to. Without it
	// no member is created or patched; what the round deletes it still deletes,
	// or a group that has failed could not be emptied to be replaced.
	requeue := time.Duration(0)
	after, podGroupFailure := c.syncPodGroup(ctx, r, owner)
	if podGroupFailure != nil {
		requeue, creations = after, nil
	}
	// Pods of two objects of one group never exist side by side: the members
	// of an object that was deleted without waiting for them are gone, or
	// released and adopted, before a member is created. Creating each member
	// as soon as its own name is free would mix the two.
	var predecessorFailure error
	if len(r.predecessors) > 0 && len(creations) > 0 {
		creations = nil
		predecessorFailure = fmt.Errorf("%w: %d Pods of an earlier object of the group still exist, among them %s",
			errNameOccupied, len(r.predecessors), r.predecessors[0].Name)
	}

	input := servinggroup.StatusInput{
		Group: group, Now: now, Members: r.members, Instances: r.instances,
		Ledger: ledger, Wants: r.plan.Wants, Updating: r.updating, Revisions: r.history,
		Fail: r.fail, RecreateRequired: r.recreate, DrainBlocked: drainBlocked,
		Coordination: r.plan.Coordination.Condition(),
		// What is still to be created stays in the count although nothing is
		// created in a round without its PodGroup: members that the cache does
		// not show yet may be among it, and a count of zero would say that the
		// group has none.
		PendingCreates: pending,
		// First the status says that members are being created, then the first
		// one is. The ModelServing controller changes the revision of a group
		// without a member freely; this write and that change exclude each other.
		CreationCommitted: len(creations) > 0 && len(r.members) == 0 && group.Status.Pods == 0,
		// Whether the round fails is known after it has acted.
		FailurePending: true,
	}
	group, err := c.writeStatus(ctx, group, servinggroup.ComputeStatus(input))
	if err != nil {
		return 0, err
	}
	input.Group = group
	c.reportUnresolved(r)
	c.reportInPlace(r)
	creating := sets.New[servinggroup.InstanceKey]()
	for _, creation := range creations {
		creating.Insert(creation.step.key)
	}
	c.reportRoles(r, ledger, creating)

	actFailure := c.act(ctx, r, group, owner, creations, now, podGroupFailure != nil)
	failure := errors.Join(podGroupFailure, predecessorFailure, actFailure)

	// The failure of the round is settled once, here. A round that fails the
	// way the one before it did leaves the status as it is; the queue then
	// retries with growing delays, and no write of its own wakes it early.
	input.FailurePending, input.Failure = false, conditionOf(failure)
	if _, err := c.writeStatus(ctx, group, servinggroup.ComputeStatus(input)); err != nil {
		return 0, errors.Join(failure, err)
	}
	switch {
	case podGroupFailure != nil && actFailure == nil && requeue > 0:
		// The PodGroup said when it is worth trying again.
		klog.ErrorS(failure, "Round failed; retrying later", "servingGroup", klog.KObj(group), "after", requeue)
		return requeue, nil
	case failure != nil:
		return 0, failure
	case hookFailure != nil:
		// The instance stays as it is until its hook succeeds. Nothing else
		// would bring the next round.
		return 0, hookFailure
	}
	return r.wakeAfter(now), nil
}

// act issues the deletions, the marks, the creations and the in-place patches
// of a round, after its status write.
//
// deletionsOnly ends it after the deletions: the round has no PodGroup.
func (c *Controller) act(ctx context.Context, r *round, group *workloadv1alpha1.ServingGroup, owner metav1.OwnerReference, creations []creation, now time.Time, deletionsOnly bool) error {
	for i := range r.steps {
		for _, pod := range r.steps[i].step.Delete {
			if err := c.deletePod(ctx, pod); err != nil {
				return fmt.Errorf("delete Pod %s: %w", pod.Name, err)
			}
		}
	}
	if deletionsOnly {
		return nil
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
	if errors.Is(err, errNameOccupied) || errors.Is(err, podgroupmanager.ErrNameOccupied) || errors.Is(err, plugins.ErrNameOccupied) {
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

// drop drops everything the process remembers about a group that is gone or
// going away.
func (c *Controller) drop(group types.UID) {
	c.forget(group, nil)
	c.readyMu.Lock()
	defer c.readyMu.Unlock()
	delete(c.roleStates, group)
	delete(c.unresolved, group)
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
