package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// The CurrentTime-folded query (vm.Query.Folded) is exact only over records that do not define
// CurrentTime, so every path that uses it must use it only in proven windows and prune with its
// probes nowhere else. These tests compare every query surface against brute-force evaluation of
// the ORIGINAL query, at the same pinned time, over stores that mix segments with and without
// records defining CurrentTime -- as undefined, a constant and an expression -- in inline, interned,
// columnarized and active segments, before and after a reopen and each rewrite.

// ctQueries all read CurrentTime unscoped. The first prunes by zone and index on CompletionDate; a
// record defining CurrentTime = 5 or = CompletionDate + 10 matches it however old it is, and the
// fixture puts those records in the OLDEST segments, which the folded probe would skip.
var ctQueries = []string{
	`CompletionDate > CurrentTime - 3*86400`,
	`CompletionDate > CurrentTime - 10*86400 && JobStatus == 4`,
	`CurrentTime - CompletionDate < 5*86400 && Owner == "u1"`,
	`CompletionDate > CurrentTime - 2*86400 || RemoteWallClockTime > 950`,
	`MY.CurrentTime - CompletionDate >= 0 && CompletionDate > CurrentTime - 12*86400`,
}

// ctAd renders fixture record i. Records are ten minutes apart, oldest first, so segments hold
// disjoint CompletionDate ranges; def, when set, is appended as the record's own CurrentTime.
func ctAd(now int64, i, n int, def string) string {
	// Age refers to CurrentTime without defining it, as job ads' periodic expressions do, so every
	// interned segment's dictionary holds the name.
	src := fmt.Sprintf("Id = %d\nCompletionDate = %d\nOwner = \"u%d\"\nJobStatus = %d\nRemoteWallClockTime = %d\nAge = CurrentTime - CompletionDate",
		i, now-int64(n-i)*600, i%5, 3+i%2, (i*37)%1000)
	if def != "" {
		src += "\nCurrentTime = " + def
	}
	return src
}

// ctDefiners are the records that define CurrentTime, by index: in the oldest segments, so a probe
// that assumed no record defines it would prune them.
var ctDefiners = map[int]string{100: "5", 140: "undefined", 180: "CompletionDate + 10", 220: "time() - 1"}

// ctRef is the brute-force answer for one query.
type ctRef struct {
	ids    []int
	sum    int64
	min    int64
	max    int64
	groups map[string][2]int64 // Owner -> {count, sum}
	dates  []float64           // matching CompletionDates, descending
}

func ctBrute(t *testing.T, c *Collection, q *vm.Query) ctRef {
	t.Helper()
	r := ctRef{groups: map[string][2]int64{}, min: math.MaxInt64, max: math.MinInt64}
	for ad := range c.Scan() {
		if !q.Matches(ad) {
			continue
		}
		id, _ := ad.EvaluateAttrInt("Id")
		r.ids = append(r.ids, int(id))
		w, _ := ad.EvaluateAttrInt("RemoteWallClockTime")
		r.sum += w
		if w < r.min {
			r.min = w
		}
		if w > r.max {
			r.max = w
		}
		o, _ := ad.EvaluateAttrString("Owner")
		g := r.groups[o]
		r.groups[o] = [2]int64{g[0] + 1, g[1] + w}
		d, _ := ad.EvaluateAttrInt("CompletionDate")
		r.dates = append(r.dates, float64(d))
	}
	sort.Ints(r.ids)
	sort.Sort(sort.Reverse(sort.Float64Slice(r.dates)))
	return r
}

