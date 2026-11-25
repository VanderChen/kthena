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

package apitest

import (
	"fmt"
	"strconv"
	"sync"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	k8stesting "k8s.io/client-go/testing"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"
	volcanofake "volcano.sh/apis/pkg/client/clientset/versioned/fake"
	volcanoscheme "volcano.sh/apis/pkg/client/clientset/versioned/scheme"

	kthenafake "github.com/volcano-sh/kthena/client-go/clientset/versioned/fake"
	kthenascheme "github.com/volcano-sh/kthena/client-go/clientset/versioned/scheme"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// DefaultStartTime is the wall clock time a Cluster starts at. Tests that care
// about timestamps advance it with Advance or replace Cluster.Now.
var DefaultStartTime = time.Date(2024, time.January, 1, 0, 0, 0, 0, time.UTC)

// defaultPodGracePeriodSeconds is stamped into deletionGracePeriodSeconds of a
// Pod that keeps terminating, mirroring the API server default.
const defaultPodGracePeriodSeconds int64 = 30

// Write describes one accepted mutating request.
type Write struct {
	Verb        string // create, update, patch, delete
	Resource    string // pods, services, podgroups, modelservings, servinggroups, controllerrevisions, configmaps, ...
	Subresource string // "" or "status"
	Namespace   string
	Name        string
}

// String renders a write as "verb resource[/subresource] namespace/name".
func (w Write) String() string {
	resource := w.Resource
	if w.Subresource != "" {
		resource += "/" + w.Subresource
	}
	return fmt.Sprintf("%s %s %s/%s", w.Verb, resource, w.Namespace, w.Name)
}

// Cluster is an in-memory API server stand-in shared by three fake clientsets.
// Its zero value is not usable; call New.
type Cluster struct {
	Kube    *kubefake.Clientset
	Kthena  *kthenafake.Clientset
	Volcano *volcanofake.Clientset

	// Now supplies timestamps (creation, deletion). Defaults to a fixed start
	// time that tests may advance. Replace it before the first request.
	Now func() time.Time

	// BeforeWrite, when set, is called for every mutating request after its
	// preconditions passed and before it is applied. Returning an error
	// rejects the request with that error and nothing is applied or logged.
	// The hook runs while the Cluster is locked: it may call Writes, but must
	// not issue requests or call Snapshot, CollectGarbage or Settle.
	BeforeWrite func(Write) error

	// mu serialises request handling and garbage collection.
	mu                 sync.Mutex
	resourceVersion    uint64
	uids               uint64
	names              uint64
	holdPodTermination bool

	timeMu sync.Mutex
	now    time.Time

	logMu  sync.Mutex
	writes []Write
}

// New returns a Cluster preloaded with the given objects. Objects may be of
// any of the three API families; they get a UID, a resourceVersion and a
// creationTimestamp when they do not carry one, and generation 1 when unset.
func New(objects ...runtime.Object) *Cluster {
	c := &Cluster{now: DefaultStartTime}
	c.Now = c.defaultNow

	seeded := map[family][]runtime.Object{}
	for _, obj := range objects {
		fam, err := familyOf(obj)
		if err != nil {
			panic(err)
		}
		seeded[fam] = append(seeded[fam], c.seed(obj))
	}

	c.Kube = kubefake.NewSimpleClientset(seeded[familyKube]...)
	c.Kthena = kthenafake.NewSimpleClientset(seeded[familyKthena]...)
	c.Volcano = volcanofake.NewSimpleClientset(seeded[familyVolcano]...)

	c.install(&c.Kube.Fake, familyKube)
	c.install(&c.Kthena.Fake, familyKthena)
	c.install(&c.Volcano.Fake, familyVolcano)
	return c
}

// familyOf reports which fake clientset serves the given object.
func familyOf(obj runtime.Object) (family, error) {
	if _, _, err := kthenascheme.Scheme.ObjectKinds(obj); err == nil {
		return familyKthena, nil
	}
	if _, _, err := volcanoscheme.Scheme.ObjectKinds(obj); err == nil {
		return familyVolcano, nil
	}
	if _, _, err := kubescheme.Scheme.ObjectKinds(obj); err == nil {
		return familyKube, nil
	}
	return familyKube, fmt.Errorf("apitest: %T belongs to none of the kube, kthena and volcano schemes", obj)
}

// seed stamps the metadata of an object handed to New.
func (c *Cluster) seed(obj runtime.Object) runtime.Object {
	out := obj.DeepCopyObject()
	m, err := meta.Accessor(out)
	if err != nil {
		panic(fmt.Errorf("apitest: %T is not an API object: %w", obj, err))
	}
	if m.GetUID() == "" {
		m.SetUID(c.nextUID())
	}
	if rv := m.GetResourceVersion(); rv == "" {
		m.SetResourceVersion(c.nextResourceVersion())
	} else if parsed, err := strconv.ParseUint(rv, 10, 64); err == nil && parsed > c.resourceVersion {
		// Keep the counter ahead of preloaded resource versions.
		c.resourceVersion = parsed
	}
	if created := m.GetCreationTimestamp(); created.IsZero() {
		m.SetCreationTimestamp(metav1.NewTime(c.Now()))
	}
	if m.GetGeneration() == 0 {
		m.SetGeneration(1)
	}
	return out
}

// install prepends the Cluster reactor to a fake clientset. Reads are left to
// the clientset's own object tracker.
func (c *Cluster) install(f *k8stesting.Fake, fam family) {
	f.PrependReactor("*", "*", func(action k8stesting.Action) (bool, runtime.Object, error) {
		return c.react(fam, action)
	})
}

func (c *Cluster) defaultNow() time.Time {
	c.timeMu.Lock()
	defer c.timeMu.Unlock()
	return c.now
}

// Advance moves the default clock forward. It has no effect once Now has been
// replaced by the test.
func (c *Cluster) Advance(d time.Duration) {
	c.timeMu.Lock()
	defer c.timeMu.Unlock()
	c.now = c.now.Add(d)
}

func (c *Cluster) trackerFor(fam family) k8stesting.ObjectTracker {
	switch fam {
	case familyKthena:
		return c.Kthena.Tracker()
	case familyVolcano:
		return c.Volcano.Tracker()
	default:
		return c.Kube.Tracker()
	}
}

// nextResourceVersion returns the next cluster-wide resource version. The
// caller must hold mu, or be New.
func (c *Cluster) nextResourceVersion() string {
	c.resourceVersion++
	return strconv.FormatUint(c.resourceVersion, 10)
}

func (c *Cluster) nextUID() types.UID {
	c.uids++
	return types.UID(fmt.Sprintf("apitest-uid-%03d", c.uids))
}

func (c *Cluster) nextNameSuffix() string {
	c.names++
	return fmt.Sprintf("%05d", c.names)
}

// Writes returns the accepted writes in order. Events are excluded, and so are
// the deletions performed by the garbage collector, which are not requests.
func (c *Cluster) Writes() []Write {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	out := make([]Write, len(c.writes))
	copy(out, c.writes)
	return out
}

// ResetWrites empties the write log.
func (c *Cluster) ResetWrites() {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.writes = nil
}

func (c *Cluster) logLocked(res *resourceInfo, w Write) {
	if res.gvr.Resource == "events" {
		return
	}
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.writes = append(c.writes, w)
}

// HoldPodTermination controls how deleted Pods behave. By default a deleted
// Pod disappears at once; while termination is held, a deleted Pod keeps
// existing with a deletionTimestamp until FinishTermination is called for it,
// which models a slow kubelet. A terminating Pod still blocks a
// foreground-deleting owner.
func (c *Cluster) HoldPodTermination(hold bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.holdPodTermination = hold
}

// FinishTermination removes a terminating Pod, as the kubelet would once the
// containers are gone. It fails when the Pod does not exist, is not
// terminating, or still carries finalizers.
func (c *Cluster) FinishTermination(namespace, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	tracker := c.trackerFor(familyKube)
	obj, err := tracker.Get(podsGVR, namespace, name)
	if err != nil {
		return err
	}
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return fmt.Errorf("apitest: %s/%s is not a Pod", namespace, name)
	}
	if pod.DeletionTimestamp == nil {
		return fmt.Errorf("apitest: pod %s/%s is not terminating", namespace, name)
	}
	if len(pod.Finalizers) > 0 {
		return fmt.Errorf("apitest: pod %s/%s still has finalizers %v", namespace, name, pod.Finalizers)
	}
	return tracker.Delete(podsGVR, namespace, name)
}

