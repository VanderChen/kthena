# ModelServing recovery policy semantics

## Motivation

`None` should disable proactive failure recovery. Treating it as a scope for
recreating one Pod obscures the fact that kubelet can restart containers inside
the existing Pod. Some runtimes also tolerate container restarts but need their
Role instance or ServingGroup to restart together when a Pod is deleted.

## API

Keep the existing policy enum and `spec.template.restartGracePeriodSeconds`:

| Setting | Existing unhealthy Pod | Deleted Pod |
| --- | --- | --- |
| SG/Role policy, grace `0` | Start recovery immediately | Recreate the configured scope |
| SG/Role policy, positive grace | Wait for Ready; recover if still unhealthy | Recreate the configured scope |
| SG/Role policy, grace `-1` | Retain the Pod indefinitely | Recreate the configured scope |
| `None`, any valid grace | Retain the Pod; kubelet follows its restartPolicy | Replace only the missing Pod |

A Role scope means one Role instance, including its entry and workers. The
defaults remain `RoleRecreate` and `0`. Both the CRD and webhook reject values
below `-1`.

Existing `Failed` Pods are also retained under `None` or `-1`. Their terminal
phase means they require external deletion or other user action to recover.
Readiness and available replica counts continue to reflect actual health.
Rollout and scaling use their own settings.

## Controller behavior

The controller stores the first observed failure time by Pod UID. Pending
recovery tasks recheck the informer cache at most one second apart and calculate
remaining grace using the current ModelServing configuration. Switching to
`None` or `-1` clears pending recovery; extending a finite duration keeps the
original start time. A Ready Pod clears its failure
time, so a later failure starts a new grace period.

Before deletion, re-read the Pod and ModelServing from the informer cache and
check ownership, identity, readiness, and the current recovery settings. Keep the
Pod UID delete precondition. Already submitted deletions and Role/ServingGroup recreations cannot be undone by a later policy
change. An old recovery task cannot clear a newer failure episode.

Pod creation must not delete an existing Failed Pod as a name-conflict shortcut;
that bypasses the configured recovery policy and finite grace period. Normal
reconciliation still fills missing Pods, including under `None`.

## Compatibility and verification

Earlier versions treated negative grace periods as immediate recovery and could
delete terminal Failed Pods under `None`. Operators must review those settings
before upgrading. Recovery settings remain outside workload revision hashes.

Regression tests cover container and init-container restarts, Failed Pods,
readiness recovery, policy changes during grace, cache checks before deletion,
missing-Pod replacement, and creation conflicts. Kind verification compares Pod
UIDs across two ServingGroups with multiple Role instances and workers, including
controller restarts while failed Pods are retained.
