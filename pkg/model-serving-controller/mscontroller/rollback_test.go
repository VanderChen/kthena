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

package mscontroller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	schedulingv1beta1 "volcano.sh/apis/pkg/apis/scheduling/v1beta1"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/apitest"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/plugins"
)

// podOwners returns, by Pod name, the UID of the Pod and the kind of its controller.
func (s *system) podOwners() (uids map[string]types.UID, kinds map[string]string) {
	s.t.Helper()
	uids, kinds = map[string]types.UID{}, map[string]string{}
	for _, pod := range s.pods() {
		uids[pod.Name] = pod.UID
		if owner := metav1.GetControllerOf(pod); owner != nil {
			kinds[pod.Name] = owner.Kind
		}
	}
	return uids, kinds
}

// The rollback switch hands every workload back to its ModelServing in the
// shape a version without group objects expects, without touching a Pod; and
// a rollback that is abandoned is undone the same way.
func TestRollbackHandsTheWorkloadsBack(t *testing.T) {
	s := newSystem(t, modelServing(rollout(func(config *workloadv1alpha1.RollingUpdateConfiguration) {
		config.MaxUnavailable, config.MaxSurge = ptr.To(intstr.FromInt32(0)), ptr.To(intstrOne)
	})))
	s.settle()

	// Stop in the middle of a rollout with surge: a temporary group exists.
	s.stuck = func(pod *corev1.Pod) bool { return pod.Spec.Containers[0].Image == "model:v2" }
	s.update(setImage("model:v2"))
	s.settle()
	if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1 3=model:v2+"; got != want {
		t.Fatalf("before the rollback: %s, want %s", got, want)
	}
	// A PodGroup and a Service that the surge group controls.
	surgeGroup := s.groups()[3]
	groupOwner := []metav1.OwnerReference{{
		APIVersion: workloadv1alpha1.SchemeGroupVersion.String(), Kind: "ServingGroup", Name: surgeGroup.Name, UID: surgeGroup.UID,
		Controller: ptr.To(true), BlockOwnerDeletion: ptr.To(true),
	}}
	groupLabels := map[string]string{workloadv1alpha1.ModelServingNameLabelKey: testName, workloadv1alpha1.GroupNameLabelKey: surgeGroup.Name}
	if _, err := s.cluster.Volcano.SchedulingV1beta1().PodGroups(testNamespace).Create(context.Background(), &schedulingv1beta1.PodGroup{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: surgeGroup.Name, Labels: groupLabels, OwnerReferences: groupOwner},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.cluster.Kube.CoreV1().Services(testNamespace).Create(context.Background(), &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Namespace: testNamespace, Name: surgeGroup.Name + "-serve-0-0", Labels: groupLabels, OwnerReferences: groupOwner},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	before, _ := s.podOwners()
	status := s.modelServing().Status
	s.cluster.ResetWrites()

	s.rollback.Store(true)
	s.settle()

	if groups := s.groups(); len(groups) != 0 {
		t.Fatalf("%d ServingGroup objects are left after the rollback", len(groups))
	}
	after, kinds := s.podOwners()
	if len(after) != len(before) {
		t.Fatalf("%d Pods after the rollback, %d before", len(after), len(before))
	}
	for name, uid := range after {
		if uid != before[name] || kinds[name] != "ModelServing" {
			t.Errorf("Pod %s: same Pod = %t, controlled by a %s; want the same Pod, controlled by the ModelServing", name, uid == before[name], kinds[name])
		}
	}
	for _, write := range s.cluster.Writes() {
		if write.Resource == "pods" && write.Verb != "patch" {
			t.Errorf("the rollback issued: %s", write)
		}
	}
	// An earlier version tells the temporary group by the mark on its Pods.
	for _, pod := range s.pods() {
		marked := pod.Annotations[workloadv1alpha1.SurgeAnnotationKey] == workloadv1alpha1.SurgeScopeServingGroup
		if inSurge := pod.Labels[workloadv1alpha1.GroupNameLabelKey] == surgeGroup.Name; marked != inSurge {
			t.Errorf("Pod %s: surge mark = %t, want %t", pod.Name, marked, inSurge)
		}
	}
	ms := s.modelServing()
	podGroup, err := s.cluster.GetPodGroup(testNamespace, surgeGroup.Name)
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(podGroup); owner == nil || owner.UID != ms.UID || owner.BlockOwnerDeletion != nil {
		t.Errorf("PodGroup controller = %+v, want the ModelServing, as an earlier version sets it", owner)
	}
	service, err := s.cluster.GetService(testNamespace, surgeGroup.Name+"-serve-0-0")
	if err != nil {
		t.Fatal(err)
	}
	if owner := metav1.GetControllerOf(service); owner == nil || owner.UID != ms.UID {
		t.Errorf("Service controller = %+v, want the ModelServing", owner)
	}
	if _, ok := ms.Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; ok {
		t.Errorf("the adoption annotation is left: %v", ms.Annotations)
	}
	if ms.Status.Replicas != status.Replicas || ms.Status.CurrentRevision != status.CurrentRevision || ms.Status.UpdateRevision != status.UpdateRevision {
		t.Errorf("status after the rollback = %+v, before %+v", ms.Status, status)
	}
	// In rollback mode nothing is reconciled: a change is left for the earlier version.
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](5) })
	s.cluster.ResetWrites()
	s.settle()
	if writes := s.cluster.Writes(); len(writes) != 0 {
		t.Errorf("in rollback mode a change of the ModelServing caused %v", writes)
	}
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](3) })

	// The rollback is abandoned: everything is taken over again, the surge
	// group as a surge group, and the rollout goes on.
	s.rollback.Store(false)
	s.cluster.ResetWrites()
	for range 10 {
		s.kubelet()
		s.cluster.Settle()
		s.pass()
		if s.modelServing().Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey] == workloadv1alpha1.ServingGroupAdoptionDone {
			break
		}
	}
	if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1 3=model:v2+"; got != want {
		t.Fatalf("taken over again: %s, want %s", got, want)
	}
	again, kinds := s.podOwners()
	for name, uid := range again {
		if uid != before[name] || kinds[name] != "ServingGroup" {
			t.Errorf("Pod %s after the rollback was abandoned: same Pod = %t, controlled by a %s", name, uid == before[name], kinds[name])
		}
	}
	s.stuck = nil
	s.settle()
	if got, want := s.summary(), "groups[0=model:v2 1=model:v2 2=model:v2] replicas=3 updated=3 current=3 available=3 Available"; got != want {
		t.Fatalf("after the rollout:\n got %s\nwant %s", got, want)
	}
}

