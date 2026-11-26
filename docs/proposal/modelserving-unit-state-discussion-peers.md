# Notes for the ModelServing unit-state discussion: how RBG, Grove and LeaderWorkerSet handle the same problems

Companion to [`modelserving-unit-state-discussion.md`](modelserving-unit-state-discussion.md), section 7. Three sets of
reading notes, one per project, all using the same seven questions:

- **P1** Identity across the recreation of a unit under the same name.
- **P2** "Established" against "still being created": what decides between creating the missing Pod and recreating
  the whole unit, and whether that evidence survives a controller restart.
- **P3** Commit of a scoped delete or recreate: where the decision is recorded before the first delete.
- **P4** Where per-unit state lives and how large or hot it is.
- **P5** The level below (or above) the object.
- **P6** Coordinated rolling update across roles.
- **P7** Upgrade and migration when the object model changed.

Nothing was run. Each note is pinned to one commit of the project and separates what was read in code from what is
stated in docs or issues and from what is inferred. The column "same gap as kthena?" compares with kthena `main` as it
is described in the discussion, not with a fresh reading of kthena code. The projects move fast: several of the fixes
cited here merged within days of the reading, and issue states are those of 2026-11-26.

These notes were produced with AI assistance and spot-checked against the pinned sources.


---

## Peer research — RoleBasedGroup (RBG): where the facts for recovering and rolling multi-Pod units live

Read on 2026-11-26. Static reading of source, docs and issues; nothing was run.

### 1. Header