// ctSurfaces checks every query surface against the brute-force answer and returns which of the
// columnar fast paths served (so a caller can require them on a proven store).
func ctSurfaces(t *testing.T, label string, c *Collection, expr string) map[string]bool {
	t.Helper()
	served := map[string]bool{}
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	ref := ctBrute(t, c, q)
	fail := func(what string, got, want any) {
		t.Errorf("%s: %s: %s = %v, want %v", label, expr, what, got, want)
	}
	var ids []int
	for ad := range c.Query(q) {
		id, _ := ad.EvaluateAttrInt("Id")
		ids = append(ids, int(id))
	}
	sort.Ints(ids)
	if fmt.Sprint(ids) != fmt.Sprint(ref.ids) {
		fail("Query ids", ids, ref.ids)
	}
	var pids []int
	for vals := range c.QueryProject(q, []string{"Id"}) {
		id, _ := vals[0].IntValue()
		pids = append(pids, int(id))
	}
	sort.Ints(pids)
	if fmt.Sprint(pids) != fmt.Sprint(ref.ids) {
		fail("QueryProject ids", pids, ref.ids)
	}
	if n, ok := c.CountQuery(q); ok {
		served["count"] = true
		if n != len(ref.ids) {
			fail("CountQuery", n, len(ref.ids))
		}
	}
	if n, ok := c.VectorEvalCount(q); ok && n != len(ref.ids) {
		fail("VectorEvalCount", n, len(ref.ids))
	}
	if n, ok := c.ColumnarEvalCount(q); ok && n != len(ref.ids) {
		fail("ColumnarEvalCount", n, len(ref.ids))
	}
	if ns, ok := c.NumStatsQuery(q, "RemoteWallClockTime"); ok {
		served["numstats"] = true
		if ns.N != len(ref.ids) || (ns.N > 0 && (ns.IntSum != ref.sum || ns.IntMin != ref.min || ns.IntMax != ref.max)) {
			fail("NumStatsQuery", fmt.Sprintf("n=%d sum=%d min=%d max=%d", ns.N, ns.IntSum, ns.IntMin, ns.IntMax),
				fmt.Sprintf("n=%d sum=%d min=%d max=%d", len(ref.ids), ref.sum, ref.min, ref.max))
		}
	}
	if gs, ok := c.GroupStatsQuery(q, "Owner", []string{"RemoteWallClockTime"}); ok {
		served["groupstats"] = true
		got := map[string][2]int64{}
		for _, g := range gs {
			o, _ := g.Values[0].StringValue()
			got[o] = [2]int64{int64(g.Count), g.Stats[0].IntSum}
		}
		if fmt.Sprint(got) != fmt.Sprint(ref.groups) {
			fail("GroupStatsQuery", got, ref.groups)
		}
	}
	if gc, ok := c.categoricalGroupCountsWhere("Owner", expr); ok {
		served["groupwhere"] = true
		want := map[string]int64{}
		for o, g := range ref.groups {
			want[o] = g[0]
		}
		for o, n := range gc {
			if n == 0 {
				delete(gc, o)
			}
		}
		if fmt.Sprint(gc) != fmt.Sprint(want) {
			fail("CategoricalGroupCountsWhere", gc, want)
		}
	}
	const k = 10
	if th, seen, ok := c.TopKOrderThreshold(q, "CompletionDate", true, k, nil); ok {
		served["topk"] = true
		if seen != len(ref.dates) {
			fail("TopKOrderThreshold seen", seen, len(ref.dates))
		} else if seen >= k && th != ref.dates[k-1] {
			fail("TopKOrderThreshold", th, ref.dates[k-1])
		}
	}
	return served
}

// ctCheckAll runs ctSurfaces for every query and, on a store with no defining record, requires the
// fast paths to have served the shapes they serve with a literal.
func ctCheckAll(t *testing.T, label string, c *Collection, clean bool) {
	t.Helper()
	for i, expr := range ctQueries {
		served := ctSurfaces(t, label, c, expr)
		if !clean || c.schemaScan.Load() == nil {
			continue
		}
		if ex := c.ExplainQuery(mustParseQ(t, expr)); !ex.CurrentTimeFolded || ex.CurrentTimeUnprovenSegments != 0 {
			t.Errorf("%s: %s: explain folded=%v unproven=%d on a proven store", label, expr,
				ex.CurrentTimeFolded, ex.CurrentTimeUnprovenSegments)
		}
		// The first two are conjunctions of numeric comparisons once folded.
		if i < 2 {
			for _, p := range []string{"count", "numstats", "groupstats", "topk"} {
				if !served[p] {
					t.Errorf("%s: %s: %s declined on a proven store", label, expr, p)
				}
			}
		}
	}
}

