package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"maps"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Negative numbers in records written before the encoder folded `-5` into a signed literal.
//
// Those records hold unary minus over the literal's magnitude, and they are not rewritten: a sealed
// segment keeps its bytes. Every reader that serves VALUES -- the value index, the column cells, the
// group keys -- has to read that node as the number it is, or the record becomes an exception the
// index hands to every probe, and a cell that makes a column aggregate decline.

// TestOldFormNegativeFixture pins the fixture itself: the old-form records hold unary minus over a
// literal, LiteralValue does not read them, and the evaluator reads each as its number.
func TestOldFormNegativeFixture(t *testing.T) {
	f := &negFixture{n: 40, negZ: true}
	for i := 0; i < f.n; i++ {
		raw := f.record(i)
		node, ok := wire.Ad(raw).LookupByName("X")
		if !ok {
			continue
		}
		ad, err := wire.DecodeInline(raw)
		if err != nil {
			t.Fatal(err)
		}
		v := classad.FromAST(ad).EvaluateAttr("X")
		if _, lit := wire.LiteralValue(node); lit {
			continue
		}
		num, ok := wire.NumberValue(node)
		if !ok {
			t.Fatalf("record %d: X (% x) is not a number to NumberValue", i, node)
		}
		switch num.Kind {
		case wire.LitInt:
			if got, err := v.IntValue(); err != nil || got != num.Int {
				t.Errorf("record %d: X evaluates to %v, NumberValue says int %d", i, v, num.Int)
			}
		case wire.LitReal:
			if got, err := v.RealValue(); err != nil || got != num.Real || math.Signbit(got) != math.Signbit(num.Real) {
				t.Errorf("record %d: X evaluates to %v, NumberValue says real %v", i, v, num.Real)
			}
		}
	}
	if !slices.Equal(oldNegNode(int64(1)), []byte{0x09, 0x00, 0x04, 0x02}) {
		t.Fatalf("oldNegNode(1) = % x, not the pre-fold bytes of -1", oldNegNode(int64(1)))
	}
}

// negQueries are the probes the index must answer exactly as a plain scan does.
var negQueries = []string{
	`X < 0`,
	`X <= -1`,
	`X == -5`,
	`X == 0`,
	`X =?= 0`,
	`X =?= -5`,
	`X != -5`,
	`X >= -2.5 && X < 0`,
	`X == -2.5`,
	`X < -1000`,
	`X == -9007199254740993`,
	`X == -9007199254740992`,
	`X < -9007199254740992`,
	`X <= -9223372036854775807`,
	`X > -9223372036854775807 && X < -10`,
	`X > 0`,
	`X is undefined`,
	`X isnt undefined`,
	`!(X >= 0)`,
}

