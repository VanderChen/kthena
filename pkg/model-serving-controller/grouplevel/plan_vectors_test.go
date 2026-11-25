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
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"k8s.io/apimachinery/pkg/types"
)

// vector is one line of testdata/plan_vectors*.jsonl.gz: one call of the plan
// of the reference model, its input in the shape of this package, and the
// writes and the view the model returned. See testdata/README.md.
type vector struct {
	Source string `json:"source"`
	Spec   struct {
		Replicas        int    `json:"replicas"`
		MaxUnavailable  int    `json:"maxUnavailable"`
		MaxSurge        int    `json:"maxSurge"`
		Partition       int    `json:"partition"`
		Target          string `json:"target"`
		Baseline        string `json:"baseline"`
		Mode            string `json:"mode"`
		RolePartitioned bool   `json:"rolePartitioned"`
	} `json:"spec"`
	Groups []struct {
		Ordinal          int    `json:"ordinal"`
		UID              string `json:"uid"`
		Revision         string `json:"revision"`
		Surge            bool   `json:"surge"`
		Terminating      bool   `json:"terminating"`
		Failed           bool   `json:"failed"`
		Drained          bool   `json:"drained"`
		Observed         bool   `json:"observed"`
		Fresh            bool   `json:"fresh"`
		Ready            bool   `json:"ready"`
		PodsReady        bool   `json:"podsReady"`
		Memberless       bool   `json:"memberless"`
		Empty            bool   `json:"empty"`
		RecreateRequired bool   `json:"recreateRequired"`
		DeletionCost     int64  `json:"deletionCost"`
	} `json:"groups"`
	Orphans []struct {
		Ordinal  int    `json:"ordinal"`
		Revision string `json:"revision"`
		Marked   bool   `json:"marked"`
	} `json:"orphans"`
	Foreign []int `json:"foreign"`
	Acts    []struct {
		Kind     string `json:"kind"`
		Ordinal  int    `json:"ordinal"`
		UID      string `json:"uid"`
		Revision string `json:"revision"`
		Surge    bool   `json:"surge"`
		Reason   string `json:"reason"`
	} `json:"acts"`
	View struct {
		Classes             map[int]Class `json:"classes"`
		Active              int           `json:"active"`
		Live                int           `json:"live"`
		Ready               int           `json:"ready"`
		Regular             int           `json:"regular"`
		RegularLive         int           `json:"regularLive"`
		Surge               int           `json:"surge"`
		NotReadyTarget      int           `json:"notReadyTarget"`
		Floor               int           `json:"floor"`
		MaxScaleDown        int           `json:"maxScaleDown"`
		MaxHealthyScaleDown int           `json:"maxHealthyScaleDown"`
		Work                bool          `json:"work"`
		ScalingDown         bool          `json:"scalingDown"`
	} `json:"view"`
}

func (v vector) input(t *testing.T) Input {
	t.Helper()
	in := Input{
		Spec: Spec{
			Replicas:        v.Spec.Replicas,
			MaxUnavailable:  v.Spec.MaxUnavailable,
			MaxSurge:        v.Spec.MaxSurge,
			Partition:       v.Spec.Partition,
			Target:          v.Spec.Target,
			Baseline:        v.Spec.Baseline,
			RolePartitioned: v.Spec.RolePartitioned,
		},
		Foreign: slices.Clone(v.Foreign),
	}
	switch v.Spec.Mode {
	case "ServingGroup":
		in.Spec.Mode = ModeServingGroup
	case "Role":
		in.Spec.Mode = ModeRole
	default:
		t.Fatalf("unknown mode %q", v.Spec.Mode)
	}
	for _, g := range v.Groups {
		in.Groups = append(in.Groups, Group{
			Ordinal:          g.Ordinal,
			UID:              types.UID(g.UID),
			Revision:         g.Revision,
			Surge:            g.Surge,
			Terminating:      g.Terminating,
			Failed:           g.Failed,
			Drained:          g.Drained,
			Observed:         g.Observed,
			Fresh:            g.Fresh,
			Ready:            g.Ready,
			PodsReady:        g.PodsReady,
			Memberless:       g.Memberless,
			Empty:            g.Empty,
			RecreateRequired: g.RecreateRequired,
			DeletionCost:     g.DeletionCost,
		})
	}
	if len(v.Orphans) > 0 {
		in.Orphans = make(map[int]Orphans, len(v.Orphans))
		for _, o := range v.Orphans {
			in.Orphans[o.Ordinal] = Orphans{Revision: o.Revision, Marked: o.Marked}
		}
	}
	return in
}

