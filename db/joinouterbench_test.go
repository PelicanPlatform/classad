package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// Where the batched path stands against BOTH incumbents, measured rather than argued.
//
// An earlier version of this compared the batched path against the hash alone and read its
// 0.3x as a case for taking the path on selectivity. It was not one. A selective FROM side
// against a large indexed table is the index probe's shape (joinprobe.go), so the hash is
// the weaker of the two incumbents there and beating it says nothing about replacing the
// stronger one. That benchmark reported ProbeKeys of 0 on every row and the number was not
// read. This one runs all three arms and asserts what each one did.
//
// It sweeps FAN-OUT -- how many inner rows share a key -- at a fixed inner size, because
// that is the axis that separates the three paths. A hash pays for the inner side once
// however few rows probe it; the probe pays per distinct key, and its cost model prices only
// keys and segments; the batched path pays per batch of 64 keys and then per row those keys
// reach. Sweeping only the FROM side's row count holds fan-out at 1, where a key count and a
// pair count are the same number and cannot be told apart.

// fanFixture builds an inner side of about innerRows rows in which each key carries fan of
// them, against a FROM side of one row per key.
func fanFixture(t testing.TB, keys, fan, segSize int) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("hist", ArchiveConfig{
		SegmentSize: segSize,
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateArchiveTable("jobs", ArchiveConfig{SegmentSize: segSize})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= keys; i++ {
		for r := 0; r < fan; r++ {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			ad.InsertAttr("ProcId", int64(r))
			ad.InsertAttrString("Pad", strings.Repeat("x", 64))
			if aerr := hist.Append(ad); aerr != nil {
				t.Fatal(aerr)
			}
		}
		aj := classad.New()
		aj.InsertAttr("ClusterId", int64(i))
		aj.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(aj); aerr != nil {
			t.Fatal(aerr)
		}
	}
	return cat
}

// joinArm is one of the three paths, with the knobs that select it.
//
// The probe arm runs with its COST GATE off rather than as the planner would run it, because
// the question is where the path belongs rather than where it currently goes: at this
// fixture's segment counts the gate declines nearly the whole sweep, so an arm that took the
// gate's answer would be the hash arm again under another name. What the planner really
// chooses is reported separately, in the last column.
type joinArm struct {
	name string
	wrap func(func())
	mode outerDrivenMode
}

func TestOuterDrivenAgainstTheIndexProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("times three join paths over several fan-outs and FROM-side sizes")
	}
	const innerRows = 20_000
	arms := []joinArm{
		{"hash", withoutProbe, outerDrivenNever},
		{"probe", forcingProbe, outerDrivenNever},
		{"batched", func(f func()) { f() }, outerDrivenForce},
	}
	for _, fan := range []int{1, 8, 64} {
		fanArm(t, innerRows, fan, arms)
	}
}

