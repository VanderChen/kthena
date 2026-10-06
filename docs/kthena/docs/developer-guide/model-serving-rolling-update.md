# Rollout Strategy

Rolling updates represent a critical operational strategy for online services aiming to achieve zero downtime. In the context of LLM inference services, the implementation of rolling updates is important to reduce the risk of service unavailability.

`ModelServing` supports rolling updates at either the `ServingGroup` or `Role` level. Select exactly one granularity with `spec.rolloutStrategy.type`:

- `ServingGroupRollingUpdate` uses `spec.rolloutStrategy.rollingUpdateConfiguration`. Its `maxUnavailable` limits unavailable `ServingGroups`, `maxSurge` limits temporary additional `ServingGroups`, and `partition` protects stable `ServingGroups` from updates.
- `RoleRollingUpdate` uses the inline `maxUnavailable`, `maxSurge`, and `partition` fields on each entry in `spec.template.roles`. The settings are applied independently to each `Role` in every `ServingGroup`. The ModelServing-level `rollingUpdateConfiguration` does not participate in the Role availability budget.

Settings at the other granularity may remain configured but do not affect rollout. Their basic syntax is validated; replica-dependent bounds and budget combinations are checked only at the selected granularity. `maxUnavailable` accepts an absolute number or a percentage and defaults to `1`. A percentage is calculated from `spec.replicas` for `ServingGroupRollingUpdate` and from the corresponding Role's `replicas` for `RoleRollingUpdate`.

Both strategies resolve and persist the target `ControllerRevision` before reconciling workload resources, including independent Role updates without `roleCoordination`. If history cannot be persisted, reconciliation retries before creating or deleting Pods. Equivalent historical templates are reused across controller upgrades and replica-only changes without rewriting their snapshot data or relabeling existing Pods.

History cleanup preserves the current and update revisions and every revision referenced by existing ServingGroups, Roles, or Pods, including terminating and surge Pods. A completed partial Role update can therefore retain more than two snapshots: unchanged Roles may still reference earlier revisions. `spec.revisionHistoryLimit` limits only unreferenced snapshots (default `10`); `0` removes all unreferenced snapshots while preserving live history. Cleanup retries independently of revision-status changes and removes the oldest eligible history owned by the current ModelServing first. Recovery that requires missing, unreadable, or foreign-owned history stops and retries rather than applying the latest template under an old revision label; already deleted historical templates cannot be reconstructed from the latest spec.

`maxSurge` accepts a non-negative absolute number or percentage, defaults to `0`, and rounds percentages up. It is calculated from `spec.replicas` for `ServingGroupRollingUpdate` and from the corresponding Role's `replicas` for `RoleRollingUpdate`. `maxSurge` percentages may exceed `100%`, provided replicas plus the resolved surge fit in a non-negative int32. `maxUnavailable` and `partition` percentages must be within `0%`–`100%`; their integer values must not exceed the corresponding replicas. At zero replicas, `maxUnavailable: 1` remains valid because it is the API default. Both budgets are resolved against the corresponding replica count: `maxUnavailable` percentages round down and `maxSurge` percentages round up. An omitted `maxUnavailable` resolves to `1`; an omitted `maxSurge` resolves to `0`. Any combination that resolves to two zeros is rejected, including at zero replicas or a full partition. For example, with three replicas, `maxUnavailable: "25%"` resolves to `0` and requires a `maxSurge` that resolves above `0`.

`partition` protects ServingGroups whose ordinals are in `[0, partition)`. The remaining ServingGroups are eligible for rolling update. This definition remains deterministic when binpack scale-down leaves a sparse ordinal set.

## ServingGroup rolling update

When a protected replica is missing and must be recreated, the ordinal itself selects the template: a missing ordinal below `partition` uses `CurrentRevision` and its historical template, while other missing ordinals use `UpdateRevision` and the current template. Before creating a `ServingGroup` that references a new revision, the controller must successfully persist its `ControllerRevision`; otherwise reconciliation stops and retries without creating a partial `ServingGroup`.

Here's a ModelServing configured with rollout strategy:

```yaml
spec:
  rolloutStrategy:
    type: ServingGroupRollingUpdate
    rollingUpdateConfiguration:
      maxUnavailable: 0
      maxSurge: 1
      partition: 0
```

With `replicas: 4` and `maxSurge: 1`, the controller temporarily changes the expected ServingGroup count from four to five while an updateable outdated ServingGroup exists. Normal replica synchronization creates or removes the required capacity; rolling-update reconciliation only selects outdated groups within the availability budget.

The controller always enforces these bounds:

$$
N_{live} \leq replicas + maxSurge
$$

$$
N_{available} \geq replicas - maxUnavailable
$$

