package collections

import (
	"fmt"
	"testing"
)

// Row groups (see colGroupRows): a segment's columnar accelerator is a SERIES of blocks, not one
// block covering the whole segment. These tests hold that line, because the failure mode of losing
// it is invisible: a segment-sized block is still correct, still passes every equivalence test, and
// only shows up as a query that got slower on big data. The design carried the row group from the
// start (the layout is documented as PAX, and the block comment still says "row-group"), and the
// production build silently handed the encoder every record in the segment.

// regroupSegments rebuilds every sealed segment's accelerator at an exact row-group size (see
// regroup in the benchmarks, which this wraps), for tests sweeping the group size.
func regroupSegments(tb testing.TB, c *Collection, groupRows int) {
	tb.Helper()
	regroup(tb, c, byRows(groupRows))
}

// groupFixture builds a store whose sealed segments hold many more than colGroupRows records each,
// so a segment-sized block and a row-grouped one are actually distinguishable.
func groupFixture(tb testing.TB, n int) (*Collection, uint32) {
	tb.Helper()
	// The segment size has to seal segments (a segment that never seals gets no columnar block at
	// all, and every assertion below would pass against the row fallback) while still holding many
	// more than colGroupRows records each -- which is the case row groups exist for. At ~200 bytes
	// a record, 64 KiB is a few hundred records per segment, so a segment spans several groups.
	store := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("Cpus = %d\nMemory = %d\nProcId = %d\nOwner = \"u%d\"\nMachine = \"m%04d.example.org\"",
			1+i%16, 1024+(i%64)*512, i%10, i%32, i)
		// Every 997th record carries an out-of-width ProcId, so the escaped cold-tail path is
		// exercised too -- that is the path the group size affects most.
		if i%997 == 996 {
			src = fmt.Sprintf("Cpus = %d\nMemory = %d\nProcId = %d\nOwner = \"u%d\"\nMachine = \"m%04d.example.org\"",
				1+i%16, 1024+(i%64)*512, 900000+i, i%32, i)
		}
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	if !store.BuildAndEnableSchemaScan(4000, 4) {
		tb.Fatal("BuildAndEnableSchemaScan returned false")
	}
	id, ok := store.intern.LookupID("ProcId")
	if !ok {
		tb.Fatal("ProcId not interned")
	}
	return store, id
}

// fatFixture builds a store of FAT ads -- ~4 KB of record each, the shape the byte budget is meant
// to bind on. Synthetic rather than the real OSPool corpus so this runs in CI, where the 38 MB
// testdata is not present.
func fatFixture(tb testing.TB, n int) (*Collection, uint32) {
	tb.Helper()
	store := New(Options{Shards: 1, SegmentSize: 1 << 22})
	for i := 0; i < n; i++ {
		var b []byte
		b = append(b, fmt.Sprintf("Cpus = %d\nMemory = %d\nProcId = %d\n", 1+i%16, 1024+(i%64)*512, i%10)...)
		// Enough distinct string attributes to make a record kilobytes wide, as a real slot ad's
		// several hundred attributes do.
		for j := 0; j < 60; j++ {
			b = append(b, fmt.Sprintf("Attr%02d = \"value-%d-%02d-%s\"\n", j, i, j, "padpadpadpadpadpadpadpadpadpadpadpadpadpadpadpad")...)
		}
		if i%97 == 96 {
			b = append(b, fmt.Sprintf("ProcId = %d\n", 900000+i)...) // out-of-width: escapes
		}
		if err := store.Put([]byte(fmt.Sprintf("f%d", i)), mustAdOld(tb, string(b))); err != nil {
			tb.Fatal(err)
		}
	}
	if !store.BuildAndEnableSchemaScan(4000, 4) {
		tb.Fatal("BuildAndEnableSchemaScan returned false")
	}
	id, ok := store.intern.LookupID("ProcId")
	if !ok {
		tb.Fatal("ProcId not interned")
	}
	return store, id
}

