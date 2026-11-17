package dbrpc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// THE OPCODE IS TESTED AGAINST THE RUNG IT SERVES.
//
// What can go wrong here is not the ranking -- db's own differential test holds that -- but the
// FRAMING: a group header read as a row, a row attached to the wrong group, a group split across
// frames losing its tail, a tagged value decoded as the wrong kind. Every one of those produces
// an answer that looks structurally fine and is wrong, so the oracle is the embedded rung over
// the same table and the same spec.

func topKFixture(tb testing.TB, owners, perOwner, payload int) (*db.DB, *Client, func()) {
	tb.Helper()
	d, err := db.OpenConfig(db.Config{Dir: tb.TempDir(), SegmentSize: 1 << 18})
	if err != nil {
		tb.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn)
	tx := d.Begin()
	n := 0
	pad := strings.Repeat("x", payload)
	for o := 0; o < owners; o++ {
		for r := 0; r < perOwner; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(1700000000+r))
			ad.InsertAttr("Rowid", int64(n))
			ad.InsertAttrString("Payload", fmt.Sprintf("o%d-r%d-%s", o, r, pad))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err = tx.Commit(); err != nil {
		tb.Fatal(err)
	}
	return d, c, func() { c.Close(); s.Close(); d.Close() }
}

// renderGroups flattens an answer to a comparable form: the group's label, then each row's
// Payload in order. Both sides go through the same rendering so a difference is a difference.
func renderWireGroups(tb testing.TB, groups []TopKWireGroup) map[string]string {
	tb.Helper()
	out := map[string]string{}
	for _, g := range groups {
		var label []string
		for _, v := range g.Values {
			label = append(label, db.GroupValueText(v))
		}
		var cells []string
		for _, row := range g.Rows {
			a, derr := wire.DecodeInline(row)
			if derr != nil {
				tb.Fatalf("row did not decode: %v", derr)
			}
			p, perr := classad.FromAST(a).EvaluateAttr("Payload").StringValue()
			if perr != nil {
				tb.Fatalf("row has no Payload: %v", perr)
			}
			cells = append(cells, p)
		}
		out[strings.Join(label, "\x00")] = strings.Join(cells, "|")
	}
	return out
}

func renderLocalGroups(tb testing.TB, groups []db.TopKRowsWireGroup) map[string]string {
	tb.Helper()
	conv := make([]TopKWireGroup, len(groups))
	for i, g := range groups {
		conv[i] = TopKWireGroup{Values: g.Values, Rows: g.Rows}
	}
	return renderWireGroups(tb, conv)
}

func assertOpcodeMatchesRung(t *testing.T, d *db.DB, c *Client, constraint string, spec TopKRowsSpec) {
	t.Helper()
	want, err := d.TopKRowsPerGroupWire(constraint, spec)
	if err != nil {
		t.Fatalf("embedded rung: %v", err)
	}
	got, err := c.TopKRowsPerGroup(context.Background(), constraint, spec)
	if err != nil {
		t.Fatalf("opcode: %v", err)
	}
	w, g := renderLocalGroups(t, want), renderWireGroups(t, got)
	if len(g) != len(w) {
		t.Fatalf("opcode returned %d groups, rung %d", len(g), len(w))
	}
	for label, rows := range w {
		if g[label] != rows {
			t.Errorf("group %q: opcode %q, rung %q", label, g[label], rows)
		}
	}
}

func TestTopKRowsOpcodeMatchesTheRung(t *testing.T) {
	d, c, done := topKFixture(t, 8, 120, 0)
	defer done()
	for _, tc := range []struct {
		name string
		cons string
		spec TopKRowsSpec
	}{
		{"one group column", "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", true),
			K: 5, Attrs: []string{"Payload"}}},
		{"ascending", "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", false),
			K: 3, Attrs: []string{"Payload"}}},
		{"two ordering terms", "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}},
			OrderBy:   []TopKOrderTerm{{Attr: "QDate", Desc: true}, {Attr: "Rowid", Desc: false}},
			K:         4, Attrs: []string{"Payload"}}},
		{"no group columns", "true", TopKRowsSpec{
			OrderBy: db.OrderOne("QDate", true), K: 6, Attrs: []string{"Payload"}}},
		{"two group columns", "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}, {Attr: "QDate"}},
			OrderBy:   db.OrderOne("Rowid", true), K: 2, Attrs: []string{"Payload"}}},
		{"constrained", "QDate > 1700000050", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", true),
			K: 3, Attrs: []string{"Payload"}}},
		{"k larger than any group", "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", true),
			K: 100000, Attrs: []string{"Payload"}}},
		{"matches nothing", "QDate < 0", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", true),
			K: 3, Attrs: []string{"Payload"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assertOpcodeMatchesRung(t, d, c, tc.cons, tc.spec)
		})
	}
}

