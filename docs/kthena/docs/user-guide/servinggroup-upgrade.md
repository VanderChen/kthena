# Upgrading to ServingGroup Objects

In Kthena v1.1.0 and earlier, the controller keeps the state of every ServingGroup in its memory, and the Pods of a ModelServing are owned by the ModelServing. Versions with [`ServingGroup` objects](./servinggroup.md) keep that state in these objects, and the objects own the Pods.

When you upgrade a cluster that runs ModelServings, the new controller takes the running workloads over in place. This page describes the upgrade step by step, what changes in behaviour, and how to roll back to the earlier version.

It covers upgrades from v1.1.0 and from builds of `main` before this change. Upgrades from older releases were not tested: upgrade to v1.1.0 first.

The commands assume the release name `kthena` and the namespace `kthena-system`, as in the [installation guide](../getting-started/installation.md). `vX.Y.Z` stands for the version you upgrade to.

## What Changes for Running Workloads

- **Pods are not recreated and not restarted.** They keep their names, UIDs and restart counts.
- The controller owner of every Pod changes from the ModelServing to a new `ServingGroup` object with the name of the group, for example `sample-0`. Pods also get the label `modelserving.volcano.sh/instance-generation: "0"`.
- PodGroups and the headless Services of the `headless-service` plugin keep their UIDs. Their owner changes to the ServingGroup as well.
- The `status` of the ModelServing is left as the earlier version wrote it while the ModelServing is taken over: the counts, the conditions and `status.observedGeneration` do not change, and the `Available` condition does not turn `False`. The status is computed again once every group has reported.
- A rolling update that was stopped before the upgrade, for example by `partition`, is still stopped afterwards. A rolling update that was in progress continues after the take-over.

Only metadata is written. The kubelet does not act on owner references, so containers are not affected.

## Before You Upgrade

### Run the Pre-flight Check

The [`kthena` CLI](../reference/kthena-cli.md) of the new version has a check that tells, for the cluster and for every ModelServing, what the upgrade will do. Run it while the cluster still runs the earlier version:

```sh
kthena preflight model-servings --all-namespaces --baseline-file baseline.json
```

`--baseline-file` records the UID and the restart counts of every Pod, the UIDs of PodGroups and headless Services, and the status of every ModelServing. You use the file after the upgrade to verify that nothing was replaced.

Every ModelServing, and the cluster, gets a verdict:

| Verdict | Meaning | Exit code |
|---|---|---|
| `ready` | Can be upgraded without disruption. | 0 |
| `attention` | The upgrade works, and something happens that you should know about. | 2 |
| `wait` | Something is in progress. The finding says what to wait for. | 2 |
| `blocked` | The ModelServing would not be taken over cleanly. The finding says why. | 3 |

Upgrade when nothing is `blocked`, and when you have read every finding with `wait` or `attention`. The findings name the objects and say what to do.

The check changes nothing. It sends one server-side dry-run patch per ModelServing to see whether the annotation that the take-over writes is admitted; `--skip-dry-run` leaves that out. See [`kthena preflight model-servings`](../reference/kthena-cli/kthena_preflight_model-servings.md) for all options.

### Check the Requirements

- **Every ModelServing is reconciled.** `status.observedGeneration` equals `metadata.generation`. The take-over leaves `status.observedGeneration` as it is, so a ModelServing that is behind before the upgrade stays behind until it is taken over:

  ```sh
  kubectl get modelservings -A \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.generation}{"\t"}{.status.observedGeneration}{"\n"}{end}'
  ```

- **No rolling update, scaling or recovery is in progress**, if you can arrange it. The new version continues them, but an upgrade from a steady state is easier to verify. One case needs care: a rolling update with `maxSurge` on a ModelServing whose group ordinals have gaps. The earlier version can create the additional group in a gap below `replicas`, without the surge mark. After the upgrade such a group is a regular group, there is one group more than `replicas`, and one group is scaled in. The pre-flight check reports this as `groups-exceed-replicas`. Let the rolling update finish before you upgrade.
- **The rollback switch does not exist.** This command must report `NotFound`:

  ```sh
  kubectl -n kthena-system get configmap kthena-servinggroup-rollback
  ```

