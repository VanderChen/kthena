# Ranktable for Ascend Resources

This directory contains examples for generating role/group level ranktables for Ascend 910 resources.

## Overview

The ranktable feature supports vLLM-Ascend and MindIE inference engines' HCCL communication needs by automatically generating ranktable ConfigMaps based on pod annotations.

## Plugins

Kthena provides two ranktable plugins:

### 1. `ranktable` Plugin

Uses the `server_id` from pod annotations to organize server lists.

**Use case**: When your external component (e.g., device plugin) provides a consistent server_id in the pod annotation.

**Example annotation**:
```json
{
  "pod_name": "qwen-inference-worker-0",
  "server_id": "192.168.1.10",
  "devices": [
    {"device_id": "0", "device_ip": "10.20.0.2"},
    {"device_id": "1", "device_ip": "10.20.0.3"}
  ]
}
```

### 2. `pod-ranktable` Plugin

Uses the **Pod IP address** as the `server_id`, ignoring any `server_id` from annotations.

**Use case**: When you want to use Pod IPs to identify servers in the ranktable, regardless of what's in the annotation.

**Example annotation** (server_id field is ignored):
```json
{
  "pod_name": "qwen-inference-worker-0",
  "server_id": "ignored-value",
  "devices": [
    {"device_id": "0", "device_ip": "10.20.0.2"},
    {"device_id": "1", "device_ip": "10.20.0.3"}
  ]
}
```

**Generated ranktable** will use Pod IP (e.g., `10.244.1.5`) as the `server_id` instead of `ignored-value`.

## Components

### 1. Pod Ranktable Parser Template

**File**: `pod-ranktable-parser-cce.yaml`

This ConfigMap defines how to parse the pod ranktable annotation (`cce.kubectl.kubernetes.io/ascend-1980-configuration`) into structured data.

```bash
kubectl apply -f pod-ranktable-parser-cce.yaml
```

### 2. Ranktable Templates

Templates are available for both role-level and group-level ranktable generation:

#### Role-Level Templates

**Files**:
- `ranktable-template-mindie-role-cce.yaml` - Uses server_id from annotation
- `pod-ranktable-template-mindie-role-cce.yaml` - Uses Pod IP as server_id

Generates one ranktable ConfigMap per role instance. Each role has its own isolated ranktable.

```bash
# For ranktable plugin (server_id from annotation)
kubectl apply -f ranktable-template-mindie-role-cce.yaml

# OR for pod-ranktable plugin (server_id from Pod IP)
kubectl apply -f pod-ranktable-template-mindie-role-cce.yaml
```

#### Group-Level Templates

**Files**:
- `ranktable-template-mindie-group-cce.yaml` - Uses server_id from annotation
- `pod-ranktable-template-mindie-group-cce.yaml` - Uses Pod IP as server_id

Generates one ranktable ConfigMap per serving group. All roles within the group share the same ranktable.

```bash
# For ranktable plugin (server_id from annotation)
kubectl apply -f ranktable-template-mindie-group-cce.yaml

# OR for pod-ranktable plugin (server_id from Pod IP)
kubectl apply -f pod-ranktable-template-mindie-group-cce.yaml
```

### 3. ModelServing Examples

Multiple example files demonstrate different configurations:

**Role-Level Examples**:
- `modelserving-test-cce.yaml` - Basic role-level ranktable example

**Group-Level Examples**:
- `modelserving-ranktable-group-cce.yaml` - Group-level with ranktable plugin (server_id from annotation)
- `modelserving-pod-ranktable-group-cce.yaml` - Group-level with pod-ranktable plugin (server_id from Pod IP)

```bash
# Apply role-level example
kubectl apply -f modelserving-test-cce.yaml

# OR apply group-level example
kubectl apply -f modelserving-ranktable-group-cce.yaml

# OR apply group-level pod-ranktable example
kubectl apply -f modelserving-pod-ranktable-group-cce.yaml
```

## How It Works

### Workflow

1. **Create Templates**: Apply the pod parser template and ranktable template ConfigMaps to `kthena-system` namespace
2. **Create ModelServing**: Create a ModelServing CR with the `ranktable` plugin configured
3. **Controller Creates ConfigMaps**: The controller (via plugin) creates empty ranktable ConfigMaps for each role/group
4. **Controller Creates Pods**: Pods are created with ranktable volume mounts
5. **External Component Injects Annotations**: An external component (e.g., device plugin) injects pod ranktable data into pod annotations
6. **Controller Generates Ranktables**: The controller (via plugin) watches pod annotations, parses them, and generates ranktables
7. **Ranktables Ready**: Once ranktables are ready, applications can read the configuration

### Key Features

