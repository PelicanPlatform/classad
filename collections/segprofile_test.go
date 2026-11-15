package collections

import (
	"encoding/binary"
	"math"
	"testing"
)

// acc builds a profile from (value, bytes) pairs given in ARBITRARY order -- the walk
// delivers records in append order, which is exactly what the profile must not assume.
func acc(attr string, bucket int64, pairs ...[2]int64) *segProfile {
	p := &profileAcc{attr: attr}
	for _, kv := range pairs {
		p.add(float64(kv[0]), kv[1], true)
	}
	return p.finish(bucket, 0, 0)
}

func TestSegProfileBreakpointsCloseBuckets(t *testing.T) {
	// Ten records of 30 bytes, values 1..10, fed newest-first to prove the sort happens.
	var pairs [][2]int64
	for v := int64(10); v >= 1; v-- {
		pairs = append(pairs, [2]int64{v, 30})
	}
	sp := acc("T", 100, pairs...)
	if sp == nil {
		t.Fatal("no profile")
	}
	// Buckets close when the running total crosses 100: at v=4 (120B), v=7 (210B), v=10 (300B).
	want := []float64{4, 7, 10}
	if len(sp.breaks) != len(want) {
		t.Fatalf("breaks = %v, want %v", sp.breaks, want)
	}
	for i := range want {
		if sp.breaks[i] != want[i] {
			t.Errorf("breaks[%d] = %v, want %v", i, sp.breaks[i], want[i])
		}
	}
	if sp.valueMin != 1 || sp.valueMax != 10 || sp.totalBytes != 300 {
		t.Errorf("min/max/total = %v/%v/%d, want 1/10/300", sp.valueMin, sp.valueMax, sp.totalBytes)
	}
}

// bytesBelow must never claim more than is really there. Claiming too much is what makes a
// pass stop while still over its ceiling, believing it freed space it did not.
func TestSegProfileBytesBelowIsALowerBound(t *testing.T) {
	var pairs [][2]int64
	for v := int64(1); v <= 10; v++ {
		pairs = append(pairs, [2]int64{v, 30})
	}
	sp := acc("T", 100, pairs...)

	for _, tc := range []struct{ cutoff, truth int64 }{
		{4, 90},   // values 1,2,3
		{5, 120},  // values 1..4
		{8, 210},  // values 1..7
		{11, 300}, // everything
	} {
		got, _ := sp.bytesBelow(float64(tc.cutoff))
		if got > tc.truth {
			t.Errorf("bytesBelow(%d) = %d, which EXCEEDS the real %d", tc.cutoff, got, tc.truth)
		}
		if got < tc.truth-sp.bucketBytes-30 {
			t.Errorf("bytesBelow(%d) = %d, further below the real %d than one bucket", tc.cutoff, got, tc.truth)
		}
	}
}

func TestSegProfileWholeSegmentOnlyWhenAllBelow(t *testing.T) {
	sp := acc("T", 100, [2]int64{1, 50}, [2]int64{2, 50})
	if _, whole := sp.bytesBelow(3); !whole {
		t.Error("every value below the cutoff: want whole-segment")
	}
	if _, whole := sp.bytesBelow(2); whole {
		t.Error("valueMax == cutoff is not below it: must not be whole-segment")
	}
}

// A record whose value cannot be bounded -- an expression, a sealed value, a non-numeric --
// is COUNTED, not dropped. The zone map's all-or-nothing rule would leave no profile at
// all, which falls straight back to the positional policy this replaces.
func TestSegProfileUnknownBytesAreNeverFreed(t *testing.T) {
	p := &profileAcc{attr: "T"}
	p.add(1, 100, true)
	p.add(0, 250, false) // an expression: no value to bound it with
	sp := p.finish(100, 0, 0)

	if sp.unknownBytes != 250 || sp.totalBytes != 350 {
		t.Fatalf("unknown/total = %d/%d, want 250/350", sp.unknownBytes, sp.totalBytes)
	}
	// Even a cutoff above every known value must not take the segment whole: the unknown
	// bytes might be anything, including data that must be kept.
	if _, whole := sp.bytesBelow(1e9); whole {
		t.Error("a segment holding unbounded bytes was taken whole")
	}
}

