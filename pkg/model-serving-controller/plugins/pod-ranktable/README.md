# Pod-Ranktable Plugin

## Overview

The `pod-ranktable` plugin is a variant of the `ranktable` plugin that uses **Pod IP addresses** as server IDs in the generated ranktable, instead of using server IDs from pod annotations.

## Key Difference

| Plugin | Server ID Source |
|--------|------------------|
| `ranktable` | `server_id` field from pod annotation |
| `pod-ranktable` | Pod's IP address (`pod.Status.PodIP`) |

## When to Use

Use `pod-ranktable` when:
- You want to use Pod IPs to identify servers in the ranktable
- Your infrastructure relies on Pod IPs for server identification
- You want to ignore or don't have consistent server IDs in pod annotations

Use `ranktable` when:
- Your external component (e.g., device plugin) provides consistent server IDs
- You need to use specific server identifiers from annotations

## Usage

### 1. Apply the Configuration Template

```bash
kubectl apply -f examples/ranktable/pod-ranktable-template-mindie-role-cce.yaml
```

### 2. Configure ModelServing

Add the `pod-ranktable` plugin to your ModelServing CR:

```yaml
apiVersion: workload.kthena.io/v1alpha1
kind: ModelServing
metadata:
  name: my-model-serving
spec:
  plugins:
    - name: pod-ranktable
      type: BuiltIn
      config:
        template: "ascend-pod-ranktable-template-mindie-role-cce"
  template:
    roles:
      - name: worker
        workerReplicas: 2
        # ... other role configuration
```

## How It Works

1. **Pod Creation**: When a pod is created, the plugin checks for the ranktable annotation
2. **Annotation Parsing**: The plugin parses the annotation to extract device information
3. **Server ID Override**: Instead of using the `server_id` from the annotation, the plugin uses `pod.Status.PodIP`
4. **Ranktable Generation**: The ranktable ConfigMap is generated with Pod IPs as server IDs

## Example

### Input (Pod Annotation)

```json
{
  "pod_name": "worker-0",
  "server_id": "ignored-value",
  "devices": [
    {"device_id": "0", "device_ip": "10.20.0.2"},
    {"device_id": "1", "device_ip": "10.20.0.3"}
  ]
}
```

### Output (Ranktable)

```json
{
  "version": "1.0",
  "server_count": "1",
  "server_list": [
    {
      "server_id": "10.244.1.5",  // Pod IP, not "ignored-value"
      "device": [
        {
          "device_id": "0",
          "device_ip": "10.20.0.2",
          "rank_id": "0"
        },
        {
          "device_id": "1",
          "device_ip": "10.20.0.3",
          "rank_id": "1"
        }
      ]
    }
  ],
  "status": "Completed"
}
```

## Implementation Details

### Code Location

- **Plugin**: `pkg/model-serving-controller/plugins/pod-ranktable/plugin.go`
- **Template Manager**: `pkg/model-serving-controller/plugins/pod-ranktable/template.go`
- **Types**: `pkg/model-serving-controller/plugins/pod-ranktable/types.go`
- **Tests**: `pkg/model-serving-controller/plugins/pod-ranktable/template_test.go`

### Key Modifications

1. **ParsePodRanktable Method**: Accepts `podIP` and `podName` parameters
2. **Server ID Override**: After parsing the annotation, `podData.ServerId` is set to the Pod IP
3. **OnPodReady Hook**: Extracts Pod IP from `pod.Status.PodIP` and passes it to the parser

```go
// Get pod IP
podIP := pod.Status.PodIP
if podIP == "" {
    allReady = false
    continue
}

// Parse annotation with Pod IP as Server ID
data, err := p.templateManager.ParsePodRanktable(
    template.PodParserTemplate,
    ann,
    podIP,      // <-- Pod IP used here
    pod.Name,
)
```

## Testing

Run tests:

```bash
go test ./pkg/model-serving-controller/plugins/pod-ranktable/... -v
```

## Limitations

- Pod must have an IP address assigned (checked in `OnPodReady`)
- If Pod IP changes (rare, but possible during rescheduling), the ranktable will be regenerated
