package dbrpc

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// WHERE A THREE-TABLE JOIN SHOULD RUN, TIMED END TO END.
//
// The two arms answer the same statement over the same store across the same connection, and
// differ only in where the pairing happens: `client` reads each table in turn, narrowing every
// table after the first to the keys the rows so far hold, and pairs them itself; `store` sends
// one chain request and reads the joined rows back.
//
// BOTH ARMS GET THE SAME JOIN ORDER, and each order is measured separately. That is the whole
// control: a client that starts its chain at the selective table is a client that already
// knows which table is selective, and that estimate comes from the indexes, the zone maps and
// the row counts, all of which are on the server. Handing it to one arm and not the other
// measures the estimate rather than the join.
//
// ALLOCATIONS ARE REPORTED BESIDE THE TIME and are the more trustworthy of the two here: the
// clock on this machine has shown 15% spread between rounds. Each pair of arms is run twice
// with the order of the two arms reversed, so a warm-up advantage lands on a different arm
// each round and the best round per arm is the one to read.
//
// The headline shape puts `history` in an ARCHIVE table, which is what it is in the mirror
// this is wanted for, and the other two in mutable tables.
//
// MEASURED, 4096 jobs, go test -benchtime 20x, best round per arm:
//
//	shape      order            client      store       store is      client allocs  store allocs
//	expanding  as-written       54.8 ms     48.2 ms     1.14x faster       943,016   767,373 (0.81x)
//	expanding  selective-first  51.8 ms     42.7 ms     1.21x faster       832,421   684,520 (0.82x)
//	filtering  as-written       44.6 ms     22.7 ms     1.96x faster       690,012   285,408 (0.41x)
//	filtering  selective-first  57.8 ms     19.3 ms     3.00x faster     1,429,427   219,464 (0.15x)
//
// The store is faster in EVERY cell, and allocates less in every cell. Read the allocations
// first: they repeat to within 0.01% between rounds, where the times move by up to 7%.
//
// TWO THINGS IN THAT TABLE ARE WORTH MORE THAN THE RATIOS.
//
// The expanding chain is the case where the store ships MORE -- a joined row carries every
// side, so an expanding chain repeats earlier sides once per output row, measured elsewhere in
// this package at 2.30x the client's bytes. It is still faster. Bytes and time point opposite
// ways here, and only one of them was ever the question.
//
// And the client gets SLOWER when it starts at the selective table, on the shape where that
// ordering ships the least: 44.6 ms to 57.8 ms, with allocations doubling. The reason is that
// a client narrows a later read by sending a constraint, and a constraint over an archive scan
// is a filter rather than an access path -- so an order chosen to minimize what crosses the
// wire maximizes what is scanned behind it. The store moves the other way on the same change,
// 22.7 ms to 19.3 ms, because it resolves the same narrowing through an index. Ordering a join
// well and executing it where the indexes are is the same decision, and splitting them across
// two processes gets it wrong in both directions.
//
// ONE ASYMMETRY TO DECLARE. The client arm reads each table by the fastest route it has: wire
// rows from a mutable table, and a text projection from the archive, because the wire-row
// opcode is not served from an archive. The store's join reads the archive's rows in wire form
// internally. That gap is real and it is the server's to have -- but it is a property of the
// read path, not of the pairing, and a reader comparing the arms should know it is in there.

const (
	benchHistWidth   = 40
	benchEpochWidth  = 20
	benchMetricWidth = 15
	benchEpochsPer   = 4
)

