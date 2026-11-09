package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// EXCHANGING A LEFT JOIN'S SIDES.
//
// The rows a LEFT join produces do not depend on which side the executor holds, so every
// test here is the same comparison: run the join with the planner free to exchange the
// sides, run it again with forceNoBuildSideSwap, and require the two to answer with the
// same rows. What each one adds is a reason the two could have differed -- a preserved row
// that matched nothing, one with no join key at all, a WHERE that drops a pair, a limit, a
// parallel walk -- because the swapped plan produces the unmatched rows from the other end
// and every one of those is a way of getting that wrong.

// leftOuterFixture is a small preserved side against a large one, as mutable tables so
// neither promises an order and neither carries an index: the planner's choice is then
// about what it HOLDS, which is what exchanging a LEFT join's sides is for.
//
// The preserved side has three kinds of row in it, and the last two are the point:
//
//   - a key that matches, several times over, so the output expands;
//   - a key that matches nothing, which the join must still produce null-extended;
//   - no key at all, which the hash drops into no bucket -- so a plan that emits its
//     leftovers by walking the buckets would never see it again.
func leftOuterFixture(tb testing.TB, keys, facts int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	kt, err := cat.CreateTable("keys")
	if err != nil {
		tb.Fatal(err)
	}
	ft, err := cat.CreateTable("facts")
	if err != nil {
		tb.Fatal(err)
	}
	for j := 0; j < facts; j++ {
		id := j % (facts / 2) // two facts per key, so a matched key expands
		ad, perr := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nExitCode = %d\nSeq = %d", id, id%3, j))
		if perr != nil {
			tb.Fatal(perr)
		}
		if perr = ft.Put(fmt.Sprintf("f%d", j), ad); perr != nil {
			tb.Fatal(perr)
		}
	}
	for i := 0; i < keys; i++ {
		src := fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"", i, i%4)
		switch i % 5 {
		case 4:
			src = fmt.Sprintf("Owner = \"user%d\"\nTag = %d", i%4, i) // no join key at all
		case 3:
			src = fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"", 900000+i, i%4) // matches nothing
		}
		ad, perr := classad.ParseOld(src)
		if perr != nil {
			tb.Fatal(perr)
		}
		if perr = kt.Put(fmt.Sprintf("k%d", i), ad); perr != nil {
			tb.Fatal(perr)
		}
	}
	return cat
}

// leftOuterSpec is `keys LEFT JOIN facts`, written with keys -- the preserved side -- first,
// which is the only way a LEFT join can be written.
func leftOuterSpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinLeft,
		Left:  JoinSide{Table: "keys", Alias: "k"},
		Right: JoinSide{Table: "facts", Alias: "f"},
		Cond:  []string{"k.ClusterId == f.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

// joinRows drains a join into sorted row text, which is what the two orientations are
// compared on: the swap is allowed to change the order and nothing else.
func joinRows(tb testing.TB, cat *Catalog, spec JoinSpec, limits JoinLimits) ([]string, JoinStats) {
	tb.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, limits, &st)
	if err != nil {
		tb.Fatal(err)
	}
	var out []string
	for ad := range seq {
		out = append(out, ad.String())
	}
	sort.Strings(out)
	return out, st
}

// unswappedRows is the same join with the planner's choice disabled, which is the answer
// every swapped plan here is measured against.
func unswappedRows(tb testing.TB, cat *Catalog, spec JoinSpec, limits JoinLimits) ([]string, JoinStats) {
	tb.Helper()
	forceNoBuildSideSwap = true
	defer func() { forceNoBuildSideSwap = false }()
	rows, st := joinRows(tb, cat, spec, limits)
	if st.BuildSideSwapped {
		tb.Fatal("the arm with the choice disabled exchanged the sides anyway")
	}
	return rows, st
}

// nullExtended counts the rows carrying no member for the side that matched nothing.
func nullExtended(rows []string, alias string) int {
	n := 0
	for _, r := range rows {
		if !strings.Contains(r, alias+" = [") {
			n++
		}
	}
	return n
}

func requireSameRows(tb testing.TB, what string, got, want []string) {
	tb.Helper()
	if len(got) != len(want) {
		tb.Fatalf("%s: %d rows against %d", what, len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			tb.Fatalf("%s: row %d differs:\n swapped: %s\n written: %s", what, i, got[i], want[i])
		}
	}
}

