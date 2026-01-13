# LWS Plugin for ModelServing

## Motivation

As users migrate their inference workloads from LeaderWorkerSet (LWS) to Kthena ModelServing, they often face compatibility issues. Existing workloads may rely on LWS-specific environment variables, labels, and annotations for service discovery, coordination, and metrics. To ease the migration process and support LWS semantics within ModelServing, we propose a new built-in plugin named `lws`.

## Goals

- Provide a drop-in compatibility layer for LWS workloads running on ModelServing.
- Inject LWS-compatible environment variables into pods.
- Inject LWS-compatible labels and annotations into pods.

## Design

The `lws` plugin will be a built-in plugin in `model-serving-controller`. It implements the `Plugin` interface and hooks into the `OnPodCreate` lifecycle method.

### injected Metadata

The plugin will map ModelServing concepts to LWS equivalents:

#### Labels

| LWS Label Key | Value Source |
|---------------|--------------|
| `leaderworkerset.sigs.k8s.io/name` | `ModelServing.Name` |
| `leaderworkerset.sigs.k8s.io/worker-index` | `WORKER_INDEX` env var (0 for Entry, 1+ for Workers) |
| `leaderworkerset.sigs.k8s.io/group-index` | Derived from `ServingGroup` name ordinal |
| `leaderworkerset.sigs.k8s.io/group-key` | SHA1 hash of `ServingGroup` name |

#### Annotations

| LWS Annotation Key | Value Source |
|--------------------|--------------|
| `leaderworkerset.sigs.k8s.io/size` | `GROUP_SIZE` env var |
| `leaderworkerset.sigs.k8s.io/replicas` | `ModelServing.Spec.Replicas` |

#### Environment Variables

| LWS Env Var | ModelServing Source | Value |
|-------------|---------------------|-------|
| `LWS_LEADER_ADDRESS` | `ENTRY_ADDRESS` | DNS name of the entry pod |
| `LWS_GROUP_SIZE` | `GROUP_SIZE` | Total number of pods in the group |
| `LWS_WORKER_INDEX` | `WORKER_INDEX` | Unique index within the group |

### Implementation Details

1.  **Registry**: The plugin will register itself with the name `lws`.
2.  **Configuration**: The plugin requires no additional configuration but can be enabled via `spec.plugins` in the `ModelServing` CRD.
3.  **Logic**:
    -   In `OnPodCreate`, the plugin inspects the `HookRequest`.
    -   It retrieves existing ModelServing environment variables (`WORKER_INDEX`, `GROUP_SIZE`, `ENTRY_ADDRESS`) from the pod spec.
    -   It parses the `ServingGroup` name to extract the group index.
    -   It injects the corresponding LWS labels, annotations, and environment variables into the Pod spec.

## Usage Example

```yaml
apiVersion: workload.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: lws-compatible-serving
spec:
  plugins:
    - name: lws
  template:
    roles:
      - name: worker
        replicas: 1
        workerReplicas: 3
        ...
```
