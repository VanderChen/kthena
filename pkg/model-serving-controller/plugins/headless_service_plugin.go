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

package plugins

import (
	"context"
	"crypto/sha256"
	"fmt"
	"maps"
	"strconv"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

const (
	HeadlessServicePluginName     = "headless-service"
	HeadlessServicePluginLabelKey = "modelserving.volcano.sh/plugin"
)

// RoleCleanup retains only in-process auxiliary deletion targets. It never
// establishes workload membership and is discarded when the Role is gone.
// After restart, normal reconciliation independently identifies orphan resources.
type RoleCleanup struct {
	mu      sync.Mutex
	targets map[string]types.UID
	done    map[string]bool
}

type HeadlessServicePlugin struct {
	BasePlugin
	name string
}

func init() {
	DefaultRegistry.Register(HeadlessServicePluginName, NewHeadlessServicePlugin)
	DefaultRegistry.RegisterRoleDelete(HeadlessServicePluginName)
}

func NewHeadlessServicePlugin(spec workloadv1alpha1.PluginSpec) (Plugin, error) {
	if spec.Scope != nil && spec.Scope.Target != "" && spec.Scope.Target != workloadv1alpha1.PluginTargetAll {
		return nil, fmt.Errorf("scope.target must be All or omitted")
	}
	return &HeadlessServicePlugin{name: spec.Name}, nil
}

func (p *HeadlessServicePlugin) Name() string { return p.name }

// DiscoveryServiceName preserves the entry Service name and bounds the new name
// to a DNS label even when the existing entry name already uses all 63 bytes.
func DiscoveryServiceName(entryName string) string {
	if len(entryName)+len("-discovery") <= 63 {
		return entryName + "-discovery"
	}
	sum := sha256.Sum256([]byte(entryName))
	return fmt.Sprintf("discovery-%x", sum[:24])
}

func (p *HeadlessServicePlugin) OnPodCreate(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil || req.Pod == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	if err := p.ensureService(ctx, req, req.RoleIndex); err != nil {
		return err
	}
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	if !req.lwsPodDNS {
		req.Pod.Spec.Hostname = req.Pod.Name
		req.Pod.Spec.Subdomain = DiscoveryServiceName(entry)
	}
	utils.AddPodEnvVars(req.Pod, corev1.EnvVar{Name: workloadv1alpha1.EntryAddressEnv, Value: entry + "." + req.ModelServing.Namespace})
	return nil
}

func (p *HeadlessServicePlugin) OnRoleSync(ctx context.Context, req *HookRequest) error {
	if req == nil || !requiresHeadlessService(req.Role) {
		return nil
	}
	return p.ensureService(ctx, req, req.RoleIndex)
}

func roleSelector(req *HookRequest) map[string]string {
	return map[string]string{
		workloadv1alpha1.GroupNameLabelKey: req.ServingGroup,
		workloadv1alpha1.RoleLabelKey:      req.RoleName,
		workloadv1alpha1.RoleIDKey:         req.RoleID,
	}
}

func (c *RoleCleanup) capture(ctx context.Context, req *HookRequest) error {
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	if c.targets == nil {
		targets := map[string]types.UID{}
		for _, name := range []string{entry, DiscoveryServiceName(entry)} {
			service, err := services.Get(ctx, name, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				continue
			}
			if err != nil {
				return fmt.Errorf("get Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
			}
			if isManagedHeadlessService(service, req.ModelServing, req.ServingGroup, req.RoleName, req.RoleID) {
				targets[name] = service.UID
			}
		}
		c.targets, c.done = targets, map[string]bool{}
	}
	return nil
}

func (p *HeadlessServicePlugin) OnRoleDelete(ctx context.Context, req *HookRequest) error {
	if req == nil || req.ModelServing == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	if req.Owner != nil {
		return p.deleteServicesOfGeneration(ctx, req)
	}
	cleanup := req.Cleanup
	if cleanup == nil {
		cleanup = &RoleCleanup{}
	}
	cleanup.mu.Lock()
	defer cleanup.mu.Unlock()
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	if err := cleanup.capture(ctx, req); err != nil {
		return err
	}

	for name, uid := range cleanup.targets {
		if cleanup.done[name] {
			continue
		}
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			cleanup.done[name] = true
			continue
		}
		if err != nil {
			return err
		}
		// A retry never transfers an old deletion decision to a replacement UID.
		if service.UID != uid || !isManagedHeadlessService(service, req.ModelServing, req.ServingGroup, req.RoleName, req.RoleID) {
			cleanup.done[name] = true
			continue
		}
		err = services.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
		}
		cleanup.done[name] = true
	}
	return nil
}

