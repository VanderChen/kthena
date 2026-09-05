//go:build integration

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
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	kthenaclient "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/datastore"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
	corev1 "k8s.io/api/core/v1"
	apiextclient "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"
	volcanoclient "volcano.sh/apis/pkg/client/clientset/versioned"
)

type auditWatchTransport struct {
	http.RoundTripper
	capture *auditWatchCapture
}
type auditWatchCapture struct {
	mu       sync.Mutex
	body     io.ReadCloser
	opened   int
	requests map[string]int
}

func (r *auditWatchTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r.capture.mu.Lock()
	r.capture.requests[req.Method]++
	r.capture.mu.Unlock()
	response, err := r.RoundTripper.RoundTrip(req)
	if err == nil && strings.HasSuffix(req.URL.Path, "/pods") && req.URL.Query().Get("watch") == "true" {
		r.capture.mu.Lock()
		r.capture.body = response.Body
		r.capture.opened++
		r.capture.mu.Unlock()
	}
	return response, err
}

// Test-only notification fault injection. Informer stores continue receiving
// real API updates; neither cache contents nor the controller's business store
// are written by the harness. All pending business hints for gated keys are lost.
type auditNotificationGate struct {
	workqueue.RateLimitingInterface
	controller *ModelServingController
	namespace  string
	closed     atomic.Bool
	dropped    atomic.Int64
	inFlight   atomic.Int64
}

func (q *auditNotificationGate) drop(item interface{}) bool {
	key, ok := item.(string)
	if !ok || !q.closed.Load() || !strings.HasPrefix(key, q.namespace+"/") {
		return false
	}
	q.controller.audit.mutex.Lock()
	delete(q.controller.audit.requests, key)
	q.controller.audit.mutex.Unlock()
	q.dropped.Add(1)
	return true
}
func (q *auditNotificationGate) Add(item interface{}) {
	if !q.drop(item) {
		q.RateLimitingInterface.Add(item)
	}
}
func (q *auditNotificationGate) AddAfter(item interface{}, delay time.Duration) {
	if !q.drop(item) {
		q.RateLimitingInterface.AddAfter(item, delay)
	}
}
func (q *auditNotificationGate) AddRateLimited(item interface{}) {
	if !q.drop(item) {
		q.RateLimitingInterface.AddRateLimited(item)
	}
}

