# Fix Rolling Update Stuck with Terminating PodGroup

## Problem Scenario

When performing a rolling update (e.g., updating the container image) on a `ModelServing` workload that has **GangPolicy** or **Ranktable** configured, the following behavior may be observed:
1.  The existing pods are terminated (scaled down) as part of the update process.
2.  New pods are not created, or they fail to get scheduled.
3.  The workload effectively scales down to 0 and does not recover.

## Root Cause Analysis

When `GangPolicy` or `Ranktable` is enabled, the ModelServing controller manages a `PodGroup` custom resource for gang scheduling. During a rolling update:

1.  The controller deletes the old `PodGroup` associated with the workload or serving group.
2.  The `PodGroup` enters a `Terminating` state. This process is asynchronous and might take some time depending on the cluster state and finalizers.
3.  The controller immediately attempts to ensure the `PodGroup` exists for the new/updated workload.
4.  It retrieves the existing `PodGroup` (which is now `Terminating`).
5.  Previously, the controller would see the `PodGroup` exists and attempt to **update** it with the new requirements.
6.  Updating a `Terminating` resource is often futile or results in the scheduler ignoring the group or the pods being unable to bind to a group that is being deleted. This prevents the new pods from being successfully scheduled.

## Solution

The solution involves modifying the `PodGroup` management logic in the controller:

1.  In the `CreateOrUpdatePodGroup` function, after retrieving an existing `PodGroup`, we now explicitly check its `DeletionTimestamp`.
2.  If `DeletionTimestamp` is set (indicating the resource is `Terminating`), the function returns an error instead of proceeding with an update.
3.  This error triggers a requeue/retry in the controller's sync loop.
4.  The controller waits (via retries) until the old `PodGroup` is completely removed from the system.
5.  On a subsequent retry, the `PodGroup` is not found, and the controller creates a fresh, healthy `PodGroup`.
6.  The new pods can now bind to this new `PodGroup` and be scheduled correctly.

### Code Change Summary

In `pkg/model-serving-controller/podgroupmanager/manager.go`:

```go
podGroup, err := m.volcanoClient.SchedulingV1beta1().PodGroups(ms.Namespace).Get(ctx, pgName, metav1.GetOptions{})
if err != nil {
    // ... handling for not found ...
}

// NEW CHECK
if podGroup.DeletionTimestamp != nil {
    return fmt.Errorf("PodGroup %s is terminating", pgName)
}

return m.updatePodGroupIfNeeded(ctx, podGroup, ms)
```
