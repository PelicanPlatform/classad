package dbrpc

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// THE CHAIN OPCODE OVER A REAL CONNECTION.
//
// What these pin is not the join -- db tests that -- but the three things a second pair of
// opcodes can get wrong: that the plan survives the wire intact, that the two-table opcodes
// still carry exactly the bytes they always did, and that a server which cannot serve a chain
// says so instead of answering a different query.

// chainRPCFixture builds the three tables behind a connected client/server pair.
func chainRPCFixture(t *testing.T) (*Client, *db.Catalog) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name string) *db.DB {
		d, cerr := cat.CreateTable(name)
		if cerr != nil {
			t.Fatal(cerr)
		}
		return d
	}
	put := func(d *db.DB, key, text string) {
		ad, perr := classad.Parse(text)
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := d.Put(key, ad); werr != nil {
			t.Fatal(werr)
		}
	}
	hist, epoch, metric := mk("history"), mk("epoch_history"), mk("job_metrics")
	for i := 1; i <= 5; i++ {
		put(hist, fmt.Sprintf("h%d", i), fmt.Sprintf(`[ ClusterId = %d; Owner = "u%d" ]`, i, i))
	}
	for i := 1; i <= 3; i++ {
		put(epoch, fmt.Sprintf("e%d", i), fmt.Sprintf(`[ ClusterId = %d; Size = %d ]`, i, 10*i))
	}
	for i := 1; i <= 2; i++ {
		put(metric, fmt.Sprintf("m%d", i), fmt.Sprintf(`[ ClusterId = %d; Eff = %d ]`, i, 50+i))
	}
	srv := NewServerCatalog(cat)
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: true, Privileged: true}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
	t.Cleanup(func() { c.Close(); srv.Close(); cat.Close() })
	return c, cat
}

func chainRPCRequest(k1, k2 db.JoinKind, post ...string) JoinRequest {
	return JoinRequest{Spec: db.JoinSpec{
		Left: db.JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
		Steps: []db.JoinStep{
			{
				Kind: k1,
				Side: db.JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"ClusterId", "Size"}},
				Cond: []string{"h.ClusterId == e.ClusterId"},
				Equi: []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
			},
			{
				Kind: k2,
				Side: db.JoinSide{Table: "job_metrics", Alias: "m", Project: []string{"ClusterId", "Eff"}},
				Cond: []string{"e.ClusterId == m.ClusterId"},
				Equi: []db.JoinEqui{{Left: "e.ClusterId", Right: "ClusterId"}},
			},
		},
		Post: post,
	}}
}

// rowShape renders a joined ad as "h:e:m", "-" for an absent side.
func rowShape(t *testing.T, ad *classad.ClassAd) string {
	t.Helper()
	parts := make([]string, 0, 3)
	for _, alias := range []string{"h", "e", "m"} {
		ex, err := classad.ParseExpr(alias + ".ClusterId")
		if err != nil {
			t.Fatal(err)
		}
		v := ex.Eval(ad)
		if v.IsUndefined() {
			parts = append(parts, "-")
			continue
		}
		n, verr := v.IntValue()
		if verr != nil {
			parts = append(parts, "?")
			continue
		}
		parts = append(parts, fmt.Sprint(n))
	}
	return strings.Join(parts, ":")
}

// TestChainOverRPCAnswersTheChain pins that the plan crosses the wire intact: the kinds are
// per step, the aliases survive, and the residual WHERE is applied once at the end.
//
// The expectation is SQL's, from the fixture: cluster 3 pairs with an epoch row and no
// metrics row; 4 and 5 pair with neither.
func TestChainOverRPCAnswersTheChain(t *testing.T) {
	c, _ := chainRPCFixture(t)
	rows, err := c.JoinRows(context.Background(), chainRPCRequest(db.JoinLeft, db.JoinLeft))
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var got []string
	for _, r := range rows {
		ad, perr := classad.Parse(r)
		if perr != nil {
			t.Fatalf("parse %q: %v", r, perr)
		}
		got = append(got, rowShape(t, ad))
	}
	sort.Strings(got)
	want := []string{"1:1:1", "2:2:2", "3:3:-", "4:-:-", "5:-:-"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("chain over RPC:\n got  %v\n want %v", got, want)
	}
}