func TestKindLiveAuditRecovery(t *testing.T) {
	configPath := os.Getenv("KTHENA_KIND_AUDIT_KUBECONFIG")
	require.NotEmpty(t, configPath, "explicit disposable Kind kubeconfig is required")
	config, err := clientcmd.LoadFromFile(configPath)
	require.NoError(t, err)
	require.Equal(t, "kind-kthena-resync-010", config.CurrentContext, "refuse to mutate another cluster")
	restConfig, err := clientcmd.NewDefaultClientConfig(*config, &clientcmd.ConfigOverrides{}).ClientConfig()
	require.NoError(t, err)
	restConfig.QPS, restConfig.Burst = 50, 100
	watches := &auditWatchCapture{requests: map[string]int{}}
	restConfig.WrapTransport = func(rt http.RoundTripper) http.RoundTripper { return &auditWatchTransport{rt, watches} }
	kube := kubernetes.NewForConfigOrDie(restConfig)
	kthena := kthenaclient.NewForConfigOrDie(restConfig)
	volcano := volcanoclient.NewForConfigOrDie(restConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	deployment, err := kube.AppsV1().Deployments("kthena-system").Get(ctx, "kthena-controller-manager", metav1.GetOptions{})
	require.NoError(t, err)
	require.Zero(t, *deployment.Spec.Replicas, "scale the shipping controller to zero before running this harness")
	require.Zero(t, deployment.Status.Replicas, "wait for the shipping controller to stop")
	ns, err := kube.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "bug-010-audit-"}}, metav1.CreateOptions{})
	require.NoError(t, err)
	t.Logf("isolated namespace=%s", ns.Name)
	t.Cleanup(func() {
		if !t.Failed() {
			_ = kube.CoreV1().Namespaces().Delete(context.Background(), ns.Name, metav1.DeleteOptions{})
		}
	})
	c, err := NewModelServingController(kube, kthena, volcano, apiextclient.NewForConfigOrDie(restConfig))
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, c.auditPeriod, "test the real default, not an accelerated timer")
	q := &auditNotificationGate{RateLimitingInterface: c.workqueue, controller: c, namespace: ns.Name}
	c.workqueue = q
	var reconcileMu sync.Mutex
	var reconciled []time.Time
	c.syncHandler = func(ctx context.Context, key string) error {
		if q.drop(key) {
			return nil
		}
		q.inFlight.Add(1)
		defer q.inFlight.Add(-1)
		if strings.HasPrefix(key, ns.Name+"/") {
			reconcileMu.Lock()
			reconciled = append(reconciled, time.Now())
			reconcileMu.Unlock()
		}
		return c.reconcileModelServing(ctx, key)
	}
	started := time.Now()
	go c.Run(ctx, 3)
	require.Eventually(t, c.initialSync.Load, 30*time.Second, 100*time.Millisecond)
	policies := map[string]workloadv1alpha1.RecoveryPolicy{"none": workloadv1alpha1.NoneRestartPolicy, "role": workloadv1alpha1.RoleRecreate, "group": workloadv1alpha1.ServingGroupRecreate, "readiness": workloadv1alpha1.NoneRestartPolicy}
	models := map[string]*workloadv1alpha1.ModelServing{}
	podTemplate := workloadv1alpha1.PodTemplateSpec{Spec: corev1.PodSpec{
		TerminationGracePeriodSeconds: ptr.To[int64](1), Containers: []corev1.Container{{Name: "main", Image: "busybox:1.36", ImagePullPolicy: corev1.PullIfNotPresent,
			Command: []string{"sh", "-c", "touch /tmp/ready; sleep 86400"}, ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{Exec: &corev1.ExecAction{Command: []string{"test", "-f", "/tmp/ready"}}}, PeriodSeconds: 1, FailureThreshold: 1}}},
	}}
	for name, policy := range policies {
		ms := &workloadv1alpha1.ModelServing{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns.Name}, Spec: workloadv1alpha1.ModelServingSpec{Replicas: ptr.To[int32](1), SchedulerName: "volcano", RecoveryPolicy: policy,
			Template: workloadv1alpha1.ServingGroup{RestartGracePeriodSeconds: ptr.To[int64](0), Roles: []workloadv1alpha1.Role{
				{Name: "prefill", Replicas: ptr.To[int32](1), WorkerReplicas: 1, EntryTemplate: podTemplate, WorkerTemplate: podTemplate.DeepCopy()},
				{Name: "decode", Replicas: ptr.To[int32](1), EntryTemplate: podTemplate},
			}},
		}}
		ms, err = kthena.WorkloadV1alpha1().ModelServings(ns.Name).Create(ctx, ms, metav1.CreateOptions{})
		require.NoError(t, err)
		models[name] = ms
	}
	listPods := func(name string) map[string]*corev1.Pod {
		list, err := kube.CoreV1().Pods(ns.Name).List(ctx, metav1.ListOptions{LabelSelector: labels.Set{workloadv1alpha1.ModelServingNameLabelKey: name}.String()})
		require.NoError(t, err)
		result := map[string]*corev1.Pod{}
		for i := range list.Items {
			result[list.Items[i].Name] = &list.Items[i]
		}
		return result
	}
	baseline := map[string]map[string]*corev1.Pod{}
	for name, ms := range models {
		require.Eventually(t, func() bool {
			pods := listPods(name)
			if len(pods) != 3 {
				return false
			}
			for _, pod := range pods {
				if !utils.IsPodRunningAndReady(pod) {
					return false
				}
			}
			return c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), name+"-0") == datastore.ServingGroupRunning
		}, 90*time.Second, 200*time.Millisecond, "baseline "+name)
		baseline[name] = listPods(name)
	}
	require.Less(t, time.Since(started), 2*time.Minute, "leave an unambiguous window before the first default audit")
	q.closed.Store(true)
	require.Eventually(t, func() bool { return q.inFlight.Load() == 0 }, 30*time.Second, 100*time.Millisecond)
	for name := range models {
		if name == "readiness" {
			command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", configPath, "--context", config.CurrentContext, "-n", ns.Name, "exec", "readiness-0-prefill-0-1", "--", "mv", "/tmp/ready", "/tmp/not-ready")
			output, err := command.CombinedOutput()
			require.NoError(t, err, string(output))
		} else {
			pod := baseline[name][name+"-0-prefill-0-1"]
			require.NoError(t, kube.CoreV1().Pods(ns.Name).Delete(ctx, pod.Name, *metav1.NewPreconditionDeleteOptions(string(pod.UID))))
		}
	}
	require.Eventually(t, func() bool {
		for name := range models {
			pod, err := c.podsLister.Pods(ns.Name).Get(name + "-0-prefill-0-1")
			if name == "readiness" {
				if err != nil || utils.IsPodRunningAndReady(pod) {
					return false
				}
			} else if err == nil {
				return false
			}
		}
		return q.dropped.Load() > 0 && q.inFlight.Load() == 0
	}, 30*time.Second, 100*time.Millisecond, "watch cache receives deletion/readiness while business notifications are lost")
	for name, ms := range models {
		require.Equal(t, datastore.ServingGroupRunning, c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), name+"-0"), "prove business-store drift")
	}
	t.Logf("drift established at %s after=%s dropped=%d; waiting for the unchanged 5m timer", time.Now().Format(time.RFC3339), time.Since(started), q.dropped.Load())
	// Hold the gate through the negative-observation window, then release it.
	// No API mutations or private refresh/queue calls follow this point.
	require.Never(t, func() bool { return len(listPods("role")) != 2 }, 5*time.Second, 200*time.Millisecond)
	reconcileMu.Lock()
	before := len(reconciled)
	reconcileMu.Unlock()
	q.closed.Store(false)
	require.Eventually(t, func() bool {
		for name, ms := range models {
			pods := listPods(name)
			if name == "readiness" {
				if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), name+"-0") != datastore.ServingGroupCreating {
					return false
				}
				continue
			}
			if len(pods) != 3 {
				return false
			}
			for _, pod := range pods {
				if !utils.IsPodRunningAndReady(pod) || pod.DeletionTimestamp != nil {
					return false
				}
			}
			if c.store.GetServingGroupStatus(utils.GetNamespaceName(ms), name+"-0") != datastore.ServingGroupRunning {
				return false
			}
		}
		return true
	}, 6*time.Minute, time.Second, "default periodic audit independently restores the correct state")
	reconcileMu.Lock()
	firstRecovery := reconciled[before]
	reconcileMu.Unlock()
	require.GreaterOrEqual(t, firstRecovery.Sub(started), 5*time.Minute-time.Second, "recovery must begin at the default timer, not a stray business notification")
	for name := range models {
		current := listPods(name)
		for podName, old := range baseline[name] {
			wantChanged := name == "group" || name == "role" && strings.Contains(podName, "prefill") || name == "none" && strings.HasSuffix(podName, "prefill-0-1")
			require.Equal(t, wantChanged, current[podName].UID != old.UID, "recovery scope for "+podName)
			t.Logf("policy=%s pod=%s beforeUID=%s afterUID=%s changed=%t", name, podName, old.UID, current[podName].UID, wantChanged)
		}
	}
	key := types.NamespacedName{Namespace: ns.Name, Name: "readiness"}
	ready, err := c.store.GetRunningPodNumByServingGroup(key, "readiness-0")
	require.NoError(t, err)
	require.Equal(t, 2, ready)
	t.Logf("PASS default-period audit: firstRecovery=%s elapsed=%s dropped=%d readiness=2/3 namespace=%s", firstRecovery.Format(time.RFC3339), time.Since(started), q.dropped.Load(), ns.Name)
	// A real watch stream interruption must reconnect through client-go,
	// without a private refresh or a manual informer-store replacement.
	watches.mu.Lock()
	body, opened := watches.body, watches.opened
	watches.mu.Unlock()
	require.NotNil(t, body)
	require.NoError(t, body.Close())
	command := exec.CommandContext(ctx, "kubectl", "--kubeconfig", configPath, "--context", config.CurrentContext, "-n", ns.Name, "exec", "readiness-0-prefill-0-1", "--", "mv", "/tmp/not-ready", "/tmp/ready")
	output, err := command.CombinedOutput()
	require.NoError(t, err, string(output))
	require.Eventually(t, func() bool {
		watches.mu.Lock()
		reconnected := watches.opened > opened
		watches.mu.Unlock()
		return reconnected && c.store.GetServingGroupStatus(key, "readiness-0") == datastore.ServingGroupRunning
	}, 60*time.Second, 200*time.Millisecond, "watch reconnect and readiness recovery")
	watches.mu.Lock()
	t.Logf("PASS watch reconnect: streams=%d API-method-counts=%v queue-depth=%d", watches.opened, watches.requests, c.workqueue.Len())
	watches.mu.Unlock()
}
