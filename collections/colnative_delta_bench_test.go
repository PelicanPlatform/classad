package collections

import (
	"fmt"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// WHAT DO DELTA RECORDS COST A COLUMNARIZED SEGMENT?
//
// Columnarization moves every schema'd attribute out of each record into per-segment columns. It
// filters MARKERS and nothing else (colnative_build.go), so a superseded record gets a columnar
// row exactly like a live one -- and a DELTA record holds two attributes where a whole ad holds
// forty-five, so its row is mostly absent fields.
//
// The seal-collapse invariant means every delta in a sealed segment is dead, so this is not a
// correctness problem and TestDeltaVsColumnarization (which is a correctness test) passes. It is a
// DENSITY problem, and nothing measured it: every performance run in this package and in the
// scheddsync benchmarks has had columnarization off.
//
// This measures the two configurations over the same workload and reports what a segment costs
// with delta records on versus off, after columnarizing.
func TestColumnarDensityWithDeltas(t *testing.T) {
	const keys = 1500
	const rounds = 10

	build := func(deltaMax int) (segBytes int64, used int64, recs, deltas, dead int) {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 18, DeltaMax: deltaMax})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		for i := 0; i < keys; i++ {
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("k%d.0", i)), jobAd(i, map[string]int64{"N": 0}))
			if r := tx.Commit(); r.Conflicted() {
				t.Fatal("seed conflicted")
			}
		}
		for r := 1; r <= rounds; r++ {
			for i := 0; i < keys; i++ {
				patch := classad.New()
				patch.InsertAttr("N", int64(r))
				tx := c.Begin()
				tx.PatchAttrs([]byte(fmt.Sprintf("k%d.0", i)), patch, nil)
				if res := tx.Commit(); res.Conflicted() {
					t.Fatal("patch conflicted")
				}
			}
		}
		if deltaMax > 0 {
			if d, _ := c.DeltaStats(); d == 0 {
				t.Fatal("delta mode on but no deltas stored: the comparison is vacuous")
			}
		}
		if !c.BuildAndEnableSchemaScan(2000, 32) {
			t.Skip("schema scan did not enable; nothing to measure")
		}
		for pass := 0; pass < 3; pass++ {
			c.ColumnarizeSealed()
		}
		// Count what ended up in the sealed segments.
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg == nil || seg.used == 0 || seg == sh.act {
					continue
				}
				segBytes += int64(len(seg.data))
				used += int64(seg.used)
				for off := uint32(0); off < uint32(seg.used); {
					tl := recTotalLen(seg.data, off)
					if tl == 0 || off+tl > uint32(seg.used) {
						break
					}
					if !recIsMarker(seg.data, off) {
						recs++
						if recIsDelta(seg.data, off) {
							deltas++
						}
						if recSuperseded(seg.data, off) != seqMax {
							dead++
						}
					}
					off += tl
				}
			}
			sh.mu.RUnlock()
		}
		return
	}

	// ColumnarDeltasDropped is package-cumulative across the whole test run, so measure the delta.
	droppedBefore := ColumnarDeltasDropped()

	offBytes, offUsed, offRecs, offDeltas, offDead := build(0)
	t.Logf("DeltaMax=0   sealed: %6.2f MB arena, %6.2f MB used, %6d records (%d delta, %d dead)",
		float64(offBytes)/(1<<20), float64(offUsed)/(1<<20), offRecs, offDeltas, offDead)

	onBytes, onUsed, onRecs, onDeltas, onDead := build(16)
	t.Logf("DeltaMax=16  sealed: %6.2f MB arena, %6.2f MB used, %6d records (%d delta, %d dead)",
		float64(onBytes)/(1<<20), float64(onUsed)/(1<<20), onRecs, onDeltas, onDead)

	t.Logf("used bytes: %.2f MB -> %.2f MB (%+.0f%%)  records: %d -> %d (%+.0f%%)  dropped by columnarization: %d",
		float64(offUsed)/(1<<20), float64(onUsed)/(1<<20),
		100*(float64(onUsed)/float64(offUsed)-1), offRecs, onRecs,
		100*(float64(onRecs)/float64(offRecs)-1), ColumnarDeltasDropped()-droppedBefore)

	// The requirement this exists to enforce: a columnarized segment holds NO delta records. A
	// delta is the attributes one write changed, so its columnar row would be almost entirely
	// absent fields -- and by the time its segment seals it is superseded garbage anyway.
	if onDeltas != 0 {
		t.Fatalf("%d delta records survived into columnarized segments (%.0f%% of %d rows)",
			onDeltas, 100*float64(onDeltas)/float64(onRecs), onRecs)
	}
	if ColumnarDeltasDropped()-droppedBefore == 0 {
		t.Fatal("columnarization dropped no delta records: with delta mode on and chains sealed, " +
			"there were supposed to be some -- this test is no longer measuring what it claims")
	}
}