func (p *HeadlessServicePlugin) ensureService(ctx context.Context, req *HookRequest, _ int) error {
	if req == nil || req.ModelServing == nil {
		return nil
	}
	if req.KubeClient == nil {
		return fmt.Errorf("Kubernetes client is not initialized")
	}
	ms := req.ModelServing
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	pods, err := req.KubeClient.CoreV1().Pods(ms.Namespace).List(ctx, metav1.ListOptions{LabelSelector: labels.SelectorFromSet(roleSelector(req)).String()})
	if err != nil {
		return err
	}
	for _, pod := range pods.Items {
		if req.ownsPod(&pod) && pod.Spec.Subdomain == entry && pod.Labels[workloadv1alpha1.EntryLabelKey] != "true" {
			return fmt.Errorf("MigrationRequired: Role %s/%s still has worker %s using legacy subdomain %s; create a new ModelServing and migrate traffic", req.ServingGroup, req.RoleID, pod.Name, entry)
		}
	}
	services := req.KubeClient.CoreV1().Services(ms.Namespace)
	for _, name := range []string{entry, DiscoveryServiceName(entry)} {
		selector := roleSelector(req)
		if name == entry {
			selector[workloadv1alpha1.EntryLabelKey] = "true"
		}
		managedLabels := roleSelector(req)
		managedLabels[workloadv1alpha1.ModelServingNameLabelKey] = ms.Name
		managedLabels[HeadlessServicePluginLabelKey] = HeadlessServicePluginName
		if req.Owner != nil {
			managedLabels[workloadv1alpha1.InstanceGenerationLabelKey] = strconv.FormatInt(req.Generation, 10)
		}
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			service, err = services.Create(ctx, &corev1.Service{
				ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ms.Namespace, Labels: managedLabels, OwnerReferences: []metav1.OwnerReference{req.controller()}},
				Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone, Selector: selector, PublishNotReadyAddresses: true},
			}, metav1.CreateOptions{})
			if apierrors.IsAlreadyExists(err) {
				service, err = services.Get(ctx, name, metav1.GetOptions{})
			}
		}
		if err != nil {
			return fmt.Errorf("ensure Headless Service %s/%s: %w", ms.Namespace, name, err)
		}
		if !req.managesService(service) {
			return fmt.Errorf("Headless Service name %s/%s is occupied by an object not managed by plugin %s: %w", ms.Namespace, name, HeadlessServicePluginName, ErrNameOccupied)
		}
		if service.DeletionTimestamp != nil {
			return fmt.Errorf("Headless Service %s/%s is terminating", ms.Namespace, name)
		}
		copy := service.DeepCopy()
		if copy.Labels == nil {
			copy.Labels = map[string]string{}
		}
		maps.Copy(copy.Labels, managedLabels)
		copy.Spec.Selector = selector
		copy.Spec.PublishNotReadyAddresses = true
		handOver := false
		if owner := metav1.GetControllerOfNoCopy(service); req.Owner != nil && (owner == nil || owner.UID != req.Owner.UID) {
			// The Service keeps its UID and its name: DNS does not change.
			copy.OwnerReferences = []metav1.OwnerReference{*req.Owner}
			for _, reference := range service.OwnerReferences {
				if reference.Controller == nil || !*reference.Controller {
					copy.OwnerReferences = append(copy.OwnerReferences, reference)
				}
			}
			handOver = true
		}
		if handOver || !maps.Equal(copy.Labels, service.Labels) || !maps.Equal(selector, service.Spec.Selector) || !service.Spec.PublishNotReadyAddresses {
			// Update carries resourceVersion and UID; conflicts are retried by the queue.
			if _, err := services.Update(ctx, copy, metav1.UpdateOptions{}); err != nil {
				return err
			}
		}
	}
	return nil
}

// deleteServicesOfGeneration deletes the Services of a Role instance of a
// ServingGroup that were made for the generation of the request or an earlier
// one. The generation is on the Service, so a deletion that is retried, also
// by another process, never removes the Service of the instance that was
// created in the meantime.
func (p *HeadlessServicePlugin) deleteServicesOfGeneration(ctx context.Context, req *HookRequest) error {
	services := req.KubeClient.CoreV1().Services(req.ModelServing.Namespace)
	entry := utils.GeneratePodName(req.ServingGroup, req.RoleID, 0)
	for _, name := range []string{entry, DiscoveryServiceName(entry)} {
		service, err := services.Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("get Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
		}
		if !req.managesService(service) || serviceGeneration(service) > req.Generation {
			continue
		}
		uid := service.UID
		err = services.Delete(ctx, name, metav1.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete Headless Service %s/%s: %w", req.ModelServing.Namespace, name, err)
		}
	}
	return nil
}

