package dbrpc

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// WHERE A JOIN'S TIME GOES ONCE IT CROSSES THE WIRE.
//
// opJoin renders every joined row to old-ClassAd text on the server and sends it in a frame
// of its own. The read path stopped doing both of those: opQueryRawWire ships wire-form rows
// batched to a byte budget, and the text render happens once at the consumer's edge, if at
// all. This measures what that difference is worth to a join, before any of it is built.
//
// WHAT IS DELIBERATELY NOT MEASURED HERE. Bytes on the wire. A joined row carries both sides,
// so an expanding join repeats the left ad once per output row and ships more than the two
// inputs were -- structurally, at a crossover that no encoding moves, because an encoding
// scales both arms alike. That is a question for a cost estimate, not for a wire format, and
// it is settled separately from this.
//
// THE SHAPE is the one the join is wanted for: history joined to epoch_history, to watch one
// job's requested memory change across its run attempts. A completed-job ad is wide and a
// run-attempt ad is narrower, several per job, and the query wants a handful of attributes
// out of both.

const (
	rpcHistWidth  = 80 // attributes on a completed-job ad
	rpcEpochWidth = 25 // attributes on a run-attempt ad
	rpcEpochs     = 4  // run attempts per job
)

// rpcJoinFixture builds the two tables and a connected client/server pair over a pipe.
func rpcJoinFixture(tb testing.TB, jobs int) *joinFixture {
	tb.Helper()
	cat, cleanup := rpcJoinCatalog(tb, jobs)
	f := &joinFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	// The handshake the application carries over CEDAR, simulated: without it the client
	// correctly declines to send a join at all.
	f.c = NewClient(cconn, WithPeerSessionAttrs(f.s.SessionAttrs()))
	tb.Cleanup(func() { f.c.Close(); f.s.Close(); cleanup() })
	return f
}