func mustParseQ(t *testing.T, expr string) *vm.Query {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// TestCurrentTimeFoldArchive covers an append-only store: per-segment proofs, zone and index pruning,
// the archive's GROUP BY WHERE, a reopen (sidecar-carried and walked proofs), columnarization,
// interning and merging -- with and without records that define CurrentTime.
func TestCurrentTimeFoldArchive(t *testing.T) {
	t.Parallel()
	for _, defining := range []bool{false, true} {
		t.Run(fmt.Sprintf("defining=%v", defining), func(t *testing.T) {
			t.Parallel()
			now := time.Now().Unix()
			const n = 1200
			opts := ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 14, CategoricalAttrs: []string{"Owner"},
				ValueAttrs: []string{"CompletionDate"}, ZoneAttrs: []string{"CompletionDate"}}
			a, err := CreateArchive(opts)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < n; i++ {
				def := ""
				if defining {
					def = ctDefiners[i]
					if i == n-2 {
						def = "5" // and one in the active segment
					}
				}
				if e := a.Append(mustAdOld(t, ctAd(now, i, n, def))); e != nil {
					t.Fatal(e)
				}
			}
			a.Reindex()
			ctCheckAll(t, "fresh", a.c, !defining)
			if !a.BuildAndEnableSchemaScan(n, 8) {
				t.Fatal("schema scan not enabled")
			}
			ctCheckAll(t, "columnar", a.c, !defining)

			// Reopen: sealed proofs come back from the sidecars; the reopened append target is walked.
			if e := a.Close(); e != nil {
				t.Fatal(e)
			}
			if a, err = OpenArchive(opts); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close() })
			ctCheckAll(t, "reopened", a.c, !defining)
			a.Rewrite() // interned outputs, proven by their dictionaries
			a.Reindex()
			ctCheckAll(t, "rewritten", a.c, !defining)
			a.c.ColumnarizeSealed()
			ctCheckAll(t, "columnarized", a.c, !defining)
			a.MergePass(MergeOptions{TargetSegments: 2, TriggerSegments: 3, MinMergeBytes: 1, KeepRecent: 1, MaxRun: 64})
			ctCheckAll(t, "merged", a.c, !defining)
			if defining {
				if ex := a.c.ExplainQuery(mustParseQ(t, ctQueries[0])); ex.CurrentTimeUnprovenSegments == 0 {
					t.Error("explain reports no unproven segment on a store with records defining CurrentTime")
				}
			}
		})
	}
}

// TestCurrentTimeFoldMutable covers a mutable store: the write path's flag on the active segment,
// interned compaction output, the whole-shard rule for delta records, and in-memory collections.
func TestCurrentTimeFoldMutable(t *testing.T) {
	t.Parallel()
	for _, persistent := range []bool{false, true} {
		for _, defining := range []bool{false, true} {
			t.Run(fmt.Sprintf("persistent=%v/defining=%v", persistent, defining), func(t *testing.T) {
				t.Parallel()
				now := time.Now().Unix()
				const n = 800
				opts := Options{Shards: 2, SegmentSize: 1 << 14, ValueAttrs: []string{"CompletionDate"}}
				if persistent {
					opts.Dir = t.TempDir()
				}
				c, err := Open(opts)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { c.Close() })
				for i := 0; i < n; i++ {
					def := ""
					if defining {
						def = ctDefiners[i]
					}
					if e := c.Put([]byte(fmt.Sprintf("k%04d", i)), mustAdOld(t, ctAd(now, i, n, def))); e != nil {
						t.Fatal(e)
					}
				}
				c.Reindex()
				ctCheckAll(t, "fresh", c, !defining)
				if !c.BuildAndEnableSchemaScan(n, 8) {
					t.Fatal("schema scan not enabled")
				}
				ctCheckAll(t, "columnar", c, !defining)
				c.Compact()
				c.Reindex()
				ctCheckAll(t, "compacted", c, !defining)
				// A late write that defines CurrentTime, through UpdateOld, lands in the active segment.
				if e := c.UpdateOld([]OldAdUpdate{{Key: []byte("late"), Text: ctAd(now, n, n+1, "7")}}); e != nil {
					t.Fatal(e)
				}
				ctCheckAll(t, "late-definer", c, false)
				if persistent {
					if e := c.Close(); e != nil {
						t.Fatal(e)
					}
					if c, err = Open(opts); err != nil {
						t.Fatal(err)
					}
					ctCheckAll(t, "reopened", c, false)
				}
			})
		}
	}
}