// Every record unbounded: a profile that says so is still useful (it reports the bytes as
// unfreeable); a nil profile would be read as "nothing known" and is not the same claim.
func TestSegProfileAllUnknownStillReports(t *testing.T) {
	p := &profileAcc{attr: "T"}
	p.add(0, 400, false)
	sp := p.finish(100, 0, 0)
	if sp == nil {
		t.Fatal("all-unknown produced no profile")
	}
	if sp.haveValues || len(sp.breaks) != 0 || sp.unknownBytes != 400 {
		t.Errorf("haveValues=%v breaks=%v unknown=%d, want false/[]/400", sp.haveValues, sp.breaks, sp.unknownBytes)
	}
	if b, whole := sp.bytesBelow(1e9); b != 0 || whole {
		t.Errorf("bytesBelow = (%d,%v), want (0,false)", b, whole)
	}
}

func TestSegProfileBlobRoundTrip(t *testing.T) {
	in := acc("EnteredHistoryTime", 1<<20,
		[2]int64{1_600_000_000, 1 << 19}, [2]int64{1_700_000_000, 1 << 19}, [2]int64{1_800_000_000, 1 << 20})
	in.recvMin, in.recvMax = 111, 222
	in.unknownBytes = 7

	got := parseProfileBlob(buildProfileBlob(in), "enteredhistorytime") // case-insensitive
	if got == nil {
		t.Fatal("round-trip produced nothing")
	}
	if got.bucketBytes != in.bucketBytes || got.totalBytes != in.totalBytes ||
		got.unknownBytes != in.unknownBytes || got.valueMin != in.valueMin ||
		got.valueMax != in.valueMax || got.recvMin != 111 || got.recvMax != 222 {
		t.Errorf("fields differ:\n got %+v\nwant %+v", *got, *in)
	}
	if len(got.breaks) != len(in.breaks) {
		t.Fatalf("breaks = %v, want %v", got.breaks, in.breaks)
	}
	for i := range in.breaks {
		if got.breaks[i] != in.breaks[i] {
			t.Errorf("breaks[%d] = %v, want %v", i, got.breaks[i], in.breaks[i])
		}
	}
}

// A profile that is not exactly right must not be used at all. One that understates leaves
// a pass over its ceiling; one that overstates drops records that should have been kept.
// Neither is detectable after the fact.
func TestParseProfileBlobRefusesBadSections(t *testing.T) {
	// Five 60-byte records at 100 bytes/bucket closes two buckets (at v=2 and v=4), so the
	// vector is long enough for an ordering violation to be expressible.
	good := buildProfileBlob(acc("T", 100,
		[2]int64{1, 60}, [2]int64{2, 60}, [2]int64{3, 60}, [2]int64{4, 60}, [2]int64{5, 60}))

	cases := map[string]func([]byte) []byte{
		"wrong magic":   func(b []byte) []byte { c := cp(b); binary.LittleEndian.PutUint32(c, 0xDEADBEEF); return c },
		"wrong version": func(b []byte) []byte { c := cp(b); binary.LittleEndian.PutUint16(c[4:], 99); return c },
		"truncated":     func(b []byte) []byte { return cp(b)[:len(b)-9] },
		"unknown exceeds total": func(b []byte) []byte {
			c := cp(b)
			binary.LittleEndian.PutUint64(c[24:], 1<<40)
			return c
		},
		"zero bucket size": func(b []byte) []byte { c := cp(b); binary.LittleEndian.PutUint64(c[8:], 0); return c },
		"breakpoints out of order": func(b []byte) []byte {
			c := cp(b)
			at := len(c) - 8 // last breakpoint
			binary.LittleEndian.PutUint64(c[at:], math.Float64bits(-1))
			return c
		},
	}
	for name, break_ := range cases {
		t.Run(name, func(t *testing.T) {
			if got := parseProfileBlob(break_(good), "T"); got != nil {
				t.Errorf("accepted a bad section: %+v", *got)
			}
		})
	}

	// A profile over a DIFFERENT attribute is in another value space entirely.
	if got := parseProfileBlob(good, "SomethingElse"); got != nil {
		t.Errorf("accepted a profile for the wrong attribute: %q", got.attr)
	}
	// Absent section: nothing known, no error.
	if got := parseProfileBlob(nil, "T"); got != nil {
		t.Error("an absent section yielded a profile")
	}
}

