package dbrpc

import (
	"bytes"
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/db"
)

// bandSpec windows hist's ExitCode between zero and the job's Cpus.
//
// It carries NO equality, which is the shape a band is for: against the fixture's 3 jobs and
// 4 history rows every pair is a candidate, and the window is the only thing that cuts them
// down. The predicate is in Cond as well, because a band narrows the candidates and the
// condition is what decides a pair.
func bandSpec() db.JoinSpec {
	return db.JoinSpec{
		Kind:  db.JoinInner,
		Left:  db.JoinSide{Table: "jobs", Alias: "j"},
		Right: db.JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"h.ExitCode >= 0", "h.ExitCode <= j.Cpus"},
		Band:  []db.JoinBand{{Attr: "ExitCode", Lo: "0", Hi: "j.Cpus"}},
	}
}

// bandSpecChain is bandSpec in the chain spelling. The same plan, so it must answer the same
// rows -- and it takes the other half of the layout byte to get there.
func bandSpecChain() db.JoinSpec {
	p := bandSpec()
	return db.JoinSpec{
		Left: p.Left,
		Steps: []db.JoinStep{{
			Kind: db.JoinInner,
			Side: p.Right,
			Cond: p.Cond,
			Band: p.Band,
		}},
	}
}

// wantBandRows is the answer worked out by hand: each job pairs with the history rows whose
// ExitCode falls in [0, Cpus]. Jobs carry Cpus 2, 4 and 8; history carries ExitCodes 0, 1, 7
// and 0, so the 7 joins the 8-cpu job alone.
func wantBandRows() []string {
	return []string{
		"1:0", "1:0", "1:1", // Cpus=2
		"2:0", "2:0", "2:1", // Cpus=4
		"3:0", "3:0", "3:1", "3:7", // Cpus=8
	}
}

// bandPairAd renders a joined row as cluster:exitcode.
func bandPairAd(t *testing.T, ad *classad.ClassAd) string {
	t.Helper()
	val := func(src string) string {
		ex, perr := classad.ParseExpr(src)
		if perr != nil {
			t.Fatalf("parsing %q: %v", src, perr)
		}
		n, nerr := ex.Eval(ad).IntValue()
		if nerr != nil {
			t.Fatalf("%s is not an integer in the joined row: %v", src, nerr)
		}
		return strconv.FormatInt(n, 10)
	}
	return val("j.ClusterId") + ":" + val("h.ExitCode")
}

// bandPairText renders a row from the TEXT reply.
func bandPairText(t *testing.T, text string) string {
	t.Helper()
	ad, err := classad.Parse(text)
	if err != nil {
		t.Fatalf("parsing joined row %q: %v", text, err)
	}
	return bandPairAd(t, ad)
}

// bandPairWire renders a row from the WIRE reply, which ships the stored nodes rather than
// text. Decoding it here rather than counting rows is deliberate: the two replies are
// separate encoders over one plan, and a band that reached only one of them would otherwise
// show up as a count that still matched.
func bandPairWire(t *testing.T, row []byte) string {
	t.Helper()
	ad, err := wire.DecodeInline(append([]byte(nil), row...))
	if err != nil {
		t.Fatalf("a wire joined row did not decode: %v", err)
	}
	return bandPairAd(t, classad.FromAST(ad))
}

// A band reaches the server and comes back as the rows it selects, on the row-streaming
// paths and in both spellings.
//
// The oracle is hand-written rather than taken from the in-process join, because the thing
// under test is whether the wire carried the window: an oracle that asked the same library
// the server asks would agree with it even if the band had been dropped at both ends.
func TestABandCrossesTheWireAndSelectsItsRows(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	ctx := context.Background()

	for _, c := range []struct {
		name string
		spec db.JoinSpec
	}{{"pair", bandSpec()}, {"chain", bandSpecChain()}} {
		t.Run(c.name, func(t *testing.T) {
			jr := JoinRequest{Spec: c.spec}

			var text []string
			if err := f.c.Join(ctx, jr, func(row string) bool {
				text = append(text, bandPairText(t, row))
				return true
			}); err != nil {
				t.Fatalf("Join: %v", err)
			}
			sort.Strings(text)
			assertRows(t, "Join", text, wantBandRows())

			var got []string
			if err := f.c.JoinWire(ctx, jr, false, func(row []byte) bool {
				got = append(got, bandPairWire(t, row))
				return true
			}); err != nil {
				t.Fatalf("JoinWire: %v", err)
			}
			sort.Strings(got)
			assertRows(t, "JoinWire", got, wantBandRows())
		})
	}
}