- **Do not change ModelServings and do not drain nodes** from `helm upgrade` until you have verified the upgrade.

### What the Upgrade Does Not Cover

- **No controller works during the switch.** From the moment the controller of the earlier version stops until a ModelServing is taken over, that ModelServing is not scaled, rolled or recovered. The serving Pods keep running.
- **Pods lost during the switch are only created again.** The earlier version did not record which Role instances were complete. A Pod that disappears in this window is created again; its Role instance or group is not recreated, whatever `recoveryPolicy` says. From then on, `recoveryPolicy` also applies to Pods lost while no controller runs.
- **The new controller waits for the old one.** If the controller manager Pod of the earlier version cannot stop, for example on a node that lost contact, the new controller does not start until that Pod is gone.
- **Only its own namespace is checked.** The new version does not notice a controller manager of the earlier version that runs in another namespace, or outside the cluster with other credentials. Make sure there is none.

## Upgrade

### 1. Record the Running Pods

If you did not write a baseline with the pre-flight check, record the Pods yourself:

```sh
snap() {
  kubectl get pods -A -l modelserving.volcano.sh/name \
    -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.uid}{"\t"}{.status.containerStatuses[*].restartCount}{"\n"}{end}' | sort
}
snap > pods-before.txt
```

### 2. Apply the CRDs

`helm upgrade` does not install or update CRDs. Apply the CRDs of the new version first:

```sh
helm show crds oci://ghcr.io/volcano-sh/charts/kthena --version vX.Y.Z \
  | kubectl apply --server-side --force-conflicts -f -
```

`--force-conflicts` lets `kubectl` take over the fields of CRDs that Helm created when the release was installed.

Other ways to get the CRD files:

- From the chart package of a GitHub release:

  ```sh
  curl -L -o kthena.tgz https://github.com/volcano-sh/kthena/releases/download/vX.Y.Z/kthena.tgz
  helm show crds kthena.tgz | kubectl apply --server-side --force-conflicts -f -
  ```

- Only the CRDs of the workload controllers, when the router is not installed:

  ```sh
  helm pull oci://ghcr.io/volcano-sh/charts/kthena --version vX.Y.Z --untar
  kubectl apply --server-side --force-conflicts -f kthena/charts/workload/crds/
  ```

- From a checkout of the repository:

  ```sh
  kubectl apply --server-side --force-conflicts -f charts/kthena/charts/workload/crds/
  ```

Check that the new CRD is there:

```sh
kubectl get crd servinggroups.workload.serving.volcano.sh
```

### 3. Upgrade the Release

```sh
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena --version vX.Y.Z \
  --namespace kthena-system
```

Add the `--set` and `-f` options that you use for this release.

If the `ServingGroup` CRD is not installed, `helm upgrade` fails before it changes anything, with this message:

```text
The CustomResourceDefinition servinggroups.workload.serving.volcano.sh is not installed. helm upgrade does not install new CustomResourceDefinitions; apply the files of charts/kthena/charts/workload/crds/ first (kubectl apply --server-side -f <dir>), then upgrade again.
```

Go back to step 2 and run `helm upgrade` again.

### 4. Watch the Take-over

```sh
kubectl -n kthena-system get pods -l app.kubernetes.io/component=kthena-controller-manager -w
```

This is what happens, in order:

1. **The old controller manager loses its credentials.** The chart runs the controller manager under a new ServiceAccount, `kthena-controller-manager-servinggroup`, and Helm removes the old one. From then on the process of the earlier version cannot write any more. This is intended. A process that runs with leader election cannot renew its lease and exits, so its Pod may show `Error` and a restart; a process without leader election, the default of the earlier versions, stays until the Deployment removes its Pod.
2. **The new Pod becomes ready** and answers the admission webhooks. The Deployment then removes the old Pod.
3. **The new process waits until it holds the leader lease.** If an earlier process held the lease, this takes until that lease expires.
4. **The new process waits while a controller manager Pod of the earlier version exists** in its namespace. While it waits it does not reconcile, and it records an event on its own Pod:

   ```sh
   kubectl -n kthena-system get events --field-selector reason=WaitingForPredecessor
   ```

   The message names the Pod it waits for.
