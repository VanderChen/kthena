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
	"encoding/json"
	"fmt"

	api "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const createdRolesKey = "created.json"

// Completion survives loss of every Pod in a Role. Incarnations fence facts
// from a previous occupant of the same SG/Role name. This grants no Ready credit.
type createdRole struct {
	Role          string `json:"role"`
	GroupInstance string `json:"groupInstance"`
	RoleInstance  string `json:"roleInstance"`
	Revision      string `json:"revision"`
	Hash          string `json:"hash"`
}
type createdRoles map[string]map[string]createdRole

func readCreatedRoles(cm *corev1.ConfigMap) (createdRoles, error) {
	facts := createdRoles{}
	if cm != nil && cm.Data[createdRolesKey] != "" {
		if err := json.Unmarshal([]byte(cm.Data[createdRolesKey]), &facts); err != nil {
			return nil, err
		}
	}
	if facts == nil {
		facts = createdRoles{}
	}
	for group, roles := range facts {
		for id, fact := range roles {
			parent, ordinal := utils.GetParentNameAndOrdinal(id)
			if group == "" || parent != fact.Role || ordinal < 0 || fact.Revision == "" || fact.Hash == "" {
				return nil, fmt.Errorf("invalid completed Role identity %s/%s", group, id)
			}
		}
	}
	return facts, nil
}

func (c *ModelServingController) persistRoleCreated(ctx context.Context, ms *api.ModelServing, group, role, instance string, pods []*corev1.Pod) error {
	observed := datastore.Role{Name: instance, Revision: utils.ObjectRevision(pods[0]), RoleTemplateHash: utils.ObjectRoleTemplateHash(pods[0])}
	template, revision, hash, err := c.roleTemplateForInstance(ctx, ms, group, role, observed, pods)
	if err != nil {
		return err
	}
	fact := createdRole{role, pods[0].Annotations[groupInstanceAnnotation], pods[0].Annotations[roleInstanceAnnotation], revision, hash}
	names := map[string]bool{}
	for _, pod := range pods {
		if pod.DeletionTimestamp != nil ||
			pod.Annotations[groupInstanceAnnotation] != fact.GroupInstance || pod.Annotations[roleInstanceAnnotation] != fact.RoleInstance ||
			utils.ObjectRevision(pod) != revision || (utils.ObjectRoleTemplateHash(pod) != "" && utils.ObjectRoleTemplateHash(pod) != hash) {
			return fmt.Errorf("cannot record incomplete or mixed Role %s/%s", group, instance)
		}
		names[pod.Name] = true
	}
	if len(names) != 1+int(template.WorkerReplicas) {
		return fmt.Errorf("cannot record incomplete Role %s/%s", group, instance)
	}
	for i := 0; i <= int(template.WorkerReplicas); i++ {
		if !names[utils.GeneratePodName(group, instance, i)] {
			return fmt.Errorf("cannot record absent member of Role %s/%s", group, instance)
		}
	}
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil {
		return err
	}
	facts, err := readCreatedRoles(cm)
	if err != nil {
		return err
	}
	if previous, ok := facts[group][instance]; ok && previous == fact {
		return nil
	}
	// A new SG incarnation cannot inherit completion of any old member.
	for _, previous := range facts[group] {
		if previous.GroupInstance != fact.GroupInstance {
			delete(facts, group)
			break
		}
	}
	if facts[group] == nil {
		facts[group] = map[string]createdRole{}
	}
	facts[group][instance] = fact
	raw, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	creating := cm == nil
	if creating {
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: groupMembersStateName(ms), Namespace: ms.Namespace,
			Labels:          map[string]string{api.ModelServingNameLabelKey: ms.Name},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(ms, api.SchemeGroupVersion.WithKind("ModelServing"))}}}
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[createdRolesKey] = string(raw)
	if creating {
		_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Create(ctx, cm, metav1.CreateOptions{})
	} else {
		_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	}
	return err
}

