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
	"reflect"
	"sort"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	pglisters "volcano.sh/apis/pkg/client/listers/scheduling/v1beta1"

	mslisters "github.com/volcano-sh/kthena/client-go/listers/workload/v1alpha1"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// auditRuntime owns only short, in-memory notification bookkeeping. Kubernetes
// requests, hooks and datastore mutations are made by the workqueue worker.
type auditRuntime struct {
	mutex    sync.Mutex
	requests map[string]*auditRequest
	states   sync.Map // key: namespace/name; value: *servingAuditState, worker-owned
	metrics  *auditMetrics
}

type auditRequest struct {
	live            bool
	deletedMS       *workloadv1alpha1.ModelServing
	deletedPods     map[types.UID]*corev1.Pod
	cleanupTopology bool
}

type podHookProgress struct {
	pod            *corev1.Pod
	runningVersion string
	ready          bool
}

type servingAuditState struct {
	uid              types.UID
	ms               *workloadv1alpha1.ModelServing
	live             bool
	pods             map[string]*podHookProgress
	deleted          map[types.UID]*corev1.Pod
	completedDeletes map[types.UID]time.Time
	grace            map[types.UID]time.Time
	roleDeletes      map[roleCleanupKey]struct{}
	groupDeletes     map[string]struct{}
	pendingSince     time.Time
}

type roleCleanupKey struct{ group, role, id string }

type servingObservation struct {
	pods      cache.Indexer
	services  cache.Indexer
	podGroups cache.Indexer
	live      bool
}

func newAuditRuntime() *auditRuntime {
	return &auditRuntime{requests: make(map[string]*auditRequest), metrics: newAuditMetrics()}
}

func newServingAuditState(ms *workloadv1alpha1.ModelServing) *servingAuditState {
	return &servingAuditState{uid: ms.UID, ms: ms.DeepCopy(), pods: make(map[string]*podHookProgress),
		deleted: make(map[types.UID]*corev1.Pod), completedDeletes: make(map[types.UID]time.Time), grace: make(map[types.UID]time.Time),
		roleDeletes: make(map[roleCleanupKey]struct{}), groupDeletes: make(map[string]struct{})}
}

// ConfigureAudit must be called before Run. Zero disables periodic auditing,
// but does not disable event processing, retry or targeted live verification.
func (c *ModelServingController) ConfigureAudit(period, timeout time.Duration) error {
	if period < 0 || timeout <= 0 {
		return fmt.Errorf("audit period must be non-negative and timeout positive")
	}
	c.auditPeriod, c.auditTimeout = period, timeout
	return nil
}

func (c *ModelServingController) rootController() *ModelServingController {
	if c.shared != nil {
		return c.shared
	}
	return c
}

func (c *ModelServingController) operationContext() context.Context {
	if c.reconcileContext != nil {
		return c.reconcileContext
	}
	return context.Background()
}

func (c *ModelServingController) requestAudit(key string, update func(*auditRequest)) {
	c = c.rootController()
	if key == "" || c.audit == nil {
		return
	}
	c.audit.mutex.Lock()
	r := c.audit.requests[key]
	if r == nil {
		r = &auditRequest{deletedPods: make(map[types.UID]*corev1.Pod)}
		c.audit.requests[key] = r
	}
	update(r)
	mode := "cache"
	if r.live {
		mode = "live"
	}
	c.audit.metrics.triggers.WithLabelValues(mode).Inc()
	c.audit.mutex.Unlock()
	c.workqueue.Add(key)
}

func (c *ModelServingController) queueChildObservation(obj interface{}, deleted bool) {
	meta := getMetaObject(obj)
	if meta == nil {
		return
	}
	key, ok := modelServingKeyFromChildResource(meta)
	if !ok {
		return
	}
	c.requestAudit(key, func(r *auditRequest) {
		if deleted {
			r.live = true
			if pod, ok := meta.(*corev1.Pod); ok {
				r.deletedPods[pod.UID] = pod.DeepCopy()
			}
		}
	})
}

