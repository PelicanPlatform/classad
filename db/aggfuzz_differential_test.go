package db

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/PelicanPlatform/classad/aggfuzz/gen"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// Differential fuzzing across the aggregate STRATEGIES, as opposed to the fuzz/ harness's axis
// (this engine against libclassad). Same process, different oracle.
//
// One grouped aggregate over an archive can be answered by several unrelated pieces of code:
// aggregateFromIndex reads per-segment postings and never touches a record; the constrained
// COUNT(*) fast path reads columnar blocks; GroupedFromColumns reads the group column and the
// aggregated columns out of those blocks; ColumnarAggregate reads one numeric column; and the
// fall-through projects and scans records. They must all produce identical answers, and nothing
// systematic checked that they did -- which is how a bucketed GROUP BY on a string column, a
// combination neither of the two changes that created it had a test for, shipped a plausible wrong
// answer. It found that one and a second (MIN/MAX over a column holding NaN, where the columnar
// accumulator reported its +-Inf seed as data); both are fixed, and this harness carries no
// suppressions.
//
// The record scan is the reference. refScan below is the dispatcher's own fall-through, assembled
// from the same exported pieces in the same order, so "the fast path disagrees with the scan" is
// exactly what this fails on.
//
// DECLINING is correct behaviour, not a divergence, and three paths do it deliberately: a boolean
// in a column, a NaN under MIN/MAX (no rule a column pass could follow matches the reference's
// order-dependent answer), and a CurrentTime-folded predicate in a segment not proven free of
// CurrentTime. Each shows up here as the scan tier, which is why the tier histogram is a report of
// what ran and not a target to be maximised.
//
// Two coverage problems are load-bearing, and both are assertions rather than reports:
//
//  1. Random data answers almost everything by scan. gen steers the data (see its package comment)
//     and the fixture asserts the archive came out accelerated at all.
//  2. A run in which some tier never fired is a run that tested nothing about that tier.
//     TestAggregateStrategyTierCoverage fails when any tier -- or any of the three strategies
//     TierColumnar merges -- is missing, and logs the histogram either way.

// The tiers the dispatcher reports, and the sub-strategies a tier merges.
//
// collections.AggTier is the ANSWER to "which strategy ran": the archive fan-out tags every return
// with it and it rides on the wire, so the fuzzer reads it rather than re-deriving it. An earlier
// version of this file inferred the tier from a zeroed scan trailer and then re-asked the
// dispatcher's own gates, in the dispatcher's order, to say which fast path it had been. That
// duplicated the order, and the order has since changed -- the fan-out is seven rungs now and the
// index rung appears TWICE, before the constrained count when the constraint is match-all and after
// the grouped-columns rung when it is not -- so the replication would now be silently wrong. The
// reported field removes the need for it entirely.
//
// What the reported field does NOT give is which of the three columnar strategies ran: a
// constrained COUNT(*) over the blocks, a grouped read of the blocks, and an ungrouped single-column
// aggregate all report TierColumnar, because that is the distinction the wire format is about
// (two orders of magnitude of work), not the one a differential test is about. So each of the three
// is ALSO asked directly, below, and compared against the scan on its own -- which is stronger than
// attributing the dispatcher's choice anyway, since it covers a path that was eligible but never
// reached because an earlier rung took the query. The probe counts are asserted the same way the
// tier counts are: a run where one never fired tested nothing about it.

// numTiers bounds the histogram. collections pins the tier values permanently (they are a wire
// format), so indexing by them is safe; a value past the end would be a tier added upstream without
// this test noticing, which checkQuery reports rather than panicking on.
const numTiers = int(collections.TierScan) + 1

// probe names one directly-asked strategy. These are not tiers: all three answer as TierColumnar.
type probe int

const (
	probeCountConstraint probe = iota // the ungrouped constrained COUNT(*) over columnar blocks
	probeGroupColumns                 // GroupedFromColumns: a grouped read of the columnar blocks
	probeColumnar                     // ColumnarAggregate: one numeric column, ungrouped
	numProbes
)

var probeNames = [numProbes]string{"count-constraint", "grouped-columns", "columnar"}

func (p probe) String() string { return probeNames[p] }

// probeSet is which strategies answered one query when asked directly. Returned from checkQuery
// rather than counted there, so the shrinker's rebuilds -- which also call checkQuery -- do not
// inflate the coverage histogram the sweep asserts on.
type probeSet uint8

func (ps probeSet) has(p probe) bool { return ps&(1<<p) != 0 }

