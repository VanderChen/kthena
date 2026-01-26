# Fix ServingGroupRecreate Policy Issue

## Problem
When `recoveryPolicy` is set to `ServingGroupRecreate`, deleting a pod should trigger the recreation of the entire ServingGroup. However, users reported that pods were not being rebuilt, and no error logs were observed.

This issue can occur due to two reasons:
1. `deleteServingGroup` failures (e.g., API errors) were being ignored in `handleDeletedPod`.
2. `shouldSkipPodHandling` was filtering out pod deletion events if the pod's revision didn't match the `ServingGroup`'s revision. This could happen if the `ServingGroup` status was already set to `Deleting` (by the first pod's deletion) but subsequent pod deletion events (triggered by `DeleteCollection`) were skipped due to revision mismatch, causing the `ServingGroup` to get stuck in the `Deleting` state and never be removed from the store, preventing recreation.

## Solution
1. Modified `handleDeletedPod` in `pkg/model-serving-controller/controller/model_serving_controller.go` to propagate errors from `deleteServingGroup`.
2. Modified `shouldSkipPodHandling` in `pkg/model-serving-controller/controller/model_serving_controller.go` to always allow processing of pod events if the `ServingGroup` status is `Deleting`, regardless of revision mismatch. This ensures that cleanup logic in `handleDeletionInProgress` (checking if all pods are gone) can execute.

## Verification
- Verified that `handleDeletedPod` returns errors.
- Verified that `shouldSkipPodHandling` allows processing when status is `Deleting`.