func cp(b []byte) []byte { return append([]byte(nil), b...) }

// A merged segment's profile must describe what it holds. Collapsing its inputs to a single
// [min,max] is what makes a merged segment age as though all of it were as new as its newest
// record -- the wart that forces rotation to run before merging.
func TestMergeProfilesPreservesTheDistribution(t *testing.T) {
	// Two segments, one old era and one new, as a merge of a backfill run beside live data.
	oldSeg := acc("T", 100, pairsAt(1000, 6, 50)...)
	newSeg := acc("T", 100, pairsAt(2000, 6, 50)...)

	merged := mergeProfiles([]*segProfile{oldSeg, newSeg}, 100, "T")
	if merged == nil {
		t.Fatal("no merged profile")
	}
	if merged.valueMin != 1000 || merged.valueMax < 2000 {
		t.Errorf("merged span = [%v,%v], want it to cover both eras", merged.valueMin, merged.valueMax)
	}
	// The whole point: a cutoff between the eras still finds the old era's bytes, which a
	// min/max alone could never report.
	got, whole := merged.bytesBelow(1500)
	if whole {
		t.Error("a cutoff between the eras took the merged segment whole")
	}
	if got <= 0 {
		t.Error("the merged profile reports nothing below a cutoff above the entire old era")
	}
	// And it stays a lower bound on the truth (300 bytes of old-era data).
	if got > 300 {
		t.Errorf("merged bytesBelow(1500) = %d, which exceeds the %d bytes really there", got, 300)
	}
}

// Merging must not silently produce a profile from inputs measured on different attributes.
func TestMergeProfilesRefusesMixedAttributes(t *testing.T) {
	a := acc("T", 100, pairsAt(1, 4, 50)...)
	b := &profileAcc{attr: "Other"}
	b.add(1, 50, true)
	if got := mergeProfiles([]*segProfile{a, b.finish(100, 0, 0)}, 100, "T"); got != nil {
		t.Errorf("merged across attributes: %+v", *got)
	}
	// A source with no profile at all is the same problem: the result would account for only
	// part of the records.
	if got := mergeProfiles([]*segProfile{a, nil}, 100, "T"); got != nil {
		t.Errorf("merged with an unprofiled source: %+v", *got)
	}
}

// Unknown bytes survive the merge: they are still unfreeable in the output.
func TestMergeProfilesCarriesUnknownBytes(t *testing.T) {
	p := &profileAcc{attr: "T"}
	p.add(1, 100, true)
	p.add(0, 200, false)
	withUnknown := p.finish(100, 0, 0)
	clean := acc("T", 100, pairsAt(5, 2, 50)...)

	merged := mergeProfiles([]*segProfile{withUnknown, clean}, 100, "T")
	if merged == nil {
		t.Fatal("no merged profile")
	}
	if merged.unknownBytes != 200 {
		t.Errorf("merged unknownBytes = %d, want 200", merged.unknownBytes)
	}
	if _, whole := merged.bytesBelow(1e9); whole {
		t.Error("the merged segment was taken whole despite carrying unbounded bytes")
	}
}

// pairsAt builds n records of `each` bytes at consecutive values from base.
func pairsAt(base int64, n int, each int64) [][2]int64 {
	out := make([][2]int64, n)
	for i := range out {
		out[i] = [2]int64{base + int64(i), each}
	}
	return out
}