An unready additional ServingGroup counts toward the replica ceiling but does not contribute to availability. If it cannot be scheduled, the rollout waits without deleting available capacity. Temporary instances created beyond the desired ordinal range carry a controller-managed `modelserving.volcano.sh/surge` Pod annotation. The annotation is separate from the template and revision hash and lets the controller finish cleanup after a restart.

After the last eligible outdated ServingGroup is removed, the controller restores missing desired instances before retiring marked surge groups. Creation stays within `replicas + maxSurge`; deletion preserves the `maxUnavailable` budget. With `maxUnavailable: 0`, a Ready surge group stays until its replacement is Ready. A rollout starting with `0..replicas-1` therefore returns to that ordinal range before it reports completion.

Unmarked sparse or high ordinals left by ordinary binpack scale-down remain normal replicas. Increasing `replicas` to include a temporary ordinal adopts that instance without replacing its Pods and removes its surge annotation. Lowering the surge budget during cleanup uses the new availability budget; zero budgets pause replacement. Existing unmarked sparse instances from an older controller are not automatically treated as temporary replicas.

A healthy high ordinal already using the target template retains its identity. An outdated high ordinal can instead be replaced at the lowest missing ordinal during a real template rollout. Partition always applies to the new ordinal: with `replicas: 2`, `partition: 2`, and old replicas `{0:v1, 3:v1}`, submitting v2 can replace 3 with 1 using historical v1. The resulting `{0:v1, 1:v1}` is a partition pause, with zero updated replicas, rather than full adoption of v2. A stable hole alone never starts a rollout. The same rule applies to Role replicas.

In the following we'll show how rolling update processes for a `ModelServing` with four replicas. Three Replica status are simulated here:

- ✅ Replica has been updated
- ❎ Replica hasn't been updated
- ⏳ Replica is in rolling update

| | R-0 | R-1 | R-2 | R-3 | Note |
| --- | --- | --- | --- | --- | --- |
| Stage1 | ✅ | ✅ | ✅ | ✅ | Before rolling update |
| Stage2 | ❎ | ❎ | ❎ | ⏳ | Rolling update started; R-3 is selected first in this example |
| Stage3 | ❎ | ❎ | ⏳ | ✅ | R-3 is updated. The next replica (R-2) is now being updated |
| Stage4 | ❎ | ⏳ | ✅ | ✅ | R-2 is updated. The next replica (R-1) is now being updated |
| Stage5 | ⏳ | ✅ | ✅ | ✅ | R-1 is updated. The last replica (R-0) is now being updated |
| Stage6 | ✅ | ✅ | ✅ | ✅ | Update completed. All replicas are on the new version |

During a rolling upgrade, the controller selects an eligible outdated replica while respecting partition and availability constraints, then deletes and rebuilds it. Unhealthy outdated replicas are prioritized; ordinal order is used within the applicable candidate ordering. The controller does not proceed beyond the availability budget until replacement capacity is ready.

## Role rolling update

Use `RoleRollingUpdate` when only the changed Roles should be recreated instead of rebuilding an entire `ServingGroup`. Configure the availability budget and partition directly on each Role:

```yaml
spec:
  rolloutStrategy:
    type: RoleRollingUpdate
  template:
    roles:
      - name: prefill
        replicas: 4
        maxUnavailable: 0
        maxSurge: 1
        partition: 0
        # entryTemplate and other Role fields are omitted
      - name: decode
        replicas: 2
        maxUnavailable: 1
        maxSurge: 1
        partition: 0
        # entryTemplate and other Role fields are omitted
```

Kthena evaluates Role updates across all `ServingGroups`. Because each `ServingGroup` applies the per-Role availability budget independently, `RoleRollingUpdate` is recommended for a ModelServing with a single `ServingGroup`.

While an updateable outdated Role replica exists, the controller temporarily changes that Role's expected replica count from `replicas` to `replicas + maxSurge`. Normal Role replica synchronization creates and later removes the additional capacity, while rolling-update reconciliation only selects outdated replicas within the `maxUnavailable` budget. An unready new replica consumes availability budget and can naturally block further deletion.

Role rollout uses the same completion rules within each ServingGroup: restore missing desired Role instances, wait for sufficient Ready capacity, and remove the marked temporary instances using that Role's budgets. Entry and worker Pods carry the marker so cleanup survives controller restart. Unmarked binpack survivors retain their identities; expansion can adopt a temporary Role instance without changing its Pod UIDs.

`RoleRollingUpdate` rejects `recoveryPolicy: ServingGroupRecreate`, because deleting an outdated Role would recreate its entire ServingGroup. Use `RoleRecreate` or `None`. `ServingGroupRollingUpdate` supports all three recovery policies.
