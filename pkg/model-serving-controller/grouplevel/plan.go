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

import (
	"cmp"
	"slices"
)

// planner holds the working copy of one round. Every chosen write is applied
// to it before the next choice: a deleted group becomes terminating, a created
// group is a new group that is not Ready, a promoted group loses the mark.
type planner struct {
	spec Spec
	// work is the working copy, by ordinal.
	work map[int]Group
	// awaiting holds the ordinals that carry ownerless member Pods. A group
	// there occupies its place and is no candidate of G2, G3b and G3c.
	awaiting map[int]bool
	foreign  map[int]bool
	acts     []Action
	// seen and count are the view and the tally of the working copy as it
	// is now; put drops them.
	seen  *View
	count *tally
}

// Plan returns the writes of one round in the order they are issued. It is a
// pure function: it does not modify its input, and the order of in.Groups and
// in.Foreign does not matter. The ordinals of in.Groups must be distinct.
//
// A write is computed on the assumption that the writes before it succeeded:
// when one fails, the caller must not issue the rest of the round. ActionWait
// entries write nothing; they report why a place is not filled or a group is
// not replaced.
func Plan(in Input) []Action {
	p := newPlanner(in)
	p.run(in.Orphans)
	return p.acts
}

func newPlanner(in Input) *planner {
	p := &planner{
		spec:     in.Spec,
		work:     make(map[int]Group, len(in.Groups)),
		awaiting: make(map[int]bool, len(in.Orphans)),
		foreign:  make(map[int]bool, len(in.Foreign)),
	}
	groups := slices.Clone(in.Groups)
	slices.SortFunc(groups, func(a, b Group) int {
		return cmp.Or(cmp.Compare(a.Ordinal, b.Ordinal), cmp.Compare(a.UID, b.UID))
	})
	for _, g := range groups {
		p.put(g)
	}
	for ordinal := range in.Orphans {
		p.awaiting[ordinal] = true
	}
	for _, ordinal := range in.Foreign {
		p.foreign[ordinal] = true
	}
	return p
}

// run is one round: the steps in the order they are issued.
func (p *planner) run(orphans map[int]Orphans) {
	sg := p.spec.Mode == ModeServingGroup

	p.recoverOrphans(orphans) // S1′
	p.promote()               // G0
	p.deleteFailed()          // G1
	p.scaleDown()             // G2
	// Neither surge nor replacement while regular > N: a group that G2 deleted
	// counts until it is gone.
	scaling := p.view().ScalingDown
	p.dropMemberless() // G2′(a)
	if sg {
		p.retargetMemberless() // G2′(b)
	}
	p.fill() // G4
	if p.regulars() < p.spec.Replicas {
		p.wait(-1, ReasonOrdinalOccupied)
	}
	if sg {
		p.createSurge(scaling)   // G3a
		p.reclaimSurge()         // G3b
		p.replaceGroups(scaling) // G3c
	}
	p.advanceBaseline() // G6
}

// sorted returns the groups of w by ordinal.
func sorted(w map[int]Group) []Group {
	groups := make([]Group, 0, len(w))
	for _, g := range w {
		groups = append(groups, g)
	}
	slices.SortFunc(groups, func(a, b Group) int { return cmp.Compare(a.Ordinal, b.Ordinal) })
	return groups
}

// descending returns the groups of w, highest ordinal first.
func descending(w map[int]Group) []Group {
	groups := sorted(w)
	slices.Reverse(groups)
	return groups
}

// podsNotReadyFirst is the head of the orders of G2 and G3b. It goes by the
// Pod cache alone, whatever the status says: it is a preference and is
// compared with no count.
func podsNotReadyFirst(a, b Group) int {
	switch {
	case a.PodsReady == b.PodsReady:
		return 0
	case a.PodsReady:
		return 1
	default:
		return -1
	}
}

// scaleDownOrder: Pods not ready, then the lower deletion cost, then the higher ordinal.
func scaleDownOrder(a, b Group) int {
	return cmp.Or(podsNotReadyFirst(a, b), cmp.Compare(a.DeletionCost, b.DeletionCost), cmp.Compare(b.Ordinal, a.Ordinal))
}