func (c *ModelServingController) queueModelServingObservation(old, current interface{}) {
	oldMS, _ := getMetaObject(old).(*workloadv1alpha1.ModelServing)
	ms, _ := getMetaObject(current).(*workloadv1alpha1.ModelServing)
	if ms == nil {
		if oldMS != nil {
			c.requestAudit(utils.GetNamespaceName(oldMS).String(), func(r *auditRequest) { r.live = true; r.deletedMS = oldMS.DeepCopy() })
		}
		return
	}
	if oldMS != nil && oldMS.UID == ms.UID && reflect.DeepEqual(oldMS.Spec, ms.Spec) && reflect.DeepEqual(oldMS.Annotations, ms.Annotations) && reflect.DeepEqual(oldMS.DeletionTimestamp, ms.DeletionTimestamp) {
		return
	}
	c.requestAudit(utils.GetNamespaceName(ms).String(), func(r *auditRequest) {
		if oldMS != nil && oldMS.UID != ms.UID {
			r.live = true
		}
		if oldMS != nil && oldMS.Spec.Template.NetworkTopology != nil && ms.Spec.Template.NetworkTopology == nil {
			r.cleanupTopology = true
		}
	})
}

func (c *ModelServingController) enqueuePeriodicAudit() error {
	list, err := c.modelServingLister.List(labels.Everything())
	if err != nil {
		return err
	}
	keys := make(map[string]struct{})
	for _, ms := range list {
		keys[utils.GetNamespaceName(ms).String()] = struct{}{}
	}
	for _, key := range c.store.ListModelServings() {
		keys[key.String()] = struct{}{}
	}
	for key := range keys {
		c.requestAudit(key, func(r *auditRequest) { r.live = true })
	}
	return nil
}

func (c *ModelServingController) runPeriodicAudit(ctx context.Context) {
	if c.auditPeriod <= 0 {
		return
	}
	ticker := time.NewTicker(c.auditPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.enqueuePeriodicAudit(); err != nil {
				klog.ErrorS(err, "Failed to enqueue periodic ModelServing audit")
			}
		}
	}
}

func (c *ModelServingController) reconcileModelServing(ctx context.Context, key string) (resultErr error) {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, c.auditTimeout)
	defer cancel()
	c.audit.mutex.Lock()
	r := c.audit.requests[key]
	delete(c.audit.requests, key)
	c.audit.mutex.Unlock()
	if r == nil {
		r = &auditRequest{}
	}
	value, exists := c.audit.states.Load(key)
	var state *servingAuditState
	if exists {
		state = value.(*servingAuditState)
	}
	live := r.live || r.deletedMS != nil || state != nil && state.live
	started := time.Now()
	defer func() {
		if live {
			c.audit.metrics.finish(started, resultErr, state)
		}
	}()
	// Preserve live intent on all failures, including a partial paginated List.
	defer func() {
		if resultErr != nil {
			c.audit.mutex.Lock()
			next := c.audit.requests[key]
			if next == nil {
				next = &auditRequest{deletedPods: make(map[types.UID]*corev1.Pod)}
				c.audit.requests[key] = next
			}
			next.live = next.live || live
			next.cleanupTopology = next.cleanupTopology || r.cleanupTopology
			if next.deletedMS == nil {
				next.deletedMS = r.deletedMS
			}
			for uid, pod := range r.deletedPods {
				next.deletedPods[uid] = pod
			}
			c.audit.mutex.Unlock()
		}
	}()
	ms, err := c.modelServingLister.ModelServings(namespace).Get(name)
	if live || apierrors.IsNotFound(err) {
		live = true
		c.audit.metrics.reads.WithLabelValues("modelservings").Inc()
		ms, err = c.modelServingClient.WorkloadV1alpha1().ModelServings(namespace).Get(ctx, name, metav1.GetOptions{})
	}
	if apierrors.IsNotFound(err) {
		old := r.deletedMS
		if state != nil {
			old = state.ms
		}
		if old != nil {
			if err := c.cleanupRetiredModelServing(ctx, old, state); err != nil {
				return err
			}
		}
		c.store.DeleteModelServing(types.NamespacedName{Namespace: namespace, Name: name})
		c.audit.states.Delete(key)
		return nil
	}
	if err != nil {
		return err
	}
	observation, err := c.readObservation(ctx, ms, live)
	if err != nil {
		return err
	}
	if state != nil && state.uid != ms.UID {
		if err := c.cleanupRetiredModelServing(ctx, state.ms, state); err != nil {
			return err
		}
		live = true
		observation, err = c.readObservation(ctx, ms, true)
		if err != nil {
			return err
		}
	}
	if state == nil || state.uid != ms.UID {
		state = newServingAuditState(ms)
		c.audit.states.Store(key, state)
	}
	if !reflect.DeepEqual(state.ms.Spec.Plugins, ms.Spec.Plugins) || !reflect.DeepEqual(state.ms.Annotations, ms.Annotations) {
		for _, progress := range state.pods {
			progress.runningVersion = ""
			progress.ready = false
		}
	}
	state.ms = ms.DeepCopy()
	state.live = live && !c.cacheMatchesObservation(ms, observation)
	if state.live {
		c.audit.metrics.drift.Inc()
	}
	for uid, pod := range r.deletedPods {
		if utils.IsOwnedByModelServingWithUID(pod, ms.UID) {
			state.deleted[uid] = pod
		}
	}
	c.store.BindModelServing(utils.GetNamespaceName(ms), ms.UID)
	view := c.observationView(ctx, ms, observation, state)
	if observation.podGroups != nil {
		ctx = podgroupmanager.WithPodGroupLister(ctx, pglisters.NewPodGroupLister(observation.podGroups))
	}
	if r.cleanupTopology && (ms.Spec.Template.GangPolicy == nil || len(ms.Spec.Template.GangPolicy.MinRoleReplicas) == 0) {
		if err := c.podGroupManager.CleanupPodGroups(ctx, ms); err != nil {
			return err
		}
	}
	start := time.Now()
	err = view.syncModelServing(ctx, key)
	if err == errAuditRequeue {
		err = nil
	}
	if live {
		klog.V(2).InfoS("Audited ModelServing from API server", "modelServing", key, "uid", ms.UID, "duration", time.Since(start), "cacheDrift", state.live, "error", err)
	}
	return err
}

