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

package grouplevel

// live: neither terminating nor failed.
func (g Group) live() bool {
	return !g.Terminating && !g.Failed
}

// confirmedReady is what Ready, NotReadyTarget, the floor of G3b and G6 count.
func (g Group) confirmedReady() bool {
	return g.live() && g.Observed && g.Fresh && g.Ready
}

// confirmedNotReady is what G3b reclaims without asking the floor.
func (g Group) confirmedNotReady() bool {
	return g.live() && g.Observed && g.Fresh && !g.Ready
}

// memberless asks neither an observed nor a fresh status: an object whose
// status was never written is memberless.
func (g Group) memberless() bool {
	return g.live() && g.Memberless
}

// replaceClass is the class of a replaceable group in G3c.
type replaceClass int

const (
	// replaceUnsettled: not observed or not fresh; no candidate.
	replaceUnsettled replaceClass = iota
	// replaceNotReady: confirmed not Ready.
	replaceNotReady
	// replaceHealthy: confirmed Ready.
	replaceHealthy
)

func (g Group) replaceClass() replaceClass {
	switch {
	case !g.Observed || !g.Fresh:
		return replaceUnsettled
	case g.Ready:
		return replaceHealthy
	default:
		return replaceNotReady
	}
}

// isSurge: the surge mark only counts under ServingGroupRollingUpdate; under
// RoleRollingUpdate every group is regular.
func isSurge(spec Spec, g Group) bool {
	return spec.Mode == ModeServingGroup && g.Surge
}

// isReplaceable: live, ServingGroupRollingUpdate, has members, and either
// RecreateRequired or an old revision at an ordinal that is not protected.
// "Has members" governs both; nothing below P is replaceable while the
// baseline cannot be read, because it could not be created again. A group
// whose revision cannot be compared with the target is not an old revision:
// it stays as it is until its history can be read.
func isReplaceable(spec Spec, g Group) bool {
	if !g.live() || spec.Mode != ModeServingGroup || g.memberless() {
		return false
	}
	if spec.Baseline == "" && g.Ordinal < spec.Partition {
		return false
	}
	return g.RecreateRequired || (g.Revision != spec.Target && g.Ordinal >= spec.Partition && !g.RevisionUnknown)
}

// placeRevision is the revision of a group created at this ordinal now: B
// below P when B differs from T, else T. ok is false when B is needed and
// cannot be read.
func placeRevision(spec Spec, ordinal int) (rev string, ok bool) {
	if spec.Mode == ModeServingGroup && ordinal < spec.Partition && spec.Baseline != spec.Target {
		return spec.Baseline, spec.Baseline != ""
	}
	return spec.Target, true
}

func classOf(spec Spec, g Group) Class {
	switch {
	case g.Terminating:
		return ClassTerminating
	case g.Failed:
		return ClassFailed
	case isReplaceable(spec, g):
		return ClassReplaceable
	case g.memberless():
		return ClassMemberless
	case spec.Mode == ModeServingGroup && g.Revision != spec.Target:
		return ClassProtected
	default:
		return ClassTarget
	}
}

// Classify classifies groups and counts them. awaiting holds the ordinals
// that await adoption (the ordinals with ownerless member Pods): such a group
// occupies its place but is not in RegularLive. The ordinals of groups must
// be distinct.
func Classify(spec Spec, groups []Group, awaiting map[int]bool) View {
	v := View{
		Classes: make(map[int]Class, len(groups)),
		Active:  len(groups),
		Floor:   max(0, spec.Replicas-spec.MaxUnavailable),
	}
	for _, g := range groups {
		class := classOf(spec, g)
		v.Classes[g.Ordinal] = class
		surge := isSurge(spec, g)
		if !surge {
			v.Regular++
		}
		if !g.live() {
			continue
		}
		v.Live++
		switch {
		case g.confirmedReady():
			v.Ready++
		case g.Revision == spec.Target && class != ClassReplaceable:
			v.NotReadyTarget++
		}
		if surge {
			v.Surge++
			continue
		}
		if !awaiting[g.Ordinal] {
			v.RegularLive++
		}
		if class == ClassReplaceable {
			v.Work = true
		}
	}
	v.MaxScaleDown = max(0, v.Live-v.Floor-v.NotReadyTarget)
	v.MaxHealthyScaleDown = max(0, v.Ready-v.Floor)
	v.ScalingDown = v.Regular > spec.Replicas
	return v
}
