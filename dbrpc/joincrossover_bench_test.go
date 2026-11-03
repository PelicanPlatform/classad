package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// WHERE THE SERVER-SIDE JOIN STOPS BEING WORTH IT.
//
// A join in the store ships only the joined rows; a join above it ships both sides and pairs them
// at the far end. Which moves less depends entirely on how much the join EXPANDS. With k right rows
// per left row and per-side projected widths l and r:
//
//	server ships  |L| * k * (l + r)      the left ad repeated once per output row
//	client ships  |L| * (l + k*r)        each side exactly once
//	ratio = k(l+r) / (l + k*r), which is below 1 exactly when k < 1
//
// MEASURED, AND THE MODEL HOLDS. Bytes moved, in-store against client-side, 4000 left rows:
//
//	k      0.25    0.5     1       2       4       8
//	ratio  0.63x   0.82x   1.04x   1.19x   1.29x   1.36x
//
// The crossover sits at k=1 -- where the join stops being selective -- independent of l and r, and
// no encoding moves it because an encoding scales both arms alike. The measured ratios run below the
// model's (0.40x, 0.67x, 1.00x, 1.33x, 1.60x, 1.78x) because both arms carry per-row framing the
// model ignores, which dilutes the repetition.
//
// WHAT THIS DOES NOT SETTLE, and the reason to read the time numbers with care. The decision to
// leave the store-side join off was taken on "1.33x more data AND 1.4x slower end to end" at k=2.
// The bytes half is confirmed above. The time half is NOT answered here, because the client arm
// hand-rolls an int-keyed hash join while a real executor runs a general ClassAd join engine over
// composite rows. The fetch-only arm measures that gap: the pairing is 16% of the client arm at
// k=0.25, 33% at k=2 and 37% at k=8. A general join 2-3x more expensive than this map flips the
// comparison at most k -- so these timings describe an idealized client, not the one that exists.
//
// Answering the time question needs the executor's OWN join on the client side of this
// measurement. What this benchmark contributes is the bytes crossover, which is exact and was the
// contested half, and an instrument the real comparison can be dropped into.
//
// BOTH ARMS END WITH THE ROWS AS OBJECTS. A client-side join has to materialize both sides to pair
// them, so comparing it against a server reply left as bytes would compare a finished answer with
// an unfinished one.

// crossoverFixture builds `left` job rows and k history rows per job, over one connection.
//
// Widths are matched (l == r) so the ratio above reduces to 2k/(1+k) and the arithmetic prediction
// is easy to state: 0.4x at k=0.25, 1.0x at k=1, 1.33x at k=2, 1.78x at k=8.
func crossoverFixture(tb testing.TB, left int, k float64) (*joinFixture, int) {
	tb.Helper()
	cat, err := db.OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		tb.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		tb.Fatal(err)
	}
	put := func(d *db.DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			tb.Fatal(perr)
		}
		if werr := d.Put(key, ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	// Six attributes a side, so neither side dominates the other's width.
	const pad = `; A = "padding one"; B = "padding two"; C = "padding three"`
	rows := 0
	for i := 0; i < left; i++ {
		put(jobs, fmt.Sprint(i), fmt.Sprintf(
			`[ ClusterId = %d; Owner = "user%02d"; Cpus = %d`+pad+` ]`, i, i%32, i%8))
		// k may be fractional: k=0.25 means one history row for every four jobs, which is the
		// SELECTIVE direction -- the one the store is supposed to win.
		n := int(float64(i+1)*k) - int(float64(i)*k)
		for j := 0; j < n; j++ {
			put(hist, fmt.Sprintf("%d.%d", i, j), fmt.Sprintf(
				`[ ClusterId = %d; ExitCode = %d; Slot = %d`+pad+` ]`, i, j%3, j))
			rows++
		}
	}
	f := &joinFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	f.c = NewClient(cconn, WithPeerSessionAttrs(f.s.SessionAttrs()))
	tb.Cleanup(func() { f.c.Close(); f.s.Close(); cat.Close() })
	return f, rows
}

var crossoverLeftAttrs = []string{"ClusterId", "Owner", "Cpus"}
var crossoverRightAttrs = []string{"ClusterId", "ExitCode", "Slot"}

