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
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

const testNamespace = "ns"

// errBoom is the failure tests inject through Cluster.BeforeWrite.
var errBoom = errors.New("apitest: injected failure")

func newPod(name string, owners ...metav1.OwnerReference) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			OwnerReferences: owners,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "busybox"}},
		},
	}
}

func newService(name string, owners ...metav1.OwnerReference) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			OwnerReferences: owners,
		},
		Spec: corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
	}
}

func newPodGroup(name string, owners ...metav1.OwnerReference) *schedulingv1beta1.PodGroup {
	return &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			OwnerReferences: owners,
		},
		Spec: schedulingv1beta1.PodGroupSpec{MinMember: 1},
	}
}

func newModelServing(name string) *workloadv1alpha1.ModelServing {
	return &workloadv1alpha1.ModelServing{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: workloadv1alpha1.ModelServingSpec{
			Replicas:      ptr.To(int32(1)),
			SchedulerName: "volcano",
		},
	}
}

func newServingGroup(name string, owners ...metav1.OwnerReference) *workloadv1alpha1.ServingGroup {
	return &workloadv1alpha1.ServingGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       testNamespace,
			OwnerReferences: owners,
		},
		Spec: workloadv1alpha1.ServingGroupSpec{Revision: "rev-1"},
	}
}

// ownerRef builds an owner reference to an object already stored in the
// cluster.
func ownerRef(apiVersion, kind, name string, uid types.UID, block bool) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion:         apiVersion,
		Kind:               kind,
		Name:               name,
		UID:                uid,
		Controller:         ptr.To(true),
		BlockOwnerDeletion: ptr.To(block),
	}
}

func servingGroupOwner(sg *workloadv1alpha1.ServingGroup, block bool) metav1.OwnerReference {
	return ownerRef(workloadv1alpha1.SchemeGroupVersion.String(), "ServingGroup", sg.Name, sg.UID, block)
}

func mustGetPod(t *testing.T, c *Cluster, name string) *corev1.Pod {
	t.Helper()
	pod, err := c.GetPod(testNamespace, name)
	if err != nil {
		t.Fatalf("GetPod(%q): %v", name, err)
	}
	return pod
}

func TestCreateStampsMetadata(t *testing.T) {
	c := New()
	ctx := context.Background()

	created, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1"), metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.UID == "" {
		t.Error("created pod has no UID")
	}
	if created.ResourceVersion == "" {
		t.Error("created pod has no resourceVersion")
	}
	if created.CreationTimestamp.Time != DefaultStartTime {
		t.Errorf("creationTimestamp = %v, want %v", created.CreationTimestamp.Time, DefaultStartTime)
	}
	if created.Generation != 1 {
		t.Errorf("generation = %d, want 1", created.Generation)
	}

	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1"), metav1.CreateOptions{}); !apierrors.IsAlreadyExists(err) {
		t.Fatalf("second create: got %v, want AlreadyExists", err)
	}
}

func TestCreateKeepsProvidedUIDAndGeneratesName(t *testing.T) {
	c := New()
	ctx := context.Background()

	pod := newPod("")
	pod.GenerateName = "p-"
	pod.UID = "fixed-uid"
	created, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.UID != "fixed-uid" {
		t.Errorf("uid = %q, want the provided one", created.UID)
	}
	if created.Name == "" || created.Name == "p-" {
		t.Errorf("name = %q, want a generated name", created.Name)
	}
	if _, err := c.GetPod(testNamespace, created.Name); err != nil {
		t.Errorf("stored under %q: %v", created.Name, err)
	}
}