// A rollback does not hand back half a group: it waits for a group that is
// being deleted, and for one that is interrupted the next round continues.
func TestRollbackWaitsAndResumes(t *testing.T) {
	s := newSystem(t, modelServing())
	s.settle()
	s.cluster.HoldPodTermination(true)
	s.update(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) })
	s.settle()
	if got, want := s.images(), "0=model:v1 1=model:v1 2=model:v1!"; got != want {
		t.Fatalf("groups = %s, want %s", got, want)
	}

	s.rollback.Store(true)
	s.cluster.ResetWrites()
	caches, snapshot := s.caches()
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	after, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
	if err != nil || after <= 0 || len(s.cluster.Writes()) != 0 {
		t.Fatalf("round with a group in deletion = %v, %v, writes %v; want it to wait and write nothing", after, err, s.cluster.Writes())
	}

	s.cluster.HoldPodTermination(false)
	for _, pod := range s.pods() {
		if pod.DeletionTimestamp != nil {
			_ = s.cluster.FinishTermination(testNamespace, pod.Name)
		}
	}
	s.cluster.Settle()

	// The process dies after every write of the rollback in turn; the next
	// one finishes it.
	total := 0
	for stopAt := 1; ; stopAt++ {
		t2 := newSystem(t, modelServing(func(ms *workloadv1alpha1.ModelServing) { ms.Spec.Replicas = ptr.To[int32](2) }))
		t2.settle()
		before, _ := t2.podOwners()
		t2.rollback.Store(true)
		writes, died := 0, false
		t2.cluster.BeforeWrite = func(apitestWrite) error {
			writes++
			if writes == stopAt {
				died = true
			}
			if died {
				return errDied
			}
			return nil
		}
		caches, snapshot := t2.caches()
		t2.ms.SetCaches(caches, snapshot.Listers().Services)
		_, _ = t2.ms.Reconcile(context.Background(), testNamespace, testName)
		t2.cluster.BeforeWrite = nil
		if !died {
			total = stopAt - 1
			break
		}
		t2.restart()
		t2.settle()
		after, kinds := t2.podOwners()
		for name, uid := range after {
			if uid != before[name] || kinds[name] != "ModelServing" {
				t.Errorf("stop before write %d: Pod %s: same Pod = %t, controlled by a %s", stopAt, name, uid == before[name], kinds[name])
			}
		}
		if len(t2.groups()) != 0 || len(after) != len(before) {
			t.Errorf("stop before write %d: %d groups and %d Pods are left, want 0 and %d", stopAt, len(t2.groups()), len(after), len(before))
		}
	}
	if total < 6 {
		t.Errorf("the rollback of two groups made %d writes; the test does not exercise it", total)
	}
}

