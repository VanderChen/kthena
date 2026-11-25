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

package servinggroup

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
)

var testNow = time.Unix(1700001000, 0)

func notReady(pod *corev1.Pod) {
	pod.Status.Conditions[0].Status = corev1.ConditionFalse
}

func restarted(pod *corev1.Pod) {
	notReady(pod)
	pod.Status.ContainerStatuses[0].RestartCount = 2
}

func failed(pod *corev1.Pod) {
	notReady(pod)
	pod.Status.Phase = corev1.PodFailed
}

// inPlace marks the Pod as patched by an in-place update that expects one
// restart of its container and has not seen the new container yet.
func inPlace(pod *corev1.Pod) {
	notReady(pod)
	pod.Spec.Containers[0].Image = "model:v2"
	pod.Annotations = map[string]string{workloadv1alpha1.InPlaceUpdateStateAnnotationKey: `{"revision":"rev-b","startedAt":"2023-11-14T22:13:20Z",` +
		`"patched":{"model":{"image":"model:v2","prevContainerID":"containerd://a","prevImageID":"sha256:1"}},"expected":{"model":1}}`}
}

func TestClassifyMember(t *testing.T) {
	enabled := Recovery{Enabled: true, Grace: 60 * time.Second}
	immediate := Recovery{Enabled: true}
	disabled := Recovery{Enabled: false, Grace: 60 * time.Second}

	tests := []struct {
		name      string
		pod       *corev1.Pod
		recovery  Recovery
		seenFor   time.Duration // how long ago the tracker first saw the fault; negative: never
		want      MemberState
		wantRetry time.Duration // from now; zero: none
	}{
		{name: "ready", pod: memberPod("prefill", 0, 0), recovery: enabled, seenFor: -1, want: MemberReady},
		{name: "ready with earlier restarts", pod: memberPod("prefill", 0, 0, func(p *corev1.Pod) { p.Status.ContainerStatuses[0].RestartCount = 3 }), recovery: immediate, seenFor: -1, want: MemberReady},
		{name: "terminating wins over everything", pod: memberPod("prefill", 0, 0, failed, terminating), recovery: immediate, seenFor: -1, want: MemberTerminating},
		{name: "pending without a restart", pod: memberPod("prefill", 0, 0, notReady, func(p *corev1.Pod) { p.Status.Phase = corev1.PodPending }), recovery: immediate, seenFor: -1, want: MemberNotReady},
		{name: "succeeded is not a fault", pod: memberPod("prefill", 0, 0, notReady, func(p *corev1.Pod) { p.Status.Phase = corev1.PodSucceeded }), recovery: immediate, seenFor: -1, want: MemberNotReady},
		{name: "restarting, first seen now", pod: memberPod("prefill", 0, 0, restarted), recovery: enabled, seenFor: -1, want: MemberFailing, wantRetry: 60 * time.Second},
		{name: "restarting within the grace period", pod: memberPod("prefill", 0, 0, restarted), recovery: enabled, seenFor: 20 * time.Second, want: MemberFailing, wantRetry: 40 * time.Second},
		{name: "restarting past the grace period", pod: memberPod("prefill", 0, 0, restarted), recovery: enabled, seenFor: 60 * time.Second, want: MemberFaulty},
		{name: "restarting with no grace period", pod: memberPod("prefill", 0, 0, restarted), recovery: immediate, seenFor: -1, want: MemberFaulty},
		{name: "init container restarted", pod: memberPod("prefill", 0, 0, notReady, func(p *corev1.Pod) {
			p.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "init", RestartCount: 1}}
		}), recovery: immediate, seenFor: -1, want: MemberFaulty},
		{name: "failed waits for the grace period too", pod: memberPod("prefill", 0, 0, failed), recovery: enabled, seenFor: 10 * time.Second, want: MemberFailing, wantRetry: 50 * time.Second},
		{name: "failed past the grace period", pod: memberPod("prefill", 0, 0, failed), recovery: enabled, seenFor: 61 * time.Second, want: MemberFaulty},
		{name: "recovery disabled tolerates a restart", pod: memberPod("prefill", 0, 0, restarted), recovery: disabled, seenFor: 600 * time.Second, want: MemberFailing},
		{name: "recovery disabled tolerates a failed Pod", pod: memberPod("prefill", 0, 0, failed), recovery: disabled, seenFor: 600 * time.Second, want: MemberFailing},
		{name: "in-place update in progress", pod: memberPod("prefill", 0, 0, inPlace), recovery: immediate, seenFor: -1, want: MemberUpdating},
		{name: "in-place update: the expected restart is no fault", pod: memberPod("prefill", 0, 0, inPlace, func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "model", RestartCount: 1, ContainerID: "containerd://b", Image: "model:v2", ImageID: "sha256:2"}
		}), recovery: immediate, seenFor: -1, want: MemberUpdating},
		{name: "in-place update: a second restart is a fault", pod: memberPod("prefill", 0, 0, inPlace, func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0] = corev1.ContainerStatus{Name: "model", RestartCount: 2, ContainerID: "containerd://c", Image: "model:v2", ImageID: "sha256:2"}
		}), recovery: immediate, seenFor: -1, want: MemberFaulty},
		{name: "in-place update: a failed Pod is a fault", pod: memberPod("prefill", 0, 0, inPlace, failed), recovery: immediate, seenFor: -1, want: MemberFaulty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			faults := NewFaultTracker()
			if tt.seenFor >= 0 {
				faults.Since(tt.pod, testNow.Add(-tt.seenFor))
			}
			got, retry := ClassifyMember(tt.pod, MemberContext{Now: testNow, Recovery: tt.recovery, Faults: faults})
			if got != tt.want {
				t.Errorf("state = %s, want %s", got, tt.want)
			}
			wantRetry := time.Time{}
			if tt.wantRetry > 0 {
				wantRetry = testNow.Add(tt.wantRetry)
			}
			if !retry.Equal(wantRetry) {
				t.Errorf("retry = %v, want %v", retry, wantRetry)
			}
		})
	}
}

