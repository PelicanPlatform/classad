package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

func withWholeRead(t *testing.T) {
	t.Helper()
	forceWholeRead = true
	t.Cleanup(func() { forceWholeRead = false })
}

func readJoinFixture(t *testing.T) *Catalog {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateTable("history")
	if err != nil {
		t.Fatal(err)
	}
	epoch, err := cat.CreateTable("epoch_history")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 24; i++ {
		ad, perr := classad.Parse(fmt.Sprintf(
			`[ ClusterId = %d; ProcId = 0; Owner = "user%d"; JobStatus = 4; RequestMemory = %d; Site = "s%d"; Filler = "x" ]`,
			i, i%3, 1024+(i%4)*512, i%2))
		if perr != nil {
			t.Fatal(perr)
		}
		if werr := hist.Put(fmt.Sprintf("h%d", i), ad); werr != nil {
			t.Fatal(werr)
		}
		for e := 0; e < 3; e++ {
			ead, perr := classad.Parse(fmt.Sprintf(
				`[ ClusterId = %d; ProcId = 0; NumShadowStarts = %d; RequestMemory = %d; MemoryUsage = %d; Filler = "y" ]`,
				i, e+1, 1024+(i%4)*512+e*256, 900+e*200))
			if perr != nil {
				t.Fatal(perr)
			}
			if werr := epoch.Put(fmt.Sprintf("e%d.%d", i, e), ead); werr != nil {
				t.Fatal(werr)
			}
		}
	}
	return cat
}