func (v vector) want() []Action {
	var acts []Action
	for _, a := range v.Acts {
		acts = append(acts, Action{
			Kind:     ActionKind(a.Kind),
			Ordinal:  a.Ordinal,
			UID:      types.UID(a.UID),
			Revision: a.Revision,
			Surge:    a.Surge,
			Reason:   a.Reason,
		})
	}
	return acts
}

func (v vector) wantView() View {
	return View{
		Classes:             v.View.Classes,
		Active:              v.View.Active,
		Live:                v.View.Live,
		Ready:               v.View.Ready,
		Regular:             v.View.Regular,
		RegularLive:         v.View.RegularLive,
		Surge:               v.View.Surge,
		NotReadyTarget:      v.View.NotReadyTarget,
		Floor:               v.View.Floor,
		MaxScaleDown:        v.View.MaxScaleDown,
		MaxHealthyScaleDown: v.View.MaxHealthyScaleDown,
		Work:                v.View.Work,
		ScalingDown:         v.View.ScalingDown,
	}
}

// cloneInput copies everything Plan could reach through in.
func cloneInput(in Input) Input {
	out := in
	out.Groups = slices.Clone(in.Groups)
	out.Foreign = slices.Clone(in.Foreign)
	if in.Orphans != nil {
		out.Orphans = make(map[int]Orphans, len(in.Orphans))
		for k, o := range in.Orphans {
			out.Orphans[k] = o
		}
	}
	return out
}

// loaded is a vector with the place it was read from and its line.
type loaded struct {
	where string
	line  []byte
	vector
}

// vectors reads every testdata/plan_vectors*.jsonl.gz, once for the tests of
// the package. The pattern lets the whole corpus of the exporter be dropped in
// next to the sample for a one-off run.
var vectors = sync.OnceValues(func() ([]loaded, error) {
	files, err := filepath.Glob(filepath.Join("testdata", "plan_vectors*.jsonl.gz"))
	if err != nil {
		return nil, err
	}
	var all []loaded
	for _, name := range files {
		data, err := os.ReadFile(name)
		if err != nil {
			return nil, err
		}
		zr, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		sc := bufio.NewScanner(zr)
		sc.Buffer(make([]byte, 0, 1<<20), 1<<26)
		for index := 0; sc.Scan(); index++ {
			v := loaded{line: bytes.Clone(sc.Bytes())}
			dec := json.NewDecoder(bytes.NewReader(v.line))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&v.vector); err != nil {
				return nil, fmt.Errorf("%s: vector %d: %w", name, index, err)
			}
			v.where = fmt.Sprintf("%s: vector %d (%s)", filepath.Base(name), index, v.Source)
			all = append(all, v)
		}
		if err := sc.Err(); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
	}
	if len(all) == 0 {
		return nil, fmt.Errorf("no vector in testdata (%v)", files)
	}
	return all, nil
})

// forEachVector calls fn with every vector, the place it was read from and its line.
func forEachVector(t *testing.T, fn func(where string, line []byte, v vector)) int {
	t.Helper()
	all, err := vectors()
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range all {
		fn(v.where, v.line, v.vector)
	}
	return len(all)
}

// maxReported bounds the mismatches that are printed in full.
const maxReported = 20

