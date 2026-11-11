package collections

// DOES ZONE-MAP TILING SUBSUME AN INDEX NESTED-LOOP JOIN? The measurement, not an
// execution change.
//
// A hash join holds one side whole, and the limit on that side is a hard failure rather
// than a worse plan, so the shape that motivates all of this -- a history archive joined
// to an epoch archive on (ClusterId, ProcId) -- has no plan at all once the epoch side
// outgrows the limit.
//
// Every sealed segment already carries a numeric [min,max] per zone-mapped attribute. For
// an equi-join key, an outer segment and an inner segment can only produce a matching pair
// if those two ranges OVERLAP: a key outside the inner range matches nothing there, which
// is the same argument zonePrune already makes for a query constant. So the join could run
// as a sequence of segment-pair joins, each hashing only the rows of the inner segments
// that one outer segment overlaps.
//
// Whether that is a plan or a fantasy is one number: INNER SEGMENTS PER OUTER SEGMENT, as a
// distribution rather than a mean. Two archives both written in ClusterId order should give
// a near-diagonal overlap matrix and a per-tile working set of a segment or two. A key that
// does not run with insertion order gives a dense matrix, every tile covers the whole inner
// side, and tiling is a full cross product dressed as an optimization. The degradation
// between those is measured here directly by writing the same rows in a shuffled order.
//
// The second number is READ AMPLIFICATION: the sum over tiles of the inner rows each tile
// reads, over the inner side's row count. A diagonal matrix reads the inner side about once;
// a dense one reads it once per outer segment, which is the cost tiling has to not have.
//
// Two facts bound what any answer here can mean, and are asserted rather than assumed:
//
//   - zone maps are NUMERIC. zonePrune/zoneMayMatch bail out on a non-numeric probe value,
//     so a string join key (Owner, GlobalJobId) gets no tiling whatever its distribution.
//     The string arm below exists to show that as a measurement rather than a claim.
//   - the ACTIVE segment has no zone map (sealZones runs at a seal). It overlaps every tile
//     and must be read by every one of them, so it is reported separately: with one tile it
//     is nothing, and with a tile per outer segment it is the whole floor.

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

const (
	// tileClusters is the key space. Both archives are written over the same clusters, so
	// every outer key has inner matches and no arm can be cheap by matching nothing.
	tileClusters = 12_000
	tileProcs    = 4 // history rows per cluster
	tileEpochs   = 3 // epoch rows per history row
	// tileSegBytes is chosen for a SEGMENT COUNT: the question is how the overlap matrix
	// behaves across many segments, and a fixture with a handful of them answers a
	// different question. The counts are asserted in tileFixture.
	tileSegBytes = 96 << 10
)

// tileOrder is how far a side's clusters may be displaced from key order as they are
// appended. It is a WINDOW, not a flag, because the realistic case is neither: history and
// epoch rows are written as jobs finish, and jobs finish near but not in submit order, so
// the key runs with insertion order to within some displacement. The window is that
// displacement -- clusters are appended in key order after shuffling each consecutive block
// of tileOrder clusters -- and sweeping it is what shows where tiling stops working.
type tileOrder int

const (
	tileSorted tileOrder = 0  // insertion order IS key order
	tileFull   tileOrder = -1 // uniformly shuffled over the whole key space
)

func (o tileOrder) String() string {
	switch o {
	case tileSorted:
		return "sorted"
	case tileFull:
		return "shuffled"
	}
	return fmt.Sprintf("jitter%d", int(o))
}

// clusterOrder is the append order tileOrder describes.
func (o tileOrder) clusterOrder(seed int64) []int {
	order := make([]int, tileClusters)
	for i := range order {
		order[i] = i
	}
	if o == tileSorted {
		return order
	}
	rng := rand.New(rand.NewSource(seed))
	w := int(o)
	if o == tileFull || w >= len(order) {
		rng.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
		return order
	}
	for lo := 0; lo < len(order); lo += w {
		hi := min(lo+w, len(order))
		blk := order[lo:hi]
		rng.Shuffle(len(blk), func(i, j int) { blk[i], blk[j] = blk[j], blk[i] })
	}
	return order
}

// tileSeg is one segment reduced to what tiling reasons about: the key's span and how many
// rows lie inside it.
//
// The span is recorded twice on purpose. zmin/zmax is the PERSISTED zone map, which is what
// a plan can prune an inner segment with and which the active segment does not have. omin/omax
// is the span actually observed in the records, which is what a tile knows about its OUTER
// segment: the outer side is being read regardless, so the keys it is about to stream bound
// themselves, with or without a zone map. Tiling therefore needs a persisted zone map on the
// inner side only. For a sealed segment the two must agree, and that is asserted.
type tileSeg struct {
	id         int
	zmin, zmax float64
	zoned      bool
	omin, omax float64
	observed   bool
	rows       int
	active     bool
}

