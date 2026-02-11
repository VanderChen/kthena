# ModelServing Integration Test Suite Architecture

```
┌─────────────────────────────────────────────────────────────────────────┐
│                        Integration Test Suite                            │
│                                                                           │
│  ┌────────────────────────────────────────────────────────────────────┐ │
│  │                        Go Test Layer                                │ │
│  │  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐             │ │
│  │  │ suite_test.go│  │lifecycle_test│  │ scaling_test │             │ │
│  │  │  (TestMain)  │  │              │  │              │             │ │
│  │  │              │  │              │  │              │             │ │
│  │  │  - Setup NS  │  │  - Load YAML │  │  - Load YAML │  ...        │ │
│  │  │  - Init      │  │  - Run tests │  │  - Run tests │             │ │
│  │  │  - Cleanup   │  │              │  │              │             │ │
│  │  └──────┬───────┘  └──────┬───────┘  └──────┬───────┘             │ │
│  │         │                 │                 │                      │ │
│  │         └─────────────────┴─────────────────┘                      │ │
│  │                           │                                         │ │
│  └───────────────────────────┼─────────────────────────────────────── │ │
│                              │                                          │
│  ┌───────────────────────────▼─────────────────────────────────────── │ │
│  │                    Test Runner (runner.go)                          │ │
│  │                                                                      │ │
│  │  ┌────────────────────────────────────────────────────────────┐   │ │
│  │  │  LoadTestCase(yaml) → Parse → Interpolate Variables        │   │ │
│  │  │  RunTestCase()      → Setup → Steps → Cleanup              │   │ │
│  │  │  Context Manager    → {{.TestID}}, {{.Namespace}}          │   │ │
│  │  └────────────────────────────────────────────────────────────┘   │ │
│  │                              │                                      │ │
│  └──────────────────────────────┼────────────────────────────────────  │ │
│                                 │                                       │
│  ┌──────────────────────────────▼──────────────────────────────────┐  │ │
│  │                    Executor (executor.go)                        │  │ │
│  │                                                                   │  │ │
│  │  ┌─────────┐  ┌────────┐  ┌────────┐  ┌──────┐  ┌────────┐    │  │ │
│  │  │ apply   │  │ patch  │  │ delete │  │ wait │  │ assert │    │  │ │
│  │  │         │  │        │  │        │  │      │  │        │    │  │ │
│  │  │ Create  │  │ Update │  │ Remove │  │ Poll │  │ Verify │    │  │ │
│  │  │ Resource│  │ Spec   │  │ K8s    │  │ Until│  │ State  │    │  │ │
│  │  └─────────┘  └────────┘  └────────┘  └──────┘  └────┬───┘    │  │ │
│  │                                                         │        │  │ │
│  └─────────────────────────────────────────────────────────┼───────   │ │
│                                                             │           │
│  ┌──────────────────────────────────────────────────────────▼───────┐ │ │
│  │              Assertions (assertions.go)                           │ │ │
│  │                                                                    │ │ │
│  │  ┌────────────┐  ┌──────────┐  ┌──────────┐  ┌─────────────┐   │ │ │
│  │  │statusField │  │ podCount │  │ podPhase │  │ podRecreated│   │ │ │
│  │  │            │  │          │  │          │  │             │   │ │ │
│  │  │ Check MS   │  │ Count    │  │ Verify   │  │ Check UID   │   │ │ │
│  │  │ Status     │  │ Pods by  │  │ Phase    │  │ Changed     │   │ │ │
│  │  │ Fields     │  │ Selector │  │ Match    │  │             │   │ │ │
│  │  └────────────┘  └──────────┘  └──────────┘  └─────────────┘   │ │ │
│  │                                                                    │ │ │
│  │  Operators: equals, notEquals, >, >=, <, <=                      │ │ │
│  └────────────────────────────────────────────────────────────────── │ │
│                                                                        │
│  ┌─────────────────────────────────────────────────────────────────┐ │ │
│  │              Chaos Injector (chaos.go)                            │ │ │
│  │                                                                    │ │ │
│  │  ┌──────────────┐  ┌─────────────┐  ┌──────────┐               │ │ │
│  │  │InjectPending │  │InjectError  │  │DeletePod │               │ │ │
│  │  │              │  │             │  │          │               │ │ │
│  │  │Add Invalid   │  │Kill Process │  │Trigger   │               │ │ │
│  │  │NodeSelector  │  │in Container │  │Recovery  │               │ │ │
│  │  └──────────────┘  └─────────────┘  └──────────┘               │ │ │
│  └────────────────────────────────────────────────────────────────── │ │
│                                                                        │
└────────────────────────────────────────────────────────────────────── │
                                                                         │
┌─────────────────────────────────────────────────────────────────────┐ │
│                        YAML Test Cases                               │ │
│                                                                       │ │
│  testcases/                                                          │ │
│  ├── lifecycle/                                                      │ │
│  │   └── 01_create_delete.yaml ──────────────────┐                 │ │
│  │       apiVersion: test.kthena.io/v1            │                 │ │
│  │       kind: TestCase                           │                 │ │
│  │       spec:                                     │                 │ │
│  │         setup:                                  │                 │ │
│  │           - action: apply                       │                 │ │
│  │             manifest: fixtures/basic_ms.yaml    │                 │ │
│  │             patches: [...]                      │                 │ │
│  │         steps:                                  │                 │ │
│  │           - action: assert                      │                 │ │
│  │             assertions: [...]                   │                 │ │
│  │         cleanup:                                │                 │ │
│  │           - action: delete                      │                 │ │
│  │                                                                   │ │
│  ├── scaling/                                                        │ │
│  │   ├── 01_servinggroup_scale.yaml ──────────┐                    │ │
│  │   │   Test: 1→3→1 replica scaling           │                    │ │
│  │   │   Actions: patch, wait, assert          │                    │ │
│  │   │                                          │                    │ │
│  │   ├── 02_role_scale.yaml ──────────────────┤                    │ │
│  │   │   Test: Role replica scaling            │                    │ │
│  │   │                                          │                    │ │
│  │   └── 03_worker_scale.yaml ────────────────┘                    │ │
│  │       Test: Worker replica scaling                               │ │
│  │                                                                   │ │
│  └── recovery/                                                       │ │
│      └── 01_pod_delete.yaml ───────────────────┐                   │ │
│          Test: Automatic pod recovery           │                   │ │
│          Actions: query, delete, assert         │                   │ │
│                                                                       │ │
└────────────────────────────────────────────────────────────────────── │
                                                                         │
┌─────────────────────────────────────────────────────────────────────┐ │
│                           Fixtures                                   │ │
│                                                                       │ │
│  fixtures/                                                           │ │
│  ├── basic_modelserving.yaml                                        │ │
│  │   - Single role                                                   │ │
│  │   - nginx container                                               │ │
│  │   - Configurable replicas                                         │ │
│  │                                                                   │ │
│  └── multi_role_modelserving.yaml                                   │ │
│      - Multiple roles (entry + storage)                              │ │
│      - Entry with worker replicas                                    │ │
│      - Demonstrates complex configurations                           │ │
│                                                                       │ │
└────────────────────────────────────────────────────────────────────── │
                                                                         │
┌─────────────────────────────────────────────────────────────────────┐ │
│                    Kubernetes Cluster                                │ │
│                                                                       │ │
│  ┌────────────────────────────────────────────────────────────────┐ │ │
│  │  ModelServing Controller  ◄──── Tests Interact With             │ │ │
│  │  Volcano Scheduler        ◄──── Tests Verify                     │ │ │
│  │  Pods, Services, CRDs     ◄──── Tests Create/Delete             │ │ │
│  └────────────────────────────────────────────────────────────────┘ │ │
│                                                                       │ │
└────────────────────────────────────────────────────────────────────── │
```

