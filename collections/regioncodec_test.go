package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"testing"
)

// A columnar block's regions are compressed with a dictionary-less codec (see regionCodec). The risk
// in that change is not the compression ratio, it is DECODABILITY: a block whose regions were written
// with one codec and are read back with another produces garbage or an error, and the codec is not
// recorded per block -- it is re-derived at reload. So these tests pin that the derivation cannot
// drift, across a retrain and across a reopen, and that a section written under the old rules is
// refused rather than misread.

// TestRegionCodecIsDictionaryLessAndStable pins the two properties the reload depends on: the region
// codec carries no trained dictionary, and it does not change when the collection's codec does.
func TestRegionCodecIsDictionaryLessAndStable(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	defer c.Close()
	for i := 0; i < 3000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nCmd = \"/bin/x%d\"",
				i/10, i%10, i%32, i))); err != nil {
			t.Fatal(err)
		}
	}
	first := c.regionCodec()
	if got := first.Name(); got != "zstd" {
		t.Errorf("region codec is %q, want plain %q (a dictionary codec defeats the purpose)", got, "zstd")
	}
	// Retrain: the collection's write codec becomes zstd+dict. The region codec must not follow, or
	// blocks built before this point would need a dictionary that the reload does not supply.
	if _, err := c.RetrainDict(2000); err != nil {
		t.Skipf("retrain declined: %v", err)
	}
	if got := c.currentCodec().Name(); got != "zstd+dict" {
		t.Fatalf("retrain left the write codec %q; the test cannot show independence", got)
	}
	if second := c.regionCodec(); second != first {
		t.Errorf("region codec changed across a retrain (%q -> %q); blocks built earlier in this "+
			"process would no longer decode", first.Name(), second.Name())
	}
}

// TestColumnarSurvivesRetrainWithRegionCodec is the behaviour that used to be coupled: a block was
// compressed with its SEGMENT's codec, so a retrain (which changes that codec) meant a block had to
// be rebuilt to stay readable. With a dictionary-less region codec the block is independent of the
// segment's dictionary generation entirely.
func TestColumnarSurvivesRetrainWithRegionCodec(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	defer c.Close()
	for i := 0; i < 3000; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nMemory = %d\nOwner = \"u%d\"",
				i/10, i%10, 1024+(i%64)*512, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 4) {
		t.Skip("no sealed segments")
	}
	procID, ok := c.intern.LookupID("ProcId")
	if !ok {
		t.Fatal("ProcId not interned")
	}
	want, ok := c.NumStatsQuery(nil, "ProcId")
	if !ok {
		t.Fatal("NumStatsQuery declined before retrain")
	}
	truth := c.allBruteCount(procID, func(v int64) bool { return v >= 5 })

	if _, err := c.RetrainDict(2000); err != nil {
		t.Skipf("retrain declined: %v", err)
	}

	// Every block must still read, and agree with the row path over the same data.
	got, ok := c.NumStatsQuery(nil, "ProcId")
	if !ok {
		t.Fatal("NumStatsQuery declined after retrain")
	}
	if got.N != want.N || got.Max != want.Max || got.IntSum != want.IntSum {
		t.Errorf("stats changed across a retrain: (n %d, max %v, sum %d) -> (n %d, max %v, sum %d)",
			want.N, want.Max, want.IntSum, got.N, got.Max, got.IntSum)
	}
	if after := c.allBruteCount(procID, func(v int64) bool { return v >= 5 }); after != truth {
		t.Errorf("row-path truth changed across a retrain: %d -> %d", truth, after)
	}
}

// TestColSectionRejectsOlderVersion pins the version gate. A v2 section's regions were compressed
// with the segment's dictionary codec; decoding those bytes with the dictionary-less region codec
// would fail or, worse, silently produce wrong values. The reload must refuse and let the segment
// rebuild.
func TestColSectionRejectsOlderVersion(t *testing.T) {
	body := []byte("not really a block, but the header is what is under test")
	const upto = 4096

	// The current version round-trips.
	if got := readColSection(wrapColSection(body, upto), upto); got == nil {
		t.Fatal("a section at the current version was rejected")
	}
	// Every older version is refused.
	for _, v := range []uint16{1, 2} {
		old := make([]byte, 0, colSectionHdr+len(body))
		old = appendU32(old, colSectionMagic)
		old = appendU16(old, v)
		old = appendU32(old, uint32(upto))
		old = appendU32(old, crc32.ChecksumIEEE(body))
		old = append(old, body...)
		if got := readColSection(old, upto); got != nil {
			t.Errorf("a v%d section was accepted; its regions were compressed with a different "+
				"codec and would be misread", v)
		}
	}
	// And a version FROM THE FUTURE is refused too, so a downgrade cannot misread a newer section.
	future := make([]byte, 0, colSectionHdr+len(body))
	future = appendU32(future, colSectionMagic)
	future = appendU16(future, colSectionVersion+1)
	future = appendU32(future, uint32(upto))
	future = appendU32(future, crc32.ChecksumIEEE(body))
	future = append(future, body...)
	if got := readColSection(future, upto); got != nil {
		t.Error("a newer-version section was accepted")
	}
	// Sanity: the version really is where this test writes it.
	if v := binary.LittleEndian.Uint16(wrapColSection(body, upto)[4:]); v != colSectionVersion {
		t.Errorf("section version at offset 4 is %d, want %d", v, colSectionVersion)
	}
}