// gatePlugin stops the creation of a Pod until it is released.
type gatePlugin struct {
	armed            bool
	entered, release chan struct{}
}

func (p *gatePlugin) Name() string { return "gate" }
func (p *gatePlugin) OnModelServingSync(context.Context, *plugins.HookRequest) plugins.HookResult {
	return plugins.HookResult{}
}
func (p *gatePlugin) OnPodCreate(context.Context, *plugins.HookRequest) error {
	if p.armed {
		p.armed = false
		close(p.entered)
		<-p.release
	}
	return nil
}
func (p *gatePlugin) OnPodReady(context.Context, *plugins.HookRequest) error   { return nil }
func (p *gatePlugin) OnRoleSync(context.Context, *plugins.HookRequest) error   { return nil }
func (p *gatePlugin) OnRoleDelete(context.Context, *plugins.HookRequest) error { return nil }

// The rollback switch is set while a round of a group is creating a Pod. The
// hand-back waits for that round: a Pod created for a group after the group
// was found empty would lose its owner with the group, or be collected.
func TestRollbackWaitsForARoundInFlight(t *testing.T) {
	gate := &gatePlugin{entered: make(chan struct{}), release: make(chan struct{})}
	s := &system{t: t, registry: plugins.NewRegistry()}
	s.registry.Register("gate", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return gate, nil })
	s.cluster = apitest.New(modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "gate", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	}))
	s.restart()
	s.settle()

	// A member is lost; the round that creates it again is held in the
	// middle, after it has seen that the switch is not set.
	lost := s.pods()[0]
	if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), lost.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.cluster.Settle()
	group := lost.Labels[workloadv1alpha1.GroupNameLabelKey]
	caches, snapshot := s.caches()
	s.sg.SetCaches(caches, snapshot.Listers().Services)
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	gate.armed = true
	roundDone := make(chan error, 1)
	go func() {
		_, err := s.sg.Reconcile(context.Background(), testNamespace, group)
		roundDone <- err
	}()
	<-gate.entered

	s.rollback.Store(true)
	handedBack := make(chan error, 1)
	go func() {
		_, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
		handedBack <- err
	}()
	select {
	case err := <-handedBack:
		t.Fatalf("the rollback went ahead while a round was in flight (error: %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	if _, kinds := s.podOwners(); len(s.groups()) != 3 || kinds[s.pods()[0].Name] != "ServingGroup" {
		t.Fatalf("something was handed back while a round was in flight: %d groups, owners %v", len(s.groups()), kinds)
	}

	close(gate.release)
	if err := <-roundDone; err != nil {
		t.Fatalf("the round in flight: %v", err)
	}
	if err := <-handedBack; err != nil {
		t.Fatalf("the rollback: %v", err)
	}
	s.settle()

	if groups := s.groups(); len(groups) != 0 {
		t.Errorf("%d ServingGroup objects are left after the rollback", len(groups))
	}
	uids, kinds := s.podOwners()
	if len(uids) != 6 {
		t.Errorf("%d Pods after the rollback, want the six of three groups: %v", len(uids), kinds)
	}
	for name := range uids {
		if kinds[name] != "ModelServing" {
			t.Errorf("Pod %s is controlled by %q after the rollback", name, kinds[name])
		}
	}
}