// The whole claim, in one test: a LEFT join whose sides the planner exchanged answers with
// the same rows, including every preserved row that joined to nothing.
func TestLeftJoinSwapKeepsEveryPreservedRow(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()

	swapped, sst := joinRows(t, cat, spec, JoinLimits{})
	asWritten, ast := unswappedRows(t, cat, spec, JoinLimits{})

	if !sst.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged, so nothing was compared: %s", sst.BuildSideWhy)
	}
	// 30 keys match two facts each, 10 match nothing, 10 have no key: 80 rows, 20 of them
	// null-extended. Asserted outright rather than only against the other arm, so a bug
	// that drops the same rows from both plans cannot pass this.
	if len(swapped) != 80 {
		t.Fatalf("the join produced %d rows, expected 80", len(swapped))
	}
	if n := nullExtended(swapped, "f"); n != 20 {
		t.Fatalf("%d rows came back null-extended, expected 20", n)
	}
	requireSameRows(t, "swapped against as written", swapped, asWritten)

	// What the exchange was for. As written the join holds the large side; swapped it holds
	// the small one, and the preserved side is small exactly when this decision matters.
	if sst.HashRows >= ast.HashRows {
		t.Fatalf("the swap held %d rows against %d as written, so it held no less",
			sst.HashRows, ast.HashRows)
	}
	if sst.HashRows != 50 {
		t.Fatalf("the swapped plan held %d rows, expected the preserved side's 50", sst.HashRows)
	}
}

// A preserved row with no join key is in no bucket, so the leftovers cannot be found by
// walking the buckets alone. This is the same fixture asked the question directly, because
// the row set above would hide the loss among 79 others.
func TestLeftJoinSwapKeepsAPreservedRowWithNoJoinKey(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()
	spec.Post = []string{"isUndefined(k.ClusterId)"}

	swapped, sst := joinRows(t, cat, spec, JoinLimits{})
	if !sst.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", sst.BuildSideWhy)
	}
	if len(swapped) != 10 {
		t.Fatalf("%d keyless rows came back, expected 10", len(swapped))
	}
	asWritten, _ := unswappedRows(t, cat, spec, JoinLimits{})
	requireSameRows(t, "keyless rows", swapped, asWritten)
}

// A pair that matched and was then dropped by WHERE must not bring its preserved row back
// null-extended -- the same line a LEFT join draws the ordinary way round. The swapped plan
// draws it by recording the match where the ON condition held rather than where the row
// survived, and this is what tells the two apart: every key whose facts the WHERE rejects
// would otherwise reappear as a row that matched nothing.
func TestLeftJoinSwapDoesNotResurrectAPairWhereDropped(t *testing.T) {
	const keys = 50
	cat := leftOuterFixture(t, keys, 5000)
	spec := leftOuterSpec()
	// Keeps a matched pair only when the fact's ExitCode is 0, and keeps every
	// null-extended row -- so a preserved row wrongly counted as unmatched SHOWS UP here
	// instead of being filtered away with the pairs it came from.
	spec.Post = []string{"isUndefined(f.ExitCode) || f.ExitCode == 0"}

	// A fact's ExitCode is its ClusterId mod 3, so a matched key's pairs are all kept or
	// all dropped together. These are the keys whose pairs are all dropped: without them
	// the test would be asking a question with no case in it.
	allDropped := 0
	for i := 0; i < keys; i++ {
		if i%5 >= 3 {
			continue // no key, or a key that matches nothing
		}
		if i%3 != 0 {
			allDropped++
		}
	}
	if allDropped == 0 {
		t.Fatal("no matched key has all of its pairs dropped, so this proves nothing")
	}

	swapped, sst := joinRows(t, cat, spec, JoinLimits{})
	if !sst.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", sst.BuildSideWhy)
	}
	if n := nullExtended(swapped, "f"); n != 20 {
		t.Fatalf("%d rows came back null-extended, expected the 20 keys that matched nothing; "+
			"%d more keys matched and had every pair dropped by WHERE", n, allDropped)
	}
	asWritten, _ := unswappedRows(t, cat, spec, JoinLimits{})
	requireSameRows(t, "with a WHERE over the pair", swapped, asWritten)
}