// chainBenchFixture builds history (archive), epoch_history and job_metrics, behind a
// connected client/server pair. metricEvery = 1 makes the chain expand; a larger value makes
// it filter.
func chainBenchFixture(tb testing.TB, jobs, metricEvery int) *Client {
	tb.Helper()
	cat, err := db.OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	pad := func(b *strings.Builder, prefix string, n int) {
		for k := 0; k < n; k++ {
			fmt.Fprintf(b, `; %s%02d = "a padding attribute value"`, prefix, k)
		}
	}
	parse := func(text string) *classad.ClassAd {
		ad, perr := classad.Parse(text)
		if perr != nil {
			tb.Fatal(perr)
		}
		return ad
	}
	hist, err := cat.CreateArchiveTable("history", db.ArchiveConfig{
		SegmentSize: 256 << 10,
		ZoneAttrs:   []string{"ClusterId"},
		ValueAttrs:  []string{"ClusterId"},
	})
	if err != nil {
		tb.Fatal(err)
	}
	mk := func(name string) *db.DB {
		d, cerr := cat.CreateTable(name)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return d
	}
	epoch, metric := mk("epoch_history"), mk("job_metrics")

	for i := 0; i < jobs; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ ClusterId = %d; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		pad(&b, "HAttr", benchHistWidth-4)
		b.WriteString(" ]")
		if werr := hist.Append(parse(b.String())); werr != nil {
			tb.Fatal(werr)
		}
		for e := 0; e < benchEpochsPer; e++ {
			var eb strings.Builder
			fmt.Fprintf(&eb, `[ ClusterId = %d; NumShadowStarts = %d; MemoryUsage = %d`, i, e+1, 900+e*200)
			pad(&eb, "EAttr", benchEpochWidth-3)
			eb.WriteString(" ]")
			if werr := epoch.Put(fmt.Sprintf("e%d.%d", i, e), parse(eb.String())); werr != nil {
				tb.Fatal(werr)
			}
		}
		if i%metricEvery == 0 {
			var mb strings.Builder
			fmt.Fprintf(&mb, `[ ClusterId = %d; CpuEfficiency = %d; PeakRss = %d`, i, 50+i%50, 2048+i%1024)
			pad(&mb, "MAttr", benchMetricWidth-3)
			mb.WriteString(" ]")
			if werr := metric.Put(fmt.Sprintf("m%d", i), parse(mb.String())); werr != nil {
				tb.Fatal(werr)
			}
		}
	}

	srv := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
	tb.Cleanup(func() { c.Close(); srv.Close(); cat.Close() })
	return c
}

// benchSides describes the three tables, in the order the chain visits them.
func benchSides(order []string) []nwaySide {
	byName := map[string]nwaySide{
		"history":       {table: "history", alias: "h", proj: []string{"ClusterId", "Owner", "RequestMemory"}},
		"epoch_history": {table: "epoch_history", alias: "e", proj: []string{"ClusterId", "NumShadowStarts", "MemoryUsage"}},
		"job_metrics":   {table: "job_metrics", alias: "m", proj: []string{"ClusterId", "CpuEfficiency", "PeakRss"}},
	}
	out := make([]nwaySide, 0, len(order))
	for _, n := range order {
		out = append(out, byName[n])
	}
	return out
}

// chainRequestFor builds the store-side plan for one visiting order. Every step joins on
// ClusterId, and the row half names the FROM side explicitly -- which is what makes the key
// set of every step knowable from one scan of it.
func chainRequestFor(sides []nwaySide) JoinRequest {
	spec := db.JoinSpec{Left: db.JoinSide{
		Table: sides[0].table, Alias: sides[0].alias, Project: sides[0].proj,
	}}
	for _, s := range sides[1:] {
		spec.Steps = append(spec.Steps, db.JoinStep{
			Kind: db.JoinInner,
			Side: db.JoinSide{Table: s.table, Alias: s.alias, Project: s.proj},
			Cond: []string{sides[0].alias + ".ClusterId == " + s.alias + ".ClusterId"},
			Equi: []db.JoinEqui{{Left: sides[0].alias + ".ClusterId", Right: "ClusterId"}},
		})
	}
	return JoinRequest{Spec: spec}
}

