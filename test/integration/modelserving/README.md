# ModelServing Controller Integration Test Suite

This directory contains a YAML-driven integration test framework for the ModelServing controller. The test suite enables end-to-end testing of ModelServing functionality in a real Kubernetes cluster.

## Overview

The integration test suite follows a hybrid architecture:
- **Test Cases**: Defined in YAML files under `testcases/` directory
- **Test Engine**: Go-based runner that parses and executes YAML test cases
- **Fixtures**: Reusable ModelServing templates under `fixtures/` directory

### Key Features

- **YAML-Driven**: Test cases are defined in YAML, making them easy to read and maintain
- **Extensible**: Add new tests by creating YAML files without modifying code
- **Flexible**: Complex scenarios can still be written in Go when needed
- **Comprehensive**: Covers lifecycle, scaling, recovery, and fault injection scenarios
- **Isolated**: Each test run uses a unique namespace for isolation

## Directory Structure

```
test/integration/modelserving/
├── runner/                          # Test execution engine
│   ├── types.go                     # Data structures for test cases
│   ├── runner.go                    # YAML parsing and orchestration
│   ├── executor.go                  # Operation executors (apply/patch/delete/wait/assert)
│   ├── assertions.go                # Assertion library
│   └── chaos.go                     # Fault injection tools
├── testcases/                       # YAML test case library
│   ├── lifecycle/                   # Lifecycle tests
│   │   └── 01_create_delete.yaml
│   ├── scaling/                     # Scaling tests
│   │   ├── 01_servinggroup_scale.yaml
│   │   ├── 02_role_scale.yaml
│   │   └── 03_worker_scale.yaml
│   └── recovery/                    # Recovery tests
│       └── 01_pod_delete.yaml
├── fixtures/                        # Reusable ModelServing templates
│   ├── basic_modelserving.yaml
│   └── multi_role_modelserving.yaml
├── suite_test.go                    # TestMain and shared setup
├── lifecycle_test.go                # Lifecycle scenario tests
├── scaling_test.go                  # Scaling scenario tests
├── recovery_test.go                 # Recovery scenario tests
└── README.md                        # This file
```

## Prerequisites

### Required
- Kubernetes cluster (1.24+)
- ModelServing controller deployed in the cluster
- Volcano scheduler deployed in the cluster
- `kubectl` configured to access the cluster
- Go 1.21+ installed

### Optional
- `make` for running convenience targets

## Running Tests

### Using Make

Run all integration tests:
```bash
make test-integration-modelserving
```

### Using Go Test

Run all tests:
```bash
go test ./test/integration/modelserving/... -v -timeout=30m
```

Run specific test suite:
```bash
# Lifecycle tests only
go test ./test/integration/modelserving -run TestLifecycle -v

# Scaling tests only
go test ./test/integration/modelserving -run TestScaling -v

# Recovery tests only
go test ./test/integration/modelserving -run TestRecovery -v
```

Run specific test case:
```bash
go test ./test/integration/modelserving -run TestServingGroupScale -v
```

### Environment Variables

- `KUBECONFIG`: Path to kubeconfig file (defaults to `~/.kube/config`)

## Running Tests in Kubernetes

For production environments or CI/CD pipelines, you can run tests directly in the Kubernetes cluster without local compilation.

### Quick Start

```bash
# Build and run all tests in cluster
make test-integration-modelserving-k8s

# Run smoke tests only
make test-integration-modelserving-k8s-smoke

# View test logs
make test-integration-modelserving-k8s-logs

# Clean up test resources
make test-integration-modelserving-k8s-clean
```

### Using the Deploy Script Directly

The `deploy.sh` script provides more control:

```bash
# Deploy all tests
./test/integration/modelserving/deploy.sh deploy

# Run smoke tests
./test/integration/modelserving/deploy.sh smoke

# Run specific tests
TEST_FILTER="TestServingGroupScale" ./test/integration/modelserving/deploy.sh deploy

# View logs
./test/integration/modelserving/deploy.sh logs

# Clean up
./test/integration/modelserving/deploy.sh clean
```

### Advanced Configuration

#### Custom Image Registry

For remote clusters, push the image to a registry:

```bash
DOCKER_REGISTRY=myregistry.io \
IMAGE_NAME=myrepo/modelserving-test \
IMAGE_TAG=v1.0.0 \
./test/integration/modelserving/deploy.sh deploy
```

#### Custom Test Filter

Run specific test patterns:

```bash
# Run only scaling tests
TEST_FILTER="TestScaling" ./test/integration/modelserving/deploy.sh deploy

# Run specific test case
TEST_FILTER="TestServingGroupScale" ./test/integration/modelserving/deploy.sh deploy
```

#### Custom Timeout

Adjust test timeout:

```bash
TEST_TIMEOUT="60m" ./test/integration/modelserving/deploy.sh deploy
```

#### Custom Namespace

Deploy to a specific namespace:

```bash
NAMESPACE="kthena-test" ./test/integration/modelserving/deploy.sh deploy
```

### Kubernetes Resources

The deployment creates the following resources:

1. **ServiceAccount**: `modelserving-test-runner`
   - Used by test pods to interact with cluster