- **Template-based**: Flexible Go templates for parsing and generating ranktables
- **Automatic Injection**: Volume mounts are automatically injected
- **Level Support**: Supports both role-level and group-level ranktable generation
- **Rank ID Generation**: Automatically generates rank_id based on device_id lexicographic order

## Usage

### Step 1: Deploy Templates

```bash
# Create kthena-system namespace if not exists
kubectl create namespace kthena-system

# Deploy pod parser template (required for both plugins)
kubectl apply -f pod-ranktable-parser-cce.yaml

# Deploy ranktable template (choose one based on your needs)
# For role-level with ranktable plugin:
kubectl apply -f ranktable-template-mindie-role-cce.yaml
# OR for role-level with pod-ranktable plugin:
kubectl apply -f pod-ranktable-template-mindie-role-cce.yaml

# OR for group-level with ranktable plugin:
kubectl apply -f ranktable-template-mindie-group-cce.yaml
# OR for group-level with pod-ranktable plugin:
kubectl apply -f pod-ranktable-template-mindie-group-cce.yaml
```

### Step 2: Create ModelServing

```bash
# For role-level example:
kubectl apply -f modelserving-test-cce.yaml

# OR for group-level with ranktable plugin:
kubectl apply -f modelserving-ranktable-group-cce.yaml

# OR for group-level with pod-ranktable plugin:
kubectl apply -f modelserving-pod-ranktable-group-cce.yaml
```

### Step 3: Verify

```bash
# Check if ranktable ConfigMaps are created
kubectl get configmap -l app.kubernetes.io/component=ranktable

# Check pod status (replace <group-name> with your serving group name)
kubectl get pods -l workload.kthena.io/group-name=<group-name>

# View ranktable content (for role-level)
kubectl get configmap <modelserving-name>-ranktable-<group-name>-<role-id> -o jsonpath='{.data.ranktable\.json}' | jq .

# View ranktable content (for group-level)
kubectl get configmap <modelserving-name>-ranktable-<group-name> -o jsonpath='{.data.ranktable\.json}' | jq .

# Example for role-level:
kubectl get configmap test-ranktable-cce-ranktable-default-inference-0 -o jsonpath='{.data.ranktable\.json}' | jq .

# Example for group-level:
kubectl get configmap test-ranktable-group-cce-ranktable-default -o jsonpath='{.data.ranktable\.json}' | jq .
```

## Configuration

### Choosing Between Role-Level and Group-Level

**Role-Level Ranktables**:
- ✅ Use when each role operates independently
- ✅ Use when roles have different communication patterns
- ✅ Simpler for single-role deployments
- ✅ Each role has isolated ranktable configuration

**Group-Level Ranktables**:
- ✅ Use when multiple roles need to communicate with each other
- ✅ Use for distributed training/inference across roles
- ✅ Automatic updates when roles are scaled or deleted
- ✅ Single unified ranktable for all pods in the serving group

**Example Use Cases**:
- **Role-Level**: Each inference role processes requests independently
- **Group-Level**: Master-worker pattern where master coordinates with multiple worker roles

### ModelServing Plugins

To enable ranktable generation, add either the `ranktable` or `pod-ranktable` plugin to your ModelServing spec:

**Using `ranktable` plugin** (server_id from annotation):
```yaml
spec:
  plugins:
    - name: ranktable
      type: BuiltIn
      config:
        template: "ranktable-template-name"
```

**Using `pod-ranktable` plugin** (server_id from Pod IP):
```yaml
spec:
  plugins:
    - name: pod-ranktable
      type: BuiltIn
      config:
        template: "pod-ranktable-template-name"
```

### ModelServing Annotations

- `kthena.io/ranktable-level`: **(Optional)** Override ranktable level (`role` or `group`)

### Pod Annotations

The annotation name is defined in the pod parser template. For CCE environments:

- **Annotation Name**: `cce.kubectl.kubernetes.io/ascend-1980-configuration`
- **Injected by**: External component (e.g., CCE device plugin)
- **Format**: JSON containing pod ranktable data

Example pod annotation:
```json
{
  "pod_name": "test-ranktable-cce-inference-worker-0",
  "server_id": "192.168.1.10",
  "devices": [
    {"device_id": "0", "device_ip": "10.20.0.2"},
    {"device_id": "1", "device_ip": "10.20.0.3"}
  ]
}
```

**Note**: When using the `pod-ranktable` plugin, the `server_id` field from the annotation is ignored and replaced with the Pod IP address.

## Template Customization

### Creating Custom Templates

You can create custom templates for different inference engines or ranktable formats:

1. **Create Pod Parser Template**: Define how to parse pod annotations
2. **Create Ranktable Template**: Define the final ranktable JSON structure
3. **Reference Parser in Ranktable Template**: Set `pod-parser-template` field

