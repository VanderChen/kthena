/*
Copyright The Volcano Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package apitest

import (
	"fmt"
	"net/http"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	k8stesting "k8s.io/client-go/testing"
)

func (c *Cluster) handlePatch(fam family, action k8stesting.PatchActionImpl) (bool, runtime.Object, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	gvr := action.GetResource()
	namespace := action.GetNamespace()
	name := action.GetName()

	stored, err := c.trackerFor(fam).Get(gvr, namespace, name)
	if err != nil {
		return true, nil, err
	}
	res := resourceFor(fam, gvr, stored)
	storedMeta, err := meta.Accessor(stored)
	if err != nil {
		return true, nil, err
	}

	patch := action.GetPatch()
	switch action.GetPatchType() {
	case types.MergePatchType:
	case types.StrategicMergePatchType:
		if res.fam != familyKube {
			// Custom resources do not serve strategic merge patches.
			return true, nil, apierrors.NewGenericServerResponse(http.StatusUnsupportedMediaType, verbPatch,
				res.groupResource(), name,
				"the body of the request was in an unknown format - accepted media types include: application/json-patch+json, application/merge-patch+json, application/apply-patch+yaml",
				0, false)
		}
	default:
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("apitest: patch type %s is not supported", action.GetPatchType()))
	}

	patchMap := map[string]any{}
	if err := json.Unmarshal(patch, &patchMap); err != nil {
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("apitest: invalid patch: %v", err))
	}
	uid, resourceVersion, err := takeMetadataPreconditions(patchMap)
	if err != nil {
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("apitest: invalid patch: %v", err))
	}
	if err := checkOptimisticConcurrency(res, storedMeta, uid, resourceVersion); err != nil {
		return true, nil, err
	}

	patched, err := applyPatch(stored, action.GetPatchType(), patchMap)
	if err != nil {
		return true, nil, apierrors.NewBadRequest(fmt.Sprintf("apitest: cannot apply patch: %v", err))
	}
	out, err := c.applyWriteLocked(res, verbPatch, action.GetSubresource(), namespace, stored, patched)
	if err != nil {
		return true, nil, err
	}
	return true, out, nil
}

// takeMetadataPreconditions removes metadata.uid and metadata.resourceVersion
// from a patch body and returns them; the API server treats them as write
// preconditions rather than as values to merge.
func takeMetadataPreconditions(patch map[string]any) (types.UID, string, error) {
	raw, ok := patch["metadata"]
	if !ok || raw == nil {
		return "", "", nil
	}
	metadata, ok := raw.(map[string]any)
	if !ok {
		return "", "", fmt.Errorf("metadata is %T, expected an object", raw)
	}

	var uid types.UID
	if v, ok := metadata["uid"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", "", fmt.Errorf("metadata.uid is %T, expected a string", v)
		}
		uid = types.UID(s)
		delete(metadata, "uid")
	}
	resourceVersion := ""
	if v, ok := metadata["resourceVersion"]; ok {
		s, ok := v.(string)
		if !ok {
			return "", "", fmt.Errorf("metadata.resourceVersion is %T, expected a string", v)
		}
		resourceVersion = s
		delete(metadata, "resourceVersion")
	}
	if len(metadata) == 0 {
		delete(patch, "metadata")
	}
	return uid, resourceVersion, nil
}

// applyPatch returns a new object of the stored object's type with the patch
// applied. The caller has already stripped the write preconditions.
func applyPatch(stored runtime.Object, patchType types.PatchType, patchMap map[string]any) (runtime.Object, error) {
	original, err := json.Marshal(stored)
	if err != nil {
		return nil, err
	}
	patch, err := json.Marshal(patchMap)
	if err != nil {
		return nil, err
	}

	var patched []byte
	switch patchType {
	case types.StrategicMergePatchType:
		patched, err = strategicpatch.StrategicMergePatch(original, patch, stored)
		if err != nil {
			return nil, err
		}
	default:
		var originalMap map[string]any
		if err := json.Unmarshal(original, &originalMap); err != nil {
			return nil, err
		}
		patched, err = json.Marshal(mergePatchObject(originalMap, patchMap))
		if err != nil {
			return nil, err
		}
	}

	out := reflect.New(reflect.TypeOf(stored).Elem()).Interface().(runtime.Object)
	if err := json.Unmarshal(patched, out); err != nil {
		return nil, err
	}
	return out, nil
}

// mergePatchObject applies an RFC 7386 JSON merge patch to original and
// returns the result.
func mergePatchObject(original, patch map[string]any) map[string]any {
	if original == nil {
		original = map[string]any{}
	}
	for key, value := range patch {
		if value == nil {
			delete(original, key)
			continue
		}
		if patchChild, ok := value.(map[string]any); ok {
			originalChild, _ := original[key].(map[string]any)
			original[key] = mergePatchObject(originalChild, patchChild)
			continue
		}
		original[key] = value
	}
	return original
}