func TestUpdateResourceVersionPreconditions(t *testing.T) {
	sg := newServingGroup("sg")
	tests := []struct {
		name            string
		resourceVersion func(stored *workloadv1alpha1.ServingGroup) string
		uid             func(stored *workloadv1alpha1.ServingGroup) types.UID
		wantConflict    bool
	}{
		{
			name:            "current resourceVersion is accepted",
			resourceVersion: func(stored *workloadv1alpha1.ServingGroup) string { return stored.ResourceVersion },
			uid:             func(stored *workloadv1alpha1.ServingGroup) types.UID { return stored.UID },
		},
		{
			name:            "empty resourceVersion is unconditional",
			resourceVersion: func(*workloadv1alpha1.ServingGroup) string { return "" },
			uid:             func(*workloadv1alpha1.ServingGroup) types.UID { return "" },
		},
		{
			name:            "stale resourceVersion conflicts",
			resourceVersion: func(*workloadv1alpha1.ServingGroup) string { return "1" },
			uid:             func(stored *workloadv1alpha1.ServingGroup) types.UID { return stored.UID },
			wantConflict:    true,
		},
		{
			name:            "wrong uid conflicts",
			resourceVersion: func(stored *workloadv1alpha1.ServingGroup) string { return stored.ResourceVersion },
			uid:             func(*workloadv1alpha1.ServingGroup) types.UID { return "other" },
			wantConflict:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(sg.DeepCopy())
			ctx := context.Background()
			stored, err := c.GetServingGroup(testNamespace, "sg")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			// Make the stored resourceVersion differ from "1".
			stored.Spec.TargetRevision = "rev-2"
			stored, err = c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(ctx, stored, metav1.UpdateOptions{})
			if err != nil {
				t.Fatalf("priming update: %v", err)
			}

			attempt := stored.DeepCopy()
			attempt.ResourceVersion = tt.resourceVersion(stored)
			attempt.UID = tt.uid(stored)
			attempt.Spec.Revision = "rev-3"
			_, err = c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).Update(ctx, attempt, metav1.UpdateOptions{})
			if tt.wantConflict {
				if !apierrors.IsConflict(err) {
					t.Fatalf("update: got %v, want Conflict", err)
				}
				after, err := c.GetServingGroup(testNamespace, "sg")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				if after.Spec.Revision != "rev-1" {
					t.Errorf("rejected update changed the object: revision = %q", after.Spec.Revision)
				}
				return
			}
			if err != nil {
				t.Fatalf("update: %v", err)
			}
			after, err := c.GetServingGroup(testNamespace, "sg")
			if err != nil {
				t.Fatalf("get: %v", err)
			}
			if after.Spec.Revision != "rev-3" {
				t.Errorf("revision = %q, want rev-3", after.Spec.Revision)
			}
			if after.ResourceVersion == stored.ResourceVersion {
				t.Error("resourceVersion was not bumped")
			}
		})
	}
}

func TestStatusSubresourceIsSeparate(t *testing.T) {
	c := New(newServingGroup("sg"))
	ctx := context.Background()
	groups := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace)

	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	sg.Spec.Revision = "rev-2"
	sg.Status.ObservedGeneration = 42
	if _, err := groups.Update(ctx, sg, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update: %v", err)
	}
	stored, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Spec.Revision != "rev-2" {
		t.Errorf("spec was not written: revision = %q", stored.Spec.Revision)
	}
	if stored.Status.ObservedGeneration != 0 {
		t.Errorf("update through the main resource changed status: observedGeneration = %d", stored.Status.ObservedGeneration)
	}
	if stored.Generation != 2 {
		t.Errorf("generation = %d, want 2 after a spec change", stored.Generation)
	}

	stored.Spec.Revision = "rev-3"
	stored.Status.ObservedGeneration = 7
	if _, err := groups.UpdateStatus(ctx, stored, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	stored, err = c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.Status.ObservedGeneration != 7 {
		t.Errorf("status was not written: observedGeneration = %d", stored.Status.ObservedGeneration)
	}
	if stored.Spec.Revision != "rev-2" {
		t.Errorf("update through status changed spec: revision = %q", stored.Spec.Revision)
	}
	if stored.Generation != 2 {
		t.Errorf("generation = %d, want it unchanged by a status write", stored.Generation)
	}
}