## Data Flow

```
1. User runs: make test-integration-modelserving
               │
2. Go test discovers test files (lifecycle_test.go, scaling_test.go, etc.)
               │
3. TestMain creates namespace: kthena-integration-xxxxx
               │
4. Test function loads YAML test case
               │
5. TestRunner parses YAML and interpolates variables
               │
6. TestRunner executes setup steps
   ├── Executor.apply() creates ModelServing from fixture
   └── Executor.wait() polls until ready
               │
7. TestRunner executes test steps
   ├── Executor.patch() scales replicas
   ├── Executor.assert() → Assertions.statusField()
   └── Executor.assert() → Assertions.podCount()
               │
8. TestRunner executes cleanup steps
   └── Executor.delete() removes ModelServing
               │
9. TestMain deletes namespace
               │
10. Test results reported
```

## Key Design Patterns

### 1. Strategy Pattern
- Different executors for different actions (apply, patch, delete, wait, assert)
- Pluggable assertion types

### 2. Template Method Pattern
- TestRunner defines test execution flow: setup → steps → cleanup
- Subclasses (YAML tests) customize the steps

### 3. Builder Pattern
- YAML files build complex test scenarios declaratively
- Patches customize fixtures without duplication

### 4. Observer Pattern
- Wait conditions poll Kubernetes resources until satisfied
- Assertions retry until timeout

### 5. Context Pattern
- Test context stores shared data between steps
- Variables like {{.TestID}} prevent conflicts

## Extension Points

### Adding New Actions
```go
// In executor.go
func (e *Executor) executeMyAction(ctx context.Context, step TestStep, runner *TestRunner) error {
    // Implementation
}

// In Execute() method
case "myAction":
    return e.executeMyAction(ctx, step, runner)
```

### Adding New Assertions
```go
// In assertions.go
func AssertMyCondition(ctx context.Context, ...) error {
    // Implementation
}

// In runAssertion() method
case "myCondition":
    return AssertMyCondition(ctx, ...)
```

### Adding New Test Cases
```yaml
# In testcases/category/nn_test_name.yaml
apiVersion: test.kthena.io/v1
kind: TestCase
metadata:
  name: my-test
spec:
  steps:
    - name: my-step
      action: apply
      # ...
```

No code changes needed - automatically discovered!
