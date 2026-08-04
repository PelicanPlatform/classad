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

// TestBuildColumnarFromSegment transcodes a real segment's records into a columnar block and
// checks: (1) reconstruct(k) reproduces the exact ad stored at offs[k]; (2) a hot-field column
// scan matches reading that field from the segment record; (3) offs[k] indexes a real record
// header whose MVCC seq is readable (the map a scan uses for visibility).
func TestBuildColumnarFromSegment(t *testing.T) {
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

	blk, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, s, []int{memIdx})
	if blk.n != n || len(offs) != n {
		t.Fatalf("block n=%d offs=%d, want %d", blk.n, len(offs), n)
	}

	// One column-scan pass: block value per record.
	scanned := make([]int64, n)
	scannedPresent := make([]bool, n)
	if err := blk.scanInt(memIdx, nil, func(k int, p bool, v int64) { scannedPresent[k], scanned[k] = p, v }); err != nil {
		t.Fatal(err)
	}

	for k := 0; k < n; k++ {
		// (1) reconstruct == the ad at offs[k].
		rec, err := blk.reconstruct(k, nil)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := seg.codec.Decompress(nil, recAd(seg.data, offs[k]))
		orig := adAttrs(w)
		dec := map[uint32][]byte{}
		s.forEach(rec, func(id uint32, node []byte) bool { dec[id] = append([]byte(nil), node...); return true })
		if len(dec) != len(orig) {
			t.Fatalf("rec[%d] reconstructed %d attrs, want %d", k, len(dec), len(orig))
		}
		for id, on := range orig {
			if dn, ok := dec[id]; !ok || !sameValue(on, dn) {
				t.Errorf("rec[%d] attr %d mismatch after columnar round-trip", k, id)
			}
		}
		// (2) hot column scan == the segment's Memory value.
		mv, _ := wire.Ad(w).Lookup(memID)
		lit, _ := wire.LiteralValue(mv)
		if !scannedPresent[k] || scanned[k] != lit.Int {
			t.Errorf("rec[%d] scanInt Memory=(%v,%d), segment=%d", k, scannedPresent[k], scanned[k], lit.Int)
		}
		// (3) offs[k] is a real record header.
		if recSeq(seg.data, offs[k]) == 0 {
			t.Errorf("rec[%d] at offs %d has zero seq", k, offs[k])
		}
	}
}