// benchClientFetch reads one table by the fastest route the CLIENT has to it: wire rows from a
// mutable table, a text projection from the archive, which does not serve them.
func benchClientFetch(c *Client) nwayFetch {
	ctx := context.Background()
	return func(table, constraint string, proj []string) ([]*classad.ClassAd, int, error) {
		var ads []*classad.ClassAd
		if table != "history" {
			err := c.QueryRawWireStream(ctx, table, constraint, proj, 0, false, func(row []byte) bool {
				a, derr := wire.DecodeInline(row)
				if derr != nil {
					return false
				}
				ads = append(ads, classad.FromAST(a))
				return true
			})
			return ads, 0, err
		}
		rows, err := c.QueryRawProject(ctx, table, constraint, proj, 0)
		if err != nil {
			return nil, 0, err
		}
		for _, r := range rows {
			ad, perr := classad.ParseOld(r)
			if perr != nil {
				return nil, 0, perr
			}
			ads = append(ads, ad)
		}
		return ads, 0, nil
	}
}

// BenchmarkJoinChainWhereItRuns is the measurement: the same three-table statement, the same
// join order, paired in the client and paired in the store.
func BenchmarkJoinChainWhereItRuns(b *testing.B) {
	const jobs = 4096
	orders := map[string][]string{
		// The order a person writes, which is also the order the tables are named in.
		"as-written": {"history", "epoch_history", "job_metrics"},
		// The selective table first, which is the order a cost-based planner would pick.
		"selective-first": {"job_metrics", "history", "epoch_history"},
	}
	for _, shape := range []struct {
		name        string
		metricEvery int
	}{
		{"expanding", 1},
		{"filtering", 50},
	} {
		c := chainBenchFixture(b, jobs, shape.metricEvery)
		for _, orderName := range []string{"as-written", "selective-first"} {
			sides := benchSides(orders[orderName])
			client := func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					rows, _, err := runNway(benchClientFetch(c), sides)
					if err != nil {
						b.Fatal(err)
					}
					if len(rows) == 0 {
						b.Fatal("the client arm produced no rows, so it measures nothing")
					}
				}
			}
			store := func(b *testing.B) {
				b.ReportAllocs()
				jr := chainRequestFor(sides)
				for i := 0; i < b.N; i++ {
					n := 0
					err := c.JoinWire(context.Background(), jr, false, func([]byte) bool {
						n++
						return true
					})
					if err != nil {
						b.Fatal(err)
					}
					if n == 0 {
						b.Fatal("the store arm produced no rows, so it measures nothing")
					}
				}
			}
			// Each arm runs in both positions, so a warm-up advantage lands on a different
			// arm each round. Read the best round per arm.
			base := shape.name + "/" + orderName
			b.Run(base+"/round1/client", client)
			b.Run(base+"/round1/store", store)
			b.Run(base+"/round2/store", store)
			b.Run(base+"/round2/client", client)
		}
	}
}

// TestJoinChainArmsAgree keeps the benchmark honest: two arms that disagree about the answer
// are not two ways of doing the same thing, and a benchmark over them compares nothing.
func TestJoinChainArmsAgree(t *testing.T) {
	for _, metricEvery := range []int{1, 50} {
		c := chainBenchFixture(t, 512, metricEvery)
		for _, order := range [][]string{
			{"history", "epoch_history", "job_metrics"},
			{"job_metrics", "history", "epoch_history"},
		} {
			sides := benchSides(order)
			rows, _, err := runNway(benchClientFetch(c), sides)
			if err != nil {
				t.Fatalf("client arm: %v", err)
			}
			n := 0
			if jerr := c.JoinWire(context.Background(), chainRequestFor(sides), false,
				func([]byte) bool { n++; return true }); jerr != nil {
				t.Fatalf("store arm: %v", jerr)
			}
			if n != len(rows) {
				t.Errorf("metricEvery=%d order=%v: client produced %d rows, the store %d",
					metricEvery, order, len(rows), n)
			}
			if n == 0 {
				t.Errorf("metricEvery=%d order=%v produced no rows", metricEvery, order)
			}
		}
	}
}