// tileSegments reports every segment of an archive with its zone span for attr, the span its
// records actually hold, and its visible record count.
func tileSegments(tb testing.TB, a *Archive, attr string) []tileSeg {
	tb.Helper()
	c := a.c
	if len(c.shards) != 1 {
		tb.Fatalf("expected a single-shard archive, got %d shards", len(c.shards))
	}
	sh := c.shards[0]
	id, ok := c.intern.LookupID(attr)
	if !ok {
		tb.Fatalf("%q was never interned; it is not an indexed attribute of this archive", attr)
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	out := make([]tileSeg, 0, len(wins))
	for i, w := range wins {
		ts := tileSeg{id: i, active: w.active}
		if z, has := w.zones[id]; has {
			ts.zmin, ts.zmax, ts.zoned = z.Min, z.Max, true
		}
		ts.rows, ts.omin, ts.omax, ts.observed = walkWindowKey(tb, c, w, s0, attr, id, sh.zoneInline)
		if ts.zoned && ts.observed && (ts.zmin != ts.omin || ts.zmax != ts.omax) {
			tb.Fatalf("segment %d: zone map says [%g,%g] but its records hold [%g,%g]",
				i, ts.zmin, ts.zmax, ts.omin, ts.omax)
		}
		out = append(out, ts)
	}
	return out
}

// walkWindowKey counts one window's visible, non-marker records and returns the numeric span
// of attr across them. It resolves the attribute the same three ways computeSegZones does,
// since a segment may be interned, inline-keyed or id-keyed.
func walkWindowKey(tb testing.TB, c *Collection, w segWindow, s0 uint64, attr string, id uint32, inline bool) (rows int, lo, hi float64, seen bool) {
	tb.Helper()
	dict := w.seg.dict.Load()
	var buf []byte
	for off := 0; off < w.used; {
		o := uint32(off)
		if end, ok := w.dmg.skip(o); ok {
			off = int(end)
			continue
		}
		total := recTotalLen(w.data, o)
		if total == 0 {
			break
		}
		if recIsMarker(w.data, o) || !w.visible(o, s0) {
			off += int(total)
			continue
		}
		rows++
		wr, err := c.recordWireIn(w.seg, w.data, o, buf)
		if err != nil {
			tb.Fatal(err)
		}
		buf = wr
		ad := wire.Ad(wr)
		var node []byte
		var ok bool
		switch {
		case dict != nil:
			if sid, has := dict.segIDOf(attr); has {
				node, ok = ad.Lookup(uint32(sid)) //idspace:translate a dictionary-keyed record is keyed by the segID
			}
		case inline:
			node, ok = ad.LookupByName(attr)
		default:
			node, ok = ad.Lookup(id)
		}
		if ok {
			if f, cell := zoneValue(node); cell == zoneNumber {
				if !seen {
					lo, hi, seen = f, f, true
				} else {
					lo, hi = math.Min(lo, f), math.Max(hi, f)
				}
			}
		}
		off += int(total)
	}
	return rows, lo, hi, seen
}

// tileOverlaps reports, for each outer segment, which inner segments its key span cannot
// rule out.
//
// The test is interval overlap, and it is not a new rule: the tiled join's per-tile probe
// is `key >= outer.min && key <= outer.max`, and zoneMayMatch keeps an inner segment for
// `>=` when inner.Max >= outer.min and for `<=` when inner.Min <= outer.max. An inner
// segment with no zone map -- the active one -- is kept by every tile, because nothing is
// known about it.
func tileOverlaps(outer, inner []tileSeg) [][]int {
	out := make([][]int, len(outer))
	for i, o := range outer {
		var hits []int
		for j, in := range inner {
			if !in.zoned || !o.observed {
				hits = append(hits, j) // nothing known: cannot be ruled out
				continue
			}
			if in.zmax >= o.omin && in.zmin <= o.omax {
				hits = append(hits, j)
			}
		}
		out[i] = hits
	}
	return out
}

// tileStats is one arm's answer.
type tileStats struct {
	outerSegs, innerSegs int
	innerActiveRows      int
	perOuter             []int // inner segments per outer segment, sorted
	innerRowsPerTile     []int // inner rows one tile reads, sorted
	outerRowsPerTile     []int // outer rows one tile holds, sorted
	hashPerTile          []int // min of the two: what the tile must actually hold
	innerRowsTotal       int
	innerRowsRead        int // summed over tiles
}

func summarizeTiles(outer, inner []tileSeg, hits [][]int) tileStats {
	st := tileStats{outerSegs: len(outer), innerSegs: len(inner)}
	for _, in := range inner {
		st.innerRowsTotal += in.rows
		if !in.zoned {
			st.innerActiveRows += in.rows
		}
	}
	for i, o := range outer {
		rows := 0
		for _, j := range hits[i] {
			rows += inner[j].rows
		}
		st.perOuter = append(st.perOuter, len(hits[i]))
		st.innerRowsPerTile = append(st.innerRowsPerTile, rows)
		st.outerRowsPerTile = append(st.outerRowsPerTile, o.rows)
		st.hashPerTile = append(st.hashPerTile, min(rows, o.rows))
		st.innerRowsRead += rows
	}
	sort.Ints(st.perOuter)
	sort.Ints(st.innerRowsPerTile)
	sort.Ints(st.outerRowsPerTile)
	sort.Ints(st.hashPerTile)
	return st
}

func (st tileStats) amplification() float64 {
	if st.innerRowsTotal == 0 {
		return math.Inf(1)
	}
	return float64(st.innerRowsRead) / float64(st.innerRowsTotal)
}

func tilePct(sorted []int, p float64) int {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(p*float64(len(sorted)-1))]
}

