package autoscaler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
)

func TestUpdateMetrics_WithRestartedPod(t *testing.T) {
	ns := "default"
	metricName := "request_count"
	
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("# TYPE " + metricName + " gauge\n" + metricName + " 10\n"))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	port, _ := strconv.Atoi(u.Port())

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pod",
			Namespace: ns,
			Labels: map[string]string{
				"app":                          "test",
				"modelserving.volcano.sh/name":  "test-ms",
				"modelserving.volcano.sh/entry": "true",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "127.0.0.1",
			StartTime: &metav1.Time{Time: metav1.Now().Time},
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
			ContainerStatuses: []corev1.ContainerStatus{
				{RestartCount: 5, Ready: true},
			},
		},
	}

	target := &v1alpha1.Target{
		TargetRef: corev1.ObjectReference{
			Kind: "ModelServing",
			Name: "test-ms",
		},
		MetricEndpoint: v1alpha1.MetricEndpoint{
			Port: int32(port),
			Uri:  "/metrics",
		},
	}
	
	// Override target port to use httptest server port
	target.MetricEndpoint.Port = int32(port)
	target.MetricEndpoint.Uri = u.Path

	binding := &v1alpha1.AutoscalingPolicyBinding{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
	}
	
	metricTargets := map[string]float64{metricName: 20}
	
	collector := NewMetricCollector(target, binding, metricTargets)
	
	kubeClient := fake.NewSimpleClientset(pod)
	informerFactory := informers.NewSharedInformerFactory(kubeClient, 0)
	podInformer := informerFactory.Core().V1().Pods()
	podInformer.Informer().GetIndexer().Add(pod)
	
	unreadyCount, metrics, err := collector.UpdateMetrics(context.TODO(), podInformer.Lister())
	if err != nil {
		t.Fatalf("UpdateMetrics failed: %v", err)
	}
	
	if unreadyCount != 0 {
		t.Errorf("Expected 0 unready instances, got %d", unreadyCount)
	}
	
	val, ok := metrics[metricName]
	if !ok {
		t.Errorf("Metric %s not found in results", metricName)
	}
	if val != 10 {
		t.Errorf("Expected metric value 10, got %f", val)
	}
}
