package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// valueCursorPair serves a table whose columns cover every shape a batch has to carry: a
// uniform int, a uniform string, a column with undefined rows, one with errors, and one that is
// an expression in every record.
func valueCursorPair(t *testing.T) (*Client, func()) {
	t.Helper()
	d, err := db.Open("")
	if err != nil {
		t.Fatal(err)
	}
	tx := d.Begin()
	for i := 0; i < 300; i++ {
		src := fmt.Sprintf("ClusterId = %d\nOwner = \"u%d\"\nRss = %d\nMem = (Rss + 1023) / 1024\n",
			i, i%7, 1024*(1+i%37))
		if i%5 == 0 {
			src += "Sparse = 1\n" // absent in the rest, so the column has undefined rows
		}
		if i%11 == 0 {
			src += "Ratio = 1 / 0\n" // an error in some rows, a value in the others
		} else {
			src += fmt.Sprintf("Ratio = %d\n", i)
		}
		if i%17 == 0 {
			src += "Mixed = \"text\"\n" // a kind the column does not pack: an exception
		} else {
			src += fmt.Sprintf("Mixed = %d\n", i)
		}
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			t.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true, IncludePrivate: true}) }()
	c := NewClient(cconn)
	return c, func() { c.Close(); s.Close(); d.Close() }
}

// TestColumnAgreesWithValueAt is the differential that makes the fast reader safe to use: for
// every cell of every column, the forward pass and the per-cell accessor must give the same
// value. They index the dense slice two different ways -- one carried forward, one recovered by
// popcount -- so a disagreement is exactly the bug a cursor is prone to.
//
// ValueAt is the oracle here rather than the thing under test, because it was the only reader
// this op had and every existing test is written against it.
func TestColumnAgreesWithValueAt(t *testing.T) {
	c, cleanup := valueCursorPair(t)
	defer cleanup()

	attrs := []string{"ClusterId", "Owner", "Rss", "Mem", "Sparse", "Ratio", "Mixed"}
	batches, cells := 0, 0
	var undef, exc int
	err := c.QueryValuesStream(context.Background(), DefaultTable, "true", attrs, 0, false,
		func(b *ValueBatch) bool {
			batches++
			var buf []classad.Value
			for col := range b.Cols {
				buf = b.Column(col, buf[:0])
				if len(buf) != b.Rows {
					t.Fatalf("column %d expanded to %d values, want %d rows", col, len(buf), b.Rows)
				}
				for row := range b.Rows {
					want := b.ValueAt(col, row)
					if msg, ok := sameValue(buf[row], want); !ok {
						t.Fatalf("%s row %d: Column gave %v, ValueAt %v (%s)",
							attrs[col], row, buf[row], want, msg)
					}
					cells++
					if want.IsUndefined() {
						undef++
					}
				}
				exc += len(b.Cols[col].Exceptions)
			}
			return true
		})
	if err != nil {
		t.Fatal(err)
	}
	if batches == 0 || cells == 0 {
		t.Fatal("nothing was read")
	}
	// The premise: the fixture has to produce the cases the two readers could disagree on.
	// Agreement over a batch of uniformly-valid columns would prove almost nothing, because
	// then the dense index is just the row index.
	if undef == 0 {
		t.Fatalf("no undefined cells in %d; the fixture must leave gaps in the validity bitmap", cells)
	}
	if exc == 0 {
		t.Fatal("no exceptions; the fixture must put cells outside their column's kind")
	}
}

// BenchmarkValueBatchReaders is the reason Column exists, as a number rather than an assertion.
// Both arms read every cell of every column; they differ only in how the dense index is reached.
func BenchmarkValueBatchReaders(b *testing.B) {
	d, err := db.Open("")
	if err != nil {
		b.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 20000; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nOwner = \"u%d\"\nRss = %d\nJobStatus = %d\nQDate = %d\nCmd = \"/bin/x\"\n",
			i, i%43, 1024*(1+i%37), 1+i%6, 1700000000+i))
		if perr != nil {
			b.Fatal(perr)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		b.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true, IncludePrivate: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	attrs := []string{"ClusterId", "Owner", "Rss", "JobStatus", "QDate", "Cmd"}
	ctx := context.Background()
	var sink int

	run := func(b *testing.B, read func(*ValueBatch) int) {
		b.ReportAllocs()
		rows := 0
		for b.Loop() {
			rows = 0
			if err := c.QueryValuesStream(ctx, DefaultTable, "true", attrs, 0, false,
				func(vb *ValueBatch) bool { sink += read(vb); rows += vb.Rows; return true }); err != nil {
				b.Fatal(err)
			}
		}
		if rows == 0 {
			b.Fatal("no rows")
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(rows), "ns/row")
	}

	// BATCH SIZE is a dimension, because the two readers differ in ORDER and not just in
	// constant: Column is one forward pass whatever the batch holds, while ValueAt recovers
	// the dense index per call and so costs more per row as the batch grows. At the default
	// the popcount hides that; the larger batch is what shows it is still there.
	for _, n := range []int{ValueBatchRows, 8 * ValueBatchRows} {
		b.Run(fmt.Sprintf("batch%d", n), func(b *testing.B) {
			old := ValueBatchRows
			ValueBatchRows = n
			defer func() { ValueBatchRows = old }()
			benchValueReaders(b, run)
		})
	}
}

func benchValueReaders(b *testing.B, run func(*testing.B, func(*ValueBatch) int)) {
	b.Run("ValueAt", func(b *testing.B) {
		run(b, func(vb *ValueBatch) int {
			n := 0
			for col := range vb.Cols {
				for row := range vb.Rows {
					if !vb.ValueAt(col, row).IsUndefined() {
						n++
					}
				}
			}
			return n
		})
	})
	b.Run("Column", func(b *testing.B) {
		var buf []classad.Value
		run(b, func(vb *ValueBatch) int {
			n := 0
			for col := range vb.Cols {
				buf = vb.Column(col, buf[:0])
				for _, v := range buf {
					if !v.IsUndefined() {
						n++
					}
				}
			}
			return n
		})
	})
}
