# A Deep Dive into the Kthena Autoscaler

> *Author: Kthena Community*  
> *Published: 2026*  
> *Tags: #Autoscaling #Kubernetes #LLM #CloudNative #Volcano*

---

As Large Language Models (LLMs) become increasingly central to modern AI applications, the infrastructure supporting them must evolve to meet demanding performance, scalability, and cost requirements. While intelligent routing and model orchestration address *where* requests go, a critical question remains: **how many inference instances should be running at any given moment?**

Enter **Kthena Autoscaler** — an optional component of the Kthena system that runs in Kubernetes environments and dynamically adjusts the number of deployed serving instances based on real-time load [[1]]. It maintains healthy business metrics (such as SLO indicators) while optimizing computational resource consumption, ensuring your LLM serving infrastructure is both responsive and cost-efficient.

In this post, we'll take a deep dive into the architecture, algorithms, and practical usage of Kthena Autoscaler, exploring how it enables intelligent, model-aware elastic scaling for production LLM workloads.

---

## 1. Why Autoscaling Matters for LLM Inference

LLM inference workloads exhibit unique characteristics that challenge traditional autoscaling approaches:

| Characteristic | Impact on Scaling |
|---------------|----------------|
| **Bursty Traffic Patterns** | Sudden spikes in user requests require rapid scale-up to maintain latency SLOs |
| **High Resource Consumption** | Each inference instance consumes significant GPU/NPU resources; over-provisioning is costly |
| **Prefill/Decode Asymmetry** | PD-disaggregated deployments need independent scaling for prefill and decode roles [[34]] |
| **Cold Start Overhead** | Loading large models into memory takes seconds to minutes; scaling decisions must account for this latency |
| **Heterogeneous Hardware** | Different instance types (GPU/NPU, different generations) offer varying performance/cost tradeoffs |

Traditional Kubernetes Horizontal Pod Autoscaler (HPA) or KEDA, while powerful, lack the model-awareness needed to make intelligent scaling decisions for LLM workloads. Kthena Autoscaler bridges this gap by:

- Collecting **inference-specific metrics** (queue length, KV cache utilization, TTFT/TPOT latency)
- Supporting **role-level scaling** for PD-disaggregated architectures
- Implementing **cost-aware optimization** across heterogeneous instance types
- Providing **panic mode** for rapid response to traffic spikes

---

## 2. Architecture Overview

Kthena Autoscaler follows a controller pattern consistent with Kubernetes design principles. The high-level architecture is shown below:

```
┌─────────────────────────────────────────┐
│         Kthena Autoscaler               │
├─────────────────────────────────────────┤
│  ┌─────────────┐  ┌─────────────────┐   │
│  │   Metrics   │  │   Scaling       │   │
│  │   Collector │  │   Controller    │   │
│  └──────┬──────┘  └────────┬────────┘   │
│         │                  │             │
│         ▼                  ▼             │
│  ┌─────────────┐  ┌─────────────────┐   │
│  │ Inference   │  │ ModelServing    │   │
│  │ Pods        │  │ CR / Deployment │   │
│  └─────────────┘  └─────────────────┘   │
└─────────────────────────────────────────┘
```

### Core Components

1. **Metrics Collector**: Periodically scrapes runtime metrics from inference engine endpoints (`/metrics`), including:
   - `kthena:num_requests_waiting`: Queue length
   - `kthena:kv_cache_usage_perc`: KV cache utilization
   - `kthena:time_to_first_token`: TTFT latency
   - `kthena:time_per_output_token`: TPOT latency

2. **Scaling Controller**: Implements the core autoscaling logic:
   - Compares observed metrics against configured `targetValue`
   - Applies tolerance thresholds to prevent thrashing
   - Calculates desired replica count using stabilization windows
   - Updates target resources via Kubernetes API

