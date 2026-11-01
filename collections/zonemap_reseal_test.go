package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// resealZoneOpts is an archive whose segments seal every few dozen records, zoned on Ts (every
// record holds it, ascending) and Late (only the second half of the records hold it).
func resealZoneOpts(dir string, internAtSeal bool) ArchiveOptions {
	return ArchiveOptions{Dir: dir, SegmentSize: 1 << 12, InternAtSeal: internAtSeal,
		ValueAttrs: []string{"Ts"}, ZoneAttrs: []string{"Ts", "Late"}}
}

func appendResealZoneAds(t *testing.T, a *Archive, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		src := fmt.Sprintf(`[Ts=%d; G=%d; Pad="%040d"]`, 1000+i, i%4, i)
		if i >= n/2 {
			src = fmt.Sprintf(`[Ts=%d; G=%d; Late=%d; Pad="%040d"]`, 1000+i, i%4, i, i)
		}
		ad, err := classad.Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
}

// checkSealedZones requires every sealed segment of a to carry a zone map that soundly bounds its
// records -- each range at least as wide as one recomputed from the records, and full only where
// every record holds a number -- and, when exact, equal to the recomputed one. It returns how many
// sealed segments there are and how many of them are interned.
func checkSealedZones(t *testing.T, label string, a *Archive, exact bool) (sealed, interned int) {
	t.Helper()
	c := a.c
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 {
			continue
		}
		sealed++
		if seg.dict.Load() != nil {
			interned++
		}
		if seg.zones == nil {
			t.Errorf("%s: sealed segment %d (interned=%v) has no zone map", label, seg.id, seg.dict.Load() != nil)
			continue
		}
		want := computeSegZones(c, seg, seg.used, sh.zoneAttrs, sh.zoneInline, sh.sealsName)
		for id, z := range seg.zones {
			r, ok := want[id]
			name, _ := c.intern.Name(id)
			switch {
			case !ok:
				t.Errorf("%s: segment %d zones %s, which its records give no range for", label, seg.id, name)
			case z.Min > r.Min || z.Max < r.Max:
				t.Errorf("%s: segment %d zone %s [%v,%v] does not cover its records' [%v,%v]", label, seg.id, name, z.Min, z.Max, r.Min, r.Max)
			case z.Full && !r.Full:
				t.Errorf("%s: segment %d zone %s claims every record holds a number; not all do", label, seg.id, name)
			}
		}
		if exact && fmt.Sprint(seg.zones) != fmt.Sprint(want) {
			t.Errorf("%s: segment %d zones %v, recomputed from its records %v", label, seg.id, seg.zones, want)
		}
	}
	return sealed, interned
}

// checkZonePrunes requires a query for the newest records to skip the older sealed segments on
// their zone maps, and to return the rows it should. On Ts every segment but the newest goes; on
// Late only the ones holding it can, since a segment none of whose records holds Late has no zone
// for it, which prunes nothing.
func checkZonePrunes(t *testing.T, label string, a *Archive, n int) {
	t.Helper()
	for _, tc := range []struct {
		expr   string
		minOut func(st ScanStats) int
	}{
		{fmt.Sprintf(`Ts >= %d`, 1000+n-5), func(st ScanStats) int { return st.SegmentsTotal - 2 }},
		{fmt.Sprintf(`Late >= %d`, n-5), func(ScanStats) int { return 1 }},
	} {
		q, err := vm.Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		var st ScanStats
		got := 0
		for range a.QueryRawProjectedStats(q, []string{"Ts"}, false, false, &st) {
			got++
		}
		if got != 5 {
			t.Errorf("%s: %s returned %d rows, want 5", label, tc.expr, got)
		}
		if st.SegmentsPruned < tc.minOut(st) {
			t.Errorf("%s: %s zone-pruned %d of %d segments, want at least %d; stats %+v",
				label, tc.expr, st.SegmentsPruned, st.SegmentsTotal, tc.minOut(st), st)
		}
	}
}

// reopenResealArchive reopens an archive and reports how many zone maps the open recomputed from
// records rather than adopting them from the sidecars.
func reopenResealArchive(t *testing.T, opts ArchiveOptions) (*Archive, int) {
	t.Helper()
	var diag OpenIndexDiag
	prev := OpenIndexDiagHook
	OpenIndexDiagHook = func(d OpenIndexDiag) { diag = d }
	defer func() { OpenIndexDiagHook = prev }()
	a, err := OpenArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	return a, diag.Timing.ZoneRecomputed
}

