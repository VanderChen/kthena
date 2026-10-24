# Following a ModelServing through controller logs

The workload chart runs the controller with `--v=2` by default. Business logs identify the
ModelServing as `modelServing="namespace/name"` and include its `uid` and `generation`.
Use the UID to distinguish a recreated object with the same name. Use the generation to
connect a configuration change to subsequent work; one workqueue attempt may combine
several informer notifications.

```sh
kubectl -n kthena-system logs deployment/kthena-controller-manager --since=30m \
  | grep 'modelServing="inference/my-model"'
```

## Following a partition change

Changing only partition can release previously protected instances without creating a new
template revision. At the default verbosity, follow these records for the same owner:

1. `ServingGroup rollout partition changed` or `Role rollout partition changed` records
   the old/new configured values, resolved ordinal boundaries, and generation. Role logs
   name the Role. `active=false` means that layer's partition is not used by the current
   rollout strategy. Percentage boundaries can also change when replicas change.
2. `Pod deletion scope marked` and `Pod deletion requests accepted` identify the actual
   SG/Role selected for rollout, with `action="rollout"`, `reason="OutdatedTemplate"`,
   partition and target revision. Selection is still subject to readiness, budgets and
   dependencies; lowering partition does not guarantee an immediate deletion.
3. `Role Pod creation requests accepted` identifies the applied revision and accepted
   Pod count. Existing Pods do not count as newly created Pods.
4. `Role became available` and `ServingGroup became available` report readiness
   observed by the normal processing flow. A partition stop can be available without
   having every instance at the target version; use the applied revisions in action
   logs and inspect the ModelServing status when checking the overall result.

Raising partition reports the new protection boundary; it does not claim that existing
instances were rolled back. Ordinary replica changes, recovery and surge cleanup use
their own action/reason fields rather than being reported as template rollout.

## Levels and outcomes

| Level | Content |
| --- | --- |
| Info (`--v=0` and above, including the default `--v=2`) | Accepted business actions, recovery scheduling/cancellation, real availability transitions, configuration changes, coordination progress/blocking changes |
| `--v=4` | Reconcile start/end with stage and duration, Pod callbacks, per-Pod identities, budget/partition/dependency decisions, repeated waits, successful status writes, mechanical PodGroup/plugin writes, Kubernetes Event mirrors |
| Error | Actual API/plugin/history failures with the responsible object and operation; worker failures also identify the stage and retry count |

`requestAccepted` means an API request succeeded; it does not mean the Pod has disappeared
or become Ready. `observedAbsent` means the controller observed removal. Availability
requires the complete Role/ServingGroup, including its required workers. A failed request
with an unknown outcome remains an error, even if a later observation finds the desired result.

`stateStore="datastore"` denotes an internal readiness transition, not a ModelServing
`/status` API write. Repeated observations of the same state do not generate another
availability transition. Kubernetes Events remain available independently of their log mirror.

Status persistence emits a short diagnostic only after a successful API write. Logs do
not separately compare status fields or mirror every Condition. Coordination logs reuse
the existing coordination-change branch and its reason, alongside the existing Event.

## Diagnosing a wait or recovery

Use `--v=4` when an object is waiting without a new action or coordination change. Filter by owner
and then SG/Role to inspect partition protection, healthy/total deletion budgets,
Terminating occupancy, dependency/maxSkew limits, or missing revision history. This is a
diagnostic setting and can produce many per-member records.

Recovery records distinguish actual Pod deletion from unhealthy restarts/Failed Pods,
the chosen scope, grace scheduling and cancellation, and original-UID deletion resumed
after restart. Plain NotReady or Terminating is not itself logged as a recovery action.
PodGroup writes remain visible at V(4); reducing their logging level does not reduce API
writes or resolve scheduler/API performance problems.
