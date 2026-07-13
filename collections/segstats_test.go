package collections

import (
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// --- sketch unit tests ---------------------------------------------------------

func TestBloomNoFalseNegatives(t *testing.T) {
	// A Bloom filter must never report a present key as absent (a false negative
	// would let a query skip a segment that holds a match). Exhaustively check that
	// every added key reads back as maybe-present.
	keys := make([]string, 0, 5000)
	for i := 0; i < 5000; i++ {
		keys = append(keys, fmt.Sprintf("site-%d.example.org", i))
	}
	b := newBloom(len(keys))
	for _, k := range keys {
		b.addHash(hashString(k))
	}
	for _, k := range keys {
		if !b.mayContain(hashString(k)) {
			t.Fatalf("false negative: %q added but reported absent", k)
		}
	}
	// Absent keys should mostly miss; measure the false-positive rate is sane.
	var fp int
	const probes = 20000
	for i := 0; i < probes; i++ {
		k := fmt.Sprintf("absent-%d", i)
		if b.mayContain(hashString(k)) {
			fp++
		}
	}
	if rate := float64(fp) / probes; rate > 0.05 {
		t.Fatalf("bloom false-positive rate %.3f too high (want <=0.05)", rate)
	}
}

func TestHLLEstimateAccuracy(t *testing.T) {
	for _, n := range []int{10, 100, 1000, 50000} {
		h := newHLL()
		for i := 0; i < n; i++ {
			h.addHash(hashString(fmt.Sprintf("v%d", i)))
		}
		est := h.estimate()
		relErr := math.Abs(est-float64(n)) / float64(n)
		if relErr > 0.10 {
			t.Errorf("HLL n=%d estimate=%.0f relErr=%.3f (want <=0.10)", n, est, relErr)
		}
	}
}

func TestHLLMergeIsUnion(t *testing.T) {
	// Two sketches with a 50% overlap should merge to ~1.5n distinct, not 2n.
	const n = 20000
	a, b := newHLL(), newHLL()
	for i := 0; i < n; i++ {
		a.addHash(hashString(fmt.Sprintf("v%d", i)))
	}
	for i := n / 2; i < n+n/2; i++ { // overlaps a on [n/2, n)
		b.addHash(hashString(fmt.Sprintf("v%d", i)))
	}
	a.merge(b)
	est := a.estimate()
	want := 1.5 * n
	if relErr := math.Abs(est-want) / want; relErr > 0.10 {
		t.Fatalf("merged HLL estimate=%.0f want~%.0f relErr=%.3f", est, want, relErr)
	}
}

// --- stats built from postings -------------------------------------------------

// buildStatsCollection makes a RAM collection indexed on a categorical (cat) and a
// value (val) attribute, ingests the ads, and reindexes so every segment carries
// stats. It returns the collection for direct segment inspection.
func statsCollection(t *testing.T, catAttr, valAttr string, ads []string) *Collection {
	t.Helper()
	// One shard so the whole corpus lands in a single segment/index for inspection.
	c := New(Options{Shards: 1, CategoricalAttrs: []string{catAttr}, ValueAttrs: []string{valAttr}})
	for i, s := range ads {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, s)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	c.Reindex()
	return c
}

// firstValStats / firstCatStats return the stats for the given interned attr from
// the first non-empty indexed segment (the small test corpus lands in one segment).
func firstStats(t *testing.T, c *Collection, attr string, cat bool) *segStats {
	t.Helper()
	id, ok := c.intern.LookupID(attr)
	if !ok {
		t.Fatalf("attr %q not interned", attr)
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			if cat {
				if cp := si.cat[id]; cp != nil {
					sh.mu.RUnlock()
					return &cp.stats
				}
			} else if vp := si.val[id]; vp != nil {
				sh.mu.RUnlock()
				return &vp.stats
			}
		}
		sh.mu.RUnlock()
	}
	t.Fatalf("no stats for %q found", attr)
	return nil
}