5. **The controllers start.** The log of the leader has the line `ModelServing and ServingGroup controllers started`. With several replicas, look at the log of every Pod:

   ```sh
   kubectl -n kthena-system logs -l app.kubernetes.io/component=kthena-controller-manager --tail=-1 --prefix \
     | grep "controllers started"
   ```

6. **Every ModelServing is taken over.** The controller creates a ServingGroup for every group that has Pods, with the revision of these Pods, and changes the owner of the Pods, the PodGroup and the Services. Progress is recorded in the annotation `modelserving.volcano.sh/servinggroup-adoption` of the ModelServing: `InProgress` while Pods owned by the ModelServing remain, `Done` when every Pod is owned by a ServingGroup and every ServingGroup has reported its status. While the annotation is `InProgress`, the ModelServing is not rolled and not scaled, and its `status` does not move: the counts, the conditions and `status.observedGeneration` are the ones the earlier version wrote. A change that you make to the ModelServing in this time is acted on, and shows in `status.observedGeneration`, after the take-over.

If the controller manager is restarted during the take-over, the next process continues it.

### 5. Verify

1. Every ModelServing is taken over. The third column must be `Done` on every line:

   ```sh
   kubectl get modelservings -A \
     -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.annotations.modelserving\.volcano\.sh/servinggroup-adoption}{"\n"}{end}'
   ```

2. ServingGroups exist, one for every group of every ModelServing:

   ```sh
   kubectl get servinggroups -A
   ```

3. Every Pod is owned by a ServingGroup. This command must print nothing:

   ```sh
   kubectl get pods -A -l modelserving.volcano.sh/name \
     -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.ownerReferences[?(@.controller==true)].kind}{"\n"}{end}' \
     | grep -v 'ServingGroup$'
   ```

4. No Pod was replaced or restarted. With the baseline of the pre-flight check:

   ```sh
   kthena preflight model-servings --compare-baseline baseline.json
   ```

   Exit code 0 means that no Pod, PodGroup or Service was replaced, no container restarted, `status.availableReplicas` did not drop and every ModelServing is taken over. Exit code 3 means that one of these does not hold. Exit code 2 means that the only changes are ones that the pre-flight check announced, or that a change of a ModelServing since then explains. Or with the `snap` function of step 1:

   ```sh
   snap > pods-after.txt
   diff pods-before.txt pods-after.txt && echo "no Pod was replaced or restarted"
   ```

5. The controller manager runs the new image under the new ServiceAccount, and no Pod of the earlier version is left:

   ```sh
   kubectl -n kthena-system get pods -l app.kubernetes.io/component=kthena-controller-manager \
     -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.serviceAccountName}{"\t"}{.spec.containers[0].image}{"\n"}{end}'
   ```

### Upgrading Without Helm Access to the Cluster

The check of step 3 needs Helm to read the cluster. It does not run when the chart is rendered without cluster access: `helm template | kubectl apply`, most GitOps tools, or the release manifest `kthena-install.yaml`. Apply the CRDs first in these setups too. The release manifest contains them.

If the `ServingGroup` CRD is missing in such a setup:

- The new controller manager Pod stays not ready. Its readiness probe, `/readyz`, returns 503 until ServingGroups can be listed. It starts no controller.
- The Deployment keeps the Pod of the earlier version, but that process has already lost its access to the API: the roles are bound to the new ServiceAccount.
- So no controller works. The serving Pods keep running; they are not scaled, rolled or recovered.

Apply the CRDs. The new process tries again every five seconds, becomes ready and continues as described above. It does not need a restart.

## After the Upgrade

