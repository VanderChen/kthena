# [Discussion] ModelServing Durable Unit State: ServingGroup CR (Plan A), ModelServing.status (Plan B), or Plan C?

> **English** | [中文](./modelserving-unit-state-discussion.zh-CN.md)
>
> Code references to `volcano-sh/kthena` `main` are pinned to [`4323a9bf`][main].

---

## TL;DR

- **The problem:** `ModelServing` has **two nested recreation levels**—`RoleRecreate` (a Role instance: entry Pod +
  workers) and `ServingGroupRecreate` (a ServingGroup across Roles)—along with rolling updates over those same multi-Pod
  units. While #2112 / PR #2126 cleanly solves **instance identity**, two restart-sensitive facts still live only in
  controller memory and watch events: whether a unit was ever complete (*established*), and whether tearing down the
  whole unit was already decided (*committed deletion*). *(Note: our target design still allows the constraint that a
  Pod lost while the controller is offline is refilled instead of triggering full recreation—of course, solving it is
  even better.)*
- **The solution space — Plan A vs. Plan B vs. Plan C:**
  - **Plan A (`ServingGroup` CR):** Gives each group its own UID, status write scope, and Kubernetes GC—*but* requires
    live Pod owner migration, splits reconciliation across two controllers, and **only solves the group layer** (Role
    instances inside a group still need a status ledger). We tried prototyping this approach to evaluate its tradeoffs.
  - **Plan B (`ModelServing.status` ledger):** Keeps a single controller and avoids Pod owner migration—*but* rewrites
    the whole table on one shared `resourceVersion`, creating quadratic rollout write amplification at larger
    Role-instance counts and conflicting with #2112's goal of avoiding `ModelServing.status` writes/schema changes.
  - **Plan C:** Given the clear drawbacks of both Plan A and Plan B, **is there a Plan C** that solves this problem
    better?

---

## 1. The Problem: Why Pods Alone Aren't Enough

A `ModelServing` manages a hierarchy where only the top and bottom layers are Kubernetes objects today, and recovery
operates at **two nested levels**:

```text
ModelServing (CR)
  └─ ServingGroup (logical today)        ← Level 1: ServingGroupRecreate
       └─ Role instance (logical today)  ← Level 2: RoleRecreate
            ├─ entry Pod   (K8s Pod, owned by ModelServing)
            └─ worker Pods (K8s Pods, owned by ModelServing)
```

Because members of a Role instance or ServingGroup are tightly coupled (a freshly created worker cannot simply rejoin a
running entry Pod), `RoleRecreate` and `ServingGroupRecreate` require tearing down and recreating the entire unit when a
member is lost.

### One observation, two opposite right answers

Suppose `llama-0-prefill-0` expects an entry Pod (`-0`) and a worker Pod (`-1`). The controller reconciles and sees only
`-0` exists—`-1` is missing:

```text
Cluster state observed:  [ llama-0-prefill-0-0 (Running) ]   llama-0-prefill-0-1 is missing

  History A (still starting up):
    Worker -1 has not been created yet         ──▶  Correct action: create ONLY worker -1

  History B (was complete, then lost a worker):
    Instance was complete; -1 was lost         ──▶  Correct action (RoleRecreate):
                                                    delete -0 and recreate the whole instance
```

The Pods currently in the cluster are **identical** in both histories.

### Three facts the current Pods cannot tell the controller

To handle multi-Pod units reliably across restarts, the controller needs three pieces of information:

| Fact needed | Plain-English question | Status upstream |
| --- | --- | --- |
| **1. Instance identity** | Pod names are positional (`<ms>-<group>-<role>-<index>-<n>`) and reused. *Which generation of that name does this Pod belong to?* | **In progress (#2112 / PR #2126):** immutable `group-instance-id` and `role-instance-id` labels on Pods, Services, and PodGroups, with UID/ID-fenced deletes. We think this is the right solution for identity. |
| **2. Was-complete ("established")** | *Was this unit ever complete before, or is it still being created for the first time?* | **Open.** |
| **3. In-progress deletion ("committed scope")** | *Had we already decided to tear down this whole unit (and which incarnation of it) before a restart?* Halfway through deletion, the surviving Pods look just like a unit with missing members. | **Open.** |

> **Scope note:** #1999 (merged) exits the controller manager on lease loss, and #2112 / PR #2126 solve **#1
> (identity)** cleanly without new CRDs or status writes. As far as we can tell from reading PR #2126 as it stands
> today, **#2 and #3 remain open**: a worker lost while the controller is offline (with the entry Pod still present and
> nothing terminating) is still refilled one-by-one, and the scope of an in-progress deletion is re-derived from policy
> on restart. Note that **our target design still allows the constraint that a Pod lost while the controller is offline
> is refilled rather than recreated**—of course, it is even better if a solution handles that too.