// TestChainWireOverRPCAgreesWithTheTextReply pins the second opcode against the first. They
// are two renderings of ONE plan, so they must answer the same rows -- a wire splice that
// dropped a member would otherwise be invisible.
func TestChainWireOverRPCAgreesWithTheTextReply(t *testing.T) {
	c, _ := chainRPCFixture(t)
	ctx := context.Background()
	jr := chainRPCRequest(db.JoinLeft, db.JoinLeft)

	text, err := c.JoinRows(ctx, jr)
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	var fromText, fromWire []string
	for _, r := range text {
		ad, perr := classad.Parse(r)
		if perr != nil {
			t.Fatal(perr)
		}
		fromText = append(fromText, rowShape(t, ad))
	}
	if werr := c.JoinWire(ctx, jr, false, func(row []byte) bool {
		a, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Errorf("decode wire row: %v", derr)
			return false
		}
		fromWire = append(fromWire, rowShape(t, classad.FromAST(a)))
		return true
	}); werr != nil {
		t.Fatalf("JoinWire: %v", werr)
	}
	sort.Strings(fromText)
	sort.Strings(fromWire)
	if strings.Join(fromText, " ") != strings.Join(fromWire, " ") {
		t.Errorf("the two chain opcodes disagree:\n text %v\n wire %v", fromText, fromWire)
	}
	if len(fromWire) != 5 {
		t.Fatalf("the wire reply produced %d rows, so it compares almost nothing", len(fromWire))
	}
}

// TestTwoTableJoinStillSendsTheTwoTableOpcode is the compatibility guard, and it checks the
// BYTES rather than the behaviour.
//
// A two-table plan must encode exactly as it did before the chain opcodes existed, or every
// peer already deployed stops being able to read it. Behaviour cannot see that: a chain of one
// step answers the same rows, so a client that quietly started sending the new opcode would
// pass every functional test and fail against every older server.
func TestTwoTableJoinStillSendsTheTwoTableOpcode(t *testing.T) {
	pair := JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
		Right: db.JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"Size"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
	o, feat := joinOpFor(pair, opJoin, opJoinChain, FeatJoin, FeatJoinChain)
	if o != opJoin || feat != FeatJoin {
		t.Fatalf("a two-table plan chose op %v / %v, want opJoin / %v", o, feat, FeatJoin)
	}
	frame := joinFrameOp(pair, opJoin, joinOpLayout(opJoin), false, false, false)(7)

	// Decoded by the decoder a server runs, with chain=false, it must come back whole.
	_, _, body, ok := reqHeader(frame)
	if !ok {
		t.Fatal("the request frame has no readable header")
	}
	got, ok := readJoinRequest(body, 0)
	if !ok {
		t.Fatal("the two-table frame did not decode")
	}
	if got.Spec.Right.Table != "epoch_history" || len(got.Spec.Equi) != 1 ||
		got.Spec.Equi[0].Right != "ClusterId" || len(got.Spec.Steps) != 0 {
		t.Fatalf("the two-table frame did not round-trip: %+v", got.Spec)
	}
}

// TestChainFrameRoundTripsThroughTheSharedDecoder pins that the chain body survives the one
// encoder and the one decoder, including the per-step kinds and the two equality halves --
// which are in different scopes and would be easy to swap.
func TestChainFrameRoundTripsThroughTheSharedDecoder(t *testing.T) {
	jr := chainRPCRequest(db.JoinInner, db.JoinLeft, "m.Eff > 0")
	frame := joinFrameOp(jr, opJoinChain, joinOpLayout(opJoinChain), false, false, false)(11)
	_, _, body, ok := reqHeader(frame)
	if !ok {
		t.Fatal("the request frame has no readable header")
	}
	got, ok := readJoinRequest(body, layoutChain)
	if !ok {
		t.Fatal("the chain frame did not decode")
	}
	if len(got.Spec.Steps) != 2 {
		t.Fatalf("decoded %d steps, want 2", len(got.Spec.Steps))
	}
	if got.Spec.Steps[0].Kind != db.JoinInner || got.Spec.Steps[1].Kind != db.JoinLeft {
		t.Errorf("per-step kinds did not survive: %v, %v",
			got.Spec.Steps[0].Kind, got.Spec.Steps[1].Kind)
	}
	// The row half is qualified and the table half is bare; swapping them would still
	// decode, still parse, and silently match nothing.
	if got.Spec.Steps[1].Equi[0].Left != "e.ClusterId" || got.Spec.Steps[1].Equi[0].Right != "ClusterId" {
		t.Errorf("the equality halves came back in the wrong scopes: %+v", got.Spec.Steps[1].Equi[0])
	}
	if len(got.Spec.Post) != 1 || got.Spec.Post[0] != "m.Eff > 0" {
		t.Errorf("the residual WHERE did not survive: %v", got.Spec.Post)
	}
	if got.Spec.Steps[1].Side.Alias != "m" || got.Spec.Steps[1].Side.Table != "job_metrics" {
		t.Errorf("the folded side did not survive: %+v", got.Spec.Steps[1].Side)
	}
}

// TestChainRefusesAnOverlongChain pins the bound on a count read off the wire before anything
// is allocated for it.
func TestChainRefusesAnOverlongChain(t *testing.T) {
	jr := chainRPCRequest(db.JoinInner, db.JoinInner)
	for len(jr.Spec.Steps) < maxJoinChainSides+2 {
		jr.Spec.Steps = append(jr.Spec.Steps, jr.Spec.Steps[0])
	}
	frame := joinFrameOp(jr, opJoinChain, joinOpLayout(opJoinChain), false, false, false)(13)
	_, _, body, ok := reqHeader(frame)
	if !ok {
		t.Fatal("the request frame has no readable header")
	}
	if _, ok := readJoinRequest(body, layoutChain); ok {
		t.Fatalf("a chain of %d sides was accepted; the cap is %d",
			len(jr.Spec.Steps)+1, maxJoinChainSides)
	}
}

