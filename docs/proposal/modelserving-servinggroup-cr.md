---
title: ServingGroup as a first-class API object
authors:
- "@LiZhenCheng9527"
reviewers:
- TBD
approvers:
- TBD

creation-date: 2026-11-25

---

## ServingGroup as a first-class API object

### Summary

A `ModelServing` runs `replicas` copies of a *ServingGroup*. A ServingGroup contains Role instances, and a Role
instance contains one entry Pod and its worker Pods. Rolling updates and failure recovery act on a whole
ServingGroup or on a whole Role instance, never on a single Pod.

Today neither unit exists in the API. A ServingGroup is a set of Pods that share the label
`modelserving.volcano.sh/group-name`. Every fact about the unit as a whole — "this group is being deleted", "this
Role instance was complete once", "this group is temporary surge capacity" — is kept in the controller's memory or
spread over the Pods of the unit.

This proposal makes ServingGroup a namespaced custom resource, `servinggroups.workload.serving.volcano.sh`:

- A ServingGroup object is named `<ModelServing name>-<ordinal>`, exactly as the logical group is named today. It is
  owned by the ModelServing and owns the Pods, the PodGroup and the headless Services of the group.
- Facts about a unit are written on the object of that unit: the group's revision in `spec`, the surge mark as an
  annotation, "this group must be replaced" as a status condition, and "this Role instance was established / is being
  drained" in a small per-instance ledger in `status`.
- The work is split between two controllers in the same process, in the way Deployment and ReplicaSet split it. The
  ModelServing controller decides how many groups exist, at which ordinals and at which revision. A new ServingGroup
  controller reconciles what is inside one group.
- The rollout rules do not change. Group names, Pod names, the meaning of `partition`, `maxUnavailable` and
  `maxSurge`, and the ordering of a rollout stay as they are.
- Workloads that are already running are taken over in place when the controller is upgraded. No Pod is restarted or
  recreated.

For users of ModelServing this is a refactoring of the implementation with a small, listed set of behaviour changes
in recovery. Its purpose is that the controller behaves the same before and after a restart, a leader change or an
upgrade.

### Motivation

#### The unit of recovery has no object

`recoveryPolicy: RoleRecreate` means: when a member of a Role instance is lost, recreate the whole instance.
`ServingGroupRecreate` means the same for the whole group. Both exist because the members of a unit are not
independent. An entry Pod and its workers hold connections to each other, and a replacement worker cannot join a
running entry.

To apply these policies the controller must tell two situations apart:

| Observation | Situation | Correct reaction |
| --- | --- | --- |
| The Pod `llama-0-prefill-0-1` does not exist | The instance is still being created; this Pod has not been created yet | Create the Pod |
| The Pod `llama-0-prefill-0-1` does not exist | The instance was complete and this Pod was deleted | Delete the rest of the instance and recreate it |

The Pods that exist are the same in both rows. What differs is whether the instance was complete before, and that is
a fact about the instance. An instance is not an object, so the fact has nowhere to be recorded.

The controller therefore decides from two sources that do not survive a restart
(`pkg/model-serving-controller/controller/model_serving_controller.go`):

- **Events.** Recovery runs in the informer callbacks. `deletePod` calls `handleDeletedPod`, which deletes the whole
  Role or group. If the delete event is not delivered to this process — the controller was restarting, or another
  replica was the leader — the reconcile loop later sees a gap and fills it with one Pod.
- **Memory.** The in-memory `datastore.Store` holds a state per group (`Creating`, `Running`, `Scaling`, `Deleting`)
  and per Role instance. At start-up `syncAll` rebuilds it by replaying every Pod as an add event. Pods that are
  failed or terminating are not registered, so a unit that consists only of such Pods is absent from the store in
  the first round.

The same shape repeats for every scope-level fact:

| Fact | Where it is today | What happens on restart or leader change |
| --- | --- | --- |
| This Role instance was complete once | Store state `RoleRunning` | Rebuilt from the Pods that are Ready at that moment |
| This group is being deleted on purpose | Store state `ServingGroupDeleting`; the UIDs selected for deletion in `podDeletions` (a `sync.Map`) | Lost. A deletion that was half done is not continued; the remaining Pods are treated as a group with missing members and the gap is filled |
| A member has been restarting since time *t* | `graceMap` and a sleeping goroutine (`handlePodAfterGraceTime`) | Lost. The grace period starts again |
| This group is temporary surge capacity | Annotation `modelserving.volcano.sh/surge` on each Pod of the group, written Pod by Pod | Kept, but can be present on only some of the Pods of a group, and is absent when the group has no Pod |
| Which revision this group runs | Label on each Pod; store field | Rebuilt from Pods; ambiguous when the Pods disagree |