// The rollback switch is removed while the hand-back of a ModelServing waits
// for a round of one of its groups. The rollback is abandoned: nothing is
// handed back by a round that only saw the switch before it waited. The rounds
// that the removal of the switch lets go on would otherwise adopt and create
// beside the hand-back.
func TestRollbackIsAbandonedWhenTheSwitchIsRemovedWhileItWaits(t *testing.T) {
	gate := &gatePlugin{entered: make(chan struct{}), release: make(chan struct{})}
	s := &system{t: t, registry: plugins.NewRegistry()}
	s.registry.Register("gate", func(workloadv1alpha1.PluginSpec) (plugins.Plugin, error) { return gate, nil })
	s.cluster = apitest.New(modelServing(func(ms *workloadv1alpha1.ModelServing) {
		ms.Spec.RecoveryPolicy = workloadv1alpha1.NoneRestartPolicy
		ms.Spec.Plugins = []workloadv1alpha1.PluginSpec{{Name: "gate", Type: workloadv1alpha1.PluginTypeBuiltIn}}
	}))
	s.restart()
	s.settle()

	lost := s.pods()[0]
	if err := s.cluster.Kube.CoreV1().Pods(testNamespace).Delete(context.Background(), lost.Name, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	s.cluster.Settle()
	group := lost.Labels[workloadv1alpha1.GroupNameLabelKey]
	caches, snapshot := s.caches()
	s.sg.SetCaches(caches, snapshot.Listers().Services)
	s.ms.SetCaches(caches, snapshot.Listers().Services)
	gate.armed = true
	roundDone := make(chan error, 1)
	go func() {
		_, err := s.sg.Reconcile(context.Background(), testNamespace, group)
		roundDone <- err
	}()
	<-gate.entered

	s.rollback.Store(true)
	handedBack := make(chan error, 1)
	go func() {
		_, err := s.ms.Reconcile(context.Background(), testNamespace, testName)
		handedBack <- err
	}()
	select {
	case err := <-handedBack:
		t.Fatalf("the rollback went ahead while a round was in flight (error: %v)", err)
	case <-time.After(200 * time.Millisecond):
	}
	// The operator removes the switch; then the round in flight ends.
	s.rollback.Store(false)
	close(gate.release)
	if err := <-roundDone; err != nil {
		t.Fatalf("the round in flight: %v", err)
	}
	if err := <-handedBack; err != nil {
		t.Fatalf("the round that waited: %v", err)
	}

	if state := s.modelServing().Annotations[workloadv1alpha1.ServingGroupAdoptionAnnotationKey]; state == workloadv1alpha1.ServingGroupAdoptionRollingBack {
		t.Errorf("the ModelServing is marked as being rolled back")
	}
	uids, kinds := s.podOwners()
	if len(s.groups()) != 3 || len(uids) != 6 {
		t.Fatalf("%d groups and %d Pods, want three groups with their six Pods", len(s.groups()), len(uids))
	}
	for name := range uids {
		if kinds[name] != "ServingGroup" {
			t.Errorf("Pod %s is controlled by %q, want its group", name, kinds[name])
		}
	}
}