// TestChainFeatureIsNotAdvertisedWithoutACatalogThatJoinsChains pins the gate. A server whose
// catalog cannot join a chain must not name the feature: a definite yes that then fails is
// worse than silence, which is the whole reason advertisement exists.
func TestChainFeatureIsNotAdvertisedWithoutACatalogThatJoinsChains(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	full := NewServerCatalog(cat)
	defer full.Close()
	if !strings.Contains(full.SessionAttrs()[SessionAttrFeatures], string(FeatJoinChain)) {
		t.Error("a catalog that joins chains does not advertise the chain feature")
	}

	// A server holding a single database has no second table, let alone a third.
	d, err := db.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	one := NewServer(d)
	defer one.Close()
	feats := one.SessionAttrs()[SessionAttrFeatures]
	for _, f := range []Feature{FeatJoinChain, FeatJoinChainWire} {
		if strings.Contains(feats, string(f)) {
			t.Errorf("a server with no catalog advertises %s", f)
		}
	}
}

// TestChainRefusesAPrivateAttributeInAnyStep is a DIRECT test of a guard no join can reach,
// which is why it is written out rather than left to a functional test.
//
// Every expression in the plan is evaluated by the server over real ads, so each is gated
// against the private-attribute rules. Adding a third table added three more places to hide
// one -- that side's constraint, its ON terms and its equality halves -- and the failure of
// the gate is SILENT: the rows come back, correct, carrying a value the caller was not
// allowed to filter on. Nothing about the answer looks wrong.
func TestChainRefusesAPrivateAttributeInAnyStep(t *testing.T) {
	// An unprivileged connection: the gate is a no-op for a privileged one by design.
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, name := range []string{"history", "epoch_history", "job_metrics"} {
		if _, cerr := cat.CreateTable(name); cerr != nil {
			t.Fatal(cerr)
		}
	}
	srv := NewServerCatalog(cat)
	defer srv.Close()
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: false}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
	defer c.Close()

	const private = "ClaimId" // a private attribute by HTCondor's own rules
	if !classad.IsPrivateAttribute(private) {
		t.Fatalf("%s is not treated as private, so this test gates nothing", private)
	}

	for _, tc := range []struct {
		where string
		set   func(jr *JoinRequest)
	}{
		{"the third side's own constraint", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Side.Constraint = private + ` == "x"`
		}},
		{"the third step's ON terms", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Cond = append(jr.Spec.Steps[1].Cond, `m.`+private+` == "x"`)
		}},
		{"the third step's equality halves", func(jr *JoinRequest) {
			jr.Spec.Steps[1].Equi = []db.JoinEqui{{Left: "e.ClusterId", Right: private}}
		}},
	} {
		t.Run(tc.where, func(t *testing.T) {
			jr := chainRPCRequest(db.JoinInner, db.JoinInner)
			tc.set(&jr)
			_, err := c.JoinRows(context.Background(), jr)
			if err == nil {
				t.Fatalf("a private attribute in %s was accepted; the rows come back "+
					"correct and the caller filtered on something it may not see", tc.where)
			}
			if !strings.Contains(err.Error(), private) {
				t.Errorf("refused, but not for the private attribute: %v", err)
			}
		})
	}
}

// TestTwoTableJoinAlsoRefusesAnAliasQualifiedPrivateRef pins that the fix reaches the opcode
// that shipped, not only the new one.
//
// The hole was there first: `h.ClaimId == "x"` in a two-table ON clause passed the gate,
// because the gate looks for attribute references and a select's member name is not one. A
// test only over the chain would leave the shipped path uncovered while reading as a fix.
func TestTwoTableJoinAlsoRefusesAnAliasQualifiedPrivateRef(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	for _, n := range []string{"history", "epoch_history"} {
		if _, cerr := cat.CreateTable(n); cerr != nil {
			t.Fatal(cerr)
		}
	}
	srv := NewServerCatalog(cat)
	defer srv.Close()
	cconn, sconn := netPipe()
	go func() { _ = srv.ServeConnOpts(sconn, ServeOptions{IncludePrivate: false}) }()
	c := NewClient(cconn, WithPeerSessionAttrs(srv.SessionAttrs()))
	defer c.Close()

	jr := JoinRequest{Spec: db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
		Right: db.JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"ClusterId"}},
		Cond:  []string{`h.ClusterId == e.ClusterId && h.ClaimId == "x"`},
		Equi:  []db.JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}}
	if _, err := c.JoinRows(context.Background(), jr); err == nil {
		t.Fatal("a two-table join filtered on a private attribute through its alias")
	}
}