// The headline: a LEFT join that cannot run as written, because holding the side it was
// written to hash exceeds what one join may hold, runs when the sides are exchanged.
func TestLeftJoinSwapRunsWhereHoldingTheOtherSideWouldNot(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()
	limits := JoinLimits{MaxHashRows: 500}

	rows, st := joinRows(t, cat, spec, limits)
	if st.Err != nil {
		t.Fatalf("the swapped join did not run: %v", st.Err)
	}
	if len(rows) != 80 {
		t.Fatalf("the swapped join produced %d rows, expected 80", len(rows))
	}

	_, ast := unswappedRows(t, cat, spec, limits)
	if ast.Err == nil {
		t.Fatal("the join as written ran under a limit it should have exceeded, so the " +
			"swapped arm proved nothing")
	}
	if !strings.Contains(ast.Err.Error(), "would hold more than") {
		t.Fatalf("the join as written failed for the wrong reason: %v", ast.Err)
	}
}

// A LEFT join is exchanged to hold less, never to read less. The swapped orientation hashes
// the PRESERVED side, and narrowing that side to the keys the streamed side holds is exactly
// a filter that drops the rows with no match -- the rows the join exists to keep. So the
// probe is refused, and the cost model has to price the swap without it: here, where an
// index covers the join key on the large side, the plan that keeps the index wins and the
// sides stay as written.
func TestLeftJoinKeepsTheOrientationThatCanBeProbed(t *testing.T) {
	cat := leftProbeFixture(t, 8192, 32)
	spec := leftProbeSpec()
	spec.Kind = JoinLeft

	var st JoinStats
	if _, err := cat.Join(spec, JoinLimits{}, &st); err != nil {
		t.Fatal(err)
	}
	if st.BuildSideSwapped {
		t.Fatalf("the sides were exchanged, giving up an index that covers the hashed "+
			"side: %s", st.BuildSideWhy)
	}
	// Anti-vacuity: the same shape as an INNER join is not swapped either, so the reason
	// above has to be the cost model rather than a blanket refusal of LEFT joins. What
	// says the refusal is not blanket is that the costs were compared at all.
	if st.BuildSideCost == 0 || st.BuildSideSwapCost == 0 {
		t.Fatalf("the two orientations were not priced, so the plan was not chosen on "+
			"cost: %s", st.BuildSideWhy)
	}
	if st.BuildSideCost >= st.BuildSideSwapCost {
		t.Fatalf("as written priced at %d and swapped at %d, so the index was not what "+
			"decided it", st.BuildSideCost, st.BuildSideSwapCost)
	}
}

// The other half of the same rule, from the planner's side. A LEFT join whose PRESERVED
// side is the large indexed one must stay as written: exchanging it would hash eight
// thousand rows instead of thirty-two AND give up the probe, so a cost model that credited
// the swap with a narrowing it will not get would choose the worse plan twice over.
func TestLeftJoinIsNotExchangedForAProbeItCannotMake(t *testing.T) {
	cat := leftProbeFixture(t, 8192, 32)
	spec := leftProbeSpec()
	// The large indexed table is the one written first, so it is the preserved side.
	spec.Kind = JoinLeft
	spec.Left = JoinSide{Table: "hist", Alias: "h"}
	spec.Right = JoinSide{Table: "jobs", Alias: "j"}

	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range seq {
		rows++
	}
	if st.Err != nil {
		t.Fatal(st.Err)
	}
	if rows != 8192 {
		t.Fatalf("the join produced %d rows, expected one per preserved row", rows)
	}
	if st.BuildSideSwapped {
		t.Fatalf("the sides were exchanged, hashing the large preserved side for a "+
			"narrowing it may not have: %s", st.BuildSideWhy)
	}
	if st.HashRows != 32 {
		t.Fatalf("the join held %d rows, expected the small side's 32", st.HashRows)
	}
}

// The refusal itself, asked of the gate directly. A swapped LEFT join reaches it only
// through a cost model that has already been told not to price the probe, so the gate is
// the last thing standing between a preserved side and a membership constraint that would
// silently drop its unmatched rows.
func TestProbeGateRefusesToNarrowAPreservedSide(t *testing.T) {
	cat := leftProbeFixture(t, 8192, 32)
	base := leftProbeSpec()
	sides, _, err := base.chain()
	if err != nil {
		t.Fatal(err)
	}

	eligible := func(kind JoinKind) (string, bool) {
		spec := base
		spec.Kind = kind
		p, cerr := compileJoin(spec, DefaultJoinLimits())
		if cerr != nil {
			t.Fatal(cerr)
		}
		attrs, _, _, _, why := cat.probeGateAll(p.steps[0], sides[1])
		return why, len(attrs) > 0
	}

	// Anti-vacuity: the same side, same equalities, same indexes -- the gate lets this one
	// through, so what stops the other is the kind and nothing else.
	if why, ok := eligible(JoinInner); !ok {
		t.Fatalf("the gate declined the inner join too, so the refusal below proves "+
			"nothing: %s", why)
	}
	why, ok := eligible(joinRightOuter)
	if ok {
		t.Fatal("the gate offered to narrow the preserved side of a swapped LEFT join")
	}
	if !strings.Contains(why, "preserved") {
		t.Fatalf("the gate declined for some other reason: %s", why)
	}
}