func newObservationIndexer() cache.Indexer {
	return cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{
		cache.NamespaceIndex: cache.MetaNamespaceIndexFunc, GroupNameKey: utils.GroupNameIndexFunc, RoleIDKey: utils.RoleIDIndexFunc,
	})
}

func listAuditPages[T any](ctx context.Context, options metav1.ListOptions, list func(context.Context, metav1.ListOptions) ([]T, string, error)) ([]T, error) {
	options.Limit = 500
	var result []T
	for {
		items, next, err := list(ctx, options)
		if err != nil {
			return nil, err
		}
		result = append(result, items...)
		if next == "" {
			return result, nil
		}
		if next == options.Continue {
			return nil, fmt.Errorf("List returned a repeated continuation token")
		}
		options.Continue = next
	}
}

func (c *ModelServingController) readObservation(ctx context.Context, ms *workloadv1alpha1.ModelServing, live bool) (*servingObservation, error) {
	o := &servingObservation{pods: newObservationIndexer(), services: cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}), live: live}
	selector := labels.SelectorFromSet(map[string]string{workloadv1alpha1.ModelServingNameLabelKey: ms.Name})
	options := metav1.ListOptions{LabelSelector: selector.String()}
	var pods []*corev1.Pod
	var services []*corev1.Service
	if live {
		items, err := listAuditPages(ctx, options, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Pod, string, error) {
			c.audit.metrics.reads.WithLabelValues("pods").Inc()
			list, err := c.kubeClientSet.CoreV1().Pods(ms.Namespace).List(ctx, opts)
			if err != nil {
				return nil, "", err
			}
			return list.Items, list.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("live Pod list: %w", err)
		}
		for i := range items {
			pods = append(pods, &items[i])
		}
		svcs, err := listAuditPages(ctx, options, func(ctx context.Context, opts metav1.ListOptions) ([]corev1.Service, string, error) {
			c.audit.metrics.reads.WithLabelValues("services").Inc()
			list, err := c.kubeClientSet.CoreV1().Services(ms.Namespace).List(ctx, opts)
			if err != nil {
				return nil, "", err
			}
			return list.Items, list.Continue, nil
		})
		if err != nil {
			return nil, fmt.Errorf("live Service list: %w", err)
		}
		for i := range svcs {
			services = append(services, &svcs[i])
		}
	} else {
		var err error
		pods, err = c.podsLister.Pods(ms.Namespace).List(selector)
		if err != nil {
			return nil, err
		}
		services, err = c.servicesLister.Services(ms.Namespace).List(selector)
		if err != nil {
			return nil, err
		}
	}
	for _, pod := range pods {
		if err := o.pods.Add(pod.DeepCopy()); err != nil {
			return nil, err
		}
	}
	for _, svc := range services {
		if err := o.services.Add(svc.DeepCopy()); err != nil {
			return nil, err
		}
	}
	if c.podGroupManager != nil && c.podGroupManager.HasPodGroupCRD() {
		o.podGroups = newObservationIndexer()
		if live {
			pgs, err := listAuditPages(ctx, options, func(ctx context.Context, opts metav1.ListOptions) ([]schedulingv1beta1.PodGroup, string, error) {
				c.audit.metrics.reads.WithLabelValues("podgroups").Inc()
				list, err := c.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace).List(ctx, opts)
				if err != nil {
					return nil, "", err
				}
				return list.Items, list.Continue, nil
			})
			if err != nil {
				return nil, fmt.Errorf("live PodGroup list: %w", err)
			}
			for i := range pgs {
				if err := o.podGroups.Add(pgs[i].DeepCopy()); err != nil {
					return nil, err
				}
			}
		} else if informer := c.podGroupManager.GetPodGroupInformer(); informer != nil {
			for _, obj := range informer.GetStore().List() {
				pg := obj.(*schedulingv1beta1.PodGroup)
				if pg.Namespace == ms.Namespace && selector.Matches(labels.Set(pg.Labels)) {
					if err := o.podGroups.Add(pg.DeepCopy()); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	return o, nil
}

func (c *ModelServingController) observationView(ctx context.Context, ms *workloadv1alpha1.ModelServing, o *servingObservation, state *servingAuditState) *ModelServingController {
	indexer := cache.NewIndexer(cache.MetaNamespaceKeyFunc, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc})
	_ = indexer.Add(ms.DeepCopy())
	return &ModelServingController{
		shared: c, audit: c.audit, observation: o, servingState: state, reconcileContext: ctx,
		kubeClientSet: &auditKubeClient{Interface: c.kubeClientSet, controller: c, ms: ms}, modelServingClient: c.modelServingClient, volcanoClient: c.volcanoClient,
		podGroupManager: c.podGroupManager, podsInformer: c.podsInformer,
		podsLister: corelisters.NewPodLister(o.pods), servicesLister: corelisters.NewServiceLister(o.services),
		configMapsLister:   &auditConfigMapLister{ctx: ctx, client: c.kubeClientSet},
		modelServingLister: mslisters.NewModelServingLister(indexer),
		workqueue:          c.workqueue, store: c.store, pluginsRegistry: c.pluginsRegistry, recorder: c.recorder,
	}
}

func objectVersions(objects []interface{}) []string {
	result := make([]string, 0, len(objects))
	for _, object := range objects {
		meta := object.(metav1.Object)
		result = append(result, meta.GetNamespace()+"/"+meta.GetName()+"/"+string(meta.GetUID())+"/"+meta.GetResourceVersion())
	}
	sort.Strings(result)
	return result
}

func (c *ModelServingController) cacheMatchesObservation(ms *workloadv1alpha1.ModelServing, o *servingObservation) bool {
	cachedMS, err := c.modelServingLister.ModelServings(ms.Namespace).Get(ms.Name)
	if err != nil || cachedMS.UID != ms.UID || cachedMS.ResourceVersion != ms.ResourceVersion {
		return false
	}
	cached, err := c.readObservation(context.Background(), ms, false)
	if err != nil {
		return false
	}
	if !reflect.DeepEqual(objectVersions(cached.pods.List()), objectVersions(o.pods.List())) || !reflect.DeepEqual(objectVersions(cached.services.List()), objectVersions(o.services.List())) {
		return false
	}
	if o.podGroups != nil && (cached.podGroups == nil || !reflect.DeepEqual(objectVersions(cached.podGroups.List()), objectVersions(o.podGroups.List()))) {
		return false
	}
	return true
}
