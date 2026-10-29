package collections

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Absence answered from the payload's remnant-name list, after a restart, with no per-record fallback.

// countFallbacks runs the column count readers for expr and returns their answers and how many
// records they handed to the ordinary evaluator in total.
func countFallbacks(t *testing.T, c *Collection, expr string) (map[string]int, int64) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	before := columnarFallbacks.Load()
	for name, fn := range map[string]func(*vm.Query) (int, bool){
		"ColumnarEvalCount": c.ColumnarEvalCount, "VectorEvalCount": c.VectorEvalCount, "CountQuery": c.CountQuery,
	} {
		if n, ok := fn(q); ok {
			got[name] = n
		}
	}
	return got, columnarFallbacks.Load() - before
}

func refCount(t *testing.T, c *Collection, expr string) int {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for range c.Query(q) {
		n++
	}
	return n
}

func checkCounts(t *testing.T, label string, f *reviewFixture, expr string) int64 {
	t.Helper()
	want := refCount(t, f.ref, expr)
	got, fb := countFallbacks(t, f.c, expr)
	if len(got) == 0 {
		t.Fatalf("%s: %q: no column reader answered", label, expr)
	}
	for name, n := range got {
		if n != want {
			t.Errorf("%s: %s(%q) = %d, want %d", label, name, expr, n, want)
		}
	}
	return fb
}

// TestReopenAbsentAttributeNoFallback: after a restart, an attribute no record holds -- a typo, or one
// only another table uses -- is answered exactly by the column readers, with no record handed to the
// ordinary evaluator; so is a non-schema attribute in the cold tail; and an expression attribute that
// lives only in remnants is still counted in full.
func TestReopenAbsentAttributeNoFallback(t *testing.T) {
	f := newReviewFixture(t, 3000, false, false)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	f.reopen()
	if n := f.c.staleSegmentCount(f.c.schemaScan.Load()); n != 0 {
		t.Fatalf("%d segments stale right after columnarization", n)
	}
	for _, expr := range []string{"NoSuchAttribute > 1", "NoSuchAttribute is undefined", "VacateReasonCode == 5", `VacateReason == "reason3"`} {
		if fb := checkCounts(t, "after reopen", f, expr); fb != 0 {
			t.Errorf("%q: %d records fell back to the ordinary evaluator, want 0", expr, fb)
		}
	}
	if fb := checkCounts(t, "after reopen", f, "MemoryUsage > 1"); fb == 0 {
		t.Error("MemoryUsage is an expression and must be evaluated per record; nothing fell back")
	}
}

// TestRemnantListMixedSegments: a store whose segments were columnarized without remnant-name lists
// (every store before them) reads correctly -- the old segments fall back, alone -- counts them stale,
// and converges onto segments with lists under the budget, exact at every step.
func TestRemnantListMixedSegments(t *testing.T) {
	colWriteRemnantList = false
	f := newReviewFixture(t, 3000, false, false)
	ok := f.c.BuildAndEnableSchemaScan(4096, 8)
	colWriteRemnantList = true
	if !ok {
		t.Fatal("schema scan did not enable")
	}
	f.c.colBudget = 4
	f.opts.ColumnarSegmentBudget = 4
	f.reopen()
	sealed := len(payloads(f.c))
	stale := f.c.staleSegmentCount(f.c.schemaScan.Load())
	if stale != sealed {
		t.Fatalf("%d of %d legacy segments counted stale", stale, sealed)
	}
	first := checkCounts(t, "legacy", f, "NoSuchAttribute > 1")
	if first == 0 {
		t.Fatal("legacy segments answered an uninterned name without reading their records")
	}
	checkCounts(t, "legacy", f, "MemoryUsage > 1")
	prev := first
	for pass := 1; ; pass++ {
		if pass > sealed {
			t.Fatal("did not converge")
		}
		f.c.BuildAndEnableSchemaScan(4096, 8)
		f.reopen()
		label := fmt.Sprintf("pass %d", pass)
		fb := checkCounts(t, label, f, "NoSuchAttribute > 1")
		checkCounts(t, label, f, "MemoryUsage > 1")
		checkCounts(t, label, f, "VacateReasonCode == 5")
		if fb > prev {
			t.Errorf("%s: fallbacks rose from %d to %d as segments converged", label, prev, fb)
		}
		prev = fb
		if f.c.staleSegmentCount(f.c.schemaScan.Load()) == 0 {
			if fb != 0 {
				t.Errorf("converged store still fell back %d times on an absent attribute", fb)
			}
			break
		}
	}
}

// TestRemnantTrailerIsInvisibleToOlderParsers: the trailer sits after every byte the payload parser
// reads, so a payload with one parses, up to the trailer, exactly as one without -- which is all an
// older build does with it. Stripping the trailer yields the same segment minus the list.
func TestRemnantTrailerIsInvisibleToOlderParsers(t *testing.T) {
	f := newReviewFixture(t, 1500, false, false)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	checked := 0
	for _, sh := range f.c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			blob := recAd(seg.data, cn.rec)
			body, _, err := splitColPayload(blob)
			if err != nil {
				t.Fatal(err)
			}
			cs := unmarshalColSegment(body, f.c.regionCodec(), f.c.intern.Intern)
			if cs == nil || cs.remnants == nil {
				t.Fatal("payload without a remnant list")
			}
			trailer := appendRemnantTrailer(nil, cs.remnants.names)
			if !bytes.HasSuffix(body, trailer) {
				t.Fatal("the remnant list is not the payload's last bytes")
			}
			old := unmarshalColSegment(body[:len(body)-len(trailer)], f.c.regionCodec(), f.c.intern.Intern)
			if old == nil || old.remnants != nil || len(old.blocks) != len(cs.blocks) || old.offsLen() != cs.offsLen() ||
				!sameSchemaLayout(old.schema(), cs.schema()) {
				t.Fatal("the payload without its trailer does not parse to the same segment")
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("no columnarized segment")
	}
}