3. **Algorithm Engine**: Supports two scaling modes:
   - **Homogeneous Instances Autoscale**: Single instance type, similar to KPA behavior with Stable/Panic modes
   - **Heterogeneous Instances Autoscale**: Multi-instance optimization using greedy algorithm with cost-aware scheduling

---

## 3. Homogeneous Scaling: Stable and Panic Modes

For deployments with a single instance type (e.g., all vLLM pods on A100 GPUs), Kthena Autoscaler implements a dual-mode strategy inspired by Kubernetes Pod Autoscaler (KPA):

### Stable Mode
- Uses a **stabilization window** (e.g., 1 minute) to observe sustained load before scaling
- Prevents overreaction to transient spikes
- Evaluates metrics at configurable `period` intervals (e.g., 30s)

### Panic Mode
- Triggered when metrics exceed `panicThresholdPercent` (e.g., 150% of target)
- Bypasses stabilization window for rapid scale-up
- Maintains panic state for `panicModeHold` duration (e.g., 5 minutes) to handle sustained spikes

```yaml
# AutoscalingPolicy example
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: llm-scaling-policy
spec:
  metrics:
  - metricName: kthena:num_requests_waiting
    targetValue: 10.0  # Target: ≤10 waiting requests per instance
  tolerancePercent: 10  # ±10% tolerance band
  behavior:
    scaleUp:
      panicPolicy:
        panicThresholdPercent: 150  # Enter panic at 15 requests
        panicModeHold: 5m
      stablePolicy:
        stabilizationWindow: 1m
        period: 30s
    scaleDown:
      stabilizationWindow: 5m  # Longer window for scale-down stability
      period: 1m
```

### Role-Level Scaling for PD-Disaggregation

A key differentiator is support for **role-level scaling** within a single `ModelServing` CRD. In PD-disaggregated deployments, prefill and decode roles have different resource profiles and scaling requirements [[34]]:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: pd-role-binding
spec:
  policyRef:
    name: llm-scaling-policy
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: deepseek-serving
      subTargets:
        kind: Role
        name: decode  # Scale only the decode role
    minReplicas: 2
    maxReplicas: 8
```

This enables fine-grained optimization: scale up decode replicas for long-output scenarios while keeping prefill replicas stable.

---

## 4. Heterogeneous Scaling: Cost-Aware Optimization

For advanced deployments with multiple instance types (e.g., mixing A100 and H100 GPUs, or GPU/NPU heterogeneous clusters), Kthena Autoscaler implements a sophisticated **cost-aware optimization algorithm**.

### The Problem

Given:
- N instance types with different costs (`c_i`) and performance characteristics
- A predicted total instance demand `K` from metric-based forecasting
- Min/max replica constraints per instance type

Find the optimal combination of instances that:
1. Meets the total demand `K`
2. Minimizes total cost
3. Respects cold-start overhead by reusing already-running instances

### The Algorithm: Greedy with Doubling Strategy

Kthena employs a **greedy algorithm with exponential batching** [[40]][[41]]:

1. **Calculate Capacity**: For each instance type `i`, compute available capacity `C_i = maxReplicas_i - minReplicas_i`

2. **Generate Batches**: Using `costExpansionRate` (default 200%), divide capacity into exponentially-sized batches:
   ```
   Batch sizes: P^0, P^1, P^2, ... where P = costExpansionRate
   ```

3. **Cost-Sort Batches**: Mix batches from all instance types and sort by ascending cost-per-instance

4. **Build Scaling Sequence**: Expand sorted batches into a linear sequence `seq` of instance additions

5. **Select Top-K**: When prediction requires `K` additional instances, take the first `K` entries from `seq`

The mathematical formulation:
```
seq = sorted( ⋃_{i=1}^{N} { P^k · c_i | k ∈ (0,1,...,M_i) } ∪ { (C_i - Σ_{k=0}^{M_i} P^k) · c_i } )
```

Where:
- `N`: Number of instance types
- `P`: costExpansionRate
- `c_i`: Cost of instance type `i`
- `M_i`: Number of explicit power terms for type `i`
- `C_i`: Total capacity for type `i`

### Why This Works

- **Cost Efficiency**: Lower-cost instances are prioritized when performance permits
- **Cold-Start Reduction**: The sequence preserves instance ordering across scaling cycles, maximizing reuse of already-running pods
- **Flexibility**: `costExpansionRate` allows tuning the tradeoff between cost optimization and selection flexibility

### Configuration Example

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: heterogeneous-binding
spec:
  policyRef:
    name: llm-scaling-policy
  heterogeneousTarget:
    costExpansionRatePercent: 20  # Allow 20% cost premium for better performance
    params:
    - target:
        targetRef:
          kind: ModelServing
          name: h100-serving  # High-performance, high-cost
      minReplicas: 1
      maxReplicas: 4
      cost: 100  # Relative cost unit
    - target:
        targetRef:
          kind: ModelServing
          name: a100-serving  # Balanced performance/cost
      minReplicas: 2
      maxReplicas: 8
      cost: 60
    - target:
        targetRef:
          kind: ModelServing
          name: ascend-serving  # Cost-optimized NPU
      minReplicas: 0
      maxReplicas: 10
      cost: 30
```

