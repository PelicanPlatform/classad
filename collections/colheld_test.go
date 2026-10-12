package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// heldFixture is a columnarized collection whose RequestMemory -- a schema field -- is an EXPRESSION in
// one record in 50 and a REAL (out of its int slot) in one in 97, in the older segments only. Weight is
// an int field with the same reals and no expression, so the columnar paths still serve it. The newest
// records hold plain integers, so the open segment, whose row walk would decline on an expression, does
// not mask a columnar reader that mishandles one. It is closed and reopened, so the columns are read
// back from disk with this process's attribute numbering.
func heldFixture(t *testing.T) *Collection {
	t.Helper()
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	put := func(c *Collection, i int, text string) {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	c := open()
	for i := range 3000 {
		mem, weight := fmt.Sprint((i%16)*1024), fmt.Sprint(i%9)
		switch {
		case i%50 == 1:
			mem = "ProcId * 512 + 7"
		case i%97 == 3:
			mem, weight = "1.5", "2.5"
		}
		// Rare attributes first in some records, so the segment dictionaries number names differently
		// from the collection's intern table.
		put(c, i, fmt.Sprintf(`[ Rare%d = 1; ClusterId = %d; ProcId = %d; Owner = "user%d"; JobStatus = %d; RequestMemory = %s; Weight = %s ]`,
			i%13, i, i%10, i%7, i%6, mem, weight))
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	for i := 3000; i < 4500; i++ {
		put(c, i, fmt.Sprintf(`[ ClusterId = %d; ProcId = %d; Owner = "user%d"; JobStatus = %d; RequestMemory = %d; Weight = %d ]`,
			i, i%10, i%7, i%6, (i%16)*1024, i%9))
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c = open()
	c.BuildAndEnableSchemaScan(4096, 8)
	return c
}

// TestColumnarizedHeldExpressionsAreNotUndefined pins the reader contract colheld.go establishes: in a
// columnarized segment, a schema field whose value stayed in the record (an expression) is NOT
// undefined, though the block -- escape bit set, nothing in the cold tail, escape class MISSING --
// describes it exactly as it would a missing attribute. And a value that escaped INTO the cold tail
// (the reals) is found there under the id the tail was written with.
//
// Every columnar count, presence count, grouped count and numeric aggregate must agree with the row
// scan. Before the fix the presence and vectorized counts called the expressions undefined, the numeric
// paths skipped them, and the numeric paths missed every escaped real after a reopen.
func TestColumnarizedHeldExpressionsAreNotUndefined(t *testing.T) {
	c := heldFixture(t)
	defer c.Close()
	if segs, _ := ColumnarizedSegments(); segs == 0 {
		t.Fatal("nothing was columnarized")
	}
	parse := func(e string) *vm.Query {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		return q
	}

	for _, e := range []string{
		"RequestMemory is undefined",   // presence count
		"RequestMemory isnt undefined", // presence count
		"isUndefined(RequestMemory)",   // vectorized
		"(RequestMemory ?: -1) >= 0",   // vectorized
		"RequestMemory >= 0",           // numeric column count
		"RequestMemory == 1.5",         // the escaped reals alone
		"RequestMemory >= 0 && JobStatus < 3",
		"RequestMemory > 1000 || ProcId == 1",
	} {
		want := len(readAll(t, c, e))
		q := parse(e)
		// CountQuery answers any native query (its last tier evaluates per record where it must), so a
		// decline there would only hide a wrong answer.
		if got, ok := c.CountQuery(q); (!ok && q.Native()) || (ok && got != want) {
			t.Errorf("CountQuery(%s) = %d (ok=%v), row scan %d", e, got, ok, want)
		}
		if got, ok := c.PresenceCountQuery(q); ok && got != want {
			t.Errorf("PresenceCountQuery(%s) = %d, row scan %d", e, got, want)
		}
		if got, ok := c.VectorEvalCount(q); ok && got != want {
			t.Errorf("VectorEvalCount(%s) = %d, row scan %d", e, got, want)
		}
		if gs, ok := c.GroupStatsQuery(q, "JobStatus", nil); ok {
			sum := 0
			for _, g := range gs {
				sum += g.Count
			}
			if sum != want {
				t.Errorf("GroupStatsQuery(%s) BY JobStatus sums to %d, row scan %d", e, sum, want)
			}
		}
	}

	// A numeric aggregate over the expression-valued field must not answer from the columns: skipping
	// the expressions as if absent gives a smaller COUNT(attr), and possibly a wrong MIN/MAX/SUM.
	all := parse("ProcId >= 0")
	n := len(readAll(t, c, "RequestMemory isnt undefined"))
	if st, ok := c.NumStatsQuery(all, "RequestMemory"); ok && st.N != n {
		t.Errorf("NumStatsQuery(RequestMemory) counted %d values, row scan has %d", st.N, n)
	}
	if gs, ok := c.GroupStatsQuery(all, "JobStatus", []string{"RequestMemory"}); ok {
		got := 0
		for _, g := range gs {
			got += g.Stats[0].N
		}
		if got != n {
			t.Errorf("GroupStatsQuery COUNT(RequestMemory) = %d, row scan has %d", got, n)
		}
	}
	// Weight escapes the same reals with no expression anywhere, so the column paths must serve it --
	// and find each real in the cold tail under the id the tail was written with. Before the fix they
	// looked under this process's id and, after the reopen, found almost none.
	for _, e := range []string{"Weight == 2.5", "Weight > 2 && Weight < 3", "Weight >= 0"} {
		want := len(readAll(t, c, e))
		q := parse(e)
		if got, ok := c.CountQuery(q); !ok || got != want {
			t.Errorf("CountQuery(%s) = %d (ok=%v), row scan %d", e, got, ok, want)
		}
		st, ok := c.NumStatsQuery(q, "Weight")
		if !ok || st.N != want {
			t.Errorf("NumStatsQuery(%s) counted %d (ok=%v), row scan %d", e, st.N, ok, want)
		}
	}
}

// TestColumnPathsDeclineOnRowExpressions: the numeric column paths' ROW fallback -- what reads the open
// segment, and any segment without a block -- meets expressions too. Comparing or aggregating one needs
// evaluating it, so it must give the query back rather than skip the record as if the value were absent.
func TestColumnPathsDeclineOnRowExpressions(t *testing.T) {
	c := heldFixture(t)
	defer c.Close()
	// Weight is expression-free so far; these land in the open segment, which has no block.
	for i := 4500; i < 4520; i++ {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId = %d; ProcId = %d; Owner = "user1"; JobStatus = 2; RequestMemory = 1024; Weight = ProcId + 100 ]`, i, i%10))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	for _, e := range []string{"Weight >= 100", "Weight >= 0 && JobStatus == 2"} {
		q, err := vm.Parse(e)
		if err != nil {
			t.Fatal(err)
		}
		want := len(readAll(t, c, e))
		if got, ok := c.CountQuery(q); ok && got != want {
			t.Errorf("CountQuery(%s) = %d, row scan %d", e, got, want)
		}
		if st, ok := c.NumStatsQuery(q, "Weight"); ok && st.N != want {
			t.Errorf("NumStatsQuery(%s) counted %d, row scan %d", e, st.N, want)
		}
		if gs, ok := c.GroupStatsQuery(q, "JobStatus", []string{"Weight"}); ok {
			n := 0
			for _, g := range gs {
				n += g.Stats[0].N
			}
			if n != want {
				t.Errorf("GroupStatsQuery(%s) COUNT(Weight) = %d, row scan %d", e, n, want)
			}
		}
	}
}
