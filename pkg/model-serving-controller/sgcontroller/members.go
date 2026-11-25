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
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/servinggroup"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// patchPod changes the metadata of a Pod with a merge patch that only applies
// to the Pod as it was read: same UID, same resourceVersion.
func (c *Controller) patchPod(ctx context.Context, pod *corev1.Pod, metadata map[string]any) error {
	metadata["uid"] = pod.UID
	metadata["resourceVersion"] = pod.ResourceVersion
	patch, err := json.Marshal(map[string]any{"metadata": metadata})
	if err != nil {
		return err
	}
	_, err = c.kube.CoreV1().Pods(pod.Namespace).Patch(ctx, pod.Name, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

// adopt makes the group the controller of a member Pod it does not own yet: a
// Pod that the ModelServing controls, created before groups were objects, or a
// Pod left without a controller. The Pod is not restarted.
//
// The Pod gets the instance-generation label when it has none. The label is
// what tells, should the Pod lose its owner again, that a group managed it.
func (c *Controller) adopt(ctx context.Context, pod *corev1.Pod, owner metav1.OwnerReference) error {
	references := []metav1.OwnerReference{owner}
	for _, reference := range pod.OwnerReferences {
		if reference.Controller == nil || !*reference.Controller {
			references = append(references, reference)
		}
	}
	metadata := map[string]any{"ownerReferences": references}
	if _, ok := pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey]; !ok {
		metadata["labels"] = map[string]any{workloadv1alpha1.InstanceGenerationLabelKey: "0"}
	}
	return c.patchPod(ctx, pod, metadata)
}

// deletePod deletes the Pod that was read, and no Pod that took its name since.
func (c *Controller) deletePod(ctx context.Context, pod *corev1.Pod) error {
	err := c.kube.CoreV1().Pods(pod.Namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &pod.UID},
	})
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// promote removes the Role surge mark from the members of the instances that
// take a regular place.
func (c *Controller) promote(ctx context.Context, r *round) error {
	for _, key := range r.plan.Promote {
		for i := range r.steps {
			if r.steps[i].key != key {
				continue
			}
			for _, member := range r.steps[i].inst.Members {
				if member.Membership != servinggroup.Owned || member.Pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] != workloadv1alpha1.SurgeScopeRole {
					continue
				}
				err := c.patchPod(ctx, member.Pod, map[string]any{"annotations": map[string]any{workloadv1alpha1.SurgeAnnotationKey: nil}})
				if err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("remove the surge mark of Pod %s: %w", member.Pod.Name, err)
				}
			}
		}
	}
	return nil
}

// groupRevision returns the revision the group as a whole is at, or rolled to.
func groupRevision(group *workloadv1alpha1.ServingGroup) string {
	if roleMode(group) {
		return group.Spec.TargetRevision
	}
	return group.Spec.Revision
}

// syncPodGroup creates or updates the PodGroup of the group. Nothing is done
// when the template of the group cannot be resolved: nothing can be created
// for the group then either.
func (c *Controller) syncPodGroup(ctx context.Context, r *round, owner metav1.OwnerReference) (time.Duration, error) {
	view, err := r.history.At(groupRevision(r.group))
	if err != nil || !c.podGroups.Enabled(view) {
		return 0, nil
	}
	after, err := c.podGroups.SyncPodGroup(ctx, view, owner, r.group.Name)
	if err != nil {
		return after, fmt.Errorf("sync the PodGroup: %w", err)
	}
	return 0, nil
}

// hookRequest returns the plugin request for a Role instance rendered from view.
func (c *Controller) hookRequest(view *workloadv1alpha1.ModelServing, group string, key servinggroup.InstanceKey, owner metav1.OwnerReference, generation int64) *plugins.HookRequest {
	req := &plugins.HookRequest{
		ModelServing:  view,
		ServingGroup:  group,
		RoleName:      key.Role,
		RoleID:        utils.GenerateRoleID(key.Role, key.Index),
		RoleIndex:     key.Index,
		KubeClient:    c.kube,
		ServiceLister: c.services,
		Owner:         &owner,
		Generation:    generation,
	}
	for i := range view.Spec.Template.Roles {
		if view.Spec.Template.Roles[i].Name == key.Role {
			req.Role = view.Spec.Template.Roles[i].DeepCopy()
		}
	}
	return req
}