func assertRows(t *testing.T, what string, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s returned %d rows, want %d: %v", what, len(got), len(want), got)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("%s row %d is %q, want %q (all: %v)", what, i, got[i], want[i], got)
		}
	}
}

// The window narrows what the server examines, and says so in the statistics trailer.
//
// Without the band every one of the 12 pairs is examined; with it the server searches a
// window per row instead. Asserting the COUNTS rather than only the rows is what separates
// "the band arrived" from "the band arrived and was used" -- a server that carried the band
// and then ignored it answers the same rows.
func TestTheBandNarrowsWhatTheServerExamines(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	ctx := context.Background()

	run := func(spec db.JoinSpec) db.JoinStats {
		var rep JoinStatsReport
		err := f.c.JoinStats(ctx, JoinRequest{Spec: spec}, func(string) bool { return true }, &rep)
		if err != nil {
			t.Fatalf("JoinStats: %v", err)
		}
		if !rep.Reported {
			t.Fatal("the server reported no statistics")
		}
		return rep.Stats
	}

	banded := run(bandSpec())
	plain := bandSpec()
	plain.Band = nil
	unbanded := run(plain)

	if banded.BandWindows == 0 {
		t.Fatalf("no window was searched, so the band did not engage: %+v", banded)
	}
	if unbanded.BandWindows != 0 {
		t.Fatalf("a plan with no band reported %d windows", unbanded.BandWindows)
	}
	// 3 jobs x 4 history rows with nothing to key on.
	if unbanded.Pairs != 12 {
		t.Fatalf("the unbanded join examined %d pairs, want the whole product (12)", unbanded.Pairs)
	}
	if banded.Pairs >= unbanded.Pairs {
		t.Fatalf("the band examined %d pairs, no fewer than the %d a scan does",
			banded.Pairs, unbanded.Pairs)
	}
	if banded.Rows != unbanded.Rows {
		t.Fatalf("the band changed the answer: %d rows against %d", banded.Rows, unbanded.Rows)
	}
}

// newJoinFixtureWithout is newJoinFixture whose CLIENT was told the peer lacks one feature.
//
// The server is left able to serve it, so what the test exercises is the client's decision
// not to send rather than the server's to refuse -- the two halves are checked separately,
// because a test that confused them would pass with either one missing.
func newJoinFixtureWithout(t *testing.T, drop Feature) *joinFixture {
	t.Helper()
	f := newJoinFixture(t)
	attrs := f.s.SessionAttrs()
	kept := make([]string, 0, 8)
	for _, name := range strings.Split(attrs[SessionAttrFeatures], ",") {
		if name != string(drop) && name != "" {
			kept = append(kept, name)
		}
	}
	if len(kept) == len(strings.Split(attrs[SessionAttrFeatures], ",")) {
		t.Fatalf("%s was not advertised to begin with, so dropping it tests nothing", drop)
	}
	attrs[SessionAttrFeatures] = strings.Join(kept, ",")
	f.c.Close()
	f.c = NewClient(mustSecondConn(t, f.s), WithPeerSessionAttrs(attrs))
	return f
}

