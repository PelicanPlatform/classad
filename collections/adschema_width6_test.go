package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func mustAdB(b *testing.B, src string) *classad.ClassAd {
	b.Helper()
	ad, err := classad.ParseOld(src)
	if err != nil {
		b.Fatal(err)
	}
	return ad
}

// TestIntWidth6ByteCodec unit-tests the 6-byte pack/unpack for the boundary and sign cases the
// width covers (readIntLE/putIntLE are width-generic; this pins the 6-byte range + sign extension).
func TestIntWidth6ByteCodec(t *testing.T) {
	for _, v := range []int64{0, 1, -1, 5_000_000_000_000, -3_000_000_000_000,
		(1 << 47) - 1, -(1 << 47), 0xFFFFFFFFFF, 0xFFFFFFFFFFFF /* max unsigned 6B */} {
		var buf [8]byte
		putIntLE(buf[:], v, 6)
		if v >= 0 {
			if got := readIntLE(buf[:], 6, true); got != v {
				t.Errorf("unsigned 6B %d -> %d", v, got)
			}
			if !intFits(v, 6, true) {
				t.Errorf("intFits(%d,6,unsigned) = false", v)
			}
		}
		if v >= -(1<<47) && v < (1<<47) {
			if got := readIntLE(buf[:], 6, false); got != v {
				t.Errorf("signed 6B %d -> %d", v, got)
			}
			if !intFits(v, 6, false) {
				t.Errorf("intFits(%d,6,signed) = false", v)
			}
		}
	}
	// A value beyond 6 bytes must NOT fit (so chooseIntWidth falls back to 8).
	if intFits(1<<48, 6, true) || intFits(1<<47, 6, false) {
		t.Fatal("intFits accepted a value outside the 6-byte range")
	}
}

// TestIntWidth6Byte checks the 6-byte width end to end on a realistic (unsigned, disk-sized) field:
// values in the 4 GB..256 TB range pick width 6, and a hot-column scan and a full-record
// reconstruct both round-trip the exact values.
func TestIntWidth6Byte(t *testing.T) {
	c := New(Options{Shards: 1})
	big := func(i int) int64 { return 5_000_000_000_000 + int64(i)*1_000_003 } // ~5e12, >2^32, <2^47
	var wires [][]byte
	for i := 0; i < 300; i++ {
		wires = append(wires, c.encodeAd(mustAdOld(t,
			fmt.Sprintf("Cpus=%d\nBig=%d", 1+i%8, big(i))).AST()))
	}
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.9, Fit: 1.0})
	bigID, _ := c.intern.LookupID("Big")
	if f := s.fields[s.byID[bigID]]; f.width != 6 || !f.unsigned {
		t.Fatalf("Big width=%d unsigned=%v, want 6/true", f.width, f.unsigned)
	}
	blk := encodeColumnarBlock(s, encodeRows(s, wires), []int{s.byID[bigID]}, identityCodec{})
	bc, _ := newBlockCache(64 << 20)
	blk.scanInt(s.byID[bigID], bc, func(k int, present bool, v int64) {
		if !present || v != big(k) {
			t.Fatalf("Big[%d]: present=%v v=%d want %d", k, present, v, big(k))
		}
	})
	for k := 0; k < 5; k++ {
		rec, err := blk.reconstruct(k, bc)
		if err != nil {
			t.Fatal(err)
		}
		var got int64
		var ok bool
		s.forEach(rec, func(id uint32, node []byte) bool {
			if id == bigID {
				if f, o := literalFloat(node); o {
					got, ok = int64(f), true
				}
			}
			return true
		})
		if !ok || got != big(k) {
			t.Fatalf("reconstruct[%d]: Big=%d(%v) want %d", k, got, ok, big(k))
		}
	}
}

// BenchmarkIntWidth6vs8 shows a 6-byte column scans faster than the 8-byte one it replaces (two
// fewer bytes of stride per record) and persists smaller, for a field whose values fit 6 bytes.
func BenchmarkIntWidth6vs8(b *testing.B) {
	c := New(Options{Shards: 1})
	var wires [][]byte
	for i := 0; i < 4000; i++ {
		v := 5_000_000_000_000 + int64(i)*1_000_003 // fits 6 bytes
		wires = append(wires, c.encodeAd(mustAdB(b, fmt.Sprintf("Cpus=%d\nBig=%d", 1+i%8, v)).AST()))
	}
	codec, _ := NewZSTDCodec(nil)
	s := buildAdSchema(wires, adSchemaOpts{Presence: 0.9, Fit: 1.0}) // Big -> 6B
	bigID, _ := c.intern.LookupID("Big")
	if s.fields[s.byID[bigID]].width != 6 {
		b.Skip("Big did not land at 6B")
	}
	// Force an 8B variant of the same field.
	fs := make([]adField, len(s.fields))
	copy(fs, s.fields)
	for i := range fs {
		if fs[i].id == bigID {
			fs[i].width = 8
		}
	}
	s8 := layoutSchema(fs)

	blk6 := encodeColumnarBlock(s, encodeRows(s, wires), []int{s.byID[bigID]}, codec)
	blk8 := encodeColumnarBlock(s8, encodeRows(s8, wires), []int{s8.byID[bigID]}, codec)
	sz := func(blk *columnarBlock) int {
		return len(marshalColSegment(oneBlockColSeg(blk, make([]uint32, blk.n)), c.intern.Name))
	}
	b.Logf("persisted block: 6B=%d bytes  8B=%d bytes (%+d)", sz(blk6), sz(blk8), sz(blk6)-sz(blk8))
	bc, _ := newBlockCache(64 << 20)
	scan := func(blk *columnarBlock, idx int) {
		blk.scanInt(idx, bc, func(k int, present bool, v int64) {})
	}
	b.Run("scan_6B", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			scan(blk6, s.byID[bigID])
		}
	})
	b.Run("scan_8B", func(b *testing.B) {
		b.ReportAllocs()
		for n := 0; n < b.N; n++ {
			scan(blk8, s8.byID[bigID])
		}
	})
}
