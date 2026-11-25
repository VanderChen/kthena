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

// Package manager runs the ModelServing controller and the ServingGroup
// controller in one process, on one set of informers.
package manager

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/wait"
	kubeinformers "k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	volcano "volcano.sh/apis/pkg/client/clientset/versioned"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	kthenainformers "github.com/volcano-sh/kthena/client-go/informers/externalversions"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/mscontroller"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/objcache"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/podgroupmanager"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/sgcontroller"
)

// servingGroupCRD is the CustomResourceDefinition the controllers cannot work without.
const servingGroupCRD = "servinggroups." + workloadv1alpha1.GroupName

// Manager owns the informers and the two controllers.
type Manager struct {
	kthena    clientset.Interface
	caches    objcache.Caches
	informers []cache.SharedIndexInformer
	podGroups *podgroupmanager.Manager
	groups    *sgcontroller.Runner
	servings  *mscontroller.Runner
}

func exists(key string) (string, error) {
	requirement, err := labels.NewRequirement(key, selection.Exists, nil)
	if err != nil {
		return "", err
	}
	return requirement.String(), nil
}

// New builds the informers and the controllers. Nothing is started.
func New(kube kubernetes.Interface, kthena clientset.Interface, volcanoClient volcano.Interface, apiext apiextclient.Interface) (*Manager, error) {
	// The kinds of the workload API are registered for the events.
	if err := workloadv1alpha1.Install(scheme.Scheme); err != nil {
		return nil, fmt.Errorf("register the workload API: %w", err)
	}
	// Only objects of ModelServings are cached: Pods and Services carry the
	// name of their group, ControllerRevisions the name of their ModelServing.
	grouped, err := exists(workloadv1alpha1.GroupNameLabelKey)
	if err != nil {
		return nil, err
	}
	named, err := exists(workloadv1alpha1.ModelServingNameLabelKey)
	if err != nil {
		return nil, err
	}
	filtered := func(selector string) kubeinformers.SharedInformerFactory {
		return kubeinformers.NewSharedInformerFactoryWithOptions(kube, 0,
			kubeinformers.WithTweakListOptions(func(options *metav1.ListOptions) { options.LabelSelector = selector }))
	}
	members := filtered(grouped)
	history := filtered(named)
	workloads := kthenainformers.NewSharedInformerFactory(kthena, 0)

	pods := members.Core().V1().Pods()
	services := members.Core().V1().Services()
	revisions := history.Apps().V1().ControllerRevisions().Informer()
	modelServings := workloads.Workload().V1alpha1().ModelServings().Informer()
	servingGroups := workloads.Workload().V1alpha1().ServingGroups().Informer()

	caches := objcache.Caches{
		ModelServings:       modelServings.GetIndexer(),
		ServingGroups:       servingGroups.GetIndexer(),
		Pods:                pods.Informer().GetIndexer(),
		ControllerRevisions: revisions.GetIndexer(),
	}
	if err := caches.AddIndexers(); err != nil {
		return nil, fmt.Errorf("index the caches: %w", err)
	}

	// The rollback switch is one ConfigMap in the namespace of the controller
	// manager. Outside a cluster there is no such namespace and no switch.
	rollingBack := func() bool { return false }
	var rollbackSwitch cache.SharedIndexInformer
	if namespace := os.Getenv("POD_NAMESPACE"); namespace != "" {
		rollbackSwitch = kubeinformers.NewSharedInformerFactoryWithOptions(kube, 0,
			kubeinformers.WithNamespace(namespace),
			kubeinformers.WithTweakListOptions(func(options *metav1.ListOptions) {
				options.FieldSelector = "metadata.name=" + mscontroller.RollbackSwitch
			})).Core().V1().ConfigMaps().Informer()
		key := namespace + "/" + mscontroller.RollbackSwitch
		rollingBack = func() bool {
			_, exists, _ := rollbackSwitch.GetStore().GetByKey(key)
			return exists
		}
	}

	broadcaster := record.NewBroadcaster()
	broadcaster.StartStructuredLogging(0)
	broadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kube.CoreV1().Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "modelserving-controller"})

	m := &Manager{
		kthena:    kthena,
		caches:    caches,
		informers: []cache.SharedIndexInformer{pods.Informer(), services.Informer(), revisions, modelServings, servingGroups},
	}
	if rollbackSwitch != nil {
		m.informers = append(m.informers, rollbackSwitch)
	}
	// The PodGroup informer exists only while the PodGroup
	// CustomResourceDefinition is installed; its handlers are added when it
	// starts. That can be before the ServingGroup controller exists: with the
	// definition installed, the PodGroup manager starts the informer while it
	// is being built.
	watch := &podGroupWatch{}
	m.podGroups = podgroupmanager.NewManager(kube, volcanoClient, apiext, watch.started)

	m.groups = sgcontroller.NewRunner(sgcontroller.New(sgcontroller.Options{
		Kube: kube, Kthena: kthena, Caches: caches, Services: services.Lister(),
		PodGroups: m.podGroups, Recorder: recorder, RollingBack: rollingBack,
	}))
	watch.bind(m.groups)
	watch.started(m.podGroups.GetPodGroupInformer())
	if err := m.groups.Register(sgcontroller.Informers{
		ServingGroups: servingGroups, ModelServings: modelServings, Pods: pods.Informer(),
		Services: services.Informer(), ControllerRevisions: revisions,
	}); err != nil {
		return nil, fmt.Errorf("watch for the ServingGroup controller: %w", err)
	}

	m.servings = mscontroller.NewRunner(mscontroller.New(mscontroller.Options{
		Kube: kube, Kthena: kthena, Caches: caches, Services: services.Lister(), Recorder: recorder,
		Volcano: volcanoClient, RollingBack: rollingBack,
	}))
	if err := m.servings.Register(mscontroller.Informers{
		ModelServings: modelServings, ServingGroups: servingGroups, Pods: pods.Informer(), ControllerRevisions: revisions,
	}); err != nil {
		return nil, fmt.Errorf("watch for the ModelServing controller: %w", err)
	}
	if rollbackSwitch != nil {
		// Setting or removing the switch concerns every ModelServing. A
		// controller that does nothing looks broken, so both say why.
		set := func(obj any) {
			klog.InfoS("Rollback mode is on: workloads are handed back to their ModelServings and nothing is reconciled",
				"switch", mscontroller.RollbackSwitch)
			if cm, ok := obj.(*corev1.ConfigMap); ok {
				recorder.Event(cm, corev1.EventTypeWarning, "RollbackMode",
					"ModelServings are not reconciled while this ConfigMap exists: their Pods are handed back so that "+
						"a version without ServingGroup objects can be installed. Delete the ConfigMap to leave rollback mode.")
			}
			m.servings.EnqueueAll()
		}
		removed := func(any) {
			klog.InfoS("Rollback mode is off: workloads are taken over again", "switch", mscontroller.RollbackSwitch)
			m.servings.EnqueueAll()
		}
		if _, err := rollbackSwitch.AddEventHandler(cache.ResourceEventHandlerFuncs{AddFunc: set, DeleteFunc: removed}); err != nil {
			return nil, fmt.Errorf("watch the rollback switch: %w", err)
		}
	}
	return m, nil
}

