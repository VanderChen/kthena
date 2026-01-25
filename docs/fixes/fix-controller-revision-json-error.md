# Fix ControllerRevision JSON Object Error

## Problem Description
When `ModelServing` controller attempts to perform a rolling update (especially with `gangPolicy` or `recoverPolicy` enabled), it tries to create a `ControllerRevision` to store the history of the `Roles` template.

The error `ControllerRevision.apps "..." is invalid: data: Required value: data must be a valid JSON object` occurs because the controller passes a slice of roles (`[]Role`) directly as the data for `ControllerRevision.Data`. When marshaled to JSON, this becomes a JSON Array (`[...]`). Kubernetes `runtime.RawExtension` validation requires the data to be a JSON Object (`{...}`).

## Solution
We modify `CreateControllerRevision` in `pkg/model-serving-controller/utils/controller_revision.go` to wrap the template data in a JSON object structure before marshaling.

The data is wrapped as:
```json
{
  "data": [ ... roles array ... ]
}
```

We also update `GetRolesFromControllerRevision` to properly unwrap this structure when retrieving the roles.

## Impact
This change ensures that `ControllerRevision` resources are successfully created and accepted by the Kubernetes API server, allowing rolling updates and recovery mechanisms to function correctly.
