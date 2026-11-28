#!/usr/bin/env bash
# Prepare member clusters for the characterization.
#  - fake GPUs: extended resource nvidia.com/gpu patched into node status
#      k014-m1: 2 worker nodes x 2 GPU (fragmented)   k014-m2, k014-m3: 1 node x 8 GPU
#  - kthena workload chart (CRDs, controller manager, validating webhook) from the kthena checkout, image tag dev-014,
#    built beforehand with:
#      docker build -t ghcr.io/volcano-sh/kthena-controller-manager:dev-014 -f docker/Dockerfile.kthena-controller-manager .
#    The networking CRDs are applied separately because the controller manager watches them;
#    kthena-router itself is not installed.
#  - ModelServing CRD on the Karmada API server (CRD only, no controller)
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/00-env.sh"
IMG=ghcr.io/volcano-sh/kthena-controller-manager:dev-014

patch_gpu() { # cluster node count
  km "$1" patch node "$2" --subresource=status --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"$3\"}]" >/dev/null
}

for c in "$MEMBER_CLUSTER_1_NAME" "$MEMBER_CLUSTER_2_NAME" "$PULL_MODE_CLUSTER_NAME"; do
  echo "== $c"
  kind load docker-image "$IMG" --name "$c" >/dev/null 2>&1
  if [[ "$c" == "$MEMBER_CLUSTER_1_NAME" ]]; then
    for n in $(km "$c" get nodes -l '!node-role.kubernetes.io/control-plane' -o name | cut -d/ -f2); do patch_gpu "$c" "$n" 2; done
  else
    for n in $(km "$c" get nodes -o name | cut -d/ -f2); do patch_gpu "$c" "$n" 8; done
  fi
  helm upgrade --install kthena "$KTHENA_REPO/charts/kthena" \
    --kubeconfig "$MEMBER_CLUSTER_KUBECONFIG" --kube-context "$c" \
    --namespace kthena-system --create-namespace \
    --set networking.enabled=false \
    --set workload.controllerManager.image.tag=dev-014 \
    --set workload.controllerManager.image.pullPolicy=IfNotPresent >/dev/null
  km "$c" apply --server-side -f "$KTHENA_REPO/charts/kthena/charts/networking/crds/" >/dev/null
  km "$c" -n kthena-system rollout status deploy/kthena-controller-manager --timeout=180s
  km "$c" get nodes -o custom-columns='NODE:.metadata.name,GPU:.status.allocatable.nvidia\.com/gpu,TAINTS:.spec.taints[*].key'
  km "$c" -n kthena-system get pod -o custom-columns='POD:.metadata.name,IMAGE:.spec.containers[0].image,IMAGEID:.status.containerStatuses[0].imageID'
done

echo "== ModelServing CRD on the Karmada API server"
kk apply --server-side -f "$KTHENA_REPO/charts/kthena/charts/workload/crds/workload.serving.volcano.sh_modelservings.yaml"
