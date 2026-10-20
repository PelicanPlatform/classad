package collections

import (
	"fmt"
	"strings"
	"testing"
)

// targetDepColls builds the collections a target-dependence case runs against: a plain
// one, an indexed one (index-candidate pre-filter), and wide-slot ones (each slot padded
// past closureDecodeMinAttrs) with and without the hot-header match closure, so the
// deferred closure-decode paths are exercised too.
func targetDepColls(t *testing.T, slots, indexed []string) map[string]*Collection {
	t.Helper()
	build := func(opts Options, pad int) *Collection {
		c := New(opts)
		for i, s := range slots {
			text := s
			if pad > 0 {
				var b strings.Builder
				for k := 0; k < pad; k++ {
					fmt.Fprintf(&b, "Pad%d = %d; ", k, k)
				}
				text = "[ " + b.String() + strings.TrimPrefix(strings.TrimSpace(s), "[")
			}
			if err := c.Put([]byte(fmt.Sprintf("s%d", i)), mustAd(t, text)); err != nil {
				t.Fatal(err)
			}
		}
		c.Reindex()
		return c
	}
	return map[string]*Collection{
		"plain":   build(Options{Shards: 1, QueryParallelism: 1}, 0),
		"indexed": build(Options{Shards: 2, ValueAttrs: indexed, CategoricalAttrs: []string{"Arch"}}, 0),
		"wide":    build(Options{Shards: 2}, closureDecodeMinAttrs+8),
		"hot":     build(Options{Shards: 2, MatchClosureRoots: []string{"Requirements"}}, closureDecodeMinAttrs+8),
	}
}

// TestMatchTargetDependentJobAttr: a job attribute whose value depends on the slot
// (TARGET.x, an unscoped name the job does not define, or another such attribute) has
// no value apart from the slot. Every match entry point must evaluate it against each
// slot, as classad.MatchClassAd does, rather than treat its no-target value as a
// constant -- in the wire-native job-side verdict, the wire-native Rank, the index plan,
// and the partial (closure) slot ad.
func TestMatchTargetDependentJobAttr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		job   string
		slots []string
		want  map[int]matchResult // MatchClassAd's result, asserted so the case stays meaningful
	}{
		{
			// MY.Foo is ERROR against this slot, and ERROR || TRUE is ERROR.
			name:  "requirements",
			job:   `[ Foo = TARGET.S * 2 > 1; Requirements = MY.Foo || true ]`,
			slots: []string{`[ Id = 1; S = "str"; Requirements = true ]`},
			want:  map[int]matchResult{},
		},
		{
			name:  "rank",
			job:   `[ Foo = TARGET.Memory; Rank = MY.Foo; Requirements = true ]`,
			slots: []string{`[ Id = 1; Memory = 10; Requirements = true ]`, `[ Id = 2; Memory = 20; Requirements = true ]`},
			want:  map[int]matchResult{1: {10, true}, 2: {20, true}},
		},
		{
			name:  "rank-transitive",
			job:   `[ Foo = TARGET.Memory; Bar = MY.Foo + 1; Rank = Bar; Requirements = TARGET.Memory > 0 ]`,
			slots: []string{`[ Id = 1; Memory = 10; Requirements = true ]`, `[ Id = 2; Memory = 20; Requirements = TARGET.Bar > 0 ]`},
			want:  map[int]matchResult{1: {11, true}, 2: {21, true}},
		},
		{
			// An unscoped name the job does not define resolves in the slot.
			name:  "unscoped-fallthrough",
			job:   `[ Requirements = (Memory =!= undefined) || TARGET.X > 5 ]`,
			slots: []string{`[ Id = 1; Memory = 10; X = 0; Requirements = true ]`, `[ Id = 2; X = 1; Requirements = true ]`},
			want:  map[int]matchResult{1: {}},
		},
		{
			name:  "isnt-undefined",
			job:   `[ Foo = TARGET.Y; Requirements = (MY.Foo =!= undefined) || TARGET.X > 5 ]`,
			slots: []string{`[ Id = 1; Y = 10; X = 0; Requirements = true ]`, `[ Id = 2; X = 1; Requirements = true ]`},
			want:  map[int]matchResult{1: {}},
		},
		{
			name:  "elvis",
			job:   `[ Foo = TARGET.Y; Requirements = (MY.Foo ?: false) || TARGET.X > 5 ]`,
			slots: []string{`[ Id = 1; Y = true; X = 0; Requirements = true ]`, `[ Id = 2; X = 1; Requirements = true ]`},
			want:  map[int]matchResult{1: {}},
		},
		{
			// The slot reads a job attribute that reads the slot back.
			name: "slot-reads-dependent-job-attr",
			job:  `[ Foo = TARGET.Memory; Half = Memory / 2; Requirements = true; Rank = 1 ]`,
			slots: []string{
				`[ Id = 1; Memory = 10; Requirements = TARGET.Foo > 5 && Half >= 5 ]`,
				`[ Id = 2; Memory = 4; Requirements = TARGET.Foo > 5 ]`,
				`[ Id = 3; Memory = 8; Requirements = Half > 5 ]`,
			},
			want: map[int]matchResult{1: {1, true}},
		},
		{
			// A function of one categorical slot attribute is folded into a membership
			// probe by evaluating it per value; a job attribute that reads the slot has
			// no value there.
			name: "finite-domain",
			job:  `[ Foo = TARGET.Wanted; Requirements = stringListMember(TARGET.Arch, MY.Foo) || TARGET.Memory > 100 ]`,
			slots: []string{
				`[ Id = 1; Arch = "X86_64"; Wanted = "X86_64,ARM"; Memory = 1; Requirements = true ]`,
				`[ Id = 2; Arch = "PPC"; Wanted = "X86_64"; Memory = 1; Requirements = true ]`,
			},
			want: map[int]matchResult{1: {}},
		},
		{
			// eval() hides what the Rank reads: no partial slot ad can be trusted.
			name:  "rank-eval",
			job:   `[ Requirements = TARGET.Memory > 0; Rank = eval("TARGET.Memory") ]`,
			slots: []string{`[ Id = 1; Memory = 10; Requirements = TARGET.Rank =!= undefined ]`, `[ Id = 2; Memory = 20; Requirements = MY.Memory > 0 ]`},
			want:  map[int]matchResult{1: {10, true}, 2: {20, true}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := bruteMatch(t, tc.job, tc.slots)
			if diff := diffMatchSets(want, tc.want, true); diff != "" {
				t.Fatalf("MatchClassAd differs from the case's expectation:\n%s", diff)
			}
			for name, c := range targetDepColls(t, tc.slots, []string{"X", "Memory"}) {
				checkEntryPoints(t, name, c, tc.job, want)
				for _, k := range []int{1, 2} { // deferred top-N, where closure decode runs
					got := c.MatchSortedRanked(mustAd(t, tc.job), k)
					if len(got) != min(k, len(want)) {
						t.Fatalf("%s limit %d: got %d matches, want %d", name, k, len(got), min(k, len(want)))
					}
					for _, rm := range got {
						id, _ := rm.Ad.EvaluateAttrInt("Id")
						w, ok := want[int(id)]
						if !ok || w.hasRank != rm.HasRank || (w.hasRank && w.rank != rm.Rank) {
							t.Fatalf("%s limit %d: Id %d rank %v/%v, MatchClassAd %v (matched %v)", name, k, id, rm.Rank, rm.HasRank, w, ok)
						}
					}
				}
			}
		})
	}
}
