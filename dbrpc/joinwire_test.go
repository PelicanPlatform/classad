package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// THE ORACLE FOR THE WIRE-FORM JOIN REPLY.
//
// opJoinWire answers the same plan as opJoin with a different encoding and a different
// framing. Both of those can be wrong quietly: a batch whose row count disagrees with its
// payload truncates the result, and a row spliced from the wrong bytes decodes to a
// different ad. So the test is the two opcodes against each other, over the same fixture,
// compared as ads rather than as bytes.

// joinWireAds runs the wire reply and returns each row decoded and rendered. It COPIES every
// row before decoding, because the slice handed to yield aliases the frame buffer -- which is
// the contract this is also pinning.
func joinWireAds(t *testing.T, c *Client, jr JoinRequest, redact bool) []string {
	t.Helper()
	var out []string
	err := c.JoinWire(context.Background(), jr, redact, func(row []byte) bool {
		cp := append([]byte(nil), row...)
		ad, derr := wire.DecodeInline(cp)
		if derr != nil {
			t.Fatalf("a wire joined row did not decode: %v", derr)
		}
		out = append(out, classad.FromAST(ad).String())
		return true
	})
	if err != nil {
		t.Fatalf("JoinWire: %v", err)
	}
	sort.Strings(out)
	return out
}

func joinTextAds(t *testing.T, c *Client, jr JoinRequest) []string {
	t.Helper()
	rows, err := c.JoinRows(context.Background(), jr)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		ad, perr := classad.Parse(r)
		if perr != nil {
			t.Fatalf("a text joined row did not parse: %v", perr)
		}
		out = append(out, ad.String())
	}
	sort.Strings(out)
	return out
}

// TestJoinWireAgreesWithTheTextReply is the oracle, over every kind of join the opcode
// carries. A projection is given on each side because that is what a relay sends and what the
// splice narrows to; the whole-ad case has its own test below.
func TestJoinWireAgreesWithTheTextReply(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	for _, kind := range []db.JoinKind{db.JoinInner, db.JoinLeft, db.JoinCross} {
		name := map[db.JoinKind]string{db.JoinInner: "inner", db.JoinLeft: "left", db.JoinCross: "cross"}[kind]
		t.Run(name, func(t *testing.T) {
			jr := equiReq(kind)
			if kind == db.JoinCross {
				jr.Spec = db.JoinSpec{Kind: db.JoinCross,
					Left:  db.JoinSide{Table: "jobs", Alias: "j"},
					Right: db.JoinSide{Table: "hist", Alias: "h"}}
			}
			jr.Spec.Left.Project = []string{"ClusterId", "Owner"}
			jr.Spec.Right.Project = []string{"ClusterId", "ExitCode"}
			want := joinTextAds(t, f.c, jr)
			got := joinWireAds(t, f.c, jr, false)
			if len(got) != len(want) {
				t.Fatalf("wire reply gave %d rows, text reply gave %d", len(got), len(want))
			}
			if len(want) == 0 {
				t.Fatal("the shape produced no rows, so it compares nothing")
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d differs:\n text: %s\n wire: %s", i, want[i], got[i])
				}
			}
		})
	}
}

// TestJoinWireBatchesManyRowsPerFrame pins the framing change, which is most of why this
// opcode exists: opJoin sends one frame per row.
//
// Without this the server could flush after every row and every other test here would pass --
// a batch of one is a correct batch. The count is read off the connection rather than
// inferred, so it says what actually crossed.
func TestJoinWireBatchesManyRowsPerFrame(t *testing.T) {
	f := newCountingJoinFixture(t, 400)
	defer f.close()
	jr := JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j", Project: []string{"ClusterId", "Owner"}},
		Right: db.JoinSide{Table: "hist", Alias: "h", Project: []string{"ExitCode"}},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}

	f.count.reset()
	rows := 0
	if err := f.c.JoinWire(context.Background(), jr, false, func([]byte) bool { rows++; return true }); err != nil {
		t.Fatal(err)
	}
	wireFrames := f.count.frames()
	if rows != 400 {
		t.Fatalf("got %d rows, want 400", rows)
	}

	f.count.reset()
	textRows, err := f.c.JoinRows(context.Background(), jr)
	if err != nil {
		t.Fatal(err)
	}
	textFrames := f.count.frames()
	if len(textRows) != 400 {
		t.Fatalf("text reply gave %d rows, want 400", len(textRows))
	}

	// The text reply sends a frame per row; the wire reply should send far fewer. The bound
	// is loose on purpose -- the point is the order of magnitude, not an exact count that
	// would break when a row's width changes.
	if wireFrames >= textFrames/4 {
		t.Errorf("wire reply used %d frames for 400 rows and the text reply used %d; "+
			"it is not batching", wireFrames, textFrames)
	}
	t.Logf("400 rows: wire reply %d frames, text reply %d", wireFrames, textFrames)
}

// TestJoinWireIsNotSentToAPeerThatCannotServeIt pins the fallback, which is what keeps an old
// server working: the client must not send the opcode at all, and must say so before any row
// so a caller can take the text reply without having relayed half an answer.
func TestJoinWireIsNotSentToAPeerThatCannotServeIt(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	// A peer that advertised the text join but not this one -- a server that can join and
	// cannot batch, which is exactly the version skew this has to survive.
	old := NewClient(f.c.conn, WithPeerSessionAttrs(sessionAttrsFor([]Feature{FeatJoin})))
	err := old.JoinWire(context.Background(), equiReq(db.JoinInner), false, func([]byte) bool {
		t.Fatal("a row was yielded by a peer that cannot serve the opcode")
		return false
	})
	if !errors.Is(err, ErrJoinWireUnsupported) {
		t.Fatalf("got %v, want ErrJoinWireUnsupported", err)
	}
}

