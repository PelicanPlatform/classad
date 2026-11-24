package classad

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// TestNormalizedNameCached verifies NewAttributeReference precomputes the folded
// name and that a struct-literal node falls back to on-demand folding.
func TestNormalizedNameCached(t *testing.T) {
	if got := ast.NewAttributeReference("FooBar", ast.NoScope).NormalizedName(); got != "foobar" {
		t.Errorf("NewAttributeReference(%q).NormalizedName() = %q, want %q", "FooBar", got, "foobar")
	}
	// A node built with a struct literal has no precomputed norm; NormalizedName
	// must still fold on demand.
	if got := (&ast.AttributeReference{Name: "MixEd"}).NormalizedName(); got != "mixed" {
		t.Errorf("struct-literal NormalizedName() = %q, want %q", got, "mixed")
	}
}

// TestEvaluateAttrCaseFoldNoAlloc guards the matchmaking read hot path: resolving
// an attribute by a mixed-case name must (a) be case-insensitive and (b) not
// allocate — lookupInternal folds into a stack buffer and indexes the map with
// string(buf) instead of calling strings.ToLower per lookup. A regression here
// (e.g. reverting to normalizeName) reintroduces ~one allocation per attribute
// reference per candidate, which at 100k slots is hundreds of thousands of
// allocations per scan.
func TestEvaluateAttrCaseFoldNoAlloc(t *testing.T) {
	ad := New()
	ad.InsertAttr("RequestCpus", 4)

	// Case-insensitive resolution through the folded lookup.
	if v := ad.EvaluateAttr("requestcpus"); !v.IsInteger() {
		t.Fatalf("EvaluateAttr(lowercased) did not resolve; got %v", v)
	}
	if v, ok := ad.EvaluateAttrInt("REQUESTCPUS"); !ok || v != 4 {
		t.Fatalf("EvaluateAttrInt(uppercased) = %d, ok=%v; want 4, true", v, ok)
	}

	// Warm the attribute index, then assert the lookup+eval allocates nothing.
	_ = ad.EvaluateAttr("RequestCpus")
	if allocs := testing.AllocsPerRun(200, func() {
		_ = ad.EvaluateAttr("RequestCpus")
	}); allocs > 0 {
		t.Errorf("EvaluateAttr allocated %v/run, want 0 (lookupInternal must fold without strings.ToLower)", allocs)
	}
}

// TestInsertExistingCaseFoldNoAlloc guards the ingest write hot path, the mirror image of
// TestEvaluateAttrCaseFoldNoAlloc. Replacing the value of an attribute the ad already has needs
// the index slot, not the lowered name -- and that is the overwhelmingly common write on a live
// job, where a tailer re-applies a whole update whitelist of attributes the ad already carries.
// Lowering through normalizeName there allocates a string per attribute per update, used for one
// map probe and discarded.
func TestInsertExistingCaseFoldNoAlloc(t *testing.T) {
	ad := New()
	for _, name := range []string{"MemoryUsage", "RemoteUserCpu", "LastJobLeaseRenewal", "JobStatus"} {
		ad.InsertAttr(name, 1)
	}

	// Case-insensitive, and it must REPLACE rather than add a second attribute.
	ad.InsertAttr("memoryusage", 7)
	if v, ok := ad.EvaluateAttrInt("MemoryUsage"); !ok || v != 7 {
		t.Fatalf("EvaluateAttrInt(MemoryUsage) = %d, ok=%v; want 7, true", v, ok)
	}
	if n := len(ad.GetAttributes()); n != 4 {
		t.Fatalf("ad has %d attributes after a case-differing insert, want 4: %v", n, ad.GetAttributes())
	}

	// Insert, not InsertAttr: the latter allocates the literal node it wraps the value in, which
	// is the value's cost and not the name's. What is under test is the name handling.
	node := &ast.IntegerLiteral{Value: 2}
	if allocs := testing.AllocsPerRun(200, func() {
		ad.Insert("MemoryUsage", node)
	}); allocs > 0 {
		t.Errorf("Insert of an existing attribute allocated %v/run, want 0 "+
			"(indexSlot must fold without strings.ToLower)", allocs)
	}
	// A differing spelling must be just as free: callers do not normalize names.
	if allocs := testing.AllocsPerRun(200, func() {
		ad.Insert("lastjobleaserenewal", node)
	}); allocs > 0 {
		t.Errorf("Insert of an existing attribute by a differing spelling allocated %v/run, want 0", allocs)
	}
}

