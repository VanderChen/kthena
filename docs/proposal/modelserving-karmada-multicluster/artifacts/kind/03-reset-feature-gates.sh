#!/usr/bin/env bash
# Hack/local-up-karmada.sh starts Karmada with --feature-gates=AllAlpha=true,AllBeta=true.
# The characterization targets the default configuration, so drop that flag from every
# Karmada component (host cluster) and from karmada-agent (pull-mode member).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/00-env.sh"

strip() { # kubectl-wrapper... namespace deployment
  local kc=("${@:1:$#-2}") ns="${@: -2:1}" d="${@: -1}"
  local before
  before=$("${kc[@]}" -n "$ns" get deploy "$d" -o json | jq -r '.spec.template.spec.containers[0].command[] | select(startswith("--feature-gates="))')
  [[ -z "$before" ]] && { echo "$d: no --feature-gates flag"; return; }
  "${kc[@]}" -n "$ns" get deploy "$d" -o json \
    | jq '.spec.template.spec.containers[0].command |= map(select(startswith("--feature-gates=") | not))' \
    | "${kc[@]}" apply -f - >/dev/null
  echo "$d: removed $before"
}

for d in $(kh -n karmada-system get deploy -o name | cut -d/ -f2); do
  strip kubectl --kubeconfig "$MAIN_KUBECONFIG" --context "$HOST_CLUSTER_NAME" karmada-system "$d"
done
strip kubectl --kubeconfig "$MEMBER_CLUSTER_KUBECONFIG" --context "$PULL_MODE_CLUSTER_NAME" karmada-system karmada-agent

for d in $(kh -n karmada-system get deploy -o name | cut -d/ -f2); do
  kh -n karmada-system rollout status deploy/"$d" --timeout=240s >/dev/null
done
km "$PULL_MODE_CLUSTER_NAME" -n karmada-system rollout status deploy/karmada-agent --timeout=240s >/dev/null
echo "all components rolled out with default feature gates"
