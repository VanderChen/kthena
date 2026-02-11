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

package modelserving

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"
	"time"

	clientset "github.com/volcano-sh/kthena/client-go/clientset/versioned"
	"github.com/volcano-sh/kthena/test/e2e/utils"
	"github.com/volcano-sh/kthena/test/integration/modelserving/runner"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

var (
	kubeClient    kubernetes.Interface
	kthenaClient  clientset.Interface
	kubeConfig    *rest.Config
	testNamespace string
)

// TestMain sets up the test environment and runs tests.
func TestMain(m *testing.M) {
	rand.Seed(time.Now().UnixNano())

	// Generate unique namespace for this test run
	testNamespace = fmt.Sprintf("kthena-integration-%s", utils.RandomString(5))

	// Get Kubernetes configuration
	var err error
	kubeConfig, err = utils.GetKubeConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to get kubeconfig: %v\n", err)
		os.Exit(1)
	}

	// Create Kubernetes clients
	kubeClient, err = kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Kubernetes client: %v\n", err)
		os.Exit(1)
	}

	kthenaClient, err = clientset.NewForConfig(kubeConfig)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create Kthena client: %v\n", err)
		os.Exit(1)
	}

	// Create test namespace
	fmt.Printf("Creating test namespace: %s\n", testNamespace)
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: testNamespace,
		},
	}
	_, err = kubeClient.CoreV1().Namespaces().Create(context.Background(), ns, metav1.CreateOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create test namespace: %v\n", err)
		os.Exit(1)
	}

	// Run tests
	code := m.Run()

	// Cleanup: delete test namespace
	fmt.Printf("Cleaning up test namespace: %s\n", testNamespace)
	err = kubeClient.CoreV1().Namespaces().Delete(context.Background(), testNamespace, metav1.DeleteOptions{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "WARNING: Failed to delete test namespace: %v\n", err)
	}

	os.Exit(code)
}

// newTestRunner creates a new test runner with a unique test ID.
func newTestRunner() *runner.TestRunner {
	testID := utils.RandomString(8)
	return runner.NewTestRunner(kubeClient, kthenaClient, kubeConfig, testNamespace, testID)
}
