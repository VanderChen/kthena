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
	"reflect"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

// family identifies the fake clientset that serves a resource.
type family int

const (
	familyKube family = iota
	familyKthena
	familyVolcano
)

// resourceInfo describes one API resource that the Cluster models exactly:
// its preconditions, subresource layout and garbage collection behaviour.
type resourceInfo struct {
	gvr schema.GroupVersionResource
	gvk schema.GroupVersionKind
	fam family
	// hasStatus is true when the resource serves a status subresource, i.e.
	// when status and the rest of the object are written separately.
	hasStatus bool
	// specGeneration is true when metadata.generation is bumped whenever spec
	// changes. Built-in workload objects such as Pods keep their generation.
	specGeneration bool
}

func (r *resourceInfo) groupResource() schema.GroupResource {
	return r.gvr.GroupResource()
}

var (
	podsGVR = corev1.SchemeGroupVersion.WithResource("pods")

	// modelledResources are the resources with full precondition, status and
	// garbage collection support. They are listed in a fixed order so that
	// garbage collection passes and snapshots are deterministic.
	modelledResources = []*resourceInfo{
		{
			gvr:       podsGVR,
			gvk:       corev1.SchemeGroupVersion.WithKind("Pod"),
			fam:       familyKube,
			hasStatus: true,
		},
		{
			gvr:       corev1.SchemeGroupVersion.WithResource("services"),
			gvk:       corev1.SchemeGroupVersion.WithKind("Service"),
			fam:       familyKube,
			hasStatus: true,
		},
		{
			gvr: corev1.SchemeGroupVersion.WithResource("configmaps"),
			gvk: corev1.SchemeGroupVersion.WithKind("ConfigMap"),
			fam: familyKube,
		},
		{
			gvr: appsv1.SchemeGroupVersion.WithResource("controllerrevisions"),
			gvk: appsv1.SchemeGroupVersion.WithKind("ControllerRevision"),
			fam: familyKube,
		},
		{
			gvr:            workloadv1alpha1.SchemeGroupVersion.WithResource("modelservings"),
			gvk:            workloadv1alpha1.ModelServingKind,
			fam:            familyKthena,
			hasStatus:      true,
			specGeneration: true,
		},
		{
			gvr:            workloadv1alpha1.SchemeGroupVersion.WithResource("servinggroups"),
			gvk:            workloadv1alpha1.ServingGroupKind,
			fam:            familyKthena,
			hasStatus:      true,
			specGeneration: true,
		},
		{
			gvr:            schedulingv1beta1.SchemeGroupVersion.WithResource("podgroups"),
			gvk:            schedulingv1beta1.SchemeGroupVersion.WithKind("PodGroup"),
			fam:            familyVolcano,
			hasStatus:      true,
			specGeneration: true,
		},
	}

	resourceByGVR = func() map[schema.GroupVersionResource]*resourceInfo {
		m := make(map[schema.GroupVersionResource]*resourceInfo, len(modelledResources))
		for _, r := range modelledResources {
			m[r.gvr] = r
		}
		return m
	}()

	resourceByGVK = func() map[schema.GroupVersionKind]*resourceInfo {
		m := make(map[schema.GroupVersionKind]*resourceInfo, len(modelledResources))
		for _, r := range modelledResources {
			m[r.gvk] = r
		}
		return m
	}()
)

// resourceFor returns the description of the requested resource. Resources
// that are not modelled explicitly still get preconditions, metadata stamping
// and write logging; their status subresource is inferred from the Go type and
// their generation is never bumped.
func resourceFor(fam family, gvr schema.GroupVersionResource, sample runtime.Object) *resourceInfo {
	if r, ok := resourceByGVR[gvr]; ok {
		return r
	}
	return &resourceInfo{
		gvr:       gvr,
		fam:       fam,
		hasStatus: sample != nil && structField(sample, "Status").IsValid(),
	}
}

// structField returns the named field of a pointer-to-struct API object, or
// the zero Value when the object does not have it.
func structField(obj runtime.Object, name string) reflect.Value {
	v := reflect.ValueOf(obj)
	if v.Kind() != reflect.Pointer || v.IsNil() {
		return reflect.Value{}
	}
	v = v.Elem()
	if v.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	f := v.FieldByName(name)
	if !f.IsValid() || !f.CanSet() {
		return reflect.Value{}
	}
	return f
}

// copyField copies the named field from src to dst. Both must be of the same
// Go type; objects without the field are left untouched.
func copyField(dst, src runtime.Object, name string) {
	dstField := structField(dst, name)
	srcField := structField(src, name)
	if !dstField.IsValid() || !srcField.IsValid() || dstField.Type() != srcField.Type() {
		return
	}
	dstField.Set(srcField)
}
