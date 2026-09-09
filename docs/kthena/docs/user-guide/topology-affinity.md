# ModelServing Topology Affinity

ModelServing topology affinity expresses placement relationships between
ServingGroups and between Roles on Volcano's HyperNode tree. The existing
`networkTopology` API is the single topology configuration container:
`groupPolicy` and `rolePolicy` define aggregation boundaries, while affinity
fields define whether different groups share or avoid the same topology
domain.

## Workload mapping

Kthena translates ModelServing concepts to Volcano scheduling concepts as
follows:

| ModelServing concept | Volcano concept |
| --- | --- |
| One ServingGroup replica | One PodGroup |
| One Role definition | One SubGroupPolicy in each PodGroup |
| One Role replica | One SubJob under that SubGroupPolicy |
| Entry and worker Pods of one Role replica | Members of the same SubJob |

Role replicas do not create separate SubGroupPolicy definitions. For example,
`prefill-0` and `prefill-1` are different SubJobs under the single `prefill`
policy. Kthena partitions them using the
`modelserving.volcano.sh/role-id` label.

`subGroupSize` is the number of Pods in one Role replica. `minSubGroups` is the
minimum number of Role-replica SubJobs required by gang scheduling; it neither
creates nor identifies SubJobs.

## API

Configure topology relationships under `spec.template.networkTopology`:

- `servingGroupAntiAffinity` spreads ServingGroups of the same ModelServing.
- `roleAffinity` co-locates the selected Roles within each ServingGroup.
- `roleAntiAffinity` separates replicas of one Role or separates selected Roles
  from one another within each ServingGroup.

Each block accepts `required` hard constraints and `preferred` soft
constraints. Preferred terms require a `weight` from 1 to 100. Every term must
set exactly one of:

- `topologyTierName`, referring to `HyperNode.spec.tierName`; or
- `topologyTier`, referring to `HyperNode.spec.tier`.

Role terms reference names declared in `spec.template.roles`. They never
reference generated Role replica IDs.

The following example separates ServingGroups across communication domains,
co-locates Prefill and Decode Roles within a rack, and spreads replicas of each
Role across node domains:

```yaml
spec:
  schedulerName: volcano
  replicas: 3
  template:
    networkTopology:
      servingGroupAntiAffinity:
        required:
          - topologyTierName: communication-domain
      roleAffinity:
        required:
          - roles: [prefill, decode]
            topologyTierName: rack
      roleAntiAffinity:
        required:
          - roles: [prefill]
            topologyTierName: node
          - roles: [decode]
            topologyTierName: node
```

This example illustrates composable topology rules; the API does not contain
special Prefill/Decode behavior. See the complete
[`topology-affinity.yaml`](https://github.com/volcano-sh/kthena/blob/main/examples/model-serving/topology-affinity.yaml)
example.

## Role anti-affinity semantics

A one-Role term spreads replicas of that Role:

```yaml
roleAntiAffinity:
  required:
    - roles: [prefill]
      topologyTierName: node
```

A multi-Role term separates different listed Roles from each other. It does not
also spread replicas of the same Role:

```yaml
roleAntiAffinity:
  required:
    - roles: [prefill, decode]
      topologyTierName: node
```

Use separate one-Role terms when both Prefill replicas and Decode replicas must
be spread independently.

Role affinity requires at least two Roles. It applies to all SubJobs under the
selected policies and does not pair replicas by ordinal. For example,
`roles: [prefill, decode]` is not limited to pairing `prefill-0` with
`decode-0`.

## Generated PodGroup

Kthena generates selectors and SubGroupPolicy references. Users do not need to
know generated PodGroup names or SubJob IDs. A ServingGroup anti-affinity rule
is translated to a PodGroup rule similar to:

```yaml
spec:
  topologyAffinity:
    podGroupAntiAffinity:
      required:
        - podGroupSelector:
            matchLabels:
              modelserving.volcano.sh/name: topology-affinity-sample
          topologyTierName: communication-domain
```

Role names are copied to the corresponding `subGroupAffinity` and
`subGroupAntiAffinity` terms as SubGroupPolicy names.

## Prerequisites and behavior

- `schedulerName` must be `volcano`.
- The installed Volcano PodGroup CRD must support `spec.topologyAffinity`.
- Role rules additionally require PodGroup `spec.subGroupPolicy`.
- The Volcano scheduler must enable the `group-topology-affinity` plugin.
- The referenced numeric or named tiers must exist in the HyperNode hierarchy.
- When `groupPolicy` or `rolePolicy` is configured, also enable Volcano's
  `network-topology-aware` plugin.

Kthena fails reconciliation and emits a `PodGroupSyncFailed` Event if the
installed PodGroup CRD lacks a required capability. Required rules may leave a
workload pending when the cluster has too few topology domains or insufficient
resources. Preferred rules may fall back to other placements.

`spec.template.networkTopology` is immutable after ModelServing creation. The
validating webhook rejects adding, removing, or changing its aggregation and
affinity fields. ModelServing and Role replicas can still scale when the
topology configuration is unchanged; this guarantees that Pods created during
scale-out use the same constraints as existing Pods. To use a different
topology policy, create a replacement ModelServing and migrate traffic to it.