---

## 5. Integration with ModelServing and Volcano

Kthena Autoscaler doesn't operate in isolation. It integrates tightly with:

### ModelServing CRD
- Autoscaler watches `ModelServing` resources and updates `spec.replicas` or role-level `replicas` fields
- Supports the three-tier architecture (`ModelServing → ServingGroup → Role`) for complex deployment patterns [[27]]

### Volcano Gang Scheduling
- When scaling up, new pods are created with `PodGroup` labels
- Volcano scheduler ensures all-or-nothing scheduling for gang-aware deployments
- Prevents partial scaling that could break inference workflows

### Metrics Pipeline
```
Inference Pod (vLLM/SGLang/TGI)
         │
         ▼
   /metrics endpoint
         │
         ▼
Autoscaler Metrics Collector
         │
         ▼
   Prometheus-compatible format
         │
         ▼
Scaling Decision Engine
```

Custom metric endpoints can be configured via `metricEndpoint` in the binding:
```yaml
metricEndpoint:
  uri: "/custom-metrics"
  port: 9090
  labelSelector:
    matchLabels:
      inference-role: prefill
```

---

## 6. Practical Usage: End-to-End Example

Let's walk through deploying autoscaling for a PD-disaggregated LLM service:

### Step 1: Define the ModelServing
```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: llm-pd-serving
spec:
  replicas: 2
  template:
    roles:
    - name: prefill
      replicas: 3
      # ... pod template
    - name: decode
      replicas: 4
      # ... pod template
```

### Step 2: Create AutoscalingPolicy
```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicy
metadata:
  name: pd-scaling-policy
spec:
  metrics:
  - metricName: kthena:num_requests_waiting
    targetValue: 5.0
  tolerancePercent: 15
  behavior:
    scaleUp:
      panicPolicy:
        panicThresholdPercent: 200
        panicModeHold: 3m
      stablePolicy:
        stabilizationWindow: 45s
        period: 15s
    scaleDown:
      stabilizationWindow: 3m
      period: 30s
```

### Step 3: Bind Policy to Roles
```yaml
# Prefill role: conservative scaling (compute-intensive)
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: prefill-binding
spec:
  policyRef:
    name: pd-scaling-policy
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: llm-pd-serving
      subTargets:
        kind: Role
        name: prefill
    minReplicas: 2
    maxReplicas: 6
---
# Decode role: aggressive scaling (latency-sensitive)
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: AutoscalingPolicyBinding
metadata:
  name: decode-binding
spec:
  policyRef:
    name: pd-scaling-policy
  homogeneousTarget:
    target:
      targetRef:
        kind: ModelServing
        name: llm-pd-serving
      subTargets:
        kind: Role
        name: decode
    minReplicas: 3
    maxReplicas: 12
```