// countConstraintDirect asks the constrained COUNT(*) fast path for its own answer, for the query
// shape it serves: a lone unfiltered COUNT(*) with no grouping. This is the one columnar strategy
// whose result is not already in hand from a direct call, and asking it here is what keeps it
// covered for the queries where an earlier rung answers first.
func (f *aggFixture) countConstraintDirect(constraint string, groups []GroupCol, aggs []AggSpec) (int, bool) {
	if len(groups) != 0 || len(aggs) != 1 || aggs[0].Func != AggCount || aggs[0].Arg != "*" || aggs[0].Filter != "" {
		return 0, false
	}
	return f.tbl.CountConstraint(constraint)
}

// refScan is the oracle: the dispatcher's own fall-through, reassembled from the exported pieces in
// the same order, with every fast path bypassed. An empty constraint stands in as "true" because
// the query parser rejects the empty string -- AggregateColsStats only reaches the scan with an
// empty constraint when every fast path has already declined, and then it returns that parse error.
func (f *aggFixture) refScan(constraint string, groups []GroupCol, aggs []AggSpec) ([]AggRow, error) {
	if strings.TrimSpace(constraint) == "" {
		constraint = "true"
	}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	seq, err := f.tbl.QueryProjectStats(constraint, attrs, nil)
	if err != nil {
		return nil, err
	}
	return AggregateValues(seq, attrs, groups, aggs, groupCol, aggCol, nil)
}

// --- tolerance -----------------------------------------------------------------------------

// aggTol is one group's float-summation error budget for one aggregate: how many numeric values
// contributed and the sum of their magnitudes.
//
// Float addition is not associative, so a REAL-valued SUM or AVG may differ in the last places
// between a column-order accumulation and a scan-order one. That is the one permitted difference,
// and it is encoded as the textbook bound for n sequential additions -- (n-1)*eps*sum|x| -- rather
// than as a relative epsilon, because a relative epsilon is wrong in both directions: it is far too
// loose for a well-conditioned sum (hiding a real wrong answer) and too tight for a cancelling one
// (a sum of mixed signs can lose every significant digit while every addition was correctly
// rounded). anyReal decides whether a SUM gets the budget -- a sum with no real in it is
// accumulated in int64 on both sides and must match bit for bit -- while an AVG always gets it,
// because it divides a float sum however the column was typed.
type aggTol struct {
	n       int
	sumAbs  float64
	anyReal bool
}

// scanTols re-runs the reference scan to measure, per rendered group and per aggregate, the inputs
// the error bound needs. It groups by the same key AggregateValues does -- deliberately calling the
// same bucketKeyText/ValueText -- so the budgets line up with the rows being compared.
func (f *aggFixture) scanTols(constraint string, groups []GroupCol, aggs []AggSpec) (map[string][]aggTol, error) {
	// The budget only ever applies to a real-valued SUM or AVG, and a filtered aggregate is
	// declined by every fast path (so the comparison is scan against scan and exact). Skipping
	// both cases skips this extra scan for most queries.
	need := false
	for _, a := range aggs {
		if a.Filter != "" {
			return nil, nil
		}
		need = need || a.Func == AggSum || a.Func == AggAvg
	}
	if !need {
		return nil, nil
	}
	if strings.TrimSpace(constraint) == "" {
		constraint = "true"
	}
	attrs, groupCol, aggCol := AggProjection(groups, aggs)
	seq, err := f.tbl.QueryProjectStats(constraint, attrs, nil)
	if err != nil {
		return nil, err
	}
	out := map[string][]aggTol{}
	scratch := make([]string, len(groups))
	for vals := range seq {
		drop := false
		for i, g := range groups {
			if g.BucketWidth > 0 {
				kt, ok := bucketKeyText(vals[groupCol[i]], g.BucketWidth)
				if !ok {
					drop = true
					break
				}
				scratch[i] = kt
			} else {
				scratch[i] = ValueText(vals[groupCol[i]])
			}
		}
		if drop {
			continue
		}
		key := strings.Join(scratch, "\x00")
		ts := out[key]
		if ts == nil {
			ts = make([]aggTol, len(aggs))
			out[key] = ts
		}
		for i := range aggs {
			if aggCol[i] < 0 {
				continue
			}
			v := vals[aggCol[i]]
			x, ok := aggNumberOf(v)
			if !ok {
				continue
			}
			ts[i].n++
			ts[i].sumAbs += math.Abs(x)
			ts[i].anyReal = ts[i].anyReal || v.IsReal()
		}
	}
	return out, nil
}