// The parallel walk shares one hash across P goroutines, so which rows matched is the only
// state a walker writes that is not its own. Walked both ways, the answer must be the same.
func TestLeftJoinSwapWalkedInParallelAnswersAsTheSerialWalkDoes(t *testing.T) {
	cat := leftOuterFixture(t, 400, 20000)
	spec := leftOuterSpec()

	before := joinParallelWalks.Load()
	par, pst := joinRows(t, cat, spec, JoinLimits{})
	if joinParallelWalks.Load() == before {
		t.Skip("this machine has no budget for a parallel walk, so there is nothing to compare")
	}
	if !pst.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", pst.BuildSideWhy)
	}

	var ser []string
	withSerialJoinWalk(t, func() {
		var sst JoinStats
		ser, sst = joinRows(t, cat, spec, JoinLimits{})
		if !sst.BuildSideSwapped {
			t.Fatalf("the serial arm did not exchange the sides: %s", sst.BuildSideWhy)
		}
	})
	if n := nullExtended(par, "f"); n == 0 {
		t.Fatal("no row came back null-extended, so the tail pass was never exercised")
	}
	requireSameRows(t, "parallel against serial", par, ser)
}

// A limit stops the join part way through, and a half-walked hash has not finished deciding
// what matched -- so the leftovers must NOT be emitted. A plan that emitted them anyway
// would hand back rows as unmatched that the rest of the stream was about to match.
//
// The count is the assertion. Each fact matches at most one key in this fixture, so a scan
// row produces at most one row out and a limit of five is five rows EXACTLY -- which makes
// a leftover emitted after the stop show up as a sixth row rather than as a row that might
// or might not have been genuine depending on which bucket the map handed over first.
//
// Both walks, because the two decide to stop in different places: the serial one in its own
// scan loop, the parallel one in the drainer that owns the row limit.
func TestLeftJoinSwapUnderALimitDoesNotEmitTheLeftovers(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()

	full, _ := joinRows(t, cat, spec, JoinLimits{})
	whole := map[string]bool{}
	for _, r := range full {
		whole[r] = true
	}
	limited := spec
	limited.Limit = 5

	check := func(t *testing.T, rows []string, st JoinStats) {
		t.Helper()
		if !st.BuildSideSwapped {
			t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
		}
		if len(rows) != 5 {
			t.Fatalf("a limit of 5 returned %d rows", len(rows))
		}
		for _, r := range rows {
			if !whole[r] {
				t.Fatalf("the limited join returned a row the unlimited one did not:\n%s", r)
			}
		}
	}

	t.Run("walked in parallel", func(t *testing.T) {
		before := joinParallelWalks.Load()
		rows, st := joinRows(t, cat, limited, JoinLimits{})
		if joinParallelWalks.Load() == before {
			t.Skip("this machine has no budget for a parallel walk")
		}
		check(t, rows, st)
	})
	t.Run("walked serially", func(t *testing.T) {
		withSerialJoinWalk(t, func() {
			rows, st := joinRows(t, cat, limited, JoinLimits{})
			check(t, rows, st)
		})
	})
}

// A consumer that stops reading stops the join, and the leftovers are not owed to a caller
// that is no longer listening. This is the case a LIMIT does not cover: the tail checks the
// limit itself, so a join that stopped at one would decline the tail anyway, while a join
// that stopped because the range loop broke has nothing in stats to decline on.
//
// Yielding after the consumer has gone is also not merely extra rows: range-over-func panics
// on it. So this fails loudly either way.
func TestLeftJoinSwapStopsWhenTheConsumerDoes(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	run := func(t *testing.T) {
		t.Helper()
		var st JoinStats
		seq, err := cat.Join(leftOuterSpec(), JoinLimits{}, &st)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for range seq {
			n++
			if n == 3 {
				break
			}
		}
		if !st.BuildSideSwapped {
			t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
		}
		if n != 3 {
			t.Fatalf("the consumer took %d rows after asking for 3", n)
		}
	}
	t.Run("walked in parallel", func(t *testing.T) {
		before := joinParallelWalks.Load()
		run(t)
		if joinParallelWalks.Load() == before {
			t.Skip("this machine has no budget for a parallel walk")
		}
	})
	t.Run("walked serially", func(t *testing.T) { withSerialJoinWalk(t, func() { run(t) }) })
}

