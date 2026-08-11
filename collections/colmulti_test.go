package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// multiFixture builds job-shaped ads with several numeric columns, plus the exceptional shapes that
// make a per-field column read disagree with a naive one: a missing value, a non-numeric value, and a
// value too wide for its fitted slot. Rates are ~1% so every attribute still clears buildAdSchema's
// 90% presence threshold and stays a schema FIELD -- otherwise the accelerator declines for an
// unrelated reason and the tests pass vacuously.
func multiFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 16})
	for i := 0; i < n; i++ {
		mem := fmt.Sprintf("%d", 1024+(i%32)*512)
		cpus := fmt.Sprintf("%d", 1+i%8)
		switch {
		case i%101 == 0:
			mem = "" // RequestMemory missing entirely
		case i%103 == 0:
			mem = "\"lots\"" // non-numeric: comparison is UNDEFINED
		case i%107 == 0:
			mem = fmt.Sprintf("%d", 1<<40+i) // too wide for the fitted slot: escapes
		}
		src := fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestCpus = %s\n"+
			"RemoteWallClockTime = %d.5\nOwner = \"u%d\"", i/10, i%10, 1+i%5, cpus, i%3600, i%32)
		if mem != "" {
			src += "\nRequestMemory = " + mem
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), mustAdOld(tb, src)); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"RequestMemory >= 0", "RequestCpus >= 0", "JobStatus >= 0", "ProcId >= 0"} {
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

var multiQueries = []string{
	"RequestMemory > 4096",                                                // one field: the old fast path
	"RequestMemory > 4096 && RequestCpus >= 4",                            // two fields
	"JobStatus == 4 && RequestMemory > 2048 && ProcId < 5",                // three fields
	"RequestMemory > 2048 && RequestMemory < 8192",                        // two probes, one field
	"RequestMemory > 2048 && RequestMemory < 8192 && RequestCpus == 4",    // both at once
	"RemoteWallClockTime > 100.0 && RequestCpus >= 2",                     // a REAL field and an int field
	"JobStatus == 4 && RequestCpus >= 1 && RequestMemory > 1000000000000", // only wide/escaped values match
}

// TestMultiFieldCountMatchesRowPath is the equivalence test: whatever the columnar path serves must
// equal the ordinary query path, including where a value is missing, non-numeric, or escaped.
func TestMultiFieldCountMatchesRowPath(t *testing.T) {
	c := multiFixture(t, 4000)
	defer c.Close()
	for _, expr := range multiQueries {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range c.Query(q) {
			want++
		}
		got, served := c.CountQuery(q)
		if !served {
			t.Errorf("%s: declined; a conjunction of scalar numeric comparisons should be columnar", expr)
			continue
		}
		if got != want {
			t.Errorf("%s: columnar %d != row %d", expr, got, want)
		}
		t.Logf("%-68s columnar=%-6d row=%d", expr, got, want)
	}
}

// TestMultiFieldStillDeclinesWhatItMust guards the boundary. These must NOT be served: the probes do
// not cover the query, or the shape is not a scalar comparison on a numeric field.
func TestMultiFieldStillDeclinesWhatItMust(t *testing.T) {
	c := multiFixture(t, 2000)
	defer c.Close()
	for _, expr := range []string{
		"RequestMemory > 4096 && ClusterId != ProcId", // attr-to-attr conjunct: omitted from probes
		"RequestMemory > 4096 && Owner == \"u3\"",     // a string field is not numeric
		"RequestMemory > 4096 || RequestCpus >= 4",    // disjunction
		"RequestMemory > RequestCpus",                 // no literal
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range c.Query(q) {
			want++
		}
		got, served := c.CountQuery(q)
		if served && got != want {
			t.Errorf("%s: served with a WRONG answer %d (row=%d)", expr, got, want)
		}
		if served {
			t.Logf("%-46s served, and agrees (%d)", expr, got)
		} else {
			t.Logf("%-46s declined (row=%d)", expr, want)
		}
	}
}

// TestMultiFieldMVCC checks visibility: superseded versions must not be counted, and an update that
// changes one of several predicated fields must move the answer.
func TestMultiFieldMVCC(t *testing.T) {
	c := multiFixture(t, 3000)
	defer c.Close()
	// Supersede a slice of records so they no longer satisfy RequestCpus >= 4.
	for i := 0; i < 500; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAdOld(t, fmt.Sprintf("ClusterId = %d\nProcId = %d\nJobStatus = 4\nRequestCpus = 1\n"+
				"RequestMemory = %d\nRemoteWallClockTime = 1.5\nOwner = \"u%d\"",
				i/10, i%10, 1024+(i%32)*512, i%32))); err != nil {
			t.Fatal(err)
		}
	}
	q, err := vm.Parse("RequestMemory > 4096 && RequestCpus >= 4")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for range c.Query(q) {
		want++
	}
	got, served := c.CountQuery(q)
	if !served {
		t.Fatal("declined after an update")
	}
	if got != want {
		t.Errorf("columnar %d != row %d after supersession", got, want)
	}
}

// BenchmarkMultiFieldCount is the point: what the multi-field column scan is worth against the row
// scan it replaces, and against the single-field special case on identical work.
func BenchmarkMultiFieldCount(b *testing.B) {
	c := multiFixture(b, 60000)
	defer c.Close()
	for _, expr := range []string{
		"RequestMemory > 4096",
		"RequestMemory > 4096 && RequestCpus >= 4",
		"JobStatus == 4 && RequestMemory > 2048 && ProcId < 5",
	} {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.CountQuery(q); !ok {
			b.Fatalf("%s declined", expr)
		}
		b.Run("columnar/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				c.CountQuery(q)
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

// BenchmarkSingleFieldRoute guards the common case. Every single-field query now goes through the
// multi-field scan, so it must not be slower than the path it replaced -- one narrowing pass over one
// column against one strided pass with a direct callback.
func BenchmarkSingleFieldRoute(b *testing.B) {
	c := multiFixture(b, 60000)
	defer c.Close()
	st := c.schemaScan.Load()
	q, err := vm.Parse("RequestMemory > 4096")
	if err != nil {
		b.Fatal(err)
	}
	preds, ok := c.numPredsOnFields(q, st.schema)
	if !ok || len(preds) != 1 {
		b.Fatalf("expected one combined predicate, got %d (ok=%v)", len(preds), ok)
	}
	fieldID, eval, ok := c.numPredOnField(q, st.schema)
	if !ok {
		b.Fatal("single-field analysis declined")
	}
	// Same answer from both, or the comparison is meaningless.
	if a, bb := c.schemaScanCountMulti(preds, st.cache), c.schemaScanCount(fieldID, st.cache, eval); a != bb {
		b.Fatalf("multi=%d single=%d", a, bb)
	}
	b.Run("multiPath", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.schemaScanCountMulti(preds, st.cache)
		}
	})
	b.Run("oldSinglePath", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.schemaScanCount(fieldID, st.cache, eval)
		}
	})
}