// blockRecordBytes is a block's uncompressed record footprint: the hot region plus its decompressed
// string and cold-tail regions. This is the quantity the block cache is budgeted in, so it is the
// quantity the group policy has to bound.
func blockRecordBytes(tb testing.TB, b *columnarBlock) int {
	tb.Helper()
	total := len(b.hot)
	for _, kind := range []streamKind{kindColdNum, kindStr, kindCold} {
		raw, err := b.regionRaw(kind)
		if err != nil {
			tb.Fatal(err)
		}
		total += len(raw)
	}
	return total
}

// TestRowGroupsBoundedByBytes is the regression gate against the cache cliff: under the default
// policy a fat-ad segment must be split into several groups, and no group may exceed the byte budget
// (plus one record, since a group is sealed after the record that crosses it).
//
// This is what a return to one-block-per-segment would break, and nothing else would notice: a
// segment-sized block is still CORRECT, still passes every equivalence test, and shows up only as a
// query that got slower on data larger than any test fixture.
func TestRowGroupsBoundedByBytes(t *testing.T) {
	store, _ := fatFixture(t, 1200)
	sealed, blocks, multi, biggest := 0, 0, 0, 0
	for _, sh := range store.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			sealed++
			blocks += len(cs.blocks)
			if len(cs.blocks) > 1 {
				multi++
			}
			total := 0
			for _, b := range cs.blocks {
				if b.n > colGroupMaxRows {
					t.Errorf("a block holds %d records, over the %d row backstop", b.n, colGroupMaxRows)
				}
				by := blockRecordBytes(t, b)
				if by > biggest {
					biggest = by
				}
				// One record of slack: the group seals AFTER the record that crosses the budget.
				if b.n > 1 && by > colGroupTargetBytes+by/b.n {
					t.Errorf("a block holds %d B of records (%d records), over the %d B budget",
						by, b.n, colGroupTargetBytes)
				}
				total += b.n
			}
			if total != len(cs.offs) {
				t.Errorf("blocks cover %d records but offs has %d", total, len(cs.offs))
			}
		}
	}
	if sealed == 0 {
		t.Fatal("no sealed segment carried a columnar block: the fixture never sealed, so this " +
			"test would pass against any block size at all")
	}
	if multi == 0 {
		t.Fatalf("no fat-ad segment was split into row groups (%d sealed, %d blocks, biggest %d B) -- "+
			"either the fixture is too small or the segment is one block again", sealed, blocks, biggest)
	}
	t.Logf("%d sealed segment(s), %d row groups, biggest %d B (budget %d B)", sealed, blocks, biggest, colGroupTargetBytes)
}

// TestRowGroupsBoundedByRows covers the other half of the policy: a table of SMALL ads never
// reaches the byte budget, so the row backstop is what bounds its blocks. It also pins the
// intended behaviour that small ads get MANY rows per group: at ~27 B a record, 128 rows would be a
// 3 KB block, a bound that governs nothing (measured within ~3% of whole-segment blocks, i.e.
// noise). The byte budget is what makes the same policy mean something at both ad widths.
func TestRowGroupsBoundedByRows(t *testing.T) {
	store, _ := groupFixture(t, 4000)
	sealed, blocks, biggest := 0, 0, 0
	multi := 0
	for _, sh := range store.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg == act || seg.used == 0 {
				continue
			}
			cs := seg.colblk.Load()
			if cs == nil {
				continue
			}
			sealed++
			blocks += len(cs.blocks)
			total := 0
			for _, b := range cs.blocks {
				if b.n > colGroupMaxRows {
					t.Errorf("a block covers %d records, over the %d row backstop", b.n, colGroupMaxRows)
				}
				if b.n > biggest {
					biggest = b.n
				}
				total += b.n
			}
			if len(cs.blocks) > 1 {
				multi++
			}
			// Every record in the segment is covered exactly once by the groups.
			if total != len(cs.offs) {
				t.Errorf("blocks cover %d records but offs has %d", total, len(cs.offs))
			}
		}
	}
	if sealed == 0 {
		t.Fatal("no sealed segment carried a columnar block: the fixture never sealed, so this " +
			"test would pass against any block size at all")
	}
	// Small ads are EXPECTED to fit one group per segment here; what matters is that the backstop
	// holds and that a block never grows unbounded.
	t.Logf("%d sealed segment(s), %d row groups, biggest %d records (backstop %d, %d multi-group)",
		sealed, blocks, biggest, colGroupMaxRows, multi)
}

