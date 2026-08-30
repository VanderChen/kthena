# Ranktable for Ascend Resources

This directory contains examples for generating role/group level ranktables for Ascend 910 resources.

## Overview

The ranktable feature supports vLLM-Ascend and MindIE inference engines' HCCL communication needs by automatically generating ranktable ConfigMaps based on pod annotations.

## Components

### 1. Pod Ranktable Parser Template
invo
**File**: `pod-ranktable-parser-standard.yaml`

This ConfigMap defines how to parse the pod ranktable annotation (`ascend.com/ranktable`) into structured data.

```bash
kubectl apply -f pod-ranktable-parser-standard.yaml
```

### 2. Ranktable Templates

#### MindIE Role-Level Template

**File**: `ranktable-template-mindie-role.yaml`

Generates ranktable at the role level for MindIE inference engine.

```bash
kubectl apply -f ranktable-template-mindie-role.yaml
```

#### vLLM-Ascend Group-Level Template

**File**: `ranktable-template-vllm-group.yaml`

Generates ranktable at the group level for vLLM-Ascend inference engine.

```bash
kubectl apply -f ranktable-template-vllm-group.yaml
```

### 3. ModelServing Example

**File**: `modelserving-with-ranktable.yaml`

Example ModelServing CR that uses ranktable generation.

```bash
kubectl apply -f modelserving-with-ranktable.yaml
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

# Deploy pod parser template
kubectl apply -f pod-ranktable-parser-standard.yaml

# Deploy ranktable template (choose one based on your inference engine)
kubectl apply -f ranktable-template-mindie-role.yaml
# OR
kubectl apply -f ranktable-template-vllm-group.yaml
```

### Step 2: Create ModelServing

```bash
kubectl apply -f modelserving-with-ranktable.yaml
```

### Step 3: Verify

```bash
# Check if ranktable ConfigMaps are created
kubectl get configmap -l app.kubernetes.io/component=ranktable

# Check pod status
kubectl get pods -l workload.kthena.io/group-name=qwen-inference

# View ranktable content
kubectl get configmap qwen-inference-worker-ranktable -o jsonpath='{.data.ranktable\.json}' | jq .
```

## Configuration

### ModelServing Plugins

To enable ranktable generation, add the `ranktable` plugin to your ModelServing spec:

```yaml
spec:
  plugins:
    - name: ranktable
      type: BuiltIn
      config:
        template: "ranktable-template-name"
```

### ModelServing Annotations

- `kthena.io/ranktable-level`: **(Optional)** Override ranktable level (`role` or `group`)

### Pod Annotations

- `ascend.com/ranktable`: **(Injected by external component)** Pod ranktable data in JSON format

Example pod annotation:
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
data:
  inference-engine: "my-engine"
  ranktable-level: "role"
  pod-parser-template: "ascend-pod-ranktable-parser-standard"
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
# Check pod annotations
kubectl get pod <pod-name> -o jsonpath='{.metadata.annotations.ascend\.com/ranktable}'

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
