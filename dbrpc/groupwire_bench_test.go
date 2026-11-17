package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// Is grouping on the server still the right call once the answer has to cross a connection?
//
// db's own BenchmarkGroupServerVsClient answers this in process. That version leaves out the
// term that decides it in production: the client arm has to drag every matching record across
// the wire to fold it, while the server arm sends one frame per GROUP. A design that won in
// process and lost over the wire would be the wrong design, so measure the wire.
//
// Three arms, all through the real server over a real connection:
//
//	server       AggregateBucketed -- GROUP BY on the server, one row per group comes back
//	client-wire  QueryRawWireStream, folded straight from the wire bytes on this side
//	client-ad    QueryRawWireStream, each row decoded to a ClassAd first
//
// client-wire is the strongest client-side fold that can be written: it never builds a value,
// never decodes an ad, and keys the map on the wire bytes so a repeat owner allocates nothing.
// It is the same trick the server's own group key uses. Beating a deliberately slow baseline
// would prove nothing; this one leaves the client nothing left to optimize, so whatever gap
// remains is the pushdown's, not the baseline's. client-ad is what callers actually write.
//
// What it says, at 400k records: server moves ~726 KB in ~3.3k allocations where client-wire
// moves ~960 MB in ~3.9M -- three orders of magnitude, because the client arm pays per RECORD
// and the server arm pays per GROUP. Take the allocation columns as the result. The wall-clock
// ratio has come out anywhere from 7x to 73x on the same machine, since both arms swing by
// several-fold between runs; it is never close, and it is never the same number twice.
func benchGroupWire(b *testing.B, groups, rows int) {
	d, err := db.OpenConfig(db.Config{Dir: b.TempDir(), SegmentSize: 1 << 20})
	if err != nil {
		b.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); d.Close() }()

	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		b.Fatal(err)
	}
	n := 0
	for g := 0; g < groups; g++ {
		for r := 0; r < rows; r++ {
			if err := tx.NewClassAd(ctx, fmt.Sprintf("k%d", n), fmt.Sprintf(
				"Owner = \"o%d\"\nOwnerID = %d\nCpus = %d\nQDate = %d",
				g, g, r%16, g*1000000+r)); err != nil {
				b.Fatal(err)
			}
			n++
		}
	}
	if err := tx.Commit(ctx); err != nil {
		b.Fatal(err)
	}
	d.EnableSchemaScan(4000, 8)
	// PROVENANCE GATE, as in db's version: with every segment still active there is no columnar
	// block, so every arm would measure the per-record path and the comparison would say nothing
	// about the columnar one.
	if info := d.SchemaScanInfo(); info.CoveredSegments == 0 {
		b.Fatalf("no columnar coverage (%d sealed, %d covered): this fixture measures the brute path",
			info.SealedSegments, info.CoveredSegments)
	}

	b.Run("server", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			got, err := c.AggregateBucketed(ctx, "true", []db.GroupCol{{Attr: "Owner"}},
				[]AggSpec{{Func: db.AggCount, Arg: "*"}})
			if err != nil {
				b.Fatal(err)
			}
			if len(got) != groups {
				b.Fatalf("groups = %d, want %d", len(got), groups)
			}
		}
	})
	b.Run("client-wire", func(b *testing.B) {
		b.ReportAllocs()
		attrs := []string{"Owner"}
		for b.Loop() {
			per := map[string]int{}
			err := c.QueryRawWireStream(ctx, DefaultTable, "true", attrs, 0, false, func(row []byte) bool {
				node, ok := wire.Ad(row).LookupByName("Owner")
				if !ok {
					return true
				}
				sv, ok := wire.StringLiteralValue(node)
				if !ok {
					return true
				}
				per[string(sv)]++ // keyed on the bytes: the compiler elides the copy
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
			if len(per) != groups {
				b.Fatalf("groups = %d, want %d", len(per), groups)
			}
		}
	})
	b.Run("client-ad", func(b *testing.B) {
		b.ReportAllocs()
		attrs := []string{"Owner"}
		for b.Loop() {
			per := map[string]int{}
			err := c.QueryRawWireStream(ctx, DefaultTable, "true", attrs, 0, false, func(row []byte) bool {
				a, derr := wire.DecodeInline(row)
				if derr != nil {
					b.Fatalf("row did not decode: %v", derr)
				}
				sv, verr := classad.FromAST(a).EvaluateAttr("Owner").StringValue()
				if verr != nil {
					return true
				}
				per[sv]++
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
			if len(per) != groups {
				b.Fatalf("groups = %d, want %d", len(per), groups)
			}
		}
	})
}

func BenchmarkGroupWire(b *testing.B) {
	for _, sh := range []struct{ groups, rows int }{{100, 4000}, {1000, 400}} {
		b.Run(fmt.Sprintf("groups=%d/rows=%d", sh.groups, sh.rows), func(b *testing.B) {
			benchGroupWire(b, sh.groups, sh.rows)
		})
	}
}