- **Leader election is on by default** (`--leader-elect`, default `true`). A process that loses the lease exits and its Pod restarts; a restart of the controller manager Pod for this reason is expected. The lease is `lease.kthena.controller-manager` in the namespace of the controller manager.
- **Run two replicas for availability.** The chart default is still one replica. Every replica answers the webhooks and one of them runs the controllers. With one replica, nothing answers the Kthena admission webhooks while the Pod restarts. Set `workload.controllerManager.replicas` to `2` in your values, or add `--set workload.controllerManager.replicas=2` to the `helm upgrade` command.
- **The API client limits are higher.** The default is 50 queries per second with a burst of 100. Change them with `--kube-api-qps` and `--kube-api-burst`, or with the chart values `workload.controllerManager.kubeAPIQPS` and `workload.controllerManager.kubeAPIBurst`.
- **`--workers` counts per controller.** The ModelServing controller and the ServingGroup controller each run that many workers (default 5).
- **The ServiceAccount has a new name**, `kthena-controller-manager-servinggroup`. If you granted the old ServiceAccount, `kthena-controller-manager`, permissions of your own, grant them to the new one.
- **The Pod template of the controller manager must carry the label `modelserving.volcano.sh/controller-protocol: servinggroup`.** The chart and the release manifest set it. If you deploy the controller manager from manifests of your own, put the label on the Pod template of the Deployment. A controller manager Pod without the label counts as a Pod of the earlier version, and the leader exits when it sees one. Every process also puts the label on its own Pod when it starts. Without the label on the template, a new replica has no label until its process has started, and the leader exits and waits each time the Deployment creates a Pod.

## Behaviour Changes

Names of groups and Pods, the ModelServing API, the definitions of the counts and of the `Available`, `Progressing` and `UpdateInProgress` conditions in the status of the ModelServing, the meaning of `partition`, `maxUnavailable` and `maxSurge`, and what counts as a failed Pod are unchanged. `recoveryPolicy: None` and `restartGracePeriodSeconds: -1` keep their meaning. Pods and headless Services keep their labels and get one more, `modelserving.volcano.sh/instance-generation`.

The tables below list what changes.

### Recovery

| Before | Now | Why |
|---|---|---|
| A Pod that was lost while no controller observed it (restart, upgrade, change of the leader) was only created again. | If its Role instance was established, the loss is handled by `recoveryPolicy`: the Role instance or the group is recreated. Exception: Pods lost during the first upgrade to this version. | Each ServingGroup records in `status.roleInstances` which Role instances were complete. The record does not depend on the controller process. |
| A Pod deleted from a Role instance that was still being created triggered `recoveryPolicy`. | The missing Pod is created. Nothing else is deleted. | An instance that was never complete has nothing to recover. |
| A deletion of a group or of a Role instance that had started was forgotten when the controller restarted. The remaining Pods stayed and the gap was filled. | The next controller process completes the deletion. | The decision is written to the ServingGroup object before the first Pod is deleted. |
| A recovery that waited for `restartGracePeriodSeconds` used the policy and the grace period from the moment the fault was seen. | The policy and the grace period are read when the recovery starts. Changing `recoveryPolicy` to `None`, setting the grace period to `-1` or raising it cancels a recovery that has not started. A recovery that has started (`Failed` on the group, `Draining` on the instance) is completed. | The controller decides from the objects as they are now, not from values it captured earlier. |

### Rolling Update and Scaling

The new controller handles the following cases differently from the earlier one. See [Rollout Strategy](../developer-guide/model-serving-rolling-update.md) for the rules of a rolling update.