2. **ClusterRole**: `modelserving-test-runner`
   - Permissions for ModelServing, Pods, Namespaces, etc.

3. **ClusterRoleBinding**: Links ServiceAccount to ClusterRole

4. **Job**: `modelserving-integration-test`
   - Runs the test suite
   - Automatically cleaned up after 1 hour (configurable)

### Scheduled Testing with CronJob

For continuous testing, deploy as a CronJob:

```bash
# Deploy CronJob (runs daily at 2 AM)
kubectl apply -f test/integration/modelserving/k8s/cronjob.yaml

# Manually trigger a test run
kubectl create job --from=cronjob/modelserving-integration-test manual-test-$(date +%s)

# View CronJob status
kubectl get cronjob modelserving-integration-test

# View recent test runs
kubectl get jobs -l app=modelserving-test
```

### Viewing Results

```bash
# Get job status
kubectl get job modelserving-integration-test

# View pod logs
kubectl logs -l app=modelserving-test -f

# Check if tests passed
kubectl get job modelserving-integration-test -o jsonpath='{.status.succeeded}'
# Output: 1 = passed, 0 or empty = failed
```

### CI/CD Integration

Example GitLab CI configuration:

```yaml
test-modelserving:
  stage: test
  image: docker:latest
  services:
    - docker:dind
  before_script:
    - apk add --no-cache kubectl
    - kubectl config use-context $KUBE_CONTEXT
  script:
    - cd test/integration/modelserving
    - ./deploy.sh deploy
  artifacts:
    when: always
    paths:
      - test-results/
```

Example GitHub Actions configuration:

```yaml
name: ModelServing Integration Tests
on: [push, pull_request]

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: azure/setup-kubectl@v3
      - uses: azure/k8s-set-context@v3
        with:
          kubeconfig: ${{ secrets.KUBE_CONFIG }}
      - name: Run integration tests
        run: |
          cd test/integration/modelserving
          ./deploy.sh deploy
```

### Environment Variables

### Test Case Structure

A test case YAML file has the following structure:

```yaml
apiVersion: test.kthena.io/v1
kind: TestCase
metadata:
  name: test-name
  description: Test description
  tags: [tag1, tag2]
spec:
  timeout: 10m
  setup:
    - name: setup-step-1
      action: apply
      # ... step details
  steps:
    - name: test-step-1
      action: wait
      # ... step details
  cleanup:
    - name: cleanup-step-1
      action: delete
      # ... step details
```

### Supported Actions

#### 1. Apply - Create a Resource

Creates a Kubernetes resource from a manifest file with optional patches.

```yaml
- name: create-modelserving
  action: apply
  manifest: test/integration/modelserving/fixtures/basic_modelserving.yaml
  patches:
    - path: metadata.name
      value: test-ms-{{.TestID}}
    - path: metadata.namespace
      value: "{{.Namespace}}"
    - path: spec.replicas
      value: 3
```

#### 2. Patch - Update a Resource

Updates an existing resource using merge patch.

```yaml
- name: scale-up
  action: patch
  resource: modelserving/test-ms-{{.TestID}}
  patch:
    spec:
      replicas: 5
```

#### 3. Delete - Delete a Resource

Deletes a resource.

```yaml
- name: delete-modelserving
  action: delete
  resource: modelserving/test-ms-{{.TestID}}
```

#### 4. Wait - Wait for Condition

Waits for a condition to be satisfied.

```yaml
- name: wait-for-ready
  action: wait
  resource: modelserving/test-ms-{{.TestID}}
  condition: ready
  timeout: 5m
```

Supported conditions:
- `ready`: ModelServing is ready (availableReplicas >= desired replicas)
- `deleted`: Resource is deleted
- `running`: Pod is in Running phase

#### 5. Assert - Check Conditions

Runs assertions to verify expected state.

```yaml
- name: verify-state
  action: assert
  assertions:
    - type: statusField
      resource: modelserving/test-ms-{{.TestID}}
      field: status.availableReplicas
      operator: equals
      value: 3
      timeout: 2m
    - type: podCount
      selector: modelserving.volcano.sh/name=test-ms-{{.TestID}}
      operator: equals
      value: 3
      timeout: 2m
```

Supported assertion types:
- `statusField`: Check ModelServing status field
- `podCount`: Check number of pods matching selector
- `podPhase`: Check pod phase (Running/Pending/Failed)

Supported operators:
- `equals` or `==`: Exact match
- `notEquals` or `!=`: Not equal
- `greaterThan` or `>`: Greater than
- `greaterThanOrEqual` or `>=`: Greater than or equal
- `lessThan` or `<`: Less than
- `lessThanOrEqual` or `<=`: Less than or equal

#### 6. Query - Query and Save Data

Queries a resource and saves data to test context for later use.

```yaml
- name: get-pod-uid
  action: query
  query:
    type: podUID
    selector: modelserving.volcano.sh/name=test-ms-{{.TestID}}
    index: 0
  saveTo: originalPodUID
```

#### 7. Sleep - Pause Execution

Pauses execution for a specified duration.