func tileMax(sorted []int) int {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[len(sorted)-1]
}

// buildTileArchive writes one side. kind "history" writes tileProcs rows per cluster;
// "epoch" writes tileEpochs rows per history row, which is what makes the inner side the
// one a hash join cannot hold.
func buildTileArchive(tb testing.TB, dir, kind string, ord tileOrder, seed int64) *Archive {
	tb.Helper()
	a, err := CreateArchive(ArchiveOptions{
		Dir:         dir,
		SegmentSize: tileSegBytes,
		ValueAttrs:  []string{"ClusterId", "ProcId"},
		// Owner is the string arm's key. A string attribute cannot be a ValueAttr (a value
		// index does not serve one), and it gets no zone map either way -- which is the
		// point the string arm makes.
		CategoricalAttrs: []string{"Owner"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { a.Close() })
	order := ord.clusterOrder(seed)
	pad := strings.Repeat("x", 48)
	reps := 1
	if kind == "epoch" {
		reps = tileEpochs
	}
	for _, cl := range order {
		for p := 0; p < tileProcs; p++ {
			for r := 0; r < reps; r++ {
				ad := classad.New()
				ad.InsertAttr("ClusterId", int64(cl))
				ad.InsertAttr("ProcId", int64(p))
				ad.InsertAttr("RunInstanceId", int64(r))
				ad.InsertAttrString("Owner", fmt.Sprintf("u%d", cl%997))
				ad.InsertAttrString("Pad", pad)
				if err := a.Append(ad); err != nil {
					tb.Fatal(err)
				}
			}
		}
	}
	if err := a.Flush(); err != nil {
		tb.Fatal(err)
	}
	return a
}

// tileFixture is one (outer order, inner order) pair, built once.
type tileFixture struct {
	outer, inner *Archive
	outerSegs    []tileSeg
	innerSegs    []tileSeg
}

func buildTileFixture(tb testing.TB, outerOrd, innerOrd tileOrder) *tileFixture {
	tb.Helper()
	dir := tb.TempDir()
	f := &tileFixture{
		outer: buildTileArchive(tb, dir+"/history", "history", outerOrd, 0x11),
		inner: buildTileArchive(tb, dir+"/epoch", "epoch", innerOrd, 0x22),
	}
	f.outerSegs = tileSegments(tb, f.outer, "ClusterId")
	f.innerSegs = tileSegments(tb, f.inner, "ClusterId")
	// A fixture with a handful of segments measures nothing about a tail, and one whose
	// segments carry no zone map measures nothing at all.
	if len(f.outerSegs) < 20 || len(f.innerSegs) < 60 {
		tb.Fatalf("fixture too small to show a distribution: %d outer, %d inner segments",
			len(f.outerSegs), len(f.innerSegs))
	}
	zoned := 0
	for _, s := range f.innerSegs {
		if s.zoned {
			zoned++
		}
	}
	if zoned < len(f.innerSegs)-1 {
		tb.Fatalf("%d of %d inner segments carry a ClusterId zone map; only the active one may lack it",
			zoned, len(f.innerSegs))
	}
	return f
}

// TestJoinTileOverlapDistribution is the headline measurement: inner segments per outer
// segment, and the inner rows each tile reads, as the join key's correlation with insertion
// order is weakened on one side and then both.
func TestJoinTileOverlapDistribution(t *testing.T) {
	if testing.Short() {
		t.Skip("builds many multi-hundred-segment archives")
	}
	cases := []struct{ outer, inner tileOrder }{
		{tileSorted, tileSorted},
		{tileSorted, 100},
		{tileSorted, 1000},
		{100, 100},
		{1000, 1000},
		{tileSorted, tileFull},
		{tileFull, tileSorted},
		{tileFull, tileFull},
	}
	for _, cs := range cases {
		name := fmt.Sprintf("outer-%s/inner-%s", cs.outer, cs.inner)
		t.Run(name, func(t *testing.T) {
			f := buildTileFixture(t, cs.outer, cs.inner)
			st := summarizeTiles(f.outerSegs, f.innerSegs, tileOverlaps(f.outerSegs, f.innerSegs))
			t.Logf("segments: outer=%d inner=%d; inner active rows=%d (unzoned, read by every tile)",
				st.outerSegs, st.innerSegs, st.innerActiveRows)
			t.Logf("inner segments per outer segment: min=%d p50=%d p90=%d p99=%d max=%d",
				st.perOuter[0], tilePct(st.perOuter, 0.50), tilePct(st.perOuter, 0.90),
				tilePct(st.perOuter, 0.99), tileMax(st.perOuter))
			t.Logf("inner rows per tile: min=%d p50=%d p90=%d p99=%d max=%d (inner total=%d)",
				st.innerRowsPerTile[0], tilePct(st.innerRowsPerTile, 0.50),
				tilePct(st.innerRowsPerTile, 0.90), tilePct(st.innerRowsPerTile, 0.99),
				tileMax(st.innerRowsPerTile), st.innerRowsTotal)
			t.Logf("rows one tile must HOLD (the smaller side): p50=%d p99=%d max=%d",
				tilePct(st.hashPerTile, 0.50), tilePct(st.hashPerTile, 0.99), tileMax(st.hashPerTile))
			t.Logf("inner read amplification: %.1fx", st.amplification())
			if st.perOuter[0] < 1 {
				t.Errorf("an outer segment overlapped no inner segment; both sides cover the same keys")
			}
			// The fully correlated case is the one the verdict rests on, so its shape is
			// asserted and not only logged. The bounds are loose -- the measured values are 5
			// inner segments per outer at every percentile and 1.6x -- because this pins that
			// the matrix is DIAGONAL, not the exact fixture. A one-sided or inverted overlap
			// test widens it immediately.
			if cs.outer == tileSorted && cs.inner == tileSorted {
				if p99 := tilePct(st.perOuter, 0.99); p99 > 15 {
					t.Errorf("both sides insertion-ordered, yet the p99 tile covers %d of %d inner segments; the matrix is not diagonal",
						p99, st.innerSegs)
				}
				if amp := st.amplification(); amp > 5 {
					t.Errorf("both sides insertion-ordered, yet the inner side is read %.1f times; tiling is not paying", amp)
				}
			}
			// A full shuffle is the other end, and it must look like one: an arm that
			// "degraded" to a tight matrix would mean the shuffle never happened.
			if cs.outer == tileFull && cs.inner == tileFull {
				if st.perOuter[0] < st.innerSegs {
					t.Errorf("both sides shuffled, yet some tile covers only %d of %d inner segments; the shuffle did not take",
						st.perOuter[0], st.innerSegs)
				}
			}
			// Every outer segment's span must be known, zone map or not: that is what makes
			// the outer side's active segment free, and it is the claim tileOverlaps rests on.
			for _, o := range f.outerSegs {
				if !o.observed {
					t.Errorf("outer segment %d holds no numeric key at all", o.id)
				}
			}
		})
	}
}

// TestJoinTileIsLossless checks the thing a distribution cannot: that restricting each outer
// segment to the inner segments its key span overlaps loses no match.
//
// It runs the tiled read by hand over the public query surface -- for each outer segment, one
// range probe for that segment's key span against the inner archive -- and compares what the
// tiles reach against a full scan of the inner side. Every key of the fixture is compared,
// not an aggregate count, because a boundary that drops or duplicates one row averages away
// in a count.
func TestJoinTileIsLossless(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two multi-hundred-segment archives")
	}
	f := buildTileFixture(t, tileSorted, tileSorted)

	// The truth: every inner row, keyed, from a full scan.
	all, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	innerAll := map[[2]int64]int{}
	for ad := range f.inner.Query(all) {
		innerAll[tileKeyOf(t, ad)]++
	}
	if len(innerAll) == 0 {
		t.Fatal("the inner side scanned empty; the fixture is measuring nothing")
	}

	// The tiled read. A key may fall in more than one outer segment's span, so the per-key
	// counts are collected per tile and the maximum taken: a tile that reaches a key at all
	// must see every row of it, which is what losslessness means here.
	tiled := map[[2]int64]int{}
	segmentReads := 0
	for _, o := range f.outerSegs {
		if !o.observed {
			t.Fatalf("outer segment %d has no key span", o.id)
		}
		q, perr := vm.Parse(fmt.Sprintf("ClusterId >= %d && ClusterId <= %d", int64(o.omin), int64(o.omax)))
		if perr != nil {
			t.Fatal(perr)
		}
		ex := f.inner.ExplainQuery(q)
		kept := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments
		if kept == 0 {
			t.Fatalf("tile %d kept no inner segment at all", o.id)
		}
		segmentReads += kept
		per := map[[2]int64]int{}
		for ad := range f.inner.Query(q) {
			per[tileKeyOf(t, ad)]++
		}
		for k, n := range per {
			if n > tiled[k] {
				tiled[k] = n
			}
		}
	}
	t.Logf("%d outer tiles read %d inner segments in total; the inner side has %d",
		len(f.outerSegs), segmentReads, len(f.innerSegs))

	if len(tiled) != len(innerAll) {
		t.Fatalf("the tiles reached %d distinct keys, the full scan %d", len(tiled), len(innerAll))
	}
	missing, wrong := 0, 0
	for k, want := range innerAll {
		got, ok := tiled[k]
		if !ok {
			missing++
			if missing <= 5 {
				t.Errorf("key (%d,%d) is in the inner side but no tile reached it", k[0], k[1])
			}
			continue
		}
		if got != want {
			wrong++
			if wrong <= 5 {
				t.Errorf("key (%d,%d): a tile saw %d rows, the full scan %d", k[0], k[1], got, want)
			}
		}
	}
	if missing != 0 || wrong != 0 {
		t.Fatalf("%d keys unreached and %d with the wrong row count, of %d", missing, wrong, len(innerAll))
	}
}

func tileKeyOf(t *testing.T, ad *classad.ClassAd) [2]int64 {
	t.Helper()
	cl, ok := ad.EvaluateAttrInt("ClusterId")
	if !ok {
		t.Fatal("a row carried no integer ClusterId")
	}
	pr, ok := ad.EvaluateAttrInt("ProcId")
	if !ok {
		t.Fatal("a row carried no integer ProcId")
	}
	return [2]int64{cl, pr}
}

// TestJoinTileStringKeyGetsNoTiling is the limit of the whole idea, measured rather than
// asserted from the doc comment: a zone map holds a numeric span, so a probe whose value is
// a string prunes nothing. A string-keyed join tiles into one tile covering everything.
func TestJoinTileStringKeyGetsNoTiling(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a multi-hundred-segment archive")
	}
	f := buildTileFixture(t, tileSorted, tileSorted)
	q, err := vm.Parse(`Owner == "u5"`)
	if err != nil {
		t.Fatal(err)
	}
	ex := f.inner.ExplainQuery(q)
	total := ex.IndexSegments + ex.IndexSkippedSegments + ex.UnindexedSegments + ex.ZonePrunedSegments
	if total == 0 {
		t.Fatal("the string probe explained no segments at all")
	}
	t.Logf("Owner == \"u5\": %d segments, %d zone-pruned", total, ex.ZonePrunedSegments)
	if ex.ZonePrunedSegments != 0 {
		t.Errorf("a string equality pruned %d segments; zone maps are numeric and must prune none",
			ex.ZonePrunedSegments)
	}
	// And the same archive's numeric key does prune, so the zero above is the string's
	// doing and not a fixture with no zone maps.
	qn, err := vm.Parse("ClusterId == 17")
	if err != nil {
		t.Fatal(err)
	}
	exn := f.inner.ExplainQuery(qn)
	t.Logf("ClusterId == 17: %d zone-pruned of %d", exn.ZonePrunedSegments,
		exn.IndexSegments+exn.IndexSkippedSegments+exn.UnindexedSegments+exn.ZonePrunedSegments)
	if exn.ZonePrunedSegments == 0 {
		t.Fatal("the numeric key pruned nothing either; the fixture has no usable zone maps")
	}
}
