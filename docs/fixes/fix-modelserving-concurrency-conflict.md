# [Bug] ModelServing concurrency conflict causes valid pods to be removed from store

## Description

There is a race condition in the `modelserving-controller` that occurs when a `ModelServing` resource is deleted and immediately recreated with the same name. Due to the deterministic naming of Pods (e.g., `<ms-name>-<index>-<role>-<id>`), the Pods for the new `ModelServing` instance share the same names as the Pods from the deleted instance.

When the old Pods are deleted (asynchronously by Kubernetes), their `delete` events are processed by the controller. If the controller has already synced the new `ModelServing` and added its (new) Pods to the internal `Store`, processing the `delete` event for the *old* Pod (which has the same name) incorrectly removes the *new* Pod from the `Store`.

## Impact

*   **Inconsistent Internal State:** The controller's internal `Store` loses track of running Pods for the new `ModelServing`.
*   **Status Errors:** The `ModelServing` status (e.g., `AvailableReplicas`) becomes incorrect.
*   **Unnecessary Recreation:** The controller may attempt to recreate Pods that actually exist and are running, potentially leading to errors or instability.

## Analysis

The issue lies in the `deletePod` function in `pkg/model-serving-controller/controller/model_serving_controller.go`.

### Scenario
1.  **Delete Old:** User deletes `ModelServing` A (Old).
2.  **Create New:** User creates `ModelServing` A (New).
3.  **Sync New:** Controller processes the new `ModelServing`, creates new Pods (e.g., `pod-0` with Revision 2), and adds them to the `Store`.
4.  **Delete Old Pod:** Kubernetes deletes the old Pods (e.g., `pod-0` with Revision 1).
5.  **Process Event:** Controller receives the `delete` event for the old `pod-0`.
6.  **Conflict:**
    *   The controller identifies `pod-0` belongs to `ModelServing` A (which exists, it's the New one).
    *   It calls `c.store.DeleteRunningPodFromServingGroup(..., pod.Name)`.
    *   **Result:** The entry for `pod-0` is removed from the `Store`, even though it referred to the *New* `pod-0`.

### Root Cause
The code executed the store deletion *before* verifying if the Pod's revision matched the current ServingGroup's revision.

```go
// Old Logic
func (c *ModelServingController) deletePod(obj interface{}) {
    // ...
    // 1. Removes pod from store based on name (collides with new pod)
    c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), servingGroupName, pod.Name)

    // 2. Checks revision (Too late!)
    if c.shouldSkipPodHandling(ms, servingGroupName, pod) {
        return
    }
    // ...
}
```

## Fix

The fix involves moving the revision check (`shouldSkipPodHandling`) *before* modifying the `Store`.

```go
// New Logic
func (c *ModelServingController) deletePod(obj interface{}) {
    // ...
    // 1. Check revision first. If it's an old pod, we skip everything.
    if c.shouldSkipPodHandling(ms, servingGroupName, pod) {
        return
    }

    // 2. Only remove from store if revision matches (i.e., it's actually the current pod being deleted)
    c.store.DeleteRunningPodFromServingGroup(utils.GetNamespaceName(ms), servingGroupName, pod.Name)
    // ...
}
```

This ensures that `delete` events from outdated revisions do not affect the state of the currently running `ModelServing`.