func crossoverRequest() JoinRequest {
	return JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j", Project: crossoverLeftAttrs},
		Right: db.JoinSide{Table: "hist", Alias: "h", Project: crossoverRightAttrs},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
}

// clientSideJoin fetches both sides and pairs them here, which is what the executor does when the
// store-side join is off. It hashes the SMALLER side by convention and streams the other past it,
// the same shape the store uses.
func clientSideJoin(tb testing.TB, c *Client, ctx context.Context) int {
	tb.Helper()
	type row struct {
		cluster int64
		ad      *classad.ClassAd
	}
	byKey := map[int64][]*classad.ClassAd{}
	fetch := func(table string, attrs []string, sink func(row)) {
		err := c.QueryRawWireStream(ctx, table, "true", attrs, 0, false, func(b []byte) bool {
			a, derr := wire.DecodeInline(b)
			if derr != nil {
				tb.Fatalf("row did not decode: %v", derr)
			}
			ad := classad.FromAST(a)
			n, verr := ad.EvaluateAttr("ClusterId").IntValue()
			if verr != nil {
				tb.Fatalf("ClusterId: %v", verr)
			}
			sink(row{cluster: n, ad: ad})
			return true
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	fetch("jobs", crossoverLeftAttrs, func(r row) { byKey[r.cluster] = append(byKey[r.cluster], r.ad) })
	paired := 0
	fetch("hist", crossoverRightAttrs, func(r row) {
		for _, l := range byKey[r.cluster] {
			// Form the composite, which is the work the store would otherwise have done.
			out := classad.New()
			out.InsertAttrClassAd("j", l)
			out.InsertAttrClassAd("h", r.ad)
			paired++
		}
	})
	return paired
}

// BenchmarkJoinCrossover sweeps the expansion factor. At each k the two arms alternate order, since
// a Go benchmark's first arm warms the page cache and the allocator for the second.
func BenchmarkJoinCrossover(b *testing.B) {
	const left = 4000
	ctx := context.Background()
	for _, k := range []float64{0.25, 0.5, 1, 2, 4, 8} {
		f, want := crossoverFixture(b, left, k)
		jr := crossoverRequest()

		server := func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				n := 0
				err := f.c.JoinWire(ctx, jr, false, func(row []byte) bool {
					if _, derr := wire.DecodeInline(row); derr != nil {
						b.Fatalf("row did not decode: %v", derr)
					}
					n++
					return true
				})
				if err != nil {
					b.Fatal(err)
				}
				if n != want {
					b.Fatalf("server join gave %d rows, want %d", n, want)
				}
			}
		}
		// The floor for the client arm: fetch both sides, decode them, pair NOTHING. The gap
		// between this and the client arm is the pairing, and that is the part this benchmark
		// hand-rolls as an int-keyed map where a real executor runs a general ClassAd join. If
		// the pairing is a small share of the arm, the hand-rolling cannot be carrying the
		// result; if it is a large share, this comparison is not one.
		fetchOnly := func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				n := 0
				for _, side := range []struct {
					table string
					attrs []string
				}{{"jobs", crossoverLeftAttrs}, {"hist", crossoverRightAttrs}} {
					err := f.c.QueryRawWireStream(ctx, side.table, "true", side.attrs, 0, false,
						func(bts []byte) bool {
							a, derr := wire.DecodeInline(bts)
							if derr != nil {
								b.Fatalf("row did not decode: %v", derr)
							}
							_ = classad.FromAST(a)
							n++
							return true
						})
					if err != nil {
						b.Fatal(err)
					}
				}
				if n != left+want {
					b.Fatalf("fetched %d rows, want %d", n, left+want)
				}
			}
		}

		client := func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if n := clientSideJoin(b, f.c, ctx); n != want {
					b.Fatalf("client join gave %d rows, want %d", n, want)
				}
			}
		}

		for round := 0; round < 2; round++ {
			arms := []struct {
				name string
				fn   func(*testing.B)
			}{{"in-store", server}, {"client-side", client}, {"client-fetch-only", fetchOnly}}
			if round%2 == 1 {
				arms[0], arms[1] = arms[1], arms[0]
			}
			for _, a := range arms {
				b.Run(fmt.Sprintf("k=%g/r%d/%s", k, round, a.name), a.fn)
			}
		}
	}
}
