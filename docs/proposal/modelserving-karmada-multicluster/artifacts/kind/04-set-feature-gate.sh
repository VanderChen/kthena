#!/usr/bin/env bash
# Set or clear --feature-gates on karmada-scheduler, karmada-controller-manager and karmada-webhook.
#   04-set-feature-gate.sh MultiplePodTemplatesScheduling=true    # set
#   04-set-feature-gate.sh ""                                     # back to defaults
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/00-env.sh"
gates=${1:-}
for d in karmada-scheduler karmada-controller-manager karmada-webhook; do
  kh -n karmada-system get deploy "$d" -o json \
    | jq --arg g "$gates" '.spec.template.spec.containers[0].command |= (map(select(startswith("--feature-gates=") | not)) + (if $g == "" then [] else ["--feature-gates=" + $g] end))' \
    | kh apply -f - >/dev/null
done
for d in karmada-scheduler karmada-controller-manager karmada-webhook; do
  kh -n karmada-system rollout status deploy/"$d" --timeout=240s >/dev/null
  echo "$d: $(kh -n karmada-system get deploy "$d" -o json | jq -r '[.spec.template.spec.containers[0].command[] | select(startswith("--feature-gates="))] | if length == 0 then "default feature gates" else .[0] end')"
done