func plainNs(tb testing.TB, a *Archive, q *vm.Query) []int64 {
	tb.Helper()
	var out []int64
	for ad := range a.c.Scan() {
		if q.Matches(ad) {
			n, _ := ad.EvaluateAttrInt("N")
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

func indexedNs(tb testing.TB, a *Archive, q *vm.Query, st *ScanStats) []int64 {
	tb.Helper()
	var out []int64
	for row := range a.QueryProjectStats(q, []string{"N"}, st) {
		n, err := row[0].IntValue()
		if err != nil {
			tb.Fatalf("projected N is %v", row[0])
		}
		out = append(out, n)
	}
	slices.Sort(out)
	return out
}

// valExceptions sums the value index's exception count for attr over every sealed sidecar.
func valExceptions(tb testing.TB, a *Archive, attr string) (exc uint64, sidecars int) {
	tb.Helper()
	c := a.c
	spec := c.spec.Load()
	id, ok := uint32(0), false
	for _, vid := range spec.valIDs {
		name, has := spec.names[vid]
		if !spec.inline {
			name, has = c.intern.Name(vid)
		}
		if has && strings.EqualFold(name, attr) {
			id, ok = vid, true
		}
	}
	if !ok {
		tb.Fatalf("%s is not value-indexed", attr)
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if mm := seg.msidx.Load(); mm != nil {
				sidecars++
				if st := mm.valStats[id]; st != nil {
					exc += st.exc
				}
			}
		}
		sh.mu.RUnlock()
	}
	return exc, sidecars
}

// TestValueIndexPostsOldFormNegatives: a value index posts an old-form negative as its number, so
// the exception set holds only the genuine expressions, and every probe -- large ints, -0, reals,
// mixed signs, presence and negation -- answers exactly what the plain scan does.
func TestValueIndexPostsOldFormNegatives(t *testing.T) {
	f := &negFixture{n: 2000, expr: true, negZ: true}
	a := newNegArchive(t, f, ArchiveOptions{ValueAttrs: []string{"X"}})
	defer a.Close()
	exc, sidecars := valExceptions(t, a, "X")
	if sidecars < 10 || f.oldNeg < 500 {
		t.Fatalf("fixture: %d sidecars, %d old-form records", sidecars, f.oldNeg)
	}
	if exc != uint64(f.exprs) {
		t.Errorf("value index holds %d exceptions for X; the fixture has %d expressions and %d old-form negatives",
			exc, f.exprs, f.oldNeg)
	}
	for _, expr := range negQueries {
		q := mustQuery(t, expr)
		want := plainNs(t, a, q)
		var st ScanStats
		got := indexedNs(t, a, q, &st)
		if !slices.Equal(got, want) {
			t.Errorf("%s: indexed query returned %d rows, plain %d", expr, len(got), len(want))
		}
	}
	// A selective probe is planned through the index with the candidates it matches, not with every
	// old-form negative in tow.
	q := mustQuery(t, `X < -1000`)
	want := plainNs(t, a, q)
	ex := a.ExplainQuery(q)
	if ex.Plan != "indexed" {
		t.Fatalf("X < -1000 planned %q (%s)", ex.Plan, ex.Reason)
	}
	var st ScanStats
	indexedNs(t, a, q, &st)
	if limit := len(want) + f.exprs + 80; st.IndexCandidates > limit {
		t.Errorf("X < -1000: the index handed over %d candidates for %d matches (%d expressions)",
			st.IndexCandidates, len(want), f.exprs)
	}
	if ex.EstCandidates > int64(4*(len(want)+f.exprs)) {
		t.Errorf("X < -1000: estimated %d candidates for %d matches", ex.EstCandidates, len(want))
	}
}

// TestAutoIndexProfileCountsOldFormNegatives: the auto-indexer's attribute profile counts an old-form
// negative as a number, so an attribute that holds them is not taken for an expression-valued one.
func TestAutoIndexProfileCountsOldFormNegatives(t *testing.T) {
	f := &negFixture{n: 400, negZ: true}
	a := newNegArchive(t, f, ArchiveOptions{})
	defer a.Close()
	p := a.c.profileAttrs(1 << 16)["x"]
	if p == nil {
		t.Fatal("no profile for X")
	}
	if p.other != 0 || p.numCount != p.present {
		t.Errorf("X profile: present=%d numeric=%d other=%d; every value is a number", p.present, p.numCount, p.other)
	}
}

// ageSidecars rewrites every sealed sidecar as the builder before this change wrote it: an old-form
// negative moved from its posting into the exception set, the stats recomputed, and the layout
// version 9. It is the state an archive opened after an upgrade starts from.
func ageSidecars(tb testing.TB, a *Archive) int {
	tb.Helper()
	c := a.c
	spec := c.spec.Load()
	aged := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := slices.Clone(sh.segs)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.msidx.Load() == nil {
				continue
			}
			si := buildSegIndex(c, seg, seg.used, spec)
			dict := seg.dict.Load()
			var buf []byte
			for off := 0; off < seg.used; {
				o := uint32(off)
				total := recTotalLen(seg.data, o)
				if total == 0 {
					break
				}
				w, err := c.recordWireIn(seg, seg.data, o, buf)
				if err == nil {
					buf = w
					for _, id := range spec.valIDs {
						node, ok := spec.attrNode(wire.Ad(w), id, dict)
						if !ok {
							continue
						}
						if _, lit := wire.LiteralValue(node); lit {
							continue
						}
						if num, ok := wire.NumberValue(node); ok {
							vp := si.val[id]
							k := float64(num.Int)
							if num.Kind == wire.LitReal {
								k = num.Real
							}
							if bm := vp.post[k]; bm != nil {
								bm.Remove(o)
								if bm.IsEmpty() {
									delete(vp.post, k)
								}
							}
							vp.posted.Remove(o)
							vp.exc.Add(o)
						}
					}
				}
				off += int(total)
			}
			for _, vp := range si.val {
				vp.finishStats()
			}
			attr, err := buildSidecarIndex(si)
			if err != nil {
				tb.Fatal(err)
			}
			binary.LittleEndian.PutUint16(attr[4:], 9)
			binary.LittleEndian.PutUint32(attr[len(attr)-4:], crc32.Checksum(attr[:len(attr)-4], crcTable))
			container := buildSegmentSidecar(attr, c.segKeyIndex(seg), c.colBlobPreserve(seg),
				buildZoneBlob(c.zonesForSeg(sh, seg), c.intern.Name), seg.used, seg.dictRecOff(), seg.colRecOff(), encodeDamage(seg.damaged()))
			if !c.installSidecar(sh, seg, snapshotPath(seg), container) {
				tb.Fatal("installSidecar failed")
			}
			if mm := seg.msidx.Load(); mm == nil || mm.version != 9 {
				tb.Fatal("aged sidecar not adopted as v9")
			}
			aged++
		}
	}
	return aged
}

