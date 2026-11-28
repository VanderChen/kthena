#!/usr/bin/env bash
# Helpers for the characterization scenarios. Source after 00-env.sh.

# snapshot <modelserving-name> [members...]: one compact view of control plane and members.
snapshot() {
  local name=$1; shift
  local members=("$@"); [[ ${#members[@]} -eq 0 ]] && members=(k014-m1 k014-m2)
  local rb=${name}-modelserving
  echo "--- template (karmada-apiserver) $(date -u +%T)"
  kk get modelserving "$name" -o json 2>/dev/null | jq -c '{specReplicas:.spec.replicas, generation:.metadata.generation, status:(.status // {} | del(.conditions))}'
  echo "--- ResourceBinding $rb"
  kk get rb "$rb" -o json 2>/dev/null | jq -c '{replicas:.spec.replicas, replicaRequirements:.spec.replicaRequirements, components:(.spec.components // [] | length), clusters:.spec.clusters}'
  kk get rb "$rb" -o json 2>/dev/null | jq -c '.status.conditions[]? | {type,status,reason,message:(.message|.[0:200])}'
  kk get rb "$rb" -o json 2>/dev/null | jq -c '.status.aggregatedStatus[]? | {cluster:.clusterName, applied, health, appliedMessage:((.appliedMessage // "")|.[0:220]), status:(.status // {} | del(.conditions))}'
  local c
  for c in "${members[@]}"; do
    echo "--- member $c"
    km "$c" get modelserving "$name" -o json 2>/dev/null | jq -c '{specReplicas:.spec.replicas, generation:.metadata.generation, status:(.status // {} | del(.conditions))}' || echo "(no ModelServing)"
    km "$c" get pod -l "modelserving.volcano.sh/name=$name" -o json 2>/dev/null \
      | jq -r '[.items[] | {g:.metadata.labels["modelserving.volcano.sh/group-name"], s:(if .spec.nodeName then .status.phase else ((.status.conditions // [])[] | select(.type=="PodScheduled") | .reason) // "Pending" end)}] | group_by(.g)[] | "  group \(.[0].g): " + ([.[].s] | group_by(.) | map("\(.[0])x\(length)") | join(" "))'
  done
}

# wait_for <seconds> <description> <command...>: poll until the command succeeds.
wait_for() {
  local timeout=$1 what=$2; shift 2
  local i
  for ((i = 0; i < timeout; i += 3)); do
    if "$@" >/dev/null 2>&1; then return 0; fi
    sleep 3
  done
  echo "TIMEOUT waiting for: $what"; return 1
}

sched_log() { kh -n karmada-system logs -l app=karmada-scheduler --since="${1:-60s}" --tail=-1 --prefix=false 2>/dev/null; }