// reclaimOrder: Pods not ready, then the higher ordinal.
func reclaimOrder(a, b Group) int {
	return cmp.Or(podsNotReadyFirst(a, b), cmp.Compare(b.Ordinal, a.Ordinal))
}

// put writes g into the working copy.
func (p *planner) put(g Group) {
	p.work[g.Ordinal] = g
	p.seen, p.count = nil, nil
}

// view classifies and counts the working copy; it is taken again after every write.
func (p *planner) view() View {
	if p.seen == nil {
		groups := make([]Group, 0, len(p.work))
		for _, g := range p.work {
			groups = append(groups, g)
		}
		v := Classify(p.spec, groups, p.awaiting)
		p.seen = &v
	}
	return *p.seen
}

// taken: an object has the name, whoever owns it. Pods left on an ordinal do
// not make it taken.
func (p *planner) taken(ordinal int) bool {
	_, ok := p.work[ordinal]
	return ok || p.foreign[ordinal]
}

// lowestFree is the lowest free ordinal below N that is not below from.
func (p *planner) lowestFree(from int) (ordinal int, ok bool) {
	for o := from; o < p.spec.Replicas; o++ {
		if !p.taken(o) {
			return o, true
		}
	}
	return 0, false
}

// regulars counts the groups without the mark, terminating and failed ones included.
func (p *planner) regulars() int {
	return p.view().Regular
}

func (p *planner) delete(g Group, why string) {
	p.acts = append(p.acts, Action{Kind: ActionDelete, Ordinal: g.Ordinal, UID: g.UID, Reason: why})
	g.Terminating, g.Ready = true, false
	p.put(g)
}

// create adds a new group that is not Ready and has no Pod. A recovered
// group is not observed until its Pods are adopted.
func (p *planner) create(ordinal int, rev string, surge bool, why string) {
	p.acts = append(p.acts, Action{Kind: ActionCreate, Ordinal: ordinal, Revision: rev, Surge: surge, Reason: why})
	p.put(Group{Ordinal: ordinal, Revision: rev, Surge: surge, Observed: why != ReasonRecover, Fresh: true})
}

// wait reports why a place is not filled, or why a deletion that would lose a
// group is not issued. ReasonBaselineUnknown names the ordinal that needs B,
// ReasonOrdinalOccupied the foreign objects below N; anything else passes by
// itself and has nothing to name.
func (p *planner) wait(ordinal int, why string) {
	var named []int
	switch why {
	case ReasonBaselineUnknown:
		named = []int{ordinal}
	case ReasonOrdinalOccupied:
		for o := range p.foreign {
			if o < p.spec.Replicas {
				named = append(named, o)
			}
		}
		slices.Sort(named)
	}
	for _, o := range named {
		a := Action{Kind: ActionWait, Ordinal: o, Reason: why}
		if !slices.Contains(p.acts, a) {
			p.acts = append(p.acts, a)
		}
	}
}

// recoverOrphans is S1′: an ordinal that carries ownerless member Pods and has
// no object of the name gets an object, before everything else and whatever N
// is. Its revision is the one of the Pods when it can be read, else the one of
// the ordinal. It carries the mark when the Pods carry the copy of it and the
// ordinal is not below N.
func (p *planner) recoverOrphans(orphans map[int]Orphans) {
	ordinals := make([]int, 0, len(orphans))
	for ordinal := range orphans {
		ordinals = append(ordinals, ordinal)
	}
	slices.Sort(ordinals)
	for _, ordinal := range ordinals {
		if p.taken(ordinal) {
			continue
		}
		rev := orphans[ordinal].Revision
		if rev == "" {
			var ok bool
			if rev, ok = placeRevision(p.spec, ordinal); !ok {
				p.wait(ordinal, ReasonBaselineUnknown)
				continue
			}
		}
		surge := p.spec.Mode == ModeServingGroup && ordinal >= p.spec.Replicas && orphans[ordinal].Marked
		p.create(ordinal, rev, surge, ReasonRecover)
	}
}

