#!/usr/bin/env bash
# Print one propagation unit (a ModelServing with replicas 1 = one ServingGroup).
#   gen-unit.sh <name> <shape> [set-label]
# shapes:
#   s1        entry 1 GPU + 3 workers x 1 GPU                       (4 Pods, 4 GPU; fits 2 nodes x 2 GPU)
#   s2        entry 3 GPU + 1 worker 1 GPU                          (2 Pods, 4 GPU; needs a node with 3 GPU)
#   s3        s1 plus nodeSelector k014/pool=big on every Pod
#   aa        entry 1 GPU + 1 worker 1 GPU, required anti-affinity between the two Pods (needs 2 nodes;
#             Karmada's nodeClaim carries no pod anti-affinity, so no estimator can see it)
#   loading   s1 whose containers never become Ready (stands for a group that is still loading its model)
set -euo pipefail
name=$1 shape=$2 set=${3:-demo}

pod() { # gpus extra-podspec-yaml(indented 12)
  cat <<POD
          spec:
            terminationGracePeriodSeconds: 0
$2            containers:
              - name: main
                image: registry.k8s.io/pause:3.10
$3                resources:
                  limits:
                    nvidia.com/gpu: $1
POD
}
sel='            nodeSelector:
              k014/pool: big
'
aa="            affinity:
              podAntiAffinity:
                requiredDuringSchedulingIgnoredDuringExecution:
                  - topologyKey: kubernetes.io/hostname
                    labelSelector:
                      matchLabels:
                        modelserving.volcano.sh/name: $name
"
probe='                readinessProbe:
                  exec:
                    command: ["/does-not-exist"]
                  periodSeconds: 5
'
case $shape in
  s1)      eg=1; wn=3; wg=1; ex=""; pr="" ;;
  s2)      eg=3; wn=1; wg=1; ex=""; pr="" ;;
  s3)      eg=1; wn=3; wg=1; ex="$sel"; pr="" ;;
  aa)      eg=1; wn=1; wg=1; ex="$aa"; pr="" ;;
  loading) eg=1; wn=3; wg=1; ex=""; pr="$probe" ;;
  *) echo "unknown shape $shape" >&2; exit 1 ;;
esac
cat <<YAML
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ModelServing
metadata:
  name: $name
  namespace: default
  labels:
    k014/unit-of: $set
spec:
  schedulerName: default-scheduler
  replicas: 1
  template:
    roles:
      - name: infer
        replicas: 1
        entryTemplate:
$(pod $eg "$ex" "$pr")
        workerReplicas: $wn
        workerTemplate:
$(pod $wg "$ex" "$pr")
YAML
