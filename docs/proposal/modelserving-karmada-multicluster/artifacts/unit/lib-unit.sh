#!/usr/bin/env bash
# Helpers for the option-G (unit) scenarios. Source after kind/00-env.sh and kind/lib.sh.

# units [set]: one line per unit: where Karmada put it and what its Pods are doing there.
units() {
  local set=${1:-demo} u c
  for u in $(kk get modelserving -l k014/unit-of="$set" -o name 2>/dev/null | cut -d/ -f2 | sort -V); do
    local rb; rb=$(kk get rb "$u-modelserving" -o json 2>/dev/null)
    local clusters sched evict
    clusters=$(jq -r '[.spec.clusters[]?.name] | join(",")' <<<"$rb")
    sched=$(jq -r '.status.conditions[]? | select(.type=="Scheduled") | if .status=="True" then "Scheduled" else "NOT-scheduled(" + (.message|.[0:70]) + ")" end' <<<"$rb")
    evict=$(jq -r '[.spec.gracefulEvictionTasks[]? | .fromCluster] | join(",")' <<<"$rb")
    local line="$u -> [${clusters:-none}] $sched"
    [[ -n "$evict" ]] && line+=" evicting-from=[$evict]"
    for c in ${clusters//,/ }; do
      local health pods
      health=$(jq -r --arg c "$c" '.status.aggregatedStatus[]? | select(.clusterName==$c) | .health' <<<"$rb")
      pods=$(km "$c" get pod -l "modelserving.volcano.sh/name=$u" -o json 2>/dev/null \
        | jq -r '[.items[] | (if .spec.nodeName then (if ([.status.conditions[]? | select(.type=="Ready" and .status=="True")] | length) > 0 then "Ready" else "NotReady" end) else "Unschedulable" end)] | group_by(.) | map("\(.[0])x\(length)") | join(" ")')
      line+=" | $c: ${health:-?} pods{${pods:-none}}"
    done
    echo "$line"
  done
}

# estimates <unit>: the last estimator answers the scheduler logged for the unit.
estimates() {
  sched_log "${2:-120s}" | grep "default/$1)" | grep "Invoked MaxAvailable" | tail -"${3:-2}" \
    | sed -E 's/.*Invoked (MaxAvailable[A-Za-z]+) of estimator ([a-z-]+) for workload\([^)]*\): (.*)","v".*/  \2 \1: \3/'
}