// TestTopKRowsOpcodeSplitsGroupsAcrossFrames is the framing case the small fixtures cannot
// reach: a group's rows exceed the batch budget, so its header lands in one frame and its tail
// in later ones. A client that started a new group per frame, or dropped rows that arrived
// without a header, would pass every test above and fail this.
//
// IT RUNS OVER A SIZE-CAPPED CONN, and that is the whole point. netPipe has no message limit,
// so a server that never split would write one enormous frame and this test would pass -- which
// it did, until the mutation that disables splitting survived. sizeCappedConn rejects an
// oversized frame exactly as the CEDAR stream does at MaxMessageSize, so the unsplit case now
// fails the way it would in production.
func TestTopKRowsOpcodeSplitsGroupsAcrossFrames(t *testing.T) {
	const maxMessage = 1 << 20
	d, err := db.OpenConfig(db.Config{Dir: t.TempDir(), SegmentSize: 1 << 18})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// 3 groups x 40 kept rows x 12 KiB is ~1.4 MiB in total and ~470 KiB per group: more
	// than one message can carry whether it is batched per answer or per group, and ~22
	// frames at the 64 KiB batch budget.
	tx := d.Begin()
	n := 0
	pad := strings.Repeat("x", 12*1024)
	for o := 0; o < 3; o++ {
		for r := 0; r < 100; r++ {
			ad := classad.New()
			ad.InsertAttrString("Owner", fmt.Sprintf("o%d", o))
			ad.InsertAttr("QDate", int64(1700000000+r))
			ad.InsertAttr("Rowid", int64(n))
			ad.InsertAttrString("Payload", fmt.Sprintf("o%d-r%d-%s", o, r, pad))
			tx.NewClassAd(fmt.Sprintf("k%d", n), ad)
			n++
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	s := NewServer(d)
	cconn, sconn := netPipe()
	capped := &sizeCappedConn{MsgConn: sconn, limit: maxMessage}
	go func() { _ = s.ServeConnOpts(capped, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()

	spec := TopKRowsSpec{
		GroupCols: []db.GroupCol{{Attr: "Owner"}},
		OrderBy:   []TopKOrderTerm{{Attr: "QDate", Desc: true}, {Attr: "Rowid", Desc: false}},
		K:         40,
		Attrs:     []string{"Payload"},
	}
	got, err := c.TopKRowsPerGroup(context.Background(), "true", spec)
	if err != nil {
		t.Fatalf("top-k over a capped conn: %v", err)
	}
	if over := capped.over.Load(); over != 0 {
		t.Errorf("server attempted %d frames over the %d-byte message limit", over, maxMessage)
	}
	if len(got) != 3 {
		t.Fatalf("groups = %d, want 3", len(got))
	}
	for _, g := range got {
		if len(g.Rows) != 40 {
			owner, _ := g.Values[0].StringValue() //nolint:errcheck // a string group here
			t.Fatalf("group %s came back with %d rows, want 40 -- a split group lost its tail",
				owner, len(g.Rows))
		}
	}
	assertOpcodeMatchesRung(t, d, c, "true", spec)
}

// TestTopKRowsOpcodeRedactsForAnUnentitledSession pins the thing the frame cannot carry: the
// rows are stored bytes, so whether a private attribute crosses is decided by the SERVER from
// the session, not by anything the client sends. A client asking politely is not the control.
func TestTopKRowsOpcodeRedactsForAnUnentitledSession(t *testing.T) {
	d, err := db.OpenConfig(db.Config{Dir: t.TempDir(), SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	tx := d.Begin()
	for i := 0; i < 20; i++ {
		ad := classad.New()
		ad.InsertAttrString("Owner", fmt.Sprintf("o%d", i%2))
		ad.InsertAttr("QDate", int64(1700000000+i))
		ad.InsertAttrString("ClaimId", fmt.Sprintf("secret-claim-%d", i))
		// Payload is an EXPRESSION over ClaimId, so the reference-chasing scan pulls
		// ClaimId into the row to make Payload evaluate -- WITHOUT the caller naming it.
		// That is the case redaction exists for and the one a name-based gate cannot
		// catch, so it is the one the test uses.
		ex, perr := classad.ParseExpr("ClaimId")
		if perr != nil {
			t.Fatal(perr)
		}
		ad.Insert("Payload", ex.AST())
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	spec := TopKRowsSpec{
		GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("QDate", true),
		K: 3, Attrs: []string{"Payload"},
	}
	ask := func(includePrivate bool) []TopKWireGroup {
		t.Helper()
		s := NewServer(d)
		cconn, sconn := netPipe()
		go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: includePrivate}) }()
		c := NewClient(cconn)
		defer func() { c.Close(); s.Close() }()
		got, gerr := c.TopKRowsPerGroup(context.Background(), "true", spec)
		if gerr != nil {
			t.Fatalf("includePrivate=%v: %v", includePrivate, gerr)
		}
		return got
	}
	carries := func(groups []TopKWireGroup) bool {
		for _, g := range groups {
			for _, row := range g.Rows {
				if bytes.Contains(row, []byte("secret-claim-")) {
					return true
				}
			}
		}
		return false
	}
	// The entitled session is the CONTROL: if it does not carry the secret, the rows never
	// held it and the unprivileged assertion proves nothing. Note the caller never names
	// ClaimId -- it rides in because Payload refers to it.
	if !carries(ask(true)) {
		t.Fatal("a session entitled to private attributes does not carry ClaimId, " +
			"so this test cannot detect a leak")
	}
	if carries(ask(false)) {
		t.Error("a session NOT entitled to private attributes received ClaimId in the row bytes")
	}
}

// TestTopKRowsOpcodeRefusesPrivateAttributes pins the other half: naming a private attribute as
// a group or an ordering term is refused outright, which is a clearer answer than grouping every
// row under undefined.
func TestTopKRowsOpcodeRefusesPrivateAttributes(t *testing.T) {
	d, err := db.OpenConfig(db.Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	s := NewServer(d)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: false}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close() }()
	for _, tc := range []struct {
		name string
		spec TopKRowsSpec
	}{
		{"private group column", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "ClaimId"}}, OrderBy: db.OrderOne("QDate", true),
			K: 2, Attrs: []string{"Payload"}}},
		{"private ordering term", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}}, OrderBy: db.OrderOne("ClaimId", true),
			K: 2, Attrs: []string{"Payload"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.TopKRowsPerGroup(context.Background(), "true", tc.spec); err == nil {
				t.Fatal("accepted a request naming a private attribute")
			}
		})
	}
}