// GetPod returns the stored Pod, or a NotFound error.
func (c *Cluster) GetPod(namespace, name string) (*corev1.Pod, error) {
	obj, err := c.getObject(corev1.SchemeGroupVersion.WithResource("pods"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*corev1.Pod), nil
}

// GetService returns the stored Service, or a NotFound error.
func (c *Cluster) GetService(namespace, name string) (*corev1.Service, error) {
	obj, err := c.getObject(corev1.SchemeGroupVersion.WithResource("services"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*corev1.Service), nil
}

// GetConfigMap returns the stored ConfigMap, or a NotFound error.
func (c *Cluster) GetConfigMap(namespace, name string) (*corev1.ConfigMap, error) {
	obj, err := c.getObject(corev1.SchemeGroupVersion.WithResource("configmaps"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*corev1.ConfigMap), nil
}

// GetControllerRevision returns the stored ControllerRevision, or a NotFound error.
func (c *Cluster) GetControllerRevision(namespace, name string) (*appsv1.ControllerRevision, error) {
	obj, err := c.getObject(appsv1.SchemeGroupVersion.WithResource("controllerrevisions"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*appsv1.ControllerRevision), nil
}

// GetModelServing returns the stored ModelServing, or a NotFound error.
func (c *Cluster) GetModelServing(namespace, name string) (*workloadv1alpha1.ModelServing, error) {
	obj, err := c.getObject(workloadv1alpha1.SchemeGroupVersion.WithResource("modelservings"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*workloadv1alpha1.ModelServing), nil
}

// GetServingGroup returns the stored ServingGroup, or a NotFound error.
func (c *Cluster) GetServingGroup(namespace, name string) (*workloadv1alpha1.ServingGroup, error) {
	obj, err := c.getObject(workloadv1alpha1.SchemeGroupVersion.WithResource("servinggroups"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*workloadv1alpha1.ServingGroup), nil
}

// GetPodGroup returns the stored PodGroup, or a NotFound error.
func (c *Cluster) GetPodGroup(namespace, name string) (*schedulingv1beta1.PodGroup, error) {
	obj, err := c.getObject(schedulingv1beta1.SchemeGroupVersion.WithResource("podgroups"), namespace, name)
	if err != nil {
		return nil, err
	}
	return obj.(*schedulingv1beta1.PodGroup), nil
}

func (c *Cluster) getObject(gvr schema.GroupVersionResource, namespace, name string) (runtime.Object, error) {
	res, ok := resourceByGVR[gvr]
	if !ok {
		return nil, apierrors.NewNotFound(gvr.GroupResource(), name)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.trackerFor(res.fam).Get(res.gvr, namespace, name)
}