// The pair budget stops the walk from the inside, part way through a side it has not
// finished probing -- so the rows still in the hash are not unmatched, they are undecided.
// Emitting them would turn a join that ran out of budget into one that answers with rows
// that are not in its result at all.
func TestLeftJoinSwapDoesNotEmitLeftoversAfterThePairBudget(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()

	full, _ := joinRows(t, cat, spec, JoinLimits{})
	whole := map[string]bool{}
	for _, r := range full {
		whole[r] = true
	}

	run := func(t *testing.T) {
		t.Helper()
		rows, st := joinRows(t, cat, spec, JoinLimits{MaxPairs: 10})
		if !st.BuildSideSwapped {
			t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
		}
		if st.Err == nil || !strings.Contains(st.Err.Error(), "candidate pairs") {
			t.Fatalf("the pair budget did not stop the join, so this proves nothing: %v", st.Err)
		}
		for _, r := range rows {
			if !whole[r] {
				t.Fatalf("after running out of pair budget the join returned a row that is "+
					"not in its result:\n%s", r)
			}
		}
	}
	t.Run("walked in parallel", func(t *testing.T) {
		before := joinParallelWalks.Load()
		run(t)
		if joinParallelWalks.Load() == before {
			t.Skip("this machine has no budget for a parallel walk")
		}
	})
	t.Run("walked serially", func(t *testing.T) { withSerialJoinWalk(t, func() { run(t) }) })
}

// The anti-join idiom -- a LEFT join whose WHERE keeps only the rows that matched nothing --
// is the one shape where a preserved row wrongly counted as unmatched is not filtered away
// again: `isUndefined(f.ClusterId)` is SATISFIED by the null-extension. It is therefore the
// question this plan has to answer exactly right, and the one an adjacent test passes
// without.
func TestLeftJoinSwapAnswersTheAntiJoin(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()
	spec.Post = []string{"isUndefined(f.ClusterId)"}

	rows, st := joinRows(t, cat, spec, JoinLimits{})
	if !st.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
	}
	// The 10 keys whose ClusterId is in no fact, and the 10 with no ClusterId at all.
	if len(rows) != 20 {
		t.Fatalf("the anti-join returned %d rows, expected the 20 keys that matched "+
			"nothing", len(rows))
	}
	if n := nullExtended(rows, "f"); n != len(rows) {
		t.Fatalf("%d of %d anti-join rows carried a matched side", len(rows)-n, len(rows))
	}
	asWritten, _ := unswappedRows(t, cat, spec, JoinLimits{})
	requireSameRows(t, "anti-join", rows, asWritten)
}

// A grouped aggregate gives up row order by construction, so it is planned with the freedom
// this change hands the planner -- and its count has to include the preserved rows the tail
// pass produces, which are the ones a plan that forgot them would silently leave out.
func TestLeftJoinSwapAggregatesTheUnmatchedRowsToo(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()

	var st JoinStats
	rows, err := cat.JoinAggregate(JoinAggSpec{
		Join: spec,
		Aggs: []AggSpec{{Func: AggCount, Arg: "*"}},
	}, JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
	}
	if len(rows) != 1 {
		t.Fatalf("a scalar aggregate returned %d rows", len(rows))
	}
	if got := rows[0].Values[0]; got != "80" {
		t.Fatalf("the aggregate counted %s joined rows, expected 80 -- the 60 pairs and the "+
			"20 preserved rows that matched nothing", got)
	}
}

