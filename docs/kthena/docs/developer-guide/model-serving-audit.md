# ModelServing live audits

The ModelServing controller uses one namespace/name workqueue for startup,
watch notifications, retries, and periodic audits. Informer callbacks enqueue
observations; workers own recovery decisions, lifecycle hooks, and operation
state. The audit does not replace, rewrite, or restart informer caches.

## Configuration

`--modelserving-audit-period=5m` periodically enqueues the union of ModelServing
cache keys and internal datastore keys. `0` disables this timer, not ordinary
events or targeted live verification. `--modelserving-audit-timeout=30s` bounds
each reconciliation's observation and actions. Use the chart's
`workload.controllerManager.image.args` to override these flags.

A live audit directly gets its ModelServing and lists Pods, Services and,
when installed, PodGroups within its namespace and label scope. Lists are
paginated; partial failures are not published. ConfigMaps are read on demand
by plugins. The controller checks owner UID, uses UID/version deletion
preconditions, and preserves deletion intent until cleanup succeeds.

If the informer and live object identities/versions differ, that key keeps
using live observations on subsequent reconciliations. A later live audit
must confirm that the cache has caught up before cache reads resume. API
errors retain the live-read requirement and use workqueue backoff. No global
lock is held during API requests. Worker count and existing client QPS/burst
settings bound concurrency; large installations should budget for paginated
reads during each audit cycle.

## Recovery and plugins

Current Ready state takes precedence over historical container restart counts.
Grace periods use UID-keyed deadlines and delayed queue entries; expiry checks
the live Pod's identity and readiness again. None, RoleRecreate and
ServingGroupRecreate retain their recovery scopes. In-memory running history
distinguishes an initially incomplete Role from one that lost running Pods.

Readiness calibration never discards Deleting Role/ServingGroup state, including
when zero Pods remain. Required hook failures remain retryable and are not
counted as successful readiness. Lifecycle hooks are at-least-once and must
be idempotent, not exactly-once. The existing PodCreate/Running/Ready/Delete
and Role/ServingGroupDelete interfaces remain available for private plugins.

Ranktable's RoleSync performs steady-state repair. It filters owner UID and
uses historical Role topology for partitioned instances. Templates using
`.Timestamp` receive a stable latest observed lifecycle transition time rather
than the wall clock of each render, preventing periodic writes without input
changes. Output ownership is checked before update or deletion.

## Observability

With the existing localhost-only `--debug-port` enabled, `/metrics` exposes:

- `kthena_modelserving_audit_total{result}` and `audit_duration_seconds`.
- `kthena_modelserving_audit_last_success_timestamp_seconds`.
- `kthena_modelserving_audit_cache_drift_total`.
- `kthena_modelserving_audit_pending_operation_age_seconds` (sampled age of
  unresolved cleanup/recovery at reconciliation).
- `kthena_modelserving_observation_enqueues_total{mode}` and `queue_depth`.
- `kthena_modelserving_audit_observation_reads_total{resource}` (scoped
  observation reads, including pagination, not all Kubernetes traffic).

All abbreviated metric names above have the `kthena_modelserving_` prefix.
Labels are bounded categories, not object names or UIDs. V(2) audit logs include
the ModelServing key, UID, cache drift, duration and result. Alert on repeated
audit errors and growing pending-operation age; a recent controller-wide
success does not prove every object has converged.

## Limits and verification

Different Kubernetes resource lists are not a cross-resource transaction.
Safety relies on ownership/version checks, idempotence and retry. Unknown
ControllerRevision history blocks template-dependent actions; it never falls
back to a guessed current template. Rollout budgets and partition/dependency
rules still use the normal reconciliation path.

An in-memory checkpoint cannot reconstruct every missed historical event
after process restart or leader change. Precise cross-process recovery intent
would need a separate persistent design. Private plugin implementations must
be integration-tested by their owners; public hook-contract tests do not
substitute for that validation.

`live_audit_kind_test.go` is compiled only with `-tags=integration`. Its
notification gate deliberately loses business notifications while real Kind
watch caches continue updating. It requires an explicit disposable kubeconfig,
the shipping controller stopped, and exercises the actual default five-minute
timer; there is no production fault-injection endpoint.