// aggNumberOf is the aggregate's view of a value: the numeric contributions SUM/AVG accumulate.
// It is numberOf plus booleans, which the reference coerces to 1/0 -- a column holding one declines
// every fast path, but the scan still sums it and the budget has to count it.
func aggNumberOf(v classad.Value) (float64, bool) {
	switch {
	case v.IsInteger():
		i, _ := v.IntValue()
		return float64(i), true
	case v.IsReal():
		r, _ := v.RealValue()
		return r, true
	case v.IsBool():
		b, _ := v.BoolValue()
		if b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

const float64Eps = 1.0 / (1 << 53) // 2^-53, the unit roundoff of a correctly-rounded addition

// sameAggValue compares one rendered aggregate value against the reference's.
//
// Exact, except for a SUM or AVG that went through float addition, which gets the summation error
// bound twice over (each side carries up to one such error) plus, for AVG, one more rounding from
// the division.
//
// Which ones those are is not a judgement call. A SUM with no real in it accumulates in int64 on
// both sides -- identically, overflow included -- so it is compared bit for bit. An AVG always
// divides a FLOAT sum, even over an all-integer column, so it always gets the budget; that is not
// a detail, it is where this rule was first wrong, and an AVG over a column of values near 2^63 is
// exactly where a column-order and a scan-order sum part company.
func sameAggValue(got, want string, fn AggFunc, tol aggTol) bool {
	if got == want {
		return true
	}
	if fn != AggSum && fn != AggAvg || tol.n < 2 {
		return false // COUNT and MIN/MAX pick a value, they do not accumulate one: bit-for-bit
	}
	if fn == AggSum && !tol.anyReal {
		return false // an all-integer SUM accumulates in int64 on both sides, overflow included
	}
	g, errG := strconv.ParseFloat(got, 64)
	w, errW := strconv.ParseFloat(want, 64)
	if errG != nil || errW != nil || !isFinite(g) || !isFinite(w) || !isFinite(tol.sumAbs) {
		return false
	}
	budget := 2 * float64(tol.n-1) * float64Eps * tol.sumAbs
	if fn == AggAvg {
		budget = budget/float64(tol.n) + ulp(math.Max(math.Abs(g), math.Abs(w)))
	}
	return math.Abs(g-w) <= budget
}

func isFinite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }

// ulp is the distance from |x| to the next representable float above it.
func ulp(x float64) float64 {
	x = math.Abs(x)
	return math.Nextafter(x, math.Inf(1)) - x
}

// --- comparison ----------------------------------------------------------------------------

// compareAgg returns the ways got differs from the reference, as a set of groups. Aggregate row
// order is undefined (the index tier returns sorted, the scan first-seen), so only the set and the
// per-group values are compared -- but a DUPLICATE group key is itself a finding, since it means a
// strategy emitted one logical group as two rows.
func compareAgg(got, want []AggRow, aggs []AggSpec, tols map[string][]aggTol) []string {
	index := func(rows []AggRow, side string) (map[string][]string, []string) {
		m := make(map[string][]string, len(rows))
		var dups []string
		for _, r := range rows {
			k := strings.Join(r.Group, "\x00")
			if _, seen := m[k]; seen {
				dups = append(dups, fmt.Sprintf("%s emitted group %q twice", side, k))
				continue
			}
			m[k] = r.Values
		}
		return m, dups
	}
	g, out := index(got, "got")
	w, dw := index(want, "scan")
	out = append(out, dw...)
	for k, wv := range w {
		gv, ok := g[k]
		if !ok {
			out = append(out, fmt.Sprintf("group %q missing (scan has %v)", k, wv))
			continue
		}
		if len(gv) != len(wv) {
			out = append(out, fmt.Sprintf("group %q has %d values, scan has %d", k, len(gv), len(wv)))
			continue
		}
		for i := range wv {
			if !sameAggValue(gv[i], wv[i], aggs[i].Func, tolAt(tols, k, i)) {
				out = append(out, fmt.Sprintf("group %q %s(%s) = %s, scan = %s",
					k, aggFuncName(aggs[i].Func), aggs[i].Arg, gv[i], wv[i]))
			}
		}
	}
	for k, gv := range g {
		if _, ok := w[k]; !ok {
			out = append(out, fmt.Sprintf("group %q = %v is not in the scan's result", k, gv))
		}
	}
	sort.Strings(out)
	return out
}

// tolAt returns the error budget for one group's one aggregate, or the zero budget (meaning
// "compare exactly") when none was measured -- which is the case for a filtered aggregate, where
// every fast path declines and the comparison is scan against scan.
func tolAt(tols map[string][]aggTol, key string, i int) aggTol {
	ts := tols[key]
	if i >= len(ts) {
		return aggTol{}
	}
	return ts[i]
}

// --- the check -----------------------------------------------------------------------------

// toDB maps a generated query onto the db types. Written out rather than sharing integer values, so
// a reordering of either enum is a compile error instead of a silently different aggregate.
func toDB(q gen.Query) ([]GroupCol, []AggSpec) {
	groups := make([]GroupCol, len(q.Groups))
	for i, g := range q.Groups {
		groups[i] = GroupCol{Attr: g.Attr, BucketWidth: g.BucketWidth}
	}
	aggs := make([]AggSpec, len(q.Aggs))
	for i, a := range q.Aggs {
		var fn AggFunc
		switch a.Func {
		case gen.FuncCount:
			fn = AggCount
		case gen.FuncSum:
			fn = AggSum
		case gen.FuncAvg:
			fn = AggAvg
		case gen.FuncMin:
			fn = AggMin
		case gen.FuncMax:
			fn = AggMax
		case gen.FuncCountDistinct:
			fn = AggCountDistinct
		}
		aggs[i] = AggSpec{Func: fn, Arg: a.Arg, Filter: a.Filter}
	}
	return groups, aggs
}

// checkQuery runs one generated query every way the archive can answer it and compares each against
// the record scan. It returns the tier the dispatcher REPORTED, which of the three columnar
// strategies answered when asked directly, and the disagreements found (empty when they all agree),
// so both the fuzz target and the shrinker can use it.
//
// It checks more than the dispatcher's own choice. All three columnar strategies are asked
// DIRECTLY as well, because the fan-out stops at the first rung that answers: a columnar path that
// is eligible but never reached would otherwise go unverified for exactly the queries where two
// strategies overlap, which is where they have the most to disagree about.
func checkQuery(f *aggFixture, q gen.Query) (collections.AggTier, probeSet, []string, error) {
	groups, aggs := toDB(q)
	want, err := f.refScan(q.Constraint, groups, aggs)
	if err != nil {
		// The reference itself cannot run (a COUNT(DISTINCT *), a filter that does not parse).
		// Then the dispatcher must fail too, rather than answer from a fast path that skipped the
		// validation the scan performs.
		if _, derr := f.tbl.AggregateCols(q.Constraint, groups, aggs); derr == nil {
			return collections.TierUnreported, 0, []string{fmt.Sprintf(
				"the scan rejects this query (%v) but the dispatcher answered it", err)}, nil
		}
		return collections.TierScan, 0, nil, err
	}
	tols, err := f.scanTols(q.Constraint, groups, aggs)
	if err != nil {
		return collections.TierScan, 0, nil, err
	}

	// The three columnar strategies, asked directly (see the doc comment). All three report
	// TierColumnar when the dispatcher picks them, so this is also the only thing that tells them
	// apart for coverage.
	var ps probeSet
	groupedRows, groupedOK := GroupedFromColumns(f.tbl, q.Constraint, groups, aggs)
	columnarRows, columnarOK := ColumnarAggregate(func(attr string) (NumStats, bool) {
		return f.tbl.NumStats(q.Constraint, attr)
	}, groups, aggs)
	countN, countOK := f.countConstraintDirect(q.Constraint, groups, aggs)

	var stats collections.ScanStats
	got, err := f.tbl.AggregateColsStats(q.Constraint, groups, aggs, &stats)
	if err != nil {
		// An EMPTY constraint is match-all to every fast path and a syntax error to the scan, so a
		// shape no fast path serves fails to aggregate at all. Known, pre-existing, and not a
		// disagreement between strategies -- counted rather than reported, so the count makes it
		// visible without drowning the signal this harness exists for.
		if strings.TrimSpace(q.Constraint) == "" {
			emptyConstraintRefusals.Add(1)
			return collections.TierScan, 0, nil, err
		}
		return collections.TierScan, 0, []string{fmt.Sprintf("dispatcher errored where the scan answered: %v", err)}, nil
	}
	tier := stats.Tier
	var diffs []string
	switch {
	case tier == collections.TierUnreported:
		// Every return in the fan-out tags itself, so an untagged answer means a return was added
		// without one -- and an untagged answer is indistinguishable from an old peer's, which is
		// exactly what the field exists to prevent.
		diffs = append(diffs, "the dispatcher answered without reporting a tier (TierUnreported); "+
			"a return in the archive aggregate fan-out is missing its stats.SetTier")
	case int(tier) >= numTiers:
		// A tier added upstream that this test does not know about: report it rather than index
		// the histogram out of range.
		diffs = append(diffs, fmt.Sprintf("the dispatcher reported %v, which this harness does not "+
			"know; add it to the tier histogram", tier))
	}
	diffs = append(diffs, prefix(tier.String(), compareAgg(got, want, aggs, tols))...)
	if groupedOK {
		ps |= 1 << probeGroupColumns
		diffs = append(diffs, prefix("grouped-columns(direct)", compareAgg(groupedRows, want, aggs, tols))...)
	}
	if columnarOK {
		ps |= 1 << probeColumnar
		diffs = append(diffs, prefix("columnar(direct)", compareAgg(columnarRows, want, aggs, tols))...)
	}
	if countOK {
		ps |= 1 << probeCountConstraint
		// One ungrouped row with one value; the reference always renders exactly that for this
		// shape, so a mismatch in the row count is itself worth saying out loud.
		switch {
		case len(want) != 1 || len(want[0].Values) != 1:
			diffs = append(diffs, fmt.Sprintf("count-constraint(direct): the scan rendered %d rows "+
				"for an ungrouped COUNT(*)", len(want)))
		case strconv.Itoa(countN) != want[0].Values[0]:
			diffs = append(diffs, fmt.Sprintf("count-constraint(direct): COUNT(*) = %d, scan = %s",
				countN, want[0].Values[0]))
		}
	}
	return tier, ps, diffs, nil
}

// aggFuncName labels an AggFunc for a message. AggFunc has no String method, and a bare integer in
// a reproducer is one more thing to look up.
func aggFuncName(f AggFunc) string {
	switch f {
	case AggCount:
		return "COUNT"
	case AggSum:
		return "SUM"
	case AggAvg:
		return "AVG"
	case AggMin:
		return "MIN"
	case AggMax:
		return "MAX"
	case AggCountDistinct:
		return "COUNT_DISTINCT"
	}
	return "?"
}

func prefix(tag string, in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = tag + ": " + s
	}
	return out
}