// TestColumnarMaterializesRetainedDeltas exercises the branch the density test cannot: a delta
// that must SURVIVE columnarization rather than be dropped.
//
// Reachable for real. Delta records and time travel are refused together at Open, but time travel
// can be switched on LATER (SetTimeTravel collapses open chains and stops writing new deltas --
// the ones already on disk stay). The retain floor then moves behind versions that are superseded
// but still inside the travel window, and those deltas are neither garbage nor live. Columnarizing
// must write the whole ad each one resolves to, as of ITS OWN commit, and must clear the delta flag
// on the record it writes.
//
// Refusing to columnarize such a segment -- the first thing this code did -- is self-defeating: the
// segment then keeps the delta records, which is the state the exclusion exists to prevent.
func TestColumnarMaterializesRetainedDeltas(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 18, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const keys = 1500
	want := map[string]int64{}
	for i := 0; i < keys; i++ {
		k := fmt.Sprintf("m%d.0", i)
		tx := c.Begin()
		tx.Put([]byte(k), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("seed conflicted")
		}
		want[k] = 0
	}
	for round := 1; round <= 10; round++ {
		for i := 0; i < keys; i++ {
			k := fmt.Sprintf("m%d.0", i)
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			tx := c.Begin()
			tx.PatchAttrs([]byte(k), patch, nil)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatal("patch conflicted")
			}
			want[k] = int64(round)
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}

	// Time travel on: the retain floor now sits behind the superseded deltas already on disk, so
	// they are inside the window and cannot simply be dropped.
	c.SetTimeTravel(&TimeTravelOptions{MaxDistance: time.Hour})

	_, matBefore, unresBefore := ColumnarDeltaDisposition()
	if !c.BuildAndEnableSchemaScan(2000, 32) {
		t.Skip("schema scan did not enable; nothing to measure")
	}
	nseg := 0
	for pass := 0; pass < 3; pass++ {
		nseg += c.ColumnarizeSealed()
	}
	sealedDeltas := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 || seg == sh.act {
				continue
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if !recIsMarker(seg.data, off) && recIsDelta(seg.data, off) {
					sealedDeltas++
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	colz := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.used > 0 && seg.columnarized() {
				colz++
			}
		}
		sh.mu.RUnlock()
	}
	// Columnarization can happen inside BuildAndEnableSchemaScan as well as in ColumnarizeSealed,
	// so the pass count is not the thing to assert on -- what matters is that segments really did
	// get columnarized, or the delta check below would be inspecting segments nothing rewrote.
	t.Logf("%d sealed segments columnarized (ColumnarizeSealed reported %d); %d delta records remain",
		colz, nseg, sealedDeltas)
	if colz == 0 {
		t.Fatal("no sealed segment was columnarized: this test is not exercising the rewrite")
	}
	if sealedDeltas != 0 {
		t.Fatalf("%d delta records remain in sealed segments", sealedDeltas)
	}
	_, matAfter, unresAfter := ColumnarDeltaDisposition()
	t.Logf("materialized %d retained deltas, %d unresolved", matAfter-matBefore, unresAfter-unresBefore)
	if matAfter-matBefore == 0 {
		t.Fatal("no delta was materialized: this test is not reaching the branch it exists for")
	}
	if unresAfter-unresBefore != 0 {
		t.Errorf("%d delta chains would not resolve", unresAfter-unresBefore)
	}

	// No delta record may survive into a columnarized segment, and every key must still read whole.
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 || seg == sh.act || !seg.columnarized() {
				continue
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if !recIsMarker(seg.data, off) && recIsDelta(seg.data, off) {
					sh.mu.RUnlock()
					t.Fatalf("a delta record survived into a columnarized segment at off=%d", off)
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	for k, n := range want {
		ad, ok := c.Get([]byte(k))
		if !ok {
			t.Fatalf("%s missing after columnarization", k)
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != n {
			t.Fatalf("%s N = %d, want %d", k, v, n)
		}
		if v, _ := ad.EvaluateAttrInt("Pad39"); v != 39 {
			t.Fatalf("%s lost a base attribute: Pad39 = %d", k, v)
		}
		if got := len(ad.AST().Attributes); got != 45 {
			t.Fatalf("%s has %d attributes, want 45", k, got)
		}
	}
}
