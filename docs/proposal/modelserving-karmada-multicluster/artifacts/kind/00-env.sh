#!/usr/bin/env bash
# Environment for the Karmada + kthena Kind characterization.
# Source this file; it does not start anything.
#
# Set WS to a directory that holds a karmada checkout (./karmada) and a kthena checkout (./kthena),
# or set KARMADA_REPO and KTHENA_REPO directly. The runs recorded under ../logs used
# karmada 6d007132 (master) and kthena 1a5145e9 (main).
export KARMADA_REPO=${KARMADA_REPO:-${WS:?set WS, or KARMADA_REPO and KTHENA_REPO}/karmada}
export KTHENA_REPO=${KTHENA_REPO:-${WS:?set WS, or KARMADA_REPO and KTHENA_REPO}/kthena}

# Kubeconfigs hold credentials: keep them out of the repository.
export KUBECONFIG_PATH=$HOME/.kube/k014
export MAIN_KUBECONFIG=$KUBECONFIG_PATH/karmada.config
export MEMBER_CLUSTER_KUBECONFIG=$KUBECONFIG_PATH/members.config
export KIND_LOG_FILE=/tmp/k014/kind-logs

export HOST_CLUSTER_NAME=k014-host
export MEMBER_CLUSTER_1_NAME=k014-m1
export MEMBER_CLUSTER_2_NAME=k014-m2
export PULL_MODE_CLUSTER_NAME=k014-m3

# kubectl shortcuts: kk = Karmada API server, kh = host cluster, km <member> = member cluster.
kk() { kubectl --kubeconfig "$MAIN_KUBECONFIG" --context karmada-apiserver "$@"; }
kh() { kubectl --kubeconfig "$MAIN_KUBECONFIG" --context "$HOST_CLUSTER_NAME" "$@"; }
km() { local c=$1; shift; kubectl --kubeconfig "$MEMBER_CLUSTER_KUBECONFIG" --context "$c" "$@"; }