```yaml
- name: wait-for-propagation
  action: sleep
  timeout: 10s
```

### Template Variables

Test cases support template variables that are interpolated at runtime:

- `{{.TestID}}`: Unique test run identifier
- `{{.Namespace}}`: Test namespace
- Custom variables can be saved using `query` action and referenced later

Example:
```yaml
patches:
  - path: metadata.name
    value: test-ms-{{.TestID}}
  - path: metadata.namespace
    value: "{{.Namespace}}"
```

### Example Test Case

Here's a complete example testing ServingGroup scaling:

```yaml
apiVersion: test.kthena.io/v1
kind: TestCase
metadata:
  name: servinggroup-scale-up-down
  description: Test ServingGroup replica scaling from 1→3→1
  tags: [scaling, servinggroup, smoke]
spec:
  timeout: 10m

  setup:
    - name: create-modelserving
      action: apply
      manifest: test/integration/modelserving/fixtures/basic_modelserving.yaml
      patches:
        - path: metadata.name
          value: test-scale-{{.TestID}}
        - path: metadata.namespace
          value: "{{.Namespace}}"
        - path: spec.replicas
          value: 1

    - name: wait-for-ready
      action: wait
      resource: modelserving/test-scale-{{.TestID}}
      condition: ready
      timeout: 5m

  steps:
    - name: verify-initial-state
      action: assert
      assertions:
        - type: statusField
          resource: modelserving/test-scale-{{.TestID}}
          field: status.availableReplicas
          operator: equals
          value: 1
        - type: podCount
          selector: modelserving.volcano.sh/name=test-scale-{{.TestID}}
          operator: equals
          value: 1

    - name: scale-up-to-3
      action: patch
      resource: modelserving/test-scale-{{.TestID}}
      patch:
        spec:
          replicas: 3

    - name: verify-scale-up
      action: assert
      assertions:
        - type: statusField
          resource: modelserving/test-scale-{{.TestID}}
          field: status.availableReplicas
          operator: equals
          value: 3
          timeout: 3m

    - name: scale-down-to-1
      action: patch
      resource: modelserving/test-scale-{{.TestID}}
      patch:
        spec:
          replicas: 1

    - name: verify-scale-down
      action: assert
      assertions:
        - type: statusField
          field: status.availableReplicas
          operator: equals
          value: 1
          timeout: 3m

  cleanup:
    - name: delete-modelserving
      action: delete
      resource: modelserving/test-scale-{{.TestID}}
```

## Test Coverage

### Lifecycle Tests

- **Create and Delete**: Basic creation and deletion lifecycle
- **Update Spec**: Updating ModelServing specification

### Scaling Tests

- **ServingGroup Scale**: Scaling `spec.replicas` (1→3→1)
- **Role Scale**: Scaling `spec.template.roles[].replicas`
- **Worker Scale**: Scaling `spec.template.roles[].workerReplicas`

### Recovery Tests

- **Pod Delete Recovery**: Automatic pod recovery after manual deletion
- **Pod Error Recovery**: Recovery from pod error state
- **Restart Policy**: Testing different recovery policies

## Extending the Framework

### Adding New Test Cases

1. Create a new YAML file in the appropriate `testcases/` subdirectory
2. Follow the test case structure described above
3. Run tests - the new case will be automatically discovered and executed

### Adding New Assertions

1. Add assertion type to `runner/assertions.go`
2. Implement assertion logic
3. Update documentation

### Adding New Actions

1. Add action handler to `runner/executor.go`
2. Implement action logic
3. Update documentation

## Troubleshooting

### Test Timeout

If tests timeout, increase the timeout in the test case YAML:
```yaml
spec:
  timeout: 20m  # Increase from default
```

Or for specific steps:
```yaml
- name: wait-for-ready
  action: wait
  timeout: 10m  # Increase wait timeout
```

### Namespace Not Cleaned Up

If the test namespace persists after test failure, manually delete it:
```bash
kubectl delete namespace kthena-integration-<random-id>
```

### Controller Not Ready

Ensure the ModelServing controller is running:
```bash
kubectl get pods -n kthena-system
kubectl logs -n kthena-system deployment/model-serving-controller
```

### Resource Already Exists

This usually happens when a previous test run didn't clean up properly. Delete the conflicting resource:
```bash
kubectl delete modelserving <name> -n <namespace>
```

## Best Practices

1. **Unique Names**: Always use `{{.TestID}}` in resource names to avoid conflicts
2. **Proper Cleanup**: Always include cleanup steps to delete created resources
3. **Reasonable Timeouts**: Set appropriate timeouts based on expected operation duration
4. **Descriptive Names**: Use clear, descriptive names for test steps
5. **Assertion Timeouts**: Include timeouts in assertions to allow time for eventual consistency
6. **Isolation**: Each test should be independent and not rely on other tests

## Contributing

When contributing new test cases:

1. Follow the existing naming conventions (01_, 02_, etc.)
2. Include descriptive metadata (name, description, tags)
3. Add appropriate cleanup steps
4. Test locally before submitting
5. Update this README if adding new capabilities

## License

Copyright The Volcano Authors. Licensed under the Apache License, Version 2.0.