// serviceGeneration returns the generation of the Role instance a Service was
// made for. A Service made before generations were recorded has none: 0.
func serviceGeneration(service *corev1.Service) int64 {
	generation, err := strconv.ParseInt(service.Labels[workloadv1alpha1.InstanceGenerationLabelKey], 10, 64)
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

// controller returns the controller reference of what a plugin creates for the request.
func (req *HookRequest) controller() metav1.OwnerReference {
	if req.Owner != nil {
		return *req.Owner
	}
	return *metav1.NewControllerRef(req.ModelServing, workloadv1alpha1.ModelServingKind)
}

// ownsPod reports whether a Pod belongs to the workload of the request: the
// ModelServing owns it, or the ServingGroup of the request controls it.
func (req *HookRequest) ownsPod(pod *corev1.Pod) bool {
	if req.Owner != nil {
		if owner := metav1.GetControllerOfNoCopy(pod); owner != nil && owner.UID == req.Owner.UID {
			return true
		}
	}
	return utils.IsOwnedByModelServingWithUID(pod, req.ModelServing.UID)
}

// managesService reports whether a Service is one this plugin made for the
// Role instance of the request. For a ServingGroup that is a Service the group
// controls; one the ModelServing controls, made before groups were objects;
// and one without a controller, left behind when a group was deleted with its
// dependents orphaned. The names and the labels have to be those of the
// instance in every case.
func (req *HookRequest) managesService(service *corev1.Service) bool {
	ms := req.ModelServing
	if req.Owner == nil {
		return isManagedHeadlessService(service, ms, req.ServingGroup, req.RoleName, req.RoleID)
	}
	if !hasHeadlessServiceIdentity(service, ms, req.ServingGroup, req.RoleName, req.RoleID) {
		return false
	}
	owner := metav1.GetControllerOfNoCopy(service)
	if owner == nil {
		return service.Labels[HeadlessServicePluginLabelKey] == HeadlessServicePluginName
	}
	return owner.UID == req.Owner.UID || isModelServingController(owner, ms)
}

func requiresHeadlessService(role *workloadv1alpha1.Role) bool {
	return role != nil && role.WorkerTemplate != nil
}

// ManagedHeadlessServiceIdentity recognizes only known names and complete Role
// identities. A plugin label alone never authorizes adoption or deletion.
func ManagedHeadlessServiceIdentity(service *corev1.Service, ms *workloadv1alpha1.ModelServing) (group, role, id string, ok bool) {
	if service == nil || ms == nil {
		return "", "", "", false
	}
	group, role, id = service.Labels[workloadv1alpha1.GroupNameLabelKey], service.Labels[workloadv1alpha1.RoleLabelKey], service.Labels[workloadv1alpha1.RoleIDKey]
	return group, role, id, isManagedHeadlessService(service, ms, group, role, id)
}

func isManagedHeadlessService(service *corev1.Service, ms *workloadv1alpha1.ModelServing, group, role, id string) bool {
	if service == nil || ms == nil || group == "" || role == "" || id == "" {
		return false
	}
	owner := metav1.GetControllerOfNoCopy(service)
	return owner != nil && isModelServingController(owner, ms) && hasHeadlessServiceIdentity(service, ms, group, role, id)
}

func isModelServingController(owner *metav1.OwnerReference, ms *workloadv1alpha1.ModelServing) bool {
	return owner.UID == ms.UID && owner.Name == ms.Name && owner.Kind == workloadv1alpha1.ModelServingKind.Kind &&
		owner.APIVersion == workloadv1alpha1.SchemeGroupVersion.String()
}

// hasHeadlessServiceIdentity reports whether a Service has the name, the type
// and the labels of a Service of this plugin for one Role instance.
func hasHeadlessServiceIdentity(service *corev1.Service, ms *workloadv1alpha1.ModelServing, group, role, id string) bool {
	if service == nil || ms == nil || group == "" || role == "" || id == "" {
		return false
	}
	parent, ordinal := utils.GetParentNameAndOrdinal(id)
	groupParent, groupOrdinal := utils.GetParentNameAndOrdinal(group)
	if parent != role || ordinal < 0 || groupParent != ms.Name || groupOrdinal < 0 {
		return false
	}
	entry := utils.GeneratePodName(group, id, 0)
	return (service.Name == entry || service.Name == DiscoveryServiceName(entry)) && service.Spec.ClusterIP == corev1.ClusterIPNone &&
		service.Labels[workloadv1alpha1.ModelServingNameLabelKey] == ms.Name && service.Labels[workloadv1alpha1.GroupNameLabelKey] == group &&
		service.Labels[workloadv1alpha1.RoleLabelKey] == role && service.Labels[workloadv1alpha1.RoleIDKey] == id
}