// A segment smaller than one bucket closed no bucket at all, so it offered the cutoff
// search nothing and could only ever be taken whole. It now narrows its bucket to what it
// holds -- and the credit must be the real size, not the configured one, or a pass believes
// it freed a megabyte where there were two hundred bytes.
func TestSegProfileNarrowsTheBucketBelowOneBucket(t *testing.T) {
	sp := acc("T", 1<<20, [2]int64{1, 100}, [2]int64{2, 100}) // 200 bytes, 1 MiB bucket

	if len(sp.breaks) != 1 {
		t.Fatalf("breaks = %v, want exactly one (the segment holds less than a bucket)", sp.breaks)
	}
	if sp.bucketBytes != 200 {
		t.Errorf("bucketBytes = %d, want the 200 bytes actually held", sp.bucketBytes)
	}
	// Crediting that breakpoint must yield 200, not 1 MiB.
	got, _ := sp.bytesBelow(3)
	if got != 200 {
		t.Errorf("bytesBelow(3) = %d, want 200", got)
	}
	// And it is still a lower bound, never an over-claim.
	if below, _ := sp.bytesBelow(2); below > 100 {
		t.Errorf("bytesBelow(2) = %d, which exceeds the 100 bytes really below 2", below)
	}
}

// The narrowing must not apply when the segment really does hold whole buckets.
func TestSegProfileKeepsTheConfiguredBucketWhenItFits(t *testing.T) {
	sp := acc("T", 100, pairsAt(1, 10, 50)...) // 500 bytes at 100/bucket
	if sp.bucketBytes != 100 {
		t.Errorf("bucketBytes = %d, want the configured 100", sp.bucketBytes)
	}
	if len(sp.breaks) != 5 {
		t.Errorf("breaks = %v, want 5", sp.breaks)
	}
}

// The bucket default is 1 MiB, lowered only where that would leave a segment with no
// internal resolution. It is never raised: a larger segment gets FINER buckets than its
// eighth, because a tighter cutoff costs a few sidecar bytes and nothing else.
func TestProfileBucketDefaultIsOneMiBUnlessTheSegmentIsSmaller(t *testing.T) {
	for _, tc := range []struct {
		name     string
		segSize  int
		explicit int64
		want     int64
	}{
		{"default segment", 8 << 20, 0, 1 << 20},
		{"large segment keeps 1 MiB", 64 << 20, 0, 1 << 20},
		{"small segment drops to an eighth", 1 << 15, 0, 1 << 12},
		{"segment at the default bucket", 1 << 20, 0, 1 << 17},
		{"unknown segment size", 0, 0, 1 << 20},
		{"explicit wins over both", 1 << 15, 777, 777},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sh := &shard{segSize: tc.segSize, profBucket: tc.explicit}
			if got := sh.profileBucketBytes(); got != tc.want {
				t.Errorf("profileBucketBytes() = %d, want %d", got, tc.want)
			}
		})
	}
}

// Whatever the bucket is, a segment must end up with enough breakpoints for a cutoff to be
// placed inside it -- that is the property the lowering exists for, and the one that makes a
// straddling segment trimmable rather than only droppable.
func TestSmallSegmentsStillGetInternalResolution(t *testing.T) {
	sh := &shard{segSize: 1 << 15}
	bucket := sh.profileBucketBytes()

	p := &profileAcc{attr: "T"}
	// A segment's worth of records, at the segment size the shard reports.
	for i := 0; i < 64; i++ {
		p.add(float64(1000+i), int64(sh.segSize)/64, true)
	}
	sp := p.finish(bucket, 0, 0)
	if sp == nil {
		t.Fatal("no profile")
	}
	if len(sp.breaks) < profileBucketsPerSegment-1 {
		t.Errorf("a full segment closed %d buckets at a %d-byte bucket; too few for a cutoff "+
			"to fall inside the segment, so it could only ever be dropped whole",
			len(sp.breaks), bucket)
	}
}
