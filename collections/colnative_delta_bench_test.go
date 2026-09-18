package collections

import (
	"fmt"
	"testing"

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

	offBytes, offUsed, offRecs, offDeltas, offDead := build(0)
	t.Logf("DeltaMax=0   sealed: %6.2f MB arena, %6.2f MB used, %6d records (%d delta, %d dead)",
		float64(offBytes)/(1<<20), float64(offUsed)/(1<<20), offRecs, offDeltas, offDead)

	onBytes, onUsed, onRecs, onDeltas, onDead := build(16)
	t.Logf("DeltaMax=16  sealed: %6.2f MB arena, %6.2f MB used, %6d records (%d delta, %d dead)",
		float64(onBytes)/(1<<20), float64(onUsed)/(1<<20), onRecs, onDeltas, onDead)

	if onDeltas == 0 {
		t.Fatal("no delta records reached a sealed segment: this test is not measuring what it claims")
	}
	t.Logf("delta records are %.0f%% of the columnarized rows (%d of %d)",
		100*float64(onDeltas)/float64(onRecs), onDeltas, onRecs)
	t.Logf("used bytes: %.2f MB -> %.2f MB (%+.0f%%)  records: %d -> %d (%+.0f%%)",
		float64(offUsed)/(1<<20), float64(onUsed)/(1<<20),
		100*(float64(onUsed)/float64(offUsed)-1), offRecs, onRecs,
		100*(float64(onRecs)/float64(offRecs)-1))
}
