#!/bin/bash
# ModelServing Integration Test Deployment Script
# This script builds the test image and deploys it as a Kubernetes Job

set -e

# Configuration
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/../../../.." && pwd)"
IMAGE_NAME="${IMAGE_NAME:-modelserving-test}"
IMAGE_TAG="${IMAGE_TAG:-latest}"
NAMESPACE="${NAMESPACE:-default}"
TEST_FILTER="${TEST_FILTER:-.*}"
TEST_TIMEOUT="${TEST_TIMEOUT:-30m}"

# Color output
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m' # No Color

print_step() {
    echo -e "${GREEN}==>${NC} $1"
}

print_warning() {
    echo -e "${YELLOW}WARNING:${NC} $1"
}

print_error() {
    echo -e "${RED}ERROR:${NC} $1"
}

# Parse command line arguments
ACTION="${1:-deploy}"
BUILD_PUSH="${2:-false}"

usage() {
    cat << EOF
Usage: $0 [ACTION] [OPTIONS]

ACTIONS:
    build       Build the test Docker image only
    deploy      Build and deploy test job to cluster (default)
    run         Run existing test job
    logs        View test job logs
    clean       Delete test job and cleanup
    smoke       Run smoke tests only

ENVIRONMENT VARIABLES:
    IMAGE_NAME      Docker image name (default: modelserving-test)
    IMAGE_TAG       Docker image tag (default: latest)
    NAMESPACE       Kubernetes namespace (default: default)
    TEST_FILTER     Test filter regex (default: .*)
    TEST_TIMEOUT    Test timeout (default: 30m)
    DOCKER_REGISTRY Registry to push image (optional, for remote clusters)

EXAMPLES:
    # Build and deploy all tests
    $0 deploy

    # Run smoke tests only
    $0 smoke

    # Run specific test
    TEST_FILTER="TestServingGroupScale" $0 deploy

    # View logs
    $0 logs

    # Cleanup
    $0 clean

    # Build image for remote registry
    DOCKER_REGISTRY=myregistry.io IMAGE_NAME=myrepo/modelserving-test $0 build
EOF
    exit 1
}

# Build Docker image
build_image() {
    print_step "Building Docker image: ${IMAGE_NAME}:${IMAGE_TAG}"

    cd "$PROJECT_ROOT"

    docker build \
        -f test/integration/modelserving/Dockerfile \
        -t "${IMAGE_NAME}:${IMAGE_TAG}" \
        .

    print_step "Image built successfully: ${IMAGE_NAME}:${IMAGE_TAG}"

    # If registry is specified, tag and push
    if [ -n "$DOCKER_REGISTRY" ]; then
        FULL_IMAGE="${DOCKER_REGISTRY}/${IMAGE_NAME}:${IMAGE_TAG}"
        print_step "Tagging image for registry: ${FULL_IMAGE}"
        docker tag "${IMAGE_NAME}:${IMAGE_TAG}" "${FULL_IMAGE}"

        print_step "Pushing image to registry..."
        docker push "${FULL_IMAGE}"
        print_step "Image pushed successfully"

        # Update image name for deployment
        IMAGE_NAME="${FULL_IMAGE}"
    fi
}

# Deploy test job to cluster
deploy_job() {
    print_step "Deploying test job to cluster"

    # Check if kubectl is available
    if ! command -v kubectl &> /dev/null; then
        print_error "kubectl not found. Please install kubectl."
        exit 1
    fi

    # Check cluster connectivity
    if ! kubectl cluster-info &> /dev/null; then
        print_error "Cannot connect to Kubernetes cluster. Please check your kubeconfig."
        exit 1
    fi

    print_step "Current cluster: $(kubectl config current-context)"

    # Clean up existing job if present
    if kubectl get job modelserving-integration-test -n "$NAMESPACE" &> /dev/null; then
        print_warning "Existing test job found. Deleting..."
        kubectl delete job modelserving-integration-test -n "$NAMESPACE" --wait=true
    fi

    # Apply RBAC and Job manifest
    print_step "Creating ServiceAccount and RBAC..."
    kubectl apply -f "$SCRIPT_DIR/job.yaml" -n "$NAMESPACE"

    # Patch job with custom image and environment
    print_step "Configuring test job..."
    kubectl patch job modelserving-integration-test -n "$NAMESPACE" --type=json -p="[
        {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/image\", \"value\": \"${IMAGE_NAME}:${IMAGE_TAG}\"},
        {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/env/0/value\", \"value\": \"${TEST_TIMEOUT}\"},
        {\"op\": \"replace\", \"path\": \"/spec/template/spec/containers/0/env/1/value\", \"value\": \"${TEST_FILTER}\"}
    ]"

    print_step "Test job deployed successfully"
    print_step "Job name: modelserving-integration-test"
    print_step "Namespace: $NAMESPACE"
    echo ""
    print_step "To view logs, run: $0 logs"
    print_step "To cleanup, run: $0 clean"
}

# View test job logs
view_logs() {
    print_step "Fetching test job logs..."

    # Wait for pod to be created
    print_step "Waiting for test pod to start..."
    kubectl wait --for=condition=ready pod \
        -l app=modelserving-test \
        -n "$NAMESPACE" \
        --timeout=60s || true

    # Stream logs
    POD_NAME=$(kubectl get pods -l app=modelserving-test -n "$NAMESPACE" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -z "$POD_NAME" ]; then
        print_error "Test pod not found"
        exit 1
    fi

    print_step "Streaming logs from pod: $POD_NAME"
    echo ""
    kubectl logs -f "$POD_NAME" -n "$NAMESPACE"
}

# Run existing test job
run_job() {
    print_step "Running test job..."

    # Check if job exists
    if ! kubectl get job modelserving-integration-test -n "$NAMESPACE" &> /dev/null; then
        print_error "Test job not found. Run '$0 deploy' first."
        exit 1
    fi

    # Delete the job and recreate to trigger a new run
    print_step "Recreating job to trigger new run..."
    kubectl delete job modelserving-integration-test -n "$NAMESPACE" --wait=true
    deploy_job
    view_logs
}

# Clean up test resources
cleanup() {
    print_step "Cleaning up test resources..."

    # Delete job
    if kubectl get job modelserving-integration-test -n "$NAMESPACE" &> /dev/null; then
        kubectl delete job modelserving-integration-test -n "$NAMESPACE" --wait=true
        print_step "Test job deleted"
    fi

    # Delete RBAC (optional, commented out to preserve for next run)
    # kubectl delete -f "$SCRIPT_DIR/job.yaml" -n "$NAMESPACE"

    # Clean up test namespaces created by tests
    print_step "Cleaning up test namespaces..."
    kubectl get namespaces -o name | grep "kthena-integration-" | xargs -r kubectl delete

    print_step "Cleanup complete"
}

# Run smoke tests
run_smoke() {
    TEST_FILTER="TestLifecycleCreateDelete|TestServingGroupScale"
    TEST_TIMEOUT="10m"
    print_step "Running smoke tests..."
    deploy_job
    view_logs
}

# Main execution
case "$ACTION" in
    build)
        build_image
        ;;
    deploy)
        build_image
        deploy_job
        echo ""
        print_step "Deployment complete. Viewing logs..."
        sleep 3
        view_logs
        ;;
    run)
        run_job
        ;;
    logs)
        view_logs
        ;;
    clean|cleanup)
        cleanup
        ;;
    smoke)
        run_smoke
        ;;
    help|--help|-h)
        usage
        ;;
    *)
        print_error "Unknown action: $ACTION"
        usage
        ;;
esac