// --- the fuzz target -----------------------------------------------------------------------

// emptyConstraintRefusals counts the queries an empty constraint made unanswerable (see checkQuery).
var emptyConstraintRefusals atomic.Int64

// tierHist counts how often each REPORTED tier answered; probeHist how often each of the three
// strategies TierColumnar merges answered when asked directly. Package-level and atomic because
// the fuzz engine and the parallel sweep both feed them.
var (
	tierHist  [numTiers]atomic.Int64
	probeHist [numProbes]atomic.Int64
)

// record adds one query's outcome to both histograms. A tier past the end is dropped here rather
// than panicking; checkQuery has already reported it as a finding.
func record(tier collections.AggTier, ps probeSet) {
	if int(tier) < numTiers {
		tierHist[tier].Add(1)
	}
	for p := probe(0); p < numProbes; p++ {
		if ps.has(p) {
			probeHist[p].Add(1)
		}
	}
}

type histSnapshot struct {
	tiers  [numTiers]int64
	probes [numProbes]int64
}

func histSnapshotNow() (out histSnapshot) {
	for i := range out.tiers {
		out.tiers[i] = tierHist[i].Load()
	}
	for i := range out.probes {
		out.probes[i] = probeHist[i].Load()
	}
	return
}

func (h histSnapshot) empty() bool {
	for _, n := range h.tiers {
		if n > 0 {
			return false
		}
	}
	return true
}