// promote is G0. ServingGroupRollingUpdate: a marked group whose ordinal is
// below N loses the mark while regular < N, lowest ordinal first, a group in
// any state. RoleRollingUpdate: every marked group loses the mark.
func (p *planner) promote() {
	sg := p.spec.Mode == ModeServingGroup
	for _, g := range sorted(p.work) {
		if !g.Surge {
			continue
		}
		if sg && (g.Ordinal >= p.spec.Replicas || p.regulars() >= p.spec.Replicas) {
			continue
		}
		p.acts = append(p.acts, Action{Kind: ActionPromote, Ordinal: g.Ordinal, UID: g.UID})
		g.Surge = false
		p.put(g)
	}
}

// deleteFailed is G1: a Failed group is deleted once it is Drained. Failed is
// irreversible: it is deleted without judging again.
func (p *planner) deleteFailed() {
	for _, g := range sorted(p.work) {
		if g.Failed && !g.Terminating && g.Drained {
			p.delete(g, ReasonFailed)
		}
	}
}

// scaleDown is G2: while regularLive > N, one group at a time. Not charged to
// the rollout budget. A group that awaits adoption is neither counted nor
// chosen. The candidates are exactly the groups regularLive counts and their
// order does not change as they go, so they are sorted once.
func (p *planner) scaleDown() {
	var candidates []Group
	for _, g := range p.work {
		if g.live() && !isSurge(p.spec, g) && !p.awaiting[g.Ordinal] {
			candidates = append(candidates, g)
		}
	}
	slices.SortFunc(candidates, scaleDownOrder)
	for i, g := range candidates {
		if regularLive := len(candidates) - i; regularLive <= p.spec.Replicas {
			return
		}
		p.delete(g, ReasonScaleDown)
	}
}

// outOfRange: a regular memberless group whose ordinal is not below N.
func (p *planner) outOfRange(g Group) bool {
	return g.memberless() && !isSurge(p.spec, g) && g.Ordinal >= p.spec.Replicas
}

// tidy tells whether G2′(a) may delete a group now. It is tidying up and asks
// for the most. When what is missing is the free ordinal or B, at and why say
// what to report.
func (p *planner) tidy() (ok bool, at int, why string) {
	n, s := p.spec.Replicas, p.spec.MaxSurge
	// 1. No other deletion is under way, of this round or of an earlier one ...
	for _, g := range p.work {
		if g.Terminating || g.Failed {
			return false, -1, ""
		}
	}
	// ... nor one that is done while the place it left is not filled yet.
	regular := p.regulars()
	if regular < n {
		return false, -1, ""
	}
	// 2. With the group gone G4 can create a regular group now: a regular
	// place is open, the bound N + S leaves room (here it counts) ...
	if regular-1 >= n || len(p.work)-1 >= n+s {
		return false, -1, ""
	}
	// ... and the lowest free ordinal below N exists and can be created.
	free, found := p.lowestFree(0)
	if !found {
		return false, -1, ReasonOrdinalOccupied
	}
	if _, known := placeRevision(p.spec, free); !known {
		return false, free, ReasonBaselineUnknown
	}
	return true, -1, ""
}

// dropMemberless is G2′(a): a regular memberless group whose ordinal is not
// below N is deleted, at most one in a round, no budget used; G4 fills the
// hole below N afterwards. Otherwise it stays and its revision is not touched.
// A marked memberless group is not looked at here.
func (p *planner) dropMemberless() {
	for _, g := range descending(p.work) {
		if !p.outOfRange(g) {
			continue
		}
		if ok, at, why := p.tidy(); !ok {
			p.wait(at, why)
			continue
		}
		why := ReasonMemberlessOutOfRange
		if g.Empty {
			why = ReasonEmptyOutOfRange
		}
		p.delete(g, why)
	}
}

// retargetMemberless is G2′(b), ServingGroupRollingUpdate only: any other
// memberless group gets the revision a group created at its ordinal would get
// now; not while B is needed and cannot be read. A group of G2′(a) is left
// alone: it is deleted, or it waits until its place below N can be filled.
func (p *planner) retargetMemberless() {
	for _, g := range sorted(p.work) {
		if !g.memberless() || p.outOfRange(g) {
			continue
		}
		want, known := placeRevision(p.spec, g.Ordinal)
		if !known {
			p.wait(g.Ordinal, ReasonBaselineUnknown)
			continue
		}
		if g.Revision != want {
			p.acts = append(p.acts, Action{Kind: ActionRetarget, Ordinal: g.Ordinal, UID: g.UID, Revision: want, Reason: ReasonMemberless})
			g.Revision = want
			p.put(g)
		}
	}
}