// Intentional Role/ServingGroup deletion removes completion facts before the
// first destructive request. A later restart then derives recovery solely from
// the Pods that actually survived.
func removeCreatedRoles(cm *corev1.ConfigMap, scope, group, instance string) error {
	facts, err := readCreatedRoles(cm)
	if err != nil {
		return err
	}
	if scope == deleteGroupScope {
		delete(facts, group)
	} else {
		delete(facts[group], instance)
		if len(facts[group]) == 0 {
			delete(facts, group)
		}
	}
	raw, err := json.Marshal(facts)
	if err != nil {
		return err
	}
	if cm.Data == nil {
		cm.Data = map[string]string{}
	}
	cm.Data[createdRolesKey] = string(raw)
	return nil
}

func (c *ModelServingController) clearCreatedRoles(ctx context.Context, ms *api.ModelServing, scope, group, instance string) error {
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil || cm == nil {
		return err
	}
	previous := cm.Data[createdRolesKey]
	if err := removeCreatedRoles(cm, scope, group, instance); err != nil {
		return err
	}
	if cm.Data[createdRolesKey] == previous || (previous == "" && cm.Data[createdRolesKey] == "{}") {
		return nil
	}
	_, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, cm, metav1.UpdateOptions{})
	return err
}

func (c *ModelServingController) restoreCreatedRoles(ctx context.Context, ms *api.ModelServing) error {
	if ms.ResourceVersion == "" {
		return nil
	}
	cm, err := c.readGroupMembersState(ctx, ms)
	if err != nil {
		return err
	}
	facts, err := readCreatedRoles(cm)
	if err != nil || len(facts) == 0 {
		return err
	}
	desired := map[string]bool{}
	for _, role := range ms.Spec.Template.Roles {
		desired[role.Name] = roleReplicas(role) > 0
	}
	pruned := false
	for group, roles := range facts {
		for instance, fact := range roles {
			if !desired[fact.Role] {
				delete(roles, instance)
				pruned = true
			}
		}
		if len(roles) == 0 {
			delete(facts, group)
		}
	}
	if pruned {
		raw, marshalErr := json.Marshal(facts)
		if marshalErr != nil {
			return marshalErr
		}
		cm.Data[createdRolesKey] = string(raw)
		if _, err = c.kubeClientSet.CoreV1().ConfigMaps(ms.Namespace).Update(ctx, cm, metav1.UpdateOptions{}); err != nil {
			return err
		}
	}
	ctx, err = c.withRolloutPodSnapshot(ctx, ms)
	if err != nil {
		return err
	}
	snapshot := ctx.Value(rolloutPodSnapshotKey{}).(*rolloutPodSnapshot)
	groups := map[string][]*corev1.Pod{}
	for _, pods := range snapshot.roles {
		for _, pod := range pods {
			group := pod.Labels[api.GroupNameLabelKey]
			groups[group] = append(groups[group], pod)
		}
	}
	key := utils.GetNamespaceName(ms)
	for group, roles := range facts {
		if c.store.GetServingGroupStatus(key, group) == datastore.ServingGroupDeleting || len(groups[group]) == 0 {
			continue
		}
		for instance, fact := range roles {
			if c.store.GetRoleStatus(key, group, fact.Role, instance) == datastore.RoleDeleting {
				continue
			}
			same := true
			for _, pod := range groups[group] {
				if pod.Annotations[groupInstanceAnnotation] != fact.GroupInstance || pod.DeletionTimestamp != nil {
					same = false
					break
				}
				if utils.GetRoleID(pod) == instance && (pod.Annotations[roleInstanceAnnotation] != fact.RoleInstance || utils.ObjectRevision(pod) != fact.Revision || (utils.ObjectRoleTemplateHash(pod) != "" && utils.ObjectRoleTemplateHash(pod) != fact.Hash)) {
					same = false
					break
				}
			}
			if !same {
				continue
			}
			c.store.AddRole(key, group, fact.Role, instance, fact.Revision, fact.Hash)
			if err := c.store.MarkRoleInitialized(key, group, fact.Role, instance); err != nil {
				return err
			}
		}
	}
	return nil
}
