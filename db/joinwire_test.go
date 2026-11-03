package db

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// THE ORACLE FOR SPLICING A JOINED ROW.
//
// spliceComposite assembles a row out of wire-format constants restated in this package,
// copying one side's attribute entries verbatim into a node of a composite it builds by
// hand. Every way that can be wrong is silent: a miscounted attribute, a key written in the
// wrong encoding, a header flag that tells the decoder to read interned ids as inline names.
// None of them fails -- they produce a row that decodes to something else, or to nothing.
//
// So the test is the real decoder against the row the ordinary path builds, over every shape
// the join has. If the format moves under this, these fail.

// joinWireRows runs JoinWire and returns each row decoded and rendered, copying as it goes
// because the rows alias one reused buffer.
func joinWireRows(t *testing.T, cat *Catalog, spec JoinSpec) []string {
	t.Helper()
	var st JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, false, &st)
	if err != nil {
		t.Fatalf("JoinWire: %v", err)
	}
	var out []string
	for row := range seq {
		ad, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Fatalf("a spliced row did not decode: %v", derr)
		}
		out = append(out, classad.FromAST(ad).String())
	}
	if st.Err != nil {
		t.Fatalf("JoinWire stopped early: %v", st.Err)
	}
	sort.Strings(out)
	return out
}

// TestSplicedRowDecodesAsTheCompositeDoes is the oracle: the same join both ways, row for row.
func TestSplicedRowDecodesAsTheCompositeDoes(t *testing.T) {
	cat := rowFormFixture(t)
	for _, tc := range rowFormSpecs() {
		t.Run(tc.name, func(t *testing.T) {
			want := joinRowTexts(t, cat, tc.spec)
			got := joinWireRows(t, cat, tc.spec)
			if len(got) != len(want) {
				t.Fatalf("spliced %d rows, the composite path produced %d", len(got), len(want))
			}
			if len(want) == 0 {
				t.Fatal("the shape produced no rows, so it compares nothing")
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("row %d differs:\n composite: %s\n   spliced: %s", i, want[i], got[i])
				}
			}
		})
	}
}