// TestTopKRowsOpcodeRefusesAnEmptyOrdering pins that the server rejects a spec with no ordering
// rather than passing it down -- there is no "top K" without one.
func TestTopKRowsOpcodeRefusesAnEmptyOrdering(t *testing.T) {
	d, c, done := topKFixture(t, 2, 10, 0)
	defer done()
	_ = d
	_, err := c.TopKRowsPerGroup(context.Background(), "true", TopKRowsSpec{
		GroupCols: []db.GroupCol{{Attr: "Owner"}}, K: 3, Attrs: []string{"Payload"},
	})
	if err == nil {
		t.Fatal("accepted a request with no ordering")
	}
}

// TestTopKRowsUnsupportedIsNotAnEmptyAnswer pins the fallback signal. A client that read a
// missing opcode as "no groups matched" would silently return nothing for a query that has an
// answer, which is the failure the sentinel exists to prevent.
func TestTopKRowsUnsupportedIsNotAnEmptyAnswer(t *testing.T) {
	if !errors.Is(ErrTopKRowsUnsupported, ErrTopKRowsUnsupported) {
		t.Fatal("sentinel does not match itself")
	}
	if ErrTopKRowsUnsupported.Error() == "" {
		t.Fatal("sentinel has no message for an operator to read")
	}
}

