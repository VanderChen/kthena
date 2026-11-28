#!/usr/bin/env bash
# Remove everything the characterization created on this host.
set -uo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/00-env.sh"
kind delete clusters "$HOST_CLUSTER_NAME" "$MEMBER_CLUSTER_1_NAME" "$MEMBER_CLUSTER_2_NAME" "$PULL_MODE_CLUSTER_NAME"
rm -rf "$KUBECONFIG_PATH" /tmp/k014
# images built by hack/local-up-karmada.sh carry this label
ids=$(docker images -q --filter "label=image.karmada.io=May_be_pruned_in_local_up_environment" | sort -u)
[[ -n "$ids" ]] && docker rmi -f $ids >/dev/null
docker image prune -f --filter "label=image.karmada.io=May_be_pruned_in_local_up_environment" >/dev/null
echo "kind clusters left: $(kind get clusters 2>/dev/null | tr '\n' ' ')"
echo "karmada images left: $(docker images --format '{{.Repository}}:{{.Tag}}' | grep -c '^karmada/\|docker.io/karmada/')"
