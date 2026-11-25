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
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"

	workloadv1alpha1 "github.com/volcano-sh/kthena/pkg/apis/workload/v1alpha1"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/inplace"
	"github.com/volcano-sh/kthena/pkg/model-serving-controller/utils"
)

// MemberState is the state of a member Pod that the group owns.
type MemberState string

const (
	// MemberTerminating: the Pod is being deleted.
	MemberTerminating MemberState = "Terminating"
	// MemberFaulty: the Pod has failed, or keeps restarting and is not ready, and
	// the grace period has passed. The fault triggers the recovery policy.
	MemberFaulty MemberState = "Faulty"
	// MemberFailing: the Pod has failed or keeps restarting and is not ready, and
	// recovery is disabled or the grace period has not passed. The member is not
	// ready. It triggers nothing, but once its scope is being recovered or removed
	// for another reason it is deleted first, like a faulty member (see HasFault).
	MemberFailing MemberState = "Failing"
	// MemberUpdating: an in-place update of the Pod is in progress. Its containers
	// restart on purpose; the member is not ready and is not faulty.
	MemberUpdating MemberState = "Updating"
	// MemberReady: the Pod is running and ready.
	MemberReady MemberState = "Ready"
	// MemberNotReady: none of the above.
	MemberNotReady MemberState = "NotReady"
)

// Recovery is the recovery configuration that applies to a member.
type Recovery struct {
	// Enabled is false when recoveryPolicy is None or the grace period is -1: a
	// failed or restarting Pod is then never deleted because of the fault.
	Enabled bool
	// Grace is how long a fault is tolerated before it counts.
	Grace time.Duration
}

// RecoveryOf returns the recovery configuration given by a policy and a grace
// period in seconds, as they appear on a ModelServing and on a ServingGroup.
func RecoveryOf(policy workloadv1alpha1.RecoveryPolicy, graceSeconds *int64) Recovery {
	recovery := Recovery{Enabled: policy != workloadv1alpha1.NoneRestartPolicy}
	if graceSeconds != nil {
		if *graceSeconds < 0 {
			recovery.Enabled = false
		} else {
			recovery.Grace = time.Duration(*graceSeconds) * time.Second
		}
	}
	return recovery
}

// Cautious combines the recovery configuration a group carries in its spec
// with the one of its ModelServing: recovery is enabled only when both enable
// it, and the longer grace period applies. A change of the ModelServing that
// makes recovery more reluctant thus takes effect before it reaches the group.
func (r Recovery) Cautious(other Recovery) Recovery {
	return Recovery{Enabled: r.Enabled && other.Enabled, Grace: max(r.Grace, other.Grace)}
}

// HasFault reports whether the Pod of a member has failed or keeps restarting,
// whether or not the fault counts yet. It depends on the Pod alone, so the
// order in which the members of a scope are deleted, faulty ones first, is the
// same for every process that continues the deletion.
func (s MemberState) HasFault() bool {
	return s == MemberFaulty || s == MemberFailing
}

// MemberContext is what the state of a member depends on besides the Pod.
type MemberContext struct {
	Now      time.Time
	Recovery Recovery
	// Faults remembers since when this process has seen a fault.
	Faults *FaultTracker
	// Overdue holds the UIDs of the Pods that keep an in-place update from
	// finishing after its progress deadline (see CheckInPlaceDeadlines). While
	// recovery is enabled such a Pod has a fault.
	Overdue sets.Set[types.UID]
}

// ClassifyMember returns the state of a member Pod owned by the group. For a
// MemberFailing member whose fault will count later it also returns that time.
//
// The state is decided from the projection of the Pod (see Project) and from
// nothing else of it, so it cannot change without Digest changing.
func ClassifyMember(pod *corev1.Pod, ctx MemberContext) (MemberState, time.Time) {
	pod = Project(pod)
	if pod.DeletionTimestamp != nil {
		return MemberTerminating, time.Time{}
	}
	state := inplace.StateOf(pod)
	fault := utils.IsPodFailed(pod) || state.UnexpectedRestart(pod)
	overdue := ctx.Recovery.Enabled && ctx.Overdue.Has(pod.UID)
	switch {
	case overdue && !state.InProgress():
		// The Pod was never patched while the rest of its unit was, and the
		// update cannot finish without it. The Pod is healthy: a grace period
		// would not change anything about it.
		return MemberFaulty, time.Time{}
	case overdue:
		// An update that did not finish in time is handled like a Pod that
		// does not come up.
	case state.InProgress() && fault:
		// A crash during an in-place update is not one of its expected restarts.
	case state.Updating(pod):
		return MemberUpdating, time.Time{}
	case utils.IsPodRunningAndReady(pod):
		return MemberReady, time.Time{}
	case !fault:
		return MemberNotReady, time.Time{}
	}

	if !ctx.Recovery.Enabled {
		ctx.Faults.Forget(pod)
		return MemberFailing, time.Time{}
	}
	expires := ctx.Faults.Since(pod, ctx.Now).Add(ctx.Recovery.Grace)
	if expires.After(ctx.Now) {
		return MemberFailing, expires
	}
	return MemberFaulty, time.Time{}
}

// FaultTracker remembers when this process first saw each failing member. It
// is the one input of a decision that is not on an API object: a new process
// starts every grace period again. It only delays the moment a recovery is
// committed and never widens it.
type FaultTracker struct {
	mu     sync.Mutex
	starts map[faultKey]time.Time
}

// faultKey identifies one period of failure of a Pod. A Pod that became ready
// in between, or was patched by an in-place update, starts a new period.
type faultKey struct {
	uid        types.UID
	readySince int64
	inPlace    string
}

func keyOf(pod *corev1.Pod) faultKey {
	key := faultKey{uid: pod.UID, inPlace: pod.Annotations[workloadv1alpha1.InPlaceUpdateStateAnnotationKey]}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			key.readySince = condition.LastTransitionTime.Unix()
		}
	}
	return key
}

// NewFaultTracker returns an empty FaultTracker.
func NewFaultTracker() *FaultTracker {
	return &FaultTracker{starts: map[faultKey]time.Time{}}
}

// Since returns when the current fault of pod was first seen, and records now
// when it was not seen before. A nil tracker has seen nothing before.
func (t *FaultTracker) Since(pod *corev1.Pod, now time.Time) time.Time {
	if t == nil {
		return now
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	key := keyOf(pod)
	if start, ok := t.starts[key]; ok {
		return start
	}
	// Earlier periods of the same Pod are over.
	for other := range t.starts {
		if other.uid == pod.UID {
			delete(t.starts, other)
		}
	}
	t.starts[key] = now
	return now
}

// Forget drops what is remembered about pod. A fault that is tolerated for a
// while is timed from the moment it is no longer tolerated.
func (t *FaultTracker) Forget(pod *corev1.Pod) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.starts {
		if key.uid == pod.UID {
			delete(t.starts, key)
		}
	}
}

// Prune drops what is remembered about Pods of scope that are not in live.
// scope names the Pods a caller is responsible for, so that several callers
// can share one tracker.
func (t *FaultTracker) Prune(scope func(types.UID) bool, live sets.Set[types.UID]) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for key := range t.starts {
		if scope(key.uid) && !live.Has(key.uid) {
			delete(t.starts, key)
		}
	}
}