// TestDeleteMissCaseFoldNoAlloc: a Delete that finds nothing is a probe, and a probe needs no
// lowered name either.
func TestDeleteMissCaseFoldNoAlloc(t *testing.T) {
	ad := New()
	ad.InsertAttr("RequestCpus", 4)
	_ = ad.Delete("NotThere")
	if allocs := testing.AllocsPerRun(200, func() {
		_ = ad.Delete("NoSuchAttribute")
	}); allocs > 0 {
		t.Errorf("Delete of an absent attribute allocated %v/run, want 0", allocs)
	}
	if !ad.Delete("requestcpus") {
		t.Error("Delete must still be case-insensitive")
	}
	if _, ok := ad.EvaluateAttrInt("RequestCpus"); ok {
		t.Error("Delete did not remove the attribute")
	}
}

// BenchmarkInsertExisting is the shape an ingest tailer runs: re-apply an update whitelist onto a
// job ad that already carries every one of its attributes.
func BenchmarkInsertExisting(b *testing.B) {
	names := []string{
		"JobStatus", "ImageSize", "ResidentSetSize", "MemoryUsage", "DiskUsage",
		"RemoteSysCpu", "RemoteUserCpu", "BytesSent", "BytesRecvd", "BlockReadBytes",
		"BlockWriteBytes", "BlockReads", "BlockWrites", "NetworkIn", "NetworkOut",
		"CommittedTime", "CommittedSlotTime", "LastJobLeaseRenewal", "NumJobStarts",
		"StatsLifetimeStarter", "TotalSuspensions", "CumulativeSuspensionTime",
		"ScratchDirFileCount", "IOWait", "JobCpuInstructions",
	}
	ad := New()
	for i, n := range names {
		ad.InsertAttr(n, int64(i))
	}
	node := &ast.IntegerLiteral{Value: 1}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, n := range names {
			ad.Insert(n, node)
		}
	}
}

// TestEqualCaseFoldNoAlloc: comparing two ads needs each name pair ORDERED, not lowered. Lowering
// allocated two throwaway strings per attribute pair, on a call that compares whole ads.
func TestEqualCaseFoldNoAlloc(t *testing.T) {
	build := func(prefix string) *ClassAd {
		ad := New()
		for _, n := range []string{"JobStatus", "MemoryUsage", "RemoteUserCpu", "Owner"} {
			ad.InsertAttr(prefix+n, 1)
		}
		return ad
	}
	a, b := build(""), build("")
	if !a.Equal(b) {
		t.Fatal("identical ads compared unequal")
	}
	// Case-insensitive on names, as normalizeName was.
	mixed := New()
	for _, n := range []string{"jobstatus", "MEMORYUSAGE", "RemoteUserCpu", "owner"} {
		mixed.InsertAttr(n, 1)
	}
	if !a.Equal(mixed) {
		t.Fatal("ads differing only in name case compared unequal")
	}
	if a.Equal(build("X")) {
		t.Fatal("ads with different names compared equal")
	}

	_ = a.Equal(b) // warm the canonical ordering
	if allocs := testing.AllocsPerRun(200, func() {
		_ = a.Equal(b)
	}); allocs > 0 {
		t.Errorf("Equal allocated %v/run, want 0 (name comparison must fold, not lower)", allocs)
	}
}