// TestInternAtSealKeepsZoneMap: a segment interned as it seals is rewritten, and the rewrite
// carries the zone map the segment sealed with -- so it prunes, and ages out under retention, at
// once rather than after the next reopen -- and persists it, so a reopen adopts it without
// decoding the records again.
//
// Not parallel: OpenIndexDiagHook is process-wide.
func TestInternAtSealKeepsZoneMap(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 600
	opts := resealZoneOpts(t.TempDir(), true)
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	appendResealZoneAds(t, a, n)
	sealed, interned := checkSealedZones(t, "interned at seal", a, true)
	if sealed < 10 || interned != sealed {
		t.Fatalf("%d of %d sealed segments interned; the fixture should intern every one of many", interned, sealed)
	}
	checkZonePrunes(t, "interned at seal", a, n)

	// Retention ages a segment out by its zone map's newest Ts: with the map in place the oldest
	// segments go now, not after a reopen.
	before := a.c.Len()
	a.c.SetRetention(Retention{MaxAgeAttr: "Ts", MaxAge: 300})
	dropped, err := a.Rotate(float64(1000 + n))
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 || a.c.Len() >= before {
		t.Errorf("Rotate dropped %d segments (%d -> %d records); the segments older than the cutoff should go", dropped, before, a.c.Len())
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a, recomputed := reopenResealArchive(t, opts)
	defer a.Close()
	if recomputed != 0 {
		t.Errorf("reopen recomputed %d zone maps instead of adopting them from the sidecars", recomputed)
	}
	checkSealedZones(t, "reopened", a, true)
	checkZonePrunes(t, "reopened", a, n)
}

// TestResealAndMergeKeepZoneMap: the other rewrites of an archive's sealed segments -- a Rewrite
// that reseals every segment in place, and a merge of adjacent ones -- carry a zone map across
// too: the source's for a reseal, the union of the sources' for a merge, leaving out an attribute
// one source has no zone for.
//
// Not parallel: OpenIndexDiagHook is process-wide.
func TestResealAndMergeKeepZoneMap(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 600
	opts := resealZoneOpts(t.TempDir(), false)
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	appendResealZoneAds(t, a, n)
	if _, interned := checkSealedZones(t, "sealed", a, true); interned != 0 {
		t.Fatalf("%d segments interned before the rewrite; the test would not see the rewrite", interned)
	}

	// A segment with no zone map -- as one interned at seal by an earlier version was left -- gets
	// one recomputed from its records when it is rewritten.
	sh := a.c.shards[0]
	sh.mu.Lock()
	for _, seg := range sh.segs {
		if seg != nil && seg != sh.act && seg.used > 0 {
			seg.zones = nil
			break
		}
	}
	sh.mu.Unlock()
	a.c.Rewrite()
	sealed, interned := checkSealedZones(t, "resealed", a, true)
	if interned != sealed {
		t.Fatalf("Rewrite interned %d of %d sealed segments", interned, sealed)
	}
	checkZonePrunes(t, "resealed", a, n)

	// Merge runs of adjacent sealed segments, one of them straddling the first record holding
	// Late: that source has no zone for Late, so neither may the merged segment.
	a.c.maintMu.Lock()
	merges := 0
	for start := 0; ; start += 3 {
		sh.mu.RLock()
		var run []*segment
		for _, seg := range sh.segs[min(start, len(sh.segs)):] {
			if seg == nil || seg == sh.act || seg.used == 0 {
				continue
			}
			if run = append(run, seg); len(run) == 3 {
				break
			}
		}
		sh.mu.RUnlock()
		if len(run) < 3 {
			break
		}
		if a.c.mergeSegments(sh, run) {
			merges++
		}
	}
	a.c.maintMu.Unlock()
	if merges < 3 {
		t.Fatalf("only %d merges happened", merges)
	}
	a.c.reindexAfterCompaction()
	checkSealedZones(t, "merged", a, false)
	checkZonePrunes(t, "merged", a, n)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	a, recomputed := reopenResealArchive(t, opts)
	defer a.Close()
	if recomputed != 0 {
		t.Errorf("reopen recomputed %d zone maps instead of adopting them from the sidecars", recomputed)
	}
	checkSealedZones(t, "merged reopened", a, false)
	checkZonePrunes(t, "merged reopened", a, n)
}

// TestRewriteZonesUnion: the zone map of a segment rebuilt from several sources is, per attribute,
// the widest of their ranges, full only where every source's is, and absent wherever any source
// has none -- a source with no zone for an attribute says nothing about its records' values.
func TestRewriteZonesUnion(t *testing.T) {
	t.Parallel()
	sh := &shard{appendOnly: true, zoneAttrs: []zoneAttr{{name: "A", id: 1}, {name: "B", id: 2}, {name: "C", id: 3}}}
	src := func(zones map[uint32]zoneRange) *segment { return &segment{zones: zones} }
	srcs := []*segment{
		src(map[uint32]zoneRange{1: {Min: 5, Max: 9, Full: true}, 2: {Min: 0, Max: 1, Full: true}, 3: {Min: 7, Max: 7, Full: true}}),
		src(map[uint32]zoneRange{1: {Min: 2, Max: 6, Full: true}, 2: {Min: -3, Max: 0, Full: false}}),
		src(map[uint32]zoneRange{1: {Min: 8, Max: 12, Full: true}, 2: {Min: 4, Max: 4, Full: true}, 3: {Min: 1, Max: 2, Full: true}}),
	}
	got := (&Collection{}).rewriteZones(sh, srcs, nil)
	want := map[uint32]zoneRange{1: {Min: 2, Max: 12, Full: true}, 2: {Min: -3, Max: 4, Full: false}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("rewriteZones = %v, want %v", got, want)
	}
	// A single source carries across unchanged.
	if got := (&Collection{}).rewriteZones(sh, srcs[:1], nil); fmt.Sprint(got) != fmt.Sprint(srcs[0].zones) {
		t.Errorf("rewriteZones of one source = %v, want its own %v", got, srcs[0].zones)
	}
	// A shard that keeps no zone maps gets none.
	if got := (&Collection{}).rewriteZones(&shard{zoneAttrs: sh.zoneAttrs}, srcs, nil); got != nil {
		t.Errorf("rewriteZones on a mutable shard = %v, want none", got)
	}
}