// TestStaleSidecarIsSupersetAndBackfilled: a sidecar from before this change -- its old-form negatives
// in the exception set -- still answers every probe exactly (exceptions are candidates the query
// re-verifies), is read again after a reopen, is reported as pending by the index backfill when it
// holds value exceptions, and is rebuilt by it into one that posts them.
func TestStaleSidecarIsSupersetAndBackfilled(t *testing.T) {
	f := &negFixture{n: 2000, expr: true, negZ: true}
	opts := ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 12, ValueAttrs: []string{"X"}}
	a := newNegArchive(t, f, opts)
	aged := ageSidecars(t, a)
	exc, _ := valExceptions(t, a, "X")
	if exc != uint64(f.exprs+f.oldNeg) {
		t.Fatalf("aged sidecars hold %d exceptions, want %d expressions + %d old-form", exc, f.exprs, f.oldNeg)
	}

	check := func(label string, wantMoreCandidates bool) {
		t.Helper()
		for _, expr := range negQueries {
			q := mustQuery(t, expr)
			want := plainNs(t, a, q)
			var st ScanStats
			if got := indexedNs(t, a, q, &st); !slices.Equal(got, want) {
				t.Errorf("%s: %s returned %d rows, plain %d", label, expr, len(got), len(want))
			}
		}
		var st ScanStats
		indexedNs(t, a, mustQuery(t, `X < -1000`), &st)
		if stale := st.IndexCandidates > f.oldNeg; stale != wantMoreCandidates {
			t.Errorf("%s: X < -1000 drew %d index candidates (%d old-form records)", label, st.IndexCandidates, f.oldNeg)
		}
	}
	check("stale", true)
	if segs, _ := a.IndexBackfillPending(); segs != aged {
		t.Errorf("backfill reports %d segments pending, %d sidecars are stale", segs, aged)
	}

	// Reopen: the v9 sidecars are adopted as they are, not rejected and rebuilt.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	var err error
	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if segs, _ := a.IndexBackfillPending(); segs != aged {
		t.Errorf("after reopen, backfill reports %d segments pending, want %d", segs, aged)
	}
	check("reopened stale", true)

	res := a.BackfillIndexes(0)
	if res.Rebuilt != aged || res.PendingSegments != 0 {
		t.Errorf("backfill rebuilt %d of %d, %d still pending", res.Rebuilt, aged, res.PendingSegments)
	}
	if exc, _ := valExceptions(t, a, "X"); exc != uint64(f.exprs) {
		t.Errorf("after backfill, %d exceptions for %d expressions", exc, f.exprs)
	}
	check("backfilled", false)
}

// TestSidecarVersionWindow: the reader takes v9 (same layout, older postings) and the current version,
// and rejects anything outside -- an unknown layout is rebuilt, never guessed at. A v9 sidecar with
// no value exception has nothing a rebuild would change.
func TestSidecarVersionWindow(t *testing.T) {
	si := &segIndex{all: roaring.BitmapOf(0, 1), cat: map[uint32]*catPostings{}, val: map[uint32]*valPostings{}}
	vp := &valPostings{post: map[float64]*roaring.Bitmap{}, exc: roaring.New(), posted: roaring.New()}
	addOffset(vp.post, -1, 0)
	vp.posted.Add(0)
	si.val[3] = vp
	vp.finishStats()
	b, err := buildSidecarIndex(si)
	if err != nil {
		t.Fatal(err)
	}
	for v, ok := range map[uint16]bool{8: false, 9: true, sidecarVersion: true, sidecarVersion + 1: false} {
		binary.LittleEndian.PutUint16(b[4:], v)
		mm, perr := parseMmapSidecar(b)
		if (perr == nil) != ok {
			t.Errorf("version %d: parse error %v, want accepted=%v", v, perr, ok)
			continue
		}
		if ok && mm.stalePostings() {
			t.Errorf("version %d without value exceptions reads as stale", v)
		}
	}
	vp.exc.Add(1)
	vp.finishStats()
	if b, err = buildSidecarIndex(si); err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint16(b[4:], 9)
	if mm, err := parseMmapSidecar(b); err != nil || !mm.stalePostings() {
		t.Errorf("v9 with a value exception: stale=%v err=%v, want stale", err == nil && mm.stalePostings(), err)
	}
	binary.LittleEndian.PutUint16(b[4:], sidecarVersion)
	if mm, err := parseMmapSidecar(b); err != nil || mm.stalePostings() {
		t.Errorf("current version with a value exception reads as stale (err %v)", err)
	}
}