// Every exported path that takes a plan, against a peer that has NOT advertised bands.
//
// A path left out is the mistake that matters here: it would encode a band for a server that
// does not read one, and the rows outside the window would pair. The table is the whole
// exported surface for that reason.
func TestTheClientRefusesABandToAPeerWithoutIt(t *testing.T) {
	f := newJoinFixtureWithout(t, FeatJoinBand)
	defer f.close()
	jr := JoinRequest{Spec: bandSpec()}
	ctx := context.Background()
	aggOf := func(spec db.JoinSpec) JoinAggRequest {
		return JoinAggRequest{Spec: db.JoinAggSpec{
			Join: spec, GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
			Aggs: []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		}}
	}
	for _, c := range []struct {
		name string
		call func() error
	}{
		{"Join", func() error { return f.c.Join(ctx, jr, func(string) bool { return true }) }},
		{"JoinRows", func() error { _, err := f.c.JoinRows(ctx, jr); return err }},
		{"JoinStats", func() error {
			var rep JoinStatsReport
			return f.c.JoinStats(ctx, jr, func(string) bool { return true }, &rep)
		}},
		{"JoinWire", func() error {
			return f.c.JoinWire(ctx, jr, false, func([]byte) bool { return true })
		}},
		{"JoinWireStats", func() error {
			var rep JoinStatsReport
			return f.c.JoinWireStats(ctx, jr, false, func([]byte) bool { return true }, &rep)
		}},
		{"ExplainJoin", func() error { _, err := f.c.ExplainJoin(ctx, jr); return err }},
		{"JoinAggregate", func() error {
			_, err := f.c.JoinAggregate(ctx, aggOf(jr.Spec))
			return err
		}},
		{"JoinAggregateStats", func() error {
			var rep JoinStatsReport
			_, err := f.c.JoinAggregateStats(ctx, aggOf(jr.Spec), &rep)
			return err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if err := c.call(); !errors.Is(err, ErrJoinBandUnsupported) {
				t.Fatalf("%s sent a band to a peer without the feature, or failed "+
					"otherwise: %v", c.name, err)
			}
		})
	}
}

// The same surface, against a peer that DOES advertise bands: every path answers.
//
// It is the premise check for the test above. Without it a refusal everywhere would pass
// that table whether the cause was the missing feature or a band nothing can send.
func TestEveryPathCarriesABandToAPeerWithIt(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	jr := JoinRequest{Spec: bandSpec()}
	ctx := context.Background()
	want := len(wantBandRows())
	aggOf := func(spec db.JoinSpec) JoinAggRequest {
		return JoinAggRequest{Spec: db.JoinAggSpec{
			Join: spec, GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
			Aggs: []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
		}}
	}
	for _, c := range []struct {
		name string
		call func() (int, error)
	}{
		{"Join", func() (int, error) {
			n := 0
			err := f.c.Join(ctx, jr, func(string) bool { n++; return true })
			return n, err
		}},
		{"JoinRows", func() (int, error) {
			rows, err := f.c.JoinRows(ctx, jr)
			return len(rows), err
		}},
		{"JoinStats", func() (int, error) {
			var rep JoinStatsReport
			n := 0
			err := f.c.JoinStats(ctx, jr, func(string) bool { n++; return true }, &rep)
			return n, err
		}},
		{"JoinWire", func() (int, error) {
			n := 0
			err := f.c.JoinWire(ctx, jr, false, func([]byte) bool { n++; return true })
			return n, err
		}},
		{"JoinWireStats", func() (int, error) {
			var rep JoinStatsReport
			n := 0
			err := f.c.JoinWireStats(ctx, jr, false, func([]byte) bool { n++; return true }, &rep)
			return n, err
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			n, err := c.call()
			if err != nil {
				t.Fatalf("%s: %v", c.name, err)
			}
			if n != want {
				t.Fatalf("%s returned %d rows, want %d", c.name, n, want)
			}
		})
	}

	// The reduce funnel counts the same pairings, grouped by owner: alice holds the 2-cpu
	// and 8-cpu jobs (3 + 4 rows), bob the 4-cpu one (3).
	t.Run("JoinAggregate", func(t *testing.T) {
		rows, err := f.c.JoinAggregate(ctx, aggOf(jr.Spec))
		if err != nil {
			t.Fatalf("JoinAggregate: %v", err)
		}
		got := map[string]string{}
		for _, r := range rows {
			got[r.Group[0]] = r.Values[0]
		}
		if got[`"alice"`] != "7" || got[`"bob"`] != "3" {
			t.Fatalf("grouped counts are %v, want alice 7 and bob 3", got)
		}
	})

	// Explaining a banded plan is the only way to see that the window ENGAGED: a step with
	// an equality answers the same rows by hashing, so the rows cannot tell the two apart.
	t.Run("ExplainJoin", func(t *testing.T) {
		ex, err := f.c.ExplainJoin(ctx, jr)
		if err != nil {
			t.Fatalf("ExplainJoin: %v", err)
		}
		if len(ex.Steps) != 1 {
			t.Fatalf("the plan has %d steps, want 1", len(ex.Steps))
		}
		if !ex.Steps[0].Banded || len(ex.Steps[0].Band) != 1 {
			t.Fatalf("the explained step is not banded: %+v", ex.Steps[0])
		}
		if ex.Steps[0].Band[0].Attr != "ExitCode" {
			t.Fatalf("the explained band is over %q, want ExitCode", ex.Steps[0].Band[0].Attr)
		}
	})
}