func TestClassifyMemberReadsOnlyTheProjection(t *testing.T) {
	// The restart count is read; a field outside the projection must not matter.
	pod := memberPod("prefill", 0, 0, restarted, func(p *corev1.Pod) {
		p.Status.Reason = "Evicted"
		p.Status.ContainerStatuses[0].State.Waiting = &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}
	})
	plain := memberPod("prefill", 0, 0, restarted)
	ctx := MemberContext{Now: testNow, Recovery: Recovery{Enabled: true}}
	a, _ := ClassifyMember(pod, ctx)
	b, _ := ClassifyMember(plain, ctx)
	if a != b || Digest([]*corev1.Pod{pod}) != Digest([]*corev1.Pod{plain}) {
		t.Errorf("two Pods with the same projection are read differently: %s and %s", a, b)
	}
}

func TestHasFault(t *testing.T) {
	for state, want := range map[MemberState]bool{
		MemberFaulty: true, MemberFailing: true,
		MemberTerminating: false, MemberUpdating: false, MemberReady: false, MemberNotReady: false,
	} {
		if got := state.HasFault(); got != want {
			t.Errorf("%s.HasFault() = %t, want %t", state, got, want)
		}
	}
}

func TestRecoveryOf(t *testing.T) {
	tests := []struct {
		policy workloadv1alpha1.RecoveryPolicy
		grace  *int64
		want   Recovery
	}{
		{workloadv1alpha1.RoleRecreate, nil, Recovery{Enabled: true}},
		{workloadv1alpha1.RoleRecreate, ptr.To[int64](0), Recovery{Enabled: true}},
		{workloadv1alpha1.ServingGroupRecreate, ptr.To[int64](30), Recovery{Enabled: true, Grace: 30 * time.Second}},
		{workloadv1alpha1.RoleRecreate, ptr.To[int64](-1), Recovery{}},
		{workloadv1alpha1.NoneRestartPolicy, ptr.To[int64](30), Recovery{Grace: 30 * time.Second}},
	}
	for _, tt := range tests {
		if got := RecoveryOf(tt.policy, tt.grace); got != tt.want {
			t.Errorf("RecoveryOf(%s, %v) = %+v, want %+v", tt.policy, tt.grace, got, tt.want)
		}
	}
}

func TestCautiousRecovery(t *testing.T) {
	group := Recovery{Enabled: true, Grace: 30 * time.Second}
	tests := []struct {
		name   string
		parent Recovery
		want   Recovery
	}{
		{"same", group, group},
		{"parent disabled", Recovery{}, Recovery{Grace: 30 * time.Second}},
		{"parent longer", Recovery{Enabled: true, Grace: 90 * time.Second}, Recovery{Enabled: true, Grace: 90 * time.Second}},
		{"parent shorter does not take effect yet", Recovery{Enabled: true, Grace: 5 * time.Second}, group},
	}
	for _, tt := range tests {
		if got := group.Cautious(tt.parent); got != tt.want {
			t.Errorf("%s: Cautious() = %+v, want %+v", tt.name, got, tt.want)
		}
	}
	if got := (Recovery{}).Cautious(group); got.Enabled {
		t.Error("a group with recovery disabled must not be enabled by its parent")
	}
}

func TestFaultTracker(t *testing.T) {
	faults := NewFaultTracker()
	pod := memberPod("prefill", 0, 0, restarted)
	start := testNow.Add(-time.Minute)

	if got := faults.Since(pod, start); !got.Equal(start) {
		t.Fatalf("first sight = %v, want %v", got, start)
	}
	if got := faults.Since(pod, testNow); !got.Equal(start) {
		t.Errorf("same fault seen again = %v, want the first sight %v", got, start)
	}

	recoveredAndFailedAgain := memberPod("prefill", 0, 0, restarted, func(p *corev1.Pod) {
		p.Status.Conditions[0].LastTransitionTime = metav1.NewTime(testNow)
	})
	if got := faults.Since(recoveredAndFailedAgain, testNow); !got.Equal(testNow) {
		t.Errorf("a new period of failure = %v, want to start now", got)
	}
	if got := faults.Since(pod, testNow.Add(time.Second)); got.Equal(start) {
		t.Error("the earlier period was not dropped when a new one started")
	}

	replaced := memberPod("prefill", 0, 0, restarted, func(p *corev1.Pod) { p.UID = "new-uid" })
	if got := faults.Since(replaced, testNow); !got.Equal(testNow) {
		t.Errorf("a Pod with a new UID = %v, want to start now", got)
	}

	faults.Forget(replaced)
	later := testNow.Add(time.Hour)
	if got := faults.Since(replaced, later); !got.Equal(later) {
		t.Errorf("after Forget = %v, want %v", got, later)
	}

	mine := func(uid types.UID) bool { return uid == replaced.UID || uid == pod.UID }
	faults.Prune(mine, sets.New(pod.UID))
	if got := faults.Since(replaced, later.Add(time.Hour)); got.Equal(later) {
		t.Error("Prune kept a Pod that is gone")
	}

	var none *FaultTracker
	if got := none.Since(pod, testNow); !got.Equal(testNow) {
		t.Errorf("nil tracker = %v, want now", got)
	}
	none.Forget(pod)
	none.Prune(mine, nil)
}