### Step 4: Monitor and Verify
```bash
# Check binding status
kubectl describe autoscalingpolicybindings pd-scaling-binding

# Watch replica changes
watch -n 5 'kubectl get modelserving llm-pd-serving -o jsonpath="{.spec.template.roles[*].replicas}"'

# Inspect autoscaler logs
kubectl logs -n kthena-system -l app=kthena-autoscaler -f
```

---

## 7. Best Practices and Troubleshooting

### Configuration Guidelines

1. **Start Conservative**: Begin with wider tolerance bands (15-20%) and longer stabilization windows
2. **Monitor Cold Starts**: Account for model loading time in panic mode duration
3. **Role-Specific Targets**: Set different `targetValue` for prefill vs decode based on their bottleneck characteristics
4. **Cost Calibration**: For heterogeneous scaling, validate `cost` values against actual cloud pricing or TCO

### Common Pitfalls

| Issue | Symptom | Solution |
|-------|---------|----------|
| Metric collection failure | No scaling events, logs show "connection refused" | Verify `metricEndpoint` port/URI matches inference engine config |
| Thrashing (rapid scale up/down) | Replica count oscillates frequently | Increase `tolerancePercent` or `stabilizationWindow` |
| Panic mode never triggers | Traffic spikes cause latency SLO violations | Lower `panicThresholdPercent` or increase `panicModeHold` |
| Heterogeneous scaling favors expensive instances | Cost unexpectedly high | Reduce `costExpansionRatePercent` or recalibrate `cost` values |

### Observability

Kthena Autoscaler exposes metrics at `/metrics`:
- `kthena_autoscaler_desired_replicas`: Target replica count after scaling decision
- `kthena_autoscaler_current_replicas`: Actual observed replica count
- `kthena_autoscaler_scaling_events_total`: Counter of scale-up/scale-down actions
- `kthena_autoscaler_metric_collection_errors`: Failed metric scrapes

Integrate with Prometheus/Grafana for dashboards and alerting on scaling anomalies.

---

## 8. Future Directions

The Kthena community is actively enhancing Autoscaler capabilities:

- **Predictive Scaling**: Integrating time-series forecasting to anticipate traffic patterns
- **Multi-Metric Optimization**: Supporting composite scaling decisions across multiple metrics (e.g., queue length + latency)
- **Cross-Cluster Scaling**: Extending heterogeneous optimization to multi-cluster deployments
- **SLO-Driven Policies**: Allowing direct specification of latency/throughput SLOs instead of metric thresholds

We welcome contributions from the community! Whether you're interested in algorithm improvements, new metric integrations, or documentation enhancements, join us on GitHub: [volcano-sh/kthena](https://github.com/volcano-sh/kthena).

---

## Conclusion

Kthena Autoscaler represents a significant step forward in cloud-native LLM infrastructure. By combining model-aware metrics, role-level granularity, and cost-aware optimization, it enables production teams to:

✅ Maintain latency SLOs during traffic spikes  
✅ Reduce infrastructure costs through intelligent resource allocation  
✅ Support complex PD-disaggregated and heterogeneous deployments  
✅ Operate with Kubernetes-native patterns and observability  

As LLM workloads continue to evolve, the need for intelligent, adaptive autoscaling will only grow. Kthena Autoscaler provides the foundation for building resilient, efficient, and cost-effective inference platforms at scale.

*Ready to get started?*  
🔗 [Kthena Documentation](https://kthena.volcano.sh)  
🔗 [Autoscaler User Guide](https://kthena.volcano.sh/docs/user-guide/autoscaler)  
🔗 [GitHub Repository](https://github.com/volcano-sh/kthena)

---

> *This post is part of the Kthena technical blog series. For more deep dives into Kthena Router, ModelServing, and ModelBooster, visit [kthena.volcano.sh/blog](https://kthena.volcano.sh/blog).*