func (c *Controller) chain(view *workloadv1alpha1.ModelServing) (*plugins.Chain, error) {
	if len(view.Spec.Plugins) == 0 {
		return nil, nil
	}
	return plugins.NewChain(c.registry, view.Spec.Plugins)
}

// roleDelete calls the OnRoleDelete hooks for an instance that is drained. The
// request carries the generation that was drained, so that a hook removes
// nothing that belongs to the instance once it is created again.
func (c *Controller) roleDelete(ctx context.Context, r *round, s *instanceStep, owner metav1.OwnerReference) error {
	view, err := r.history.At(groupRevision(r.group))
	if err != nil {
		// The hooks that clean up are known without the template.
		view = r.history.ModelServing()
	}
	chain, err := plugins.NewRoleDeleteChain(c.registry, view.Spec.Plugins)
	if err != nil {
		return fmt.Errorf("build the Role delete hooks: %w", err)
	}
	return chain.OnRoleDelete(ctx, c.hookRequest(view, r.group.Name, s.key, owner, s.inst.Generation()))
}

// syncRoles calls the OnRoleSync hooks for the instances that are to exist
// and are not being drained.
func (c *Controller) syncRoles(ctx context.Context, r *round, owner metav1.OwnerReference) error {
	for i := range r.steps {
		s := &r.steps[i]
		if s.want == nil || !s.want.LayoutKnown || s.inst.Phase() == workloadv1alpha1.RoleInstanceDraining {
			continue
		}
		view, err := r.history.At(s.want.Revision)
		if err != nil {
			continue
		}
		chain, err := c.chain(view)
		if err != nil {
			return fmt.Errorf("build the plugins of revision %s: %w", s.want.Revision, err)
		}
		if chain == nil {
			continue
		}
		generation := s.inst.Generation()
		if len(s.step.Create) > 0 {
			generation = s.step.Generation
		}
		if err := chain.OnRoleSync(ctx, c.hookRequest(view, r.group.Name, s.key, owner, generation)); err != nil {
			return fmt.Errorf("sync the plugins of Role instance %s-%d: %w", s.key.Role, s.key.Index, err)
		}
	}
	return nil
}

// createMembers creates the missing members of one Role instance, the entry
// Pod first.
func (c *Controller) createMembers(ctx context.Context, r *round, group *workloadv1alpha1.ServingGroup, owner metav1.OwnerReference, creation creation) error {
	s := creation.step
	revision := s.want.Revision
	view, err := r.history.At(revision)
	if err != nil {
		return err
	}
	hash, err := r.history.RoleHash(revision, s.key.Role)
	if err != nil {
		return err
	}
	chain, err := c.chain(view)
	if err != nil {
		return fmt.Errorf("build the plugins of revision %s: %w", revision, err)
	}
	request := c.hookRequest(view, group.Name, s.key, owner, s.step.Generation)
	if request.Role == nil {
		return fmt.Errorf("revision %s has no Role %s", revision, s.key.Role)
	}
	taskName := c.podGroups.GenerateTaskName(s.key.Role, s.key.Index)

	ordinals := append([]int(nil), creation.ordinals...)
	sort.Ints(ordinals)
	for _, ordinal := range ordinals {
		// Rendering changes nested fields of the Role: every Pod gets its own copy.
		role := *request.Role.DeepCopy()
		var pod *corev1.Pod
		if ordinal == 0 {
			pod = utils.GenerateEntryPod(role, view, group.Name, request.RoleID, revision, hash)
		} else if pod = utils.GenerateWorkerPod(role, view, group.Name, request.RoleID, ordinal, revision, hash); pod == nil {
			return fmt.Errorf("Role %s of revision %s has workers and no worker template", s.key.Role, revision)
		}
		pod.OwnerReferences = []metav1.OwnerReference{owner}
		pod.Labels[workloadv1alpha1.InstanceGenerationLabelKey] = strconv.FormatInt(s.step.Generation, 10)
		delete(pod.Annotations, workloadv1alpha1.SurgeAnnotationKey)
		if mark := surgeMark(group, s.want); mark != "" {
			if pod.Annotations == nil {
				pod.Annotations = map[string]string{}
			}
			pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] = mark
		}
		c.podGroups.AnnotatePodWithPodGroup(pod, view, group.Name, taskName)

		if chain != nil {
			hook := *request
			hook.Role, hook.Pod, hook.IsEntry = &role, pod, ordinal == 0
			if err := chain.OnPodCreate(ctx, &hook); err != nil {
				return fmt.Errorf("prepare Pod %s: %w", pod.Name, err)
			}
		}
		if err := c.createPod(ctx, pod, owner); err != nil {
			return err
		}
	}
	return nil
}

