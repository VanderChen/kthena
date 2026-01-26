# Fix Scale Down Priority Logic

## Issue Description

The previous implementation of the scale-down logic in `ModelServingController` had an incorrect priority order. It prioritized the index (higher index deleted first) over the status and deletion cost. This led to situations where healthy, running pods with higher indices were deleted before pending or unhealthy pods with lower indices, or where pods with higher deletion costs were deleted before those with lower costs.

## Solution

The scale-down logic in both `scaleDownRoles` and `scaleDownServingGroups` has been updated to strictly follow this priority order:

1.  **Status (Primary)**: Roles/ServingGroups in non-running states (e.g., `Creating`, `Scaling`, `Deleting`) are prioritized for deletion. This ensures that pending or stuck resources are removed first.
2.  **Deletion Cost (Secondary)**: Among resources with the same status, those with a lower aggregate Pod Deletion Cost are deleted first. This respects the `controller.kubernetes.io/pod-deletion-cost` annotation.
3.  **Index (Tertiary)**: Finally, if status and deletion cost are equal, resources with higher indices are deleted first to maintain index continuity (similar to StatefulSet behavior).

## Changes

- Modified `scaleDownRoles` in `pkg/model-serving-controller/controller/model_serving_controller.go` to reorder the sorting logic.
- Modified `scaleDownServingGroups` in `pkg/model-serving-controller/controller/model_serving_controller.go` to reorder the sorting logic.
- Updated unit tests in `pkg/model-serving-controller/controller/model_serving_controller_test.go` to verify the correct priority behavior.

## Verification

New and existing unit tests confirm that:
- Unhealthy/Pending instances are deleted before Running ones, regardless of index or cost.
- Instances with lower deletion cost are deleted before those with higher cost (given same status).
- Higher indices are deleted first only when status and cost are tied.
