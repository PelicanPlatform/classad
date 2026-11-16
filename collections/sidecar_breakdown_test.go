package collections

import (
	"fmt"
	"testing"
)

// breakdownArchive builds a persistent archive whose segments are small enough to seal into
// sidecars, with enough indexed attributes that a breakdown has something to break down.
func breakdownArchive(t *testing.T) *Archive {
	t.Helper()
	a, err := CreateArchive(ArchiveOptions{
		Dir:              t.TempDir(),
		SegmentSize:      1 << 14,
		CategoricalAttrs: []string{"Owner", "State", "Pool"},
		ValueAttrs:       []string{"Memory", "Cpus"},
		InternAtSeal:     true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	owners := []string{"alice", "bob", "carol", "dave", "erin"}
	states := []string{"Idle", "Running", "Held"}
	for i := 0; a.Stats().Segments < 24 && i < 200000; i++ {
		ad := fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; Pool="p%d"; Memory=%d; Cpus=%d ]`,
			i, owners[i%len(owners)], states[i%len(states)], i%7, (i%16+1)*512, i%8)
		if err := a.Append(mustAd(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

// TestSidecarBreakdownMatchesReparse pins the equivalence the fast path rests on: reading the
// MPH/bloom sizes off the segment's live mmapSegIndex gives exactly what re-parsing the mapped
// bytes gave. The re-parse was the cost -- four maps and a scattered per-attribute stats read for
// every sealed segment, on every call -- and the only thing making it safe to drop is that the
// index it rebuilt is the one already in hand.
func TestSidecarBreakdownMatchesReparse(t *testing.T) {
	t.Parallel()
	a := breakdownArchive(t)
	checked := 0
	var wantMPH, wantBloom int64
	for _, sh := range a.c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			mm := seg.msidx.Load()
			if mm == nil {
				continue
			}
			// The old computation, verbatim: parse the mapped bytes from scratch.
			si, err := parseMmapSidecar(mm.data)
			if err != nil {
				sh.mu.RUnlock()
				t.Fatalf("segment %d: sidecar no longer parses: %v", seg.id, err)
			}
			var oldMPH, oldBloom int64
			for _, attrOff := range si.catDir {
				bloomOff := si.catBloomOff(attrOff)
				oldBloom += 8 + int64(le32(si.data, bloomOff+4)/64)*8
				mphOff := si.catMPHOff(attrOff)
				if mphLen := le32(si.data, mphOff); mphLen > 0 {
					oldMPH += 4 + int64(mphLen) + int64(le32(si.data, mphOff+4))*4
				} else {
					oldMPH += 4
				}
			}
			newMPH, newBloom := sidecarSketchBreakdown(mm)
			if newMPH != oldMPH || newBloom != oldBloom {
				sh.mu.RUnlock()
				t.Fatalf("segment %d: breakdown off the live index gives mph=%d bloom=%d, "+
					"re-parsing the bytes gives mph=%d bloom=%d", seg.id, newMPH, newBloom, oldMPH, oldBloom)
			}
			wantMPH += oldMPH
			wantBloom += oldBloom
			checked++
		}
		sh.mu.RUnlock()
	}
	// Without a floor this passes over an archive that sealed nothing, comparing no segments.
	if checked < 8 {
		t.Fatalf("only %d sealed sidecars; the comparison is vacuous", checked)
	}
	if wantMPH == 0 && wantBloom == 0 {
		t.Fatal("every sidecar reports zero MPH and zero bloom bytes; the fixture carries no sketches")
	}
	if ss := a.SidecarSizes(); ss.MPHBytes != wantMPH || ss.BloomBytes != wantBloom {
		t.Errorf("SidecarSizes reports mph=%d bloom=%d, want mph=%d bloom=%d",
			ss.MPHBytes, ss.BloomBytes, wantMPH, wantBloom)
	}
}

// TestSidecarBytesMatchesSidecarSizes keeps the cheap accessor honest: it answers the same count
// and the same bytes as the full measure, and differs only by what it declines to measure. Two
// walks over the same segments are exactly the kind of pair that drifts.
func TestSidecarBytesMatchesSidecarSizes(t *testing.T) {
	t.Parallel()
	a := breakdownArchive(t)
	full := a.SidecarSizes()
	segs, mapped := a.SidecarBytes()
	if full.Segments == 0 || full.MappedBytes == 0 {
		t.Fatal("no sealed sidecars in the fixture; the comparison is vacuous")
	}
	if segs != full.Segments || mapped != full.MappedBytes {
		t.Errorf("SidecarBytes = (%d segments, %d bytes), SidecarSizes = (%d segments, %d bytes)",
			segs, mapped, full.Segments, full.MappedBytes)
	}
}