func joinRowTexts(t *testing.T, cat *Catalog, spec JoinSpec) []string {
	t.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for row := range seq {
		out = append(out, row.String())
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	sort.Strings(out)
	return out
}

// readProjectionSpecs are join shapes whose answer must not depend on how the sides were
// read. Each one puts a reference somewhere the output projection does not mention, which is
// the case a read projection gets wrong.
func readProjectionSpecs() map[string]JoinSpec {
	base := func() JoinSpec {
		return JoinSpec{
			Kind:  JoinInner,
			Left:  JoinSide{Table: "history", Alias: "h", Project: []string{"ClusterId", "Owner"}},
			Right: JoinSide{Table: "epoch_history", Alias: "e", Project: []string{"NumShadowStarts"}},
			Cond:  []string{"h.ClusterId == e.ClusterId"},
			Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
		}
	}
	specs := map[string]JoinSpec{}

	specs["plain equi"] = base()

	// The condition reads RequestMemory on both sides; NEITHER output projection names it.
	// Drop it from the read and the condition sees UNDEFINED and the join returns nothing.
	s := base()
	s.Cond = append(s.Cond, "e.RequestMemory > h.RequestMemory")
	specs["condition reads an unprojected attribute"] = s

	// The residual WHERE reads a side. It can only read what the OUTPUT projection carries
	// (see TestWhereReadsOnlyTheProjectedRow), so the attribute is projected here; the read
	// projection still has to carry it, which is the thing under test.
	s = base()
	s.Right.Project = []string{"NumShadowStarts", "MemoryUsage"}
	s.Post = []string{"e.MemoryUsage > 1000"}
	specs["where reads a projected attribute"] = s

	// A LEFT join is the dangerous one: a dropped attribute does not reduce the row count,
	// it turns matched rows into null-extended ones, which looks like data rather than a bug.
	s = base()
	s.Kind = JoinLeft
	s.Cond = append(s.Cond, "e.RequestMemory > h.RequestMemory")
	specs["left join whose condition reads an unprojected attribute"] = s

	// The equality halves name an attribute the output never carries.
	s = base()
	s.Left.Project = []string{"Owner"}
	s.Right.Project = []string{"NumShadowStarts"}
	specs["equality on an unprojected attribute"] = s

	// An equality on an attribute NO condition term mentions. Equi is meant to be the
	// equality conjuncts OF Cond, but nothing enforces that, and here ProcId reaches the
	// read projection only through the equality. Drop that contribution and the hash key is
	// undefined on both sides, which matches nothing: every row disappears. Every ProcId is
	// 0, so the hash narrows nothing and the rows must equal the plain equi case.
	s = base()
	s.Equi = append(s.Equi, JoinEqui{Left: "ProcId", Right: "ProcId"})
	specs["equality the condition does not mention"] = s

	// A whole-ad output projection on one side must not be narrowed at all.
	s = base()
	s.Left.Project = nil
	specs["left side carries the whole ad"] = s

	// A term the alias compiler declines must take the projection down with it rather than
	// contributing the references it happened to parse.
	s = base()
	s.Cond = append(s.Cond, `strcat(h.Owner, "") == e.Owner || e.NumShadowStarts >= 1`)
	specs["a declined term"] = s

	// A side read by nothing at all still has to produce its rows.
	s = base()
	s.Left.Project = []string{}
	specs["left side projects nothing"] = s

	return specs
}

// TestNarrowedReadMatchesWholeRead is the oracle: every shape, both ways, same rows.
func TestNarrowedReadMatchesWholeRead(t *testing.T) {
	for name, spec := range readProjectionSpecs() {
		t.Run(name, func(t *testing.T) {
			cat := readJoinFixture(t)
			narrow := joinRowTexts(t, cat, spec)
			func() {
				withWholeRead(t)
				whole := joinRowTexts(t, cat, spec)
				if len(narrow) != len(whole) {
					t.Fatalf("narrowed read gave %d rows, whole read gave %d\nnarrow: %v\nwhole:  %v",
						len(narrow), len(whole), narrow, whole)
				}
				for i := range narrow {
					if narrow[i] != whole[i] {
						t.Fatalf("row %d differs\n narrow: %s\n whole:  %s", i, narrow[i], whole[i])
					}
				}
			}()
			if len(narrow) == 0 {
				t.Fatal("the join produced no rows; the comparison is vacuous")
			}
		})
	}
}

// TestNarrowedReadActuallyNarrows stops the oracle above passing because nothing was
// narrowed. An equivalence test is satisfied by a no-op, so the plan is asserted directly.
func TestNarrowedReadActuallyNarrows(t *testing.T) {
	spec := readProjectionSpecs()["condition reads an unprojected attribute"]
	p := planJoinRead(spec)
	if p.left == nil || p.right == nil {
		t.Fatalf("expected both sides narrowed, got left=%v right=%v", p.left, p.right)
	}
	for _, want := range []string{"ClusterId", "Owner", "RequestMemory"} {
		if !hasProjAttr(p.left, want) {
			t.Errorf("left read projection %v is missing %s", p.left, want)
		}
	}
	for _, want := range []string{"ClusterId", "NumShadowStarts", "RequestMemory"} {
		if !hasProjAttr(p.right, want) {
			t.Errorf("right read projection %v is missing %s", p.right, want)
		}
	}
	// Filler is read by nothing and carried by nothing.
	if hasProjAttr(p.left, "Filler") || hasProjAttr(p.right, "Filler") {
		t.Errorf("read projection carries an attribute nothing asked for: left=%v right=%v", p.left, p.right)
	}
}

// TestDeclinedTermReadsWholeAds pins the all-or-nothing rule: one term the compiler will not
// take natively, and BOTH sides go back to the whole ad, because the term may have been
// reaching into either.
func TestDeclinedTermReadsWholeAds(t *testing.T) {
	spec := readProjectionSpecs()["a declined term"]
	p := planJoinRead(spec)
	if p.left != nil || p.right != nil {
		t.Fatalf("a declined term must take both projections down, got left=%v right=%v", p.left, p.right)
	}
}

// TestWholeAdProjectionIsNotNarrowed: a side whose caller asked for the whole ad is read
// whole, whatever the other side does.
func TestWholeAdProjectionIsNotNarrowed(t *testing.T) {
	spec := readProjectionSpecs()["left side carries the whole ad"]
	p := planJoinRead(spec)
	if p.left != nil {
		t.Errorf("a nil output projection must read the whole ad, got %v", p.left)
	}
	if p.right == nil {
		t.Error("the other side should still be narrowed")
	}
}

// TestNonLiteralEquiHalfDeclinesOneSide: an equality half this cannot account for costs its
// OWN side's projection and leaves the other narrow.
func TestNonLiteralEquiHalfDeclinesOneSide(t *testing.T) {
	spec := readProjectionSpecs()["plain equi"]
	spec.Equi = []JoinEqui{{Left: `strcat(Owner, "")`, Right: "ClusterId"}}
	p := planJoinRead(spec)
	if p.left != nil {
		t.Errorf("an unanalysable left half must read the whole left ad, got %v", p.left)
	}
	if p.right == nil {
		t.Error("the right half is a bare name; that side should stay narrow")
	}
}

// TestWireRowsSurviveTheirBuffer pins the assumption decodeWireRows rests on. Wire rows alias
// a buffer the scan reuses, and a join HOLDS the hashed side, so if decoding aliased those
// bytes every held row would be overwritten by the next one and the join would silently
// produce rows built from the wrong record. Decoding copies; this fails if that changes.
func TestWireRowsSurviveTheirBuffer(t *testing.T) {
	cat := readJoinFixture(t)
	d, ok := cat.Table("history")
	if !ok {
		t.Fatal("no history table")
	}
	rows, err := d.QueryRawWireRefs("true", []string{"ClusterId", "Owner"}, false)
	if err != nil {
		t.Fatal(err)
	}
	var held []*classad.ClassAd
	for ad := range decodeWireRows(rows) {
		held = append(held, ad)
	}
	if len(held) != 24 {
		t.Fatalf("held %d rows, want 24", len(held))
	}
	// Every ad was decoded before the buffer moved on; all of them must still read right.
	seen := map[int64]bool{}
	for i, ad := range held {
		v := ad.EvaluateAttr("ClusterId")
		n, verr := v.IntValue()
		if verr != nil {
			t.Fatalf("row %d: ClusterId did not survive the buffer: %v", i, verr)
		}
		if seen[n] {
			t.Fatalf("ClusterId %d seen twice: held rows alias one reused buffer", n)
		}
		seen[n] = true
		o := ad.EvaluateAttr("Owner")
		s, serr := o.StringValue()
		if serr != nil || !strings.HasPrefix(s, "user") {
			t.Fatalf("row %d: Owner did not survive the buffer: %q (%v)", i, s, serr)
		}
	}
}

func hasProjAttr(list []string, want string) bool {
	for _, s := range list {
		if strings.EqualFold(s, want) {
			return true
		}
	}
	return false
}

// TestWhereReadsTheSidesNotTheProjectedRow is the fix for a silently wrong answer: a WHERE
// term naming an attribute the OUTPUT projection does not carry used to read UNDEFINED and
// match nothing, because emit built the row through the projection and filtered afterwards.
// A WHERE is not a projection, and the two have no reason to be the same list.
func TestWhereReadsTheSidesNotTheProjectedRow(t *testing.T) {
	cat := readJoinFixture(t)
	spec := readProjectionSpecs()["plain equi"] // right side projects NumShadowStarts only
	spec.Post = []string{"e.MemoryUsage > 1000"}

	got := joinRowTexts(t, cat, spec)
	if len(got) == 0 {
		t.Fatal("a WHERE over an unprojected attribute matched nothing; it must read the side as read")
	}
	// MemoryUsage is 900/1100/1300 per job, so exactly two of the three attempts survive,
	// and the filter is doing work rather than passing everything.
	if want := 24 * 2; len(got) != want {
		t.Fatalf("got %d rows, want %d", len(got), want)
	}
	// The output still carries only what was projected: filtering on an attribute does not
	// smuggle it into the result.
	for _, row := range got {
		if strings.Contains(row, "MemoryUsage") {
			t.Fatalf("the filtered attribute leaked into the output row: %s", row)
		}
	}
}

// TestWhereOnAnUnprojectedAttributeMatchesTheWholeReadPath pins that the fix is in the
// FILTER, not in the read: the same query answers the same whether the sides were narrowed
// or read whole.
func TestWhereOnAnUnprojectedAttributeMatchesTheWholeReadPath(t *testing.T) {
	cat := readJoinFixture(t)
	spec := readProjectionSpecs()["plain equi"]
	spec.Post = []string{"e.MemoryUsage > 1000"}

	narrow := joinRowTexts(t, cat, spec)
	withWholeRead(t)
	whole := joinRowTexts(t, cat, spec)
	if len(narrow) != len(whole) {
		t.Fatalf("narrowed read gave %d rows, whole read gave %d", len(narrow), len(whole))
	}
	for i := range narrow {
		if narrow[i] != whole[i] {
			t.Fatalf("row %d differs\n narrow: %s\n whole:  %s", i, narrow[i], whole[i])
		}
	}
}

// TestLeftJoinWhereDropsRatherThanNullExtends: a LEFT row whose pair MATCHED and was then
// dropped by WHERE must stay dropped, not come back null-extended. Now that the filter reads
// the unprojected sides it drops rows it previously never saw, so the rule it must not break
// is worth re-asserting against an attribute the output does not carry.
func TestLeftJoinWhereDropsRatherThanNullExtends(t *testing.T) {
	cat := readJoinFixture(t)
	spec := readProjectionSpecs()["plain equi"]
	spec.Kind = JoinLeft
	spec.Post = []string{"e.MemoryUsage > 100000"} // matches nothing

	got := joinRowTexts(t, cat, spec)
	if len(got) != 0 {
		t.Fatalf("every pair matched and was dropped by WHERE, so no row should survive; got %d:\n%v",
			len(got), got[:min(3, len(got))])
	}
}

// TestDeclinedWhereTermStillReadsTheSides: a term the alias compiler will not take falls
// back to a composite, and that composite must be built WITHOUT the output projection too --
// otherwise the bug simply moves into the fallback.
func TestDeclinedWhereTermStillReadsTheSides(t *testing.T) {
	cat := readJoinFixture(t)
	spec := readProjectionSpecs()["plain equi"]
	spec.Post = []string{`strcat(e.Filler, "") == "y"`} // strcat is not strict: declined

	p, err := compileJoin(spec, DefaultJoinLimits())
	if err != nil {
		t.Fatal(err)
	}
	if p.postAlias != nil {
		t.Fatal("strcat should not compile natively; this test is not exercising the fallback")
	}
	if got := joinRowTexts(t, cat, spec); len(got) == 0 {
		t.Fatal("the fallback filtered a projected row; it must read the sides as read")
	}
}

// TestWhereIdentityAgainstUndefinedChangesAnswer records the one way this fix is NOT purely
// additive, so it is not discovered as a surprise.
//
// Everywhere else, filtering the sides rather than the projected row can only turn dropped
// rows into kept ones: a term that read UNDEFINED was not true, so its rows were lost. But
// `=?=` and `is` compare identity, and UNDEFINED =?= UNDEFINED is TRUE -- so a term asking
// whether an attribute is undefined used to be true for every row merely because the
// projection had hidden the attribute, and is now true only when the side really lacks it.
//
// That is the correct reading of the term. It does mean such a query answers differently.
func TestWhereIdentityAgainstUndefinedChangesAnswer(t *testing.T) {
	cat := readJoinFixture(t)
	spec := readProjectionSpecs()["plain equi"] // right side projects NumShadowStarts only

	// MemoryUsage is defined on every epoch row but is not projected.
	spec.Post = []string{"e.MemoryUsage =?= UNDEFINED"}
	if got := joinRowTexts(t, cat, spec); len(got) != 0 {
		t.Fatalf("the attribute is defined on the side, so the term is false: got %d rows", len(got))
	}

	// An attribute no row defines is undefined whichever way it is read.
	spec.Post = []string{"e.NoSuchAttribute =?= UNDEFINED"}
	if got := joinRowTexts(t, cat, spec); len(got) == 0 {
		t.Fatal("an attribute absent from the side really is undefined; the term should hold")
	}
}