// surgeMark returns the surge mark of a member created now: the Role mark for
// temporary capacity of a Role rolling update, else a copy of the mark of the
// group, from which the mark of the group can be restored when the group
// object is lost and its Pods are not.
func surgeMark(group *workloadv1alpha1.ServingGroup, want *servinggroup.Want) string {
	if want.Surge {
		return workloadv1alpha1.SurgeScopeRole
	}
	if group.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup {
		return workloadv1alpha1.SurgeScopeServingGroup
	}
	return ""
}

// createPod creates a member. When the name is taken, only a direct read can
// tell whether by the member itself, which the cache does not show yet, or by
// something else.
func (c *Controller) createPod(ctx context.Context, pod *corev1.Pod, owner metav1.OwnerReference) error {
	_, err := c.kube.CoreV1().Pods(pod.Namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create Pod %s: %w", pod.Name, err)
	}
	existing, err := c.kube.CoreV1().Pods(pod.Namespace).Get(ctx, pod.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("create Pod %s: the name was taken and is free again", pod.Name)
	}
	if err != nil {
		return fmt.Errorf("create Pod %s: %w", pod.Name, err)
	}
	if controller := metav1.GetControllerOfNoCopy(existing); controller != nil && controller.UID == owner.UID {
		return nil
	}
	return fmt.Errorf("create Pod %s: %w", pod.Name, errNameOccupied)
}

// notifyReady calls the OnPodReady hooks once for every member that is ready.
// What was called is remembered in the process: a new process calls again,
// and the hooks have to bear that.
func (c *Controller) notifyReady(ctx context.Context, r *round, owner metav1.OwnerReference) error {
	for i := range r.steps {
		s := &r.steps[i]
		for _, member := range s.inst.Members {
			if member.Membership != servinggroup.Owned || member.State != servinggroup.MemberReady {
				continue
			}
			c.readyMu.Lock()
			notified := c.readyNotified.Has(member.Pod.UID)
			c.readyMu.Unlock()
			if notified {
				continue
			}
			view, err := r.history.At(utils.ObjectRevision(member.Pod))
			if err != nil {
				// The hook belongs to the revision of the Pod; it waits for it.
				continue
			}
			chain, err := c.chain(view)
			if err != nil {
				return fmt.Errorf("build the plugins of revision %s: %w", utils.ObjectRevision(member.Pod), err)
			}
			if chain != nil {
				request := c.hookRequest(view, r.group.Name, s.key, owner, s.inst.Generation())
				request.Pod, request.IsEntry = member.Pod, member.Ordinal == 0
				if err := chain.OnPodReady(ctx, request); err != nil {
					return fmt.Errorf("ready hook of Pod %s: %w", member.Pod.Name, err)
				}
			}
			c.readyMu.Lock()
			c.readyNotified.Insert(member.Pod.UID)
			c.readyMu.Unlock()
		}
	}
	return nil
}