// TestInternedArchiveMixedSchemaRowPath: an archive interning each segment at seal holds records keyed
// by segment-local ids. While a schema change converges, its segments that lack the new field answer
// through the row path, which has to look the field up by the segment's id rather than by name.
func TestInternedArchiveMixedSchemaRowPath(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 16, InternAtSeal: true, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	ref := New(Options{Shards: 1})
	defer ref.Close()
	for i := range 3000 {
		src := reviewAd(i, 0, false)
		if err := a.Append(mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
		if err := ref.Put([]byte(fmt.Sprintf("k%d", i)), mustAd(t, src)); err != nil {
			t.Fatal(err)
		}
	}
	a.c.colBudget = 1 << 20
	if !a.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no enable")
	}
	dicts := 0
	for _, seg := range a.c.shards[0].segs {
		if seg != nil && seg.columnarized() && seg.dict.Load() != nil {
			dicts++
		}
	}
	if dicts == 0 {
		t.Fatal("fixture: no interned columnarized segment")
	}
	a.c.colBudget = 2
	a.AddSchemaRequireAttrs("VacateReasonCode")
	a.BuildAndEnableSchemaScan(4096, 8)
	if !schemaHas(a.c, "VacateReasonCode") || a.c.staleSegmentCount(a.c.schemaScan.Load()) == 0 {
		t.Fatal("fixture: not in a mixed state")
	}
	for _, expr := range []string{"VacateReasonCode == 5", "VacateReasonCode >= 3 && ProcId < 5"} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range ref.Query(q) {
			want++
		}
		if n, ok := a.c.CountQuery(q); ok && n != want {
			t.Errorf("CountQuery(%q) = %d, want %d", expr, n, want)
		}
		if st, ok := a.c.NumStatsQuery(q, "VacateReasonCode"); ok && st.N != want {
			t.Errorf("NumStats(%q).N = %d, want %d", expr, st.N, want)
		}
		if g, ok := a.c.GroupCountQuery(q, "JobStatus"); ok {
			n := 0
			for _, x := range g {
				n += x.Count
			}
			if n != want {
				t.Errorf("GROUP BY JobStatus WHERE %q totals %d, want %d", expr, n, want)
			}
		}
	}
}

// TestLegacyInternedArchiveAbsentIsExact: an append-only archive interned at seal holds every record
// name in its segment dictionaries, which Open interns, so even its segments columnarized before the
// remnant-name list existed answer an absent attribute without reading records.
func TestLegacyInternedArchiveAbsentIsExact(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, InternAtSeal: true, GroupSchemaCount: -1}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2000 {
		if aerr := a.Append(mustAd(t, reviewAd(i, 0, false))); aerr != nil {
			t.Fatal(aerr)
		}
	}
	a.c.colBudget = 1 << 20
	colWriteRemnantList = false
	ok := a.BuildAndEnableSchemaScan(4096, 8)
	colWriteRemnantList = true
	if !ok {
		t.Fatal("no enable")
	}
	if cerr := a.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if a.c.staleSegmentCount(a.c.schemaScan.Load()) == 0 {
		t.Fatal("fixture: legacy segments not stale")
	}
	q, err := vm.Parse("NoSuchAttribute > 1")
	if err != nil {
		t.Fatal(err)
	}
	before := columnarFallbacks.Load()
	n, ok := a.c.ColumnarEvalCount(q)
	if !ok || n != 0 {
		t.Fatalf("ColumnarEvalCount = %d, %v; want 0, true", n, ok)
	}
	if fb := columnarFallbacks.Load() - before; fb != 0 {
		t.Errorf("%d records fell back on an attribute no record holds", fb)
	}
}

// TestReopenSidecarBlocksAbsentIsExact: a persistent table with the columnar rewrite disabled keeps
// sidecar blocks, whose cold tails hold every non-schema attribute; after a restart an absent
// attribute is answered without fallback there too, and a remnant-free expression still counts.
func TestReopenSidecarBlocksAbsentIsExact(t *testing.T) {
	f := newReviewFixture(t, 3000, false, false)
	f.c.colBudget = -1
	f.opts.ColumnarSegmentBudget = -1
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	f.reopen()
	covered := 0
	for _, seg := range f.c.shards[0].segs {
		if seg != nil && !seg.columnarized() && seg.colblk.Load() != nil {
			covered++
		}
	}
	if covered == 0 {
		t.Fatal("fixture: no sidecar block after reopen")
	}
	for _, expr := range []string{"NoSuchAttribute > 1", "VacateReasonCode == 5"} {
		if fb := checkCounts(t, "sidecar reopen", f, expr); fb != 0 {
			t.Errorf("%q: %d records fell back", expr, fb)
		}
	}
	checkCounts(t, "sidecar reopen", f, "MemoryUsage > 1")
}
