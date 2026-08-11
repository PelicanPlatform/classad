package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// colScope evaluates the query itself against columns, so the risk is not "does it prune correctly"
// but "does it read every value type correctly" -- strings are positional, bools are bit-packed,
// non-schema attributes live in the cold tail, and an expression cannot be judged from its node.

// scopeFixture2 exercises every read path: numeric slots, a bool, a string, a NON-schema attribute
// (present in too few records to become a field), an escaped too-wide number, and an expression.
func scopeFixture2(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
			"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nWantCheckpoint = %t",
			i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512, i%3 == 0)
		switch {
		case i%997 == 11:
			src += "\nRareAttr = 42" // present in ~0.1%: never a schema field, lives in the cold tail
		case i%991 == 13:
			src = fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\n"+
				"RequestCpus = %d\nOwner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nWantCheckpoint = false",
				1<<40+i, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512) // too-wide ClusterId: escapes
		case i%983 == 17:
			src += "\nDerived = RequestCpus * 2" // an expression: forces the per-record fallback
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ProcId >= 0", "JobStatus >= 0", "RequestMemory >= 0", "RequestCpus >= 0"} {
		q, err := vm.Parse(e)
		if err != nil {
			tb.Fatal(err)
		}
		for i := 0; i < 20; i++ {
			for range c.Query(q) {
			}
		}
	}
	if !c.BuildAndEnableSchemaScan(4000, 8) {
		tb.Skip("no sealed segments")
	}
	return c
}

// scopeQueries2 are the shapes NO hand-written column scan serves.
var scopeQueries2 = []string{
	`Owner == "user3"`,                           // string equality
	`Owner != "user3"`,                           // string inequality
	`Owner == "user3" && RequestMemory > 4096`,   // string + numeric (numeric part is prunable)
	"RequestMemory > RequestCpus * 512",          // arithmetic between attributes
	"ProcId >= 5 && ClusterId != ProcId",         // attribute to attribute
	"RequestMemory > 4096 || RequestCpus >= 7",   // disjunction
	"WantCheckpoint",                             // a bare bool field
	"!WantCheckpoint && RequestCpus >= 4",        // negated bool
	"RareAttr == 42",                             // a NON-schema attribute, only in the cold tail
	"Derived > 4",                                // an EXPRESSION value: per-record fallback
	`Cmd == "/home/user3/run.sh" && ProcId == 3`, // two strings, one numeric
}

// TestColScopeMatchesRowPath is the equivalence gate for every newly-served shape.
func TestColScopeMatchesRowPath(t *testing.T) {
	c := scopeFixture2(t, 5000)
	defer c.Close()
	for _, expr := range scopeQueries2 {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range c.Query(q) {
			want++
		}
		got, served := c.ColumnarEvalCount(q)
		if !served {
			t.Errorf("%s: declined; it is native and should be servable", expr)
			continue
		}
		if got != want {
			t.Errorf("%s: colScope %d != row %d", expr, got, want)
		}
		t.Logf("%-48s colScope=%-6d row=%d", expr, got, want)
	}
	// The fixture must contain the exceptional records, or several rows above prove nothing.
	for _, expr := range []string{"RareAttr == 42", "Derived > 4", "ClusterId > 1000000000000"} {
		if n := rowTruth(t, c, expr); n == 0 {
			t.Errorf("fixture has no records matching %q; that shape is untested", expr)
		}
	}
}

// TestColScopeRoutedOnlyWithoutIndex pins the gate. colScope evaluates EVERY visible record, so a
// selective constraint on an indexed attribute is better served by the row path's posting list --
// routing without checking that was measured 2.9x slower on an archive.
func TestColScopeRoutedOnlyWithoutIndex(t *testing.T) {
	c := scopeFixture2(t, 3000)
	defer c.Close()
	// A string comparison is served: zero-copy region reads made it competitive with the row path
	// (6.09ms vs 6.68ms over 60k records) at a twentieth of the allocations.
	sq, err := vm.Parse(`Owner == "user3"`)
	if err != nil {
		t.Fatal(err)
	}
	got, served := c.CountQuery(sq)
	if !served {
		t.Error("a string comparison should be served")
	} else if want := rowTruth(t, c, `Owner == "user3"`); got != want {
		t.Errorf("string comparison: %d != row %d", got, want)
	}

	// A shape colScope serves that ALSO has an index-usable probe. The attr-to-attr conjunct is
	// omitted from Probes, so the RequestMemory probe is what an index could prune with.
	const mixed = "RequestMemory > 4096 && ClusterId != ProcId"
	q, err := vm.Parse(mixed)
	if err != nil {
		t.Fatal(err)
	}
	if _, served := c.CountQuery(q); !served {
		t.Error("with no index, this should fall through to colScope")
	}
	// Index RequestMemory, and the routing must step aside for the posting list.
	if !c.AddIndex(nil, []string{"RequestMemory"}) {
		t.Skip("AddIndex declined")
	}
	c.Reindex()
	if _, served := c.CountQuery(q); served {
		t.Error("with RequestMemory indexed, CountQuery should decline so the row path can prune")
	}
	// An arithmetic query has NO index-usable probe, so an index cannot prune it and colScope stays
	// the right choice even now -- the gate asks whether the index helps THIS query, not whether one
	// exists.
	aq, err := vm.Parse("RequestMemory > RequestCpus * 512")
	if err != nil {
		t.Fatal(err)
	}
	if _, served := c.CountQuery(aq); !served {
		t.Error("an arithmetic query has no prunable probe; colScope should still serve it")
	}
	if got, _ := c.ColumnarEvalCount(q); got != rowTruth(t, c, mixed) {
		t.Error("the answer changed after indexing")
	}
}

// TestColScopeMVCC checks visibility through the evaluator path.
func TestColScopeMVCC(t *testing.T) {
	c := scopeFixture2(t, 3000)
	defer c.Close()
	for i := 0; i < 300; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(t, fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = 4\nRequestMemory = 65536\nRequestCpus = 1\n"+
				"Owner = \"replaced\"\nCmd = \"/x\"\nWantCheckpoint = false", i, i%10))); err != nil {
			t.Fatal(err)
		}
	}
	for _, expr := range []string{`Owner == "replaced"`, `Owner == "user3"`, "RequestMemory > RequestCpus * 512"} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, served := c.ColumnarEvalCount(q)
		if !served {
			t.Fatalf("%s: declined", expr)
		}
		if want := rowTruth(t, c, expr); got != want {
			t.Errorf("%s after supersession: colScope %d != row %d", expr, got, want)
		}
	}
}

// BenchmarkColScopeShapes measures what the newly-served shapes cost against the row scan.
func BenchmarkColScopeShapes(b *testing.B) {
	c := scopeFixture2(b, 60000)
	defer c.Close()
	for _, expr := range []string{
		`Owner == "user3"`,
		`Owner == "user3" && RequestMemory > 4096`,
		"RequestMemory > RequestCpus * 512",
		"RequestMemory > 4096 || RequestCpus >= 7",
		"WantCheckpoint",
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.ColumnarEvalCount(q); !ok {
			b.Fatalf("%s declined", expr)
		}
		b.Run("colScope/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.ColumnarEvalCount(q)
			}
		})
		b.Run("rowScan/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				n := 0
				for range c.Query(q) {
					n++
				}
			}
		})
	}
}
