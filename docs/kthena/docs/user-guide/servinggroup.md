# ServingGroup Objects

A ModelServing runs `spec.replicas` ServingGroups. Each of them is a Kubernetes object of kind `ServingGroup` (API group `workload.serving.volcano.sh/v1alpha1`, short name `sg`). You do not create ServingGroups: the ModelServing controller creates one for every group of a ModelServing and deletes it when the group is removed or replaced.

This page explains what you see when you run `kubectl get servinggroups`, and what you may do with these objects.

If you upgrade from a Kthena version that had no `ServingGroup` objects, read [Upgrading to ServingGroup Objects](./servinggroup-upgrade.md) first.

## What a ServingGroup Is

A ServingGroup object is named `<modelserving>-<ordinal>`. It is owned by the ModelServing and owns everything that belongs to the group.

The examples on this page use a ModelServing `gang` with two replicas, `gang-0` and `gang-1`. It has one Role, `leader`, with one entry Pod and two worker Pods, and uses the `headless-service` plugin and gang scheduling:

```text
ModelServing  gang
└── ServingGroup  gang-0
    ├── Pod       gang-0-leader-0-0             entry Pod of Role instance leader-0
    ├── Pod       gang-0-leader-0-1             worker Pod
    ├── Pod       gang-0-leader-0-2             worker Pod
    ├── Service   gang-0-leader-0-0             headless Services of the headless-service plugin
    ├── Service   gang-0-leader-0-0-discovery
    └── PodGroup  gang-0                        with gang scheduling (Volcano)
```

Two controllers in `kthena-controller-manager` share the work:

- The **ModelServing controller** decides which ServingGroups exist and at which revision. It creates and deletes `ServingGroup` objects and writes their `spec`. It does not create or delete Pods.
- The **ServingGroup controller** manages what is inside one group: it creates and deletes the Pods, maintains the PodGroup and the plugin resources, applies `recoveryPolicy`, and writes the `status` of the group.

What this gives you:

- The state of a group is stored on its object, not in the memory of the controller. After a restart of the controller manager, or a change of the leader, the controller continues from what the objects say.
- A Pod that is lost while no controller is running is handled by `recoveryPolicy` when the controller is back, in the same way as a Pod lost while it runs.
- You can see the state of every group with `kubectl`.

## Inspecting ServingGroups

### List the Groups of a ModelServing

ServingGroups carry the label `modelserving.volcano.sh/name` with the name of their ModelServing:

```text
$ kubectl get servinggroups -l modelserving.volcano.sh/name=gang
NAME     REVISION     READY   PODS   EXPECTED   AGE
gang-0   79bdf4ffdf   True    3      3          6m
gang-1   79bdf4ffdf   True    3      3          6m
```

| Column | Field | Meaning |
|---|---|---|
| `REVISION` | `status.revision` | The revision that every Pod of the group runs. Empty while the Pods run different revisions, for example in the middle of a Role rolling update, and when the group has no Pod. |
| `READY` | `Ready` condition | `True` when every Role instance of the group is complete and ready. |
| `PODS` | `status.readyPods` | The number of Pods that are running and ready. |
| `EXPECTED` | `spec.expectedPods` | The number of Pods of the complete group. |
| `FAILED` | `Failed` condition | Shown with `-o wide`. `True` when the group is being replaced as a whole. |
| `AGE` | `metadata.creationTimestamp` | A group that was replaced is a new object: its age starts again. |

The same revision values appear in `status.currentRevision` and `status.updateRevision` of the ModelServing and in the `modelserving.volcano.sh/revision` label of the Pods.

