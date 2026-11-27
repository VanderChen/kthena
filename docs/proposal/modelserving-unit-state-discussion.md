# [Discussion] ModelServing Durable Unit State: ServingGroup CR (Plan A), ModelServing.status (Plan B), or a Plan C?

> **English** | [中文](./modelserving-unit-state-discussion.zh-CN.md)
>
> **Draft of a GitHub Discussion for `volcano-sh/kthena`.** There is no upstream proposal for a `ServingGroup` CR or a
> `ModelServing.status` ledger yet. Before proposing any API change, we would like to align with the community on the
> right direction: **Plan A (CR)**, **Plan B (`ModelServing.status`)**, or a **Plan C** we have not fully explored yet.
> Because `CONTRIBUTING.md` recommends bringing large design questions to Discussions first, this is written as a
> Discussion; the same structure also fits the Question issue template (TL;DR and Sections 1–5 describe the problem and
> options, Section 6 gives our take and questions, and Section 7 lists the environment and references).
>
> Code links to `main` are pinned to [`4323a9bf`][main]. Links to our exploratory Plan A prototype on a fork are pinned
> to [`98fcbf01`][proto].

---

## TL;DR

- **The problem:** `ModelServing` recovery (`RoleRecreate`, `ServingGroupRecreate`) and rolling updates operate on
  multi-Pod units (`ServingGroup` and `Role` instances), not single Pods. While #2112 / PR #2126 cleanly solves
  **instance identity**, two restart-sensitive facts still live only in controller memory and watch events: whether a
  unit was ever complete (*established*), and whether tearing down the whole unit was already decided (*committed
  deletion*). As a result, a controller restart or leader failover can cause under-recovery (refilling one Pod instead
  of recreating the unit) or abandon an in-progress deletion.
- **The architectural choice — Plan A vs. Plan B vs. Plan C:**
  - **Plan A (`ServingGroup` CR):** Gives each group its own UID, status write scope, and K8s GC—*but* requires live
    Pod owner migration, splits reconciliation across two controllers, and **only solves the group layer** (Role
    instances inside a group still need a status ledger). We prototyped Plan A on a fork to measure these tradeoffs.
  - **Plan B (`ModelServing.status` ledger):** Keeps a single controller and avoids Pod owner migration—*but* rewrites
    the whole table on one shared `resourceVersion`, creating quadratic rollout write amplification at larger
    Role-instance counts and conflicting with #2112's goal of avoiding `ModelServing.status` writes/schema changes.
  - **Plan C (is there a better third way?):** Could an alternative avoid both traps—for example, a **record-only group
    CR** (single controller, no Pod ownership takeover), **entry-Pod ownership** (LeaderWorkerSet style), a
    **compressed/sharded ledger**, or another design the community prefers?
