#!/bin/bash

# Copyright The Volcano Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

KT_ROOT=$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )/..
CLUSTER_NAME=${CLUSTER_NAME:-kthena}
CLUSTER_CONTEXT=("--name" "${CLUSTER_NAME}")
KIND_OPT=${KIND_OPT:-}
INSTALL_MODE=${INSTALL_MODE:-"kind"}
KT_NAMESPACE=${KT_NAMESPACE:-"kthena-system"}
IMAGE_PREFIX=${IMAGE_PREFIX:-ghcr.io/volcano-sh}
TAG=${TAG:-$(git rev-parse --verify HEAD 2>/dev/null || echo "latest")}
HELM_RELEASE_NAME=${HELM_RELEASE_NAME:-kthena}
OS=${OS:-$(go env GOOS 2>/dev/null || echo "linux")}

# prepare deploy yaml and docker images
function prepare {
  echo "Preparing..."
  install-helm

  echo "Building docker images"
  make docker-build-all HUB=${IMAGE_PREFIX} TAG=${TAG}
}

function install-kthena {
  # Create namespace if it doesn't exist
  kubectl create namespace ${KT_NAMESPACE} --dry-run=client -o yaml | kubectl apply -f -
  
  # Install kthena using helm
  helm upgrade --install ${HELM_RELEASE_NAME} ${KT_ROOT}/charts/kthena \
    --namespace ${KT_NAMESPACE} \
    --set workload.controllerManager.image.repository=${IMAGE_PREFIX}/kthena-controller-manager \
    --set workload.controllerManager.image.tag=${TAG} \
    --set workload.controllerManager.image.pullPolicy=IfNotPresent \
    --set workload.controllerManager.downloaderImage.repository=${IMAGE_PREFIX}/downloader \
    --set workload.controllerManager.downloaderImage.tag=${TAG} \
    --set workload.controllerManager.runtimeImage.repository=${IMAGE_PREFIX}/runtime \
    --set workload.controllerManager.runtimeImage.tag=${TAG} \
    --set networking.kthenaRouter.image.repository=${IMAGE_PREFIX}/kthena-router \
    --set networking.kthenaRouter.image.tag=${TAG} \
    --set networking.kthenaRouter.image.pullPolicy=IfNotPresent
}

function uninstall-kthena {
  helm uninstall ${HELM_RELEASE_NAME} --namespace ${KT_NAMESPACE} || true
  kubectl delete namespace ${KT_NAMESPACE} --ignore-not-found=true
}

# clean up
function cleanup {
  uninstall-kthena

  if [ "${INSTALL_MODE}" == "kind" ]; then
    echo "Running kind: [kind delete cluster ${CLUSTER_CONTEXT[*]}]"
    kind delete cluster "${CLUSTER_CONTEXT[@]}" || true
  fi
}

echo $* | grep -E -q "\-\-help|\-h"
if [[ $? -eq 0 ]]; then
  echo "Customize the kind-cluster name:

    export CLUSTER_NAME=<custom cluster name>  # default: kthena

Customize kind options other than --name:

    export KIND_OPT=<kind options>

Using existing kubernetes cluster rather than starting a kind cluster:

    export INSTALL_MODE=existing

Customize image prefix:

    export IMAGE_PREFIX=<image prefix>  # default: ghcr.io/volcano-sh

Customize image tag:

    export TAG=<image tag>  # default: git commit hash

Customize namespace:

    export KT_NAMESPACE=<namespace>  # default: kthena-system

Customize helm release name:

    export HELM_RELEASE_NAME=<release name>  # default: kthena

Cleanup all installation:

    ./hack/local-up-kthena.sh -q

Run preflight checks only (without deploying):

    ./hack/local-up-kthena.sh --check-only
"
  exit 0
fi

echo $* | grep -E -q "\-\-quit|\-q"
if [[ $? -eq 0 ]]; then
  cleanup
  exit 0
fi

# Check for --check-only flag
echo $* | grep -E -q "\-\-check-only"
if [[ $? -eq 0 ]]; then
  source "${KT_ROOT}/hack/lib/install.sh"
  check-prerequisites
  exit $?
fi

source "${KT_ROOT}/hack/lib/install.sh"

# Run preflight checks (will exit with error if fatal issues found)
check-prerequisites || exit 1

prepare

if [ "${INSTALL_MODE}" == "kind" ]; then
  kind-up-cluster
  export KUBECONFIG=${HOME}/.kube/config
fi

install-kthena

echo ""
echo "Kthena has been installed successfully!"
echo "Namespace: ${KT_NAMESPACE}"
echo "Release name: ${HELM_RELEASE_NAME}"
echo ""
echo "To check the status:"
echo "  kubectl get pods -n ${KT_NAMESPACE}"
echo ""
echo "To uninstall:"
echo "  ./hack/local-up-kthena.sh -q"

