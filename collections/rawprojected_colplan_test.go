package collections

import (
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// colPlanAd is the i-th job ad for the raw-projection columnar tests. Every shape the
// projector treats differently is present: literals the schema columnarizes, an expression
// over attributes outside any small projection (MemoryUsage), a reference chain that only a
// TRANSITIVE closure completes (Chain0 -> Chain1 -> Chain2 -> ResidentSetSize), private
// attributes and an expression over one (ClaimId, Leak), the type fields, an attribute most
// records lack (Sometimes) and one too rare to enter the schema at all (RareAttr, so it stays
// in the record remnant).
func colPlanAd(i int) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d; Owner="user%d"`,
		i, i%10, 1+i%5, i%7)
	fmt.Fprintf(&b, `; RequestDisk=%d; ResidentSetSize=%d; DiskUsage=%d; CgroupFloor=%d`,
		(1+i%4)*2048, (1+i%6)*512, (1+i%3)*256, 64+i%5)
	b.WriteString(`; MemoryUsage = ((ResidentSetSize + 1023) / 1024)`)
	b.WriteString(`; Chain0 = Chain1 + 1; Chain1 = Chain2 * 2; Chain2 = ResidentSetSize`)
	fmt.Fprintf(&b, `; ClaimId = "<secret-%d>"; Leak = ClaimId`, i)
	if i%4 == 0 {
		fmt.Fprintf(&b, `; Sometimes = %d`, i)
	}
	if i%50 == 0 {
		fmt.Fprintf(&b, `; RareAttr = "rare-%d"`, i)
	}
	for f := 0; f < 8; f++ {
		fmt.Fprintf(&b, `; Filler%02d="filler %d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

const colPlanN = 1600

// colPlanMutable is a persistent mutable collection whose sealed segments are interned and
// columnarized, with an active tail of inline records and, on top, delta records: the last
// keys are patched after columnarization so their current versions are deltas over a full
// record.
//
// encrypted seals Owner (a literal) and MemoryUsage (an expression) at rest, so sealed values
// travel through both paths: a redacted read must render them undefined either way.
func colPlanMutable(t testing.TB, encrypted bool) *Collection {
	t.Helper()
	opts := Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 16, DeltaMax: 8}
	if encrypted {
		opts.DataKey = make([]byte, 32)
		for i := range opts.DataKey {
			opts.DataKey[i] = byte(i + 7)
		}
		opts.EncryptedAttrs = []string{"Owner", "MemoryUsage"}
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	put := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, colPlanAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	put(0, colPlanN*3/4)
	if _, err := c.RetrainDict(2000); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	put(colPlanN*3/4, colPlanN)
	for i := colPlanN - 200; i < colPlanN; i++ {
		patch := classad.New()
		patch.InsertAttr("JobStatus", int64(4))
		tx := c.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, nil)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("patch conflicted")
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no delta records stored: the delta arm is not covered")
	}
	checkColPlanFixture(t, c)
	return c
}