// rpcJoinCatalog builds the two tables, for a caller that wants to choose the transport.
func rpcJoinCatalog(tb testing.TB, jobs int) (*db.Catalog, func()) {
	tb.Helper()
	cat, err := db.OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	hist, err := cat.CreateTable("history")
	if err != nil {
		tb.Fatal(err)
	}
	epoch, err := cat.CreateTable("epoch_history")
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < jobs; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ ClusterId = %d; ProcId = 0; Owner = "user%02d"; JobStatus = 4; RequestMemory = %d`,
			i, i%32, 1024+(i%8)*512)
		for k := 0; k < rpcHistWidth-5; k++ {
			fmt.Fprintf(&b, `; HAttr%02d = "a completed job attribute value"`, k)
		}
		b.WriteString(" ]")
		ad, perr := classad.Parse(b.String())
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprintf("h%d", i), ad); werr != nil {
			tb.Fatal(werr)
		}
		for e := 0; e < rpcEpochs; e++ {
			var eb strings.Builder
			fmt.Fprintf(&eb, `[ ClusterId = %d; ProcId = 0; NumShadowStarts = %d; RequestMemory = %d; MemoryUsage = %d`,
				i, e+1, 1024+(i%8)*512+e*256, 900+e*200)
			for k := 0; k < rpcEpochWidth-5; k++ {
				fmt.Fprintf(&eb, `; EAttr%02d = "a run attempt attribute value"`, k)
			}
			eb.WriteString(" ]")
			ead, perr := classad.Parse(eb.String())
			if perr != nil {
				tb.Fatal(perr)
			}
			if werr := epoch.Put(fmt.Sprintf("e%d.%d", i, e), ead); werr != nil {
				tb.Fatal(werr)
			}
		}
	}

	return cat, func() { cat.Close() }
}

func rpcJoinRequest() JoinRequest {
	return JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner", "RequestMemory"}},
		Right: db.JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts", "RequestMemory", "MemoryUsage"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
}

// BenchmarkProbeWhereTheRPCJoinSpends splits the crossing into parts that can be aimed at
// separately. NOT a before/after: these are components, not alternatives.
//
// (a) is what a caller gets today, text handed over as text. (b) adds the parse that caller
// then has to do to sort or project the rows, which is where the text actually costs
// something. The gap between them is what a wire-form row would remove from the consumer, and
// (a) alone is what batching and the server's render reach.
func BenchmarkProbeWhereTheRPCJoinSpends(b *testing.B) {
	const jobs = 2000
	f := rpcJoinFixture(b, jobs)
	jr := rpcJoinRequest()
	ctx := context.Background()
	want := jobs * rpcEpochs

	// (a) the join as it crosses today: server renders text, one frame per row, caller keeps
	// the string.
	b.Run("rows-as-text", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := f.c.Join(ctx, jr, func(row string) bool { n++; return true })
			if err != nil {
				b.Fatal(err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	})

	// (b) the same, with the parse a caller needs before it can sort, project or filter the
	// rows -- which every consumer of a joined row does.
	b.Run("rows-as-text-then-parsed", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := f.c.Join(ctx, jr, func(row string) bool {
				ad, perr := classad.Parse(row)
				if perr != nil || ad == nil {
					b.Fatalf("row did not parse: %v", perr)
				}
				n++
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	})

	// (c) the same join run in the store with nothing crossing a wire, so the difference
	// between this and (a) is the crossing itself: the render and the framing.
	b.Run("in-process-no-wire", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var st db.JoinStats
			seq, err := f.cat.Join(jr.Spec, db.JoinLimits{}, &st)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for range seq {
				n++
			}
			if st.Err != nil {
				b.Fatal(st.Err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	})

	// (d) the server's render alone, over the same rows: adString per joined row, no wire.
	// The gap between (c) and (d) is what a wire-form row removes from the SERVER.
	b.Run("in-process-plus-render", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			var st db.JoinStats
			seq, err := f.cat.Join(jr.Spec, db.JoinLimits{}, &st)
			if err != nil {
				b.Fatal(err)
			}
			n := 0
			for ad := range seq {
				if s := ad.String(); s == "" {
					b.Fatal("empty render")
				}
				n++
			}
			if st.Err != nil {
				b.Fatal(st.Err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	})
}

// bufferedPipe is netPipe over a real socket pair, so a frame is handed to the kernel rather
// than to a blocked goroutine.
//
// It exists because net.Pipe is SYNCHRONOUS and unbuffered: every WriteMsg blocks until the
// reader takes it, and pipeConn writes a header and a body separately, so one row costs two
// goroutine round-trips. A per-frame cost measured that way is an upper bound on a harness,
// not a prediction about a connection, and opJoin sends one frame per row -- exactly the
// shape that artifact flatters. Measuring both says how much of the framing cost is real.
func bufferedPipe(tb testing.TB) (MsgConn, MsgConn) {
	tb.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	defer l.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, aerr := l.Accept()
		ch <- res{c, aerr}
	}()
	cc, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		tb.Fatal(err)
	}
	r := <-ch
	if r.err != nil {
		tb.Fatal(r.err)
	}
	tb.Cleanup(func() { cc.Close(); r.c.Close() })
	return NewStreamConn(cc), NewStreamConn(r.c)
}

// BenchmarkRPCJoinFramingOverTCP is arm (a) again over a buffered socket, to separate the
// per-frame cost that is real from the part net.Pipe invents.
func BenchmarkRPCJoinFramingOverTCP(b *testing.B) {
	const jobs = 2000
	cat, cleanup := rpcJoinCatalog(b, jobs)
	defer cleanup()
	s := NewServerCatalog(cat)
	defer s.Close()
	cconn, sconn := bufferedPipe(b)
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(s.SessionAttrs()))
	defer c.Close()

	jr := rpcJoinRequest()
	ctx := context.Background()
	want := jobs * rpcEpochs
	b.ReportAllocs()
	for b.Loop() {
		n := 0
		if err := c.Join(ctx, jr, func(row string) bool { n++; return true }); err != nil {
			b.Fatal(err)
		}
		if n != want {
			b.Fatalf("got %d rows, want %d", n, want)
		}
	}
}

// BenchmarkJoinOverTheWire is the A/B: the same join crossing the same connection both ways,
// in one process, with the arms alternating round to round.
//
// Each arm ends with the rows as OBJECTS, because that is what a consumer needs before it can
// sort, project or filter them -- comparing text-as-strings against wire-as-bytes would
// compare a finished answer against an unfinished one. The text arm parses; the wire arm
// decodes.
func BenchmarkJoinOverTheWire(b *testing.B) {
	const jobs = 2000
	f := rpcJoinFixture(b, jobs)
	jr := rpcJoinRequest()
	ctx := context.Background()
	want := jobs * rpcEpochs

	text := func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := f.c.Join(ctx, jr, func(row string) bool {
				if ad, perr := classad.Parse(row); perr != nil || ad == nil {
					b.Fatalf("row did not parse: %v", perr)
				}
				n++
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	}
	wireArm := func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			n := 0
			err := f.c.JoinWire(ctx, jr, false, func(row []byte) bool {
				if ad, derr := wire.DecodeInline(row); derr != nil || ad == nil {
					b.Fatalf("row did not decode: %v", derr)
				}
				n++
				return true
			})
			if err != nil {
				b.Fatal(err)
			}
			if n != want {
				b.Fatalf("got %d rows, want %d", n, want)
			}
		}
	}

	for round := 0; round < 4; round++ {
		arms := []struct {
			name string
			fn   func(*testing.B)
		}{{"text-rows-parsed", text}, {"wire-rows-decoded", wireArm}}
		if round%2 == 1 {
			arms[0], arms[1] = arms[1], arms[0]
		}
		for _, a := range arms {
			b.Run(fmt.Sprintf("r%d/%s", round, a.name), a.fn)
		}
	}
}