- **What we want to align on before proposing any API change:**
  1. **Guarantees:** Should recovery and in-progress unit deletions survive controller restarts/failovers, or are the
     current gaps acceptable?
  2. **Direction:** If those guarantees are wanted, should Kthena pursue **Plan A (CR)**, **Plan B
     (`ModelServing.status`)**, or a **Plan C**?
  3. **What can land first:** Regardless of Plan A/B/C, can we start with storage-independent improvements (helping with
     #2126, refactoring the reconcile loop to be level-triggered, fixing rollout reporting and dropped delete events)?

---

## 1. The Problem: Why Pods Alone Aren't Enough

A `ModelServing` manages a hierarchy where only the top and bottom layers are Kubernetes objects today:

```text
ModelServing (CR)
  └─ ServingGroup (logical today)        ← ServingGroupRecreate acts here
       └─ Role instance (logical today)  ← RoleRecreate acts here
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
    Instance was complete; -1 was lost while
    the controller was restarting/failing over ──▶  Correct action (RoleRecreate):
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

> **Scope of this discussion:** #1999 (merged) exits the controller manager on lease loss, and #2112 / PR #2126 solve
> **#1 (identity)** cleanly without new CRDs or status writes. As far as we can tell from reading PR #2126 as it stands
> today, **#2 and #3 remain open**: a worker lost while the controller is down (with the entry Pod still present and
> nothing terminating) is still refilled one-by-one, and the scope of an in-progress deletion is re-derived from policy
> on restart. This discussion is about whether upstream wants guarantees for #2 and #3, and if so, where that state
> should live.

### What goes wrong on `main` today

Because these facts live in controller memory and informer callbacks on `main`:

1. **Offline losses are under-recovered:** If a node drains during a controller upgrade or leader failover and kills a
   worker of a healthy instance, the new leader misses the delete event and simply recreates that one Pod—violating
   `RoleRecreate`/`ServingGroupRecreate` during the exact window when node drains often happen.
2. **Interrupted deletions get "repaired":** A controller restart mid-teardown loses the in-memory deletion maps, so the
   remaining Pods can be mistaken for a healthy unit missing its entry Pod and refilled instead of deleted (as described
   in #2112).
3. **Late delete events can kill healthy replacements** (#1957), because replacement detection relies on 1-second
   timestamp comparisons.
4. **Non-deterministic behavior:** The exact same cluster state triggers different actions depending on which events the
   active process happened to witness.

<details>
<summary>Details: where each fact lives on <code>main</code> (with code links) and prototype conformance numbers</summary>

#### Where facts live on `main` ([`4323a9bf`][main])

| Fact | Where it lives on `main` | What happens after restart or leader change |
| --- | --- | --- |
| A member was lost | Informer delete callback: [`deletePod`][main-deletepod] calls [`handleDeletedPod`][main-handledeleted], which deletes the Role or group | Events missed while down are gone; reconcile later sees a gap and creates one Pod |
| Group / Role instance state (`Creating`, `Running`, `Scaling`, `Deleting`) | In-memory [`datastore`][main-states] | Rebuilt by [`syncAll`][main-syncall] by replaying existing Pods as add events |
| Deletion targets and Role cleanups | Two `sync.Map`s: [`podDeletions` and `roleCleanups`][main-maps] | Lost; as #2112 notes, a deleting Role can be mistaken for a healthy Role missing its entry Pod and refilled |
| Failure grace-period start time *t* | [`graceMap`][main-maps] and a [sleeping goroutine][main-grace] | Lost; the grace period restarts from zero |
| Successor reusing a Pod name | [`recreatedSinceDeletion`][main-recreated] compares 1-second timestamps | #1957: a late delete event can delete the healthy replacement |
| Temporary surge capacity | An [annotation][main-surge] on each Pod of the unit | Persisted, but can exist on only some Pods, and a unit with no Pods has none |

#### Conformance comparison: `main` vs. prototype

We ran our ModelServing rollout/recovery conformance suite ([`kthena-rollout-runner`][runner]) on Kind (Kubernetes
v1.37.0, Volcano 1.16.0) comparing `main` at `bc26f686` against the prototype at `9ac97725`:

- **778 cases** with a verdict on the prototype: **690 pass**, **70 fail**, **18 inconclusive**.
- **93 cases** fail on `main` and pass on the prototype (**59** of them in the recovery suite).
- **68 cases** fail on both.
- **2 cases** pass on `main` and fail on the prototype (both timing-related; both pass when rerun alone).

*Caveat:* This measures the prototype as a whole—it does not isolate how much came from the CRD versus the
level-triggered reconcile refactor—and the conformance contract is ours, not upstream's.

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
2. **A record outside the Pods:** Either a per-group object (**Plan A**) or fields in `ModelServing.status` (**Plan B**),
   or a hybrid variant (**Plan C**).
3. **One member owns the others (LeaderWorkerSet model):** Entry Pod owns the workers, so losing the entry Pod *is*
   losing the unit and the record can live on it. Today in ModelServing, entry and worker Pods are siblings owned by
   `ModelServing`. Adopting this would mean losing the entry Pod recreates the instance under *every* policy (including
   `None`), and still does not cover cross-Role `ServingGroup`s.

---

## 3. Plan A vs. Plan B vs. Plan C

Here is how the baseline (#2126) compares with **Plan A** and **Plan B** at a glance (potential **Plan C** options are
listed right after):

| Dimension | Labels on children only (#2126) | Plan A: `ServingGroup` CR (prototyped on fork) | Plan B: Ledger in `ModelServing.status` (not implemented) |
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
| **Validation so far** | PR under review | Fork prototype: e2e, conformance suite, Kind upgrade drills | Design estimate only |

---

### Plan A: `ServingGroup` as a CRD

**How it works:** Introduce a `ServingGroup` CRD. The `ModelServing` controller manages the count and revision of
`ServingGroup` objects; a second controller reconciles one group at a time and owns its Pods. Group-level facts live on
the `ServingGroup` object, while Role-instance facts live in `ServingGroup.status.roleInstances[]` (phase
`Creating`/`Established`/`Draining` + generation).

To evaluate this path with concrete data, we built a prototype on a fork: [branch][proto] (+51,837/−31,595 lines across
268 files vs. `bc26f686`), [proposal][proto-proposal], [design notes][design] (Chinese).

**Pros:**
- Each group gets a native Kubernetes **UID** (distinct owners across same-name incarnations).
- Deleting a group is a foreground delete of one object—**Kubernetes GC finishes cleanup even if kthena is offline** (as
  long as the `ModelServing` still exists).
- Per-group `status` and queue key: each status write is scoped to one group.
- Operational visibility: `kubectl get servinggroup` shows one row per group; `kubectl delete servinggroup` rebuilds a
  single group.

**Cons and tradeoffs surfaced by prototyping:**

1. **It only solves half the problem — the Role layer still needs a rule-based ledger.**
   This is the key structural limitation of Plan A:

   ```text
   ServingGroup (CRD)         ──▶ gets UID, dedicated status, and K8s GC
     └─ Role instance (logic) ──▶ NO UID, NO GC; still managed via status ledger + cleanup hooks
   ```

   As a result, Plan A has to maintain **both** mechanisms—and its own design notes acknowledge that the Role layer and
   in-place updates do not benefit from the CRD. In practice, hand-maintaining a generation counter in the Role ledger
   caused a subtle bug: scaling an instance down removed its ledger entry ([`ledger.go`][proto-ledger-remove]), so
   scaling that index back up restarted at generation `0` ([`ledger.go`][proto-ledger-gen]). A delayed cleanup hook from
   generation `1` then deleted both headless Services of the new generation `0` instance
   ([`headless_service_plugin.go`][proto-plugin]). We reproduced this in unit tests (plugin and group controller with a
   fake API server; on a real cluster it would require a stale leader acting after losing its lease, so we haven't seen
   it on a cluster; later reconcile rounds recreate the Services while Pods keep their UIDs, with duration/traffic
   impact unmeasured). The fix is random per-incarnation IDs compared for equality—just like #2126. The takeaway is
   that **having a `ServingGroup` CRD does not eliminate rule-based identity and cleanup at the Role level**, and
   coordinated Role rollout rules remain unchanged.

2. **Invasive migration and webhook conflict:**
   Running Pods are currently owned by `ModelServing`. Upgrading requires changing every Pod's controller owner in
   place, preventing old and new controllers from overlapping, and maintaining a dedicated rollback path. (We ran
   upgrade drills on Kind from v1.1.0 and `main` using placeholder images—Pods kept their UIDs and restart counts, but
   live traffic impact was not tested.) In addition, PR #2126's validating webhook rejects modifying a Pod's
   `ModelServing` controller owner—directly conflicting with live takeover unless one side changes.

3. **Two-controller coordination overhead and a new empty state:**
   Splitting one controller into two introduces spec/status lag and a state that does not exist logically: **a
   `ServingGroup` object exists before any of its Pods do.** Several late bugs stemmed directly from this (a group
   recreating old instance counts from a one-step-stale spec; needing an annotation plus trust rules to know whether a
   group was born in the current rollout; three iterations to get surge-group reclamation right because newly created
   surge groups also have no Pods; updating the conformance suite to tolerate Pod-less groups).

4. **Non-atomic cross-group budget and GC coupling:**
   `maxUnavailable` becomes a sum across objects (two overlapping processes can double-spend budget, relying on leader
   election to shrink the window), and recreating a group under the same name depends on garbage collection timing.

<details>
<summary>Details: measurements from the Plan A prototype</summary>

| Measurement | Observed result |
| --- | --- |
| Conformance suite (`main` vs. prototype) | See Section 1 details |
| `make test-e2e-controller-manager` | Passes (50 tests) |
| API write volume in one e2e run | 164 `ServingGroup`s updated **1,573** times vs. **668** updates of 51 `ModelServing`s (**2.35×**). 1,427 of the 1,573 are `status` writes across 360 Pods (~4 status writes per Pod over its life) |
| `ServingGroup` object size (≤ 4 Role instances) | 1.3–1.6 KB; each ledger entry is 48–63 bytes (compact JSON, excluding `managedFields`) |
| Large groups (many Role instances) | Not tested (largest group had 6) |
| Parallel group throughput vs. single queue key | Not measured; no data yet showing a concrete benefit |

</details>

---

### Plan B: Keep `ServingGroup` Logical, Store the Ledger in `ModelServing.status`

**How it works:** Keep one controller and keep Pods owned by `ModelServing`. Add a table in `ModelServing.status` with
one entry per Role instance plus any group-level fields needed. *(Not implemented yet.)*

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

### Plan C: Is There a Third Option?

Because both Plan A and Plan B carry significant structural tradeoffs, we also want to ask: **is there a Plan C that
fits Kthena better?** Below are four candidate directions we have *not* designed or tested yet, listed to spark
discussion or rule them out:

| Plan C candidate | Idea | What it avoids | Main tradeoff |
| --- | --- | --- | --- |
| **C1. Record-only group CR** | One object per group owned by `ModelServing`, read/written by the *single* `ModelServing` controller, **not** owning Pods | Avoids Plan A's Pod ownership migration & second controller, and avoids Plan B's parent-status write amplification | Still introduces a new CRD; gives up single-group Kubernetes GC while kthena is offline |
| **C2. Entry-Pod-anchored Role instance (LWS style)** | Entry Pod owns worker Pods, supplies UID, carries the record, and foreground-deleting it commits teardown | Eliminates the Role-instance ledger completely | Losing an entry Pod recreates the whole instance under *every* policy (including `None`); does not cover cross-Role `ServingGroup`s |
| **C3. Compressed ledger (Indexed Job style)** | Compress steady-state `established` instances into compact ranges in status | Shrinks steady-state status size | Conflicts with binding each record to a specific incarnation |
| **C4. Sharded ledger objects** | Split the status table across multiple helper objects | Reduces single-object write amplification | One step away from C1 (record-only group CR) |

If there is another **Plan C** (or a hybrid of the above, or simply accepting the restart gaps and stopping at #2126 +
reconcile refactoring), we would love to hear it.

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
   a Pod lost while the controller is away ([rbg#562][rbg562], [rbg#566][rbg566]) and deletes before recording by
   default. In the paths we read, what actually closed gaps was: (a) a durable "established" record that outlives the
   lost member, (b) UID preconditions on destructive writes, and (c) persisting deletion intent before the first delete.
2. **Every project still has a non-object layer handled by rules**—RBG's components, Grove's replica indices, and LWS's
   workers—mirroring Plan A's Role-instance tradeoff.
3. **LWS avoids a ledger by letting one member own the others.** Its leader Pod owns the worker StatefulSet, provides
   the UID, carries the record, and commits group teardown via foreground delete + GC. That only works because leader
   loss *means* group loss (which for ModelServing would force recreation on entry Pod loss even under `None`) and does
   not cover cross-Role units. Notably, LWS maintainers also rejected per-worker finalizers for the same reason we did
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
| **"Established"** | Durable `RoleInstance.status.lastReadyGeneration`, but loss is detected only from existing Pods—offline loss is still refilled ([rbg#562][rbg562], [rbg#566][rbg566]) | Durable `PodClique.status.lastScheduled` replaced timestamp heuristic ([grove#789][grove789], contributor: *"explicit, persisted evidence... is safer"*). Scaling group still uses creation + 5s | Previously none: offline worker loss didn't recreate group ([lws#1301][lws1301]). Fixed on `main` via [lws#1319][lws1319] (worker-UID digest + `started` flag on leader Pod; unreleased) |
| **Committed deletion** | Persisted before first delete only with `maxGroupRestarts`; default path deletes first, writes status after | Scale-in/update selections written before delete; gang termination is *"action-first / flag-second"* (accepts one extra recreation after crash) | Foreground `DELETE` of leader Pod with UID precondition; GC completes scope |
| **Per-unit parent state** | None; cost shifts to cross-level reads/writes ([rbg#520][rbg520]) | Only in-flight scale-in/update indices (per-`PodGang` status list was removed unused). [grove#781][grove781] reports ~35% churn regression pointing to one extra child status write | Two per-LWS JSON annotation maps (revision/index → restart counts & charged leader) for opt-in budget/backoff (~221 KiB @ 4,000 groups vs. 256 KiB cap) |
| **Model migration** | By recreation (downtime window; no migration tool by design) | In-place metadata patches behind a status gate | Groups roll; `DisaggregatedSet` never adopts existing LWS |
| **Coordinated rollout** | Open stall between role dependencies and `maxSkew` ([rbg#638][rbg638]) | Stalls and premature "completed" ([grove#786][grove786], [grove#916][grove916], [grove#952][grove952]) | Replans from children each reconcile; open deadlock ([lws#1292][lws1292]); no "stalled" signal |

---

## 5. What Can Land First (Regardless of Plan A, B, or C)

Several high-value improvements do **not** depend on choosing between Plan A, Plan B, or Plan C:

1. **Instance identity (PR #2126):** We can review #2126 against the edge cases we collected and contribute tests.
2. **Level-triggered reconcile refactor (no API change):** Although not measured separately, we believe most of our
   prototype's conformance gains came from two structural changes rather than the CRD itself:
   - Informer callbacks **only enqueue** (never mutate cluster state directly).
   - Each reconcile computes desired writes from a snapshot using pure functions: a shared planner for groups and Role
     instances ([`grouplevel`][proto-grouplevel], checked against 17,504 reference-model vectors) and a per-instance
     state transition function ([`ledger.go`][proto-ledger]) that reads unit state from an abstract store.
   This can land as a refactoring of the existing controller—keeping state where it is today with zero API changes—so a
   durable store can be plugged in cleanly later if desired. Because it is the largest piece, we want alignment before
   sending a PR series.
3. **Coordinated Role rollout reporting edge cases:** From reading
   [`role_rolling_update_coordinator.go`][main-coord] and running the same rules as ported into our prototype, two cases
   stop replacing instances while reporting misleading conditions:
   - *(a)* A low-ordinal old instance is unready (without a recovery-triggering fault), consuming the `maxUnavailable`
     budget while descending replacement order blocks passing a healthy higher-ordinal instance—yet the status reports
     `ProgressAvailable`.
   - *(b)* A Role's template is unchanged between baseline and target, while one instance still runs an intermediate
     revision—`inProgress` becomes `false` with reason `RolloutComplete`, which the [validating webhook][main-webhook]
     interprets as "no coordinated rollout in progress."
   *(We have not reproduced either on `main` or on a cluster yet and will file separate issues once we do.)*
4. **Dropped `DELETED` watch events:** If a watch misses a draining Pod's `DELETED` event, the informer cache keeps the
   Pod and the unit stays stuck deleting (this affects both `main` and our prototype). Units waiting for Pods to vanish
   need periodic direct API reads.
5. **Shared conformance suite:** Comparing any future proposals needs cases everyone trusts. We can offer
   [`kthena-rollout-runner`][runner] and its test cases as a starting point (noting the underlying contract is ours and
   would need community review).

---

## 6. Our Current Take & Questions for the Community

### Where we stand

- **No clear winner between Plan A and Plan B yet**—both carry real structural costs, which is why we want to discuss
  **Plan A vs. Plan B vs. a possible Plan C** before opening any CRD or status-schema proposal.
- **Identity belongs on child resources as random per-incarnation IDs** (PR #2126). We agree with this direction and
  would drop our prototype's generation counters in favor of it.
- **Choosing between Plan A, Plan B, and Plan C depends heavily on guarantees and scale.** Evidence for "established"
  and "committed deletion" must outlive a lost member (either outside the Pods, or on a member that owns the others at
  the cost of changing entry-Pod loss semantics). Plan A is heavy and still leaves Role instances to a ledger; Plan B is
  simpler to introduce but scales poorly if Role-instance counts are large. Hearing the community's expected scale and
  ideas for Plan C would help narrow the choice.
- **Preferred next step:** Land the storage-independent improvements in Section 5 first, and choose Plan A, B, or C with
  clearer requirements and data.

### Questions we would love feedback on

1. **Restart-safe guarantees:** Should a member lost while the controller is down trigger `recoveryPolicy`, and should a
   committed unit deletion finish after a controller restart—or are those gaps acceptable upstream?
2. **Plan A vs. Plan B vs. Plan C:** If those guarantees are required, which direction fits Kthena best: **Plan A**
   (`ServingGroup` CR), **Plan B** (`ModelServing.status`), one of the **Plan C** candidates (e.g., record-only group
   CR, entry-Pod ownership), or another **Plan C** we haven't thought of?
3. **Group CR on its own merits:** Independent of recovery state, is a `ServingGroup` CR wanted for its own sake (e.g.,
   to inspect, delete, and rebuild a single group via `kubectl`, or to let GC clean up a deleted group while kthena is
   offline)?
4. **Target scale:** How many `ServingGroup`s and `Role` instances per `ModelServing` should the design target?
5. **Level-triggered refactor first:** Would upstream welcome a series of PRs refactoring the `ModelServing` controller
   into a level-triggered planner/state machine *independent* of Questions 1–4?
6. **Role-level CRDs:** Does anyone see a strong reason to make Role instances CRDs as well? *(We do not.)*

---

## 7. Environment, References & Disclosure

- **Kthena:** `main` at [`4323a9bf`][main] (conformance comparison used `main` @ `bc26f686` vs. prototype @ `9ac97725`).
- **Test cluster:** Kind with Kubernetes v1.37.0, Volcano 1.16.0.
- **Prototype artifacts:** [branch][proto], [proposal][proto-proposal], [implementation design][proto-design] (Chinese),
  [design notes][design] (Chinese).
- **Conformance runner:** [`kthena-rollout-runner`][runner].
- **Peer-project notes:** [`modelserving-unit-state-discussion-peers.md`](modelserving-unit-state-discussion-peers.md).
- **Related issues / PRs:** #1957, #2112, #2126, #1999, #1950.

*Disclosure:* Parts of the prototype and of this analysis were produced with AI assistance. All measurements come from
Kind runs and dumped cluster objects, and code references can be verified at the pinned commits. Items noted as
unreproduced or unmeasured are explicitly marked as such.

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