### What goes wrong on `main` today

Because these facts live in controller memory and informer callbacks on `main`:

1. **Offline losses are refilled instead of recreated:** If a node drains during a controller upgrade or leader failover
   and deletes a worker of a healthy instance, the new leader misses the delete event and simply recreates that one Pod.
   *(As noted above, our target design still permits this constraint, though solving it is better.)*
2. **Interrupted deletions get "repaired":** A controller restart mid-teardown loses the in-memory deletion maps, so the
   remaining Pods can be mistaken for a healthy unit missing its entry Pod and refilled instead of deleted (as described
   in #2112).
3. **Late delete events can kill healthy replacements** (#1957), because replacement detection relies on 1-second
   timestamp comparisons.
4. **Non-deterministic behavior:** The exact same cluster state triggers different actions depending on which events the
   active process happened to witness.

<details>
<summary>Details: where each fact lives on <code>main</code> (with code links)</summary>

#### Where facts live on `main` ([`4323a9bf`][main])

| Fact | Where it lives on `main` | What happens after restart or leader change |
| --- | --- | --- |
| A member was lost | Informer delete callback: [`deletePod`][main-deletepod] calls [`handleDeletedPod`][main-handledeleted], which deletes the Role or group | Events missed while down are gone; reconcile later sees a gap and creates one Pod |
| Group / Role instance state (`Creating`, `Running`, `Scaling`, `Deleting`) | In-memory [`datastore`][main-states] | Rebuilt by [`syncAll`][main-syncall] by replaying existing Pods as add events |
| Deletion targets and Role cleanups | Two `sync.Map`s: [`podDeletions` and `roleCleanups`][main-maps] | Lost; as #2112 notes, a deleting Role can be mistaken for a healthy Role missing its entry Pod and refilled |
| Failure grace-period start time *t* | [`graceMap`][main-maps] and a [sleeping goroutine][main-grace] | Lost; the grace period restarts from zero |
| Successor reusing a Pod name | [`recreatedSinceDeletion`][main-recreated] compares 1-second timestamps | #1957: a late delete event can delete the healthy replacement |
| Temporary surge capacity | An [annotation][main-surge] on each Pod of the unit | Persisted, but can exist on only some Pods, and a unit with no Pods has none |

</details>

---

## 2. Why Can't We Just Store This on the Pods?

A natural first question is: *"Why not put `established` or `deleting` annotations/labels on the Pods themselves,
just like #2126 does for identity?"*

The catch is fundamental: **"established" and "committed deletion" describe the unit *before* a member vanished.** If
that record is stored only on the member Pods, and members can disappear without a trace, the record is vulnerable to
the very event it is meant to survive.

Three intuitive Pod-only approaches all break on concrete failure sequences:

- **Mark `established` on the entry Pod only** → breaks if the entry Pod itself is lost (a half-created unit that loses
  its entry Pod leaves the exact same surviving worker as a complete unit that loses its entry Pod and another worker).
- **Treat the first `DELETE` as the commit** → breaks if the controller crashes after deleting the entry Pod and
  `recoveryPolicy` is changed to `None` before it comes back.
- **Remove a `surge` annotation Pod-by-Pod** → breaks if the controller crashes mid-promotion and the last still-marked
  worker disappears, making the unit look promoted even though the final write never finished.

<details>
<summary>Details: exact failure sequences for the three Pod-only schemes</summary>

| Record on the Pods | Sequence that breaks it |
| --- | --- |
| An `established` mark on the entry Pod only ("entry missing, workers present" = loss) | **History 1:** entry and worker 1 exist, worker 2 not yet created; entry is deleted. **History 2:** instance was complete; entry and worker 2 are lost. Both leave only worker 1, but History 1 should complete creation while History 2 should recreate the instance. |
| The first `DELETE` is the commit | Whole-unit deletion is decided, entry Pod is deleted, controller crashes, policy changes to `None`. The new leader cannot distinguish "finish committed deletion" from "missing entry Pod under `None` — recreate just the entry." |
| Surge mark removed Pod-by-Pod ("surge while any Pod carries it") | Mark is removed from entry Pod, controller crashes, last marked worker vanishes. The unit now reads as promoted even though the final unmark never occurred. |

</details>

We have not proven that no Pod-only scheme could ever work—only that these three fail. Three mechanisms *do* satisfy the
requirement that the evidence outlives a lost member:

1. **Pod finalizers** (hold deleted Pods until the controller records the loss): **Rejected.** If the controller is down
   or uninstalled, protected Pods stay `Terminating`, blocking namespace deletion and foreground deletion of the
   `ModelServing`.
2. **A record outside the Pods:** Either a per-group object (**Plan A**) or fields in `ModelServing.status` (**Plan B**).
3. **One member owns the others (LeaderWorkerSet model):** Entry Pod owns the workers, so losing the entry Pod *is*
   losing the unit and the record can live on it. However, even in LWS the lower layer beneath the leader Pod still
   cannot be independently recognized as its own recovery unit. Moreover, **ModelServing has two nested recreation
   levels** (`RoleRecreate` and `ServingGroupRecreate`): entry-Pod ownership only covers a single layer, forces
   recreation on entry Pod loss under *every* policy (including `None`), and does not cover cross-Role `ServingGroup`s.

---

## 3. Plan A vs. Plan B vs. Plan C

Here is how the baseline (#2126) compares with **Plan A** and **Plan B** at a glance:

| Dimension | Labels on children only (#2126) | Plan A: `ServingGroup` CR | Plan B: Ledger in `ModelServing.status` |
| --- | --- | --- | --- |
| **Instance identity** | Yes, both levels | Group: UID · Role instance: by rules | By rules, both levels |
| **"Established" across restart** | No | Yes | Yes |
| **Committed deletion across restart** | Re-derived from Pods & policy | Yes | Yes |
| **Survives member loss** | n/a | Yes | Yes |
| **Role-instance level** | Same mechanism as group | Ledger in `ServingGroup.status`, by rules | Same mechanism as group |
| **Cross-group rollout budget** | As today | Sum across objects (non-atomic) | Can be one conditional write |
| **Status write scope** | No new status writes | One group | Entire `ModelServing` |
| **Single-group GC while kthena is down** | Waits for kthena | Garbage collector finishes it | Waits for kthena |
| **Upgrading running workloads** | No Pod owner change | Live Pod owner takeover, exclusive cutover, dedicated rollback | No Pod owner change; old binaries wipe unknown status fields |
| **New API surface** | Labels + 1 validating webhook | CRD, RBAC, second controller | New fields in `ModelServing.status` |
| **Validation so far** | PR under review | Prototype tried (e2e and upgrade drills) | Design estimate only |

---

### Plan A: `ServingGroup` as a CRD

**How it works:** Introduce a `ServingGroup` CRD. The `ModelServing` controller manages the count and revision of
`ServingGroup` objects; a second controller reconciles one group at a time and owns its Pods. Group-level facts live on
the `ServingGroup` object, while Role-instance facts live in `ServingGroup.status.roleInstances[]` (phase
`Creating`/`Established`/`Draining` + generation).

We tried implementing a prototype of this approach to evaluate its practical tradeoffs.

**Pros:**
- Each group gets a native Kubernetes **UID** (distinct owners across same-name incarnations).
- Deleting a group is a foreground delete of one object—**Kubernetes GC finishes cleanup even if kthena is offline** (as
  long as the `ModelServing` still exists).
- Per-group `status` and queue key: each status write is scoped to one group.
- Operational visibility: `kubectl get servinggroup` shows one row per group; `kubectl delete servinggroup` rebuilds a
  single group.

**Cons and tradeoffs:**

1. **It only solves half the problem — the Role layer still needs a rule-based ledger.**
   Because ModelServing has **two recreation levels**, giving `ServingGroup` a CRD still leaves the lower Role-instance
   layer without one:

   ```text
   ServingGroup (CRD)         ──▶ gets UID, dedicated status, and K8s GC
     └─ Role instance (logic) ──▶ NO UID, NO GC; still managed via status ledger + cleanup hooks
   ```

   As a result, Plan A has to maintain **both** mechanisms—Role-level operations and in-place updates do not benefit from
   the CRD. In our prototype, hand-maintaining a generation counter in the Role ledger exposed this directly: scaling an
   instance down removed its ledger entry, so scaling that index back up restarted at generation `0`, and a delayed
   cleanup hook from generation `1` deleted both headless Services of the new generation `0` instance in unit tests with
   a fake API server (on a real cluster this requires a stale leader acting after losing its lease; later reconcile
   rounds recreate the Services while Pods keep their UIDs, with duration/traffic impact unmeasured). Random
   per-incarnation IDs compared for equality (as in #2126) fix this—showing that **a `ServingGroup` CRD does not
   eliminate rule-based identity and cleanup at the Role level**, and coordinated Role rollout rules remain unchanged.

2. **Invasive migration and webhook conflict:**
   Running Pods are currently owned by `ModelServing`. Upgrading requires changing every Pod's controller owner in
   place, preventing old and new controllers from overlapping, and maintaining a dedicated rollback path. (In Kind
   upgrade drills from v1.1.0 and `main` using placeholder images, Pods kept their UIDs and restart counts, but live
   traffic impact was not measured.) In addition, PR #2126's validating webhook rejects modifying a Pod's `ModelServing`
   controller owner—directly conflicting with live takeover unless one side changes.

3. **Two-controller coordination overhead and a new empty state:**
   Splitting one controller into two introduces spec/status lag and a state that does not exist logically: **a
   `ServingGroup` object exists before any of its Pods do.** Several late defects stemmed directly from this (a group
   recreating old instance counts from a one-step-stale spec; needing an annotation plus trust rules to know whether a
   group was born in the current rollout; multiple iterations to get surge-group reclamation right because newly created
   surge groups also have no Pods).

4. **Non-atomic cross-group budget and GC coupling:**
   `maxUnavailable` becomes a sum across objects (two overlapping processes can double-spend budget, relying on leader
   election to shrink the window), and recreating a group under the same name depends on garbage collection timing.

<details>
<summary>Details: measurements from trying the Plan A prototype</summary>

| Measurement | Observed result |
| --- | --- |
| `make test-e2e-controller-manager` | Passes (50 tests) |
| API write volume in one e2e run | 164 `ServingGroup`s updated **1,573** times vs. **668** updates of 51 `ModelServing`s (**2.35×**). 1,427 of the 1,573 are `status` writes across 360 Pods (~4 status writes per Pod over its life) |
| `ServingGroup` object size (≤ 4 Role instances) | 1.3–1.6 KB; each ledger entry is 48–63 bytes (compact JSON, excluding `managedFields`) |
| Large groups (many Role instances) | Not tested (largest group had 6) |
| Parallel group throughput vs. single queue key | Not measured; no data yet showing a concrete benefit |

</details>

---

### Plan B: Keep `ServingGroup` Logical, Store the Ledger in `ModelServing.status`

**How it works:** Keep one controller and keep Pods owned by `ModelServing`. Add a table in `ModelServing.status` with
one entry per Role instance plus any group-level fields needed. *(Not implemented.)*

**Pros:**
- **No Pod ownership migration** (though upgrades still require a schema change and handling old/new controller overlap;
  see below).
- **Single writer, single snapshot:** No two-controller handoff lag and no "group object without Pods" state.
- **Atomic cross-group budgeting:** Spending rollout budget across groups can be a single conditional write.

**Cons and risks:**
- **Status size and quadratic write amplification at scale:** Every change rewrites the entire ledger and broadcasts it
  to every watcher of the `ModelServing`. A `ModelServing` object is ~2 KB today. While the object stays well below
  etcd's size limit even at 1,000 Role instances (~48–63 KB ledger only, ~128–153 KB estimated with `managedFields`),
  total bytes written across a full rollout grow **quadratically** with instance count—under a simple model, surpassing
  Plan A at a few dozen instances, ~6× at 300, and ~20× at 1,000. *(Estimates only; Plan B has not been measured.)*
- **Shared `resourceVersion` contention:** All groups share one `ModelServing` version, requiring either a single
  serialized writer or conflict retries.
- **Old binaries silently erase the ledger:** A controller binary that does not know the new status fields drops them on
  its next `UpdateStatus`. Zero values must therefore be safe, and an in-flight committed deletion can still be
  interrupted during mixed-version upgrades or rollbacks.
- **Rules at both levels and tension with #2112:** There is no group UID to lean on, and #2112 explicitly aims to avoid
  extra writes and schema changes in `ModelServing.Status`.

<details>
<summary>Details: estimated <code>ModelServing.status</code> size and rollout write-volume model</summary>

Using the ledger entry size measured on the Plan A prototype (48–63 bytes compact JSON, excluding `managedFields`) and
an estimate for `managedFields`:

| Role instances in one `ModelServing` | Ledger only | Estimated with `managedFields` |
| --- | --- | --- |
| 100 | 5–6 KB | 13–15 KB |
| 300 | 14–19 KB | 38–46 KB |
| 1,000 | 48–63 KB | 128–153 KB |

**Write-volume comparison assumptions:** A simple model assuming 2 non-mergeable ledger writes per instance and 2 Pods
per instance for Plan B, compared against Plan A's measured ~4 status writes per Pod and small-group object size. Under
this model, Plan B crosses Plan A at a few dozen instances, reaches ~6× at 300 instances, and ~20× at 1,000 instances.
Write batching and larger group sizes would shift these numbers substantially; Plan B has not been implemented or
measured.

</details>

---

### Plan C: Does a Third Option Exist?

Plan A introduces a CRD, live Pod owner migration, and two-controller complexity while still leaving the Role layer to a
status ledger. Plan B keeps a single controller without ownership migration, but faces status-size growth, quadratic
write amplification, and shared `resourceVersion` contention as Role-instance counts scale.

This raises an open question: **is there a Plan C that avoids both sets of tradeoffs?** We do not have a concrete Plan C
to propose yet and would like to explore whether one exists.

---

## 4. How Comparable Projects Handle This

We read the source code, docs, and issues of three peer projects that manage multi-Pod units with reused names:
**RoleBasedGroup** ([`sgl-project/rbg`][rbg] @ `a19d2c83`), **NVIDIA Dynamo Grove** ([`ai-dynamo/grove`][grove] @
`15411d9c`), and **LeaderWorkerSet / DisaggregatedSet** ([`kubernetes-sigs/lws`][lws] @ `1cb7505e`). *(Static reading
only; nothing was run. Full code-referenced notes are in
[`modelserving-unit-state-discussion-peers.md`](modelserving-unit-state-discussion-peers.md).)*

### Five key takeaways

All three projects ran into the exact same three problems—many of them in issues from the last few months:

1. **Adding a CRD at a level does not automatically fix these gaps.** RBG models every level as a CRD, yet still refills
   a Pod lost while the controller is offline ([rbg#562][rbg562], [rbg#566][rbg566])—a constraint our target design also
   still allows, though solving it is even better—and deletes before recording by default. In the paths we read, what
   actually closed gaps was: (a) a durable "established" record that outlives the lost member, (b) UID preconditions on
   destructive writes, and (c) persisting deletion intent before the first delete.
2. **Every project still has a non-object layer handled by rules**—RBG's components, Grove's replica indices, and LWS's
   workers—mirroring Plan A's Role-instance tradeoff.
3. **LWS anchors on a leader Pod, but still cannot independently recognize the lower layer—and ModelServing has two
   recreation levels.** LWS's leader Pod owns the worker StatefulSet, provides the UID, carries the record, and commits
   group teardown via foreground delete + GC. Even so, the worker layer beneath the leader still lacks independent unit
   recognition (offline worker loss was only recently addressed on `main` via a worker-UID digest on the leader Pod in
   [lws#1319][lws1319], unreleased). Unlike LWS and DisaggregatedSet—which only handle a single recreation layer and
   have no cross-Role recreation unit—**ModelServing has two nested recreation levels** (`RoleRecreate` and
   `ServingGroupRecreate`). Anchoring on the entry Pod would also force recreation on entry Pod loss under *every*
   policy (including `None`). Notably, LWS maintainers also rejected per-worker finalizers for the same reason we did
   ([lws#1319 review][lws1319-review]).
4. **None of the three puts a full per-unit ledger in parent status.** Grove stores only in-flight selections; LWS caps
   two opt-in JSON annotation maps per LWS (~221 KiB at 4,000 groups vs. a 256 KiB limit). This reinforces our concern
   about Plan B at scale.
5. **Timestamp heuristics proved fragile, and in-place Pod owner takeover is avoided.** Grove replaced a timestamp
   heuristic that killed healthy replicas ([grove#789][grove789]) with durable `lastScheduled`; in the migrations we
   read, none changed the owner of running Pods in place (RBG recreated, Grove patched metadata, LWS rolls groups; we
   did not exhaust the full history of all three projects).

### Summary across projects

| Dimension | RoleBasedGroup (RBG) | NVIDIA Dynamo Grove | LeaderWorkerSet / DisaggregatedSet |
| --- | --- | --- | --- |
| **Object hierarchy** | CRDs at every level (`RBGSet` → `RBG` → `RIS` → `RI`) | CRDs for `PCS`, `PCSG`, `PodClique`, `PodGang`, `PodGangMap`; **replica indices** (gang-termination units) are not objects | `LWS` & `DisaggregatedSet`; group is a leader Pod owning a worker StatefulSet; `DisaggregatedSet` has no cross-role unit (one LWS per slice/rev/role) |
| **Identity** | Owner-UID Pod listing + same-name old-owner create block ([rbg#496][rbg496], [rbg#511][rbg511]). UID preconditions on restart/scale Pod deletes, but not on in-place update fallback or `RoleInstance` deletes | Owner-UID reads + random Pod names. No UID preconditions on deletes ([comment][grove-retry] accepts retried delete hitting new `PodClique`s). Name-keyed state failed twice ([grove#782][grove782], [grove#943][grove943]) | Leader Pod UID; queue key carries UID ([lws#998][lws998]). Slot-keyed counters broke across incarnations ([lws#1247][lws1247]); UID preconditions added post-v1.0.0 |
| **"Established"** | Durable `RoleInstance.status.lastReadyGeneration`, but loss is detected only from existing Pods—offline loss is still refilled ([rbg#562][rbg562], [rbg#566][rbg566]) | Durable `PodClique.status.lastScheduled` replaced timestamp heuristic ([grove#789][grove789], contributor: *"explicit, persisted evidence... is safer"*). Scaling group still uses creation + 5s | Previously none: offline worker loss didn't recreate group ([lws#1301][lws1301]). Fixed on `main` via [lws#1319][lws1319] (worker-UID digest + `started` flag on leader Pod; unreleased); lower worker layer still has no independent unit identity |
| **Committed deletion** | Persisted before first delete only with `maxGroupRestarts`; default path deletes first, writes status after | Scale-in/update selections written before delete; gang termination is *"action-first / flag-second"* (accepts one extra recreation after crash) | Foreground `DELETE` of leader Pod with UID precondition; GC completes scope |
| **Per-unit parent state** | None; cost shifts to cross-level reads/writes ([rbg#520][rbg520]) | Only in-flight scale-in/update indices (per-`PodGang` status list was removed unused). [grove#781][grove781] reports ~35% churn regression pointing to one extra child status write | Two per-LWS JSON annotation maps (revision/index → restart counts & charged leader) for opt-in budget/backoff (~221 KiB @ 4,000 groups vs. 256 KiB cap) |
| **Model migration** | By recreation (downtime window; no migration tool by design) | In-place metadata patches behind a status gate | Groups roll; `DisaggregatedSet` never adopts existing LWS |
| **Coordinated rollout** | Open stall between role dependencies and `maxSkew` ([rbg#638][rbg638]) | Stalls and premature "completed" ([grove#786][grove786], [grove#916][grove916], [grove#952][grove952]) | Replans from children each reconcile; open deadlock ([lws#1292][lws1292]); no "stalled" signal |

---

## 5. Conclusion

Managing `ModelServing`'s two nested recreation levels (`ServingGroup` and `Role` instances) reliably across restarts
requires durable unit state beyond #2126's instance-identity labels. Neither **Plan A (`ServingGroup` CR)** nor
**Plan B (`ModelServing.status` ledger)** is clearly ideal—Plan A is invasive and still leaves the Role layer to a
ledger, while Plan B faces status-size growth and write amplification at scale.

We need to discuss and solve this problem together: between **Plan A**, **Plan B**, and whether a **Plan C** exists,
what is the right solution for `ModelServing`?

[main]: https://github.com/volcano-sh/kthena/tree/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89
[main-deletepod]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L462-L509
[main-handledeleted]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2355-L2386
[main-recreated]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L511-L529
[main-syncall]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L791-L810
[main-maps]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L114-L119
[main-grace]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/model_serving_controller.go#L2250-L2322
[main-states]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/datastore/store.go#L84-L97
[main-surge]: https://github.com/volcano-sh/kthena/blob/4323a9bfb8f5ea96d37a2e057277d6e2e92e9d89/pkg/model-serving-controller/controller/coordinated_role_observation.go#L179
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