#### Consequences

1. **The same cluster state leads to different actions.** Whether a lost member triggers a scoped recovery depends
   on whether this process received the delete event. A group that is half way through a deliberate deletion is
   completed by the process that started it and repaired by any other process. The outcome depends on when the
   restart happened.

2. **Losses during a controller outage are under-recovered.** A node is drained while the controller is being
   upgraded, or while leadership moves to another replica. A worker of an established instance is deleted. The new
   process fills in one Pod. The guarantee that `RoleRecreate` and `ServingGroupRecreate` promise does not hold in
   exactly the window in which nodes are most often drained. Upgrades and leader changes are part of normal
   operation and must not weaken a recovery policy.

3. **Marks spread over Pods are not atomic.** The surge mark is written on every Pod of a group. A group can be
   half marked. A group whose Pods were all replaced has lost its mark and has become a regular group. Each rule
   that reads the mark needs a further rule for "only some Pods carry it".

4. **Informer callbacks do the work of the reconciler.** `updatePod` and `deletePod` read and write the store and
   call the API server. The reconciler does the same from worker goroutines. The design is edge-triggered where it
   should be level-triggered, and every new feature has to be correct in both places.

5. **One queue key for everything.** The work queue is keyed by ModelServing. Recovery in one group, a Role scale-out
   in another and the rollout of a third are serialized.

6. **A group cannot be inspected.** To learn the state of a group one collects labels and annotations from its
   Pods and reads the controller log. No object says that a group is being replaced or why a rollout is waiting.

Rolling updates with `maxSurge`, `partition`, in-place update and Role coordination have each added state of this
kind. Every addition increases the number of cases that behave differently after a restart, and each case has so far
been fixed on its own.

#### Why an object, and not another place

| Place for scope-level facts | Problem |
| --- | --- |
| Controller memory | Lost on restart; shared between callbacks and workers |
| A mark on every Pod of the scope | Cannot be written atomically; undefined when only some Pods carry it or the scope has no Pod |
| A private ConfigMap per ModelServing | A second table beside the objects that must be kept consistent with them |
| A finalizer on each Pod | Deterministic, but a protected Pod that is deleted while the controller is away stays terminating, and with it the ModelServing and the namespace |
| **An object for the scope** | One conditional write records a fact. The cost is one more CRD and a second controller |

Kubernetes solved the same problem with the same means: a Deployment does not track Pods; it manages ReplicaSet
objects and reads their `spec` and `status`.

#### Goals

