#!/bin/bash
# Verification script for ModelServing Integration Test Suite

set -e

echo "================================================"
echo "ModelServing Integration Test Suite Verification"
echo "================================================"
echo ""

# Colors
GREEN='\033[0;32m'
RED='\033[0;31m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

# Function to print status
print_status() {
    if [ $1 -eq 0 ]; then
        echo -e "${GREEN}✓${NC} $2"
    else
        echo -e "${RED}✗${NC} $2"
    fi
}

# Check if Go is installed
echo "Checking prerequisites..."
if command -v go &> /dev/null; then
    GO_VERSION=$(go version | awk '{print $3}')
    print_status 0 "Go is installed: $GO_VERSION"
else
    print_status 1 "Go is not installed"
    exit 1
fi

# Check if kubectl is installed
if command -v kubectl &> /dev/null; then
    print_status 0 "kubectl is installed"
else
    print_status 1 "kubectl is not installed"
    echo "Note: kubectl is required to run integration tests"
fi

echo ""
echo "Checking file structure..."

# Check if all required files exist
REQUIRED_FILES=(
    "test/integration/modelserving/runner/types.go"
    "test/integration/modelserving/runner/runner.go"
    "test/integration/modelserving/runner/executor.go"
    "test/integration/modelserving/runner/assertions.go"
    "test/integration/modelserving/runner/chaos.go"
    "test/integration/modelserving/suite_test.go"
    "test/integration/modelserving/lifecycle_test.go"
    "test/integration/modelserving/scaling_test.go"
    "test/integration/modelserving/recovery_test.go"
    "test/integration/modelserving/fixtures/basic_modelserving.yaml"
    "test/integration/modelserving/fixtures/multi_role_modelserving.yaml"
    "test/integration/modelserving/testcases/lifecycle/01_create_delete.yaml"
    "test/integration/modelserving/testcases/scaling/01_servinggroup_scale.yaml"
    "test/integration/modelserving/testcases/scaling/02_role_scale.yaml"
    "test/integration/modelserving/testcases/scaling/03_worker_scale.yaml"
    "test/integration/modelserving/testcases/recovery/01_pod_delete.yaml"
    "test/integration/modelserving/README.md"
)

ALL_FILES_EXIST=true
for file in "${REQUIRED_FILES[@]}"; do
    if [ -f "$file" ]; then
        print_status 0 "$file"
    else
        print_status 1 "$file (missing)"
        ALL_FILES_EXIST=false
    fi
done

if [ "$ALL_FILES_EXIST" = false ]; then
    echo ""
    echo -e "${RED}Some required files are missing!${NC}"
    exit 1
fi

echo ""
echo "Checking Go code compilation..."

# Try to build the runner package
if go build ./test/integration/modelserving/runner/... 2>&1 | grep -q "error"; then
    print_status 1 "Runner package compilation failed"
    go build ./test/integration/modelserving/runner/...
    exit 1
else
    print_status 0 "Runner package compiles successfully"
fi

# Try to compile the test package
if go test -c ./test/integration/modelserving -o /tmp/test-binary 2>&1 | grep -q "error"; then
    print_status 1 "Test package compilation failed"
    go test -c ./test/integration/modelserving
    exit 1
else
    print_status 0 "Test package compiles successfully"
    rm -f /tmp/test-binary
fi

echo ""
echo "Checking YAML test case syntax..."

# Basic YAML validation
YAML_FILES=$(find test/integration/modelserving/testcases -name "*.yaml")
YAML_VALID=true

for yaml_file in $YAML_FILES; do
    # Check if file has required fields
    if grep -q "apiVersion:" "$yaml_file" && \
       grep -q "kind: TestCase" "$yaml_file" && \
       grep -q "metadata:" "$yaml_file" && \
       grep -q "spec:" "$yaml_file"; then
        print_status 0 "$(basename $yaml_file) has valid structure"
    else
        print_status 1 "$(basename $yaml_file) has invalid structure"
        YAML_VALID=false
    fi
done

if [ "$YAML_VALID" = false ]; then
    echo ""
    echo -e "${RED}Some YAML test cases have invalid structure!${NC}"
    exit 1
fi

echo ""
echo "Checking Makefile targets..."

if grep -q "test-integration-modelserving:" Makefile; then
    print_status 0 "test-integration-modelserving target exists"
else
    print_status 1 "test-integration-modelserving target missing"
fi

if grep -q "test-integration-modelserving-smoke:" Makefile; then
    print_status 0 "test-integration-modelserving-smoke target exists"
else
    print_status 1 "test-integration-modelserving-smoke target missing"
fi

echo ""
echo "Checking cluster connectivity (optional)..."

if kubectl cluster-info &> /dev/null; then
    print_status 0 "Connected to Kubernetes cluster"

    # Check if ModelServing CRD exists
    if kubectl get crd modelservings.workload.serving.volcano.sh &> /dev/null; then
        print_status 0 "ModelServing CRD is installed"
    else
        print_status 1 "ModelServing CRD not found (tests will fail without it)"
    fi

    # Check if controller is running
    if kubectl get pods -n kthena-system -l app=model-serving-controller &> /dev/null 2>&1; then
        CONTROLLER_STATUS=$(kubectl get pods -n kthena-system -l app=model-serving-controller -o jsonpath='{.items[0].status.phase}' 2>/dev/null || echo "NotFound")
        if [ "$CONTROLLER_STATUS" = "Running" ]; then
            print_status 0 "ModelServing controller is running"
        else
            print_status 1 "ModelServing controller not running (status: $CONTROLLER_STATUS)"
        fi
    else
        echo -e "${YELLOW}⚠${NC} ModelServing controller status unknown (might be in different namespace)"
    fi
else
    echo -e "${YELLOW}⚠${NC} Not connected to Kubernetes cluster (integration tests require a cluster)"
fi

echo ""
echo "================================================"
echo -e "${GREEN}✓ Verification Complete!${NC}"
echo "================================================"
echo ""
echo "Next steps:"
echo "  1. Ensure ModelServing controller is deployed in your cluster"
echo "  2. Run smoke tests: make test-integration-modelserving-smoke"
echo "  3. Run full test suite: make test-integration-modelserving"
echo ""
echo "For more information, see:"
echo "  - test/integration/modelserving/README.md"
echo "  - test/integration/modelserving/ARCHITECTURE.md"
echo "  - test/integration/modelserving/IMPLEMENTATION_SUMMARY.md"
echo ""