// fill is G4: while regular < N and active < N + S, one group at a time at
// the lowest free ordinal below N. When B is needed and cannot be read nothing
// is created, at that ordinal or at a higher one.
func (p *planner) fill() {
	n, s := p.spec.Replicas, p.spec.MaxSurge
	next := 0
	for regular := p.regulars(); regular < n && len(p.work) < n+s; regular++ {
		free, found := p.lowestFree(next)
		if !found {
			return
		}
		rev, known := placeRevision(p.spec, free)
		if !known {
			p.wait(free, ReasonBaselineUnknown)
			return
		}
		p.create(free, rev, false, ReasonRegular)
		next = free + 1
	}
}

// tally is what G0 and G4 read of the live groups of the working copy.
type tally struct {
	// regular counts the live groups without the mark.
	regular int
	// marked counts the live groups G0 could promote: marked, below N.
	marked int
	// free lists the ordinals below N that neither a live group nor a
	// foreign object takes, lowest first.
	free []int
}

func (p *planner) tally() *tally {
	if p.count == nil {
		n := p.spec.Replicas
		t := &tally{}
		for _, g := range p.work {
			switch {
			case !g.live():
			case !isSurge(p.spec, g):
				t.regular++
			case g.Ordinal < n:
				t.marked++
			}
		}
		for o := range n {
			if g, ok := p.work[o]; (!ok || !g.live()) && !p.foreign[o] {
				t.free = append(t.free, o)
			}
		}
		p.count = t
	}
	return p.count
}

// settled is the number of regular groups once every deletion under way is
// complete (the terminating and failed groups are gone) and G0 and G4 have
// come to rest, and why G4 left a place unfilled. without, when set, is taken
// out first.
//
// G0 and G4 are replayed on the tally of the live groups, not on a copy of
// the working copy: the rules are those of promote and fill, and
// TestSettledAgainstSteps holds the two together. One thing differs on
// purpose. The bound N + S takes no part here: it passes by itself (the surge
// is reclaimed, the deletions finish), unlike a foreign object or a B that
// cannot be read.
func (p *planner) settled(without *Group) (regular int, why string, at int) {
	n := p.spec.Replicas
	t := p.tally()
	regular, marked, free := t.regular, t.marked, t.free
	freed := -1
	if without != nil && without.live() {
		switch {
		case !isSurge(p.spec, *without):
			regular--
		case without.Ordinal < n:
			marked--
		}
		if o := without.Ordinal; o >= 0 && o < n && !p.foreign[o] {
			freed = o
		}
	}
	// G0: a marked group below N loses the mark while regular < N.
	for ; marked > 0 && regular < n; marked-- {
		regular++
	}
	// G4: one group at a time at the lowest free ordinal below N.
	for regular < n {
		var ordinal int
		switch {
		case freed >= 0 && (len(free) == 0 || freed < free[0]):
			ordinal, freed = freed, -1
		case len(free) > 0:
			ordinal, free = free[0], free[1:]
		default:
			return regular, ReasonOrdinalOccupied, -1
		}
		if _, known := placeRevision(p.spec, ordinal); !known {
			return regular, ReasonBaselineUnknown, ordinal
		}
		regular++
	}
	return regular, "", -1
}

// canBeFilled tells whether the place of the regular group g can be filled
// once it is gone: settled without g is not below settled with it. So a free
// ordinal serves one need; a marked group below N that G0 would promote is a
// place as well; an ordinal that needs a B that cannot be read is none, and
// neither is any ordinal above it. When it cannot, at and why say what to
// report.
func (p *planner) canBeFilled(g Group) (ok bool, at int, why string) {
	with, _, _ := p.settled(nil)
	without, why, at := p.settled(&g)
	if without >= with {
		return true, -1, ""
	}
	return false, at, why
}

// recreated tells whether G3c replaces g by deleting it. In place, a group is
// retargeted instead unless it asks for recreation.
func (p *planner) recreated(g Group) bool {
	return !p.spec.InPlace || g.RecreateRequired
}