func (h histSnapshot) String() string {
	var parts []string
	var total int64
	for i, n := range h.tiers {
		total += n
		parts = append(parts, fmt.Sprintf("%s=%d", collections.AggTier(i), n))
	}
	var probes []string
	for i, n := range h.probes {
		probes = append(probes, fmt.Sprintf("%s=%d", probe(i), n))
	}
	return fmt.Sprintf("%s total=%d | direct probes: %s | empty-constraint-refusals=%d",
		strings.Join(parts, " "), total, strings.Join(probes, " "), emptyConstraintRefusals.Load())
}

// FuzzAggregateStrategies is the coverage-guided target. The first input byte selects one of gen's
// archive shapes; the rest drives the query generator. Mutating the first byte moves between
// archives, mutating the rest explores queries against one -- and because an archive is expensive
// to build and a query is not, the shapes are built once per worker and cached.
func FuzzAggregateStrategies(f *testing.F) {
	cache := newFixtureCache(f)
	shapes := gen.Shapes()
	for k := 0; k < 4*len(shapes); k++ {
		f.Add(append([]byte{byte(k % len(shapes))}, gen.Bytes(k)...))
	}
	// A fuzz WORKER runs the target but not the seed corpus, so its histogram is partial and often
	// empty; the authoritative one is TestAggregateStrategyTierCoverage's. Logged here only when
	// this process actually answered something, to keep a worker's run from printing a row of
	// zeroes that reads like a coverage failure.
	f.Cleanup(func() {
		if h := histSnapshotNow(); !h.empty() {
			f.Logf("aggregate tier histogram (this process only): %s", h)
		}
	})

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) == 0 {
			in = []byte{0}
		}
		shape := gen.ShapeByID(int(in[0]))
		q := gen.QueryFrom(in[1:])
		fx := cache.get(t, shape)
		tier, ps, diffs, err := checkQuery(fx, q)
		if err != nil {
			return // a query the reference itself refuses; checkQuery already required the same of the dispatcher
		}
		record(tier, ps)
		if len(diffs) == 0 {
			return
		}
		reportDisagreement(t, fx, q, in, tier, diffs)
	})
}