// Every field of a band survives the encoder, including the two that say whether an end is
// open -- those are one byte, and a byte read as zero is a window that quietly admits its
// boundary rows.
func TestABandRoundTripsThroughTheEncoder(t *testing.T) {
	bands := []db.JoinBand{
		{Attr: "Ts", Lo: "j.Lo", Hi: "j.Hi"},
		{Attr: "Seq", Lo: "j.A", LoOpen: true},
		{Attr: "Pos", Hi: "j.B", HiOpen: true},
		{Attr: "Both", Lo: "j.C", Hi: "j.D", LoOpen: true, HiOpen: true},
	}
	pair := bandSpec()
	pair.Band = bands
	chain := bandSpecChain()
	chain.Steps[0].Band = bands

	for _, c := range []struct {
		name   string
		spec   db.JoinSpec
		layout joinLayout
		get    func(db.JoinSpec) []db.JoinBand
	}{
		{"pair", pair, layoutBand, func(s db.JoinSpec) []db.JoinBand { return s.Band }},
		{"chain", chain, layoutBand | layoutChain,
			func(s db.JoinSpec) []db.JoinBand { return s.Steps[0].Band }},
	} {
		t.Run(c.name, func(t *testing.T) {
			b := appendJoinPlan(nil, JoinRequest{Spec: c.spec}, c.layout)
			got, ok := readJoinRequest(&reader{b: b}, c.layout)
			if !ok {
				t.Fatal("the encoded plan did not read back")
			}
			out := c.get(got.Spec)
			if len(out) != len(bands) {
				t.Fatalf("read back %d bands, want %d", len(out), len(bands))
			}
			for i := range bands {
				if out[i] != bands[i] {
					t.Fatalf("band %d read back as %+v, want %+v", i, out[i], bands[i])
				}
			}
		})
	}
}

// A plan with no band encodes and reads back the SAME BYTES it always has.
//
// The band opcodes are new, but the layout now travels as a value rather than as a constant
// per call site, and getting that wrong would change the body of the opcodes that predate
// bands -- which every deployed server already reads.
func TestAnUnbandedPlanIsUnchangedOnTheWire(t *testing.T) {
	spec := bandSpec()
	spec.Band = nil
	jr := JoinRequest{Spec: spec}
	got := appendJoinPlan(nil, jr, 0)

	// Built the way the body was before a layout existed: sides, conditions, equalities,
	// and nothing after them.
	var want []byte
	want = append(want, byte(spec.Kind))
	want = putI32(want, 0)
	want = putI32(want, 0)
	want = putI32(want, 0)
	want = putI32(want, 0)
	want = putJoinSide(want, spec.Left)
	want = putJoinSide(want, spec.Right)
	want = putStrs(want, spec.Cond)
	want = putStrs(want, spec.Post)
	want = putJoinEqui(want, spec.Equi)

	if !bytes.Equal(got, want) {
		t.Fatalf("an unbanded plan encoded to %d bytes, want the %d it always has",
			len(got), len(want))
	}
}

// A layout byte this build does not define is refused rather than masked down to one it
// does. The bits name what the body holds, so reading a newer peer's frame as the nearest
// layout understood would parse the wrong bytes as a plan.
func TestAnUnknownLayoutByteIsRefused(t *testing.T) {
	for _, c := range []struct {
		name string
		byte uint8
		want joinLayout
		ok   bool
	}{
		{"pair", 0, 0, true},
		{"chain", 1, layoutChain, true},
		{"band", 2, layoutBand, true},
		{"banded chain", 3, layoutBand | layoutChain, true},
		{"one bit past", 4, 0, false},
		{"all bits", 255, 0, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, ok := readJoinLayout(&reader{b: []byte{c.byte}}, 0)
			if ok != c.ok || got != c.want {
				t.Fatalf("layout byte %d read as (%d, %v), want (%d, %v)",
					c.byte, got, ok, c.want, c.ok)
			}
		})
	}
	// The band opcodes require the band bit: a plan without one belongs on the opcode that
	// predates them, and accepting it here would give one plan two spellings on the wire.
	for _, b := range []uint8{0, 1} {
		if _, ok := readJoinLayout(&reader{b: []byte{b}}, layoutBand); ok {
			t.Fatalf("a band opcode accepted layout byte %d, which carries no band", b)
		}
	}
}