func TestGenerationFollowsSpecOnly(t *testing.T) {
	c := New(newServingGroup("sg"), newPod("p1"))
	ctx := context.Background()
	groups := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace)

	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	sg.Labels = map[string]string{"a": "b"}
	updated, err := groups.Update(ctx, sg, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Generation != 1 {
		t.Errorf("generation = %d, want it unchanged by a metadata-only update", updated.Generation)
	}
	if updated.ResourceVersion == sg.ResourceVersion {
		t.Error("resourceVersion was not bumped by a metadata-only update")
	}

	updated.Spec.Revision = "rev-9"
	updated, err = groups.Update(ctx, updated, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.Generation != 2 {
		t.Errorf("generation = %d, want 2 after a spec change", updated.Generation)
	}

	// Pods keep the generation the API server gave them.
	pod := mustGetPod(t, c, "p1")
	pod.Spec.ActiveDeadlineSeconds = ptr.To(int64(5))
	pod, err = c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update pod: %v", err)
	}
	if pod.Generation != 1 {
		t.Errorf("pod generation = %d, want it unchanged", pod.Generation)
	}
}

func TestResourceVersionIsMonotonicAcrossClientsets(t *testing.T) {
	c := New(newPod("p1"), newServingGroup("sg"), newPodGroup("pg"))
	ctx := context.Background()

	var versions []uint64
	record := func(rv string) {
		t.Helper()
		parsed, err := strconv.ParseUint(rv, 10, 64)
		if err != nil {
			t.Fatalf("resourceVersion %q is not a decimal number: %v", rv, err)
		}
		versions = append(versions, parsed)
	}

	pod := mustGetPod(t, c, "p1")
	pod.Labels = map[string]string{"k": "v"}
	pod, err := c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update pod: %v", err)
	}
	record(pod.ResourceVersion)

	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get serving group: %v", err)
	}
	sg.Status.ObservedGeneration = 1
	sg, err = c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).UpdateStatus(ctx, sg, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update serving group status: %v", err)
	}
	record(sg.ResourceVersion)

	pg, err := c.GetPodGroup(testNamespace, "pg")
	if err != nil {
		t.Fatalf("get pod group: %v", err)
	}
	pg.Spec.MinMember = 2
	pg, err = c.Volcano.SchedulingV1beta1().PodGroups(testNamespace).Update(ctx, pg, metav1.UpdateOptions{})
	if err != nil {
		t.Fatalf("update pod group: %v", err)
	}
	record(pg.ResourceVersion)

	for i := 1; i < len(versions); i++ {
		if versions[i] <= versions[i-1] {
			t.Fatalf("resource versions are not monotonic: %v", versions)
		}
	}
}