// rolling tells whether there is work: a replaceable regular group that is
// replaced in place, or by recreation and whose place can be filled. With
// recreationOnly the groups replaced in place do not count: nothing is deleted
// for them, so they are no reason for surge (G3a, G3b).
func (p *planner) rolling(recreationOnly bool) bool {
	classes := p.view().Classes
	for _, g := range p.work {
		if classes[g.Ordinal] != ClassReplaceable || isSurge(p.spec, g) {
			continue
		}
		if !p.recreated(g) {
			if recreationOnly {
				continue
			}
			return true
		}
		if ok, _, _ := p.canBeFilled(g); ok {
			return true
		}
	}
	return false
}

// createSurge is G3a: surge for the work that is executed by recreation, not
// while scaling down, at the lowest free ordinals not below N, until
// active = N + S.
func (p *planner) createSurge(scaling bool) {
	if scaling || !p.rolling(true) {
		return
	}
	ordinal := p.spec.Replicas
	for len(p.work) < p.spec.Replicas+p.spec.MaxSurge {
		for p.taken(ordinal) {
			ordinal++
		}
		p.create(ordinal, p.spec.Target, true, ReasonSurge)
	}
}

// reclaimSurge is G3b: surge that is not needed any more, or that exceeds the
// capacity. No work: every live marked group that does not await adoption.
// Work and live > N + S: down to live = N + S.
//
// A surge group stands in for a regular place until N other groups have
// their members, and is reclaimed only then. At the end of a rollout the last
// old group is still terminating, or its replacement exists and has no Pod
// yet; a surge group that left then would leave fewer than N groups with
// Pods, although no place is spare. Whether the surge group has members
// itself is not asked: one that is new has none yet, and neither has one
// whose Pods the cache does not show yet. With maxUnavailable at replicas
// every old group goes in the round that creates the surge group, and the
// next round would take a surge group without a Pod away before it had one.
//
// The one exception is that there is no room: a regular place is missing
// while the live groups fill N + S (more surge groups than maxSurge allows,
// after it was lowered), so that place can only be created after a surge
// group has left. A regular group that is terminating or failed still holds
// its place.
//
// One that is confirmed not Ready is then reclaimed without asking the floor:
// it takes nothing off ready. Every other one (confirmed Ready, not observed,
// not fresh) only when ready stays at or above the floor. G3a and G3b never
// both act in one round.
func (p *planner) reclaimSurge() {
	v := p.view()
	bound := p.spec.Replicas + p.spec.MaxSurge
	going := p.rolling(true)
	if going && v.Live <= bound {
		return
	}
	var marked []Group
	// filled counts the groups that have their members.
	filled := 0
	for _, g := range p.work {
		if p.hasMembers(g) {
			filled++
		}
		if g.Surge && g.live() && !p.awaiting[g.Ordinal] {
			marked = append(marked, g)
		}
	}
	slices.SortFunc(marked, reclaimOrder)
	// Only marked groups are deleted here, so the regular groups stay as they are.
	missing := p.regulars() < p.spec.Replicas
	live, ready := v.Live, v.Ready
	for _, g := range marked {
		if going && live <= bound {
			return
		}
		// others: the groups that have their members, without g itself.
		has := p.hasMembers(g)
		others := filled
		if has {
			others--
		}
		if others < p.spec.Replicas && !(missing && live >= bound) {
			continue
		}
		if !g.confirmedNotReady() {
			// A confirmed Ready one takes one off ready, the others take
			// nothing off, so ready itself has to be at or above the floor.
			left := ready
			if g.confirmedReady() {
				left--
			}
			if left < v.Floor {
				continue
			}
			ready = left
		}
		p.delete(g, ReasonReclaimSurge)
		live--
		if has {
			filled--
		}
	}
}

// hasMembers: the group is live and a member Pod of it that is not
// terminating is there, or it expects none. A group created in this round has
// none yet, and neither has one whose status only says that its Pods are
// being created. One that awaits adoption has not adopted them yet.
func (p *planner) hasMembers(g Group) bool {
	return g.live() && !p.awaiting[g.Ordinal] && (g.HasPods || g.Empty)
}

// unfilled is a place G3c cannot fill, and why.
type unfilled struct {
	at  int
	why string
}