- **Repo**: <https://github.com/sgl-project/rbg> — canonical (`gh api repos/sgl-project/rbg` returns `full_name: sgl-project/rbg`, not archived, no redirect). The Go module path is `sigs.k8s.io/rbgs`.
- **Commit read**: `main` @ `a19d2c83fc1aca4f57a23cf7eee56f687e2da023`, committed 2026-11-26T09:54:00Z (merge of PR #646).
- **Releases**: latest tag `v0.9.0-alpha.5` (pre-release, 2026-11-21, `0bdf2208c021`; head is 14 commits ahead). Latest stable `v0.8.0` (2026-08-31, `a146aa912e55`). The restart-policy behaviour described in P2/P3 (PRs #559, #573) is in `v0.9.0-alpha.4` and later, **not** in `v0.8.0` (checked with `gh api .../compare`).
- **Method**: no clone was made; files were streamed from `raw.githubusercontent.com` / `codeload` at the SHA and issues/PRs read with `gh api` (read-only). `_test.go` files were not read.
- **Read**: `api/workloads/v1alpha2/{roleinstance,roleinstanceset,rolebasedgroup,rolebasedgroupset}_types.go`; `pkg/reconciler/roleinstance/**` (reconciler, status, event handler, `sync/instance_scale.go`, `sync/restart_budget.go`, `sync/instance_update.go`); `pkg/reconciler/roleinstanceset/statefulmode/**` and parts of `statelessmode/**`; `internal/controller/workloads/{rolebasedgroup_controller,rolebasedgroup_rollout_status,rolebasedgroupset_rolling}.go`; `pkg/dependency`, `pkg/reconciler/{common,lws_reconciler,roleinstanceset_reconciler}.go`; KEPs 30-instanceset, 284, 455, 502, 576; `doc/features/failure-handling.md`, `doc/migration-v1alpha1-to-v1alpha2.md`; issues/PRs #186, #339/#340, #363, #380, #464, #482/#484, #487, #496/#497, #500, #507, #511/#512, #520, #542, #559, #562, #563/#573, #566, #582, #618, #638.
- **Evidence tags**: **[code]** read at the SHA · **[doc]** stated in a doc, KEP, issue or PR · **[inferred]** my deduction from the code; neither stated by the project nor observed.
- **Object model [code]**: `RoleBasedGroupSet → RoleBasedGroup (RBG) → per role: RoleInstanceSet (RIS) → RoleInstance (RI) → Pods`. Every level is a CR with its own controller. Rough mapping to kthena: ServingGroup ≈ RBG, Role ≈ RIS, Role instance ≈ RI, ModelServing ≈ RBGSet. A role can instead be backed by Deployment, StatefulSet or LeaderWorkerSet ([`workload_reconciler.go#L61-L72`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/workload_reconciler.go#L61-L72)); these are deprecated and compatibility-gated in 0.9.0, and the v1alpha1 `InstanceSet` has had no reconciler since v0.7.0 ([migration doc L20](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L20)).
- **Names are reused [code]**: stateful-mode RI = `<set>-<ordinal>` (the default mode, [`stateful_instance_set_utils.go#L71-L73`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_utils.go#L71-L73)); Pod = `<instance>-<index>` or `<instance>-<component>-<id>` ([`instance_utils.go#L75-L88`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/utils/instance_utils.go#L75-L88)). Only stateless-mode RIs get a random 5-character id ([`scale.go#L331-L340`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statelessmode/sync/scale.go#L331-L340)).
- **Verdict on the hypothesis** ("the same problems exist in RBG"): confirmed for P1, P2, P3 and P6 by RBG's own issues, most of them from Oct–Nov 2026. RBG has P1 largely solved at the Pod level and has the durable-latch half of P2; it has P3 solved only on an opt-in path, and P2(a) is still open.

### 2. Summary

| # | What RBG does | Durable across controller restart? | Evidence | Same gap as kthena? |
|---|---|---|---|---|
| P1 | Identity is the API-server UID of the owner, checked on the child: Pods are listed by owner-UID index, deleted with a UID precondition, and a same-name Pod of a previous UID blocks create and readiness until GC removes it. No epoch label on Pods. | UID checks: yes. Membership of a restart wave is an in-memory set of Pod UIDs; after a restart it falls back to `creationTimestamp < lastRestartTime`. | [`instance_reconciler.go#L329-L341`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_reconciler.go#L329-L341), [`instance_scale.go#L444-L481`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L444-L481), [#496](https://github.com/sgl-project/rbg/issues/496), [#511](https://github.com/sgl-project/rbg/issues/511) | **Partly.** Solved at Pod level, after incidents. RoleInstance deletes carry no UID precondition; the wave epoch is not durable. |
| P2 | "Established" is a persisted latch, `RoleInstance.status.lastReadyGeneration`; arming is selectable (`activateAfter: Ready` or `Created`). Loss is detected only from Pods that still exist (Terminating, Failed, container restarted). | Latch: yes. Signal for a Pod that vanished unobserved: no. | [`instance_scale.go#L1032-L1104`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L1032-L1104), [`failure-handling.md#L27-L33`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/features/failure-handling.md#L27-L33), [#562](https://github.com/sgl-project/rbg/issues/562), [#566](https://github.com/sgl-project/rbg/issues/566) | (a) **Yes**: offline loss is refilled; acknowledged and open. (b) **No**: not recreated by default; opt-in with `Created`. |
| P3 | Two paths. With `maxGroupRestarts`: an optimistic-lock status patch (count, `Restarting=True`, time) before the first delete, resumed after restart. Default: delete first, write status at the end of the reconcile. RIS rollout intent and RBGSet checkpoints are persisted before acting. | Budget path: yes. Default path: only once the post-delete status write lands. | [`restart_budget.go#L504-L546`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L504-L546), [`instance_scale.go#L334-L341`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L334-L341), [`instance_reconciler.go#L152-L166`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_reconciler.go#L152-L166), [#482](https://github.com/sgl-project/rbg/issues/482) | **Partly.** Solved on the opt-in path; the default path has the same window. |
| P4 | Per-unit state sits in the unit's own CR status (RI). Parents hold counters and O(1) records. Several safety helpers are in memory. | Mixed; see P4. | [`roleinstance_types.go#L144-L235`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstance_types.go#L144-L235), [`roleinstanceset_types.go#L161-L238`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstanceset_types.go#L161-L238), [#520](https://github.com/sgl-project/rbg/issues/520) | **No** per-unit list in a parent status. The cost appears as cross-level reads and writes instead. |
| P5 | Every level is an object: the group always was, the instance since KEP-30. Below RI, "components" are not objects and have no lifecycle of their own; the restart scope is always the whole RI. The group-scope recreate policy was removed. | n/a | [KEP-30 L46-L62](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/30-instanceset/README.md#L46-L62), [#339](https://github.com/sgl-project/rbg/issues/339), [`lws_reconciler.go#L310-L316`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/lws_reconciler.go#L310-L316) | **No** (different choice). Objects did not remove P1–P3. |
| P6 | Role `dependencies` gate plus `CoordinatedPolicy` maxSkew, turned into a per-reconcile computed partition; RIS partition / maxUnavailable / maxSurge; RBGSet request/acknowledge barrier. | Recomputed each reconcile from status. Barrier acknowledgement and RIS replacement scope are persisted. | [`rolebasedgroup_controller.go#L2041-L2203`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_controller.go#L2041-L2203), [`utils.go#L232-L237`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/utils/utils.go#L232-L237), [#638](https://github.com/sgl-project/rbg/pull/638), [#464](https://github.com/sgl-project/rbg/issues/464) | **Yes**: open stall and misleading-status issues. |
| P7 | Object-model changes were done by recreation: a zero-Pod window, no migration tool by design, v1alpha1 InstanceSet left unreconciled. Only the API-version change is in place (conversion webhook). | n/a | [migration doc L9](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L9), [L135-L139](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L135-L139), [L157-L161](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L157-L161) | n/a. RBG chose recreation. |

### 3. Per problem

#### P1 — Identity across same-name recreation

What the implementation does **[code]**:

- **Membership by owner UID.** The RI controller lists Pods through a field index on the owner UID and then drops non-controller references ([`instance_reconciler.go#L329-L341`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_reconciler.go#L329-L341)). A same-name Pod owned by a previous UID is never in the working set.
- **Deletes pinned to the incarnation** ([`instance_scale.go#L444-L481`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L444-L481)); it also refuses to delete a Pod it does not control:
  ```go
  // Only the UID precondition: it pins the Pod incarnation, which is what the
  // delete is addressed by (namespace + name), so a same-name replacement
  // created between the read and the delete is never removed. [...]
  opts.Preconditions = &metav1.Preconditions{UID: &uid}
  ```
- **Predecessor still present.** Before each create the controller GETs the name. A Pod controlled by a previous UID of the same-named RI, or terminating, yields `RecreatePodConflictError` ("awaiting garbage collection of the previous instance"), reported as reason `PodRecreateRace` and retried ([`instance_scale.go#L68-L91`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L68-L91), [`#L380-L399`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L380-L399), [`roleinstance_types.go#L372-L377`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstance_types.go#L372-L377)). Readiness rejects a same-name Pod that is not controlled by this UID or is terminating ([`instance_status.go#L322-L348`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_status.go#L322-L348)).
- **Events carry no decision.** The Pod handler only maps the controller reference to `namespace/name` and enqueues ([`instance_event_handler.go#L87-L124`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_event_handler.go#L87-L124)). A late delete event causes a fresh level-based reconcile; there is nothing to "apply" to a successor.
- **Above the Pod.** RBG refuses a workload controlled by another RBG UID ([`common.go#L131-L154`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/common.go#L131-L154)). RBGSet deletes a child with `client.Preconditions{UID, ResourceVersion}` and foreground propagation ([`rolebasedgroupset_rolling.go#L644-L648`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroupset_rolling.go#L644-L648)). A same-name RI successor cannot exist until the predecessor object is gone (name uniqueness; create swallows `AlreadyExists`, [`stateful_instance_control.go#L90-L99`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_control.go#L90-L99)).

These checks were added after incidents **[doc]**: [#496](https://github.com/sgl-project/rbg/issues/496) → [PR #497](https://github.com/sgl-project/rbg/pull/497) (readiness accepted the old UID's same-name Pod for a recreated RI; confirmed there with a fake-client test, while the early surge reclaim was a proposed explanation of an outage, not reproduced end to end); [#511](https://github.com/sgl-project/rbg/issues/511) → [PR #512](https://github.com/sgl-project/rbg/pull/512) (a same-name Pod of the deleted StatefulSet blocked creation with no retry); [PR #487](https://github.com/sgl-project/rbg/pull/487), [PR #500](https://github.com/sgl-project/rbg/pull/500) (workloads, RBGSet children, ScalingAdapter, PodGroup of a same-named predecessor); [#507](https://github.com/sgl-project/rbg/issues/507) is still open.

Residual gaps:

- **No epoch on members [code].** Pods carry no instance-generation label. A restart wave is an in-memory set of Pod UIDs keyed by the RI's `namespace/name` ([`sync/api.go#L69-L75`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/api.go#L69-L75), [`instance_scale.go#L852-L860`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L852-L860)). After a controller restart it degrades to `pod.CreationTimestamp.Before(status.lastRestartTime)`, which has second granularity ([`#L955-L979`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L955-L979)). The docs state the consequence: "allowing at most one extra backoff-bounded recreation" ([`failure-handling.md#L58-L68`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/features/failure-handling.md#L58-L68)). [#582](https://github.com/sgl-project/rbg/issues/582) (open) reports mixed generations inside one RI during a wave.
- **RoleInstance deletes are not pinned [code].** RIS deletes an RI by name with no precondition, in stateful mode ([`stateful_instance_control.go#L86-L88`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_control.go#L86-L88)) and stateless mode ([`scale.go#L277-L282`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statelessmode/sync/scale.go#L277-L282)). Pod deletes in the update path have none either ([`instance_update.go#L116-L121`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_update.go#L116-L121)). A deposed leader's in-flight delete could therefore hit a same-name successor RI **[inferred]**; I found no issue reporting it.
- `--leader-elect` defaults to `false` in the binary ([`main.go#L167-L170`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/cmd/rbgs/main.go#L167-L170)); there is no fencing beyond the checks above.

Confidence: high for the code paths; the consequence of the missing preconditions is inferred.

#### P2 — "Established" vs "still being created"

Decision inputs **[code]** ([`instance_scale.go#L1032-L1079`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L1032-L1079), [`#L1093-L1104`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L1093-L1104)):

1. a persisted latch, `status.lastReadyGeneration` ([`roleinstance_types.go#L197-L205`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstance_types.go#L197-L205));
2. a policy knob, `restartPolicyConfig.activateAfter`: `Ready` (default) or `Created` ([`rolebasedgroup_types.go#L188-L225`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroup_types.go#L188-L225));
3. the state of Pods that currently exist: a desired member with `deletionTimestamp`, phase `Failed`, or a container `restartCount > 0` not explained by recorded in-place-update baselines.

```go
if instance.Spec.GetRestartPolicyConfig().GetActivateAfter() == workloadsv1alpha2.RestartPolicyActivateAfterCreated {
	// Armed as soon as the instance exists; only the generation guard applies.
	return true
}
return wasInstanceReady(instance) ||
	instance.Status.LastReadyGeneration == instance.Generation ||
	hasRecentRestart(instance)
```

The latch is written when the RI is Ready with revisions converged, and the status writer only lets it advance ([`instance_status.go#L228-L232`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_status.go#L228-L232), [`#L602-L607`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_status.go#L602-L607)). A spec change invalidates it through the generation comparison. The stated reason for a latch **[doc]**: "Gating on the live `RoleInstanceReady` condition instead would be unsound: status is written after scaling, so a fault flips the condition to False before the restart signal becomes visible, and the policy would then never fire again" ([`failure-handling.md#L52-L56`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/features/failure-handling.md#L52-L56)). That was bug [#542](https://github.com/sgl-project/rbg/issues/542), fixed by [PR #559](https://github.com/sgl-project/rbg/pull/559) on 2026-11-11.

- **(a) Member lost while the controller is down, Pod fully gone: refilled, not recreated [code + doc].** `shouldRecreateInstance` only iterates over Pods that exist; an empty slot produces no trigger and `createPods` fills it ([`#L1053-L1076`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L1053-L1076), [`#L123-L126`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L123-L126)). The code says so: "Unobserved disappearance is deferred: https://github.com/sgl-project/rbg/issues/562". The docs repeat it ([`failure-handling.md#L27-L33`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/features/failure-handling.md#L27-L33)). [#562](https://github.com/sgl-project/rbg/issues/562) (open) lists "the controller is down, or failing over, while the Pod terminates", and a comment there by `cheyang` (listed as an approver in KEP metadata) gives this repro, derived from the code and not run: "stop the controller, delete a member of a Ready instance, wait until it's gone, and start the controller again; only that member is recreated". [#566](https://github.com/sgl-project/rbg/issues/566) (open) is the same loss when a Terminating Pod disappears during restart backoff; its reporter observed that one on a real cluster. The proposed fix, not merged, is to treat a missing desired member as lost when `status.lastReadyGeneration == metadata.generation`: "That is a level signal, so it also holds across controller restarts".
- **(b) Member lost during first creation.** With the default `Ready`, the triggers are not armed: Failed Pods are deleted and replaced, missing Pods are created, and the unit is not recreated **[code]**. With `Created`, the whole RI is recreated if the loss is observed. #562 notes the limit of that mode: "there is no latch before the first Ready, so a missing member can't be told apart from one that hasn't been created yet".
- **What does survive a restart:** the latch (RI status), and the two level signals held in Pod status, `Failed` phase and container restart count.
- **LWS-backed roles** delegate to LWS `RecreateGroupOnPodRestart` ([`lws_reconciler.go#L310-L316`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/lws_reconciler.go#L310-L316)); `activateAfter` and `maxGroupRestarts` are RIS-only ([`rolebasedgroup_validation.go#L63-L80`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroup_validation.go#L63-L80)).

Confidence: high. (a) rests on code, docs and two open issues; none of it was run by me.

#### P3 — Commit of a scoped delete/recreate

Two paths in the same package **[code]**:

- **Budgeted path** — taken only when `maxGroupRestarts` is set or budget state exists ([`restart_budget.go#L260-L267`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L260-L267)). `acceptBudgetRestart` reads the RI uncached, checks UID and generation, re-lists owned Pods live, re-evaluates the trigger, then writes `groupRestartCount`, `restartCount`, `lastRestartTime` and `Restarting=True` in one `Status().Patch` with `MergeFromWithOptimisticLock`, and only then returns the delete list ([`#L504-L546`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L504-L546), [`#L160-L196`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L160-L196)). Its comment: "Budget + Restarting + backoff are persisted before the caller deletes any Pod." A new process that reads `Restarting=True` calls `resumeBudgetRestartWave`, which re-lists, re-issues pending deletes, and persists completion before another rebuild can be accepted ([`#L354-L359`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L354-L359), [`#L479-L502`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L479-L502)).
- **Default path** — the condition and counters are set on the in-memory copy, the deletes run, and status is written at the end of the reconcile ([`instance_scale.go#L334-L341`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L334-L341), [`#L114-L122`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L114-L122), [`instance_reconciler.go#L152-L166`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_reconciler.go#L152-L166)). If the process dies between the first delete and that write, nothing is recorded. A still-Terminating Pod re-fires the trigger and the scope is finished; a Pod that is already gone leaves the P2(a) refill **[inferred]**.
- **Restart mid-wave with the record persisted:** `pendingDeletionsFromPersistedWave` re-deletes desired members created before `lastRestartTime` ([`#L878-L943`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L878-L943)).
- **Policy or spec changed in between:** in the budget path the resume branch runs before the limit and policy checks ([`#L354-L365`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L354-L365)), and the default path's pending-wave functions check neither the policy nor the generation, so a started wave is finished **[code; not run]**. The budget is keyed by `groupRestartRevision`, which "identifies component topology and Pod templates, including sizes, but not policy or budget configuration", and exhaustion "suppresses creation and updates until explicit recovery or a new Pod-template revision, even if the budget is changed or removed" ([`roleinstance_types.go#L207-L221`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstance_types.go#L207-L221)). Acceptance aborts if the generation moved.

The same problem one level up, and how it was fixed **[doc + code]**: [#482](https://github.com/sgl-project/rbg/issues/482), "Stateful early rollback replacement state is process-local and can lose surge protection". The issue describes the sequence (it does not report a run): after a controller restart the RIS treats the rollout as complete and condemns surge while the replacement is not Ready. The state is now `RoleInstanceSet.status.rolloutReplacementState {updateRevision, partition, replicas}`, "persisted before replacing a base instance" ([`roleinstanceset_types.go#L198-L238`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstanceset_types.go#L198-L238)):

```go
// Persist intent before either an in-place update or deletion. A failed
// write must leave the target untouched so a restart cannot lose surge.
```

([`stateful_instance_set_control.go#L955-L961`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_control.go#L955-L961)). The write is a merge patch carrying `metadata.uid` and `resourceVersion`; the response is decoded into a fresh object and checked, with the error "did not persist rolloutReplacementState; upgrade the RoleInstanceSet CRD to match the controller" ([`status_updater.go#L113-L147`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_status_updater.go#L113-L147)). A resourceVersion expectation then holds back the next reconcile until the informer has seen the write: "Never time out into destructive work" ([`controller.go#L241-L245`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_controller.go#L241-L245)).

RBGSet does the same with a `groupset-observed-replicas` annotation ("The observed-replicas checkpoint is what tells the two apart" — scale-out versus replacement, [`rolebasedgroupset_rolling.go#L430-L450`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroupset_rolling.go#L430-L450)) and a per-child request/acknowledge pair that "persist on the child object, so controller restart or leader handover does not release the budget" ([KEP-455 L409-L414](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/455-rbgs-rolling-update/README.md#L409-L414)).

Confidence: high for the code; the crash-window consequences are inferred.

#### P4 — Where per-unit state lives, size and write rate

| Carrier | Content | Evidence |
|---|---|---|
| `RoleInstance.status` (the unit's own object) | `lastReadyGeneration`, `restartCount`, `lastRestartTime`, `groupRestartCount`, `groupRestartRevision`, `restartBudgetExhausted`, `Restarting` condition, `phase`, `lastPhaseCompleteGeneration`, per-component counters, `inPlaceUpdateContainerBaselines` (map keyed by Pod name, garbage-collected for deleted Pods) | [`roleinstance_types.go#L144-L235`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstance_types.go#L144-L235) |
| `RoleInstanceSet.status` | counters, revisions, `rolloutReplacementState` (one range), `Established` condition (used for status classification only, [`progressing.go#L403-L443`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/progressing.go#L403-L443)) | [`roleinstanceset_types.go#L161-L260`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstanceset_types.go#L161-L260) |
| `RoleBasedGroup.status` | `roleStatuses[]` (one entry per role: replicas, ready, updated), conditions, `observedRolloutRequest` | [`rolebasedgroup_types.go#L668-L701`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroup_types.go#L668-L701) |
| `RoleBasedGroupSet` | counters and revisions in status; `groupset-observed-replicas` annotation | [`rolebasedgroupset_types.go#L129-L182`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroupset_types.go#L129-L182) |
| In memory only | restart-wave Pod UIDs and issued deletes; per-RI "unhealthy since" timers ("on controller restart it resets, which is acceptable"); expectations | [`sync/api.go#L69-L75`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/api.go#L69-L75), [`controller.go#L75-L89`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_controller.go#L75-L89) |

- **No per-unit list in any parent status [code].** Parent records are counters or a single range.
- **How status is written [code].** RI: `Status().Update` inside `RetryOnConflict`, re-reading the object and merging fields owned by other transactions; the latch only advances ([`instance_status.go#L511-L628`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/instance_status.go#L511-L628)); it writes only when the computed status differs. Budget transactions: optimistic-lock `Status().Patch` on an uncached read. RIS intent: precondition patch with write-back verification (P3). RBG: server-side apply on the status subresource with `Force` ([`utils.go#L48-L85`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/utils/utils.go#L48-L85)).
- **Documented scale and write concerns [doc].**
  - [#520](https://github.com/sgl-project/rbg/issues/520) (open), measured on the RBGSet-rollout PR build, not on head: with 100 Pending RBGs, 27 GET/s and 27 PATCH/s, work-queue depth 76, and a new healthy RBG reaching Ready in 20–26 s instead of 5 s; "about 3 PATCH per reconcile". The causes named are an unconditional requeue and uncached per-role reads for the cross-level rollout barrier.
  - [KEP-576 L220-L230](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/576-compact-discovery-configmap/README.md#L220-L230): for the one per-instance enumeration RBG had (the discovery ConfigMap), a sample-based estimate in the KEP is about 2.35 MB at 4000 replicas × 5 Pods, over the 1 MiB limit, and "Even below the limit, every membership change rewrites the whole object". It was replaced by a compact format.
  - [KEP-284 performance](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/284-pod-cache-filter/performance.md) measures the cost of the Pod informer cache only. [KEP-502 L646-L655](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/502-workload-status-model/README.md#L646-L655) says of the added status fields: "request volume remains to be measured".
  - I found no benchmark of RI/RIS status size or status write rate.

#### P5 — The level below the object

- **RBG made every level an object [code].** The group (RBG) was always the root CR. The multi-Pod instance became an object with KEP-30: v1alpha1 `InstanceSet`/`Instance` (PRs #83 and #110, opened Nov 2025), then v1alpha2 `RoleInstanceSet`/`RoleInstance` ([PR #186](https://github.com/sgl-project/rbg/pull/186), merged 2026-03-09), which the migration doc dates to v0.7.0 and which is the v1alpha2 default workload.
- **Why [doc].** KEP-30 gives product reasons: independence from Deployment/StatefulSet/LWS, "Uses **Instance** (instead of Pod) as the minimal orchestration unit", shared lifecycle, gang scheduling, in-place update, instance-level `maxSurge` ([KEP-30 L46-L62](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/30-instanceset/README.md#L46-L62)). Identity or durable recovery state is not among them. PR #186's description is an unfilled template. KEP-502 later notes that "RI reconciliation is already stateful: restart-policy recreation, in-place update, and readiness recovery use persisted facts and conditions to decide the next step" ([L367](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/502-workload-status-model/README.md#L367)).
- **Below RI [code].** Components (leader, worker, custom) are not objects. RBG keeps no per-component identity or commit: the policy has two values and the scope is always the whole RI ([`rolebasedgroup_types.go#L177-L186`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroup_types.go#L177-L186)). Component start/delete ordering is recomputed from annotations each reconcile ([`instance_scale.go#L226-L260`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/instance_scale.go#L226-L260)).
- **Where a level is not an object (LWS-backed roles) [code].** RBG keeps no state for an LWS group. To decide rollout completion it reads LWS internals uncached: revisions, the leader Pod and the worker StatefulSet per group ([`rolebasedgroup_rollout_status.go#L177-L250`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_rollout_status.go#L177-L250)).
- **Group-scope recreate was removed [doc].** `RecreateRBGOnPodRestart` needed a second (Pod) controller that wrote a `RestartInProgress` condition into RBG status while the RBG controller also wrote status with SSA `Force=true`; the condition could stick at `True`. [#339](https://github.com/sgl-project/rbg/issues/339): "no valid production use case"; [PR #340](https://github.com/sgl-project/rbg/pull/340) removed 1,996 lines. Only `None` and `RecreateRoleInstanceOnPodRestart` remain.
- **What all-objects cost [doc].** A same-name-predecessor bug at each object boundary (#487, #496, #500, #507, #511, see P1); cross-level polling ([#520](https://github.com/sgl-project/rbg/issues/520)); migration by recreation (P7). And P2(a) and the default path of P3 are still open one year after instances became objects.

#### P6 — Coordinated rolling update across roles

What it does **[code]**:

- **Ordering.** Roles are topologically sorted by `dependencies`. On every reconcile, not only at start-up, role reconciliation stops with an error at the first role whose dependency is not ready ([`rolebasedgroup_controller.go#L1019-L1054`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_controller.go#L1019-L1054), [`dependency.go#L94-L117`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/dependency/dependency.go#L94-L117)). Ready means `readyReplicas == replicas` ([`roleinstanceset_reconciler.go#L654-L673`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset_reconciler.go#L654-L673)).
- **Skew.** For a `CoordinatedPolicy` with `maxSkew`, the RBG controller recomputes a per-role partition each reconcile from role status and passes it down; nothing is recorded ([`#L2041-L2121`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_controller.go#L2041-L2121), [`#L2160-L2203`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_controller.go#L2160-L2203)). The code admits the input can be stale: "Due to the synchronization between rbg and workloads, the rbg may not get the least updatedReplicas correctly here. We add maxSkew/2 to the next rolling step".
- **Within a role.** RIS has `partition`, `maxUnavailable`, `maxSurge`, `paused`, and in-place or `RecreatePod` updates ([`roleinstanceset_types.go#L118-L152`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/roleinstanceset_types.go#L118-L152)). `currentRevision` advances only behind a five-part guard that requires the previous persisted status to agree; the code calls this the central defense against the "rollout right after creation deletes everything" cascade ([`stateful_instance_set_control.go#L1028-L1048`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstanceset/statefulmode/stateful_instance_set_control.go#L1028-L1048)).
- **Progress reporting.** RBG: `roleStatuses[]`, `RollingUpdateInProgress`, and `observedRolloutRequest`, set only after uncached verification of every role's workload and instances ([`rolebasedgroup_rollout_status.go#L41-L68`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_rollout_status.go#L41-L68), [`#L99-L158`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/internal/controller/workloads/rolebasedgroup_rollout_status.go#L99-L158)).

Known stalls and misleading status:

- **Dependencies versus maxSkew deadlock, open.** `RoleInMaxSkewCoordinationV2` is a stub **[code]**:
  ```go
  // ... coordination rules are managed via separate CoordinatedPolicy
  // resources, not embedded in the RoleBasedGroup spec, so this always returns false.
  ```
  ([`utils.go#L232-L237`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/utils/utils.go#L232-L237)). The dependency therefore must be fully ready while it is itself rolling, and maxSkew holds it back until the dependent catches up. [PR #638](https://github.com/sgl-project/rbg/pull/638) (open) describes the resulting deadlock **[doc]**; I verified the stub and the gate, not the deadlock.
- [#464](https://github.com/sgl-project/rbg/issues/464) (open): a stateful RIS rollout stops when fresh unhealthy instances exhaust `maxUnavailable`, until an unrelated event arrives. [PR #484](https://github.com/sgl-project/rbg/pull/484) fixed related cases.
- [#520](https://github.com/sgl-project/rbg/issues/520) item 5: "every unready RBG reports `RollingUpdateInProgress=True` / `RolesUpdating` even when no update was requested", and RBGSet consumes that condition as a gate.
- [#618](https://github.com/sgl-project/rbg/issues/618) (open): RBGSet `Recreate` never converges under a label-injecting admission policy ("children keep getting new UIDs"); `/scale` reports surge children as replicas.

Confidence: high for the mechanisms; the stalls are as reported in the issues.

#### P7 — Upgrade/migration when the object model changed

- **Workload kind changed (Deployment / StatefulSet / LWS → RoleInstanceSet): recreated [doc].** "The controller creates a `RoleInstanceSet` and treats the old Deployment, StatefulSet, or LeaderWorkerSet as an orphan, deleting it in the same reconcile pass. The deletion happens even if creating the replacement failed." "The role runs with zero Pods until the `RoleInstanceSet` is Ready. There is no overlap or surge period." ([migration doc L135-L139](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L135-L139)). For an RBGSet: "Plan it as a full outage of that role across the set" ([L165-L170](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L165-L170)).
- **No tool, by decision [doc].** "RBG intentionally does **not** provide an executable migration command or script. A migration changes running workloads and may recreate Pods" ([L9](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L9)).
- **v1alpha1 `InstanceSet` → `RoleInstanceSet`: not migrated [doc].** No reconciler since v0.7.0; "the old InstanceSet and its Pods run alongside the replacement until the old object is explicitly deleted" ([L157-L161](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/migration-v1alpha1-to-v1alpha2.md#L157-L161)). KEP-30 had already said "Existing RBGs cannot be directly switched to use InstanceSet as the underlying workload type" ([L104-L105](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/keps/30-instanceset/README.md#L104-L105)).
- **Migration bug [doc].** [#511](https://github.com/sgl-project/rbg/issues/511): after the StatefulSet was deleted, the new RI stayed unready behind the same-name orphan Pod until a reconcile was triggered by hand.
- **What is done in place [code].** The API version, by conversion webhook and storage rewrite. New status fields are zero-value safe; the latch's comment reads: Starting at 0 means "never Ready at any recorded generation", so upgraded objects keep today's behavior until their next Ready. Defaults are resolved in getters because "a CRD default would materialize on every existing role ... rewriting its serialized template and rolling the role on upgrade" ([`rolebasedgroup_types.go#L219-L222`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/api/workloads/v1alpha2/rolebasedgroup_types.go#L219-L222)). A changed status format is re-stamped rather than reset ([`restart_budget.go#L398-L418`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/pkg/reconciler/roleinstance/sync/restart_budget.go#L398-L418)). The docs list what is not rescued on upgrade: "Existing instances stuck NotReady without a readiness latch are not rescued automatically" ([`failure-handling.md#L119-L122`](https://github.com/sgl-project/rbg/blob/a19d2c83fc1aca4f57a23cf7eee56f687e2da023/doc/features/failure-handling.md#L119-L122)).

### 4. What kthena could borrow, and what RBG's experience warns against

Borrow:

1. **A generation-keyed "established" latch on the unit's own record**, advanced only forward and never re-derived from the live Ready condition (`lastReadyGeneration`, P2). It is zero-value safe on upgrade. RBG's own open proposal (#562) is to use it as the level signal for a missing member, which is the P2(a) fix.
2. **Commit as a compare-and-swap on a fresh read before the first delete**, then resume from the persisted record (`acceptBudgetRestart` / `resumeBudgetRestartWave`, P3). The record is keyed by topology revision, not by policy, so a later policy edit does not cancel a committed scope.
3. **Write-back verification of intent writes**: precondition on UID and resourceVersion, decode the response into a fresh object, fail if the field did not persist (catches an older CRD pruning it), and hold destructive work until the cache has seen the write (`PersistRolloutReplacementState`, P3).
4. **Bounded parent records** — one range (`rolloutReplacementState`) or one scalar (`groupset-observed-replicas`) instead of a per-unit list — together with UID preconditions on member deletes and an explicit "waiting for the predecessor's GC" state (`PodRecreateRace`, P1).

Warnings:

5. **Making units objects did not solve P1–P3.** UIDs fixed stale-identity only after each boundary had its own bug (#487, #496, #500, #507, #511). "Established" and "scope committed" still needed explicit status fields; P2(a) (#562, #566) and the default path of P3 remain open.
6. **Edge-observed evidence and in-memory wave state leak across restarts.** The Terminating-Pod trigger misses offline loss; the wave marker degrades to second-granularity timestamps and allows mixed generations (#582). An epoch stamped on members would have avoided the fallback.
7. **Recovery state written into a parent's status by two controllers was withdrawn.** The group-scope recreate policy raced under SSA `Force`, stuck `RestartInProgress`, and was deleted for lack of a production use case (#339, #340).
8. **Cross-level barriers and object-model migration have measurable cost.** The RBGSet → RBG → RIS → RI completion barrier needed uncached reads and polling (#520: Ready 5 s → 20–26 s with 100 pending groups on the PR build). Changing the backing object meant a zero-Pod window with no tool (P7), and the per-instance enumeration in one object hit 1 MiB (KEP-576).

### 5. Unverified points and limits

- Nothing was run. Every behavioural statement is from reading code at the head SHA or from the project's text. Stable `v0.8.0` code was not read; per #542 it gates on the live Ready condition.
- The repro in #562 is a commenter's reading of the code, not a run. #566 and the #520 numbers are the reporters' measurements; the #520 build was a PR head, so its cost cannot be attributed to head or to CRs as such.
- **[inferred]** items: the crash window in the default restart path (P3) and a stale RoleInstance delete hitting a successor (P1). Completion of a started wave after a policy change (P3) is read from the code order, not run. No issue or test was found for any of them.
- The #638 deadlock was not reproduced; only the stub and the dependency gate were verified.
- Whether the Helm chart enables leader election was not checked; only the binary's flag default was.
- The "same gap as kthena?" column is judged against the problem statement in the brief. I did not read kthena code for this report.
- `_test.go` files were not read, so statements about test coverage come from PR descriptions only.
- KEP-30's `kep.yaml` contains KEP-29's metadata, so its status and milestone could not be determined. PR #186 gives no motivation. The dates for PRs #83 and #110 are their creation dates.
- Search scope for further issues: "recreate", "restart policy", "stale", "same name", "leader election", "controller restart", "migration", "status size", "write amplification", "etcd size", "scale benchmark", "maxSkew", "partition". The four size/scale queries ("status size", "write amplification", "etcd size", "scale benchmark") returned nothing relevant beyond #520.

---

## Peer study: NVIDIA Dynamo Grove — where the controller keeps recovery and rollout facts

Read on 2026-11-26. Source reading only: nothing was built or run, so no runtime behaviour is claimed.

### 1. Header

| Item | Value |
|---|---|
| Repo | `ai-dynamo/grove` is the canonical location. `NVIDIA/grove` is not a second publication: `gh api repos/NVIDIA/grove` answers with `full_name: ai-dynamo/grove` (a redirect). Checked 2026-11-26 |
| Commit | `main` @ `15411d9ccb37cfbae9e7cded612762e1ad41777c`, committed 2026-11-26T08:03:03Z |
| Release | `v0.2.1-beta1`, published 2026-11-11 (tag commit `c9b6835b`). Head is 17 commits ahead. Findings are for head and were not re-checked against the tag |
| Read | API types (`operator/api/core/v1beta1` PodCliqueSet, PodClique, PodCliqueScalingGroup, PodGangMap; v1alpha1 PodCliqueSet; `scheduler/api` PodGang), `operator/api/common`; controllers `podcliqueset` (reconcilespec, components `podcliquesetreplica`, `podclique`, `podgangmap` steady state, `podgangmigrator`), `podclique` (reconciler, status, delete, `pod` component), `podcliquescalinggroup` (reconcilespec, status, `podclique` component); `internal/expect`; `internal/utils/component`; Kueue `ensureWorkload`; GREP-393, the v1beta1 upgrade guide, core concepts; the issues and PRs linked below |
| Not read in full | PodGang sync flow, the Coherent sub-step planner, KAI/Volcano/LPX backends, webhooks, e2e tests |
| Evidence tags | **[code]** read in source at the commit above. **[doc]** stated in a doc, issue or PR. **[inf]** my inference from the code, not observed |

Abbreviations are Grove's own: PCS = PodCliqueSet, PCSG = PodCliqueScalingGroup, PCLQ = PodClique.
Mapping used: ModelServing ≈ PCS; ServingGroup ≈ one PCS replica index; Role ≈ a PodClique template or a PCSG config; multi-Pod Role instance ≈ one PCSG replica (its member PodCliques, e.g. leader and worker); Pod ≈ Pod.

**Result for the hypothesis.** The problem classes P1–P6 all exist in Grove, and its issue history shows the project ran into each of them. At head some parts are closed (a durable "was established" marker on PodClique, index reservation during scale-in, identity on the read side, a persisted plan for scale-in and updates) and some are not (nothing is recorded before a gang-termination delete, deletes carry no UID precondition, the PCSG level still infers "was healthy" from timestamps). P7 was handled by migrating in place. The last table column says, per problem, whether the gap is open at head.

### 2. Summary

| # | What Grove does | Durable across restart? | Evidence | Same gap as kthena? |
|---|---|---|---|---|
| P1 | Reads are identity-safe: Pods are listed by controller-owner UID, children are filtered with `IsControlledBy`, Pod names are random (`generateName`), PodGang names carry an epoch. Deletes are not guarded: by name or by label selector, no UID precondition (one Kueue path excepted). A deposed leader is not fenced | UIDs, ownerRefs and the epoch (in PodGangMap): yes. Expectation stores (UID sets): memory only | [pod.go L84-L104](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/pod.go#L84-L104), [syncflow.go L147-L158](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/syncflow.go#L147-L158), [gangterminate.go L246-L249](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L246-L249) | Partly. Stale observations: no. Late or retried deletes: yes |
| P2 | A missing Pod is refilled. The whole unit is recreated only when `MinAvailableBreached` stays True longer than `terminationDelay` (default 4h) and the unit was established before. PodClique: `status.lastScheduled`, never reset. PCSG: a heuristic, "condition changed later than creation + 5s" | PodClique: yes, in its own status. PCSG: the condition is stored, the conclusion drawn from it is a guess. No in-memory input | [podclique.go L98-L127](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L98-L127), [L136-L158](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L136-L158) | Partly. (a) no: the outcome does not depend on the controller seeing the loss. (b) no for PodClique; the PCSG heuristic can misclassify |
| P3 | Scale-in and update selections are written to status (Coherent: to PodGangMap spec) before any delete. Gang termination is not: "action-first / flag-second", one label-selector delete of the unit's PodCliques, then Kubernetes GC removes the Pods | Scale-in, rolling, Coherent: yes. Gang termination: nothing before the delete, a flag on the PCSGs after it | [podcliqueset.go L107-L128](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L107-L128), [gangterminate.go L209-L228](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L209-L228) | Partly: yes for gang termination, no for scale-in and updates |
| P4 | Per-unit state is in the status of child objects (PodClique, PCSG, PodGang) and in one spec-only PodGangMap per PCS replica. Parent status holds counters and in-flight index sets. A per-PodGang list in parent status was declared in v1alpha1, never set, removed in v1beta1 | Yes, except the expectation stores | [podcliqueset.go L75-L113](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L75-L113), [upgrade guide L16](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/user-guide/upgrading-to-v1beta1-v0.2.0-beta1.md#L16), [#781](https://github.com/ai-dynamo/grove/issues/781) | Partly: no per-unit list in the parent; write amplification from child status is documented |
| P5 | Objects: PCS, PCSG, PodClique, Pod, PodGang, PodGangMap. Not objects: the PCS replica index and the PCSG replica index, which are the two units gang termination recreates. Their identity is a label on children; their existence is inferred from surviving children | Labels and child status: yes. No record of the unit itself | [scaling.go L107-L110](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/scaling.go#L107-L110), [#948](https://github.com/ai-dynamo/grove/issues/948) | Yes |
| P6 | One PCS replica at a time, recorded in `status.updateProgress.currentlyUpdating`. Inside it, RollingRecreate rolls each PodClique and PCSG independently; Coherent moves minimum units through PodGangMap sub-steps. Startup order is enforced by an init container in each Pod, not by the controller | Yes (parent and child status, PodGangMap) | [rollingupdate.go L222-L239](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/rollingupdate.go#L222-L239), [podcliqueset.go L141-L226](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L141-L226) | Partly: stalls and misleading completion have occurred ([#400](https://github.com/ai-dynamo/grove/issues/400), [#786](https://github.com/ai-dynamo/grove/issues/786), [#916](https://github.com/ai-dynamo/grove/issues/916), [#952](https://github.com/ai-dynamo/grove/issues/952)) |
| P7 | API version change: in place, conversion webhook. PodGang renaming plus the new PodGangMap: in place, a one-time migration behind a status-condition gate, metadata-only patches. Unintended: one operator upgrade changed the template hash and recreated all Pods | The gate is a PCS status condition | [gate.go L45-L106](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/podgangmigrator/gate.go#L45-L106), [podgangmigrator.go L77-L109](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podgangmigrator/podgangmigrator.go#L77-L109), [#709](https://github.com/ai-dynamo/grove/issues/709) | No: running workloads were migrated, not recreated |

### 3. Details

#### P1. Identity across same-name recreation

Names. PodClique names are positional, `<pcs>-<i>-<clique>` or `<pcsg>-<j>-<clique>` **[code]** ([namegen.go L78-L87](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/common/namegen.go#L78-L87)), so gang termination, a PCSG replica replacement and scale-in followed by scale-out all recreate a PodClique under the same name. Pods use `GenerateName: fmt.Sprintf("%s-", pclq.Name)` plus a controller ownerReference **[code]** ([pod.go L180-L186](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/pod.go#L180-L186)); only the Pod index (label and hostname) is reused.

What protects the successor:
- Pods are listed through a field index on the controller-owner UID, not by label **[code]** ([pod.go L84-L104](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/pod.go#L84-L104)). A successor PodClique never counts its predecessor's Pods: "The PodClique lists its own pods by owner UID, so it does not see these." ([syncflow.go L147-L158](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/syncflow.go#L147-L158)).
- Deleting a PodClique removes its finalizer and leaves the Pods to the garbage collector, which works by ownerReference UID **[code]** ([reconciledelete.go L35-L55](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconciledelete.go#L35-L55)). The PodClique delete flow issues no Pod deletes of its own, so none can arrive late.
- Parents filter children with `metav1.IsControlledBy`: "Exclude PodCliques controlled by an older PodCliqueSet of the same name" **[code]** ([podclique.go L178-L182](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L178-L182)).
- A successor PodClique is not created while a same-name object still exists **[code]** ([sync.go L350-L357](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/sync.go#L350-L357)). Waiting for the predecessor's *Pods* happens only for scheduler backends that hold a Pod finalizer ([syncflow.go L135-L145](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/syncflow.go#L135-L145), [standalonepclqdistribution.go L273-L292](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/standalonepclqdistribution.go#L273-L292)); with other backends the old and new Pod sets can overlap **[inf]**.
- A PCS replica index being scaled in stays reserved in `status.scalingProgress.scaleInReplicaIndices` until no child remains, "preventing premature reuse during deletion" **[code]** ([podcliqueset.go L123-L128](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L123-L128)).
- In-memory expectations track object UIDs, not names or counts **[code]** ([expectations.go L47-L58](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/expect/expectations.go#L47-L58)). A late Pod delete event only removes that Pod's UID ([register.go L132-L150](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/register.go#L132-L150)).

What does not:
- Deletes carry no precondition. PodCliques are deleted by name through an empty object ([podclique.go L190-L211](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podclique/podclique.go#L190-L211)) or with `DeleteAllOf` on a label selector ([gangterminate.go L246-L249](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L246-L249), [podclique.go L206-L209](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/podclique.go#L206-L209)) **[code]**. The only `client.Preconditions{UID, ResourceVersion}` in `operator/internal` is in the Kueue backend ([backend.go L168-L169](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/scheduler/kueue/backend.go#L168-L169)). Grove's own comment accepts the consequence: "a retried task re-runs the DeleteAllOf — recycling the just-recreated PodCliques once more" ([gangterminate.go L302-L304](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L302-L304)).
- Leader election is the stock controller-runtime lease ([manager.go L110-L116](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/manager.go#L110-L116)) **[code]**. A deposed leader's in-flight label-selector delete would hit the successors **[inf]**.
- State keyed by name has failed twice. [#782](https://github.com/ai-dynamo/grove/issues/782): a PCS recreated under the same name stayed stuck because an in-memory expectation was keyed by namespace/name; fixed by adding the UID ([reconcilespec.go L109-L116](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/reconcilespec.go#L109-L116)). [#943](https://github.com/ai-dynamo/grove/issues/943): a PodGang recreated under the same name adopted its predecessor's Kueue Workload and admission; fixed in [PR #944](https://github.com/ai-dynamo/grove/pull/944) **[doc]**.
- The expectation store is lost at restart. [#926](https://github.com/ai-dynamo/grove/issues/926) (open): after an operator restart a Pod deleted by another controller is not replaced until it finishes terminating **[doc]**.
- PodGang epochs are unix-nano values taken when a PodGangMap is first built ([steadystate.go L31-L60](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podgangmap/steadystate.go#L31-L60)). The scale-out entry keeps its epoch, so scale-out PodGang names repeat (the cause of #943), and a 19-digit epoch pushes names past the 63-byte label limit ([#874](https://github.com/ai-dynamo/grove/issues/874), open, verified) **[doc]**.

Confidence: high for the code facts, medium for the two inferences.

#### P2. "Established" versus "still being created"

Grove's policy is not "recreate the unit when a member is lost". The API states it: "A PodGang does not have a RestartPolicy field because the restart policy is predefined: If the number of pods in any of the cliques falls below the threshold, the entire PodGang will be restarted" ([podcliqueset.go L260-L265](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L260-L265)), after `terminationDelay`, "Defaults to 4 hours" ([L291-L298](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L291-L298)) **[code]**.

- Missing Pod: the PodClique's Pod component computes desired minus live (corrected by expectations) and creates the difference ([syncflow.go L243-L262](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/syncflow.go#L243-L262), [L382-L398](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/syncflow.go#L382-L398)) **[code]**. The exception is an old-generation PodGang during a Coherent update.
- Breach: recomputed on every status reconcile from the Pods the PodClique owns, `scheduledReplicas < minAvailable` or too few ready-or-starting Pods ([reconcilestatus.go L231-L297](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilestatus.go#L231-L297)) **[code]**. The condition and its `lastTransitionTime` are stored in PodClique status. No events and no in-memory state feed the decision: the evaluator reads child objects and `time.Now()` ([gangterminate.go L70-L107](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L70-L107)).
- Established, PodClique: `return pclq.Status.LastScheduled != nil`, "a durable marker ... never reset once set" ([podclique.go L104-L110](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L104-L110), set in [reconcilestatus.go L306-L319](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilestatus.go#L306-L319), with a backfill for objects from older versions) **[code]**. A never-scheduled PodClique is left out of gang termination ([podclique.go L143-L149](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L143-L149)). The marker lives on the PodClique object, so a recreated PodClique starts without it; Grove relies on exactly that to stop a PCSG replica from being terminated in a loop ([constants.go L129-L137](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/common/constants/constants.go#L129-L137)).
- Established, PCSG: `return cond.LastTransitionTime.After(pcsg.CreationTimestamp.Add(InitialScheduleGrace))` with a 5-second grace ([podclique.go L98-L127](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/utils/component/podclique.go#L98-L127)) **[code]**.

History, which is the closest match to the kthena question **[doc]**:
1. Until 2026-05 a PodClique with fewer scheduled Pods than `minAvailable` was reported as *not* breached, so that never-scheduled gangs were not recreated. Lost-after-established looked the same, and gang termination did not fire: [#277](https://github.com/ai-dynamo/grove/issues/277), fixed by [PR #561](https://github.com/ai-dynamo/grove/pull/561).
2. [PR #614](https://github.com/ai-dynamo/grove/pull/614) added the timestamp heuristic. [#789](https://github.com/ai-dynamo/grove/issues/789): a delayed first reconcile was read as a regression; the reporter saw "6 PCS replica deletions within 88 seconds" and "8 healthy `frontend` Pods terminated". A contributor (`oleg-kushniriov`) replied that "explicit, persisted evidence of a healthy state is safer and more reliable than the current timing heuristic" ([comment](https://github.com/ai-dynamo/grove/issues/789#issuecomment-5395204069)). [#1015](https://github.com/ai-dynamo/grove/issues/1015) shows the opposite error: a real loss within 5 seconds of creation is ignored.
3. At head the PodClique uses the durable marker; the PCSG still uses the heuristic. [PR #790](https://github.com/ai-dynamo/grove/pull/790) (a persisted `HealthyStateObserved` condition) is open; #789 was closed after a project collaborator deferred it to the terminal-states work ([comment](https://github.com/ai-dynamo/grove/issues/789#issuecomment-6138169524), [#877](https://github.com/ai-dynamo/grove/issues/877)).

The two concrete cases:
- (a) Pod lost while the controller is down and already gone: the unit is refilled, not recreated, and this is the same result as with the controller running **[code]**. Whole-unit recreation follows only if the breach outlasts `terminationDelay`. The clock starts when the controller first writes the condition, so an outage before that write is not counted; if the condition was already True before the outage, the outage is counted **[inf]**.
- (b) Pod lost during first creation: refill only. A never-scheduled PodClique is excluded, and a PCS replica with a missing standalone PodClique is skipped ([gangterminate.go L144-L148](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L144-L148)) **[code]**. For a PCSG this holds only if its first condition write came within 5 seconds of creation; a later first write, for example because the operator restarted, is read as "was healthy" **[inf]**.

Confidence: high.

#### P3. Commit of a scoped delete or recreate

| Operation | Recorded before the first delete? | After a restart |
|---|---|---|
| PCS scale-in of replica *i* | Yes. `status.scalingProgress.scaleInReplicaIndices`, patched with an optimistic lock ([scaling.go L38-L59](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/scaling.go#L38-L59), [L365-L389](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/scaling.go#L365-L389)); the next reconcile waits until the cache shows it ([reconciler.go L98-L109](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/reconciler.go#L98-L109)). Removed only when no component still reports children ([reconcilespec.go L207-L247](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/reconcilespec.go#L207-L247)) **[code]** | Finishes the same indices |
| Rolling update, which PCS replica | Yes. `status.updateProgress.currentlyUpdating[0]` ([rollingupdate.go L222-L239](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/rollingupdate.go#L222-L239)); children act only when their replica-index label matches ([reconcilespec.go L115-L145](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilespec.go#L115-L145)) **[code]** | Continues with that replica |
| RollingRecreate of a PCSG replica | No separate record. The pod-template-hash label on the member PodCliques is "the old-vs-new discriminator and is intentionally label-only"; UID delete-expectations cover cache lag only ([rollingupdate.go L184-L244](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/rollingupdate.go#L184-L244)) **[code]** | A half-deleted replica has one member recreated at the new hash and one at the old, is classified old, and is deleted again **[inf]** |
| Coherent update | Yes. The placement is written to PodGangMap spec first; the PCSG reconciler recreates any replica whose members' `grove.io/podgang` label differs from the committed PodGang ([coherentupdate.go L34-L93](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/coherentupdate.go#L34-L93)) **[code]** | Plan position is rebuilt "from the committed current-hash entries" ([coherentupdate.go L32-L38](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podgangmap/coherentupdate.go#L32-L38)) |
| Gang termination of a PCS replica | No. "Ordering is action-first / flag-second: if the controller crashes between the DeleteAllOf and the flag writes, the next reconcile sees the breach still True (new PCLQs Pending) with no flag set, fires once more (one extra churn), and converges." ([gangterminate.go L209-L228](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/gangterminate.go#L209-L228)). The flag is a `GangTerminationInProgress` condition on each PCSG; a PCS with standalone PodCliques only has no flag **[code]** | Pod deletion is finished by Kubernetes GC without the controller. The decision itself is re-derived from child conditions |
| Gang termination of a PCSG replica | No. One `DeleteAllOf` by PCSG name and replica-index label ([podclique.go L189-L228](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/podclique.go#L189-L228)). Missing PodCliques are created before the termination check runs ([sync.go L143-L160](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/sync.go#L143-L160)) **[code]** | Same as above |

- Partial execution **[inf]**: if a label-selector delete removed the breached PodClique but not its siblings, the next reconcile recreates the missing one (fresh status, never scheduled, so not a candidate) and nothing marks the siblings for deletion. The unit is refilled, not finished. The exposure is small because the scope is one API call over a handful of PodClique objects, with GC doing the Pod-level work.
- Spec change in between **[code]**: while a child is updating its breach condition is forced to `Unknown`, which suspends gang termination ([reconcilestatus.go L231-L239](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilestatus.go#L231-L239), [GREP-393 L917-L925](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L917-L925)). A new template overrides the parent's update progress ([reconcilespec.go L79-L106](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/reconcilespec.go#L79-L106)) and children requeue while their recorded target hash is stale ([sync.go L207-L224](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/sync.go#L207-L224)). An earlier version lost track of Pods here: [#400](https://github.com/ai-dynamo/grove/issues/400).
- A recreated child is always built from the current PCS template: "Grove does not persist any previous revisions of the PodCliqueSet spec" ([GREP-393 L165](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L165)) **[doc]**. Consequences: a replica recreated during an update jumps to the new revision ([#941](https://github.com/ai-dynamo/grove/issues/941), open), and gang recovery resets a scaled PodClique to the template's replica count ([#826](https://github.com/ai-dynamo/grove/issues/826)).
- When the flag is cleared, including "controller restarts", was raised in [#798](https://github.com/ai-dynamo/grove/issues/798) and closed without a recorded decision **[doc]**.

Confidence: high for the table, medium for the partial-execution inference.

#### P4. Where per-unit state lives, and how big or hot it is

- Parent (PCS) status: replica counters, conditions, the generation hash, `updateProgress` (one `currentlyUpdating` entry, plus name lists of in-scope components for Coherent), `scalingProgress` (in-flight indices only) ([podcliqueset.go L75-L113](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L75-L113)) **[code]**. Nothing grows with the steady-state replica count.
- v1alpha1 declared `PodGangStatutes []PodGangStatus` in PCS status ([v1alpha1 L101-L102](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1alpha1/podcliqueset.go#L101-L102)); the upgrade guide says "`status.podGangStatuses` of the PCS is removed. Grove never set it." ([L16](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/user-guide/upgrading-to-v1beta1-v0.2.0-beta1.md#L16)) **[doc]**.
- Child status: PodClique ([podclique.go L116-L157](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podclique.go#L116-L157)), PCSG ([scalinggroup.go L83-L112](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/scalinggroup.go#L83-L112)), PodGang ([podgang.go L188-L208](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/scheduler/api/core/v1alpha1/podgang.go#L188-L208)) **[code]**.
- PodGangMap: one per PCS replica, spec only. GREP-393 records that an earlier draft kept a `PodGangMapping` status field on every PodClique and PCSG, synchronised in both directions with the map, and that this "two-way arrangement has been removed" in favour of one source ([L488-L503](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L488-L503)) **[doc]**.
- Memory only: Pod UID expectations per PodClique and PodGang, PodClique UID expectations per PCSG, generation-hash and scaling-progress expectations per PCS.
- Hot path **[code]**: status is patched only when it changed, with an optimistic lock, because skipping "matters at scale where pod events drive many reconciles" ([reconcilestatus.go L96-L109](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilestatus.go#L96-L109)). A 3-minute status resync was added after a lost update left a condition stuck and armed the termination timer ([#775](https://github.com/ai-dynamo/grove/issues/775), [constants.go L30-L34](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/constants/constants.go#L30-L34)).
- Reported cost **[doc]**: [#781](https://github.com/ai-dynamo/grove/issues/781) reports a regression of about 35% in the SoakChurn benchmark (737.87s to 999.54s). Its investigation points to one extra condition transition per new PodClique, because each write "fires a watch event, cascading into extra reconciles on every controller observing `PodCliques`".
- Not found: any documented limit on object count or status size.

Confidence: high.

#### P5. The level below the object

| Level | Object? | Identity |
|---|---|---|
| PodCliqueSet | Yes (status, scale) | user-given name |
| PCS replica index | **No** | label `grove.io/podcliqueset-replica-index` on children. Helper objects per replica: PodGangMap `<pcs>-<i>` (no status), headless Service, PodGangs |
| PodCliqueScalingGroup | Yes, one per PCS replica and group (status, scale) | `<pcs>-<i>-<group>` |
| PCSG replica index | **No** | label `grove.io/podcliquescalinggroup-replica-index` on member PodCliques |
| PodClique | Yes (status, scale, finalizer) | positional name, apiserver UID |
| Pod | Yes | random name; index is a label |
| PodGang | Yes (`scheduler.grove.io`) | `<pcs>-<i>-<epoch>[-<group>-<j>]` |
| PodGangMap | Yes, spec only | `<pcs>-<i>` |

The two levels that are not objects are the two units that gang termination and replica replacement act on.
- PCS replica **[code]**: there is no record that replica *i* exists. Expected indices are computed from the children found, and a Service stands in as witness while the PodCliques are gone: "Gang termination deletes every PodClique for a PCS replica but keeps its headless Service. Include the Service so a standalone-only replica keeps the same index while its PodCliques are recreated." ([scaling.go L107-L110](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/scaling.go#L107-L110), [L160-L174](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/scaling.go#L160-L174)). P1 is covered by `scalingProgress`, P2 by the children's conditions, P3 by status for scale-in and updates and by nothing for gang termination.
- PCSG replica **[code]**: a group-by on a label. Scale-in counts live replicas and assumes indices are contiguous; when a lower replica is terminating the top replica is never deleted ([#948](https://github.com/ai-dynamo/grove/issues/948), open) **[doc]**. P2 comes from the member PodCliques' `lastScheduled`; P3 from the hash label, the PodGangMap, or nothing.
- Pod within a PodClique: the index is assigned from the cached Pod list, and two live Pods have received the same index, after which creation stops ([#900](https://github.com/ai-dynamo/grove/issues/900), open) **[doc]**.

Why these levels are objects. The docs present PodClique and PCSG as scaling targets, "three levels of scaling" ([core concepts L25-L35](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/user-guide/01_core-concepts/01_overview.md#L25-L35)), and both carry a scale subresource ([podclique.go L28](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podclique.go#L28), [scalinggroup.go L24](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/scalinggroup.go#L24)). PodGang is the contract with the scheduler. PodGangMap was added by GREP-393 as the single description of PodGang composition. I found no design text that argues for or against making a replica index an object.

What it cost **[code]** unless marked: four workload reconcilers plus one for topology ([register.go L34-L70](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/register.go#L34-L70)); the PodClique reconciler watches Pods, PCS, PCSG, PodGang and PodGangMap ([register.go L54-L92](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/register.go#L54-L92)); children read the parent's status to learn whether they may act; staleness guards between parent and child hashes; in-memory expectations to wait for the controller's own status writes; a gate protocol between Pods, PodGangs and PodGangMap entries that has left replacement Pods gated ([#916](https://github.com/ai-dynamo/grove/issues/916), open, verified; [#316](https://github.com/ai-dynamo/grove/issues/316)) **[doc]**; long derived names (#874); the write cascade in #781; and loss of child-held runtime state when the child is recreated (#826).

Confidence: high.

#### P6. Coordinated rolling update

- Startup order (`cliqueStartupType`, `startsAfter`) is enforced inside each Pod by an injected `grove-initc` init container that waits for the parent cliques' Pods in the same PodGang ([pod.go L225-L236](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/pod.go#L225-L236)) **[code]**. The controller does not sequence creation. [#1022](https://github.com/ai-dynamo/grove/issues/1022) (open): the init container counts terminating parent Pods as ready **[doc]**.
- One PCS replica at a time. Order: replicas with no scheduled Pods, then breached replicas, then ascending index ([rollingupdate.go L177-L219](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podcliquesetreplica/rollingupdate.go#L177-L219)) **[code]**.
- RollingRecreate, inside the selected replica: each standalone PodClique replaces old-hash Pods within its `maxUnavailable` ([rollingupdate.go L67-L125](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/components/pod/rollingupdate.go#L67-L125)); each PCSG replaces whole replicas within its own budget ([rollingupdate.go L68-L88](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliquescalinggroup/components/podclique/rollingupdate.go#L68-L88)). There is no ordering between cliques **[code]**.
- Coherent: the PodGangMap component emits one sub-step at a time and waits for the PodGangs of that epoch; each minimum unit is "scheduled atomically as a new PodGang" ([podcliqueset.go L578-L582](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L578-L582)). Scaling is rejected by the webhook for the whole update ([GREP-393 L987-L1003](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L987-L1003)) **[doc]**.
- Status: PCS `updateProgress` with updated and total counts for PodCliques and PCSGs, and `currentlyUpdating[]` with `replicaIndex`, `inFlightEpochs`, `message`; each child has its own `updateProgress` and an `UpdateInProgress` condition that turns `Unknown / ProgressDeadlineExceeded` when `lastProgressedAt` is older than `progressDeadline` ([podcliqueset.go L141-L257](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/api/core/v1beta1/podcliqueset.go#L141-L257), [reconcilestatus.go L357-L406](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconcilestatus.go#L357-L406)) **[code]**.
- Known problems **[doc]**: [#400](https://github.com/ai-dynamo/grove/issues/400), a second update during the first marked the update ended with wrong-hash Pods still running; [#786](https://github.com/ai-dynamo/grove/issues/786), `updateEndedAt` set before the replacements were Ready; [#916](https://github.com/ai-dynamo/grove/issues/916) (open), a replacement Pod stays gated after its anchor epoch was pruned; [#952](https://github.com/ai-dynamo/grove/issues/952) (open), `updatedReplicas` counts terminating Pods so the hashes never converge; [#733](https://github.com/ai-dynamo/grove/issues/733) (open), `CurrentlyUpdating` is "a transient, non-monotonic status" that polling clients miss; GREP-393 states that preemption "can leave a coherent update parked indefinitely" ([L161](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L161)).

Confidence: high for RollingRecreate, medium for Coherent (planner not read in full).

#### P7. Upgrade and migration

- API version (v0.2.0-beta1, `grove.io/v1alpha1` to `v1beta1`): conversion webhook, "Existing objects are not rewritten" ([upgrade guide L40](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/user-guide/upgrading-to-v1beta1-v0.2.0-beta1.md#L40)); downgrade needs a rewrite script **[doc]**.
- PodGang naming and PodGangMap (the upgrade to v0.1.0-alpha.13 per #837): in place **[code]**. Before the manager starts, every legacy PCS gets a `PodGangMigrationInProgress` condition ([main.go L167](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/cmd/main.go#L167), [gate.go L45-L106](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/podgangmigrator/gate.go#L45-L106)). While it is set the PodClique and PCSG reconcilers requeue without acting ([reconciler.go L93-L104](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podclique/reconciler.go#L93-L104)). The migrator rewrites labels and annotations on PodCliques and Pods and "touches metadata only, never spec, so the Pod is not recreated" ([podgangmigrator.go L77-L109](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podgangmigrator/podgangmigrator.go#L77-L109), [L234-L236](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/operator/internal/controller/podcliqueset/components/podgangmigrator/podgangmigrator.go#L234-L236)). The migration is mandatory and one-way ([GREP-393 L961](https://github.com/ai-dynamo/grove/blob/15411d9ccb37cfbae9e7cded612762e1ad41777c/docs/proposals/393-coherent-rolling-updates/README.md#L961)).
- A defect of the in-place path **[doc]**: existing Pods kept stale Volcano PodGroup annotations ([#837](https://github.com/ai-dynamo/grove/issues/837), closed).
- An unintended recreation **[doc]**: upgrading v0.1.0-alpha.9 to alpha.11 recreated every Pod because a Kubernetes dependency bump changed the serialised PodSpec and therefore the hash ([#709](https://github.com/ai-dynamo/grove/issues/709), open).

Confidence: high.

### 4. For kthena

What kthena could borrow:
- A durable, never-reset "was established" marker on the unit's own record, with a backfill for objects that predate it: `status.lastScheduled`. Grove reached it after two failed attempts (#277, #789), and the principle is stated in a comment on #789.
- A bounded reservation in parent status that holds a reused index until its children are gone: `scalingProgress`. Its size follows the operations in flight, not the replica count.
- Identity on the read side through owner UID and random Pod names, and removal through GC rather than controller cleanup deletes. This closes the stale-observation half of P1 without tokens.
- Write the plan first and let executors compare a label on the children with the committed target (PodGangMap, template-hash label). A half-finished scope is then re-derived from objects that survive member loss.

What Grove's experience warns against:
- Inferring "was healthy" from timestamps (#789, #1015) and acting before recording ("action-first / flag-second"; the retry semantics in #798 are still undecided).
- Deletes without a UID precondition and in-memory state keyed by name (#782, #943, the retry comment in `gangterminate.go`, #926 after restart).
- Leaving the recreated unit without a record of its own: Grove needs a Service as identity witness and has an index bug in #948. Making the neighbouring levels objects has costs too: the write cascade (#781), state lost on recreation (#826), gate deadlocks (#916), name length (#874).
- Using a template hash as revision identity without stored revisions: an operator upgrade recreated every Pod (#709), and a replica recreated mid-update jumps to the new revision (#941).

### 5. Unverified points and limits

- Nothing was run. Every **[inf]** item is reasoning from code: overlap of old and new Pod sets, the deposed-leader delete, the start of the breach clock around an outage, the PCSG heuristic after a delayed first reconcile, re-deletion of a half-replaced PCSG replica, and refill after a partially applied label-selector delete.
- The "same gap as kthena" column compares Grove with the problem statement of the discussion. I did not read kthena code for it.
- Whether PodGang objects survive a PCS-replica gang termination is unclear. The code deletes only PodCliques and the PodGangMap, and a rebuilt map adopts the epoch of existing PodGangs; GREP-393 describes the PodGangs as torn down. If they survive, their never-reset `lastScheduled` would satisfy the dependency gate for the recreated Pods at once. Not traced.
- How #781 was resolved is not determined: it is closed, and the PR linked to it (#780) was closed unmerged.
- The Coherent sub-step planner, the PodGang sync flow and the scheduler backends were not read in full; P6 for Coherent rests mainly on GREP-393.
- I did not open the scale-test dashboard referenced in #781 and found no documented scale limits.
- Issue states are as of 2026-11-26. Queries were read-only; nothing was posted.

---

## Peer research: LeaderWorkerSet and DisaggregatedSet (kubernetes-sigs/lws)

Read on 2026-11-26. Research only: nothing was run.

### 1. What was examined

| Item | Value |
| --- | --- |
| Repo, branch | `kubernetes-sigs/lws`, `main` @ `1cb7505e8bfe98dd0b240439c12436d52deb500e`, committed 2026-11-26T15:43:07Z (`fix: recreate a group when a recorded worker disappears (#1319)`) |
| Latest release | `v1.0.0`, 2026-10-16, commit `588881cada4fcc145e83eced00b88af920892ec6` |
| DisaggregatedSet | Exists under that name. Same repo and same controller-manager binary, registered unconditionally (no feature gate). API group `disaggregatedset.x-k8s.io`, single served version `v1` ([types][dt-ann], [group][dt-gv]); controller `pkg/controllers/disaggregatedset/`, webhook `pkg/webhooks/disaggregatedset/`, shared read layer `pkg/replicagroups/`. KEP-766 is `status: implementable`, `stage: alpha`; KEP-846 (slices) implementable; KEP-960 (sub-roles) and KEP-1022 (scaling during rollout) provisional. First shipped in v0.9.0 ([KEP-846][k846-compat]). So the API version says `v1` while the KEP says alpha. |
| Read in full or in the relevant parts | `api/leaderworkerset/v1/leaderworkerset_types.go`, `pkg/controllers/{pod_controller,pod_worker_uids,leaderworkerset_controller}.go`, `leaderworkerset_hash.go` (L45–300), `pkg/webhooks/pod_webhook.go`, `pkg/utils/{pod,revision}`, `api/disaggregatedset/v1`, DS `disaggregatedset_controller.go`, `lws_manager.go`, `executor.go` (L55–440), `status.go` (L36–208, L542–566), `pkg/replicagroups/observer.go` (L17–140), KEP-766, KEP-898, KEP-846, KEP-173 (transition section), failure-handling docs |
| Issues and PRs read | lws#1301, #1319, #998, #999, #280, #225, #3, #1247, #1249, #1359, #1177, #1174, #1172, #1258, #1283, #1213, #1292, #1180, #1150, #1148, #1134, #965. `lws#N` = `https://github.com/kubernetes-sigs/lws/issues/N` |

Two things differ from the maintainer's hypothesis.

- **main is not v1.0.0 for these problems.** The fix for lws#1301 merged about an hour before the clone. Checked on the `v1.0.0` tag: it has the UID-carrying queue key (#999) but lacks the worker UID record (#1319), the charged-leader token and the UID precondition on the restart-policy delete (#1249), the same for exhausted groups (#1359), and the takeover barrier (#1258).
- **DisaggregatedSet has no "replica made of one prefill and one decode".** Its hierarchy is DS → slice (an index) → revision (a hash) → one LWS per (slice, revision, role) → group (leader Pod + worker StatefulSet) → Pod. Roles are paired only as pools inside one slice and revision.

Evidence labels: **[C]** verified by reading code at the commit above; **[D]** stated in a doc, KEP, issue or PR; **[I]** inferred. The kthena side of the last table column is taken from the problem statement of the discussion and was not re-verified against kthena code.

### 2. Summary

| | What LWS does | What DisaggregatedSet does | Durable across restart? | Evidence | Same gap as kthena? |
| --- | --- | --- | --- | --- | --- |
| P1 identity | A group's identity is its leader Pod's UID. The queue key carries UID and a deleted-Pod snapshot; membership is checked through the ownerReference UID chain; leader deletes carry a UID precondition; the worker-UID record and the exhausted mark are patched with an optimistic lock (the backoff pending mark is a plain merge patch); in Ordinal mode the successor is held until the predecessor's Pods are gone, unless the replacement policy is `GroupReplacementImmediate`; Hash mode never reuses a name | Child LWS names are deterministic. Every LWS write is conditional on resourceVersion, deletes on UID + resourceVersion; new LWS are created at 0 replicas; after a restart each LWS is re-stamped to invalidate a predecessor's in-flight writes | Yes: enforced by the API server. The in-memory ledgers are rebuilt by a barrier read | [pc-req], [pc-uid], [pc-commit], [pc-bel], [pc-gate], [lm-prep] | **No** on main. **Partly** at v1.0.0 ([v1-del]) |
| P2 established | `None`, `RecreateGroupOnPodRestart` (default), `RecreateGroupAfterStart`. Evidence: deletion seen on the event, container restart counts, and (main only) a `worker-uids` record on the leader with a `:started` flag | Nothing of its own; each role's LWS applies its restart policy | On main yes, once the record is written. Up to v1.0.0 no (lws#1301) | [pc-loss], [wu-rec], [wu-det], [doc-fh] | **Partly**: no release has the fix yet; closed on main |
| P3 commit | No separate record by default: the commit is the foreground DELETE of the leader Pod, and the Kubernetes garbage collector finishes the scope. With a restart budget, count and leader-UID token are written before the delete | The scoped delete is a whole LWS (foreground, UID + resourceVersion). Rollout steps are not recorded; they are replanned from child `spec` | Yes | [pc-commit], [pc-chg], [lm-del], [dc-clean] | **No** |
| P4 where state lives | Per group on the leader Pod; rollout cursor on the leader StatefulSet or Deployment; LWS status holds counters and conditions only. Two JSON maps per LWS, keyed by revision and group index, exist in LWS annotations, opt-in | Status is keyed by role and sub-role, bounded by spec. Per-revision state is on child LWS annotations | n/a | [ty-st], [ty-rst], [pc-tok], [dt-st] | **Partly**: the parent maps have a hard size ceiling |
| P5 levels | The group is not a CR, but it is anchored on a real object, the leader Pod | DS and each (slice, revision, role) LWS are objects. Slice, revision, sub-role are not. A cross-role serving unit does not exist | n/a | [pc-wsts], [du-name], [dt-slice] | Different structure |
| P6 rolling update | Partition plus surge ordinals on the leader StatefulSet; state is its `spec` and two annotations; status recomputed every pass | One LWS per revision per role; a planner recomputes targets from child `spec` and readiness each reconcile | Yes, except two in-memory ledgers | [lc-plan], [lc-meta], [ex-roll], [st-pub] | **Partly**: open stall, no stalled signal |
| P7 migration | Template-affecting changes roll groups. Identity mode is immutable. Bookkeeping is migrated in place | No adoption of existing LWS. A naming change was bridged for two releases, then dropped | n/a | [k173], [wh-imm], [lc-place], [k846-mig] | Recreation is the norm |

### 3. Per problem

#### P1. Identity across same-name recreation

LWS, Ordinal mode (default: leader `<lws>-<i>`, workers `<lws>-<i>-<j>`, names reused):

- **The token is the leader Pod's UID [C].** The worker StatefulSet carries a controller ownerReference to the leader Pod. `workerPodBelongsToLeader` walks Pod → StatefulSet (UID) → leader (UID) and returns false on any mismatch ([pc-bel]). Its caller explains why: "so background deletion of the previous group does not recreate the replacement leader again" ([pc-own]).
- **The queue key is not a name [C].** "A StatefulSet can create a replacement with the same namespace and name before reconciliation starts, so a namespaced name alone is insufficient." The request is `{NamespacedName, UID, DeletedPod}` ([pc-req]). This is the fix for lws#998 (PR #999, in v1.0.0).
- **Leader delete events are dropped [C]:** "Replaying recovery from that snapshot could reset the budget or release worker finalizers for a replacement group with the same name/revision" ([pc-ldel]).
- **Writes are conditional [C].** All three leader deletes use `Preconditions{UID: &leader.UID}` and treat NotFound and Conflict as done: "Never delete a same-name replacement on behalf of the previous group" ([pc-commit], [pc-split], [pc-exh]). The exhausted mark and the worker UID record use `MergeFromWithOptimisticLock` ([pc-exh], [wu-rec]). At v1.0.0 the restart-policy delete had no precondition ([v1-del]); #1249 and #1359 added them after the release.
- **The predecessor blocks the successor [C].** Foreground deletion keeps the leader object until its dependents are gone. Every leader starts with a scheduling gate, lifted in Ordinal mode only when no other Pod carries the same group index ([pc-gate], [wh-key]); `GroupReplacementImmediate` skips this wait. The worker StatefulSet is not created while an old one is terminating ([pc-osts]).
- **The name-derived key is not an incarnation ID [C].** In Ordinal mode `group-key = sha1(namespace/podName)` ([wh-key]). Where a per-incarnation name was needed, a random one was added: "The statefulset controller recreates a leader with the same name, group index and revision, so without it the new leader would reuse a PodGroup that cleanup may already be deleting" ([sp-inc]).
- **A counter keyed by the reused slot was not enough [D].** lws#1247: "The count is keyed by `<revision>/<groupIndex>`, and in Ordinal mode the recreated leader has the same name and revision, so the count alone cannot tell 'this leader was already charged' from 'the new leader failed again'." The fix stores 64 bits of `sha256(leader UID)` next to the count ([pc-tok]).
- **Stale cache after a leader change [C], main only.** Rollout deletes use UID + resourceVersion and are tracked in memory until observed ([lc-del], [lc-pend]). Before the first rollout decision per LWS a new process does one uncached LIST: "A new leader can then act on a stale cache that still shows leader pods its predecessor deleted, while pendingDeletes starts empty" ([lc-take]).
- **Not covered [I]:** the pod controller has no fencing beyond the UID precondition. A late delete from a deposed leader still lands on the same incarnation. That costs one extra group restart; it cannot hit a successor.

Hash mode avoids reuse: each leader gets a random key, its hostname and worker StatefulSet are named from it, and "Group identity is not stable. A recreated group gets a new key" ([k898-cav]) [D].

DisaggregatedSet [C]:

- The name is `<ds>-<slice>-<revision>-<role>` and the revision is an 8-hex content hash ([du-name]). A rollback A → B → A reuses the name. A terminating target is skipped: "reconciliation waits until its deterministic name can be created again" ([lm-term]). A foreign object under the name is refused: "refusing to adopt it" ([lm-adopt], lws#981).
- "Create never starts Pods: an ambiguous request may commit after the user changes the desired revision" ([lm-create]). Scale re-checks UID, generation and replicas, then patches with an optimistic lock ([lm-scale]).
- "prepareSlice invalidates conditional LWS requests left in flight by a previous process ... Either an old request wins first (our patch conflicts), or our patch wins and the old request can no longer apply" ([lm-prep]).
- "Observer owns a cold-on-leadership informer cache. Its initial lists are ordered and consistent, so a new leader does not start from a warm standby's stale view" ([ob]).
- Unlike the LWS mechanisms above, these three are already in v1.0.0 (symbol check on the tag).

Confidence: high for the code paths. The behaviour under real races is not something I observed.

#### P2. "Established" versus "still being created"

Decision path on main [C] ([pc-loss], [pc-start]):

```go
if !isRecreatePending && !podutils.ContainerRestarted(pod) && !podutils.PodDeleted(pod) {
	if !isLeader {
		return false, ctrl.Result{}, nil
	}
	var err error
	workerLoss, err = r.detectWorkerUIDLoss(ctx, &leader)
```

| Evidence | Carrier | Survives a controller restart |
| --- | --- | --- |
| Worker deleted: `DeletionTimestamp` on the event object or the deleted-Pod snapshot ([pc-ev]) | workqueue, in memory | No |
| Container restarted: `RestartCount > 0` on a Running or Pending Pod ([pu]) | Pod status | Yes, while that Pod exists |
| Worker missing or replaced: `revision\|sha256:<digest of worker UIDs>:<count>[:started]` ([wu-rec]) | leader Pod annotation | Yes (main only) |
| Recreation decided but deferred by backoff ([pc-commit]) | leader Pod annotation | Yes |

"Started" means every member exists and none is Pending; a member the scheduler could not place makes the controller wait and emit `GroupUnschedulable` ([pc-gss]) [C].

- **(a) Worker lost while the controller is down and already recreated.** On main: the leader's reconcile compares the recorded count and digest with the current worker UIDs and, on a mismatch, enters the normal restart-policy path. The group is recreated per policy, provided a record existed ([wu-det]) [C]. For `RecreateGroupAfterStart` only `record.started` is consulted, so a Pending replacement does not block it ([pc-start]). At v1.0.0 and before: not recreated. The only signals there were the deletion event and the restart count ([v1-hrp]), and the reporter's envtest shows "Leader not deleted within 30s" for both offline cases (lws#1301) [D, not my observation].
- **Remaining window on main [D] + [I].** "The controller needs a prior record to recover a missed deletion" ([doc-fh]). The record is written on a leader reconcile once all workers exist, or once all have left Pending under AfterStart, and is skipped if it would not fit in the annotations ([wu-rec]). A group that exists when the controller is upgraded records whatever membership it has at its first reconcile, so a loss during the upgrade window is absorbed into the baseline [I].
- **(b) Pod lost during first start.** Default policy: yes, the whole group is recreated; no start-state check applies ([pc-start]) [C]. `RecreateGroupAfterStart`: no; the StatefulSet replaces that Pod alone. The restart count stays on a restarted Pod, so a restart during startup qualifies the group once it has started and that Pod is reconciled again [I].
- **Losing the leader is always a whole-group loss**, under every policy, because the worker StatefulSet is owned by the leader's UID. [I] for Ordinal from the ownerReference; [D] for Hash: "Losing the leader always replaces the whole group ... Workers cannot survive their leader" ([k898-cav]).
- **Why not a finalizer [D].** The first version of #1319 put a finalizer on every worker. The maintainer asked for the leader record instead: "this finalizer lands on almost every worker Pod. Every worker deletion ... then waits for a controller patch, which adds N writes per group recreation, and deleted workers stay `Terminating` whenever the controller is down or uninstalled. A leader record costs one write per group formation, cannot leave Pods stuck" ([review comment](https://github.com/kubernetes-sigs/lws/pull/1319#discussion_r4663692205)).

DisaggregatedSet: no group-level restart logic of its own [C]. A role-ratio "serving unit" with rebalancing after involuntary loss is only a proposal (lws#1134, open).

Confidence: high for main; the v1.0.0 statement rests on the tag's code plus the reporter's test.

#### P3. Commit of a scoped delete or recreate

LWS [C] ([pc-commit]):

```go
deletionOpt := metav1.DeletePropagationForeground
if err := r.Delete(ctx, &leader, &client.DeleteOptions{
	PropagationPolicy: &deletionOpt,
	Preconditions:     &metav1.Preconditions{UID: &leader.UID},
```

- **Default: the DELETE of the anchor is the commit.** With foreground propagation the leader stays, terminating, until the worker StatefulSet and its Pods are gone. The garbage collector finishes the scope, whether or not the LWS controller is running. The StatefulSet controller then recreates the leader. If the process dies before the DELETE, the decision is re-derived from the durable P2 evidence: the restart count, and on main the UID record.
- **Maintainer's statement of where the state is [D]** (lws#1283): "the controller state is already persisted: the leader `DeletionTimestamp`, the restart-count annotation, and the charged-leader token from #1249."
- **With `maxGroupRestarts` or `restartBackoff`:** count + 1 and the leader-UID token are written to LWS annotations first; then the delete. A retry that finds the token "retr[ies] that deletion without charging, exhausting, or backing off this same failure again, even if the Pod cache still shows it alive" ([pc-chg], [pc-commit]).
- **A deferred decision is not re-evaluated:** a recreation pending backoff "passed this guard when it was decided, so it is not checked again" ([pc-start]).
- **Spec change in between.** The restart policy is part of the revision: "an old group keeps its original restart behavior until its rolling-update turn" ([pc-rev]). The successor's worker StatefulSet is built from the ControllerRevision named by its leader's revision label, "not the live one" ([pc-wsts]). That is the fix for lws#280. `maxGroupRestarts` and `restartBackoff` are deliberately live and excluded from the revision ([rv-patch]).

DisaggregatedSet [C]: the only scoped deletes are whole LWS objects (drained old revisions, removed slices), foreground with UID + resourceVersion ([lm-del]). Nothing is recorded before a rollout step. "The planner does not store or advance through numbered steps" ([KEP-766][k766-state]); the executor drains old first, then grows the target ([ex-apply]), and a crash in between is replanned from the new `spec`. Two durable aids: `initial-replicas` on each LWS freezes when a revision becomes old ([dt-ann]), and the newest drained LWS is kept, empty, as "a temporary rollout marker" until the target is Ready ([dc-clean], [KEP-766][k766-order]).

Confidence: high.

#### P4. Where per-unit state lives and how big it is

LWS [C]:

| Carrier | What | Size and write pattern |
| --- | --- | --- |
| Leader Pod | `worker-uids`, `group-recreate-pending`, `group-restart-budget-exhausted`, cleanup finalizer, replacement scheduling gate, `group-ready` condition ([ty-rst], [ty-gate]) | Fixed size per group. The UID record is a digest, not a list: one write per group formation |
| Leader StatefulSet or Deployment | `spec.replicas` (includes surge), partition, `replicas` and `rollout-baseline-replicas` annotations ([lc-meta], [lc-base]) | One object per LWS |
| Worker StatefulSet | ownerReference to the leader, group and revision labels | One object per group |
| ControllerRevision | template per revision | Truncated when an update completes |
| LWS status | `replicas`, `readyReplicas`, `updatedReplicas`, conditions, `hpaPodSelector`, `observedGeneration` ([ty-st]) | No per-group list |
| LWS annotations | `group-restart-counts`, `group-restart-charged-leaders`: JSON maps keyed `<revision>/<groupIndex>` | Only with a budget or backoff; one entry per group that has restarted |
| Memory | pending leader deletes, takeover snapshot, metric dedupe LRU | Rebuilt or re-derived |

The two per-LWS maps have a documented size [C] + [D]. The code comment says it "keeps 4,000 groups' count and token annotations around 221 KiB" ([pc-tok]); #1249 gives 225,887 bytes and adds "There is no entry cap, eviction ...". The annotation limit is 256 KiB and `spec.replicas` allows 1,000,000 ([ty-max]). The write is a conflict-retried full `Update` of the LWS, so all groups of one LWS serialise on one object, and a size error is returned before the leader delete ([pc-mut], [pc-commit]). So past the ceiling a charged recreation is blocked [I].

DisaggregatedSet [C]: status is a list per role (at most 10) with nested sub-roles (at most 32), totals across slices; nothing per slice, revision or group ([dt-st]). The DS itself carries `max-unavailable` and `revision-hash-version` annotations. Each child LWS carries `initial-replicas`, the sub-role maps and an `observation-receipt` rewritten on every conditional write ([lm-pend]). The unobserved-write ledger and stale-status cache are in memory.

No scale test result was found. #1172 says of the DS cache: "Large-cluster cost has not been benchmarked" [D]. One write-amplification fix is on record: #1213 stopped repeated DS status writes against a stale parent version.

#### P5. The level below and above the object

| Level | An object? | Identity | Status | Garbage collection |
| --- | --- | --- | --- | --- |
| LeaderWorkerSet | CR | UID | counters, conditions | owns leader StatefulSet or Deployment, shared Service, ControllerRevisions |
| Group (an LWS replica) | No CR; anchored on the leader Pod | leader Pod UID, plus ordinal or random key | none in LWS status; leader annotations and conditions | worker StatefulSet and per-replica Service are owned by the leader Pod |
| DisaggregatedSet | CR | UID | per role and sub-role | owns its LWS and RoleScalers |
| Slice | No: a label and a name segment ([dt-slice]) | index | none | the controller deletes LWS of removed slices |
| Revision | No: an 8-hex hash label and name segment; no ControllerRevision at DS level | content hash | none | the controller deletes drained LWS |
| (slice, revision, role) | LWS | UID | LWS status | ownerReference to the DS |
| Sub-role | No: a Pod label and LWS annotations | name | `SubRoleStatus` | – |
| Cross-role serving unit | Does not exist | – | – | proposed in lws#1134 |

- At the group level P1–P3 are handled through the anchor: the leader Pod supplies the UID, the annotation carrier, the finalizer and the cascade. The price is that leader loss always means group loss.
- At the slice and revision levels there is nothing to protect separately, because every destructive operation targets an LWS object.
- KEP-766 moved towards more objects, not fewer: one LWS per revision, treating "LWS as a read-only resource (similar to how Deployment treats ReplicaSet)", rejecting partition-based in-place updates ([k766-alt4]) [D].
- **Was a per-group object proposed?** I found none. `keps/` has no such proposal (grep for group object/CRD wording). The nearest are KEP-898 Alternative 2, "Controller-managed bare leader pods", rejected because it "reimplements what ReplicaSets already do well" ([k898-alt]), and lws#965 (open), which asks for hierarchical workload groups with status aggregated through the hierarchy; the only maintainer reply points to DisaggregatedSet. Per-group PodGroup objects exist for scheduling only.

#### P6. Coordinated rolling update

LWS [C]:

- Ordinal: the cursor is the leader StatefulSet's partition. Surge groups are ordinals at or above `spec.replicas`, created on the new revision and trimmed at the end ([lc-plan]). The `replicas` annotation records the LWS replica count at the last apply and separates a new scale request from rollout surplus. `rollout-baseline-replicas` (main only) records the pre-rollout count for the availability floor ([lc-base]). With a user partition, surge replicas stay "until the rolling update is completely done and the partition field is reset to 0" ([ty-ru]).
- Group states are recomputed every pass from leader Pods, worker StatefulSets and worker Pods ([lc-states]); status and conditions likewise ([lc-cond]). The only non-durable piece is the pending-delete ledger, covered by the takeover LIST.
- Hash mode: a Deployment does the rollout; a `group-ready` readiness gate on the leader makes it count whole groups ([ty-gate]).
- History [D]: ordinal surge was contested in lws#3 ("replicas have stable identities that are baked into the pod names"). lws#225: a controller restart mid-rollout updated every worker StatefulSet at once; fixed in 2024 by #229, which stopped patching existing worker StatefulSets. lws#717: template change plus scale-up deadlocked; fixed on main by #1258; the semantics issue lws#1018 is still open.

DisaggregatedSet [C] + [D]:

- Each role is one dimension. Per slice, one old revision at a time drains while the target grows. Progress per role is a fraction, and roles must stay within a window of `1 / min(positive role size)` ([k766-state]). `spec` is issued work, Ready is credit; a revision's Ready replicas count as serving capacity only if every role it requires has a Ready group.
- Stored: `initial-replicas`, `max-unavailable`, `revision-hash-version`, the receipt. Recomputed each reconcile: everything else ([ex-roll]). Status sums child LWS status ([st-pub]).
- Known stalls: lws#1292 (open) is a permanent deadlock between pools with disjoint budgets, reported as `Progressing=True, Available=False` "with no escalation". Closed: #1180, #1150, #1148. `Available` is False during every rollout by definition, which misled consumers and led to `MinimumReplicasAvailable` (lws#1191, [k766-report]).
- Neither API has a progress deadline or a Stalled condition (grep of `api/` and `pkg/`) [C]. A blocked DS rollout emits a Normal event `RevisionDrainBlocked` every 10 s and keeps `Progressing=True` with reason `RolloutInProgress` ([ex-roll], [st-cond]).

#### P7. Upgrade and migration

- **Subdomain policy [D] + [C]:** "a change in subdomain policy will trigger a rolling update" ([k173]); `networkConfig` is part of the revision ([rv-patch]). Groups are recreated.
- **Group identity mode [C] + [D]:** immutable on an LWS ([wh-imm]); migrating a live LWS is a non-goal ([k898-ng]). Through a DS the change is an ordinary rollout that replaces whole LWS objects. Downgrading with Hash workloads is documented as unsupported ([k898-cav]).
- **Migrated in place, no restart [C]:** ControllerRevisions created before placement was recorded are replaced under the same key; otherwise "every group would be replaced at once" ([lc-place]). The default termination policy is not stamped on the template because that "would change the StatefulSet and roll existing groups on upgrade" ([lc-seq]). The worker UID baseline is recorded on existing leaders ([pc-rec]).
- **Recreated, not migrated [C]:** a group whose leader and workers were admitted by different LWS versions and so joined different PodGroups is recreated, only while no worker is scheduled ([pc-split]).
- **DS naming change [D]:** adding the slice segment would have renamed and recreated every LWS. v0.10 and v0.11 adopted old objects in place as slice 0 and converged at their next rollout; v1.0.0 removed the path: "Direct upgrades that leave pre-slices objects behind are not supported" ([k846-mig], [k846-compat]).
- **DS revision hash change [C]:** legacy sets "remain on v1 indefinitely", selected by a marker annotation written before the first LWS is created ([dc-rev]).
- **Adoption [C]:** a DS never adopts an existing LWS ([lm-adopt]).

### 4. What kthena could borrow, and what this warns against

Could borrow:

1. **Anchor the unit on one member whose loss is defined as loss of the unit.** LWS then gets identity, the established record and the scope commit from that member, and the garbage collector completes the scope ([pc-commit], [wu-rec]). In kthena terms this fits a Role instance only if "entry lost means recreate the instance" is acceptable under every policy.
2. **Record "established" as a fixed-size digest, count and started flag,** tied to the revision, written once with a resourceVersion lock ([wu-rec]).
3. **Carry the UID in the queue key and in every destructive write.** Five separate fixes were needed between August and November 2026: #999, #1174, #1177, #1249, #1359.
4. **Fence a predecessor without leases:** re-stamp each child on takeover, create at zero, and start from a cold consistent cache ([lm-prep], [lm-create], [ob]); or one uncached LIST per parent ([lc-take]).

Warns against:

5. **Pod finalizers as the durable record.** The LWS maintainer rejected them for the reason the design notes of the kthena prototype give: Pods stuck Terminating when the controller is down, and N extra writes per recreation (#1319 review).
6. **A counter keyed by a reused slot** cannot tell incarnations apart (lws#1247).
7. **Per-group entries in maps on the parent** have a hard ceiling (about 221 KiB at 4,000 groups, no cap or eviction) and one writer queue per parent ([pc-tok], [pc-mut]). LWS accepted that only for an opt-in feature.
8. **"Stateless, recompute from children" did not stay stateless.** DS still needed at least four durable annotations and two in-memory ledgers, and has an open stall with no stalled signal (lws#1292). Separately, LWS found its offline-loss gap only after 1.0, from a test that stops and restarts the manager (lws#1301); a restart test that only re-listed did not expose it (lws#1283).

### 5. Unverified points and limits

- Nothing was run. Runtime statements about lws#1301 and lws#998 are the reporters' observations.
- The clone was shallow. Attribution of a mechanism to a PR comes from PR descriptions, not `git blame`.
- The v1.0.0 comparison is a symbol check on the tag plus a read of `handleRestartPolicy` there, not a full diff. Releases before v1.0.0 were not examined.
- Not read: `planner*.go` (the planner mathematics), `subrole_groups.go`, `scaler_manager.go`, `pkg/computedomain`, the second half of `leaderworkerset_hash.go`, KEP-849, and KEP-960 and KEP-1022 beyond their metadata.
- "Ordinal leader loss removes the worker StatefulSet under policy `None`" is inferred from the ownerReference and garbage-collection semantics.
- "No per-group object was proposed" is absence of evidence. Searched: `keps/` and docs by grep; issues and PRs for "group CRD", "group status", "per-replica status", "RecreateGroup", "controller restart", "same name replacement", "takeover", "stale cache", "in-flight", "annotation size", "DisaggregatedSet". Not searched: Slack, meeting notes, the original design document linked from lws#3.
- No benchmark or documented scale limit was found beyond the 4,000-group annotation figure.
- `main` moves fast here: two of the cited fixes (#1319, #1359) merged on 2026-11-26, the day of this research.

[ty-rst]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/leaderworkerset/v1/leaderworkerset_types.go#L115-L147
[ty-gate]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/leaderworkerset/v1/leaderworkerset_types.go#L189-L200
[ty-max]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/leaderworkerset/v1/leaderworkerset_types.go#L221-L225
[ty-ru]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/leaderworkerset/v1/leaderworkerset_types.go#L662-L711
[ty-st]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/leaderworkerset/v1/leaderworkerset_types.go#L768-L795
[pc-req]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L106-L114
[pc-ldel]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L160-L172
[pc-rev]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L269-L283
[pc-rec]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L318-L325
[pc-osts]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L478-L483
[pc-split]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L521-L574
[pc-uid]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L612-L623
[pc-loss]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L642-L655
[pc-own]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L657-L670
[pc-chg]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L692-L705
[pc-start]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L707-L739
[pc-commit]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L740-L799
[pc-tok]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L863-L874
[pc-exh]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L921-L973
[pc-mut]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1177-L1241
[pc-gate]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1391-L1408
[pc-bel]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1558-L1585
[pc-gss]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1694-L1713
[pc-wsts]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1810-L1816
[pc-ev]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_controller.go#L1937-L1983
[wu-rec]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_worker_uids.go#L56-L100
[wu-det]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/pod_worker_uids.go#L128-L141
[wh-key]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/webhooks/pod_webhook.go#L106-L146
[wh-imm]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/webhooks/leaderworkerset_webhook.go#L174-L180
[lc-base]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L731-L757
[lc-plan]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L855-L1137
[lc-cond]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L1347-L1505
[lc-states]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L1623-L1759
[lc-pend]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L1761-L1784
[lc-take]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L1901-L1940
[lc-del]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L1942-L1998
[lc-place]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L2435-L2464
[lc-seq]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L2527-L2531
[lc-meta]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/leaderworkerset_controller.go#L2546-L2593
[rv-patch]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/utils/revision/revision_utils.go#L411-L468
[sp-inc]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/schedulerprovider/kubernetes_provider.go#L159-L183
[pu]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/utils/pod/pod_utils.go#L30-L51
[dt-ann]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/disaggregatedset/v1/disaggregatedset_types.go#L26-L85
[dt-slice]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/disaggregatedset/v1/disaggregatedset_types.go#L274-L280
[dt-st]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/disaggregatedset/v1/disaggregatedset_types.go#L366-L446
[dt-gv]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/api/disaggregatedset/v1/groupversion_info.go#L27-L30
[dc-rev]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/disaggregatedset_controller.go#L257-L296
[dc-clean]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/disaggregatedset_controller.go#L642-L707
[lm-create]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L289-L296
[lm-adopt]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L381-L394
[lm-scale]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L420-L455
[lm-del]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L640-L658
[lm-term]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L724-L730
[lm-prep]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L823-L864
[lm-pend]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/lws_manager.go#L866-L950
[du-name]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/utils/disaggregatedset/utils.go#L97-L125
[ex-roll]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/executor.go#L164-L242
[ex-apply]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/executor.go#L362-L420
[st-pub]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/status.go#L41-L178
[st-cond]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/controllers/disaggregatedset/status.go#L542-L560
[ob]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/pkg/replicagroups/observer.go#L50-L122
[k766-state]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/766-DisaggregatedSet/README.md?plain=1#L166-L228
[k766-order]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/766-DisaggregatedSet/README.md?plain=1#L345-L357
[k766-report]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/766-DisaggregatedSet/README.md?plain=1#L359-L371
[k766-alt4]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/766-DisaggregatedSet/README.md?plain=1#L515-L539
[k898-ng]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/898-group-identity/README.md?plain=1#L59-L64
[k898-cav]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/898-group-identity/README.md?plain=1#L88-L100
[k898-alt]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/898-group-identity/README.md?plain=1#L262-L268
[k846-mig]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/846-disaggregatedset-slices/README.md?plain=1#L90-L94
[k846-compat]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/846-disaggregatedset-slices/README.md?plain=1#L165-L169
[k173]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/keps/173-headless-service-per-replica/README.md?plain=1#L164-L187
[doc-fh]: https://github.com/kubernetes-sigs/lws/blob/1cb7505e8bfe98dd0b240439c12436d52deb500e/site/content/en/docs/concepts/leaderworkerset/failure-handling/_index.md?plain=1#L55-L63
[v1-hrp]: https://github.com/kubernetes-sigs/lws/blob/588881cada4fcc145e83eced00b88af920892ec6/pkg/controllers/pod_controller.go#L562-L567
[v1-del]: https://github.com/kubernetes-sigs/lws/blob/588881cada4fcc145e83eced00b88af920892ec6/pkg/controllers/pod_controller.go#L685-L690