func TestWriteLog(t *testing.T) {
	c := New(newServingGroup("sg"))
	ctx := context.Background()

	if _, err := c.Kube.CoreV1().Pods(testNamespace).Create(ctx, newPod("p1"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	sg, err := c.GetServingGroup(testNamespace, "sg")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	sg.Status.ObservedGeneration = 3
	if _, err := c.Kthena.WorkloadV1alpha1().ServingGroups(testNamespace).UpdateStatus(ctx, sg, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("update status: %v", err)
	}
	if err := c.Kube.CoreV1().Pods(testNamespace).Delete(ctx, "p1", metav1.DeleteOptions{}); err != nil {
		t.Fatalf("delete pod: %v", err)
	}
	if _, err := c.Kube.CoreV1().Events(testNamespace).Create(ctx, &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{Name: "e1", Namespace: testNamespace},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create event: %v", err)
	}

	want := []Write{
		{Verb: verbCreate, Resource: "pods", Namespace: testNamespace, Name: "p1"},
		{Verb: verbUpdate, Resource: "servinggroups", Subresource: statusSubresource, Namespace: testNamespace, Name: "sg"},
		{Verb: verbDelete, Resource: "pods", Namespace: testNamespace, Name: "p1"},
	}
	got := c.Writes()
	if len(got) != len(want) {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("write %d = %v, want %v", i, got[i], want[i])
		}
	}

	c.ResetWrites()
	if got := c.Writes(); len(got) != 0 {
		t.Errorf("writes after reset = %v, want none", got)
	}
}

func TestBeforeWriteAbortsTheWrite(t *testing.T) {
	c := New(newPod("p1"))
	ctx := context.Background()

	boom := apierrors.NewInternalError(errBoom)
	var seen []Write
	c.BeforeWrite = func(w Write) error {
		seen = append(seen, w)
		if w.Verb == verbUpdate {
			return boom
		}
		return nil
	}

	pod := mustGetPod(t, c, "p1")
	before := pod.DeepCopy()
	pod.Labels = map[string]string{"k": "v"}
	if _, err := c.Kube.CoreV1().Pods(testNamespace).Update(ctx, pod, metav1.UpdateOptions{}); err == nil {
		t.Fatal("update: got no error, want the injected one")
	}

	after := mustGetPod(t, c, "p1")
	if len(after.Labels) != 0 {
		t.Errorf("labels = %v, want the write to have been dropped", after.Labels)
	}
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("resourceVersion = %q, want it unchanged at %q", after.ResourceVersion, before.ResourceVersion)
	}
	if got := c.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want the rejected write to be unlogged", got)
	}
	if len(seen) != 1 || seen[0].Verb != verbUpdate {
		t.Errorf("hook saw %v, want one update", seen)
	}
}

func TestBeforeWriteCanStopAfterWriteK(t *testing.T) {
	c := New(newPod("p1"), newPod("p2"), newPod("p3"))
	ctx := context.Background()

	c.BeforeWrite = func(Write) error {
		if len(c.Writes()) >= 2 {
			return apierrors.NewInternalError(errBoom)
		}
		return nil
	}
	for _, name := range []string{"p1", "p2", "p3"} {
		err := c.Kube.CoreV1().Pods(testNamespace).Delete(ctx, name, metav1.DeleteOptions{})
		if name == "p3" && err == nil {
			t.Fatal("third delete: got no error, want it blocked")
		}
		if name != "p3" && err != nil {
			t.Fatalf("delete %s: %v", name, err)
		}
	}
	if _, err := c.GetPod(testNamespace, "p3"); err != nil {
		t.Errorf("p3: %v, want it still there", err)
	}
	if got := len(c.Writes()); got != 2 {
		t.Errorf("writes = %d, want 2", got)
	}
}