// TestRowGroupSizeIsTransparent sweeps the group size over the REAL store aggregate path and
// requires an identical answer every time, including the extremes. The group size is a performance
// dial; if it can change an answer, the record indexing is wrong somewhere.
func TestRowGroupSizeIsTransparent(t *testing.T) {
	store, procID := groupFixture(t, 3000)

	// Ground truth from the row path, ignoring the columnar blocks entirely.
	wantCount := store.allBruteCount(procID, func(v int64) bool { return v >= 5 })
	stats, ok := store.NumStatsQuery(nil, "ProcId")
	if !ok {
		t.Fatal("NumStatsQuery declined")
	}
	wantMax, wantN, wantSum := stats.Max, stats.N, stats.IntSum
	if info := store.SchemaScanInfo(); info.CoveredSegments == 0 {
		t.Fatalf("no sealed segment is covered by the accelerator (%d sealed): the sweep below "+
			"would compare the row fallback against itself", info.SealedSegments)
	}

	for _, groupRows := range []int{1, 7, colGroupRows, 512, 1 << 20} {
		t.Run(fmt.Sprintf("group%d", groupRows), func(t *testing.T) {
			regroupSegments(t, store, groupRows)
			if got := store.schemaScanIntCount(store.schemaScan.Load().schema,
				store.schemaScan.Load().schema.byID[procID], func(v int64) bool { return v >= 5 }); got != wantCount {
				t.Errorf("count = %d, want %d", got, wantCount)
			}
			got, ok := store.NumStatsQuery(nil, "ProcId")
			if !ok {
				t.Fatal("NumStatsQuery declined")
			}
			if got.Max != wantMax || got.N != wantN || got.IntSum != wantSum {
				t.Errorf("stats = (max %v, n %d, sum %d), want (max %v, n %d, sum %d)",
					got.Max, got.N, got.IntSum, wantMax, wantN, wantSum)
			}
		})
	}
	// The escaped values must actually be in the data, or the sweep proves less than it looks.
	if wantMax < 900000 {
		t.Errorf("fixture MAX(ProcId) = %v; the out-of-width escaped values are missing, so the "+
			"cold-tail path was never exercised", wantMax)
	}
}