// TestCurrentTimeFoldDeltaBase: a delta record holds only what its write changed, so its ad is merged
// over a base in an older segment. A base that defines CurrentTime makes the merged ad define it even
// though nothing in the delta's own segment does, so in a store with delta records a window is proven
// only with its whole shard.
func TestCurrentTimeFoldDeltaBase(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 12, DeltaMax: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// The base defines CurrentTime = 5, so the query below is TRUE for it however old it is.
	if e := c.Put([]byte("j1"), mustAdOld(t, ctAd(now, 0, 1000, "5"))); e != nil {
		t.Fatal(e)
	}
	// Fill until the base's segment seals, then patch it: the delta lands in the fresh active segment.
	for i := 1; c.shards[0].act == c.shards[0].segs[0]; i++ {
		if e := c.Put([]byte(fmt.Sprintf("f%04d", i)), mustAdOld(t, ctAd(now, i, 100000, ""))); e != nil {
			t.Fatal(e)
		}
	}
	tx := c.Begin()
	tx.PutPatch([]byte("j1"), mustAdOld(t, strings.Replace(ctAd(now, 0, 1000, "5"), "JobStatus = 3", "JobStatus = 2", 1)),
		[]string{"JobStatus"}, false)
	if res := tx.Commit(); res.Committed != 1 {
		t.Fatalf("patch not committed: %+v", res)
	}
	seg0 := c.shards[0].segs[0]
	// Keep the base's segment unproven (as a sidecar predating the proof would leave it), so only
	// the shard-wide rule stands between the delta's window and the folded query.
	seg0.ct.provedTo.Store(0)
	seg0.ct.track.Store(0)
	seg0.ct.defines.Store(false)
	seg0.ct.failed.Store(true)
	act, delta := c.shards[0].act, false
	for off := 0; off < act.used; off += int(recTotalLen(act.data, uint32(off))) {
		delta = delta || recIsDelta(act.data, uint32(off))
	}
	if !delta || act == seg0 {
		t.Fatal("the patch was not stored as a delta record in a later segment than its base")
	}
	q := mustParseQ(t, `CompletionDate > CurrentTime - 3600 && JobStatus == 2`)
	n := 0
	for range c.Query(q) {
		n++
	}
	if want := len(ctBrute(t, c, q).ids); n != want || want != 1 {
		t.Errorf("Query = %d, brute = %d, want 1 (the patched record, whose base defines CurrentTime)", n, want)
	}
}

// TestCurrentTimeProofSidecar: the proof travels in the key index's flags field. A sidecar written
// before the field existed (flags zero) reads as not proven -- still correct -- and Reindex writes the
// proof back once a query has walked the segment; the next open adopts it without walking.
func TestCurrentTimeProofSidecar(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	const n = 600
	opts := ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 14, ZoneAttrs: []string{"CompletionDate"}}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if e := a.Append(mustAdOld(t, ctAd(now, i, n, ctDefiners[i]))); e != nil {
			t.Fatal(e)
		}
	}
	a.Reindex()
	sealed := func(a *Archive) []*segment {
		sh := a.c.shards[0]
		var out []*segment
		for _, s := range sh.segs {
			if s != nil && s != sh.act && s.keyIdx.Load() != nil {
				out = append(out, s)
			}
		}
		return out
	}
	var defSeg, cleanSeg *segment
	for _, s := range sealed(a) {
		f := s.ctKIDXFlags(s.used)
		if f == kidxCTChecked|kidxCTDefines && defSeg == nil {
			defSeg = s
		}
		if f == kidxCTChecked {
			cleanSeg = s
		}
	}
	if defSeg == nil || cleanSeg == nil {
		t.Fatalf("seal did not record both proofs (defining %v, clean %v)", defSeg != nil, cleanSeg != nil)
	}
	// Rewrite every sidecar as an older build would have: the same sections, flags zero.
	paths := map[string]bool{}
	for _, s := range sealed(a) {
		paths[snapshotPath(s)] = true
	}
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	for p := range paths {
		stripKIDXFlags(t, p)
	}
	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	for _, s := range sealed(a) {
		if s.ct.persisted.Load() || s.ctKnown(s.used) {
			t.Fatalf("a sidecar without the proof was adopted as proven: %s", s.path)
		}
	}
	ctSurfaces(t, "legacy", a.c, ctQueries[0]) // walks the segments (ensureCTProofs)
	for _, s := range sealed(a) {
		if !s.ctKnown(s.used) {
			t.Fatalf("segment %s not proven after a CurrentTime query", s.path)
		}
	}
	a.Reindex() // writes the walked proofs back
	if e := a.Close(); e != nil {
		t.Fatal(e)
	}
	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	defs := 0
	for _, s := range sealed(a) {
		if !s.ct.persisted.Load() {
			t.Errorf("segment %s: proof not persisted by Reindex", s.path)
		}
		if s.ct.defines.Load() {
			defs++
		}
	}
	if defs == 0 {
		t.Error("no segment adopted a defining proof")
	}
	ctSurfaces(t, "upgraded", a.c, ctQueries[0])
}

