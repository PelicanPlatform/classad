package collections

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/klauspost/compress/zstd"
)

// This measurement answers: for persistent segments, what is the size/speed tradeoff of
// GLOBAL interning (one id space for the whole collection) vs PER-SEGMENT interning (each
// sealed segment carries its own id->name dictionary, Parquet/ORC style)? Baseline is the
// current INLINE encoding (attribute names in every record). Segments are compressed
// INDEPENDENTLY (on-disk reality), so per-segment interning includes its dict inside the
// compressed segment and gets no cross-segment name dedup.
//
// Run: go test -run TestInterningSizeTradeoff -v ./  (needs testdata/ospool_slots.ldif)

func zstdAll(b []byte) []byte {
	enc, _ := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedDefault))
	defer enc.Close()
	return enc.EncodeAll(b, nil)
}

// dictBytes is the serialized size of an id->name dictionary: each name length-prefixed.
func dictBytes(names []string) int {
	n := 0
	var tmp [binary.MaxVarintLen64]byte
	for _, s := range names {
		n += binary.PutUvarint(tmp[:], uint64(len(s))) + len(s)
	}
	return n
}

func TestInterningSizeTradeoff(t *testing.T) {
	ads, _ := loadOSPoolAds(t)
	asts := make([]*ast.ClassAd, len(ads))
	for i, a := range ads {
		asts[i] = a.AST()
	}
	N := len(asts)

	// --- INLINE baseline: names in every record; segment = concat, zstd per segment.
	inlineRaw, inlineZ := 0, 0
	// --- GLOBAL interned: one table for all; dict once (zstd once); records zstd per segment.
	gt := wire.NewInternTable()
	globalRecRaw := 0
	// --- PER-SEGMENT interned: fresh table per segment; [dict||records] zstd per segment.
	// All three measured at several segment sizes K.
	for _, K := range []int{128, 256, 512, 2048, N} {
		inlineRaw, inlineZ = 0, 0
		gRaw, gZ := 0, 0   // global-interned records (zstd per seg) -- dict added once below
		psRaw, psZ := 0, 0 // per-segment interned incl. dict, zstd per seg
		var gDistinct int  // recomputed identically each K (cheap); dict size added once
		psDictRaw := 0     // total per-segment dict bytes (raw)
		perSegNames := 0   // sum of distinct-names-per-segment (for reporting)

		for lo := 0; lo < N; lo += K {
			hi := lo + K
			if hi > N {
				hi = N
			}
			var inlineSeg, gSeg, psRecs []byte
			pt := wire.NewInternTable() // per-segment table
			for _, a := range asts[lo:hi] {
				inlineSeg = wire.EncodeInline(inlineSeg, a)
				gSeg = appendRec(gSeg, wire.Encode(nil, a, gt))
				psRecs = appendRec(psRecs, wire.Encode(nil, a, pt))
			}
			// per-segment dict = the names this segment interned.
			pnames := pt.Names()
			perSegNames += len(pnames)
			d := dictBytes(pnames)
			psDictRaw += d
			psSeg := append(marshalDict(pnames), psRecs...)

			inlineRaw += len(inlineSeg)
			inlineZ += len(zstdAll(inlineSeg))
			gRaw += len(gSeg)
			gZ += len(zstdAll(gSeg))
			psRaw += len(psSeg)
			psZ += len(zstdAll(psSeg))
		}
		gDistinct = gt.Len()
		gDictRaw := dictBytes(gt.Names())
		gDictZ := len(zstdAll(marshalDict(gt.Names())))
		globalRecRaw = gRaw

		// Global totals include the single dict (stored/compressed once).
		gTotRaw := gRaw + gDictRaw
		gTotZ := gZ + gDictZ

		fmt.Printf("\n===== segment size K=%d (%d segments) =====\n", K, (N+K-1)/K)
		fmt.Printf("  global distinct names=%d dict=%s(raw) %s(zstd);  per-seg dict total=%s(raw), avg names/seg=%d\n",
			gDistinct, human(gDictRaw), human(gDictZ), human(psDictRaw), perSegNames*K/max(1, N))
		fmt.Printf("  %-22s raw=%-10s zstd=%-10s\n", "INLINE (names/rec)", human(inlineRaw), human(inlineZ))
		fmt.Printf("  %-22s raw=%-10s zstd=%-10s   (records only=%s raw)\n", "GLOBAL interned", human(gTotRaw), human(gTotZ), human(globalRecRaw))
		fmt.Printf("  %-22s raw=%-10s zstd=%-10s\n", "PER-SEGMENT interned", human(psRaw), human(psZ))
		fmt.Printf("  --> interning zstd win vs inline:  global %.1f%%   per-seg %.1f%%\n",
			100*(1-float64(gTotZ)/float64(inlineZ)), 100*(1-float64(psZ)/float64(inlineZ)))
		fmt.Printf("  --> per-seg zstd overhead vs global: %+.2f%% (%s)\n",
			100*(float64(psZ)/float64(gTotZ)-1), human(psZ-gTotZ))
	}
	_ = inlineRaw
	_ = inlineZ
	_ = globalRecRaw
}

// appendRec length-prefixes a record so a concatenated segment self-delimits (models a real
// segment's record framing; adds the same few bytes to global and per-segment alike).
func appendRec(dst, rec []byte) []byte {
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(rec)))
	dst = append(dst, tmp[:n]...)
	return append(dst, rec...)
}

func marshalDict(names []string) []byte {
	var b []byte
	var tmp [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(tmp[:], uint64(len(names)))
	b = append(b, tmp[:n]...)
	for _, s := range names {
		n = binary.PutUvarint(tmp[:], uint64(len(s)))
		b = append(b, tmp[:n]...)
		b = append(b, s...)
	}
	return b
}

func human(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.2fMB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1fKB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%dB", n)
}

// BenchmarkDecodeInlineVsInterned compares full-ad decode cost of an inline record vs an
// interned one (global table), the read-side speed axis of the tradeoff.
func BenchmarkDecodeEncodings(b *testing.B) {
	ads, _ := loadOSPoolAds(b)
	if len(ads) == 0 {
		return
	}
	a := ads[len(ads)/2].AST()
	t := wire.NewInternTable()
	interned := wire.Encode(nil, a, t)
	inline := wire.EncodeInline(nil, a)

	b.Run("inline", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := wire.DecodeInline(inline); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("interned", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := wire.Decode(interned, t); err != nil {
				b.Fatal(err)
			}
		}
	})
}