func TestSegStatsExactFromPostings(t *testing.T) {
	// Corpus: Cpus in {1,4,4,4,16,16}, Site in {"a","a","b","c","c","c"}.
	ads := []string{
		`[Site="a"; Cpus=1]`,
		`[Site="a"; Cpus=4]`,
		`[Site="b"; Cpus=4]`,
		`[Site="c"; Cpus=4]`,
		`[Site="c"; Cpus=16]`,
		`[Site="c"; Cpus=16]`,
	}
	c := statsCollection(t, "Site", "Cpus", ads)

	vs := firstStats(t, c, "Cpus", false)
	if !vs.hasRange || vs.min != 1 || vs.max != 16 {
		t.Errorf("val min/max = %v/%v hasRange=%v, want 1/16/true", vs.min, vs.max, vs.hasRange)
	}
	if vs.ndv != 3 { // {1,4,16}
		t.Errorf("val ndv = %d, want 3", vs.ndv)
	}
	if vs.covered != 6 || vs.exc != 0 {
		t.Errorf("val covered/exc = %d/%d, want 6/0", vs.covered, vs.exc)
	}
	// Cpus=4 is the heavy hitter (3 records).
	if len(vs.top) == 0 || vs.top[0].fkey != 4 || vs.top[0].count != 3 {
		t.Errorf("val top[0] = %+v, want fkey=4 count=3", vs.top[0])
	}

	cs := firstStats(t, c, "Site", true)
	if cs.ndv != 3 { // {a,b,c}
		t.Errorf("cat ndv = %d, want 3", cs.ndv)
	}
	if cs.bloom == nil || !cs.bloom.mayContain(hashString("c")) {
		t.Error("cat bloom should contain present key \"c\"")
	}
	if cs.bloom.mayContain(hashString("zzz-not-present")) {
		t.Log("cat bloom false positive on absent key (allowed, but note it)")
	}
	if cs.top[0].skey != "c" || cs.top[0].count != 3 {
		t.Errorf("cat top[0] = %+v, want skey=c count=3", cs.top[0])
	}
}

// --- skip benchmark ------------------------------------------------------------

// benchRangeSkip ingests a monotonically increasing Seq (so each 1 MiB segment
// covers a contiguous, disjoint Seq range) and runs a range query. A narrow query
// hits only a few segments; min/max stats let every other segment's indexed prefix
// be skipped without touching its postings. width is the fraction of Seq queried.
func benchRangeSkip(b *testing.B, width float64) {
	codec, cerr := NewZSTDCodec(nil)
	if cerr != nil {
		b.Fatal(cerr)
	}
	c := New(Options{Shards: 8, Codec: codec, ValueAttrs: []string{"Seq"}})
	const n = 200000
	for i := 0; i < n; i++ {
		ad, err := classad.Parse(fmt.Sprintf(
			`[ ID=%d; Seq=%d; Owner="user%d"; Cpus=%d ]`, i, i, i%1000, i%16))
		if err != nil {
			b.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			b.Fatal(err)
		}
	}
	c.Reindex()
	lo := int(float64(n) * (1 - width)) // last `width` fraction of the Seq range
	q, err := vm.Parse(fmt.Sprintf(`Seq >= %d`, lo))
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	var matches int
	for i := 0; i < b.N; i++ {
		matches = 0
		for range c.Query(q) {
			matches++
		}
	}
	b.StopTimer()
	if matches == 0 {
		b.Fatal("expected matches")
	}
	b.ReportMetric(float64(matches), "matches")
}

// Narrow: only the tail segments overlap the range; the rest skip. Wide: every
// segment overlaps, so no segment is skipped (the baseline this is measured against).
func BenchmarkRangeSkipNarrow(b *testing.B) { benchRangeSkip(b, 0.02) }
func BenchmarkRangeSkipWide(b *testing.B)   { benchRangeSkip(b, 1.0) }

// benchPlanPerSegment isolates the per-segment planning cost: given one built
// segment index, how long do the skip test + selectivity-ordered candidate build
// take (the work repeated per covering segment per query). Compare against the
// tens of microseconds a full shard query costs to see the planner's share.
func benchPlanPerSegment(b *testing.B, filter string, cats, vals []string) {
	ads := make([]string, 20000)
	for i := range ads {
		ads[i] = fmt.Sprintf(`[Site="site%d"; Cpus=%d; Memory=%d]`, i%50, i%16, (i%32)*512)
	}
	c := New(Options{Shards: 1, CategoricalAttrs: cats, ValueAttrs: vals})
	for i, s := range ads {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(&testing.T{}, s)); err != nil {
			b.Fatal(err)
		}
	}
	c.Reindex()
	var si *segIndex
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg != nil {
				if idx := seg.idx.Load(); idx != nil {
					si = idx
				}
			}
		}
	}
	if si == nil {
		b.Fatal("no segment index built")
	}
	usable := c.planIndex(mustQuery(b, filter).Probes())
	if len(usable) == 0 {
		b.Fatal("filter produced no usable probes")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if si.skipsPrefix(usable) {
			continue
		}
		_ = si.candidateOffsets(usable)
	}
}