func fanArm(t *testing.T, innerRows, fan int, arms []joinArm) {
	keys := innerRows / fan
	cat := fanFixture(t, keys, fan, 65536)
	_, segments, _, _, _ := cat.joinSideStats("hist", "")
	const rounds = 3

	run := func(a joinArm, limit int) (time.Duration, int, JoinStats) {
		spec := outerSpec(JoinInner)
		spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", limit)
		var st JoinStats
		var d time.Duration
		pairs := 0
		withOuterDrivenMode(a.mode, func() {
			a.wrap(func() {
				seq, err := cat.Join(spec, JoinLimits{}, &st)
				if err != nil {
					t.Fatalf("Join: %v", err)
				}
				start := time.Now()
				for range seq {
					pairs++
				}
				d = time.Since(start)
			})
		})
		if st.Err != nil {
			t.Fatalf("join stopped early: %v", st.Err)
		}
		return d, pairs, st
	}

	type key struct {
		arm   string
		limit int
	}
	samples := map[key][]time.Duration{}
	pairsOf := map[key]int{}
	statOf := map[key]JoinStats{}
	limits := make([]int, 0, 4)
	for _, frac := range []float64{0.003, 0.05, 0.2, 0.8} {
		limits = append(limits, max(int(float64(keys)*frac), 1))
	}

	for r := 0; r < rounds; r++ {
		for _, limit := range limits {
			// Rotate which arm goes first. An arm measured always first pays the cold
			// cache for the two behind it.
			for i := range arms {
				a := arms[(i+r)%len(arms)]
				d, pairs, st := run(a, limit)
				k := key{a.name, limit}
				if prev, ok := pairsOf[k]; ok && prev != pairs {
					t.Fatalf("%s fan=%d limit=%d matched %d pairs this round and %d last",
						a.name, fan, limit, pairs, prev)
				}
				pairsOf[k] = pairs
				statOf[k] = st
				samples[k] = append(samples[k], d)
			}
		}
	}

	fmt.Printf("\n=== fan %d: %d keys x %d rows = %d inner rows over %d segments ===\n",
		fan, keys, fan, innerRows, segments)
	fmt.Printf("%7s %8s %9s %11s %11s %11s %8s %8s %9s\n",
		"keys", "pairs", "pairs/in", "hash", "probe", "batched", "b/hash", "b/probe", "chosen")
	for _, limit := range limits {
		hk := key{"hash", limit}
		pk := key{"probe", limit}
		bk := key{"batched", limit}
		// An arm that matched fewer pairs did less work, and comparing the times would
		// report that as a win.
		if pairsOf[hk] != pairsOf[pk] || pairsOf[hk] != pairsOf[bk] {
			t.Fatalf("fan=%d limit=%d: arms matched %d, %d and %d pairs; they are not comparable",
				fan, limit, pairsOf[hk], pairsOf[pk], pairsOf[bk])
		}
		if pairsOf[hk] == 0 {
			t.Fatalf("fan=%d limit=%d matched no pairs; the arms measure nothing", fan, limit)
		}
		// Provenance on EVERY arm, not only the one under test. Arms that all ran the same
		// plan produce a tidy 1.00x and say nothing at all, and that is how the version of
		// this benchmark that compared against a probe which never probed read as a result.
		if st := statOf[pk]; st.ProbeKeys == 0 {
			t.Fatalf("fan=%d limit=%d: the probe arm did not probe (%q)", fan, limit, st.ProbeWhy)
		}
		if st := statOf[bk]; st.OuterDrivenBatches == 0 {
			t.Fatalf("fan=%d limit=%d: the batched arm ran no batches, so it is another arm twice",
				fan, limit)
		}
		if st := statOf[hk]; st.ProbeKeys != 0 || st.OuterDrivenBatches != 0 {
			t.Fatalf("fan=%d limit=%d: the hash arm probed %d keys and ran %d batches",
				fan, limit, st.ProbeKeys, st.OuterDrivenBatches)
		}
		h, p, b := medianDur(samples[hk]), medianDur(samples[pk]), medianDur(samples[bk])
		fmt.Printf("%7d %8d %8.1f%% %11v %11v %11v %7.2fx %7.2fx %9s\n",
			limit, pairsOf[hk], 100*float64(pairsOf[hk])/float64(innerRows), h, p, b,
			float64(b.Nanoseconds())/float64(h.Nanoseconds()),
			float64(b.Nanoseconds())/float64(p.Nanoseconds()),
			chosenPath(t, cat, limit))
	}
}

// chosenPath is what the planner really picks for this shape, with every gate at its
// production setting. Reported alongside the arms because the sweep forces paths the planner
// would decline, and a table that did not say so would read as a comparison of live plans.
func chosenPath(t *testing.T, cat *Catalog, limit int) string {
	t.Helper()
	spec := outerSpec(JoinInner)
	spec.Left.Constraint = fmt.Sprintf("ClusterId <= %d", limit)
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	for range seq { //nolint:revive // drained for its stats
	}
	switch {
	case st.OuterDrivenBatches > 0:
		return "batched"
	case st.ProbeKeys > 0:
		return "probe"
	default:
		return "hash"
	}
}

func medianDur(ds []time.Duration) time.Duration {
	c := append([]time.Duration(nil), ds...)
	sort.Slice(c, func(i, j int) bool { return c[i] < c[j] })
	return c[len(c)/2]
}