// Run starts the informers and, once they have synced, the two controllers.
// It blocks until ctx is done.
//
// Nothing is started before the ServingGroup CustomResourceDefinition is
// served. The controllers keep their state in ServingGroup objects; without
// the definition there is no way of working that is safe, so the process
// waits and says so rather than falling back to one.
func (m *Manager) Run(ctx context.Context, workers int) {
	if err := m.WaitForServingGroups(ctx); err != nil {
		return
	}
	for _, informer := range m.informers {
		go informer.RunWithContext(ctx)
	}
	if err := m.podGroups.Run(ctx); err != nil {
		klog.ErrorS(err, "Cannot watch PodGroups; gang scheduling is not available")
	}
	synced := make([]cache.InformerSynced, 0, len(m.informers))
	for _, informer := range m.informers {
		synced = append(synced, informer.HasSynced)
	}
	if !cache.WaitForCacheSync(ctx.Done(), synced...) {
		return
	}
	go m.groups.Run(ctx, workers)
	go m.servings.Run(ctx, workers)
	<-ctx.Done()
}

// WaitForServingGroups blocks until the ServingGroup CustomResourceDefinition
// is established and a ServingGroup can be listed. The process is ready to
// take over from its predecessor once it returns nil, whether or not it holds
// the lease.
func (m *Manager) WaitForServingGroups(ctx context.Context) error {
	reported := false
	return wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		_, err := m.kthena.WorkloadV1alpha1().ServingGroups(metav1.NamespaceAll).List(ctx, metav1.ListOptions{Limit: 1})
		if err == nil {
			if reported {
				klog.InfoS("The ServingGroup CustomResourceDefinition is served; starting the ModelServing controllers")
			}
			return true, nil
		}
		if !reported || !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "ServingGroups cannot be listed; the ModelServing controllers do not start. "+
				"Install the CustomResourceDefinition "+servingGroupCRD+" and grant the controller access to servinggroups", "retryIn", "5s")
			reported = true
		}
		return false, nil
	})
}

// podGroupWatcher is what watches an informer of PodGroups: the runner of
// the ServingGroup controller.
type podGroupWatcher interface {
	RegisterPodGroups(informer cache.SharedIndexInformer)
}

// podGroupWatch brings a PodGroup informer and its watcher together, in
// whichever order they come into being, once.
type podGroupWatch struct {
	mu      sync.Mutex
	watcher podGroupWatcher
	pending []cache.SharedIndexInformer
	watched map[cache.SharedIndexInformer]bool
}

// started is called with every PodGroup informer that is started. nil is ignored.
func (w *podGroupWatch) started(informer cache.SharedIndexInformer) {
	if informer == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.watched[informer] {
		return
	}
	if w.watched == nil {
		w.watched = map[cache.SharedIndexInformer]bool{}
	}
	w.watched[informer] = true
	if w.watcher == nil {
		w.pending = append(w.pending, informer)
		return
	}
	w.watcher.RegisterPodGroups(informer)
}

// bind names the watcher. Informers that started before it are handed to it.
func (w *podGroupWatch) bind(watcher podGroupWatcher) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.watcher = watcher
	for _, informer := range w.pending {
		watcher.RegisterPodGroups(informer)
	}
	w.pending = nil
}