// Single indexed probe (the common case: fast-pathed, no ordering) and a
// three-probe conjunction (ordered by selectivity).
func BenchmarkPlanPerSegmentOne(b *testing.B) {
	benchPlanPerSegment(b, `Cpus >= 8`, nil, []string{"Cpus"})
}
func BenchmarkPlanPerSegmentThree(b *testing.B) {
	benchPlanPerSegment(b, `Site == "site3" && Cpus >= 8 && Memory < 8192`,
		[]string{"Site"}, []string{"Cpus", "Memory"})
}

// Skippable: the range is entirely above the segment's max Cpus, so skipsPrefix
// returns true and candidateOffsets is never built. Measures the PURE added-cost
// of the skip decision (min/max compare) — the whole point is that it is ~ns and
// buys skipping all 20k records.
func BenchmarkPlanPerSegmentSkip(b *testing.B) {
	benchPlanPerSegment(b, `Cpus >= 1000`, nil, []string{"Cpus"})
}

// --- skip correctness ----------------------------------------------------------

func TestCanSkipValueRanges(t *testing.T) {
	ads := []string{`[Site="a"; Cpus=8]`, `[Site="a"; Cpus=12]`, `[Site="b"; Cpus=16]`}
	c := statsCollection(t, "Site", "Cpus", ads) // Cpus in [8,16]
	vs := firstStats(t, c, "Cpus", false)
	si := &segIndex{val: map[uint32]*valPostings{1: {stats: *vs}}, cat: map[uint32]*catPostings{}}
	id := func() uint32 { i, _ := c.intern.LookupID("Cpus"); return i }()
	si.val = map[uint32]*valPostings{id: {stats: *vs}}

	cases := []struct {
		op   string
		val  float64
		skip bool
	}{
		{"<", 8, true},    // nothing below the min
		{"<", 9, false},   // 8 qualifies
		{"<=", 7, true},   // nothing <=7 (min is 8)
		{">", 16, true},   // nothing above the max
		{">=", 17, true},  // nothing >=17
		{">=", 16, false}, // 16 qualifies
		{"==", 4, true},   // 4 outside [8,16]
		{"==", 12, false}, // 12 present
		{"in", 100, true}, // outside range
	}
	for _, tc := range cases {
		up := usableProbe{attrID: id, cat: false, op: tc.op, fvals: []float64{tc.val}}
		if got := si.canSkip(up); got != tc.skip {
			t.Errorf("canSkip(Cpus %s %v) = %v, want %v", tc.op, tc.val, got, tc.skip)
		}
	}
}

func TestCanSkipBlockedByException(t *testing.T) {
	// A computed (non-literal) Cpus is an exception: it re-verifies, so no range
	// skip is ever safe while an exception is present.
	ads := []string{`[Site="a"; Cpus=8]`, `[Site="a"; Cpus=RequestCpus*2]`}
	c := statsCollection(t, "Site", "Cpus", ads)
	vs := firstStats(t, c, "Cpus", false)
	if vs.exc == 0 {
		t.Fatal("expected a Cpus exception in the corpus")
	}
	id, _ := c.intern.LookupID("Cpus")
	si := &segIndex{val: map[uint32]*valPostings{id: {stats: *vs}}}
	up := usableProbe{attrID: id, op: "==", fvals: []float64{999}}
	if si.canSkip(up) {
		t.Error("must not skip a prefix with an exceptional record")
	}
}

func TestCanSkipCategoricalBloom(t *testing.T) {
	ads := []string{`[Site="alpha"; Cpus=1]`, `[Site="beta"; Cpus=1]`}
	c := statsCollection(t, "Site", "Cpus", ads)
	cs := firstStats(t, c, "Site", true)
	id, _ := c.intern.LookupID("Site")
	si := &segIndex{cat: map[uint32]*catPostings{id: {stats: *cs}}}
	// Present values never skip.
	for _, v := range []string{"alpha", "beta"} {
		up := usableProbe{attrID: id, cat: true, op: "==", svals: []string{v}}
		if si.canSkip(up) {
			t.Errorf("must not skip present value %q", v)
		}
	}
	// A value the bloom proves absent skips.
	up := usableProbe{attrID: id, cat: true, op: "==", svals: []string{"definitely-not-a-site"}}
	if !si.canSkip(up) {
		t.Error("expected skip for a bloom-absent value")
	}
	// `!=` never skips.
	up.op = "!="
	up.svals = []string{"alpha"}
	if si.canSkip(up) {
		t.Error("!= must never skip")
	}
}