// stripKIDXFlags zeroes the key index flags in the sidecar at path, as a build predating them wrote it.
func stripKIDXFlags(t *testing.T, path string) {
	t.Helper()
	data, closer, err := mapFile(path)
	if err != nil {
		t.Fatal(err)
	}
	attr, key, col, zone, ok := splitSegmentSidecar(data)
	if !ok {
		t.Fatalf("%s: not a sidecar container", path)
	}
	ki, err := parseKeyIndex(key)
	if err != nil {
		t.Fatal(err)
	}
	v5, _ := splitSidecarV5(data)
	used, dictRec := 0, uint32(0)
	if _, _, _, _, u, d, ok := splitSegmentSidecarV4(data); ok {
		used, dictRec = u, d
	}
	nk := append([]byte(nil), key...)
	nk[6], nk[7] = 0, 0
	binary.LittleEndian.PutUint32(nk[len(nk)-4:], crc32.ChecksumIEEE(nk[:len(nk)-4]))
	out := buildSegmentSidecar(append([]byte(nil), attr...), nk, append([]byte(nil), col...), append([]byte(nil), zone...),
		used, dictRec, v5.colRec, append([]byte(nil), v5.damage...))
	_ = closer()
	if strings.HasSuffix(path, ".idx") && ki.flags == 0 {
		t.Fatalf("%s: sidecar had no proof to strip", path)
	}
	if e := writeFileAtomic(path, out); e != nil {
		t.Fatal(e)
	}
}

// TestNamesCurrentTime: the write path's check finds the attribute in any case, in inline and
// globally interned ads, and is not fooled by the name appearing as a value or inside a nested record.
func TestNamesCurrentTime(t *testing.T) {
	t.Parallel()
	for _, inline := range []bool{false, true} {
		opts := Options{Shards: 1}
		if inline {
			opts.Dir = t.TempDir()
		}
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		for src, want := range map[string]bool{
			`[A = 1; CurrentTime = 5]`:                     true,
			`[A = 1; currentTIME = undefined]`:             true,
			`[CURRENTTIME = A + 1; A = 1]`:                 true,
			`[A = "CurrentTime"; B = 11]`:                  false,
			`[A = [CurrentTime = 5]; B = 1]`:               false,
			`[CurrentTimeX = 1; XCurrentTime = 2]`:         false,
			`[A = 11; B = "currenttime"; C = CurrentTime]`: false,
		} {
			w := c.encodeAd(mustAd(t, src).AST())
			if got := c.namesCurrentTime(w); got != want {
				t.Errorf("inline=%v %s: namesCurrentTime = %v, want %v", inline, src, got, want)
			}
		}
		c.Close()
	}
}

// TestConstantQueryProjection: a projected scan builds a matched record from the columns when the
// query reads no attribute, since the record cannot change a constant's answer. It must still be the
// constant TRUE: folding CurrentTime makes `CurrentTime < 0` the constant false, and a constant false
// projected from the columns returned every record.
func TestConstantQueryProjection(t *testing.T) {
	t.Parallel()
	now := time.Now().Unix()
	const n = 400
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		if e := c.Put([]byte(fmt.Sprintf("k%04d", i)), mustAdOld(t, ctAd(now, i, n, ""))); e != nil {
			t.Fatal(e)
		}
	}
	c.Reindex()
	if !c.BuildAndEnableSchemaScan(n, 8) {
		t.Fatal("schema scan not enabled")
	}
	c.ColumnarizeSealed()
	for expr, want := range map[string]int{`1 > 2`: 0, `CurrentTime < 0`: 0, `CurrentTime > 0`: n, `true`: n} {
		got := 0
		for range c.QueryRawWire(mustParseQ(t, expr), []string{"Id", "Owner"}, false) {
			got++
		}
		if got != want {
			t.Errorf("QueryRawWire(%s) = %d rows, want %d", expr, got, want)
		}
	}
}