A ServingGroup with the annotation `modelserving.volcano.sh/surge: serving-group` is temporary capacity of a rolling update with `maxSurge`. It is removed when the rollout ends: every group runs the new revision and `replicas` other groups have a Pod. Until then it stays, and serves if it is ready. A surge group that stays after every other group is updated means that a regular group has no Pod: look for the group with 0 Pods in `kubectl get servinggroups` and read its `ReconcileFailed` condition (see [A quota that is too small](#a-quota-that-is-too-small)). The annotation is managed by the controller; do not set or remove it.

Under `RoleRollingUpdate`, a ServingGroup that the controller created new carries the annotation `modelserving.volcano.sh/created-revision` for as long as the revision it was created from is the update revision of the ModelServing. Every Role instance of such a group is created from that revision. This matters when a Role has a `partition`. The partition keeps the lowest instances of a group at the current revision, and such an instance is created from the current revision again when it is lost. A group that is added during the rollout never ran the current revision: all its instances run the update revision, and one that is lost comes back at the update revision. The controller removes the annotation when the update revision changes, and never adds it later: from then on the group is treated like the groups that were there before. A group that has no Pod yet keeps the annotation, with the new update revision as its value, and is created whole from that revision. A ServingGroup that was created for Pods that existed already, during an upgrade from a version without ServingGroup objects or after a deletion with `--cascade=orphan`, does not carry it. Do not set or remove the annotation.

### Read the Status of a Group

```sh
kubectl get servinggroup gang-0 -o yaml
```

The output, abridged:

```yaml
apiVersion: workload.serving.volcano.sh/v1alpha1
kind: ServingGroup
metadata:
  name: gang-0
  namespace: default
  generation: 1
  labels:
    modelserving.volcano.sh/name: gang
  ownerReferences:
  - apiVersion: workload.serving.volcano.sh/v1alpha1
    kind: ModelServing
    name: gang
    controller: true
    blockOwnerDeletion: true
spec:
  revision: 79bdf4ffdf
  targetRevision: 79bdf4ffdf
  baselineRevision: 79bdf4ffdf
  expectedPods: 3
  recoveryPolicy: RoleRecreate
  restartGracePeriodSeconds: 0
  roles:
  - name: leader
    replicas: 1
    maxUnavailable: 1
  sourceGeneration: 1
status:
  observedGeneration: 1
  conditions:
  - type: Ready
    status: "True"
    reason: AllInstancesReady
    message: every Role instance is ready
    lastTransitionTime: "2026-11-25T05:01:32Z"
    observedGeneration: 1
  revision: 79bdf4ffdf
  referencedRevisions:
  - 79bdf4ffdf
  pods: 3
  readyPods: 3
  faultyMembers: 0
  terminatingPods: 0
  labelSelector: modelserving.volcano.sh/group-name=gang-0
  observedPods: v1.8133017729bb14994d35f21742bae521
  roleInstances:
  - role: leader
    index: 0
    phase: Established
    generation: 1
```

The `spec` is written by the ModelServing controller. It holds the revision of the group, the revision the rollout of the ModelServing moves to (`targetRevision`, the update revision of the ModelServing) and the settings of the ModelServing that apply inside a group. Under `ServingGroupRollingUpdate`, a group whose `revision` differs from `targetRevision` is at an earlier revision: it starts no update of its Pods. The ModelServing controller updates or replaces it within the rollout budget, unless `partition` protects it. Under `RoleRollingUpdate`, `revision` is empty and the Role instances are rolled to `targetRevision`. The `spec` contains no Pod template: templates are read from the `ControllerRevision` of the revision.

The `status` is written by the ServingGroup controller:

| Field | Meaning |
|---|---|
| `observedGeneration` | The generation of the `spec` this status was computed from. While it is lower than `metadata.generation`, the status does not yet reflect the latest `spec`. |
| `conditions` | See [Conditions](#conditions). |
| `revision` | The revision that every Pod of the group runs, read from the Pods. Empty when the Pods run different revisions or the group has no Pod. |
| `referencedRevisions` | Every revision that a Pod of the group refers to, including terminating Pods. The `ControllerRevision` objects of these revisions are kept. |
| `pods` | The number of Pods of the group, including terminating Pods and Pods that the controller has decided to create and not created yet. |
| `readyPods` | The number of Pods that are running and ready. |
| `faultyMembers` | The number of Pods that have failed, or keep restarting and are not ready, and are being recovered. A fault that is tolerated, or still within the grace period, is not counted. |
| `terminatingPods` | The number of Pods that are being deleted. |
| `roleInstances` | One entry for every Role instance of the group. See [Role Instances](#role-instances). |
| `labelSelector` | Selects the Pods of the group. Used by the `scale` subresource. |
| `observedPods` | A digest of the Pods this status was computed from. The controllers use it to tell whether the status is up to date. It has no meaning for users. |

`kubectl describe servinggroup gang-0` shows the same conditions and the events of the object.

### Conditions

| Condition | Meaning | Reasons | What to do |
|---|---|---|---|
| `Ready` | `True` when every Role has its declared number of instances and every Pod of every instance is running and ready. A group whose Roles all have zero replicas is ready. | `AllInstancesReady`. `InstancesNotReady`: the message names an instance that is not ready or is being updated in place. `InstanceCountMismatch`: a Role has more or fewer instances than declared, as during Role scaling or a Role rolling update. `TemplateUnresolved`: the revision of the group cannot be read; the group then counts as ready when it has at least `spec.expectedPods` Pods and all of them are ready. | Look at the Pods of the group. |
| `Failed` | The group is replaced as a whole. The condition never returns to `False`: the object is deleted and created again. | `MemberLost` and `MemberFaulty`, under `recoveryPolicy: ServingGroupRecreate`. `AllPodsLost`: under `ServingGroupRollingUpdate`, no Pod of the group is left. | Nothing. The ModelServing controller deletes the group once `Drained` is `True`. |
| `Drained` | Present together with `Failed`. `True` when no Pod with a fault and no terminating Pod is left in the group, so that the remaining Pods may be deleted. | `NoFaultyMember`, `FaultyMembersRemain`. | If it stays `False`, a Pod of the group does not terminate. Look at that Pod. |
| `RecreateRequired` | The group can only be brought to its revision by replacing it. Under `ServingGroupRollingUpdate` it stays `True` until the group is replaced. A group under `RoleRollingUpdate` does not have the condition; it is removed when the rollout strategy is changed to `RoleRollingUpdate`. | `InPlaceUpdateNotPossible`: the Pods of the group cannot be updated in place, or the group is updated by recreation and has Pods of another revision. The message says why. | Nothing. The group is replaced within the rollout budget. See [Fallback to Recreate](./inplace-update.md#fallback-to-recreate). |
| `DrainBlocked` | A Role instance cannot be removed because the `OnRoleDelete` hook of a plugin keeps failing. The instance is not created again before the hook succeeds. | `RoleDeleteHookFailed`. The message carries the error of the hook. | Check the log of the controller manager and the resources of the plugin. |
| `ReconcileFailed` | The last round of the ServingGroup controller for this group ended with an error. `Ready` is still reported from the Pods. The condition is removed when a round succeeds. | `NameOccupied`: an object with the name of a member Pod, of the PodGroup or of a Service of the group exists and is not controlled by the group. The same reason is reported while Pods of an earlier ServingGroup object of the same name still exist: the group creates no Pod until they are gone. `Error`: any other error; the message carries it. | For `NameOccupied`, remove the object that holds the name. Pods of an earlier object are deleted by the garbage collector: wait. The controller retries. |
| `CoordinatedRoleRolloutBlocked` | Reported per group for a coordinated Role rolling update. See [`roleCoordination`](../reference/crd/workload.serving.volcano.sh.md#rolecoordination). | | |

### Role Instances

`status.roleInstances` has one entry for every Role instance of the group: the Role, the index of the instance, a phase and a generation.

| Phase | Meaning |
|---|---|
| `Creating` | The instance is being created. A missing Pod is created; nothing else is deleted. |
| `Established` | The instance was complete: the controller has seen all of its Pods at the same time. From now on, a Pod that is missing or terminating was lost, and `recoveryPolicy` applies. |
| `Draining` | The Pods of the instance are being deleted: because of a recovery, a scale-down or a rolling update of the Role. |

`generation` counts how often the Pods of the instance were created as a set. It starts at 0 and increases by one every time the instance is recreated. Every Pod carries the generation it was created for in the label `modelserving.volcano.sh/instance-generation`.

### Events

Events about a group or a Role instance are recorded on the ModelServing, so that one command shows the history of the whole workload:

```sh
kubectl describe modelserving gang
```

Groups:

| Event | Type | Meaning |
|---|---|---|
| `ServingGroupCreated` | Normal | A ServingGroup was created. The message names the group, its revision and why: `regular` (a place below `replicas` is filled), `surge` (temporary capacity of a rolling update), `recover` (Pods of the group exist without the object). |
| `ServingGroupDeleted` | Normal | A ServingGroup was deleted. The message names the group and why: `scale-down`, `replace` or `replace-notready` (rolling update), `failed` (recovery of the whole group), `reclaim-surge` (a surge group is removed), `memberless-out-of-range` or `empty-out-of-range` (a group without Pods at an ordinal at or above `replicas`). |
| `OrdinalOccupied` | Warning | A ServingGroup object with the name of a group exists and is not controlled by this ModelServing. The group is not created until that object is gone. |

Role instances:

| Event | Type | Meaning |
|---|---|---|
| `RoleCreating` | Normal | The Pods of a Role instance are being created, or a Pod of the instance is not ready. An instance whose Pods are updated in place is in this state until they are ready again. |
| `RoleRunning` | Normal | Every Pod of the Role instance is ready. |
| `RoleDeleting` | Normal | The Role instance is being removed: by a scale-down, a rolling update of the Role or a recovery. |

An event is recorded when the instance enters the state, not in every round. The message names the instance and its group, for example `Role leader/leader-0 in ServingGroup gang-0 is now Running`. After a restart of the controller manager or a change of the leader, `RoleRunning` is not recorded again for the instances that are running.

Rolling updates:

| Event | Type | Meaning |
|---|---|---|
| `InPlaceUpdateStarted` | Normal | The images of the Pods of a group, or of a Role instance, were patched. See [In-place Update](./inplace-update.md). |
| `InPlaceUpdateCompleted` | Normal | Every Pod of the group or of the Role instance runs the new revision and is ready. |
| `InPlaceUpdateFallback` | Normal | A group or a Role instance is recreated instead of updated in place. The message says why. |
| `InPlaceUpdateTimeout` | Warning | An in-place update did not finish within `progressDeadlineSeconds`. `recoveryPolicy` is applied to its Pods. |
| `DependencyNotReady`, `MaxSkewLimitReached`, `OldVersionDependencyPresent`, `IncompatiblePartitions` | Warning | A coordinated Role rolling update waits. The event carries the reason and the message of the `CoordinatedRoleRolloutBlocked` condition of the ModelServing and is recorded when the condition changes. See [`roleCoordination`](../reference/crd/workload.serving.volcano.sh.md#rolecoordination). |
| `ProgressAvailable`, `RolloutComplete` | Normal | The coordinated Role rolling update can go on, or is complete. |

Failures:

| Event | Type | Meaning |
|---|---|---|
| `PluginError` | Warning | The `OnPodCreate` hook of a plugin returned an error for a Pod that was to be created. The Pod is not created, and the ServingGroup reports `ReconcileFailed`. The message carries the error. |
| `RevisionUnresolved` | Warning | A revision cannot be read or recorded. Either the `ControllerRevision` of the revision of a ServingGroup is missing, is not owned by the ModelServing or cannot be decoded; the message names the group, and the event is recorded once for a group and a revision. Or the revision of the current template of the ModelServing cannot be recorded. Nothing is created or replaced from such a revision until it can be read. A reduction of `replicas` is still carried out. |

Upgrade and rollback (see [Upgrading to ServingGroup Objects](./servinggroup-upgrade.md)):

| Event | Type | Meaning |
|---|---|---|
| `PodNotTakenOver` | Warning | A Pod is owned by the ModelServing and is not a member of one of its groups, for example a copy of a member Pod under another name. It is not taken over and not deleted. Recorded while the workloads of an earlier version are taken over. |
| `RolledBack` | Normal | In rollback mode, the workloads were handed back to the ModelServing and its ServingGroup objects are gone. |

Two events are recorded on the ServingGroup itself and show in `kubectl describe servinggroup`:

| Event | Type | Meaning |
|---|---|---|
| `OwnerNotFound` | Warning | The ServingGroup is not controlled by a ModelServing, or its name is not `<modelserving>-<ordinal>`. The controller leaves such a group alone: it creates and deletes nothing for it. The event is not recorded for a group whose ModelServing is being deleted; the garbage collector deletes that group. |
| `StatusNotPersisted` | Warning | The API server did not keep the status of the group as the controller wrote it. This happens when the `ServingGroup` CRD is older than the controller manager. The controller does not act on the group. Apply the CRDs of the installed version. |

Two more events concern the controller manager, not a workload: `WaitingForPredecessor` on the controller manager Pod and `RollbackMode` on the rollback switch. See [Upgrading to ServingGroup Objects](./servinggroup-upgrade.md).

### When a Rollout Waits

When a ModelServing has groups to create, delete or replace and can do none of it for five minutes, the ModelServing gets the condition `RolloutBlocked`. The reason says what it waits for; the message names the group.

| Reason | Meaning | What to do |
|---|---|---|
| `WaitingForDeletion` | A ServingGroup is being deleted and keeps its ordinal until its Pods are gone, or a failed group still has Pods that do not terminate. | Find the Pod of that group that does not terminate. |
| `GroupNotObserved` | The status of a ServingGroup does not reflect its `spec` or its Pods yet. | Check the `ReconcileFailed` condition of the group and the log of the controller manager. |
| `TargetNotReady` | A ServingGroup of the new revision does not become ready. | Look at the Pods of that group: image, probes, scheduling. If the group has no Pods, they could not be created: the message of the condition ends with the error, which is also in the `ReconcileFailed` condition of the group. |
| `BudgetExhausted` | A ServingGroup is not ready, and replacing another one would exceed `maxUnavailable`. | Fix the group that is not ready, or raise `maxUnavailable`. |
| `OrdinalOccupied` | The name of a group to create is held by an object that the ModelServing does not control. Reported at once. | Remove that object. |
| `BaselineUnknown` | A group below `partition` has to be created from the current revision, and that revision cannot be determined. Reported at once. | With `partition` set to 0, the group is created from the update revision. |

### A Quota That Is Too Small

A rolling update with `maxSurge` needs room for `replicas + maxSurge` groups (under `RoleRollingUpdate`: `replicas + maxSurge` instances of the Role in every group) in every limit that is checked when Pods or PodGroups are created: a ResourceQuota on `pods`, on resource requests such as GPUs, or on `count/podgroups.scheduling.volcano.sh`.

If the limit is smaller, the last group to be replaced can be left without Pods while the surge group holds what it needs. The surge group is not removed, because it is the group that stands in for that place. The ModelServing then reports `RolloutBlocked` with reason `TargetNotReady`, and the ServingGroup it names reports `ReconcileFailed` with the error of the API server.

Raise the limit, or delete the surge ServingGroup (the one with the annotation `modelserving.volcano.sh/surge`); it is not created again. Lowering `maxSurge` does not remove it.

## Finding the Objects of a Group

Pods carry these labels:

| Label | Value |
|---|---|
| `modelserving.volcano.sh/name` | Name of the ModelServing |
| `modelserving.volcano.sh/group-name` | Name of the ServingGroup |
| `modelserving.volcano.sh/role` | Name of the Role |
| `modelserving.volcano.sh/role-id` | Role instance, for example `leader-0` |
| `modelserving.volcano.sh/entry` | `true` on the entry Pod of a Role instance |
| `modelserving.volcano.sh/revision` | Revision the Pod runs |
| `modelserving.volcano.sh/instance-generation` | Generation of the Role instance the Pod was created for |

```sh
# All Pods of a ModelServing, with their group and Role instance
kubectl get pods -l modelserving.volcano.sh/name=gang \
  -L modelserving.volcano.sh/group-name,modelserving.volcano.sh/role-id,modelserving.volcano.sh/instance-generation

# The Pods and Services of one group, and its PodGroup
kubectl get pods,services -l modelserving.volcano.sh/group-name=gang-0
kubectl get podgroups.scheduling.volcano.sh gang-0

# From a Pod to its group and its ModelServing
kubectl get pod gang-0-leader-0-1 -L modelserving.volcano.sh/group-name,modelserving.volcano.sh/name
```

The controller owner of a Pod is its ServingGroup, and the controller owner of the ServingGroup is the ModelServing. A tool that finds the Pods of a ModelServing by owner reference has to follow both steps. Apart from `modelserving.volcano.sh/instance-generation`, which is new, the labels are the same as in earlier versions.

## How Recovery Shows Up

`recoveryPolicy` applies when a Pod of an established Role instance is deleted, or when a Pod has failed or keeps restarting without becoming ready for longer than `restartGracePeriodSeconds`.

| `recoveryPolicy` | What happens | What you see |
|---|---|---|
| `RoleRecreate` (default) | The Role instance of the Pod is recreated: all its Pods and, with the `headless-service` plugin, its Services. | The entry in `status.roleInstances` goes to `Draining` and back to `Creating` and `Established`, and its `generation` increases by one. The new Pods carry the new generation in their label. The ServingGroup and the PodGroup are the same objects as before. Other Role instances of the group are not touched. |
| `ServingGroupRecreate` | The whole group is replaced. | The group gets the condition `Failed`. Pods with a fault are deleted first; when none is left, `Drained` becomes `True`. The ModelServing controller then deletes the ServingGroup, which deletes its Pods, Services and PodGroup, and creates a ServingGroup with the same name. The new object has a new UID and a new age; its Role instances start at generation 0. |
| `None` | Only a missing Pod is created again. A Pod that has failed or keeps restarting is left alone. | The ServingGroup, the phases and the generations stay as they are. |

With `restartGracePeriodSeconds: -1`, a Pod that has failed or keeps restarting is tolerated under every policy.

Under `ServingGroupRollingUpdate`, a group that had an established Role instance and has lost all of its Pods is replaced as a whole under every policy (reason `AllPodsLost`).

Because the phase of each Role instance is stored in the `status` of the group, recovery does not depend on the controller having seen the Pod disappear. If a Pod of an established instance is lost while the controller manager is down, the policy is applied when the controller manager is back.

## What You May and May Not Do

**Read.** Listing and reading ServingGroups is always safe.

**Do not edit the `spec`.** It is written by the ModelServing controller, which compares the `spec` of every group with the ModelServing and writes the settings back when they differ. A changed `spec.revision` is not written back: the controllers act on it as the revision of the group. To change a group, change the ModelServing.

**Do not edit the `status`.** The controllers decide from it. The chart grants write access to `servinggroups/status` only to the controller manager.

**Do not scale a ServingGroup.** The `scale` subresource exists so that a PodDisruptionBudget can find the expected number of Pods of a group (see [PodDisruptionBudgets and Eviction](#poddisruptionbudgets-and-eviction)). `spec.expectedPods` does not control how many Pods are created, and the ModelServing controller writes its value back. To change the size of a group, change `replicas` or `workerReplicas` of a Role in the ModelServing.

**Deleting a ServingGroup restarts the group.** Use it when you want one group rebuilt, for example a group whose Pods are in a state that the recovery policy tolerates:

```sh
kubectl delete servinggroup gang-0 --cascade=foreground
```

The object stays, marked for deletion, until its Pods, Services and PodGroup are deleted. Then the ModelServing controller creates a ServingGroup with the same name and a new UID, and the group gets new Pods. The new group has the revision that its ordinal has now: the update revision, or the current revision when the ordinal is below `partition` under `ServingGroupRollingUpdate`. Under `RoleRollingUpdate` every Role instance of the new group is created from the update revision, the instances below the `partition` of a Role included. The deletion is not checked against `maxUnavailable`: the group is unavailable until its new Pods are ready.

Without `--cascade=foreground`, `kubectl` removes the object at once and the garbage collector deletes the Pods afterwards. The result is the same. The new ServingGroup creates no Pod while a Pod of the old object exists: it reports `ReconcileFailed` with reason `NameOccupied` until the last of them is gone.

**Deleting with `--cascade=orphan` keeps the Pods.** The ModelServing controller creates the ServingGroup again and the new object adopts the Pods, the Services and the PodGroup that were left behind. Pods are not restarted. There is normally no reason to do this.

**Deleting a Pod triggers `recoveryPolicy`.** Under `RoleRecreate`, deleting one Pod of an established Role instance recreates the whole instance.

## PodDisruptionBudgets and Eviction

A PodDisruptionBudget that uses `maxUnavailable`, or `minAvailable` as a percentage, asks the controller owner of the selected Pods for the expected number of Pods. The owner is the ServingGroup, and its `scale` subresource reports the number of Pods of the complete group.

- A budget that selects all Pods of a ModelServing gets the sum over its groups.
- A budget that selects only the Pods of one Role still gets the number of Pods of the whole group, which is too high. Use an absolute `minAvailable` for such a budget.

The eviction webhook of Kthena (`workload.controllerManager.evictionWebhook.enabled`, off by default) protects the Pods of a ModelServing as before. It recognises Pods that are owned by a ServingGroup of the ModelServing.

## RBAC

The chart installs two ClusterRoles that are aggregated into the default `view`, `edit` and `admin` roles of Kubernetes:

| ClusterRole | Aggregated into | Grants |
|---|---|---|
| `kthena-servinggroup-view` | `view`, `edit`, `admin` | `get`, `list`, `watch` on `servinggroups`, `servinggroups/status` and `servinggroups/scale` |
| `kthena-servinggroup-edit` | `edit`, `admin` | `delete` on `servinggroups` |

Creating and updating ServingGroups, and writing their status, is reserved for the controller manager.

## Related Documentation

- [Upgrading to ServingGroup Objects](./servinggroup-upgrade.md)
- [Model Serving Controller](../architecture/model-serving-controller.mdx)
- [In-place Update](./inplace-update.md)
- [Workload CRD reference](../reference/crd/workload.serving.volcano.sh.md#servinggroup)