| Before | Now | Why |
|---|---|---|
| A group protected by `partition` that was not ready did not reduce the rollout budget. | It does: a rolling update does not take the number of ready groups below `replicas - maxUnavailable`, whichever group is not ready. | `maxUnavailable` is a limit for the whole ModelServing. |
| With gaps in the group ordinals, the additional group of a `maxSurge` rollout could be created in a gap below `replicas`, without the surge mark. | Surge groups are always created at ordinals at or above `replicas`. The ServingGroup carries the annotation `modelserving.volcano.sh/surge`, and its Pods carry a copy. When `replicas` grows to include the group, the annotation is removed from the ServingGroup and then from its Pods. | A surge group is temporary and has to be recognisable to be removed. |
| Reducing `replicas` during a `maxSurge` rollout could remove a surge group. | Scale-down only selects regular groups. Surge groups are removed when the rollout ends. | Surge capacity belongs to the rollout, not to `replicas`. |
| Replacing outdated groups and scaling down ran at the same time. | No group is replaced while a group removed by a scale-down still exists. | A group that is being deleted still counts until it is gone. |
| `status.currentRevision` advanced when every group was updated and available. | It advances only when the rollout is complete: `partition` is 0 (under `RoleRollingUpdate`: no Role has a `partition`), `replicas` is greater than 0, exactly `replicas` groups exist and none of them is a surge group, every group runs the new revision and is ready, and no group has all its Roles at zero replicas. | A partition, a surge group that is still there, or a group without Pods does not show that the new revision works. |
| Under `RoleRollingUpdate` with gang scheduling, when the revision that a Role instance runs could not be read, the PodGroup of the group was not updated and the rest of the reconcile went on. | The PodGroup is not updated, and the group creates and updates no Pod until the revision can be read. It still deletes what it has to delete. The ServingGroup reports `ReconcileFailed`; the message names the Role instance. | No Pod is created for a PodGroup whose layout is not known. |

Under `RoleRollingUpdate`, the Role instances inside a group are planned by the same rules as the groups of a ModelServing, with `replicas`, `maxUnavailable`, `maxSurge` and `partition` of the Role in the place of those of the ModelServing. The first four rows of the table therefore hold for Role instances as well:

- A Role instance below the `partition` of its Role that is not ready uses the `maxUnavailable` of the Role.
- Surge instances are created at indexes at or above the `replicas` of the Role, and their Pods carry the annotation `modelserving.volcano.sh/surge: role`.
- A scale-down of a Role does not select a surge instance.
- No Role instance is replaced while an instance removed by a scale-down still exists.

The earlier version had separate code for Roles. In these four cases its behaviour can differ from what is described here.

### Operation

