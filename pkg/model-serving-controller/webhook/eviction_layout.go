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

package webhook

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// evictionLayout resolves observed layouts, not the latest desired worker count.
// Failures are cached too: unknown history never grants availability credit.
type evictionLayout struct {
	ctx    context.Context
	client kubernetes.Interface
	ms     *api.ModelServing
	roles  map[string][]api.Role
}

func newEvictionLayout(ctx context.Context, client kubernetes.Interface, ms *api.ModelServing) *evictionLayout {
	return &evictionLayout{ctx: ctx, client: client, ms: ms, roles: map[string][]api.Role{}}
}

func (h *EvictionHandler) layoutFor(ms *api.ModelServing) *evictionLayout {
	if h.layout != nil {
		return h.layout
	}
	return newEvictionLayout(context.Background(), h.kubeClient, ms)
}

func (l *evictionLayout) observedRoles(revision string) []api.Role {
	if roles, ok := l.roles[revision]; ok {
		return roles
	}
	l.roles[revision] = nil
	if revision == "" {
		return nil
	}
	// A current content identity is sufficient even before history is recorded.
	// Manual/legacy aliases continue through the owned ControllerRevision lookup.
	if revision == utils.ModelServingRevision(l.ms) {
		l.roles[revision] = l.ms.Spec.Template.Roles
		return l.roles[revision]
	}
	cr, err := utils.GetControllerRevision(l.ctx, l.client, l.ms, revision)
	if err == nil && cr != nil {
		l.roles[revision], err = utils.GetRolesFromControllerRevision(cr)
	}
	if err != nil || cr == nil {
		klog.Warningf("Cannot resolve eviction layout for ModelServing %s/%s revision %s: %v", l.ms.Namespace, l.ms.Name, revision, err)
	}
	return l.roles[revision]
}

func (l *evictionLayout) roleComplete(group, role, roleID string, pods []*corev1.Pod) bool {
	if len(l.ms.Spec.Template.Roles) == 0 {
		return len(pods) > 0
	}
	if len(pods) == 0 || group == "" || roleID == "" {
		return false
	}
	revision := utils.ObjectRevision(pods[0])
	hash := utils.ObjectRoleTemplateHash(pods[0])
	names := map[string]bool{}
	for _, pod := range pods {
		if utils.ObjectRevision(pod) != revision || utils.ObjectRoleTemplateHash(pod) != hash ||
			pod.Labels[api.GroupNameLabelKey] != group || pod.Labels[api.RoleLabelKey] != role || pod.Labels[api.RoleIDKey] != roleID {
			return false
		}
		names[pod.Name] = true
	}
	for _, observed := range l.observedRoles(revision) {
		if observed.Name != role {
			continue
		}
		// Checking identities prevents a duplicate/wrong worker ordinal from filling
		// the hole of an entry or required worker merely by increasing the count.
		for index := 0; index <= int(observed.WorkerReplicas); index++ {
			if !names[utils.GeneratePodName(group, roleID, index)] {
				return false
			}
		}
		return true
	}
	return false
}

func (l *evictionLayout) servingGroupComplete(group string, pods []*corev1.Pod) bool {
	if len(l.ms.Spec.Template.Roles) == 0 {
		return len(pods) > 0
	}
	for _, role := range l.ms.Spec.Template.Roles {
		expected := replicasOrDefault(role.Replicas)
		if expected < 0 {
			return false
		}
		instances := map[string][]*corev1.Pod{}
		for _, pod := range pods {
			if pod.Labels[api.RoleLabelKey] == role.Name {
				id := pod.Labels[api.RoleIDKey]
				instances[id] = append(instances[id], pod)
			}
		}
		complete := 0
		for id, instance := range instances {
			if l.roleComplete(group, role.Name, id, instance) {
				complete++
			}
		}
		if complete < int(expected) {
			return false
		}
	}
	return true
}