// TestPlanAgainstModel replays every call of the reference model: the whole
// ordered list of writes has to be the model's.
func TestPlanAgainstModel(t *testing.T) {
	failed := 0
	bySource := map[string]int{}
	total := forEachVector(t, func(where string, line []byte, v vector) {
		bySource[v.Source]++
		in := v.input(t)
		before := cloneInput(in)
		got, want := Plan(in), v.want()
		if !slices.Equal(got, want) {
			if failed++; failed <= maxReported {
				t.Errorf("%s: Plan differs from the model\ninput: %s\n got: %+v\nwant: %+v", where, line, got, want)
			}
			return
		}
		if !reflect.DeepEqual(in, before) {
			t.Errorf("%s: Plan modified its input\ninput: %s", where, line)
		}
		// The order of the slices is no input.
		slices.Reverse(in.Groups)
		slices.Reverse(in.Foreign)
		if again := Plan(in); !slices.Equal(again, want) {
			if failed++; failed <= maxReported {
				t.Errorf("%s: Plan depends on the order of its input\ninput: %s\n got: %+v\nwant: %+v", where, line, again, want)
			}
		}
	})
	if failed > maxReported {
		t.Errorf("%d more mismatches not shown", failed-maxReported)
	}
	t.Logf("%d vectors: %v", total, bySource)
}

// TestClassifyAgainstModel compares the classes and the counts of the input
// of every vector with the model's.
func TestClassifyAgainstModel(t *testing.T) {
	failed := 0
	forEachVector(t, func(where string, line []byte, v vector) {
		in := v.input(t)
		awaiting := map[int]bool{}
		for ordinal := range in.Orphans {
			awaiting[ordinal] = true
		}
		got, want := Classify(in.Spec, in.Groups, awaiting), v.wantView()
		if !reflect.DeepEqual(got, want) {
			if failed++; failed <= maxReported {
				t.Errorf("%s: Classify differs from the model\ninput: %s\n got: %+v\nwant: %+v", where, line, got, want)
			}
		}
	})
	if failed > maxReported {
		t.Errorf("%d more mismatches not shown", failed-maxReported)
	}
}

// settledBySteps is what settled has to be: G0 and G4 themselves, run on a
// copy of the live groups of the working copy, without the bound N + S.
func settledBySteps(p *planner, without *Group) (regular int, why string, at int) {
	q := &planner{spec: p.spec, work: map[int]Group{}, awaiting: p.awaiting, foreign: p.foreign}
	q.spec.MaxSurge = len(p.work) + 1 // G4 never gets there
	for _, g := range p.work {
		if g.live() && (without == nil || g.Ordinal != without.Ordinal) {
			q.put(g)
		}
	}
	q.promote()
	q.fill()
	regular = q.regulars()
	if regular >= q.spec.Replicas {
		return regular, "", -1
	}
	free, found := q.lowestFree(0)
	if !found {
		return regular, ReasonOrdinalOccupied, -1
	}
	return regular, ReasonBaselineUnknown, free
}

// TestSettledAgainstSteps holds the tally settled counts on to the steps it
// stands for, on the groups of every vector before and after the round, with
// each group taken out in turn.
func TestSettledAgainstSteps(t *testing.T) {
	failed := 0
	compare := func(where string, line []byte, p *planner) {
		for _, without := range append([]*Group{nil}, pointers(sorted(p.work))...) {
			regular, why, at := p.settled(without)
			wantRegular, wantWhy, wantAt := settledBySteps(p, without)
			if regular != wantRegular || why != wantWhy || at != wantAt {
				if failed++; failed <= maxReported {
					t.Errorf("%s: settled(%+v) = %d %q %d, the steps give %d %q %d\ninput: %s", where, without, regular, why, at,
						wantRegular, wantWhy, wantAt, line)
				}
			}
		}
	}
	forEachVector(t, func(where string, line []byte, v vector) {
		in := v.input(t)
		p := newPlanner(in)
		compare(where+" before the round", line, p)
		p.run(in.Orphans)
		compare(where+" after the round", line, p)
	})
	if failed > maxReported {
		t.Errorf("%d more mismatches not shown", failed-maxReported)
	}
}