// TestSpliceIsTakenForTheOrdinaryShape pins that the fast path is the one that runs.
//
// Without it every shape could fall to encodeCompositeInline and the oracle above would still
// pass -- the fallback builds the same ad, which is what makes it a safe fallback and a
// useless implementation. This is what separates the two.
func TestSpliceIsTakenForTheOrdinaryShape(t *testing.T) {
	cat := rowFormFixture(t)
	spec := JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
	p, err := compileJoin(spec, DefaultJoinLimits())
	if err != nil {
		t.Fatal(err)
	}
	d, ok := cat.Table("history")
	if !ok {
		t.Fatal("no history table")
	}
	rows, err := d.QueryRawWireRefs("ClusterId == 1", []string{"ClusterId", "Owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var side *joinRow
	for r := range wireRows(rows) {
		side = r
		break
	}
	if side == nil {
		t.Fatal("the fixture produced no row")
	}
	if _, ok := p.spliceComposite(nil, side, nil); !ok {
		t.Fatal("the splice declined a narrowed wire row, which is the shape it exists for")
	}
	if side.ad != nil {
		t.Fatal("splicing decoded the row; the whole point is that it does not")
	}
}

// TestSpliceDeclinesWhatItCannotCopy pins the two refusals, because both would otherwise be
// silent: a side with no bytes has nothing to copy, and a whole-ad projection has no narrowed
// body. Each must decline so the caller builds the row the ordinary way.
func TestSpliceDeclinesWhatItCannotCopy(t *testing.T) {
	p := &joinPlan{leftAlias: "h", rightAlias: "e",
		leftProject: []string{"ClusterId"}, rightProject: []string{"NumShadowStarts"}}

	ad, err := classad.Parse(`[ ClusterId = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.spliceComposite(nil, &joinRow{ad: ad}, nil); ok {
		t.Error("a side that arrived decoded has no bytes to splice; it must decline")
	}

	whole := &joinPlan{leftAlias: "h", rightAlias: "e"} // nil projections: the whole ad
	cat := rowFormFixture(t)
	d, _ := cat.Table("history")
	rows, qerr := d.QueryRawWireRefs("ClusterId == 1", []string{"ClusterId"}, false)
	if qerr != nil {
		t.Fatal(qerr)
	}
	var side *joinRow
	for r := range wireRows(rows) {
		side = r
		break
	}
	if _, ok := whole.spliceComposite(nil, side, nil); ok {
		t.Error("a whole-ad projection has no narrowed body to copy; it must decline")
	}
}

// TestUnmatchedLeftRowSplicesWithoutTheRightMember pins SQL's NULL in the wire form: the
// absent side contributes no member at all, so every reference through its alias reads
// undefined -- which is what the composite path gives by leaving the member out.
func TestUnmatchedLeftRowSplicesWithoutTheRightMember(t *testing.T) {
	cat := rowFormFixture(t)
	spec := JoinSpec{
		Kind:  JoinLeft,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
		Cond:  []string{"h.ClusterId == e.ClusterId && e.NumShadowStarts > 99"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
	rows := joinWireRows(t, cat, spec)
	if len(rows) == 0 {
		t.Fatal("the LEFT join produced no rows")
	}
	for i, r := range rows {
		if strings.Contains(r, "e =") {
			t.Fatalf("row %d carries a right member though nothing matched: %s", i, r)
		}
		if !strings.Contains(r, "h =") {
			t.Fatalf("row %d lost its left member: %s", i, r)
		}
	}
}

// TestWireRowsAliasOneBuffer pins the contract the caller is given, because the cheap mistake
// is to keep a row. It is the same contract the store's own raw scans have, and it is stated
// here so a change that started copying would be a deliberate one.
func TestWireRowsAliasOneBuffer(t *testing.T) {
	cat := rowFormFixture(t)
	spec := JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
		Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
		Cond:  []string{"h.ClusterId == e.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
	var st JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, false, &st)
	if err != nil {
		t.Fatal(err)
	}
	var kept [][]byte
	n := 0
	for row := range seq {
		if n < 2 {
			kept = append(kept, row) // deliberately NOT copied
		}
		n++
	}
	if n < 8 || len(kept) != 2 {
		t.Fatalf("fixture produced %d rows; need several to reuse the buffer", n)
	}
	// Both kept slices point into the same buffer, so they now read alike. The point is not
	// that this is desirable -- it is that it is the documented contract, and a caller that
	// wants to keep a row copies it.
	if string(kept[0]) != string(kept[1]) {
		t.Skip("the buffer did not have to grow, so the rows did not alias; nothing to pin here")
	}
	// And a copy survives, which is what every caller in this package does.
	cp := append([]byte(nil), kept[0]...)
	if _, derr := wire.DecodeInline(cp); derr != nil {
		t.Fatalf("a copied row did not decode: %v", derr)
	}
}

// TestSpliceDeclinesAnInternedRow covers the branch no join can reach.
//
// Every side a join reads today arrives from QueryRawWireRefs, which serves self-contained
// inline-names rows, so the inline check never fires through the ordinary path -- a mutation
// deleting it passes every other test here. The branch still earns its place, and the reason
// is the shape of the failure rather than its likelihood: LookupByName answers (nil, false)
// for an attribute that is ABSENT and for an ad that is not inline, so without the check an
// interned row splices an EMPTY member. The joined row would carry an alias with nothing
// under it, decode cleanly, and be wrong.
func TestSpliceDeclinesAnInternedRow(t *testing.T) {
	ad, err := classad.Parse(`[ ClusterId = 1; Owner = "alice" ]`)
	if err != nil {
		t.Fatal(err)
	}
	interned := wire.Encode(nil, ad.AST(), wire.NewInternTable())
	row := &joinRow{w: wire.Ad(interned)}

	// The premise: this row really is not inline, and LookupByName really does conflate.
	if _, _, _, inline, ok := row.w.SplitBody(); !ok || inline {
		t.Fatalf("fixture is not an interned ad (ok=%v inline=%v); it cannot exercise the branch", ok, inline)
	}
	if _, found := row.w.LookupByName("Owner"); found {
		t.Fatal("LookupByName found an attribute in an interned ad; the conflation this guards is gone")
	}

	if _, ok := row.memberBody([]string{"ClusterId", "Owner"}); ok {
		t.Fatal("memberBody accepted an interned row; it would have spliced an empty member")
	}
	p := &joinPlan{leftAlias: "h", rightAlias: "e", leftProject: []string{"ClusterId"}}
	if _, ok := p.spliceComposite(nil, row, nil); ok {
		t.Fatal("the splice accepted an interned row")
	}
}

// privateJoinFixture gives each side a private attribute, so a redacted join has something to
// withhold and an unredacted one has something to carry.
func privateJoinFixture(t *testing.T) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	jobs, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateTable("hist")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 4; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; Owner = "user%d"; ClaimId = "secret-left-%d" ]`, i, i, i))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := jobs.Put(fmt.Sprint(i), ad); werr != nil {
			t.Fatal(werr)
		}
		h, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; ExitCode = 0; TransferKey = "secret-right-%d" ]`, i, i))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprint(i), h); werr != nil {
			t.Fatal(werr)
		}
	}
	return cat
}

func privateJoinSpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j", Project: []string{"ClusterId", "Owner", "ClaimId"}},
		Right: JoinSide{Table: "hist", Alias: "h", Project: []string{"ExitCode", "TransferKey"}},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// TestRedactedWireJoinWithholdsPrivateAttributes pins the one thing a wire row can get wrong
// that no decode will reveal: the sides are read UNREDACTED inside the store on purpose, so a
// row shipped without re-deciding would hand an unprivileged reader every private attribute.
//
// Both sides are checked, because they are narrowed by different code -- the left member and
// the right member go through the same splice but from different projections, and a redaction
// applied to one would look like a pass.
func TestRedactedWireJoinWithholdsPrivateAttributes(t *testing.T) {
	cat := privateJoinFixture(t)
	spec := privateJoinSpec()

	var st JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, true, &st)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for row := range seq {
		ad, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Fatalf("redacted row did not decode: %v", derr)
		}
		text := classad.FromAST(ad).String()
		for _, secret := range []string{"ClaimId", "secret-left", "TransferKey", "secret-right"} {
			if strings.Contains(text, secret) {
				t.Fatalf("a redacted joined row carries %q: %s", secret, text)
			}
		}
		if !strings.Contains(text, "Owner") || !strings.Contains(text, "ExitCode") {
			t.Fatalf("redaction took the public attributes too: %s", text)
		}
		rows++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if rows != 4 {
		t.Fatalf("got %d rows, want 4: redaction must not change which rows match", rows)
	}

	// Unredacted, the same join carries them -- so the test above is withholding something
	// that was really there.
	seq, err = cat.JoinWire(spec, JoinLimits{}, false, &st)
	if err != nil {
		t.Fatal(err)
	}
	carried := 0
	for row := range seq {
		ad, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Fatal(derr)
		}
		text := classad.FromAST(ad).StringWithPrivate()
		if strings.Contains(text, "secret-left") && strings.Contains(text, "secret-right") {
			carried++
		}
	}
	if carried != 4 {
		t.Fatalf("%d of 4 unredacted rows carried both private attributes; the redaction test proves nothing", carried)
	}
}

// TestRedactionDoesNotChangeWhichRowsMatch pins that a condition over a private attribute
// still decides the join. Redacting before the condition would make the ANSWER depend on who
// asked, which is a different failure from leaking and just as quiet.
func TestRedactionDoesNotChangeWhichRowsMatch(t *testing.T) {
	cat := privateJoinFixture(t)
	spec := privateJoinSpec()
	spec.Post = []string{`j.ClaimId == "secret-left-2"`}

	for _, redact := range []bool{false, true} {
		var st JoinStats
		seq, err := cat.JoinWire(spec, JoinLimits{}, redact, &st)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range seq {
			n++
		}
		if st.Err != nil {
			t.Fatal(st.Err)
		}
		if n != 1 {
			t.Errorf("redact=%v: got %d rows, want 1 -- the filter reads a private attribute "+
				"and must still decide the join", redact, n)
		}
	}
}

// TestRedactedWholeAdJoinRefuses pins the refusal. A whole-ad member has no projection to
// filter and the private names live inside the member, so nothing here can withhold them;
// shipping it would leak every one. The caller falls back to the text stream, whose render
// withholds them.
func TestRedactedWholeAdJoinRefuses(t *testing.T) {
	cat := privateJoinFixture(t)
	spec := privateJoinSpec()
	spec.Right.Project = nil // the whole ad

	var st JoinStats
	if _, err := cat.JoinWire(spec, JoinLimits{}, true, &st); err == nil {
		t.Fatal("a redacted whole-ad join was accepted; it cannot withhold anything")
	} else if !errors.Is(err, ErrJoinWireRedactNeedsProjection) {
		t.Fatalf("refused with %v, want ErrJoinWireRedactNeedsProjection", err)
	}
	// Unredacted it is fine: nothing is being withheld, so nothing can leak.
	if _, err := cat.JoinWire(spec, JoinLimits{}, false, &st); err != nil {
		t.Fatalf("an unredacted whole-ad join should be served: %v", err)
	}
}