// replaceGroups is G3c: work, not scaling down. Candidates are the replaceable
// groups that are settled and do not await adoption, regular and surge alike.
// One that is confirmed not Ready while maxScaleDown > 0; else one that is
// confirmed Ready while both budgets are > 0; highest ordinal first within a
// class; the budgets are taken again after each.
//
// "Work" is asked once, when the step is entered: after the last replaceable
// regular group is gone a replaceable surge group can still be replaced in
// the same round, within the budgets.
func (p *planner) replaceGroups(scaling bool) {
	if scaling {
		return
	}
	if !p.rolling(false) {
		// Nothing is rolled because no place can be filled. The plan still says why.
		classes := p.view().Classes
		for _, g := range descending(p.work) {
			if classes[g.Ordinal] != ClassReplaceable || isSurge(p.spec, g) {
				continue
			}
			if ok, at, why := p.canBeFilled(g); !ok {
				p.wait(at, why)
			}
		}
		return
	}
	// The step changes groups and adds none: one order serves every pass.
	order := descending(p.work)
	for {
		v := p.view()
		// The candidate of each class: the highest ordinal that may be replaced.
		var notReady, healthy *Group
		var blocked []unfilled
		for _, old := range order {
			g := p.work[old.Ordinal]
			if v.Classes[g.Ordinal] != ClassReplaceable || p.awaiting[g.Ordinal] {
				continue
			}
			class := g.replaceClass()
			if class == replaceUnsettled {
				continue
			}
			pick := &healthy
			if class == replaceNotReady {
				pick = &notReady
			}
			if *pick != nil {
				continue
			}
			// A regular group is deleted only when its place can be filled
			// afterwards; one that cannot is passed over. A surge group and a
			// group replaced in place have no such condition.
			if !isSurge(p.spec, g) && p.recreated(g) {
				if ok, at, why := p.canBeFilled(g); !ok {
					blocked = append(blocked, unfilled{at, why})
					continue
				}
			}
			*pick = &g
		}
		switch {
		case notReady != nil && v.MaxScaleDown > 0:
			p.replaceOne(*notReady, ReasonReplaceNotReady)
		case healthy != nil && min(v.MaxScaleDown, v.MaxHealthyScaleDown) > 0:
			p.replaceOne(*healthy, ReasonReplace)
		default:
			if notReady == nil && healthy == nil {
				// No candidate is left but ones whose place cannot be filled:
				// none was passed over unasked, so blocked names them all.
				for _, b := range blocked {
					p.wait(b.at, b.why)
				}
			}
			return
		}
	}
}

// replaceOne replaces one candidate of G3c. A replacement is a deletion; the
// place is filled by G4 of a later round. In place, spec.revision is set to
// the target instead and the ServingGroup controller updates the members: the
// group is then of the target revision and not Ready, so it uses the same
// budgets.
func (p *planner) replaceOne(g Group, why string) {
	if p.recreated(g) {
		p.delete(g, why)
		return
	}
	p.acts = append(p.acts, Action{Kind: ActionRetarget, Ordinal: g.Ordinal, UID: g.UID, Revision: p.spec.Target, Reason: ReasonInPlace})
	g.Revision, g.Ready, g.PodsReady, g.Observed = p.spec.Target, false, false, false
	p.put(g)
}

// advanceBaseline is G6: the baseline moves only when the whole rollout is
// complete: no partition, N > 0, active = N, no marked group, every group of
// the target revision and confirmed Ready, and no empty group (zero counts
// must not advance it).
func (p *planner) advanceBaseline() {
	spec := p.spec
	partitioned := spec.Partition != 0
	if spec.Mode == ModeRole {
		partitioned = spec.RolePartitioned
	}
	if spec.Baseline == spec.Target || partitioned || spec.Replicas <= 0 || len(p.work) != spec.Replicas {
		return
	}
	for _, g := range p.work {
		if g.Surge || g.Revision != spec.Target || !g.confirmedReady() || g.Empty {
			return
		}
	}
	p.acts = append(p.acts, Action{Kind: ActionAdvanceBaseline, Ordinal: -1, Revision: spec.Target, Reason: ReasonComplete})
}