func TestStatusSubresourceAcrossKinds(t *testing.T) {
	tests := []struct {
		name string
		// mutate changes both spec and status of the stored object and writes
		// it through the main resource and through status; it returns what the
		// store holds after each of the two writes.
		run func(t *testing.T, c *Cluster) (afterUpdate, afterStatus struct{ spec, status string })
	}{
		{
			name: "pods",
			run: func(t *testing.T, c *Cluster) (afterUpdate, afterStatus struct{ spec, status string }) {
				ctx := context.Background()
				pods := c.Kube.CoreV1().Pods(testNamespace)
				pod := mustGetPod(t, c, "p1")
				pod.Spec.Subdomain = "spec-write"
				pod.Status.Message = "status-write"
				if _, err := pods.Update(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update: %v", err)
				}
				pod = mustGetPod(t, c, "p1")
				afterUpdate.spec, afterUpdate.status = pod.Spec.Subdomain, pod.Status.Message

				pod.Spec.Subdomain = "ignored"
				pod.Status.Message = "status-write"
				if _, err := pods.UpdateStatus(ctx, pod, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update status: %v", err)
				}
				pod = mustGetPod(t, c, "p1")
				afterStatus.spec, afterStatus.status = pod.Spec.Subdomain, pod.Status.Message
				return afterUpdate, afterStatus
			},
		},
		{
			name: "modelservings",
			run: func(t *testing.T, c *Cluster) (afterUpdate, afterStatus struct{ spec, status string }) {
				ctx := context.Background()
				servings := c.Kthena.WorkloadV1alpha1().ModelServings(testNamespace)
				ms, err := c.GetModelServing(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				ms.Spec.SchedulerName = "spec-write"
				ms.Status.CurrentRevision = "status-write"
				if _, err := servings.Update(ctx, ms, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update: %v", err)
				}
				ms, err = c.GetModelServing(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				afterUpdate.spec, afterUpdate.status = ms.Spec.SchedulerName, ms.Status.CurrentRevision

				ms.Spec.SchedulerName = "ignored"
				ms.Status.CurrentRevision = "status-write"
				if _, err := servings.UpdateStatus(ctx, ms, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update status: %v", err)
				}
				ms, err = c.GetModelServing(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				afterStatus.spec, afterStatus.status = ms.Spec.SchedulerName, ms.Status.CurrentRevision
				return afterUpdate, afterStatus
			},
		},
		{
			name: "podgroups",
			run: func(t *testing.T, c *Cluster) (afterUpdate, afterStatus struct{ spec, status string }) {
				ctx := context.Background()
				groups := c.Volcano.SchedulingV1beta1().PodGroups(testNamespace)
				pg, err := c.GetPodGroup(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				pg.Spec.Queue = "spec-write"
				pg.Status.Phase = "status-write"
				if _, err := groups.Update(ctx, pg, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update: %v", err)
				}
				pg, err = c.GetPodGroup(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				afterUpdate.spec, afterUpdate.status = pg.Spec.Queue, string(pg.Status.Phase)

				pg.Spec.Queue = "ignored"
				pg.Status.Phase = "status-write"
				if _, err := groups.UpdateStatus(ctx, pg, metav1.UpdateOptions{}); err != nil {
					t.Fatalf("update status: %v", err)
				}
				pg, err = c.GetPodGroup(testNamespace, "p1")
				if err != nil {
					t.Fatalf("get: %v", err)
				}
				afterStatus.spec, afterStatus.status = pg.Spec.Queue, string(pg.Status.Phase)
				return afterUpdate, afterStatus
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := New(newPod("p1"), newModelServing("p1"), newPodGroup("p1"))
			afterUpdate, afterStatus := tt.run(t, c)
			if afterUpdate.spec != "spec-write" {
				t.Errorf("after update, spec = %q, want it written", afterUpdate.spec)
			}
			if afterUpdate.status != "" {
				t.Errorf("after update, status = %q, want it untouched", afterUpdate.status)
			}
			if afterStatus.spec != "spec-write" {
				t.Errorf("after a status write, spec = %q, want it untouched", afterStatus.spec)
			}
			if afterStatus.status != "status-write" {
				t.Errorf("after a status write, status = %q, want it written", afterStatus.status)
			}
		})
	}
}

func TestUpdateOfMissingObjectIsNotFound(t *testing.T) {
	c := New()
	_, err := c.Kube.CoreV1().Pods(testNamespace).Update(context.Background(), newPod("p1"), metav1.UpdateOptions{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("update: got %v, want NotFound", err)
	}
	if got := c.Writes(); len(got) != 0 {
		t.Errorf("writes = %v, want none", got)
	}
}

func TestReadsAndWatchesStillWork(t *testing.T) {
	c := New(newPod("p1"))
	ctx := context.Background()
	pods := c.Kube.CoreV1().Pods(testNamespace)

	watcher, err := pods.Watch(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	defer watcher.Stop()

	if _, err := pods.Get(ctx, "p1", metav1.GetOptions{}); err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := pods.Create(ctx, newPod("p2"), metav1.CreateOptions{}); err != nil {
		t.Fatalf("create: %v", err)
	}
	list, err := pods.List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Items) != 2 {
		t.Errorf("list = %d pods, want 2", len(list.Items))
	}

	select {
	case event := <-watcher.ResultChan():
		if event.Type != watch.Added {
			t.Errorf("event = %v, want Added", event.Type)
		}
	case <-time.After(5 * time.Second):
		t.Error("no watch event for the created pod")
	}
}