- Every fact that a decision depends on is stored on an API object. Two processes that read the same objects decide
  the same writes. The exceptions are listed in [Decisions take objects as their only input](#decisions-take-objects-as-their-only-input).
- A lost member of an established Role instance is recovered according to `recoveryPolicy`, whether or not a
  controller was running when the member was lost.
- A deliberate deletion or a recovery that was started is completed by whichever process comes next, with the same
  scope.
- Group-level rollout behaviour is unchanged: names, absolute-ordinal `partition`, temporary surge groups, promotion
  of a surge group when `replicas` grows, reclaim at the end, filling the lowest free ordinal.
- A ServingGroup is visible: `kubectl get servinggroups` shows revision and readiness, and `kubectl describe` shows
  conditions and the per-instance ledger.
- Different groups of one ModelServing are reconciled in parallel.
- Running workloads are taken over without restarting or recreating a Pod when the controller is upgraded, and can
  be handed back to the previous version in a controlled way.

#### Non-Goals

- Changing the rollout semantics of ModelServing, or any field of the ModelServing `spec`.
- Making a Role instance an object. Only ServingGroup becomes a CR.
- Random group names. Ordinals are kept.
- A user-managed ServingGroup. The objects are created, updated and deleted by the ModelServing controller. Users
  read them; a manual edit of `spec` is overwritten.
- Persisting the start of a restart-failure grace period. It stays in memory, as today.
- Speeding up rollouts. Parallelism between groups is a property of the design; no throughput is promised.

### Proposal

```text
ModelServing ──owns──► ServingGroup ──owns──► Pod, PodGroup, headless Service
     │                      │
     │ ModelServing         │ ServingGroup
     │ controller           │ controller
     ▼                      ▼
 number of groups,        Role instances, member Pods,
 ordinals, revisions,     recovery, in-place update,
 order of a rollout       PodGroup and Services
```

- The **ModelServing controller** creates and deletes ServingGroup objects, writes their `spec`, and aggregates
  their `status` into the ModelServing `status`. It does not create or delete Pods.
- The **ServingGroup controller** creates and deletes the Pods, the PodGroup and the Services of one group, runs
  Role-level scaling, `RoleRollingUpdate`, Role coordination, recovery and in-place update inside the group, calls
  the plugin hooks, and writes the group's `status`. It never creates or deletes a ServingGroup; when a group must
  be replaced as a whole it sets the condition `Failed` and the ModelServing controller deletes the object.
- Both controllers run in the same process under the same leader lease and share their informers.

#### User Stories

##### Story 1: a node drain during a controller upgrade

A cluster operator upgrades Kthena and drains a GPU node in the same maintenance window. A ModelServing with
`recoveryPolicy: RoleRecreate` has a Role instance with one entry and three workers; one worker is on the drained
node. The controller is not running when the worker is evicted.

Today the new controller process creates one worker. The entry never learns about it and the instance serves with
three workers or not at all.

With this proposal the ledger of the group says the instance is `Established`. The new process sees an established
instance with a missing member, confirms it with a direct read, and recreates the whole instance.

##### Story 2: a leader change in the middle of a group replacement

A group is being replaced during a rolling update. The controller has deleted two of its four Pods when its leader
lease is lost.

Today the next leader sees a group with two missing Pods. It has no record that the group was being deleted and
recreates the two Pods at the old revision.

With this proposal the ServingGroup object has a `deletionTimestamp`. The garbage collector finishes deleting its
Pods without Kthena. The next leader sees an ordinal that is still occupied, waits until the object is gone, and
creates the replacement at the target revision.

##### Story 3: looking at a rollout

```console
$ kubectl get servinggroups -l modelserving.volcano.sh/name=llama
NAME      REVISION     READY   PODS   EXPECTED   AGE
llama-0   5c4d8f6b7    True    6      6          3d
llama-1   5c4d8f6b7    True    6      6          3d
llama-2   6b7f9c5d4    False   4      6          40s
llama-3   6b7f9c5d4    True    6      6          5m
```

`llama-3` carries the annotation `modelserving.volcano.sh/surge: serving-group`: it is temporary capacity and will
be reclaimed when the rollout ends. When a rollout cannot proceed, the ModelServing has a condition `RolloutBlocked`
whose reason says what it is waiting for.

##### Story 4: upgrading a cluster that serves traffic

An operator installs the new CRD and runs `helm upgrade`. The new controller waits until the old controller Pod is
gone, creates a ServingGroup object for every running group and changes the controller owner of the group's Pods
from the ModelServing to that object. Pod UIDs, container restart counts, host names and DNS records are unchanged.
A ModelServing whose `spec` nobody changed has no Pod replaced.

#### Notes/Constraints/Caveats

- **Replacement keeps the name.** Replacing a group, and recovering a whole group, is "delete the object, wait until
  it is gone, create an object with the same name". The name is the position; the UID is the generation. While the
  old object exists the ordinal cannot be rebuilt. A group stuck in termination (a foreign finalizer, a stopped
  garbage collector) blocks its ordinal, and the ModelServing reports `RolloutBlocked` with reason
  `WaitingForDeletion`.
- **Deleting a group depends on the garbage collector.** A group is deleted with foreground propagation. Kthena
  does not delete the Pods of a group that is being deleted.
- **`status` is an input.** The ledger cannot be rebuilt from observation. Its zero value is the state in which
  nothing is deleted, and write access to `servinggroups/status` is limited to the controller.
- **Leader election becomes the default, and a process that loses the lease exits.** Every write carries a
  precondition, so two processes cannot create a group twice. Two processes that both reconcile can still spend the
  same disruption budget twice.
- **The Go type `ServingGroup` in `pkg/apis/workload/v1alpha1` is renamed** to `ServingGroupTemplate`, because the
  kind needs the name. The JSON form of ModelServing is unchanged. Go code outside this repository that imports the
  type must follow the rename.
- **Writes.** A group's `status` is rewritten when a field of a member Pod that the controller reads changes
  (deletion, phase, readiness, restart counts). Nothing is written while the Pods do not change.

#### Risks and Mitigations

| Risk | Mitigation |
| --- | --- |
| The old and the new controller run at the same time during the upgrade. The old controller treats a Pod whose owner was changed as deleted and applies `recoveryPolicy` to the rest of its unit | The new version does not start its controllers while a controller-manager Pod of an older version exists in its namespace; it refuses to become ready when the ServingGroup CRD or its RBAC is missing; it uses a new ServiceAccount. See [Upgrade](#upgrade-and-rollback) |
| `status` is cleared or rewritten by a client | The zero value of the ledger leads to "create what is missing", never to a deletion. `Failed` is acted on only together with `Drained` and a status that is fresh |
| A group object is deleted by a user | Background or foreground deletion: the group comes back under the same name, as a deleted StatefulSet Pod does. Orphan deletion: the ModelServing controller recreates the object and the ServingGroup controller adopts the Pods |
| More objects and more writes to the API server | One object per group; `spec` holds no Pod template; `status` holds a few values per Role instance. Rounds without work do no direct reads |
| The change is large | The group-level plan is a pure function tested against a reference model of the rules; every transition has a unit test at each point where the process can stop; conformance scenarios are run before and after on the same cluster |

### Design Details

#### API

```go
// ServingGroup is one replica of a ModelServing.
// +kubebuilder:resource:shortName=sg
// +kubebuilder:subresource:status
// +kubebuilder:subresource:scale:specpath=.spec.expectedPods,statuspath=.status.pods,selectorpath=.status.labelSelector
type ServingGroup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ServingGroupSpec   `json:"spec,omitempty"`
	Status            ServingGroupStatus `json:"status,omitempty"`
}

type ServingGroupSpec struct {
	// Revision is the revision this group runs as a whole, as it appears in the
	// revision label of a Pod. Set under ServingGroupRollingUpdate, empty under
	// RoleRollingUpdate.
	Revision string `json:"revision,omitempty"`
	// TargetRevision is the revision the Role instances of this group roll to under RoleRollingUpdate.
	TargetRevision string `json:"targetRevision,omitempty"`
	// BaselineRevision is a copy of ModelServing.status.currentRevision. Role instances
	// protected by a Role partition are created from it.
	BaselineRevision string `json:"baselineRevision,omitempty"`
	// SourceGeneration is the generation of the ModelServing this spec was derived from.
	SourceGeneration int64 `json:"sourceGeneration,omitempty"`
	// ExpectedPods is the number of Pods of a complete group.
	ExpectedPods int32 `json:"expectedPods"`

	// RolloutStrategy carries the rollout type, the update method that takes effect,
	// the in-place progress deadline and the Role coordination settings.
	RolloutStrategy           *ServingGroupRolloutStrategy `json:"rolloutStrategy,omitempty"`
	RecoveryPolicy            RecoveryPolicy               `json:"recoveryPolicy,omitempty"`
	RestartGracePeriodSeconds *int64                       `json:"restartGracePeriodSeconds,omitempty"`
	GangPolicy                *GangPolicy                  `json:"gangPolicy,omitempty"`
	NetworkTopology           *NetworkTopology             `json:"networkTopology,omitempty"`
	QueueName                 string                       `json:"queueName,omitempty"`
	// Roles carries the per-Role settings that are not part of a revision:
	// replicas and the Role-level rolling update configuration.
	Roles []ServingGroupRole `json:"roles,omitempty"`
}

type ServingGroupStatus struct {
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedPods is a digest of the member Pods this status was computed from.
	ObservedPods string             `json:"observedPods,omitempty"`
	Conditions   []metav1.Condition `json:"conditions,omitempty"`
	// Revision is the revision applied by every Role instance; empty when they differ.
	Revision string `json:"revision,omitempty"`
	// ReferencedRevisions lists every revision used by a Pod of this group.
	ReferencedRevisions []string `json:"referencedRevisions,omitempty"`
	// Pods counts the Pods of the group, including terminating ones and Pods
	// the controller has decided to create.
	Pods            int32  `json:"pods"`
	ReadyPods       int32  `json:"readyPods"`
	FaultyMembers   int32  `json:"faultyMembers"`
	TerminatingPods int32  `json:"terminatingPods"`
	LabelSelector   string `json:"labelSelector,omitempty"`
	// RoleInstances is the ledger: one entry per Role instance.
	RoleInstances []RoleInstanceStatus `json:"roleInstances,omitempty"`
}

type RoleInstanceStatus struct {
	Role  string `json:"role"`
	Index int32  `json:"index"`
	// Phase is Creating (the zero value), Established or Draining.
	Phase RoleInstancePhase `json:"phase,omitempty"`
	// Generation numbers the successive sets of Pods of this instance.
	Generation int64 `json:"generation,omitempty"`
}
```

`spec` has no Pod template. Templates, plugins and the scheduler name are read from the ControllerRevision of the
revision, which is how the controller already reads historical templates. The object stays small and a revision has
one identity. `spec` has no schema defaults either: the stored spec is exactly what the ModelServing controller
wrote, so it can be compared with the desired one. `status` has no `phase`; the printed columns come from the
conditions.

Conditions of a ServingGroup:

| Type | True means | Written by | Read by |
| --- | --- | --- | --- |
| `Ready` | Every Role has `replicas` instances and every instance is running and ready. A group whose Roles all have zero replicas is ready. Same definition as today | ServingGroup controller | ModelServing controller: budgets, status |
| `Failed` | The group must be deleted and recreated at its ordinal. Never returns to false | ServingGroup controller | ModelServing controller |
| `Drained` | Meaningful with `Failed`: no faulty member and no terminating Pod is left | ServingGroup controller | ModelServing controller: deletes the object when both are true |
| `RecreateRequired` | The group can only be brought to its revision by replacing it: its Pods cannot be patched in place, or adopted Pods run another revision | ServingGroup controller | ModelServing controller: the group is a replacement candidate |
| `DrainBlocked` | A Role instance cannot be drained because `OnRoleDelete` keeps failing | ServingGroup controller | Users |
| `ReconcileFailed` | The last round ended with an error. `Ready` is still reported as observed | ServingGroup controller | Users |
| `CoordinatedRoleRolloutBlocked` | As on ModelServing today, reported per group | ServingGroup controller | ModelServing controller: aggregated |

Other additions:

| Object | Addition | Meaning |
| --- | --- | --- |
| ServingGroup | label `modelserving.volcano.sh/name` | The owning ModelServing |
| ServingGroup | annotation `modelserving.volcano.sh/surge: serving-group` | Temporary surge group. Set at creation, removed on promotion, never added later |
| Pod | controller owner is the ServingGroup; `blockOwnerDeletion: true` | Also for the PodGroup and the Services |
| Pod | label `modelserving.volcano.sh/instance-generation` | Generation of the Role instance the Pod was created for |
| Pod | annotation `modelserving.volcano.sh/surge: serving-group` | A copy of the group's mark, kept for tools that read it from Pods. Not an input |
| ModelServing | condition `RolloutBlocked` | There is work to do, the plan of this round is empty, and the cause has lasted longer than a threshold. Reasons: `BaselineUnknown`, `OrdinalOccupied`, `WaitingForDeletion`, `GroupNotObserved`, `TargetNotReady`, `BudgetExhausted` |
| ModelServing | annotation `modelserving.volcano.sh/servinggroup-adoption` | Progress of the takeover of workloads created by an older version: `InProgress`, `Done`, `RollingBack` |

The counts and the three existing conditions of the ModelServing `status` keep their definitions.

#### The contract between the two controllers

The controllers exchange information only through objects.

| # | Rule | Reason |
| --- | --- | --- |
| A1 | A group whose `status.observedGeneration` is lower than `metadata.generation` is *unobserved*: it keeps its place but is not counted as ready and is not a replacement candidate. The ServingGroup controller advances `observedGeneration` when its decisions were made from this `spec`, also in a round that ended with an error | The status still describes the previous spec right after a spec write |
| A2 | A spec carries the generation of the ModelServing it came from. The ModelServing controller never writes a spec with a lower `sourceGeneration` than the object already has | A process with a stale cache cannot write an old spec back |
| A3 | `Failed` requests deletion; `Drained` reports that it is safe. The group is deleted only when both are true | The decision does not rest on a counter being zero |
| A4 | Every group carries a copy of the baseline revision | The baseline can be read back when the ModelServing status was cleared |
| A6 | `status.referencedRevisions` reports the revisions used by the group's Pods | ControllerRevision clean-up does not list Pods |
| A7 | Every status write records, in `observedPods`, which member Pods it was computed from. The ModelServing controller computes the same digest from its Pod cache with the same function. If the two differ the status is *stale* and the group is treated as unobserved | A group that has just lost a member, or has just recovered, is not judged by a status written before that. After a restart neither controller has to wait for the other |

#### Group level: the ModelServing controller

The unit is a ServingGroup object. Groups are classified from the object alone: terminating (`deletionTimestamp`),
failed (`Failed`), otherwise live; surge or regular (annotation); revision (`spec.revision`); confirmed ready
(observed, fresh, `Ready`).

One round computes a plan as a pure function of the ModelServing and its groups. The steps are the existing ordinal
rules:

| Step | Action |
| --- | --- |
| Find back | Create an object for an ordinal that has member Pods without an owner (after an orphan deletion) |
| Promote | Remove the surge mark from a surge group whose ordinal is below `replicas` while fewer than `replicas` regular groups exist |
| Delete failed | Delete a group that is `Failed` and `Drained`, with foreground propagation |
| Scale in | Delete regular groups above `replicas`: not ready first, then lowest deletion cost, then highest ordinal |
| Fill | Create regular groups at the lowest free ordinals below `replicas`; baseline revision below `partition`, target revision otherwise |
| Surge | While groups remain to be replaced, create surge groups at the lowest free ordinals at or above `replicas`, up to `replicas + maxSurge` objects |
| Reclaim | Delete surge groups when nothing remains to be replaced, without going below `replicas - maxUnavailable` ready groups |
| Replace | Delete replaceable groups within the two budgets, not ready first, then highest ordinal. With in-place update, change `spec.revision` instead |
| Issue | Write the desired `spec` to every group whose spec differs |
| Advance | Set `status.currentRevision` to the target when all groups are at the target and ready and `partition` is 0 |

Group names are deterministic, so a repeated create fails with `AlreadyExists`; the controller then checks the UID
of the object's controller owner. An object of the same name that belongs to someone else occupies the ordinal: it
is neither adopted nor deleted, and the ModelServing reports `OrdinalOccupied`.

#### Inside a group: the ServingGroup controller

Member Pods are classified from the Pod alone: terminating, faulty (a fault that counts), failing (a fault that is
tolerated or still within the grace period), updating in place, ready, not ready. The state of a Role instance is the ledger entry combined with its members:

```text
                 all members seen                 lost or failed member (RoleRecreate),
   Creating ───────────────────────► Established ───────────────────────────────────────► Draining
      ▲                                                 no longer wanted                        │
      └───────────────────── no Pod of this generation left, still wanted ──────────────────────┘
```

| # | Rule | Prevents |
| --- | --- | --- |
| L1 | Write the ledger first, then delete Pods. Every deletion of an instance that the controller starts is preceded by a status write of `Draining`. If the write is rejected nothing is deleted or created in that round | A deletion that stops half way and cannot be told from a loss |
| L2 | The zero value causes no deletion. A cleared status leads to "create what is missing" | The risk of using status as input |
| L3 | `Established` is written after all members were seen in the cache, not after the create calls returned | A member that was deleted before it was observed |
| L4 | Evidence for `Draining` or `Failed` is confirmed by a direct read before the write | A Pod cache that is older than the ledger |
| L5 | Every entry has a generation number and every Pod is labelled with the generation it was created for. `Draining(g)` deletes only Pods of generation ≤ g | A process that reads an old `Draining` deleting the Pods of the next generation |
| L6 | Two-step deletion: faulty members first; the remaining members after the faulty ones are gone | Healthy members stopping before the faulty one has stopped |
| L7 | A member name taken by a Pod that does not belong to the group is not deleted; the member waits | Deleting foreign Pods |
| L8 | Before the first Pod of a group is created, `status.pods` is written with a resourceVersion precondition | The ModelServing controller changing the revision of a group while its Pods are being created |

Recovery by policy:

| `recoveryPolicy` | Lost member of an established instance | Faulty member (failed, or restarting and not ready) | Missing member of an instance being created |
| --- | --- | --- | --- |
| `None` | Create the missing Pod | Never deleted; the member is reported as not ready | Create it |
| `RoleRecreate` | Write `Draining`, delete the instance, recreate it | The same, once the fault has lasted for the restart grace period; never with a grace period of `-1` | Create it |
| `ServingGroupRecreate` | Write `Failed` on the group; the ModelServing controller deletes and recreates the group | The same, with the same grace rule | Create it |

What counts as a fault, the grace period and the meaning of `None` and `-1` are as today.

Whole-group recovery in sequence:

```text
ServingGroup controller   status: Failed=True              (the commit point)
ServingGroup controller   delete the faulty members only
ServingGroup controller   status: Drained=True             (no faulty member, no terminating Pod)
ModelServing controller   DELETE the group, foreground, with UID and resourceVersion
garbage collector         delete the group's Pods, PodGroup and Services; then the object
ModelServing controller   create a group at the lowest free ordinal below replicas
```

#### Decisions take objects as their only input

The writes of a round are a function of the latest `spec` and of objects: the ModelServing, its ServingGroups, their
Pods, PodGroups and Services, and ControllerRevisions. Time is compared only with timestamps on objects.

- A cache is an old copy of the objects. A round that has something to write reads the objects directly from the API
  server and computes the plan again from what it read.
- Every write has a precondition: creations use deterministic names; deletions carry the UID, and deletions of
  groups also the resourceVersion; status writes carry the resourceVersion.
- The process does not remember what it has sent. No expectations are kept, as in the StatefulSet controller.

What remains in memory can only delay a write, never bring it forward or widen its scope: the informer caches, the
work queues, whether `OnRoleDelete` was already called for a draining instance, and two exemptions:

- The start of a restart-failure grace period. A new process waits for a full grace period from the moment it first
  sees the failing member, as today. The grace period affects only *when* a recovery is committed; after the commit
  nothing depends on it.
- The moment since which a group has been unobserved or stale. It only decides when `RolloutBlocked` with reason
  `GroupNotObserved` is reported.

#### Plugins and other components

- The ServingGroup controller calls `OnPodCreate`, `OnPodReady`, `OnRoleSync` and `OnRoleDelete`.
- `OnModelServingSync` is called by the ModelServing controller before it plans groups, and by the ServingGroup
  controller before it changes a group. A blocking error stops the writes of that round in either controller, so no
  Pod is created while the hook blocks, as today.
- `OnRoleDelete` is guaranteed for an instance that the ServingGroup controller drains: it is called after the Pods
  of the instance are gone and before the instance is recreated or forgotten. When a whole group is deleted with
  foreground propagation the hook is called at most once on a best-effort basis; resources created by a plugin must
  be owned by the ServingGroup so that they are garbage collected with it.
- The eviction webhook, the autoscaler and ModelBooster find Pods by label and accept both owners: the ModelServing
  (workloads not yet taken over) and a ServingGroup of the ModelServing.
- A PodDisruptionBudget that selects the Pods of a group resolves the expected count through the `scale`
  subresource of ServingGroup.
- RBAC: `get`, `list`, `watch` on `servinggroups` for the aggregated view, edit and admin roles; `delete` for edit
  and admin; everything else only for the controller.

#### Upgrade and rollback

Workloads created by an older version have Pods, PodGroups and Services owned by the ModelServing.

**Exclusivity.** The older controller recognises its Pods by owner. When the owner of a Pod changes it treats the
Pod as deleted and applies `recoveryPolicy` to the rest of the unit. The two versions must therefore never reconcile
at the same time, and the new version enforces this itself:

| # | Guard |
| --- | --- |
| X1 | The controller-manager Pod template carries a label that names the ownership protocol. While a controller-manager Pod without the label exists in the namespace, the new process serves webhooks only and starts no controller; if such a Pod appears later the process exits and waits again |
| X2 | Without the ServingGroup CRD, or without permission on it, the process does not become ready and starts no controller. It never falls back to the old behaviour |
| X3 | The chart uses a new ServiceAccount, so the credentials of an older Pod stop working after the upgrade |
| X4 | Leader election is on by default and a process that loses the lease exits |

**Takeover**, per ModelServing, idempotent and resumable at every step:

| Step | Controller | Action |
| --- | --- | --- |
| T1 | ModelServing | Read the legacy objects directly. None: annotate `Done`. Some: annotate `InProgress`. While `InProgress` no group-level write is made for this ModelServing. The validating webhook admits an update that leaves `spec` unchanged, so that the annotation can be written on an object that today's rules would reject |
| T2′ | — | Not implemented. Legacy PodGroups and Services of a group that has no live Pod are taken over in place once the group object exists; those at ordinals that are no longer wanted stay owned by the ModelServing and are collected with it |
| T3 | ModelServing | Create a ServingGroup with the existing name for every group that has a live Pod. `spec.revision` comes from the Pods' revision label. The surge mark is decided once, by the rule the old version used: the ordinal is at or above `replicas` and at least one Pod carries the mark |
| T4 | ServingGroup | For each legacy member: read the ServingGroup directly, then replace the controller owner of the Pod, the PodGroup and the Services with it and add the generation label |
| T5 | ServingGroup | The ledger starts at its zero value. An instance whose members are all present is written as `Established` |
| T7 | ModelServing | No legacy object is left and every taken-over group is observed: annotate `Done` |

Only metadata is written. The kubelet does not read owner references, so containers are not affected.

The ledger starts empty. A member that was lost while no controller was running *during this first upgrade* is
therefore only filled in, as it is today. From then on losses during an outage are recovered by policy.

**Rollback** is a mode of the new version, entered by creating a ConfigMap in the controller's namespace. While it
exists the controllers do no normal reconciliation. For every ModelServing the controller sets the annotation to
`RollingBack`, moves the owner of Pods, PodGroups and Services back to the ModelServing, makes the surge mark on the
Pods agree with the group object, deletes the ServingGroup objects with orphan propagation and removes the
annotation. The old version is installed afterwards. Installing the old chart without this step makes the old
controller treat every Pod as foreign.

#### Behaviour changes

Recovery, compared with today:

| # | Today | With this proposal |
| --- | --- | --- |
| 1 | No per-instance record is persisted | `status.roleInstances` records phase and generation of every Role instance |
| 2 | A member lost while no controller observed the deletion is filled in | A lost member of an established instance is recovered by `recoveryPolicy`, also after a controller outage. Exception: the first upgrade to this version |
| 3 | A Pod deleted from an instance that is still being created triggers a scoped recovery when the delete event is observed | A member missing from an instance that was never established is created; nothing else is deleted |
| 4 | A deletion of a group or an instance that was started is forgotten when the controller restarts; the remaining Pods stay and the gap is filled | It is completed by the next process, for the Pods it was started for |
| 5 | A recovery that waits for the grace period is carried out with the policy and the grace period captured when the fault was seen | The policy and the grace period are read when the recovery is committed: changing to `None` or `-1`, or raising the grace period, cancels it. Once `Failed` or `Draining` is written it is completed |

Rollout, where today's controller differs from the documented rollout rules and this proposal follows the rules:

| # | Today | With this proposal |
| --- | --- | --- |
| 6 | A partition-protected group that is not ready does not reduce the rollout budget | It does: the number of ready groups is not taken below `replicas - maxUnavailable` |
| 7 | With sparse ordinals, surge capacity can be created in a hole below `replicas`, unmarked | Surge groups are created at ordinals at or above `replicas` and are marked |
| 8 | A scale-in during a surge rollout can select the surge group | Scale-in selects regular groups only; surge groups are reclaimed when the rollout ends |
| 9 | Replacement and scale-in run in the same pass | No group is replaced while a scaled-in group is still terminating |
| 10 | `status.currentRevision` advances when every group is updated and available | It advances only when `partition` is 0 and no group has all its Roles at zero replicas |

Operation:

| # | Today | With this proposal |
| --- | --- | --- |
| 11 | Leader election is off by default; a process that loses the lease keeps running | Leader election is on by default; a process that loses the lease exits |
| 12 | Each whole-group replacement recreates Pods; the PodGroup and the Services are updated in place | The ServingGroup object, its PodGroup and its Services are recreated with the group (new UIDs, same names) |
| 13 | A PodDisruptionBudget with a percentage resolves the expected count from the ModelServing scale, which counts groups | It resolves it from the ServingGroup scale, which counts Pods |

Everything else — names, labels, the ModelServing API, status counts and conditions, events, rollout order and
budgets — is unchanged.

#### Test Plan

- **Unit tests**, table driven: member classification, ledger transitions, status computation, the group-level
  plan, takeover steps. For every write there is a test that stops after it and continues with a controller that
  has no memory.
- **Differential tests of the group-level plan** against a reference model of the rules, replaying the existing
  rollout scenarios (scale during rollout, repeated updates, partition, sparse ordinals, surge promotion and
  reclaim) step by step.
- **E2E on Kind** (`test/e2e/controller-manager`): the existing ModelServing suites, plus the life cycle and status
  of ServingGroup, whole-group recovery, deletion of a group by a user with each propagation policy, and
  continuation after a leader change.
- **Conformance**: the rollout and recovery scenarios used to validate the current controller are run against the
  version before and after this change on the same cluster and compared case by case. Differences must map to the
  table in [Behaviour changes](#behaviour-changes).
- **Upgrade drills on Kind** from v1.1.0 and from the commit before this change: steady state, a rollout with a
  surge group in progress, a partition stop, sparse ordinals, an in-place update in progress, killing the new
  process after each takeover step, upgrading without the CRD. Asserted: Pod UIDs, container restart counts,
  readiness, PodGroup UIDs and `availableReplicas` do not change. Then rollback and a second upgrade.
- **Failover**: with two replicas, the leader is killed after each kind of write; Pods are deleted while no
  controller runs and the recovery scope is checked.

### Alternatives

**Keep the logical group and protect members with a Pod finalizer.** A finalizer keeps the deleted Pod visible until
the controller has recorded the loss, which gives the same determinism without a new object. A protected Pod that is
deleted while the controller is not running stays terminating, and so do the ModelServing and the namespace.
Uninstalling and downgrading need an ordered procedure.

**Record scope-level facts on every Pod of the scope.** This is what the surge mark does today. Writes are not
atomic, each reader needs rules for partially marked scopes, and a scope without Pods has no carrier.

**A private ConfigMap or annotations on the ModelServing.** One object for all groups: every recovery in any group
contends on it, it grows with the number of instances, and it is a table beside the objects.

**Random group names with count-based rollout, as Deployment and ReplicaSet do.** Fewer rules at the group level (no
surge mark, no sparse ordinals). Group and Pod names would change on every replacement, `partition` would mean a
count at the group level and an ordinal at the Role level, and host names derived from the ordinal would be lost.

**Make Role instances objects too.** The ordinal of a Role instance is used by the HPA selector, by Volcano task
names and by the Role-level `partition`; an instance without workers is a single Pod. The ledger in the group's
status gives the same guarantees with a few values per instance.

**Keep both code paths behind a feature gate.** For one ModelServing, the owner of the Pods, the evidence for
recovery and the interface between the controllers change together; a group cannot be half converted. Two paths
would mean two controllers to maintain, and the old path would have to learn not to treat taken-over Pods as
deleted.