// bandlessCatalog is a catalog that joins but does not honour a window: the shape of a build
// that predates bands, which satisfies the join interface unchanged.
type bandlessCatalog struct{ *db.Catalog }

func (bandlessCatalog) JoinsBands() bool { return false }

// A server whose catalog does not honour bands refuses the request, even though the opcode
// reached a handler that could read it.
//
// This is the half advertisement cannot cover. A client may hold a stale list, or name the
// feature for a server that was reconfigured; the server's own check is what makes the band
// impossible to drop rather than merely unlikely to be.
func TestAServerRefusesABandItsCatalogDoesNotHonour(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	// The server keeps advertising bands -- the client's view is deliberately left intact,
	// so the request is actually sent and the SERVER is what refuses it.
	f.s.cat = bandlessCatalog{f.cat}

	err := f.c.Join(context.Background(), JoinRequest{Spec: bandSpec()},
		func(string) bool { return true })
	if err == nil {
		t.Fatal("a catalog that does not honour bands served a banded join")
	}
	if !errors.Is(err, ErrJoinUnsupported) && !errors.Is(err, ErrBadRequest) {
		t.Fatalf("the refusal was %v, want a bad request", err)
	}
}

// EVERY expression a plan carries reaches the private-attribute gate.
//
// The gate works by listing the expressions the server will evaluate, so what breaks it is
// not a wrong check but a FORGOTTEN field: a band's bounds are evaluated against real ads
// exactly as a constraint is, and one left off the list is a private attribute readable by
// anyone who puts it in a window. Each slot holds a distinct sentinel so a missing one names
// itself rather than hiding behind an identical string.
func TestTheGateSeesEveryExpressionInAPlan(t *testing.T) {
	spec := db.JoinSpec{
		Left:  db.JoinSide{Table: "jobs", Alias: "j", Constraint: "sentinelLeftConstraint"},
		Right: db.JoinSide{Table: "hist", Alias: "h", Constraint: "sentinelRightConstraint"},
		Cond:  []string{"sentinelCond"},
		Post:  []string{"sentinelPost"},
		Equi:  []db.JoinEqui{{Left: "sentinelEquiLeft", Right: "sentinelEquiRight"}},
		Band: []db.JoinBand{{
			Attr: "Ts", Lo: "sentinelBandLo", Hi: "sentinelBandHi",
		}},
		Steps: []db.JoinStep{{
			Side: db.JoinSide{Table: "ev", Alias: "e", Constraint: "sentinelStepConstraint"},
			Cond: []string{"sentinelStepCond"},
			Equi: []db.JoinEqui{{Left: "sentinelStepEquiLeft", Right: "sentinelStepEquiRight"}},
			Band: []db.JoinBand{{
				Attr: "Seq", Lo: "sentinelStepBandLo", Hi: "sentinelStepBandHi",
			}},
		}},
	}
	got := map[string]bool{}
	for _, src := range joinExprs(spec) {
		got[src] = true
	}
	for _, want := range []string{
		"sentinelLeftConstraint", "sentinelRightConstraint",
		"sentinelCond", "sentinelPost", "sentinelEquiLeft", "sentinelEquiRight",
		"sentinelBandLo", "sentinelBandHi",
		"sentinelStepConstraint", "sentinelStepCond",
		"sentinelStepEquiLeft", "sentinelStepEquiRight",
		"sentinelStepBandLo", "sentinelStepBandHi",
	} {
		if !got[want] {
			t.Errorf("%s never reaches the private-attribute gate", want)
		}
	}
}