// TestColumnsServeOldFormNegatives: with the columnar accelerator built over old-form negatives -- in a
// segment's own columnar payload, and in a sidecar block beside an unrewritten one -- the column
// aggregates read them as numbers instead of declining the whole query, every answer is the plain
// evaluation's, and the records still read back exactly as they were written.
func TestColumnsServeOldFormNegatives(t *testing.T) {
	for _, native := range []bool{true, false} {
		for _, negZ := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/negzero=%v", native, negZ), func(t *testing.T) {
				f := &negFixture{n: 1500, negZ: negZ}
				a := newNegArchive(t, f, ArchiveOptions{})
				defer a.Close()
				c := a.c
				rendered := func() map[int64]string {
					out := map[int64]string{}
					for ad := range c.Scan() {
						n, _ := ad.EvaluateAttrInt("N")
						if e, ok := ad.Lookup("X"); ok {
							out[n] = e.String()
						}
					}
					return out
				}
				before := rendered()
				if !native {
					c.colBudget = -1 // no rewrite: every sealed segment gets a sidecar block
				}
				// Required: X mixes ints and reals, so no one kind clears the presence floor.
				a.AddSchemaRequireAttrs("X")
				if !a.BuildAndEnableSchemaScan(1<<16, 8) {
					t.Fatal("schema scan did not enable")
				}
				for c.ColumnarizeSealed() > 0 {
				}
				columnarized := 0
				for _, sh := range c.shards {
					sh.mu.RLock()
					for _, seg := range sh.segs {
						if seg != nil && seg.columnarized() {
							columnarized++
						}
					}
					sh.mu.RUnlock()
				}
				if native != (columnarized > 10) {
					t.Fatalf("fixture: %d segments columnarized", columnarized)
				}
				if after := rendered(); !maps.Equal(before, after) {
					t.Fatalf("columnarizing changed how X reads back (%d vs %d records)", len(before), len(after))
				}
				// Old-form records in the open segment too, which the aggregates read by row.
				for i := 0; i < 30; i++ {
					appendWire(t, a, inlineAd([]rawAttr{{"N", wire.AppendIntNode(nil, int64(10_000+i))}, {"X", oldNegNode(int64(i%4 + 1))}}))
				}

				// A record without X in a columnar-native segment goes to the row path from the
				// per-record evaluator (ColumnarEvalCount) whatever this change does, once per reference
				// to X, so that tier is allowed those fallbacks and no more.
				lacking := 0
				for ad := range c.Scan() {
					if _, ok := ad.Lookup("X"); !ok {
						lacking++
					}
				}
				fb := ColumnarFallbacks()
				evalFB := int64(0)
				// No literal past 2^53 here: the predicate tiers compare in float64 and decline one by
				// design. The values past it are still in the data, and in these answers.
				for _, expr := range []string{`X < 0`, `X >= -2.5 && X < 0`, `X == 0`, `X <= -1000`, `X > 0`, ``} {
					var q *vm.Query
					if expr == "" {
						expr = "<all>"
					} else {
						q = mustQuery(t, expr)
					}
					rows := matchingAds(t, a, q)
					if q != nil {
						if n, ok := c.CountQuery(q); !ok || n != len(rows) {
							t.Errorf("CountQuery(%s) = %d, %v; plain %d", expr, n, ok, len(rows))
						}
						if n, ok := c.VectorEvalCount(mustQuery(t, expr+` || N < 0`)); !ok || n != len(rows) {
							t.Errorf("VectorEvalCount(%s) = %d, %v; plain %d", expr, n, ok, len(rows))
						}
						before := ColumnarFallbacks()
						if n, ok := c.ColumnarEvalCount(q); !ok || n != len(rows) {
							t.Errorf("ColumnarEvalCount(%s) = %d, %v; plain %d", expr, n, ok, len(rows))
						}
						d := ColumnarFallbacks() - before
						if d > int64(lacking*strings.Count(expr, "X")) {
							t.Errorf("ColumnarEvalCount(%s) handed %d records to the row path; %d lack X", expr, d, lacking)
						}
						evalFB += d
					}
					want := plainNumStats(t, rows, "X")
					got, ok := c.NumStatsQuery(q, "X")
					if !ok || got.N != want.N || got.AnyReal != want.AnyReal || (got.N > 0 && (got.Min != want.Min || got.Max != want.Max ||
						(!got.AnyReal && (got.IntMin != want.IntMin || got.IntMax != want.IntMax)))) {
						t.Errorf("NumStatsQuery(%s) = %+v, %v; plain %+v", expr, got, ok, want)
					}
					if len(rows) > 10 {
						th, seen, ok := c.TopKOrderThreshold(q, "X", false, 10, nil)
						if !ok || seen != want.N {
							t.Errorf("TopKOrderThreshold(%s) = %v seen %d, %v; plain seen %d", expr, th, seen, ok, want.N)
						}
					}
				}
				absent := 0
				for ad := range c.Scan() {
					if _, ok := ad.Lookup("X"); !ok {
						absent++
					}
				}
				if n, ok := c.PresenceCountQuery(mustQuery(t, `X is undefined`)); !ok || n != absent {
					t.Errorf("PresenceCountQuery(X is undefined) = %d, %v; plain %d", n, ok, absent)
				}
				for _, expr := range []string{`N >= 0`, `X < 0`} {
					q := mustQuery(t, expr)
					want := map[string]int{}
					for _, ad := range matchingAds(t, a, q) {
						want[ad.EvaluateAttr("X").String()]++
					}
					gs, ok := c.GroupStatsQuery(q, "X", nil)
					if !ok {
						t.Errorf("GroupStatsQuery(%s) by X declined", expr)
						continue
					}
					got := map[string]int{}
					for _, g := range gs {
						got[g.Values[0].String()] += g.Count
					}
					if !maps.Equal(got, want) {
						t.Errorf("GroupStatsQuery(%s) by X = %v; plain %v", expr, got, want)
					}
				}
				if n := ColumnarFallbacks() - fb - evalFB; n != 0 {
					t.Errorf("the columns handed %d records to the row path", n)
				}
			})
		}
	}
}