Example custom template:
```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: my-custom-ranktable-template
  namespace: kthena-system
  labels:
    app.kubernetes.io/component: pod-ranktable-template
data:
  inference-engine: "my-engine"
  ranktable-level: "role"
  pod-parser-template: "ascend-pod-ranktable-parser-cce"
  ranktable-template: |
    {
      "custom_field": "value",
      "server_count": "{{ .ServerCount }}",
      ...
    }
  mount-path: "/custom/path"
  filename: "custom-ranktable.json"
```

## Ranktable Refresh on Restart

The ranktable controller automatically handles ranktable refresh when pods, roles, or groups are restarted:

### Pod Restart

When a single pod is restarted (e.g., due to failure or manual deletion):

1. **Controller detects pod restart**: The controller watches for pod creation events
2. **Clears ranktable ConfigMap**: The ranktable ConfigMap for the affected role/group is cleared
3. **New Pod Starts**: The new pod starts with ranktable volume mounted
4. **External component injects annotation**: Device plugin or other component injects new ranktable annotation
5. **Controller regenerates ranktable**: Once all pods have annotations, controller regenerates the ranktable
6. **Application Reads Config**: Application can read the populated ranktable file

### Role Restart

When an entire role is restarted (for role-level ranktables):

1. **All pods in role are deleted**: Controller deletes all pods in the role
2. **Ranktable ConfigMap is cleared**: The role's ranktable ConfigMap is emptied
3. **New pods are created**: Controller creates new pods with volume mounts
4. **Ranktable is regenerated**: After all new pods get annotations, ranktable is regenerated

### Group Restart

When an entire serving group is restarted (for group-level ranktables):

1. **All pods in group are deleted**: Controller deletes all pods in the serving group
2. **Ranktable ConfigMap is cleared**: The group's ranktable ConfigMap is emptied
3. **New pods are created**: Controller creates new pods for all roles in the group
4. **Ranktable is regenerated**: After all new pods get annotations, ranktable is regenerated

### Role Scale Down (Group-Level Ranktables)

When using group-level ranktables and a role is scaled down or deleted:

1. **Pod deletion triggers update**: When pods are deleted, the controller detects the change
2. **Group ranktable is updated**: The group-level ranktable ConfigMap is regenerated to reflect remaining pods
3. **OnRoleDelete hook**: When a role is completely deleted, the OnRoleDelete hook updates the group ranktable
4. **Automatic synchronization**: The ranktable stays in sync with the actual pod count across all roles in the group

**Example scenario**:
- Serving group has 2 roles: `inference-master` (2 pods) and `inference-worker` (2 pods)
- Scale down `inference-worker` from 2 to 1 replica
- Group ranktable is automatically updated to include only 3 pods instead of 4
- Delete `inference-worker` role completely
- Group ranktable is automatically updated to include only 2 pods from `inference-master`

### Key Features

- **Automatic detection**: Controller automatically detects pod/role/group restarts
- **ConfigMap clearing**: Ranktable ConfigMaps are cleared to trigger refresh
- **Consistency**: Rank IDs are regenerated based on new device assignments

## Troubleshooting


### Ranktable Not Generated / Empty

**Symptom**: Ranktable file is empty or application waits indefinitely

**Possible Causes**:
1. Pod ranktable annotation not injected by external component
2. Controller not running or has errors
3. Template parsing errors
4. ConfigMap was cleared due to restart but annotations not yet injected

**Solutions**:
```bash
# Check pod annotations (for CCE)
kubectl get pod <pod-name> -o jsonpath='{.metadata.annotations.cce\.kubectl\.kubernetes\.io/ascend-1980-configuration}'

# Check controller logs
kubectl logs -n kthena-system deployment/kthena-controller-manager

# Check ConfigMap content
kubectl get configmap <ranktable-configmap-name> -o yaml

# Check if ConfigMap is empty (indicates waiting for pod annotations)
kubectl get configmap <ranktable-configmap-name> -o jsonpath='{.data.ranktable\.json}'
```

### Ranktable Format Errors

**Symptom**: Ranktable JSON is invalid or incorrect

**Solutions**:
1. Verify template syntax in ConfigMap
2. Check controller logs for template rendering errors
3. Validate pod annotation format

### Missing ConfigMaps

**Symptom**: Ranktable ConfigMaps not created

**Solutions**:
1. Verify ModelServing has the correct plugin configuration
2. Check if template ConfigMaps exist in `kthena-system` namespace
3. Review controller RBAC permissions

## References

- [Detailed Design Document](../../docs/proposal/group-role-ranktable-for-ascend-detailed-design.md)
- [MindIE Multi-Node Inference](https://www.hiascend.com/document/detail/zh/mindie/22RC1/envdeployment/instg/mindie_instg_0027.html)
- [Go Template Documentation](https://pkg.go.dev/text/template)