// A malformed band is refused by the decoder rather than read into a window that means
// something else.
func TestAMalformedBandIsRefused(t *testing.T) {
	// A well-formed one first, so the cases below fail for their own reason and not
	// because the prefix was wrong all along.
	good := func(ends uint8, n int32) []byte {
		b := putI32(nil, n)
		b = putStr(putStr(putStr(b, "Ts"), "j.Lo"), "j.Hi")
		return putU8(b, ends)
	}
	if _, ok := readJoinBands(&reader{b: good(3, 1)}); !ok {
		t.Fatal("a well-formed band with both ends open did not read")
	}
	for _, c := range []struct {
		name string
		body []byte
	}{
		{"an ends byte with an undefined bit", good(4, 1)},
		{"an ends byte with every bit", good(255, 1)},
		{"a negative count", putI32(nil, -1)},
		{"a count past the cap", putI32(nil, int32(maxJoinBands+1))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := readJoinBands(&reader{b: c.body}); ok {
				t.Fatal("the decoder accepted it")
			}
		})
	}
}

// The reduce opcode refuses a layout byte it does not define. It carries bands as two new
// values of a field that already existed, so this is the check that makes an older server
// reject a banded reduction instead of running it with the window gone.
func TestAnUnknownAggLayoutIsRefused(t *testing.T) {
	f := newJoinFixture(t)
	defer f.close()
	jr := JoinAggRequest{Spec: db.JoinAggSpec{
		Join:      bandSpec(),
		GroupCols: []db.GroupCol{{Attr: "j.Owner"}},
		Aggs:      []db.AggSpec{{Func: db.AggCount, Arg: "*"}},
	}}
	// A refusal comes back as a STATUS on an otherwise successful exchange, not as a
	// transport error, so the status is what this has to read.
	status, _, err := f.c.callCtx(context.Background(), joinAggFrame(jr, joinLayout(7), false))
	if err != nil {
		t.Fatalf("the exchange itself failed: %v", err)
	}
	if status != stBadReq {
		t.Fatalf("the reduce opcode answered status %d for a layout byte it does not "+
			"define, want stBadReq (%d)", status, stBadReq)
	}

	// And the case that the byte is the ONLY thing standing in the way. Layout 4 sets a
	// bit this build does not define but neither of the two it does, so the body that
	// follows is a plain pair and parses perfectly as one. A handler that read the byte
	// and failed to act on it would serve this request rather than refusing it, and every
	// later check would agree the frame was well formed -- which is why the undefined bit
	// has to be refused where it is read.
	plain := jr
	plain.Spec.Join = bandSpec()
	plain.Spec.Join.Band = nil
	status, _, err = f.c.callCtx(context.Background(), joinAggFrame(plain, joinLayout(4), false))
	if err != nil {
		t.Fatalf("the exchange itself failed: %v", err)
	}
	if status != stBadReq {
		t.Fatalf("an undefined layout bit over a body that parses without it answered "+
			"status %d, want stBadReq (%d)", status, stBadReq)
	}
}

// bothlessCatalog joins, but folds in no third table and honours no window.
type bothlessCatalog struct{ *db.Catalog }

func (bothlessCatalog) JoinsChains() bool { return false }
func (bothlessCatalog) JoinsBands() bool  { return false }

// Each half of the capability check is load-bearing on its own.
//
// One function answers for both because a handler that checked one and forgot the other
// would serve the request, correctly as far as its own code could tell, with a conjunct of
// the match missing. The matrix is here so that removing either half fails.
func TestACatalogMustAnswerForEveryPartOfALayout(t *testing.T) {
	cat, err := db.OpenCatalog("")
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	full := any(cat)                     // the real one: chains and bands
	neither := any(bothlessCatalog{cat}) // a build that predates both

	for _, c := range []struct {
		name   string
		cat    any
		layout joinLayout
		want   bool
	}{
		{"plain pair, full catalog", full, 0, true},
		{"plain pair, old catalog", neither, 0, true},
		{"chain, full catalog", full, layoutChain, true},
		{"chain, old catalog", neither, layoutChain, false},
		{"band, full catalog", full, layoutBand, true},
		{"band, old catalog", neither, layoutBand, false},
		{"banded chain, full catalog", full, layoutBand | layoutChain, true},
		{"banded chain, old catalog", neither, layoutBand | layoutChain, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := joinCatalogServes(c.cat, c.layout); got != c.want {
				t.Fatalf("joinCatalogServes = %v, want %v", got, c.want)
			}
		})
	}
}