// TestTopKRowsOpcodeOverAHistoryTable pins that a name resolving to an archive is served, as
// the aggregate opcodes are -- history is where "the top N per group" mostly points.
//
// The ordering is TOTAL (QDate then Rowid, unique), so the expected answer is determined and
// can be written out rather than taken from an oracle: each owner's three newest payloads.
func TestTopKRowsOpcodeOverAHistoryTable(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()
	ctx := context.Background()

	if err = c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{SegmentSize: 1 << 13}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	const owners, per = 4, 30
	for i := 0; i < owners*per; i++ {
		if aerr := c.ArchiveAppend(ctx, "history", fmt.Sprintf(
			"Owner = \"o%d\"\nQDate = %d\nRowid = %d\nPayload = \"p%d\"",
			i%owners, 1700000000+i, i, i)); aerr != nil {
			t.Fatalf("ArchiveAppend: %v", aerr)
		}
	}

	got, err := c.TopKRowsPerGroupTable(ctx, "history", "true", TopKRowsSpec{
		GroupCols: []db.GroupCol{{Attr: "Owner"}},
		OrderBy:   []TopKOrderTerm{{Attr: "QDate", Desc: true}, {Attr: "Rowid", Desc: false}},
		K:         3, Attrs: []string{"Payload"},
	})
	if err != nil {
		t.Fatalf("opcode over history: %v", err)
	}
	if len(got) != owners {
		t.Fatalf("groups = %d, want %d", len(got), owners)
	}
	// Record i belongs to owner i%owners and has QDate 1700000000+i, so an owner's three
	// newest are the last three records bearing its remainder.
	for _, g := range renderWireGroups(t, got) {
		cells := strings.Split(g, "|")
		if len(cells) != 3 {
			t.Fatalf("group came back with %d rows, want 3", len(cells))
		}
	}
	byLabel := renderWireGroups(t, got)
	for o := 0; o < owners; o++ {
		var want []string
		for n := 0; n < 3; n++ {
			want = append(want, fmt.Sprintf("p%d", owners*(per-1-n)+o))
		}
		label := fmt.Sprintf("%q", fmt.Sprintf("o%d", o))
		if got := byLabel[label]; got != strings.Join(want, "|") {
			t.Errorf("owner o%d: %q, want %q", o, got, strings.Join(want, "|"))
		}
	}
}

// TestTopKRowsOpcodeRowsSurviveLaterTraffic pins that a returned row is the caller's own bytes
// and not a view into a frame.
//
// reader.bytesRef hands back a sub-slice of the frame -- "callers that retain it past the
// frame's lifetime must copy" -- so the client copies each kept row. Today's transport does not
// recycle frame buffers, which means dropping that copy breaks nothing and no test above
// notices: the mutation survives. This runs a second, larger query over the same client and
// then re-reads the first answer, so the day a frame buffer IS pooled, this is what fails
// instead of a caller somewhere reading another query's bytes.
func TestTopKRowsOpcodeRowsSurviveLaterTraffic(t *testing.T) {
	d, c, done := topKFixture(t, 4, 80, 512)
	defer done()
	ctx := context.Background()
	spec := TopKRowsSpec{
		GroupCols: []db.GroupCol{{Attr: "Owner"}},
		OrderBy:   []TopKOrderTerm{{Attr: "QDate", Desc: true}, {Attr: "Rowid", Desc: false}},
		K:         4, Attrs: []string{"Payload"},
	}
	first, err := c.TopKRowsPerGroup(ctx, "true", spec)
	if err != nil {
		t.Fatal(err)
	}
	before := renderWireGroups(t, first)
	if len(before) != 4 {
		t.Fatalf("groups = %d, want 4", len(before))
	}

	// Enough later traffic to reuse any pooled buffer several times over.
	for i := 0; i < 5; i++ {
		if _, lerr := c.TopKRowsPerGroup(ctx, "true", TopKRowsSpec{
			GroupCols: []db.GroupCol{{Attr: "Owner"}},
			OrderBy:   db.OrderOne("Rowid", false),
			K:         40, Attrs: []string{"Payload"},
		}); lerr != nil {
			t.Fatal(lerr)
		}
	}

	after := renderWireGroups(t, first)
	for label, rows := range before {
		if after[label] != rows {
			t.Errorf("group %q changed after later traffic:\n before %q\n after  %q",
				label, rows, after[label])
		}
	}
	// And the rows must still be what the embedded rung says, not merely self-consistent.
	want, err := d.TopKRowsPerGroupWire("true", spec)
	if err != nil {
		t.Fatal(err)
	}
	for label, rows := range renderLocalGroups(t, want) {
		if after[label] != rows {
			t.Errorf("group %q: client %q, rung %q", label, after[label], rows)
		}
	}
}