// The wire form splices each side's bytes under its alias, and a null-extended row produced
// by the tail pass has no bytes for the STREAMED side -- which is side 0, the one the splice
// caches per scan row. Decoding what it produced is what says the cache was not reused.
func TestLeftJoinSwapRendersTheTailRowsToWire(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	spec := leftOuterSpec()
	spec.Left.Project = []string{"ClusterId", "Owner"}
	spec.Right.Project = []string{"ClusterId", "ExitCode"}

	ads, st := joinRows(t, cat, spec, JoinLimits{})
	if !st.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged: %s", st.BuildSideWhy)
	}
	if n := nullExtended(ads, "f"); n != 20 {
		t.Fatalf("%d rows came back null-extended, expected 20", n)
	}

	var wst JoinStats
	seq, err := cat.JoinWire(spec, JoinLimits{}, false, &wst)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for row := range seq {
		ad, derr := wire.DecodeInline(row)
		if derr != nil {
			t.Fatalf("a wire row did not decode: %v", derr)
		}
		got = append(got, classad.FromAST(ad).String())
	}
	if wst.Err != nil {
		t.Fatal(wst.Err)
	}
	sort.Strings(got)
	requireSameRows(t, "wire against ads", got, ads)
}

// ExplainJoin reports the exchange for what it is: the caller's LEFT join read from the
// other end, which is a step kind of "right" and a plan that says it reordered.
func TestExplainJoinNamesAnExchangedLeftJoin(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	ex, err := cat.ExplainJoin(leftOuterSpec(), JoinLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !ex.Reordered {
		t.Fatalf("the plan did not report a reordering: %s", ex.OrderWhy)
	}
	if len(ex.Steps) != 1 || ex.Steps[0].Kind != "right" {
		t.Fatalf("the step reports kind %q, expected \"right\"", ex.Steps[0].Kind)
	}
	if ex.Sides[0].Alias != "f" {
		t.Fatalf("the streamed side is %q, expected the side written second", ex.Sides[0].Alias)
	}
	if !strings.Contains(ex.Steps[0].ProbeWhy, "preserved") {
		t.Fatalf("the plan does not say why the preserved side may not be narrowed: %q",
			ex.Steps[0].ProbeWhy)
	}
}

// joinRightOuter means something different from the kinds a caller may write, and a spec
// arriving with it would be executed as a plan nobody asked for. Every entry point refuses
// a kind it does not recognise.
func TestJoinRefusesAKindNoCallerMayWrite(t *testing.T) {
	cat := leftOuterFixture(t, 50, 5000)
	for _, tc := range []struct {
		name string
		spec JoinSpec
	}{
		{"the internal right-outer kind", func() JoinSpec {
			s := leftOuterSpec()
			s.Kind = joinRightOuter
			return s
		}()},
		{"a kind that is nothing at all", func() JoinSpec {
			s := leftOuterSpec()
			s.Kind = JoinKind(200)
			return s
		}()},
		{"a chain step's kind", JoinSpec{
			Left: JoinSide{Table: "keys", Alias: "k"},
			Steps: []JoinStep{{
				Kind: joinRightOuter,
				Side: JoinSide{Table: "facts", Alias: "f"},
				Cond: []string{"k.ClusterId == f.ClusterId"},
				Equi: []JoinEqui{{Left: "k.ClusterId", Right: "ClusterId"}},
			}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := cat.Join(tc.spec, JoinLimits{}, nil); err == nil {
				t.Fatal("Join accepted it")
			}
			if _, err := cat.ExplainJoin(tc.spec, JoinLimits{}); err == nil {
				t.Fatal("ExplainJoin accepted it")
			}
			if _, err := cat.JoinWire(tc.spec, JoinLimits{}, false, nil); err == nil {
				t.Fatal("JoinWire accepted it")
			}
		})
	}
}

// leftProbeFixture is the shape the probe is for -- a small table against a large INDEXED
// one -- in archives that have given up newest-first, so the only thing left to stop the
// planner exchanging the sides is the cost of doing so.
func leftProbeFixture(tb testing.TB, big, small int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	cfg := ArchiveConfig{SegmentSize: 4096, Unordered: true, ValueAttrs: []string{"ClusterId"}}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	hist, jobs := mk("hist"), mk("jobs")
	for i := 0; i < big; i++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Pad", strings.Repeat("x", 64))
		if aerr := hist.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	stride := big / small
	for k := 0; k < small; k++ {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(k*stride))
		ad.InsertAttrString("Owner", "alice")
		if aerr := jobs.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return cat
}

func leftProbeSpec() JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: "jobs", Alias: "j"},
		Right: JoinSide{Table: "hist", Alias: "h"},
		Cond:  []string{"j.ClusterId == h.ClusterId"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}
