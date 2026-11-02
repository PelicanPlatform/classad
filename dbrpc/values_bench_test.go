package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// BenchmarkReadShapes compares the two ways a consumer that wants VALUES can get them over this
// connection, on the same archive, the same projection and the same loopback:
//
//	text     the reference-chasing text projection, parsed back and evaluated attribute by
//	         attribute -- what every such consumer does today
//	values   the typed value stream, read column-wise
//
// The projection deliberately includes an expression-valued attribute (MemoryUsage, written over
// its siblings), because that is the case that must be evaluated rather than read, and a
// projection of literals only would flatter both arms equally and the typed one more.
//
// Both arms do the same amount of USEFUL work: every attribute of every matching record ends up
// as a value the benchmark reads. What differs is whether a record is rendered to ClassAd source
// text for the consumer to parse straight back.
func BenchmarkReadShapes(b *testing.B) {
	// AD WIDTH is a dimension, not a detail. A six-column projection out of a nine-attribute
	// record is a different question from the same projection out of a job ad, and the narrow
	// case flatters whichever path reads the whole record: at nine attributes reassembly is
	// nearly free, so a columnar projection has almost nothing to beat. Real job ads are wide,
	// so a benchmark that only measured the narrow one would be measuring the fixture.
	for _, wide := range []bool{false, true} {
		width := "narrow"
		if wide {
			width = "wide"
		}
		b.Run(width, func(b *testing.B) {
			for _, columnar := range []bool{false, true} {
				name := "records"
				if columnar {
					name = "columnar"
				}
				b.Run(name, func(b *testing.B) { benchReadFixture(b, columnar, wide) })
			}
		})
	}
}

func benchReadFixture(b *testing.B, columnar, wide bool) {
	const n = 20000
	cat, err := db.OpenCatalog(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	defer cat.Close()
	arch, err := cat.CreateArchiveTable("history", db.ArchiveConfig{SegmentSize: 256 << 10})
	if err != nil {
		b.Fatal(err)
	}
	for i := 0; i < n; i++ {
		text := fmt.Sprintf(
			"ClusterId = %d\nProcId = %d\nOwner = \"u%d\"\nJobStatus = %d\n"+
				"ResidentSetSize = %d\nMemoryUsage = (ResidentSetSize + 1023) / 1024\n"+
				"RequestMemory = %d\nCmd = \"/bin/run%d\"\nQDate = %d\n",
			i, i%10, i%43, 1+i%6, 1024*(1+i%37), 512*(1+i%5), i%7, 1700000000+i)
		if wide {
			// Filler of the kind a real job ad carries: a mix of numbers and strings that no
			// projection here names, so they cost the read exactly what carrying them costs.
			for f := 0; f < 36; f++ {
				if f%3 == 0 {
					text += fmt.Sprintf("Pad%d = \"v%d-%d\"\n", f, f, i%97)
				} else {
					text += fmt.Sprintf("Pad%d = %d\n", f, (i*7+f)%100000)
				}
			}
		}
		ad, perr := classad.ParseOld(text)
		if perr != nil {
			b.Fatal(perr)
		}
		if err := arch.Append(ad); err != nil {
			b.Fatal(err)
		}
	}
	if columnar {
		// REQUIRED when asked for, never best-effort: a fixture that quietly failed to
		// columnarize would measure the record path while being labelled as a verdict on the
		// columnar one.
		if !arch.BuildAndEnableSchemaScan(2000, 8) {
			b.Fatal("no columnar schema was derived")
		}
		info := arch.SchemaScanInfo()
		if !info.Enabled || info.SealedSegments == 0 || info.CoveredSegments < info.SealedSegments {
			b.Fatalf("columnar coverage is %+v; every sealed segment must be covered", info)
		}
	}

	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	ctx := context.Background()
	var sink int64

	// Two projections, because the difference between them is the number that decides what to
	// build next. "expr" includes MemoryUsage, stored as an expression in every record, so the
	// server evaluates per record; "literals" is the same width with a literal in its place, so
	// nothing on either side evaluates anything. The typed arm MOVES the evaluation from the
	// consumer to the server rather than deleting it, and the pair says how much that costs.
	for _, proj := range []struct {
		name  string
		attrs []string
	}{
		{"expr", []string{"ClusterId", "Owner", "JobStatus", "MemoryUsage", "RequestMemory", "QDate"}},
		{"literals", []string{"ClusterId", "Owner", "JobStatus", "ResidentSetSize", "RequestMemory", "QDate"}},
	} {
		attrs := proj.attrs
		b.Run(proj.name, func(b *testing.B) {
			benchReadShapes(b, c, ctx, attrs, &sink)
		})
	}
	if sink == 0 {
		b.Fatal("the benchmark read nothing")
	}
}

func benchReadShapes(b *testing.B, c *Client, ctx context.Context, attrs []string, sink *int64) {
	b.Run("text", func(b *testing.B) {
		b.ReportAllocs()
		rows := 0
		for i := 0; i < b.N; i++ {
			err := c.QueryRawProjectRefsStream(ctx, "history", "true", attrs, 0, func(row string) bool {
				ad, perr := classad.ParseOld(row)
				if perr != nil {
					b.Fatal(perr)
				}
				for _, a := range attrs {
					v := ad.EvaluateAttr(a)
					if x, err := v.IntValue(); err == nil {
						*sink += x
					}
				}
				rows++
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
	})

	b.Run("values", func(b *testing.B) {
		b.ReportAllocs()
		rows := 0
		for i := 0; i < b.N; i++ {
			err := c.QueryValuesStream(ctx, "history", "true", attrs, 0, false, func(bt *ValueBatch) bool {
				for col := range attrs {
					// Column-wise, which is the shape the op exists to hand out: the dense
					// slice is read straight, with no per-row dispatch.
					cl := &bt.Cols[col]
					for _, x := range cl.Ints {
						*sink += x
					}
					for _, x := range cl.Exceptions {
						if v, err := x.Value.IntValue(); err == nil {
							*sink += v
						}
					}
				}
				rows += bt.Rows
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(rows), "ns/row")
	})
}
