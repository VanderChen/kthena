#!/usr/bin/env bash
# Run the ModelServing interpreter draft offline with `karmadactl interpret`
# (no cluster needed). KARMADACTL defaults to a binary built from the karmada checkout:
#   (cd karmada && go build -o /tmp/k014/karmadactl ./cmd/karmadactl)
set -uo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"
KARMADACTL=${KARMADACTL:-/tmp/k014/karmadactl}
C=modelserving-customization.yaml
T=testdata

run() { echo; echo "### $*"; "$KARMADACTL" interpret -f "$C" "$@" 2>&1; echo "(exit $?)"; }

run --check
run --operation InterpretReplica --observed-file $T/observed.yaml
run --operation InterpretReplica --observed-file $T/observed-new.yaml
run --operation ReviseReplica --observed-file $T/desired-retain.yaml --desired-replica 3
run --operation Retain --desired-file $T/desired-retain.yaml --observed-file $T/observed-autoscaled.yaml
run --operation InterpretStatus --observed-file $T/observed.yaml
run --operation InterpretStatus --observed-file $T/observed-new.yaml
run --operation InterpretHealth --observed-file $T/observed.yaml
run --operation InterpretHealth --observed-file $T/observed-new.yaml
run --operation InterpretDependency --observed-file $T/observed.yaml
run --operation AggregateStatus --observed-file $T/template.yaml --status-file $T/status-items.yaml
