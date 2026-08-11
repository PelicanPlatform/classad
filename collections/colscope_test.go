package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// scopeFixture builds job-shaped ads with several numeric columns, so a query can combine them.
func scopeFixture(tb testing.TB, n int) *Collection {
	tb.Helper()
	c := New(Options{Shards: 1, SegmentSize: 1 << 20})
	for i := 0; i < n; i++ {
		ad := mustAdOld(tb, fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nJobStatus = %d\nRequestMemory = %d\nRequestCpus = %d\n"+
				"Owner = \"user%d\"\nCmd = \"/home/user%d/run.sh\"\nArgs = \"--in in%d.dat\"\n"+
				"RemoteWallClockTime = %d.5",
			i/10, i%10, 1+i%5, 1024+(i%32)*512, 1+i%8, i%512, i%512, i, i%7200))
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			tb.Fatal(err)
		}
	}
	for _, e := range []string{"ProcId >= 0", "RequestMemory >= 0", "RequestCpus >= 0", "JobStatus >= 0"} {
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

// scopeQueries are shapes today's columnar path CANNOT serve: multiple fields, and attribute-to-
// attribute comparisons.
var scopeQueries = []string{
	"RequestMemory > 4096 && RequestCpus >= 4",
	"JobStatus == 4 && RequestMemory > 2048 && ProcId < 5",
	"RequestMemory > RequestCpus * 512",
	"ProcId >= 5 && ClusterId != ProcId",
}

// TestColEvalCountMatchesRowPath is the correctness gate for the prototype: whatever it agrees to
// answer must equal the row path, since it evaluates the real query rather than a summary of it.
func TestColEvalCountMatchesRowPath(t *testing.T) {
	c := scopeFixture(t, 20000)
	defer c.Close()
	for _, expr := range scopeQueries {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		want := 0
		for range c.Query(q) {
			want++
		}
		// Today's fast path must decline these -- that is the point of the exercise.
		if _, served := c.CountQuery(q); served {
			t.Logf("note: %s is already served by a special case", expr)
		}
		got, ok := c.colEvalCount(q)
		if !ok {
			t.Errorf("%s: prototype declined; it should serve a native numeric query", expr)
			continue
		}
		if got != want {
			t.Errorf("%s: colScope %d != row %d", expr, got, want)
		}
		t.Logf("%-46s colScope=%-6d row=%d", expr, got, want)
	}
}

// BenchmarkColEvalVsRow is the question the prototype exists to answer.
func BenchmarkColEvalVsRow(b *testing.B) {
	c := scopeFixture(b, 20000)
	defer c.Close()
	forceBlocksEverywhere(b, c) // else the blockless active segment's full decode dominates
	for _, expr := range scopeQueries {
		q, err := vm.Parse(expr)
		if err != nil {
			b.Fatal(err)
		}
		if _, ok := c.colEvalCount(q); !ok {
			b.Fatalf("%s: prototype declined", expr)
		}
		if lastColEvalSplit[1] != 0 {
			b.Fatalf("%s: %d records fell to a full decode; the number would be confounded",
				expr, lastColEvalSplit[1])
		}
		b.Run("colScope/"+expr, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := c.colEvalCount(q); !ok {
					b.Fatal("declined mid-benchmark")
				}
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

// BenchmarkColEvalOverhead isolates the interpreter cost from the column-read cost: the same
// single-field query served three ways over identical data. The special case reads the same column
// and does the same comparison, so the gap between it and colScope IS the per-record cost of running
// the query through the evaluator.
func BenchmarkColEvalOverhead(b *testing.B) {
	c := scopeFixture(b, 20000)
	defer c.Close()
	q, err := vm.Parse("ProcId >= 5")
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := c.CountQuery(q); !ok {
		b.Skip("special case declined")
	}
	if _, ok := c.colEvalCount(q); !ok {
		b.Skip("prototype declined")
	}
	b.Run("specialCase", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.CountQuery(q)
		}
	})
	b.Run("colScope", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.colEvalCount(q)
		}
	})
	b.Run("rowScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for range c.Query(q) {
				n++
			}
		}
	})
}

// forceBlocksEverywhere gives EVERY segment a columnar block, including the active append target, so
// a benchmark measures columnar evaluation rather than the decode of a blockless window.
func forceBlocksEverywhere(tb testing.TB, c *Collection) {
	tb.Helper()
	st := c.schemaScan.Load()
	if st == nil {
		tb.Fatal("schema scan not enabled")
	}
	for _, sh := range c.shards {
		sh.mu.RLock()
		segs := append([]*segment(nil), sh.segs...)
		sh.mu.RUnlock()
		for _, seg := range segs {
			if seg == nil || seg.used == 0 || seg.colblk.Load() != nil {
				continue
			}
			d := seg.dict.Load()
			bl, offs := buildColumnarFromSegment(seg.data, seg.used, seg.codec, c.regionCodec(),
				st.schema, st.hot, defaultColGrouping(),
				func(dst, w []byte) ([]byte, bool) { return c.recordToInternedDict(d, dst, w) })
			seg.colblk.Store(&colSegment{blocks: bl, offs: offs})
		}
	}
}

// BenchmarkColEvalPure measures columnar evaluation with NO blockless window, and asserts the split
// so the measurement cannot silently include a full decode.
func BenchmarkColEvalPure(b *testing.B) {
	c := scopeFixture(b, 20000)
	defer c.Close()
	forceBlocksEverywhere(b, c)
	q, err := vm.Parse("ProcId >= 5")
	if err != nil {
		b.Fatal(err)
	}
	if _, ok := c.colEvalCount(q); !ok {
		b.Skip("declined")
	}
	if lastColEvalSplit[1] != 0 {
		b.Fatalf("%d records still went through a full decode; the measurement would be confounded",
			lastColEvalSplit[1])
	}
	b.Logf("all %d records served columnar", lastColEvalSplit[0])
	b.Run("specialCase", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.CountQuery(q)
		}
	})
	b.Run("colScope", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			c.colEvalCount(q)
		}
	})
	b.Run("rowScan", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			n := 0
			for range c.Query(q) {
				n++
			}
		}
	})
}