// TestJoinWireRefusesARedactedWholeAd pins the refusal rather than the leak. A whole-ad member
// has no projection to withhold private attributes from, so the server refuses the way an
// older server refuses the opcode and the caller falls back to the text reply, whose render
// withholds them.
func TestJoinWireRefusesARedactedWholeAd(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	jr := equiReq(db.JoinInner) // no projection on either side
	err := f.c.JoinWire(context.Background(), jr, true, func([]byte) bool {
		t.Fatal("a row crossed for a redacted whole-ad join; it cannot withhold anything")
		return false
	})
	if !errors.Is(err, ErrJoinWireUnsupported) {
		t.Fatalf("got %v, want ErrJoinWireUnsupported", err)
	}
	// Unredacted the same request is served: nothing is being withheld, so nothing can leak.
	n := 0
	if err := f.c.JoinWire(context.Background(), jr, false, func([]byte) bool { n++; return true }); err != nil {
		t.Fatalf("an unredacted whole-ad join should be served: %v", err)
	}
	if n == 0 {
		t.Fatal("the unredacted whole-ad join produced no rows")
	}
}

// TestJoinWireRedactsOverTheWire pins that the private attributes do not cross. The sides are
// read UNREDACTED inside the store on purpose, so this is the only thing standing between an
// unprivileged reader and every claim id in both tables.
func TestJoinWireRedactsOverTheWire(t *testing.T) {
	f := newPrivateJoinFixture(t)
	defer f.close()
	jr := JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j", Project: []string{"ClusterId", "Owner", "ClaimId"}},
		Right: db.JoinSide{Table: "hist", Alias: "h", Project: []string{"ExitCode", "TransferKey"}},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
	for _, row := range joinWireAds(t, f.c, jr, true) {
		for _, secret := range []string{"ClaimId", "secret-left", "TransferKey", "secret-right"} {
			if strings.Contains(row, secret) {
				t.Fatalf("a redacted row carried %q across the wire: %s", secret, row)
			}
		}
		if !strings.Contains(row, "Owner") || !strings.Contains(row, "ExitCode") {
			t.Fatalf("redaction took the public attributes too: %s", row)
		}
	}
	// And unredacted they are there, so the check above is withholding something real.
	carried := 0
	for _, row := range joinWireAds(t, f.c, jr, false) {
		if strings.Contains(row, "secret-left") && strings.Contains(row, "secret-right") {
			carried++
		}
	}
	if carried == 0 {
		t.Fatal("no unredacted row carried a private attribute; the redaction test proves nothing")
	}
}

// countingConn wraps a MsgConn and counts the frames the server wrote, so a test can say how
// many frames an answer actually took rather than inferring it from a row count.
type countingConn struct {
	MsgConn
	n atomic.Int64
}

func (c *countingConn) ReadMsg() ([]byte, error) {
	b, err := c.MsgConn.ReadMsg()
	if err == nil {
		c.n.Add(1)
	}
	return b, err
}

func (c *countingConn) reset()      { c.n.Store(0) }
func (c *countingConn) frames() int { return int(c.n.Load()) }

type countingJoinFixture struct {
	*joinFixture
	count *countingConn
}

// newCountingJoinFixture builds a join whose result is n rows -- one history row per job, so
// the row count is exactly n and the frame count is the only variable.
func newCountingJoinFixture(t *testing.T, n int) *countingJoinFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		put := func(d *db.DB, key, text string) {
			ad, perr := classad.Parse(text)
			if perr != nil {
				t.Fatal(perr)
			}
			if werr := d.Put(key, ad); werr != nil {
				t.Fatal(werr)
			}
		}
		put(jobs, fmt.Sprint(i), fmt.Sprintf(`[ ClusterId = %d; Owner = "user%d" ]`, i, i%13))
		put(hist, fmt.Sprint(i), fmt.Sprintf(`[ ClusterId = %d; ExitCode = %d ]`, i, i%3))
	}
	f := &joinFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	cc := &countingConn{MsgConn: cconn}
	f.c = NewClient(cc, WithPeerSessionAttrs(f.s.SessionAttrs()))
	return &countingJoinFixture{joinFixture: f, count: cc}
}

// newPrivateJoinFixture gives each side a private attribute, so a redacted join has something
// to withhold and an unredacted one has something to carry.
func newPrivateJoinFixture(t *testing.T) *joinFixture {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		put := func(d *db.DB, key, text string) {
			ad, perr := classad.Parse(text)
			if perr != nil {
				t.Fatal(perr)
			}
			if werr := d.Put(key, ad); werr != nil {
				t.Fatal(werr)
			}
		}
		put(jobs, fmt.Sprint(i), fmt.Sprintf(
			`[ ClusterId = %d; Owner = "user%d"; ClaimId = "secret-left-%d" ]`, i, i, i))
		put(hist, fmt.Sprint(i), fmt.Sprintf(
			`[ ClusterId = %d; ExitCode = 0; TransferKey = "secret-right-%d" ]`, i, i))
	}
	f := &joinFixture{s: NewServerCatalog(cat), cat: cat}
	cconn, sconn := netPipe()
	go func() { _ = f.s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	f.c = NewClient(cconn, WithPeerSessionAttrs(f.s.SessionAttrs()))
	return f
}