func pointers(groups []Group) []*Group {
	out := make([]*Group, len(groups))
	for i := range groups {
		out[i] = &groups[i]
	}
	return out
}

// TestInPlaceFallsBackToRecreation: when every group of an old revision asks
// for recreation, nothing is executed in place, and the plan of an in-place
// rollout is the plan of a rollout by recreation.
func TestInPlaceFallsBackToRecreation(t *testing.T) {
	failed, compared := 0, 0
	forEachVector(t, func(where string, line []byte, v vector) {
		in := v.input(t)
		if in.Spec.Mode != ModeServingGroup || len(in.Orphans) > 0 {
			return // a group that S1′ recovers has no RecreateRequired
		}
		for i := range in.Groups {
			if in.Groups[i].Revision != in.Spec.Target {
				in.Groups[i].RecreateRequired = true
			}
		}
		want := Plan(in)
		in.Spec.InPlace = true
		compared++
		if got := Plan(in); !slices.Equal(got, want) {
			if failed++; failed <= maxReported {
				t.Errorf("%s (RecreateRequired on every old group): in place differs from recreation\ninput: %s\n got: %+v\nwant: %+v", where, line, got, want)
			}
		}
	})
	if failed > maxReported {
		t.Errorf("%d more mismatches not shown", failed-maxReported)
	}
	t.Logf("%d vectors compared", compared)
}

// TestInPlaceOnVectors replays the input of every vector as an in-place
// rollout. The model has no in-place update, so there is no expected plan;
// what an in-place round may and may not write is checked instead.
func TestInPlaceOnVectors(t *testing.T) {
	failed, retargets := 0, 0
	forEachVector(t, func(where string, line []byte, v vector) {
		in := v.input(t)
		if in.Spec.Mode != ModeServingGroup {
			return
		}
		in.Spec.InPlace = true
		byOrdinal := map[int]Group{}
		recreation := false
		for _, g := range in.Groups {
			byOrdinal[g.Ordinal] = g
			recreation = recreation || g.RecreateRequired
		}
		view := Classify(in.Spec, in.Groups, nil)
		var problems []string
		for _, a := range Plan(in) {
			g, known := byOrdinal[a.Ordinal]
			switch {
			case a.Kind == ActionDelete && (a.Reason == ReasonReplace || a.Reason == ReasonReplaceNotReady):
				if !known || !g.RecreateRequired {
					problems = append(problems, fmt.Sprintf("%+v deletes a group that can be updated in place", a))
				}
			case a.Kind == ActionRetarget && a.Reason == ReasonInPlace:
				retargets++
				_, awaiting := in.Orphans[a.Ordinal]
				if !known || g.RecreateRequired || view.Classes[a.Ordinal] != ClassReplaceable || !g.Observed || !g.Fresh || awaiting ||
					a.Revision != in.Spec.Target || a.UID != g.UID {
					problems = append(problems, fmt.Sprintf("%+v is no settled replaceable group that can be updated in place", a))
				}
			case a.Kind == ActionCreate && a.Reason == ReasonSurge:
				if !recreation {
					problems = append(problems, fmt.Sprintf("%+v: surge although no group asks for recreation", a))
				}
			}
		}
		if len(problems) > 0 {
			if failed++; failed <= maxReported {
				t.Errorf("%s as an in-place rollout: %v\ninput: %s", where, problems, line)
			}
		}
	})
	if failed > maxReported {
		t.Errorf("%d more vectors not shown", failed-maxReported)
	}
	if retargets == 0 {
		t.Error("no vector was updated in place")
	}
	t.Logf("%d in-place retargets", retargets)
}