// plainNumStats is NumStats computed from evaluated records, the reference's rules: integers and reals
// contribute, anything else does not.
func plainNumStats(tb testing.TB, rows []*classad.ClassAd, attr string) NumStats {
	tb.Helper()
	s := NumStats{Min: math.Inf(1), Max: math.Inf(-1), IntMin: math.MaxInt64, IntMax: math.MinInt64}
	for _, ad := range rows {
		v := ad.EvaluateAttr(attr)
		if i, err := v.IntValue(); err == nil && v.IsInteger() {
			s.N++
			if i < s.IntMin {
				s.IntMin = i
			}
			if i > s.IntMax {
				s.IntMax = i
			}
			s.Min, s.Max = math.Min(s.Min, float64(i)), math.Max(s.Max, float64(i))
			continue
		}
		if r, err := v.RealValue(); err == nil && v.IsReal() {
			s.N++
			s.AnyReal = true
			s.Min, s.Max = math.Min(s.Min, r), math.Max(s.Max, r)
		}
	}
	return s
}

// matchingAds evaluates q against every record of an unfiltered scan: the ground truth.
func matchingAds(tb testing.TB, a *Archive, q *vm.Query) []*classad.ClassAd {
	tb.Helper()
	var out []*classad.ClassAd
	for ad := range a.c.Scan() {
		if q == nil || q.Matches(ad) {
			out = append(out, ad)
		}
	}
	return out
}

