# ModelServing: where should the controller keep what it knows about a ServingGroup and a Role instance?

> **Draft of a discussion to open in `volcano-sh/kthena`.** It asks for direction; it does not propose merging anything.
> `CONTRIBUTING.md` asks that large changes go to Discussions first, so it is written for a GitHub Discussion. The same
> text fits an issue: sections 1 to 8 answer "an in-depth description of the question", section 9 answers "what do you
> think about this question", section 10 is the environment.
>
> References to `main` are pinned to [`4323a9bf`][main]. References to the prototype are pinned to [`98fcbf01`][proto]
> on a fork.

## Summary

`recoveryPolicy` and the rolling updates of ModelServing act on units that are larger than a Pod: a Role instance (an
entry Pod and its workers) and a ServingGroup. To act on such a unit correctly the controller needs three things that
the current set of Pods does not tell it: which incarnation of a reused name a Pod belongs to, whether the unit was
complete before, and whether a deletion of the whole unit has been decided. Today these live in controller memory and
in informer events. What happens after a failure then depends on whether the controller was running, and was the
leader, at that moment.

We built a complete prototype of one answer, ServingGroup as a CRD with a controller of its own, and ran it against the
e2e suite and a conformance suite. Having done that, we think we went too far down one path on our own. The prototype
works, but it is large, it needs a live change of Pod ownership, and it solves the problem at the group level only:
inside a group the Role instances still need a ledger that is kept right by rules. The alternative we considered, the
same ledger in `ModelServing.status`, grows with the number of Role instances. We do not have a design that we are
confident is the right one.

Before sending any PR we would like the community's view on three things:

1. Which of the guarantees in section 1 does upstream want?
2. If it wants them, where should the durable record live?
3. Which parts do not depend on that choice and can land first?

## 1. The problem

### 1.1 One observation, two correct reactions

`RoleRecreate` means: when a member of a Role instance is lost, recreate the whole instance. `ServingGroupRecreate`
means the same for the whole group. Both exist because the members of a unit are not independent: a replacement worker
cannot join a running entry. To apply them the controller has to tell two situations apart:

| Observation | Situation | Correct reaction |
| --- | --- | --- |
| The Pod `llama-0-prefill-0-1` does not exist | The instance is still being created | Create the Pod |
| The Pod `llama-0-prefill-0-1` does not exist | The instance was complete and this Pod was lost | Delete the rest of the instance and recreate it |

The Pods that exist are the same in both rows.

### 1.2 Three things the Pods do not tell

| Needed | Why the Pods are not enough |
| --- | --- |
| **Identity.** The two incarnations of one name must be told apart | Names are positions (`<ms>-<group>-<role>-<index>-<n>`) and are reused |
| **Established.** Was this unit complete before? | The Pods that exist are the same whether a member has not been created yet or was lost |
| **Committed scope.** Has the deletion of this whole unit been decided, and for which incarnation? | Half-way through, what is left looks like a unit with missing members |

### 1.3 Where they are today

| Fact | Where it is on `main` | After a restart or a leader change |
| --- | --- | --- |
| A member was lost | The informer delete callback: [`deletePod`][main-deletepod] calls [`handleDeletedPod`][main-handledeleted], which deletes the Role or the group | An event that this process did not receive is gone. The reconcile loop later sees a gap and creates one Pod |
| State of a group and of a Role instance (`Creating`, `Running`, `Scaling`, `Deleting`) | The in-memory [`datastore`][main-states] | Rebuilt by [`syncAll`][main-syncall], which replays the Pods that exist as add events |
| Which Pods a deletion selected; cleanup of a Role | Two `sync.Map`s, [`podDeletions` and `roleCleanups`][main-maps] | Lost. #2112 describes the result: a Role that was being deleted "could be mistaken for a healthy Role missing its entry Pod and refilled instead of deleted" |
| A member has been failing since time *t* | [`graceMap`][main-maps] and a [sleeping goroutine][main-grace] | Lost; the grace period starts again |
| A successor reuses the name | [`recreatedSinceDeletion`][main-recreated] compares timestamps with a resolution of one second | #1957: a late delete event deletes the healthy replacement |
| This unit is temporary surge capacity | An [annotation][main-surge] on each Pod of the unit | Kept, but it can be on only some of the Pods, and a unit without Pods has none |

### 1.4 What follows

