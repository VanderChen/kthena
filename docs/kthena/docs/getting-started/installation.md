---
sidebar_position: 1
---

# Installation

This guide will help you install Kthena on your Kubernetes cluster.

## Prerequisites

Before installing Kthena, ensure you have the following:

### Required Prerequisites

- **Kubernetes cluster** (version 1.28 or later)
- **kubectl** configured to access your cluster
- **Helm** (version 3.0 or later)
- Cluster admin permissions

### Optional Prerequisites

- **[cert-manager](https://cert-manager.io/docs/installation/)** - Required only if using the `cert-manager` certificate management mode (see [Certificate Management](../general/cert-manager.md) for details)

## Installation Methods

### Method 1: Helm Installation (Recommended)

Kthena Helm charts are published to the GitHub Container Registry (GHCR).

1. **Install Kthena:**

   ```bash
   helm install kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 --namespace kthena-system --create-namespace
   ```

   Chart versions on GHCR have a `v` prefix (for example, `--version v1.1.0`), and `--version` is required.

:::note
Starting with v1.2.0, charts will be published to the [volcano-sh Helm repository](https://github.com/volcano-sh/helm-charts) in addition to GHCR.
:::

### Method 2: Manual Installation with GitHub Release Manifests

Kthena provides all necessary components in a single manifest file for easy installation from GitHub Releases.

1. **Apply the Kthena manifest:**

   ```bash
   kubectl apply --server-side -f https://github.com/volcano-sh/kthena/releases/latest/download/kthena-install.yaml
   ```

   To install a specific version, replace `latest` with the desired release tag (e.g., `v1.2.3`):

   ```bash
   kubectl apply --server-side -f https://github.com/volcano-sh/kthena/releases/download/vX.Y.Z/kthena-install.yaml
   ```

### Method 3: Helm Installation from GitHub Release Package

You can also download the Helm chart package from [GitHub releases](https://github.com/volcano-sh/kthena/releases) and install it locally.

1. **Download the Helm chart package:**

   For the latest version:
   ```bash
   curl -L -o kthena.tgz https://github.com/volcano-sh/kthena/releases/latest/download/kthena.tgz
   ```

   For a specific version (replace `vX.Y.Z` with the desired release tag):
   ```bash
   curl -L -o kthena.tgz https://github.com/volcano-sh/kthena/releases/download/vX.Y.Z/kthena.tgz
   ```

2. **Install from the downloaded package:**

   ```bash
   helm install kthena kthena.tgz --namespace kthena-system --create-namespace
   ```

## Configuration Options

### Component-Scoped Installation

Kthena is modular: the **workload** controllers and the **networking** router are separate Helm subcharts with separate CRD groups, and neither depends on the other at runtime. Install only what you need — a component-scoped install brings in just that component's CRDs, RBAC, and webhooks.

| Subchart     | Component                                                  | CRDs installed                                       |
| :----------- | :--------------------------------------------------------- | :--------------------------------------------------- |
| `workload`   | `kthena-controller-manager` (model lifecycle, admission) and `kthena-autoscaler` (autoscaling) | `ModelServing`, `ModelBooster`, `AutoscalingPolicy`  |
| `networking` | `kthena-router` (inference traffic routing)                | `ModelRoute`, `ModelServer`, `ExternalModelProvider` |

**Workload controllers only** — manage model workloads with Kthena while keeping your existing gateway for traffic:

```bash
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 \
  --namespace kthena-system --create-namespace \
  --set networking.enabled=false
```

**Router only** — use Kthena Router as a standalone, model-aware LLM gateway in front of pods managed by Deployments, StatefulSets, another operator, or external providers:

```bash
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 \
  --namespace kthena-system --create-namespace \
  --set workload.enabled=false
```

:::note
The deprecated `ModelBooster` API requires **both** subcharts. It remains available in v1.1, with removal no earlier than v1.5. For new deployments, use `ModelServing`, `ModelServer`, and `ModelRoute` directly; with a component-scoped install, use that component's CRDs. See the [deprecation details](../user-guide/model-deployment.md#modelbooster-deprecation).
:::

You can enable the other component later with `helm upgrade --set <subchart>.enabled=true`. Note that Helm does not install files under a chart's `crds/` directory during an upgrade, so apply the newly enabled component's CRDs yourself first:

```bash
# 1. Fetch and unpack the chart. Replace <chart-version> with the chart version
#    you have installed — `helm list -n kthena-system` shows it.
helm pull oci://ghcr.io/volcano-sh/charts/kthena --version <chart-version> --untar

# 2. Apply the CRDs of the subchart you are enabling: use `networking` for the
#    router, or `workload` for the controllers.
kubectl apply --server-side -f kthena/charts/<subchart>/crds/

# 3. Enable the subchart. This example turns on the router; use
#    `--set workload.enabled=true` to turn on the controllers instead.
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 \
  --namespace kthena-system --reuse-values \
  --set networking.enabled=true
```

### Helm Values

You can customize the installation by providing values:

```bash
helm install kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 \
  --namespace kthena-system \
  --create-namespace \
  --set workload.controllerManager.replicas=2 \
  --set networking.kthenaRouter.tls.enabled=true
```

### Common Configuration Parameters

| Parameter                             | Description                                                    | Default |
| :------------------------------------ | :------------------------------------------------------------- | :------ |
| `workload.enabled`                    | Install the workload controllers and their CRDs                | `true`  |
| `networking.enabled`                  | Install the Kthena Router and its CRDs                         | `true`  |
| `workload.controllerManager.replicas` | Number of controller manager replicas                          | `1`     |
| `networking.kthenaRouter.replicas`    | Number of router replicas                                      | `1`     |
| `networking.kthenaRouter.tls.enabled` | Enable TLS for the router                                      | `false` |
| `global.certManagementMode`           | Certificate management mode (`auto`, `cert-manager`, `manual`) | `auto`  |

### Full Values Reference

For a complete list of all configurable Helm values, see the [Helm Chart Values Reference](../reference/helm-chart-values.md).

## Upgrade CRDs before Helm

Helm installs CRDs from a chart during the first installation, but `helm upgrade` does not update them. Apply the CRDs from the target Kthena version before upgrading the release:

```bash
helm show crds oci://ghcr.io/volcano-sh/charts/kthena \
  --version vX.Y.Z \
  | kubectl apply --server-side -f -

helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena \
  --version vX.Y.Z \
  --namespace kthena-system
```

This order is required for releases that add a CRD, including the release that introduces `ExternalModelProvider`. If the Router starts before that CRD exists, its informer cannot finish its initial sync and the Router does not begin serving requests.

## Autoscaler Deployment

The autoscaler is a component of its own: the `kthena-autoscaler` binary and image, deployed next to `kthena-controller-manager` under a ServiceAccount that can read `AutoscalingPolicy`, `ModelServing` and pods, patch `ModelServing`, and update `AutoscalingPolicy` status. A restart or upgrade of the Controller Manager does not restart the autoscaler, and a failing autoscaler does not interrupt admission. The autoscaler still needs the Controller Manager's webhook to be up when it writes a new replica count.

Settings under `workload.autoscaler` that are left empty are taken from `workload.controllerManager`: resources, tolerations, sync period, API client limits, the image tag and pull policy, and the image repository with the image name replaced by `kthena-autoscaler`. `workload.autoscaler.resource` replaces the Controller Manager's resources as a whole. Of the arguments in `workload.controllerManager.image.args`, the chart reads `--controllers`, `--leader-elect` and `--autoscaling-sync-period-seconds` to configure the autoscaler the way the Controller Manager was configured; nothing else is passed on. Use `workload.autoscaler.args` for other arguments.

The autoscaler pod is ready once it has read its resources from the API server. With leader election, a standby replica is ready once it has read the Lease and found the leader holding it; it reads the other resources when it becomes the leader. Without leader election there is one replica and the Deployment uses the `Recreate` strategy, so that an upgrade of the autoscaler does not run the old and the new one side by side: the old pod stops before the new one starts, and nothing scales while the new image is pulled. With leader election (`workload.autoscaler.leaderElect`, or `--leader-elect` in the Controller Manager's arguments) the Deployment rolls, and the next leader starts when the previous lease expires, about 15 to 20 seconds later. An instance that loses its lease restarts.

### Upgrading from a release where the Controller Manager ran the autoscaler

Upgrade with the chart of the new release, as described in [Upgrade CRDs before Helm](#upgrade-crds-before-helm). The chart and both images then come from the same release, which the upgrade relies on: `kthena-controller-manager` no longer contains the autoscaler, and `kthena-autoscaler` of the same release is what scales.

Before the upgrade:

- A registry mirror needs the `kthena-autoscaler` image of the new release next to `kthena-controller-manager`. If the mirror uses other image names, set `workload.autoscaler.image.repository`; the chart does not render without it. If the Controller Manager image is pinned by digest, pin the autoscaler with `workload.autoscaler.image.tag`, and set `workload.autoscaler.image.repository` as well when the digest is part of the repository value.
- The autoscaler pod has its own identity: ServiceAccount `kthena-autoscaler`, label `app.kubernetes.io/component=kthena-autoscaler`, and `kthena-autoscaler` as the name its API requests carry. Update NetworkPolicies, log queries, alerts and audit filters that select the Controller Manager for autoscaling. `imagePullSecrets` added by hand to the `kthena-controller-manager` ServiceAccount are not copied to the new one.
- A `--controllers` argument in `workload.controllerManager.image.args` has to be written without quotes and backslashes; the chart does not render otherwise, because it could not tell whether the release selects the autoscaler.
- The release runs one more pod. It requests the same resources as the Controller Manager unless `workload.autoscaler.resource` is set, which matters under a ResourceQuota.
- The new autoscaler starts without scaling history, as the built-in one did after every restart, so a scale-down that a stabilization window was holding back can happen right away. Upgrade outside such a window, or raise `minReplicas` of the affected policies for the duration.

After `helm upgrade`, check that both Deployments have rolled out:

```bash
kubectl rollout status deployment/kthena-autoscaler -n kthena-system --timeout=5m
kubectl rollout status deployment/kthena-controller-manager -n kthena-system --timeout=5m
```

`helm upgrade` reports success once the objects are applied. An autoscaler that cannot pull its image, is not allowed to read its resources (or, as a standby, the Lease), or cannot reach the API server does not become ready, and once the previous Controller Manager pod is gone nothing else scales. If the rollout does not finish, fix the cause, or return to the previous release with `helm rollback kthena -n kthena-system`. `helm upgrade --atomic` waits and rolls back in one step, but it also waits for the Router's `LoadBalancer` Service to get an address, which does not happen on a cluster without a load balancer.

What to expect during the upgrade:

- The built-in autoscaler of the previous Controller Manager pod runs until that pod exits, and the new autoscaler runs from the moment its pod has started. Depending on which of the two new images is pulled first, both scale for a while, or neither does. With the images already on the node, the two overlapped for about ten seconds in a test. Two Controller Manager pods have overlapped in the same way on every upgrade so far.
- A release whose Controller Manager uses `--leader-elect` gets an autoscaler that elects its leader on its own Lease, `lease.kthena.autoscaler`, with as many replicas as the Controller Manager. A leader change of the Controller Manager no longer restarts the autoscaler. During the upgrade the built-in autoscaler of whichever previous Controller Manager pod holds the Controller Manager's lease runs next to the new one, until the last of those pods is replaced: about twenty seconds in the same test with two replicas.
- A release with several Controller Manager replicas and no leader election ran one autoscaler per replica. It now runs one.

To have a period without autoscaling instead of a period with two autoscalers, upgrade in two steps. First stop the built-in autoscaler with the chart version that is installed, then upgrade:

```bash
# Keep the values the release is installed with
helm get values kthena -n kthena-system -o yaml > kthena-values.yaml

# 1. Installed chart version, same values, and the autoscaler taken out of the Controller Manager
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena --version <installed version> \
  --namespace kthena-system --reuse-values \
  --set-json 'workload.controllerManager.controllers="*,-autoscaler"'
kubectl rollout status deployment/kthena-controller-manager -n kthena-system

# 2. New chart version with the values saved above
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena --version <new version> \
  --namespace kthena-system --reset-values -f kthena-values.yaml
```

Step 2 must not carry the `controllers` value of step 1 over: with it, the new chart deploys no autoscaler. A `helm upgrade` without value flags carries it over, and so does `--reuse-values`; `--reset-values` with the saved values does not. If the release already sets `workload.controllerManager.controllers`, keep that selection in step 1 and only remove the autoscaler from it. A release that selects only the autoscaler cannot be upgraded this way: without the autoscaler its selection is empty, which enables every controller.

`workload.controllerManager.controllers` still decides whether the release has an autoscaler, as it did when the autoscaler was one of the Controller Manager's controllers:

| Existing configuration | After the upgrade |
| :--- | :--- |
| `controllers` empty, or a selection that includes the autoscaler | The autoscaler is deployed. |
| `controllers` leaves the autoscaler out, for example `*,-autoscaler` or `modelserving,modelbooster` | No autoscaler is deployed. |
| `controllers` selects only the autoscaler | The autoscaler is deployed. The Controller Manager runs no controller and only serves the webhooks. |
| `workload.controllerManager.replicas: 0` | The autoscaler Deployment has no replica either, unless `workload.autoscaler.replicas` is set. |

`controllers: "-autoscaler"` without `*` has never disabled a controller: the binary enables all of them when the list selects none. A release with that value keeps its autoscaler. Use `*,-autoscaler` or `workload.autoscaler.enabled=false` to turn it off.

The Controller Manager still accepts the name `autoscaler` in `--controllers`, where it no longer starts anything (a list that names only `autoscaler` still selects no other controller), and the flag `--autoscaling-sync-period-seconds`, which has no effect. Both are deprecated and will be removed in a later release. The chart passes the sync period to the autoscaler, whether it was set through `workload.controllerManager.autoscalingSyncPeriodSeconds` or through `workload.controllerManager.image.args`.

### Upgrades that do not move the chart and both images together

| What was done | Result |
| :--- | :--- |
| New chart with the image tags of the old release, for example `helm upgrade --reuse-values` | The old Controller Manager keeps running its built-in autoscaler. A sync period set through `workload.controllerManager.autoscalingSyncPeriodSeconds` no longer reaches it, because the new chart passes that value to the autoscaler only. The `kthena-autoscaler` Deployment waits for an image that does not exist. Pass the new tags. |
| New chart with the same image tag as before, for example `latest` | The Controller Manager Deployment does not change, so its pod is not replaced and keeps running the built-in autoscaler of the old image next to the new one. Restart it with `kubectl rollout restart deployment/kthena-controller-manager -n kthena-system`, and make sure the new pod gets the new image: with the default pull policy `IfNotPresent`, a node that already has an image under that tag keeps using it, so set `workload.controllerManager.image.pullPolicy=Always` or use a tag that names the release. |
| New chart, Controller Manager tag pinned to the old release, `workload.autoscaler.image.tag` set to the new one | Two autoscalers scale the same targets. Keep both images on the same release. |
| Old chart, or a manifest maintained by hand, with the new Controller Manager image | Nothing scales: the Controller Manager has no autoscaler and nothing deploys one. The Controller Manager logs `Controller <autoscaler> runs as kthena-autoscaler, not in this process`. |
| `kubectl apply` of the new `kthena-install.yaml` | Same as the chart upgrade with default values: the manifest contains the autoscaler, with one replica and no leader election. |

### Going back

`helm rollback` removes the `kthena-autoscaler` objects and brings back the Controller Manager that runs the autoscaler itself. With leader election, the Lease `lease.kthena.autoscaler` stays behind and can be deleted. Two other ways back leave the objects in place, next to a Controller Manager that runs its own autoscaler:

- `kubectl apply` of an older manifest does not prune them.
- After an upgrade that Helm recorded as failed, a `helm upgrade` to the older chart compares against the last deployed revision, which does not contain them. Use `helm rollback` instead.

A `kthena-autoscaler` Deployment that is left behind scales the same targets as the built-in autoscaler. If it was left because its image could not be pulled, it starts doing so when the image becomes available. Check with `kubectl get deployment kthena-autoscaler -n kthena-system`, and delete the Deployment, ServiceAccount, ClusterRole and ClusterRoleBinding named `kthena-autoscaler` (and the Role and RoleBinding, with leader election) if they are left.

## Verification

After installation, verify that all components are running:

```bash
# Check all pods are running
kubectl get pods -n kthena-system

# Check CRDs are installed (CRDs are included in the main manifest/chart)
kubectl get crd | grep kthena

# Check services
kubectl get svc -n kthena-system
```

## Optional Components

### Gang Scheduling

Kthena leverages **Volcano** (a high-performance batch system for Kubernetes) to provide gang scheduling capabilities.

If you need gang scheduling capabilities, you can install Volcano by following the official installation guide of [Volcano](https://volcano.sh/en/docs/installation/).

### Kthena CLI

The Kthena CLI provides kubectl‑style commands for managing AI inference workloads on Kubernetes, featuring quick deployment via curated templates and optional integration with kubectl‑ai for natural‑language command generation.

#### Installation

Download the latest binary from the [releases page](https://github.com/volcano-sh/kthena/releases) or build from source:

```bash
go install github.com/volcano-sh/kthena/cli/kthena@latest
```

Please see [Kthena CLI Documentation](../reference/kthena-cli.md) for more details.
