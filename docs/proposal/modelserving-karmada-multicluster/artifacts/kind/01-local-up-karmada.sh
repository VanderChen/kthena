#!/usr/bin/env bash
# Bring up Karmada (built from the karmada checkout) on Kind with
# hack/local-up-karmada.sh, using dedicated cluster names (k014-*) and kubeconfigs.
#
# member1 gets two worker nodes so that node-level fragmentation can be modelled.
# The kind config under artifacts/ is patched only for the duration of the run
# and restored with `git checkout` afterwards (see the trap).
set -euo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/00-env.sh"

mkdir -p "$KUBECONFIG_PATH" "$KIND_LOG_FILE"
cd "$KARMADA_REPO"
git --no-pager log --oneline -1

M1=artifacts/kindClusterConfig/member1.yaml
trap 'git -C "$KARMADA_REPO" checkout -- "$M1"; git -C "$KARMADA_REPO" status --short' EXIT
printf '  - role: worker\n  - role: worker\n' >> "$M1"

hack/local-up-karmada.sh