1. **Losses while the controller is away are under-recovered.** A node is drained while the controller is being
   upgraded or while leadership moves. A worker of a complete instance is deleted. The new process creates one Pod. The
   policy does not hold in the window in which nodes are most often drained.
2. **A deletion that was half done is not finished.** What is left is repaired instead.
3. **A late event can hit the successor** (#1957).
4. **The same cluster state leads to different actions**, depending on which events this process saw.

We compared `main` at `bc26f686` and the prototype at `9ac97725` with a conformance suite for ModelServing rollout and
recovery ([`kthena-rollout-runner`][runner]) on Kind (Kubernetes v1.37.0, Volcano 1.16.0). 778 cases have a verdict on
the prototype: 690 pass, 70 fail, 18 are inconclusive. Of these, 93 fail on `main` and pass on the prototype, 59 of
them in the recovery suite; 68 fail on both. 2 pass on `main` and fail on the prototype; both are timing, and both pass
when rerun alone. This measures the prototype as a whole. It does not show which part of it is responsible, and the
suite and its contract are ours, not upstream's.

### 1.5 What is already in progress

- #1999 (merged): the controller manager exits when it loses the lease.
- #2112 and PR #2126: immutable `group-instance-id` and `role-instance-id` labels on Pods, Services and PodGroups, with
  deletion fenced by owner UID, instance ID and UID precondition. This addresses **identity**, at both levels, with one
  mechanism and without a new object or a status write. We think it is the right way to do identity (see 3.4).

This discussion is about the other two rows of 1.2. As far as we can tell from reading #2126 as it is today, a worker
that is lost while the controller is away, with the entry Pod still there and nothing terminating, is still refilled,
and the scope of a deletion that was under way is derived again from the policy at restart. Whether upstream considers
these gaps acceptable is our first question.

## 2. A constraint we ran into

"Established" and "committed" describe the unit as it was before a member was lost. If the record is carried by the
members, and members can vanish without a trace, the record is exposed to the very event it is about. Three ways of
putting it on Pods, each with a sequence that breaks it:

| Record on the Pods | Sequence that breaks it |
| --- | --- |
| An "established" mark on the entry Pod only; "entry missing, workers present" is read as a loss | History 1: entry and worker 1 exist, worker 2 not yet, the entry is deleted. History 2: the instance was complete, the entry and worker 2 are lost. Both leave worker 1 alone. The first should be completed, the second recreated |
| The first `DELETE` is the commit | The deletion is decided, the entry is deleted, the controller crashes, the policy is changed to `None`. The new process cannot tell "finish the deletion" from "an entry was lost under `None`, create it" |
| A surge mark that is removed Pod by Pod, read as "surge while any Pod carries it" | The mark is removed from the entry, the controller crashes, the last marked worker is lost. The unit reads as promoted although the last write never happened |

Three things satisfy the constraint:

- A finalizer that keeps a deleted Pod until the fact is recorded. We rejected it because protected Pods stay
  terminating while the controller is not running or has been uninstalled, which blocks the deletion of the namespace
  and a foreground deletion of the ModelServing.
- A record outside the members: Paths A and B below.
- One member that owns the others, so that its loss is the loss of the unit and the record can sit on it. This is what
  LeaderWorkerSet does (section 7). In ModelServing today the entry Pod and the workers are siblings owned by the
  ModelServing.

We have not shown that no other scheme on the members can work; the three in the table are the ones we examined.

## 3. Path A: ServingGroup as an API object

### 3.1 What it is

A `ServingGroup` CRD. The ModelServing controller creates and deletes ServingGroup objects and decides how many there
are and at which revision. A second controller, keyed by group, owns the Pods of one group. Facts about a group are
fields of the object. Facts about the Role instances of a group are a ledger in `ServingGroup.status.roleInstances[]`,
one entry per instance: a phase (`Creating`, `Established`, `Draining`) and a generation.

A prototype exists: [branch][proto], [proposal][proto-proposal], [design notes][design] (Chinese). It is +51,837/−31,595
lines in 268 files against `main` at `bc26f686`.

### 3.2 What it gives

- The group has a UID. Its Pods, PodGroup and Services point at it, so the two incarnations of `llama-0` are different
  owners.
- Deleting a group is a foreground deletion of one object. The garbage collector finishes it, also while kthena is not
  running, as long as the ModelServing exists.
- Each group has its own `status` and its own queue key. A write is as large as one group.
- `kubectl get servinggroup` shows one line per group; `kubectl delete servinggroup` rebuilds one group.

### 3.3 What it costs

- **A live change of ownership.** Running Pods are owned by the ModelServing. Upgrading replaces the controller owner
  of every Pod in place, old and new controllers must not overlap, and going back is a procedure of its own. We
  exercised this on Kind from v1.1.0 and from `main`: Pods keep their UID and restart count. We used placeholder
  images, so we have no data on requests.
- **Two controllers instead of one.** They exchange `spec` and `status`, and each sees the other one step late.
- **A state the logical model does not have: the object exists and its Pods do not yet.** Several defects that we found
  late came from these two points. A group whose `spec` arrives one step after the ModelServing changed recreated
  instances from the old count. A group "does not know whether it was born in this rollout", which needed an annotation
  and rules for when to trust it. The rule that reclaims a surge group took three rounds to get right, because a surge
  group that has just been created has no Pods either. The conformance suite itself had to learn that a group object
  can exist before its Pods.
- **The budget is a sum over objects.** With two processes alive, one unit of `maxUnavailable` can be spent twice. The
  prototype relies on leader election to make that window small.
- **Recreating a group under the same name depends on the garbage collector.**
- **It does not fit #2126 as it is.** The validating webhook of #2126 rejects a change of the ModelServing controller
  owner on a Pod, which is what the takeover does. One of the two would have to change.

### 3.4 What it does not solve: the level below

This is the point we under-weighted. The object gives the group a UID, a status and garbage collection. A Role
instance inside the group gets none of these. For it, the prototype does what Path B does for everything: a ledger in
the `status` of the nearest object, kept right by rules. So Path A needs both mechanisms. Its own design note says that
the Role level and in-place update do not benefit from it.

What that meant in practice:

- **Identity is a counter kept by hand.** The generation lives in the ledger entry. The entry is removed when an
  instance is scaled away ([`ledger.go`][proto-ledger-remove]), so an instance that is scaled back at the same index
  starts at generation 0 again ([`ledger.go`][proto-ledger-gen]). The cleanup hook for headless Services deletes
  Services whose generation is not greater than the one in the request
  ([`headless_service_plugin.go`][proto-plugin]). A hook that was issued for the earlier life, at generation 1, and
  arrives late deletes both Services of the instance that came back. We reproduced this in unit tests of the plugin and
  of the group controller with a fake API server. It needs a second process that still acts after losing the lease, so
  we have not seen it on a cluster. In the test, later rounds recreate the Services and the Pods keep their UIDs; how
  long that takes on a cluster and what it does to traffic is not measured. The fix is known: a random ID per
  incarnation, compared for equality, as #2126 does. The point is that the object did not spare us this.
- **One instance is cleaned up by hooks, not by the garbage collector**, because the group lives on.
- **Coordinated Role rollout is unchanged.** Its rules were moved as they were (see section 8).

### 3.5 What we measured on the prototype

| Measurement | Result |
| --- | --- |
| Conformance suite, `main` against prototype | See 1.4 |
| `make test-e2e-controller-manager` | Passes (50 tests) |
| Writes in one run of that e2e suite | 164 ServingGroup objects were updated 1,573 times, against 668 updates of 51 ModelServing objects (2.35 times). 1,427 of the 1,573 are `status` writes, for 360 Pods: about four per Pod over its life |
| Size of a ServingGroup object, with at most 4 Role instances | 1.3 to 1.6 KB; a ledger entry is 48 to 63 bytes. Compact JSON, without `managedFields` |
| A group with many Role instances | Not tested. The largest group had 6 |
| Groups in parallel against one queue key | Not measured. We have no number that shows a benefit |

## 4. Path B: ServingGroup stays logical, the ledger is in `ModelServing.status`

### 4.1 What it is

One controller, as today. `ModelServing.status` gets a table with one entry per Role instance, and what is needed per
group. Pods stay owned by the ModelServing. Not implemented.

### 4.2 What it gives

- No change of ownership of Pods. An upgrade still needs a schema change and a rule for old and new binaries running
  side by side (4.3).
- One writer and one snapshot. There is no hand-over between controllers, and no object without Pods.
- Spending the budget across groups can be one conditional write.

### 4.3 What it costs

- **Size and write volume.** Every change rewrites the whole table and sends it to every watcher of the ModelServing.
  With the entry size measured on the prototype, and an estimate for `managedFields`:

  | Role instances in one ModelServing | Ledger only | With `managedFields` (estimate) |
  | --- | --- | --- |
  | 100 | 5 to 6 KB | 13 to 15 KB |
  | 300 | 14 to 19 KB | 38 to 46 KB |
  | 1000 | 48 to 63 KB | 128 to 153 KB |

  A ModelServing object is about 2 KB today. The size stays far below the etcd limit. The concern is the bytes written
  over a full rollout. In a simple model (two ledger writes per instance that cannot be merged, two Pods per instance,
  and for Path A the four status writes per Pod and the object size measured above, with small groups) Path B grows
  with the square of the number of instances: it passes Path A at a few dozen instances, is about six times Path A at
  300 and about twenty times at 1000. Merging writes and the size of groups move these numbers a lot. We have not
  measured Path B.
- **All groups share one `resourceVersion`.** Either one worker writes for all groups, or writers conflict and retry.
- **Old binaries erase it.** A controller that does not know the field drops it on its next `UpdateStatus`. The zero
  value therefore has to be the safe side, and a deletion that was committed can be interrupted while old and new
  overlap or after a downgrade.
- **Identity and cleanup are by rules at both levels.** There is no UID to fall back on.
- **It goes against the stated aim of #2112**, which is to avoid extra writes and schema changes in
  `ModelServing.Status`.

## 5. Variants we have not explored

We list them so that they can be ruled in or out. None is designed, let alone tested.

- **A group object that only records.** One object per group, owned by the ModelServing, written by the one
  ModelServing controller, not the owner of any Pod. It would have the write scope and the visibility of Path A
  without the change of ownership and without the second controller. It would not have garbage collection of a single
  group.
- **A Role instance anchored on its entry Pod**, as LeaderWorkerSet anchors a group on its leader: the workers are
  owned by the entry Pod, whose UID is the identity, which carries the record, and whose foreground deletion is the
  commit. It would cover the Role-instance level without any ledger. An entry Pod that is lost would then recreate the
  instance under every policy. It does not cover a group.
- **A compressed ledger**, as Indexed Job does for completed indexes. In the steady state nearly all instances are
  established. This conflicts with binding the record to an incarnation.
- **A ledger split over several objects.** One more step and it is the first variant.

## 6. Side by side

| | Labels on children only (#2126) | Path A: group object | Path B: parent status |
| --- | --- | --- | --- |
| Identity | Yes, both levels | Group: UID. Role instance: by rules | By rules, both levels |
| Established, across a restart | No | Yes | Yes |
| Committed scope, across a restart | Derived again from the Pods | Yes | Yes |
| Record survives the loss of members | n/a | Yes | Yes |
| Role-instance level | Same mechanism as the group | Ledger in the group's status, by rules | Same mechanism as the group |
| Budget across groups | As today | Sum over objects | Can be one write |
| Write scope | No new writes to status | One group | The whole ModelServing |
| Cleanup of one group while kthena is away | Waits | Garbage collector | Waits |
| Upgrade of running workloads | No change of owner | Change of owner, exclusive, controlled rollback | No change of owner; old binaries erase the record |
| New API surface | Labels, one webhook | A CRD, RBAC, a second controller | Fields in status |
| Verified | PR under review | Prototype: e2e, conformance suite, upgrade drills on Kind | Nothing |

## 7. How comparable projects handle this

We read the implementation of three projects with multi-Pod units whose names are reused: RoleBasedGroup
([`sgl-project/rbg`][rbg] at `a19d2c83`), NVIDIA Dynamo Grove ([`ai-dynamo/grove`][grove] at `15411d9c`), and
LeaderWorkerSet with DisaggregatedSet ([`kubernetes-sigs/lws`][lws] at `1cb7505e`). Nothing was run; this is source,
docs and issues. The notes with every code reference are in
[`modelserving-unit-state-discussion-peers.md`](modelserving-unit-state-discussion-peers.md).

All three have met the same three problems, most of them in issues from the last few months.

| | RoleBasedGroup | Grove | LeaderWorkerSet / DisaggregatedSet |
| --- | --- | --- | --- |
| Which levels are objects | Every level: RoleBasedGroupSet, RoleBasedGroup, RoleInstanceSet, RoleInstance | PodCliqueSet, PodCliqueScalingGroup, PodClique, PodGang, PodGangMap. The replica index of a PodCliqueSet and of a scaling group are not, and those are the units that gang termination recreates | LWS and DisaggregatedSet. A group is not: it is a leader Pod that owns the worker StatefulSet. DisaggregatedSet has no unit across roles: one LWS per slice, revision and role |
| Identity | Pods are listed by owner UID, and a same-name Pod of the previous owner blocks creation; added after [rbg#496][rbg496] and [rbg#511][rbg511]. Pod deletes in the restart and scale paths carry a UID precondition; the fallback of in-place update and deletes of a RoleInstance do not | Reads go by owner UID and Pod names are random. Deletes carry no UID precondition; a [comment in the code][grove-retry] accepts that a retried delete removes the PodCliques that were just recreated. State keyed by name failed twice ([grove#782][grove782], [grove#943][grove943]) | The leader Pod's UID. The queue key carries the UID since [lws#998][lws998]; a counter keyed by the reused slot could not tell incarnations apart ([lws#1247][lws1247]). Several UID preconditions were added after v1.0.0 |
| Established | A persisted latch, `RoleInstance.status.lastReadyGeneration`. A loss is detected only from Pods that still exist, so a Pod that is lost while the controller is away is refilled; open as [rbg#562][rbg562] and [rbg#566][rbg566] | `PodClique.status.lastScheduled`, never reset. It replaced a timestamp heuristic that terminated healthy replicas ([grove#789][grove789]); a contributor wrote there that "explicit, persisted evidence of a healthy state is safer and more reliable than the current timing heuristic". The scaling group still uses "creation plus 5 seconds" | Until 2026-11-26, nothing: a worker lost while the controller was away never recreated the group ([lws#1301][lws1301]). Fixed on `main` by [lws#1319][lws1319] with a digest of the worker UIDs and a `started` flag on the leader Pod. Not in a release yet |
| Committed scope | Recorded before the first delete only with `maxGroupRestarts`. By default it deletes first and writes status afterwards | Scale-in and update selections are written before any delete. Gang termination is, in the code's words, "action-first / flag-second" | The foreground `DELETE` of the leader Pod with a UID precondition. The garbage collector finishes the scope |
| Per-unit state in a parent | None. The cost shows as reads and writes across levels ([rbg#520][rbg520]) | No ledger of all replicas; the parent status holds the indices being scaled in and the replica being updated. A per-PodGang list in the parent status was declared, never set, and removed. [grove#781][grove781] reports a regression of about 35% in a churn benchmark and points to one extra status transition on each new child | Two JSON maps per LWS in its annotations, keyed by revision and group index (restart counts and the leader that was charged), only with a restart budget or backoff: together about 221 KiB at 4,000 groups, against a limit of 256 KiB |
| Change of object model | By recreation, with a window without Pods; no migration tool, by design | In place: metadata-only patches behind a gate in status | Groups are rolled. A DisaggregatedSet never adopts an existing LWS |
| Coordinated rollout | An open PR about a stall between role dependencies and `maxSkew` ([rbg#638][rbg638]) | Stalls and early "completed" ([grove#786][grove786], [grove#916][grove916], [grove#952][grove952]) | DisaggregatedSet replans from its children on every reconcile; an open deadlock ([lws#1292][lws1292]); neither API has a "stalled" signal |

What we take from this:

1. **An object at a level did not remove the problems at that level.** RBG has an object at every level and still
   refills a Pod lost while the controller was away, and by default still deletes before it records. In the
   paths we read, what closed a gap was one of three things, with different coverage in each project: a durable
   "established" record that outlives the member that was lost, the UID on destructive writes, and a record before
   the first delete.
2. **Every project has a level below its objects**, and handles it by rules: the components of an RBG instance, the
   replica indices of Grove, the workers of an LWS group. This is the same shape as 3.4.
3. **LWS shows a carrier we had not considered: one member that owns the others.** The leader Pod supplies the UID,
   carries the record and is the handle of the scoped delete, and the garbage collector finishes the scope. It works
   because the loss of the leader is defined as the loss of the group. For a Role instance that would mean that a lost
   entry Pod recreates the instance under every policy, `None` included. It does nothing for a unit across Roles, and
   neither LWS nor DisaggregatedSet has such a unit. The LWS maintainers rejected a finalizer on every worker for the
   reason we did ([review of lws#1319][lws1319-review]).
4. **None of the three keeps a ledger of every unit in a parent object.** Grove keeps only the selections in flight.
   LWS accepts two maps, for an opt-in feature, and documents their ceiling. This supports the worry about Path B.
5. **Deciding from timestamps went wrong in Grove** ([grove#789][grove789]), and its gang termination, which acts
   before it records, accepts one extra recreation after a crash.
6. **The migrations we read did not change the owner of running Pods.** RBG recreated, Grove patched metadata, LWS
   rolls groups. We did not go through the whole history of the three projects.

## 8. What can land first, whatever the choice

1. **Identity**: #2126. We can review it with the cases we collected, and contribute tests.
2. **The structure of the reconcile loop.** We believe, without having measured it separately, that most of what the
   prototype fixes comes from two changes of structure and not from the object: informer callbacks only enqueue, and
   each round computes its writes from the objects it read. In the prototype this is a planner that decides which units
   exist, at which revision and which is replaced next, used for groups and for Role instances alike
   ([`grouplevel`][proto-grouplevel]), and a transition function for one Role instance over its record, its observed
   members and the policy ([`ledger.go`][proto-ledger]). Both are pure functions. The planner is checked against
   17,504 vectors from a reference model. The transition function reads a record that can come from anywhere,
   including memory. So this part can be offered as a refactoring of the current controller with the facts still where
   they are today and no API change; a durable record can be plugged in later. It is also the largest piece, and we
   would like agreement on the approach before sending a series.
3. **Reporting of coordinated Role rollout.** Reading [`role_rolling_update_coordinator.go`][main-coord], and running
   the same rules as ported to the prototype, we see two cases in which no instance is replaced while the condition
   says that progress is available or that the rollout is complete. (a) An old instance at a low ordinal is not ready
   but has no fault that starts a recovery; it uses the unavailable budget, and the descending order does not allow
   passing the healthy instance above it. (b) The template of a Role is the same at the baseline and at the target, and
   one of its instances still runs a revision in between; `inProgress` is false for that Role, and the reason is
   `RolloutComplete`. The validating webhook [reads that reason][main-webhook] as "no coordinated rollout is under
   way". We have not reproduced either on `main` or on a cluster yet and will file them separately when we have.
4. **A lost delete event.** When the watch drops the `DELETED` event of a Pod that is being drained, the informer cache
   keeps the Pod and the unit stays in its deleting state. This fails on `main` and on the prototype alike. Units that
   wait for a Pod to disappear need a periodic direct read.
5. **A shared conformance suite.** Whichever path is taken, comparing them needs cases that both sides accept. We can
   offer the runner and its cases as a starting point; the contract behind it is ours and would need review.

## 9. Our current view

- We do not have a design that we would defend as clearly better than the others. That is why this is a discussion and
  not a proposal.
- Identity belongs on the children, as random IDs per incarnation. #2126 does this, and we would drop our generation
  counters for it.
- If upstream wants "established" and "committed scope" to survive a restart, the evidence has to outlive the member
  that is lost. A record outside the Pods and a member that owns the others are two ways to get that, and the second
  changes what a lost entry Pod means. A group object is the only form we have built. It is heavy, and it leaves the
  Role-instance level to a ledger. The parent status is lighter to introduce and grows with the number of instances.
  We would like to hear the scale that users have in mind before preferring any of them.
- We would rather land items 2 to 5 of section 8 first and decide about the record with data.
- We are not asking to merge the prototype.

Questions:

1. Should a member that is lost while the controller is not running be recovered according to `recoveryPolicy`, and
   should a deletion of a unit that was decided before a restart be finished after it? Or are these acceptable gaps?
2. If they are required, which carrier is acceptable upstream: a group object, fields in `ModelServing.status`, or
   something else?
3. Is an object per group wanted for its own sake: to see, delete and recover one group, or to have a group cleaned up
   while kthena is not running?
4. How many groups and Role instances per ModelServing should the design assume?
5. Is a level-triggered refactoring of the ModelServing controller welcome as a series of PRs that does not depend on
   questions 1 to 4?
6. Does anyone see a reason to make Role instances objects as well? We do not.

## 10. Environment and references

- Kthena: `main` at [`4323a9bf`][main]. The comparison in 1.4 used `main` at `bc26f686` and the prototype at
  `9ac97725`.
- Kubernetes v1.37.0 on Kind, Volcano 1.16.0.
- Prototype: [branch][proto], [proposal][proto-proposal], [implementation design][proto-design] (Chinese),
  [design notes][design] (Chinese).
- Conformance runner: [`kthena-rollout-runner`][runner].
- Related: #1957, #2112, #2126, #1999, #1950.

Parts of the prototype and of this analysis were produced with AI assistance. The measurements come from runs on Kind
clusters and from the objects dumped in those runs, and the code references can be checked at the pinned commits. The
items marked as not reproduced or not measured are exactly that.

[main]: https://github.com/volcano-sh/kthena/tree/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89
[main-deletepod]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L462-L509
[main-handledeleted]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2355-L2386
[main-recreated]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L511-L529
[main-syncall]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L791-L810
[main-maps]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L114-L119
[main-grace]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2250-L2322
[main-states]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/datastore/store.go#L84-L97
[main-surge]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/coordinated_role_observation.go#L179
[main-coord]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/role_rolling_update_coordinator.go#L717
[main-webhook]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/webhook/role_coordination_validator.go#L284
[proto]: https://github.com/VanderChen/kthena/tree/98fcbf01b93c812539d0651e20b20a790a7a4bb1
[proto-proposal]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/docs/proposal/modelserving-servinggroup-cr.md
[proto-design]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/docs/proposal/modelserving-servinggroup-cr-design.zh-CN.md
[proto-ledger]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L226-L328
[proto-ledger-gen]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L72-L81
[proto-ledger-remove]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/servinggroup/ledger.go#L263
[proto-plugin]: https://github.com/VanderChen/kthena/blob/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/plugins/headless_service_plugin.go#L260-L284
[proto-grouplevel]: https://github.com/VanderChen/kthena/tree/98fcbf01b93c812539d0651e20b20a790a7a4bb1/pkg/model-serving-controller/grouplevel
[design]: https://github.com/VanderChen/kthena/tree/d5686ba8e7e57bf143c1deb1b63b89be93059d32/docs/proposal/modelserving-servinggroup-cr
[runner]: https://github.com/VanderChen/kthena-rollout-runner/tree/6b0af07ec21926824b0d813456985e736831d9c9
[rbg]: https://github.com/sgl-project/rbg/tree/a19d2c83fc1aca4f57a23cf7eee56f687e2da023
[rbg496]: https://github.com/sgl-project/rbg/issues/496
[rbg511]: https://github.com/sgl-project/rbg/issues/511
[rbg520]: https://github.com/sgl-project/rbg/issues/520
[rbg562]: https://github.com/sgl-project/rbg/issues/562
[rbg566]: https://github.com/sgl-project/rbg/issues/566
[rbg638]: https://github.com/sgl-project/rbg/pull/638
[grove]: https://github.com/ai-dynamo/grove/tree/15411d9ccb37cfbae9e7cded612762e1ad41777c
[grove-retry]: https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L302-L304
[grove781]: https://github.com/ai-dynamo/grove/issues/781
[grove782]: https://github.com/ai-dynamo/grove/issues/782
[grove786]: https://github.com/ai-dynamo/grove/issues/786
[grove789]: https://github.com/ai-dynamo/grove/issues/789#issuecomment-5395204069
[grove916]: https://github.com/ai-dynamo/grove/issues/916
[grove943]: https://github.com/ai-dynamo/grove/issues/943
[grove952]: https://github.com/ai-dynamo/grove/issues/952
[lws]: https://github.com/kubernetes-sigs/lws/tree/1cb7505e8bfe98dd0b240439c12436d52deb500e
[lws998]: https://github.com/kubernetes-sigs/lws/issues/998
[lws1247]: https://github.com/kubernetes-sigs/lws/issues/1247
[lws1292]: https://github.com/kubernetes-sigs/lws/issues/1292
[lws1301]: https://github.com/kubernetes-sigs/lws/issues/1301
[lws1319]: https://github.com/kubernetes-sigs/lws/pull/1319
[lws1319-review]: https://github.com/kubernetes-sigs/lws/pull/1319#discussion_r4663692205