// colPlanArchive is a history archive with columnarized sealed segments and an unsealed tail.
func colPlanArchive(t testing.TB) *Collection {
	t.Helper()
	a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 16,
		ZoneAttrs: []string{"ClusterId"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	app := func(lo, hi int) {
		for i := lo; i < hi; i++ {
			if err := a.Append(mustAd(t, colPlanAd(i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	app(0, colPlanN*3/4)
	if !a.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no columnar schema derived")
	}
	app(colPlanN*3/4, colPlanN)
	checkColPlanFixture(t, a.c)
	return a.c
}

// checkColPlanFixture asserts what the comparisons below depend on, so a fixture that drifted
// cannot make them vacuous: columnarized segments exist, the schema carries the literals and
// does NOT carry RareAttr (which must be read from the record remnant).
func checkColPlanFixture(t testing.TB, c *Collection) {
	t.Helper()
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("fixture columnarized nothing")
	}
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("schema scan not enabled")
	}
	inSchema := func(name string) bool {
		id, ok := c.intern.LookupID(name)
		if !ok {
			return false
		}
		_, in := st.schema.byID[id]
		return in
	}
	if !inSchema("ClusterId") || !inSchema("ResidentSetSize") {
		t.Fatal("schema does not carry the literal columns")
	}
	if inSchema("RareAttr") {
		t.Fatal("RareAttr entered the schema: the record-remnant arm is not covered")
	}
}

// rawRows renders a raw projected scan to one comparable string per row: the type fields and
// the expressions in the order they were emitted. RawAd aliases scan buffers, so it is copied.
func rawRows(seq iter.Seq[RawAd]) []string {
	var out []string
	for ra := range seq {
		var b strings.Builder
		fmt.Fprintf(&b, "MyType=%q TargetType=%q", ra.MyType, ra.TargetType)
		for _, e := range ra.Exprs {
			b.WriteString(" | ")
			b.Write(e)
		}
		out = append(out, b.String())
	}
	return out
}

// refRawProjected is the raw projected query exactly as it ran before it had a projection
// plan: every matched record reassembled and handed whole to the projector. It is the oracle
// the columnar path must reproduce.
func refRawProjected(c *Collection, q *vm.Query, projection []string, chaseRefs, redact bool, st *ScanStats) iter.Seq[RawAd] {
	return func(yield func(RawAd) bool) {
		p := c.newRawProjector(projection, chaseRefs, redact)
		if q == nil {
			emit := c.yieldRawProjected(yield, p)
			for _, sh := range c.shards {
				if !c.scanShard(sh, queryPlan{}, emit) {
					return
				}
			}
			return
		}
		c.scanQuery(c.newQueryPlan(q, nil, st), c.yieldRawProjected(yield, p))
	}
}

// TestRawProjectedColumnarMatchesReassembly checks that the raw projected scan returns the same
// rows from columns as it does from reassembled records, over a mutable collection (interned
// columnarized segments, an inline active tail, delta records), the same encrypted at rest, and an
// archive (inline-keyed columnarized segments).
//
// Under chaseRefs the projector closes each emitted expression over the ad it is handed, and a
// columnar ad is narrowed to the projection -- so MemoryUsage projected alone must still bring
// ResidentSetSize, and Chain0 must bring the whole chain. Under redact the private ClaimId never
// leaves, whether named or reached by Leak.
func TestRawProjectedColumnarMatchesReassembly(t *testing.T) {
	fixtures := []struct {
		name string
		c    *Collection
	}{
		{"mutable", colPlanMutable(t, false)},
		{"encrypted", colPlanMutable(t, true)},
		{"archive", colPlanArchive(t)},
	}
	projections := []struct {
		name string
		proj []string
		// fast, per chaseRefs (index 0 off, 1 on): whether a column-decided record must be
		// projected from its columns rather than reassembled. Without chaseRefs any non-empty
		// projection qualifies; with it, only one whose projected ad refers to nothing outside
		// itself.
		fast [2]bool
	}{
		{"one literal", []string{"ClusterId"}, [2]bool{true, true}},
		{"literals", []string{"ClusterId", "ProcId", "JobStatus", "Owner", "RequestDisk"}, [2]bool{true, true}},
		{"absent from most", []string{"ClusterId", "Sometimes"}, [2]bool{true, true}},
		{"record remnant", []string{"ClusterId", "RareAttr"}, [2]bool{true, true}},
		{"type fields named", []string{"ClusterId", "MyType", "TargetType"}, [2]bool{true, true}},
		{"private named", []string{"ClusterId", "ClaimId"}, [2]bool{true, true}},
		{"expression outside", []string{"MemoryUsage"}, [2]bool{true, false}},
		{"transitive", []string{"ClusterId", "Chain0"}, [2]bool{true, false}},
		{"expression closed", []string{"MemoryUsage", "ResidentSetSize"}, [2]bool{true, true}},
		{"expression over private", []string{"ClusterId", "Leak"}, [2]bool{true, false}},
		{"unknown name", []string{"ClusterId", "NoSuchAttribute"}, [2]bool{true, true}},
		{"empty", nil, [2]bool{false, false}},
	}
	constraints := []string{
		"ProcId < 4",      // decided from the columns
		"true",            // constant: the columns may project every record
		"MemoryUsage > 1", // reads an expression: needs the ad, so always reassembled
	}
	for _, fx := range fixtures {
		for _, pj := range projections {
			for _, chase := range []bool{false, true} {
				for _, redact := range []bool{false, true} {
					name := fmt.Sprintf("%s/%s/chase=%v/redact=%v", fx.name, pj.name, chase, redact)
					t.Run(name, func(t *testing.T) {
						t.Parallel() // read-only over shared fixtures; the collection serves concurrent reads
						for _, cons := range constraints {
							q := mustParseQ(t, cons)
							var refSt, gotSt ScanStats
							want := rawRows(refRawProjected(fx.c, q, pj.proj, chase, redact, &refSt))
							got := rawRows(fx.c.QueryRawProjectedStats(q, pj.proj, chase, redact, &gotSt))
							compareRawRows(t, cons, got, want)
							if len(want) == 0 {
								t.Fatalf("%s: no rows: the comparison proves nothing", cons)
							}
							if pj.fast[boolInt(chase)] && cons == "ProcId < 4" {
								if gotSt.RecordsColumnDecided == 0 {
									t.Fatalf("%s: nothing decided from columns: the fast path is not exercised", cons)
								}
								if gotSt.RecordsReassembled >= refSt.RecordsReassembled {
									t.Errorf("%s: %d records reassembled with the projection plan, %d without: "+
										"the columnar projection did not run", cons,
										gotSt.RecordsReassembled, refSt.RecordsReassembled)
								}
							}
						}
						// The no-query scan takes the same plan.
						compareRawRows(t, "scan", rawRows(fx.c.ScanRawProjected(pj.proj, chase, redact)),
							rawRows(refRawProjected(fx.c, nil, pj.proj, chase, redact, nil)))
					})
				}
			}
		}
	}
}

// compareRawRows compares two raw projected results byte for byte: the same rows in the same
// order, each with the same type fields and the same "Name = Value" expressions in the same
// order.
func compareRawRows(t *testing.T, label string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d rows, want %d", label, len(got), len(want))
	}
	bad := 0
	for i := range want {
		if got[i] != want[i] {
			if bad < 3 {
				t.Errorf("%s: row %d differs:\n got  %s\n want %s", label, i, got[i], want[i])
			}
			bad++
		}
	}
	if bad > 0 {
		t.Errorf("%s: %d of %d rows differ", label, bad, len(want))
	}
}

// TestRawProjectedColumnarChasesReferences pins the specific rows the closure exists for, so a
// failure names the missing attribute rather than a diff: an expression projected alone, and
// a chain only a transitive closure completes, both served from a columnarized archive.
func TestRawProjectedColumnarChasesReferences(t *testing.T) {
	c := colPlanArchive(t)
	q := mustParseQ(t, "ProcId == 3")
	for _, tc := range []struct {
		proj []string
		want []string
	}{
		{[]string{"MemoryUsage"}, []string{"MemoryUsage", "ResidentSetSize"}},
		{[]string{"ClusterId", "Chain0"}, []string{"ClusterId", "Chain0", "Chain1", "Chain2", "ResidentSetSize"}},
	} {
		rows := 0
		for ra := range c.QueryRawProjected(q, tc.proj, true, false) {
			rows++
			var names []string
			for _, e := range ra.Exprs {
				name, _, _ := strings.Cut(string(e), " = ")
				names = append(names, name)
			}
			slices.Sort(names)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(names, want) {
				t.Fatalf("projecting %v with chaseRefs: row carries %v, want %v", tc.proj, names, want)
			}
			if ra.MyType != "Job" || ra.TargetType != "Machine" {
				t.Fatalf("type fields lost: MyType=%q TargetType=%q", ra.MyType, ra.TargetType)
			}
		}
		if rows == 0 {
			t.Fatalf("projecting %v: no rows", tc.proj)
		}
	}
}