| Before | Now | Why |
|---|---|---|
| Pods, PodGroups and headless Services were owned by the ModelServing. | They are owned by a ServingGroup, which is owned by the ModelServing. The labels they had are unchanged; Pods and headless Services get one more, `modelserving.volcano.sh/instance-generation`. | See [ServingGroup Objects](./servinggroup.md). |
| Replacing a whole group deleted its Pods, its PodGroup and its headless Services and created them again. There was no object for the group. | The same happens, and the ServingGroup object is deleted and created again with them: same name, new UID, new age. The Pods, the PodGroup and the Services are deleted by the garbage collector. | They are owned by the ServingGroup and deleted with it. |
| Leader election was off by default. A process that lost the lease kept running. | Leader election is on by default. A process that loses the lease exits. | Two controller processes must not reconcile at the same time. |
| A PodDisruptionBudget with `maxUnavailable`, or with `minAvailable` as a percentage, took the expected number from the scale of the ModelServing, which counts groups. | It takes it from the scale of the ServingGroups, which counts Pods. Check the allowed disruptions of such budgets after the upgrade. See [PodDisruptionBudgets and Eviction](./servinggroup.md#poddisruptionbudgets-and-eviction). | The controller owner of the Pods is the ServingGroup. |
| No condition said why a rollout does not proceed. | The ModelServing gets the condition `RolloutBlocked` with a reason. See [When a Rollout Waits](./servinggroup.md#when-a-rollout-waits). Once a ModelServing had the condition, it keeps it with status `False` and reason `NotBlocked` when the rollout goes on. A ModelServing that was never blocked does not have the condition. | |
| The API client used 5 queries per second with a burst of 10. | 50 and 100. | The controllers write the status of every group and read from the API server before they delete or create. |
| The controller manager ran under the ServiceAccount `kthena-controller-manager`. | `kthena-controller-manager-servinggroup`. | A process of the earlier version that is still alive loses its credentials. |
| Plugins: `OnRoleDelete` was the place to remove the resources of a Role instance. While it failed, the whole ModelServing was not reconciled. | It is still called when a Role instance is removed or recreated. While it fails, only that Role instance is held up; its ServingGroup reports `DrainBlocked`. When a whole group is deleted, the hook is not guaranteed to run: resources that a plugin creates must be controlled by the ServingGroup (`HookRequest.Owner`), so that they are deleted with it. | A deleted group is cleaned up by the garbage collector. |
| Plugins: `OnModelServingSync` ran once in every reconcile of the ModelServing. `OnPodReady` ran when the controller saw a Pod become ready. | `OnModelServingSync` also runs once in every round of every ServingGroup. `OnPodReady` runs once for a Pod in a controller process, and again after a restart of the controller manager or a change of the leader. See [Hooks and ServingGroup Objects](./modelserving-plugin-framework.md#hooks-and-servinggroup-objects). | The Pods are managed by the ServingGroup controller, which asks the hooks as well. |
| A change of the annotation `scheduling.volcano.sh/queue-name` of a ModelServing did not start a reconcile. Like every update that left `spec` unchanged, it took effect with the next reconcile. | Every update of a ModelServing starts a round, also one that only changes labels or annotations. A new queue name reaches the PodGroups at once. | The queue is part of the `spec` of every ServingGroup. |
| Removing `networkTopology` from a ModelServing without `gangPolicy.minRoleReplicas` deleted all its PodGroups. They were created again afterwards. | The PodGroups are updated in place and keep their UIDs. | The PodGroup of a group is brought in line with the group in every round. |
| Users with the `edit` or `admin` role in a namespace could delete the Pods of a group. | They may also delete ServingGroups, which restarts all Pods of the group. See [RBAC](./servinggroup.md#rbac). | Deleting a ServingGroup is the way to have one group rebuilt. |
| | `kubectl scale servinggroup` is accepted from a user who may update `servinggroups/scale`, and has no lasting effect: the ModelServing controller writes `spec.expectedPods` back. See [What You May and May Not Do](./servinggroup.md#what-you-may-and-may-not-do). | The `scale` subresource exists for PodDisruptionBudgets. |
| With `--debug-port`, the path `/debug/modelserving/cache` returned the store that the controller kept in memory. | The path returns the ServingGroups of every ModelServing as the cache of the controllers holds them: name, UID, revisions, generations, conditions, Role instances and Pods. The JSON has another shape. | The controllers keep no store of their own. |

### Status, Events and API

| Before | Now | Why |
|---|---|---|
| `status.observedGeneration` equal to `metadata.generation` meant that the controller had already created or deleted the Pods for that generation. | It means that the ModelServing controller has seen the generation and handed the settings down to the ServingGroups. The groups act on them afterwards. After a change that is not a new revision, for example of the `replicas` of a Role, the ModelServing can still show `Available` with full counts for a moment. Wait for what you changed: for a new template, `status.updatedReplicas` and the `Available` condition; for any other change, the ServingGroups. Each ServingGroup reports the `spec` it acted on in `status.observedGeneration` and whether it is complete in the `Ready` condition. | Pods are created and deleted by the ServingGroup controller, after the ModelServing controller has written the `spec` of the group. |
| `status.availableReplicas` was computed from the Pods in the same reconcile. | It counts the ServingGroups whose `Ready` condition is `True`. It follows a change of a Pod once the ServingGroup controller has written the status of the group. | Whether a group is ready is decided in one place, the ServingGroup. |
| After a restart of the controller manager, `RoleRunning` was recorded again for every Role instance. | `RoleCreating`, `RoleRunning` and `RoleDeleting` keep their meaning and are recorded on the ModelServing when a Role instance enters the state. A new controller process does not record `RoleRunning` again for the instances it finds running. | The controller process remembers what it reported last. |
| The warning event `DeletionIdentityChanged` was recorded when a Pod had been replaced under the same name while its group or Role instance was being deleted. | The event does not exist any more. | Every deletion applies only to the Pod that was read. A Pod that took its name is not touched. |
| | New events on the ModelServing: `ServingGroupCreated`, `ServingGroupDeleted`, `OrdinalOccupied`, `PodNotTakenOver`, `RolledBack`. New events on the ServingGroup: `OwnerNotFound`, `StatusNotPersisted`. `PluginError`, `RevisionUnresolved` and the in-place update events are still recorded on the ModelServing. See [Events](./servinggroup.md#events). | |
| Every update of a ModelServing was validated by the admission webhook. | An update that leaves `spec` unchanged, for example of labels or annotations, is admitted without validating the object. | The controller writes an annotation to every ModelServing, also to one that was stored before a validation rule existed. |
| The Go type of `spec.template` in `pkg/apis/workload/v1alpha1` was `ServingGroup`. | It is `ServingGroupTemplate`; `ServingGroup` is the type of the new resource. The JSON and YAML form of a ModelServing is unchanged. Go code that imports the package has to follow the rename. | The kind needs the name. |

## Rolling Back

`helm rollback` alone is not enough. The earlier version recognises the Pods of a ModelServing by their owner. Pods owned by a ServingGroup are foreign to it, so it would not manage the running workloads.

The new version therefore has a rollback mode that hands the workloads back first. Like the upgrade, it does not restart any Pod.

### 1. Turn On Rollback Mode

Create the switch, a ConfigMap in the namespace of the controller manager. Its content does not matter:

```sh
kubectl -n kthena-system create configmap kthena-servinggroup-rollback
```

While this ConfigMap exists, the controllers of the new version do not reconcile. For every ModelServing they:

1. Wait for ServingGroups that are being deleted.
2. Set the annotation `modelserving.volcano.sh/servinggroup-adoption` to `RollingBack`.
3. Change the owner of the Pods, the Services and the PodGroup of every group back to the ModelServing.
4. Delete the ServingGroup objects without deleting their Pods.
5. Remove the annotation and the `RolloutBlocked` condition from the ModelServing.

Do not create or change ModelServings while the ConfigMap exists: nothing is scaled, rolled or recovered, and the status of the ModelServings is not updated.

### 2. Wait Until Everything Is Handed Back

```sh
# No ServingGroup is left
kubectl get servinggroups -A

# No ModelServing carries the annotation: the third column is empty on every line
kubectl get modelservings -A \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.annotations.modelserving\.volcano\.sh/servinggroup-adoption}{"\n"}{end}'

# Every ModelServing that was handed back has a RolledBack event
kubectl get events -A --field-selector reason=RolledBack

# The switch carries a RollbackMode event
kubectl -n kthena-system get events --field-selector reason=RollbackMode
```

Every Pod is owned by its ModelServing again. Pods and headless Services keep the label `modelserving.volcano.sh/instance-generation`; the earlier version ignores it. This command must print nothing:

```sh
kubectl get pods -A -l modelserving.volcano.sh/name \
  -o jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\t"}{.metadata.ownerReferences[?(@.controller==true)].kind}{"\n"}{end}' \
  | grep -v 'ModelServing$'
```

### 3. Install the Earlier Version

Install the chart of the earlier version, with the values you used for it:

```sh
helm upgrade kthena oci://ghcr.io/volcano-sh/charts/kthena --version v1.1.0 \
  --namespace kthena-system
```

`helm rollback kthena REVISION --namespace kthena-system` does the same; `helm history kthena --namespace kthena-system` lists the revisions.

Helm restores the old ServiceAccount and removes the new one. The process of the new version loses its credentials, cannot renew its lease and exits; its Pod may show `Error` before it is replaced. The earlier version starts and manages the Pods as before.

Leave the CRDs as they are. The earlier version works with the newer `ModelServing` CRD and ignores the `ServingGroup` CRD.

### 4. Remove the Switch Before You Upgrade Again

The earlier version does not read the ConfigMap. Delete it once the earlier version runs, and in any case before you upgrade again:

```sh
kubectl -n kthena-system delete configmap kthena-servinggroup-rollback
```

If the ConfigMap is still there when the new version starts, the new version takes nothing over and reconciles nothing. This is safe, but the ModelServings are not managed. The new version says so in its log and with a `RollbackMode` event on the ConfigMap. Deleting the ConfigMap starts the take-over.

### Abandoning a Rollback

To stay on the new version after all, delete the ConfigMap before you install the earlier version. The new version takes the workloads over again, as in the upgrade.

## Troubleshooting

| Symptom | Cause | What to do |
|---|---|---|
| `helm upgrade` fails with "The CustomResourceDefinition servinggroups.workload.serving.volcano.sh is not installed". | The CRDs of the new version were not applied. Nothing was changed. | [Apply the CRDs](#2-apply-the-crds) and run `helm upgrade` again. |
| The controller manager Pod that is being replaced shows `Error` or a restart, during the upgrade or the rollback. | Its ServiceAccount was replaced; the process lost its credentials and, with leader election, exited. | Nothing. The Deployment removes the Pod. |
| The new controller manager Pod stays not ready; its readiness probe fails with status 503. Its log says `ServingGroups cannot be listed`. | The `ServingGroup` CRD is not installed, or the controller manager has no permission to list ServingGroups. | [Apply the CRDs](#2-apply-the-crds). If the manifests were applied by hand, make sure the ClusterRole of the new version was applied. The Pod becomes ready by itself. |
| The new Pod is ready, but no ServingGroup appears. The Pod has a `WaitingForPredecessor` event. | A controller manager Pod of the earlier version still exists in the namespace. | The event names the Pod. The controllers start when it is gone. On a node that lost contact, the Pod stays until the node is back or the Pod is removed. |
| The new controller manager Pod restarts repeatedly. Its log says `A controller manager of an earlier version runs next to this one`. | A Pod of the earlier version appeared while the new version was working, for example because the Deployment was rolled back without the [rollback procedure](#rolling-back). | Remove the Pod of the earlier version, or follow the rollback procedure. |
| The new version runs and takes nothing over. The ConfigMap `kthena-servinggroup-rollback` exists and has a `RollbackMode` event. | The rollback switch was left in place. | Delete the ConfigMap. |
| The annotation of a ModelServing stays `InProgress`. | A Pod owned by the ModelServing is not taken over, or a ServingGroup does not report. | Run the owner check of [step 5](#5-verify) to find Pods that are still owned by the ModelServing. A Pod whose name is not the name of a member, `<group>-<role>-<instance>-<member>`, is never taken over: delete it, or remove its owner reference. Then look at the conditions of the ServingGroups (`ReconcileFailed`) and at the log of the controller manager. |
| A ServingGroup has `ReconcileFailed` with reason `NameOccupied`. | A Pod with the name of a member exists and does not belong to the group. Or Pods of an earlier ServingGroup object of the same name still exist, after that object was deleted in the background, which is the default of `kubectl delete`; the message says how many. | Remove the Pod that holds the name. Pods of an earlier object are deleted by the garbage collector: wait. See [Conditions](./servinggroup.md#conditions). |
| During the upgrade, `status.observedGeneration` of a ModelServing stays below `metadata.generation`, or its conditions do not change. | The status of a ModelServing is not written while it is taken over. | Wait until the annotation is `Done`. See [step 4](#4-watch-the-take-over). |
| The rollback does not finish: ServingGroups remain. | A ServingGroup that is being deleted cannot be handed back; the rollback waits until it is gone. Another cause is an error while changing owners. | Find the Pod of that group that does not terminate. Look for `hand back ServingGroup` in the log of the controller manager. |
| After the upgrade a rolling update does not continue. | See the `RolloutBlocked` condition of the ModelServing. | [When a Rollout Waits](./servinggroup.md#when-a-rollout-waits). |

## Related Documentation

- [ServingGroup Objects](./servinggroup.md)
- [Installation](../getting-started/installation.md)
- [`kthena preflight model-servings`](../reference/kthena-cli/kthena_preflight_model-servings.md)
- [Workload CRD reference](../reference/crd/workload.serving.volcano.sh.md#servinggroup)
