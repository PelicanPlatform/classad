package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// segmentWires decompresses a segment's live-arena records (skipping markers) into wire ads.
func segmentWires(t *testing.T, seg *segment) [][]byte {
	t.Helper()
	var ws [][]byte
	for off := 0; off < seg.used; {
		o := uint32(off)
		total := recTotalLen(seg.data, o)
		if total == 0 {
			break
		}
		if !recIsMarker(seg.data, o) {
			w, err := seg.codec.Decompress(nil, recAd(seg.data, o))
			if err != nil {
				t.Fatal(err)
			}
			ws = append(ws, w)
		}
		off += int(total)
	}
	return ws
}

// oneBlockColSeg wraps a single block as a segment accelerator, for tests that build a block
// directly rather than from a segment.
func oneBlockColSeg(blk *columnarBlock, offs []uint32) *colSegment {
	return &colSegment{blocks: []*columnarBlock{blk}, offs: offs}
}

// TestBuildColumnarFromSegment transcodes a real segment's records into columnar ROW-GROUP blocks
// and checks, for several group sizes: (1) reconstruct(k) reproduces the exact ad stored at
// offs[k]; (2) a hot-field column scan matches reading that field from the segment record; (3)
// offs[k] indexes a real record header whose MVCC seq is readable (the map a scan uses for
// visibility); (4) the records split into the expected groups, and every record is covered
// exactly once.
//
// Sweeping the group size is the point: the group size must be transparent to every reader. A
// bug that indexes a group-local record with a segment-wide index (or the reverse) reads another
// record's value or another record's MVCC header, which is a wrong answer rather than a slow one,
// and a single-group fixture cannot see it.
func TestBuildColumnarFromSegmentRowGroups(t *testing.T) {
	store := New(Options{Shards: 1})
	const n = 500
	for i := 0; i < n; i++ {
		ad := mustAdOld(t, fmt.Sprintf(
			"Cpus = %d\nMemory = %d\nDisk = %d\nBig = %t\nArch = \"X86_64\"\nMachine = \"m%03d.example.org\"\nReq = (Cpus >= 1)",
			1+i%16, 1024+(i%64)*256, i*4096, i%3 == 0, i))
		if err := store.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	seg := store.shards[0].act
	if seg == nil || seg.used == 0 {
		t.Fatal("no active segment data")
	}
	segWires := segmentWires(t, seg)
	if len(segWires) != n {
		t.Fatalf("segment holds %d records, want %d", len(segWires), n)
	}

	s := buildAdSchema(segWires, adSchemaOpts{Presence: 0.90, Fit: 0.95, Strings: true})
	memID, _ := store.intern.LookupID("Memory")
	memIdx, ok := s.byID[memID]
	if !ok || s.fields[memIdx].kind != akInt {
		t.Skip("Memory not an int schema field")
	}

	// Group sizes: the production default, one that divides n exactly, an extreme of one record
	// per group, and one larger than the segment (a single short group -- the old whole-segment
	// shape). All must produce identical per-record answers.
	for _, groupRows := range []int{colGroupRows, 100, 1, n + 7} {
		t.Run(fmt.Sprintf("group%d", groupRows), func(t *testing.T) {
			blocks, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, s, []int{memIdx}, byRows(groupRows), store.recordToInterned)
			if len(offs) != n {
				t.Fatalf("offs=%d, want %d", len(offs), n)
			}
			// (4) expected group split, and the counts sum to every record exactly once.
			wantBlocks := (n + groupRows - 1) / groupRows
			if len(blocks) != wantBlocks {
				t.Fatalf("got %d row groups, want %d at groupRows=%d", len(blocks), wantBlocks, groupRows)
			}
			total := 0
			for i, b := range blocks {
				if b.n > groupRows {
					t.Errorf("group %d holds %d records, over the %d limit", i, b.n, groupRows)
				}
				if i < len(blocks)-1 && b.n != groupRows {
					t.Errorf("group %d holds %d records; only the last group may be short", i, b.n)
				}
				total += b.n
			}
			if total != n {
				t.Fatalf("row groups cover %d records, want %d", total, n)
			}

			// One column-scan pass per group, gathered into segment-wide order.
			scanned := make([]int64, n)
			scannedPresent := make([]bool, n)
			base := 0
			for _, b := range blocks {
				bb := base
				if err := b.scanInt(memIdx, nil, func(k int, p bool, v int64) {
					scannedPresent[bb+k], scanned[bb+k] = p, v
				}); err != nil {
					t.Fatal(err)
				}
				base += b.n
			}

			base = 0
			for bi, b := range blocks {
				for k := 0; k < b.n; k++ {
					gk := base + k
					// (1) reconstruct == the ad at offs[gk].
					rec, err := b.reconstruct(k, nil)
					if err != nil {
						t.Fatal(err)
					}
					w, _ := seg.codec.Decompress(nil, recAd(seg.data, offs[gk]))
					orig := adAttrs(w)
					dec := map[uint32][]byte{}
					s.forEach(rec, func(id uint32, node []byte) bool { dec[id] = append([]byte(nil), node...); return true })
					if len(dec) != len(orig) {
						t.Fatalf("group %d rec[%d] reconstructed %d attrs, want %d", bi, k, len(dec), len(orig))
					}
					for id, on := range orig {
						if dn, ok := dec[id]; !ok || !sameValue(on, dn) {
							t.Errorf("group %d rec[%d] attr %d mismatch after columnar round-trip", bi, k, id)
						}
					}
					// (2) hot column scan == the segment's Memory value.
					mv, _ := wire.Ad(w).Lookup(memID)
					lit, _ := wire.LiteralValue(mv)
					if !scannedPresent[gk] || scanned[gk] != lit.Int {
						t.Errorf("rec[%d] scanInt Memory=(%v,%d), segment=%d", gk, scannedPresent[gk], scanned[gk], lit.Int)
					}
					// (3) offs[gk] is a real record header.
					if recSeq(seg.data, offs[gk]) == 0 {
						t.Errorf("rec[%d] at offs %d has zero seq", gk, offs[gk])
					}
				}
				base += b.n
			}
		})
	}
}