// TestAggregateStrategyTierCoverage is the bounded, deterministic run: every shape against a fixed
// sweep of generated queries, every answer compared against the record scan, and -- the point of
// the test -- a FAILURE when a strategy never answered. Coverage here is an assertion, not a
// report: a run in which the index tier never fired proves nothing about the index tier, and would
// otherwise be indistinguishable from a run in which it was correct.
//
// Both halves are asserted. The reported tiers must all have fired, TierUnreported must not have
// fired at all (it means a return in the fan-out forgot to tag itself), and each of the three
// strategies TierColumnar merges must have answered at least one query when asked directly --
// which is the only coverage statement available about them, since the wire tier does not
// distinguish them.
//
// It is also the authoritative histogram. `go test -fuzz` spreads its work over worker processes,
// so a histogram accumulated inside the fuzz target is per-worker and partial.
//
// Findings are bucketed by signature rather than reported one per query, so one bug does not read
// as hundreds, and only the first gets the (expensive) shrink.
func TestAggregateStrategyTierCoverage(t *testing.T) {
	const queriesPerShape = 150
	cache := newFixtureCache(t)
	fs := &findings{seen: map[string]int{}, first: map[string]string{}}
	for _, shape := range gen.Shapes() {
		t.Run(fmt.Sprintf("shape%02d", shape.ID), func(t *testing.T) {
			fx := cache.get(t, shape)
			t.Parallel() // read-only from here: each shape has its own archive
			for k := 0; k < queriesPerShape; k++ {
				step := shape.ID*queriesPerShape + k
				q := gen.QueryAt(step)
				tier, ps, diffs, err := checkQuery(fx, q)
				if err != nil {
					continue
				}
				record(tier, ps)
				if len(diffs) > 0 {
					fs.add(t, fx, q, append([]byte{byte(shape.ID)}, gen.Bytes(step)...), tier, diffs)
				}
			}
		})
	}
	t.Cleanup(func() {
		h := histSnapshotNow()
		t.Logf("aggregate tier histogram: %s", h)
		for i, n := range h.tiers {
			tier := collections.AggTier(i)
			if tier == collections.TierUnreported {
				// The opposite assertion: this one must stay at zero. A nonzero count means the
				// dispatcher answered without saying how, which cannot be told from a peer too old
				// to report a tier -- already reported per query, counted again here.
				if n != 0 {
					t.Errorf("%d answers came back with no tier reported", n)
				}
				continue
			}
			if n == 0 {
				t.Errorf("tier %s never answered a query: the sweep proves nothing about it. "+
					"Either the generator stopped reaching it or the dispatcher stopped using it. "+
					"Histogram: %s", tier, h)
			}
		}
		for i, n := range h.probes {
			if n == 0 {
				t.Errorf("the %s strategy never answered a query when asked directly, so nothing "+
					"compared it against the scan. It reports as TierColumnar when the dispatcher "+
					"picks it, so the tier counts above cannot stand in for this. Histogram: %s",
					probe(i), h)
			}
		}
		fs.report(t)
	})
}