// TestGroupColumnExceptionsReadOldFormNegatives: a record holding part of a group schema's attributes is
// an exception the vector scan reads from the base cold tail, and an old-form negative there is a number,
// not an expression that hands the whole block back to per-record evaluation. The group's own members
// hold old-form negatives too, which the group schema must still see as int columns.
//
// Groups are derived from attributes that are present together, so a group has no partial holder when
// it is derived; the exceptions come from records written afterwards, as they do in a live table.
func TestGroupColumnExceptionsReadOldFormNegatives(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: 4, GroupStabilityRuns: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	put := func(from, to int, partial bool) {
		for i := from; i < to; i++ {
			attrs := []rawAttr{{"ClusterId", wire.AppendIntNode(nil, int64(i))}}
			switch {
			case i%4 == 0:
				attrs = append(attrs, rawAttr{"GA", oldNegNode(int64(i%7 + 1))}, rawAttr{"GB", oldNegNode(int64(i%3 + 1))})
			case i%4 == 1 && partial: // GA without GB: an exception to the group column
				attrs = append(attrs, rawAttr{"GA", oldNegNode(int64(i%5 + 1))})
			}
			putWire(t, c, []byte(fmt.Sprintf("%d.0", i)), inlineAd(attrs))
		}
	}
	put(0, 2000, false)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	put(2000, 5000, true)
	// The committed schema and groups, applied to the segments sealed since: rewritten in place where
	// the budget allows, given sidecar blocks where it does not. Not a re-derivation, which would
	// regroup around the partial holders.
	for c.ColumnarizeSealed() > 0 {
	}
	st := c.schemaScan.Load()
	c.coverSealedSegments(st.schema, st.hot)
	exceptions := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cs := seg.colblk.Load(); cs != nil {
				for _, g := range cs.groups {
					if len(g.ids) < 2 {
						continue
					}
					for _, gb := range g.blocks {
						if gb != nil {
							exceptions += len(gb.exceptions)
						}
					}
				}
			}
		}
		sh.mu.RUnlock()
	}
	if exceptions == 0 {
		t.Fatal("fixture: no exceptions to a two-attribute group")
	}
	queries := []string{`GA < -3`, `GA < 0 && ClusterId < 4000`, `GA > -2 || GB == -1`, `GA > GB`}
	fb := ColumnarFallbacks()
	for _, q := range queries {
		want := 0
		for ad := range c.Scan() {
			if mustQuery(t, q).Matches(ad) {
				want++
			}
		}
		got, ok := c.VectorEvalCount(mustQuery(t, q))
		if !ok || got != want {
			t.Errorf("VectorEvalCount(%s) = %d, %v; plain %d", q, got, ok, want)
		}
		if split := lastVecSplit.Load(); split.vecBlocks == 0 || split.scopeBlocks != 0 {
			t.Errorf("%s: %d blocks vectorized, %d handed to per-record evaluation", q, split.vecBlocks, split.scopeBlocks)
		}
	}
	if d := ColumnarFallbacks() - fb; d != 0 {
		t.Errorf("the columns handed %d records to the row path", d)
	}
}

// TestSchemaDerivationCountsOldFormNegatives: an attribute holding only old-form negatives gets a
// SIGNED int column, sized from its values, as the same attribute written today does. Counted as no
// storable kind, it cleared no presence floor and got no column at all; mixed with non-negative
// literals, it got an unsigned one that every negative escaped.
func TestSchemaDerivationCountsOldFormNegatives(t *testing.T) {
	c := New(Options{Shards: 1})
	var allNeg, mixed [][]byte
	for i := 0; i < 40; i++ {
		allNeg = append(allNeg, oldFormInternedAd(c, map[string][]byte{"ExitCode": oldNegNode(int64(i%5 + 1))}))
		node := wire.AppendIntNode(nil, int64(i%3))
		if i%2 == 0 {
			node = oldNegNode(int64(200 + i))
		}
		mixed = append(mixed, oldFormInternedAd(c, map[string][]byte{"Offset": node}))
	}
	assertIntField(t, buildAdSchema(allNeg, negAdSchemaOpts), c, "ExitCode", false)
	s := buildAdSchema(mixed, negAdSchemaOpts)
	assertIntField(t, s, c, "Offset", false)
	if f := s.fields[s.byID[mustIntern(t, c, "Offset")]]; f.width < 2 {
		t.Errorf("Offset column is %d bytes wide; its old-form values reach -239", f.width)
	}
	// A HOT column is re-fitted to nearly all of its sampled values, and those include the old-form ones.
	s2, _ := refitHotIntWidths(s, mixed, []int{s.byID[mustIntern(t, c, "Offset")]}, hotIntFit)
	if f := s2.fields[s2.byID[mustIntern(t, c, "Offset")]]; f.unsigned || f.width < 2 {
		t.Errorf("hot Offset column re-fitted to unsigned=%v width %d; its old-form values reach -239", f.unsigned, f.width)
	}
}