// TestColSegmentPersistMultiGroup round-trips a MULTI-group accelerator through the sidecar
// serialization. v1 stored exactly one block, so the block count, the per-block streams and the
// segment-wide offs array are all new framing, and a reload that mis-parses any of them hands the
// scan another record's bytes.
func TestColSegmentPersistMultiGroup(t *testing.T) {
	store, procID := groupFixture(t, 2000)
	st := store.schemaScan.Load()
	// Group by an explicit small row count so there are definitely several groups to serialize,
	// independent of whether this fixture's ads reach the byte budget.
	regroupSegments(t, store, 64)

	var cs *colSegment
	for _, sh := range store.shards {
		sh.mu.RLock()
		act := sh.act
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg != nil && seg != act && seg.used > 0 {
				if got := seg.colblk.Load(); got != nil && len(got.blocks) > 1 {
					cs = got
				}
			}
		}
	}
	if cs == nil {
		t.Fatal("no sealed segment with more than one row group: nothing multi-group to round-trip")
	}

	blob := marshalColSegment(cs, store.intern.Name)
	if blob == nil {
		t.Fatal("marshalColSegment returned nil")
	}
	got := unmarshalColSegment(blob, identityCodec{}, store.intern.Intern)
	if got == nil {
		t.Fatal("unmarshalColSegment returned nil for a multi-group segment")
	}
	if len(got.blocks) != len(cs.blocks) {
		t.Fatalf("reloaded %d row groups, want %d", len(got.blocks), len(cs.blocks))
	}
	if len(got.offs) != len(cs.offs) {
		t.Fatalf("reloaded %d offs, want %d", len(got.offs), len(cs.offs))
	}
	for i := range cs.blocks {
		a, b := cs.blocks[i], got.blocks[i]
		if a.n != b.n || a.hotStride != b.hotStride {
			t.Errorf("group %d: n/stride = %d/%d, want %d/%d", i, b.n, b.hotStride, a.n, a.hotStride)
		}
		if string(a.hot) != string(b.hot) {
			t.Errorf("group %d: hot region differs after round-trip", i)
		}
		if string(a.strComp) != string(b.strComp) || string(a.coldComp) != string(b.coldComp) ||
			string(a.coldNumComp) != string(b.coldNumComp) {
			t.Errorf("group %d: a compressed stream differs after round-trip", i)
		}
	}
	// And the reloaded blocks read the same values: scan the schema's ProcId column across both.
	idx, ok := st.schema.byID[procID]
	if !ok {
		t.Skip("ProcId not a schema field")
	}
	read := func(cs *colSegment) []int64 {
		var out []int64
		for _, b := range cs.blocks {
			if err := b.scanInt(idx, nil, func(_ int, present bool, v int64) {
				if present {
					out = append(out, v)
				}
			}); err != nil {
				t.Fatal(err)
			}
		}
		return out
	}
	origVals, gotVals := read(cs), read(got)
	if len(origVals) != len(gotVals) {
		t.Fatalf("reloaded %d column values, want %d", len(gotVals), len(origVals))
	}
	for i := range origVals {
		if origVals[i] != gotVals[i] {
			t.Fatalf("reloaded value %d = %d, want %d", i, gotVals[i], origVals[i])
		}
	}
}

// TestColSegmentRejectsGroupOffsMismatch pins the reload guard: if the blocks' record counts do not
// sum to the offs length, a scan would map a record to another record's arena offset and read the
// wrong MVCC header -- a wrong answer. The reload must refuse and let the segment rebuild.
func TestColSegmentRejectsGroupOffsMismatch(t *testing.T) {
	store, _ := groupFixture(t, 2000)
	st := store.schemaScan.Load()
	var seg *segment
	for _, sh := range store.shards {
		sh.mu.RLock()
		act := sh.act
		for _, s := range sh.segs {
			if s != nil && s != act && s.used > 0 {
				seg = s
			}
		}
		sh.mu.RUnlock()
	}
	if seg == nil {
		t.Fatal("no sealed segment")
	}
	d := seg.dict.Load()
	blocks, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, store.regionCodec(), st.schema, st.hot, byRows(64),
		func(dst, w []byte) ([]byte, bool) { return store.recordToInternedDict(d, dst, w) })
	if len(blocks) < 2 {
		t.Fatal("fixture produced a single row group; the mismatch guard would not be exercised")
	}
	// A truthful blob reloads.
	if unmarshalColSegment(marshalColSegment(&colSegment{blocks: blocks, offs: offs}, store.intern.Name),
		identityCodec{}, store.intern.Intern) == nil {
		t.Fatal("a well-formed multi-group blob failed to reload")
	}
	// Drop one record from offs: the counts no longer sum, and the reload must refuse.
	short := &colSegment{blocks: blocks, offs: offs[:len(offs)-1]}
	if got := unmarshalColSegment(marshalColSegment(short, store.intern.Name),
		identityCodec{}, store.intern.Intern); got != nil {
		t.Error("a blob whose block counts do not sum to its offs length was accepted; a scan " +
			"would read the wrong record's MVCC header")
	}
}
