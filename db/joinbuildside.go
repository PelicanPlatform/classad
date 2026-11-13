package db

// Choosing which side to hash.
//
// The join streams one side and builds a hash on the other. Which is which has always been
// syntax: the side written first streams. That is a plan, chosen without looking at the
// tables, and the cost model can now choose it.
//
// WHAT THE CHOICE IS WORTH. Both sides are READ once either way, so the records touched are
// the same unless an index can narrow one of them. Everything else pulls in one of two
// opposite directions:
//
//   - Hash the SMALL side, say memory and the hash itself. The hashed side is the one held,
//     and it is the one MaxHashRows refuses -- a join that exceeds it fails where the same
//     join the other way round would not. Building a hash also costs more per row than
//     probing one, so the orientation that holds less is the faster one too: measured at 64
//     keys against 40000 rows, 1.9x and a quarter of the bytes (BenchmarkJoinBuildSide).
//   - Stream the SMALL side, says the index probe. The probe narrows the HASHED side to the
//     key values the STREAMED side holds, so few keys against a large INDEXED table is the
//     regime where narrowing wins -- and it wins by more than holding the big side costs.
//
// So there is no rule of thumb to apply. Which way round is better turns on whether an index
// covers the join key on the larger side, which is what the cost model knows and what
// "hash the smaller side" cannot see.
//
// WHAT MAY NOT BE SWAPPED. The streamed side's order is the join's output order -- an
// archive reads newest-first and that is condor_history's order -- so swapping changes what
// the caller sees. It is allowed only where there is no order to preserve: a mutable table,
// which promises none; an archive whose config says so (ArchiveConfig.Unordered); or a
// caller that discards order by construction, which is the grouped aggregate.
//
// A LEFT JOIN MAY BE SWAPPED, and what that takes is worth stating because exchanging its
// sides is not the same operation as exchanging an inner join's. `A LEFT JOIN B` preserves
// A, and preservation is a property of the QUERY, not of which side the executor happens to
// hold -- so the swapped plan still has to produce A's unmatched rows. It does, from the
// other end: the preserved side is hashed, the stream marks the rows it joins to, and what
// is left unmarked when the stream runs out is emitted null-extended (joinRightOuter).
//
// That buys one thing and gives up another, and both are specific.
//
// It buys the HOLDING. As written, `A LEFT JOIN B` hashes B, so a hundred-row A against a
// large B holds the large side -- and MaxHashRows refuses the join outright where the same
// query the other way round would run in a few megabytes. That is the case this is for.
//
// It gives up the PROBE. The index probe narrows the hashed side to the key values the
// streamed side holds, which is precisely a filter that drops the rows that match nothing --
// the rows a LEFT join exists to keep. So a swapped LEFT join may not be probed (see
// probeGateAll), and its swapped orientation is therefore priced at a full read of the
// preserved side rather than at what an index would make it. A LEFT join can be exchanged
// to hold less; it cannot be exchanged to read less.

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// buildSideMargin is how much cheaper the swapped orientation must be before the sides are
// exchanged.
//
// Not 1.0, because the inputs are estimates. Two orientations that price within a few
// percent of each other are, as far as these numbers can tell, the same plan, and swapping
// on that would make the output order turn on estimator noise -- for a join that is allowed
// to be reordered, but still for nothing.
var buildSideMargin = 1.2

// minBuildSideRows is the size the larger side must reach before the choice is made at all.
//
// Below it both sides are small enough to hold for nothing, so there is no benefit to weigh
// -- and a real cost to trying: exchanging the sides changes which order the rows come out
// in, and on a side of four rows against one of three the decision is being made on the
// difference between four and three. minProbeRows is the same judgement for the probe.
var minBuildSideRows = 1024

// forceNoBuildSideSwap keeps the sides as written, so a test can compare the two answers and
// a benchmark the two costs in one process. It is a plain variable rather than a parameter
// because nothing outside a test may set it, and a test that does must not call t.Parallel --
// Go runs parallel tests only after the serial ones finish, which is what keeps this from
// needing synchronisation.
var forceNoBuildSideSwap bool

// planBuildSide returns spec with its sides in the order they should be read, and records
// what it decided in stats.
//
// unordered says the caller discards row order whatever happens, which is true of the
// grouped aggregate and of nothing else here.
func (cat *Catalog) planBuildSide(spec JoinSpec, limits JoinLimits, unordered bool, stats *JoinStats) JoinSpec {
	keep := func(format string, a ...any) JoinSpec {
		stats.BuildSideWhy = fmt.Sprintf(format, a...)
		return spec
	}
	switch {
	case forceNoBuildSideSwap:
		return keep("the build-side choice is disabled")
	case len(spec.Steps) > 0:
		return keep("a chain is read in the order its steps were written")
	case spec.Kind != JoinInner && spec.Kind != JoinCross && spec.Kind != JoinLeft:
		return keep("a join of kind %d does not have a second orientation to compare",
			int(spec.Kind))
	// A band is not symmetric the way an equality is. `e.Ts BETWEEN j.Lo AND j.Hi` swapped
	// is a window over neither attribute: it asks, for one value of Ts, which of two
	// attributes of the other side bracket it. swapSides exchanges the sides and flips each
	// equality's halves, and there is no flip of a band that says the same thing -- so a
	// swap here would either refuse the plan on a bound that now reads its own side, or
	// window the wrong table. Neither orientation is compared instead.
	case len(spec.Band) > 0:
		return keep("%s is windowed on %s, which is a window over that side and not a "+
			"condition between the two", spec.Right.Alias, spec.Band[0].Attr)
	case !unordered && !cat.orderFree(spec.Left):
		return keep("%s is read in a fixed order, which is the order the join's rows come "+
			"out in; set the archive's Unordered to let the planner choose", spec.Left.Alias)
	}

	// Both orientations of an inner join may narrow the side they hash. Only one of a LEFT
	// join's may: swapped, the hashed side is the PRESERVED one, and narrowing it to the
	// streamed side's keys would drop the unmatched rows the join is for. Pricing the
	// swapped orientation as though it could be probed would recommend a swap for a saving
	// the executor then refuses to make.
	probeable := spec.Kind != JoinLeft
	asWritten, ok1 := cat.orientationCost(spec.Left, spec.Right, spec.Equi, false, true, limits)
	swapped, ok2 := cat.orientationCost(spec.Right, spec.Left, spec.Equi, true, probeable, limits)
	if !ok1 || !ok2 {
		return keep("one of the two orientations could not be priced")
	}
	if max(asWritten.hashRows, swapped.hashRows) < minBuildSideRows {
		return keep("both sides are under %d rows, which is small enough to hold either way",
			minBuildSideRows)
	}
	stats.BuildSideCost, stats.BuildSideSwapCost = asWritten.total, swapped.total

	// A side that does not fit decides on its own: an orientation whose hash exceeds what
	// the join may hold does not run at all, so a slower one that runs beats it.
	switch {
	case asWritten.fits && !swapped.fits:
		return keep("hashing %s would exceed what one join may hold", spec.Left.Alias)
	case swapped.fits && !asWritten.fits:
		stats.BuildSideSwapped = true
		stats.BuildSideWhy = fmt.Sprintf("hashing %s would exceed what one join may hold, "+
			"and hashing %s does not", spec.Right.Alias, spec.Left.Alias)
		return swapSides(spec)
	}

	// Records touched decides when the two orientations differ in it, which happens when an
	// index can narrow one side's read and not the other's.
	if better(swapped.total, asWritten.total) {
		stats.BuildSideSwapped = true
		stats.BuildSideWhy = fmt.Sprintf("reading %s and hashing %s touches ~%d records "+
			"against ~%d as written", spec.Right.Alias, spec.Left.Alias, swapped.total,
			asWritten.total)
		return swapSides(spec)
	}
	if better(asWritten.total, swapped.total) {
		return keep("reading %s and hashing %s touches ~%d records against ~%d the other "+
			"way round", spec.Left.Alias, spec.Right.Alias, asWritten.total, swapped.total)
	}

	// Neither reads less. Both sides are read once whichever way round, so with no index to
	// separate them the totals are the SAME number and the choice is about what is HELD:
	// hash the smaller side. Deciding this on records alone would leave a join holding a
	// large table beside a small one it could have held instead, and report the two plans
	// as equal because by that measure they are.
	if better(swapped.hashRows, asWritten.hashRows) {
		stats.BuildSideSwapped = true
		stats.BuildSideWhy = fmt.Sprintf("neither side reads less, so the smaller is "+
			"hashed: %s holds ~%d rows against %s's ~%d", spec.Left.Alias, swapped.hashRows,
			spec.Right.Alias, asWritten.hashRows)
		return swapSides(spec)
	}
	return keep("%s is already the side hashed, and neither orientation reads less (~%d "+
		"records either way)", spec.Right.Alias, asWritten.total)
}

// better reports that a is enough below b to be worth acting on. Enough, rather than at all,
// because the inputs are estimates: two plans within a few percent of each other are the
// same plan as far as these numbers can tell, and exchanging the sides on that would make
// the output order turn on estimator noise.
func better(a, b int) bool { return float64(a)*buildSideMargin < float64(b) }

// swapSides exchanges the two sides of a two-table spec.
//
// Cond and Post are expressions over the ALIASES, so they say the same thing whichever side
// is read first and are left alone. Equi is the exception: its halves are positional, so
// each one's halves exchange with the sides.
//
// A LEFT join changes KIND as well as order, because which rows survive is written in terms
// of position: "keep every left row" means the side now read second. joinRightOuter is that
// same sentence about the side now hashed, and saying it is what keeps the swap an exchange
// of plans rather than of queries.
func swapSides(spec JoinSpec) JoinSpec {
	out := spec
	if spec.Kind == JoinLeft {
		out.Kind = joinRightOuter
	}
	out.Left, out.Right = spec.Right, spec.Left
	out.Equi = make([]JoinEqui, len(spec.Equi))
	for i, e := range spec.Equi {
		out.Equi[i] = JoinEqui{Left: e.Right, Right: e.Left}
	}
	return out
}

// orientation is what one way round costs.
type orientation struct {
	total    int  // records touched: the streamed side, plus the hashed side's read
	hashRows int  // rows the hash would hold, which is what memory and MaxHashRows see
	fits     bool // the hashed side is within what one join may hold
}

// orientationCost prices streaming stream and hashing hash.
//
// The hashed side's read is priced at what an index probe would make it, when one applies:
// that is the whole reason the orientations differ, so pricing both at a full read would
// make the decision a coin toss between equal numbers.
//
// probeable is false where the executor will refuse to narrow the hashed side whatever the
// indexes say -- which is a swapped LEFT join, whose hashed side is the preserved one. The
// price then has to be the full read, because that is what will actually be paid.
//
// limits are the ones this join will RUN under, not the store's own. They differ whenever a
// caller asked for less, and `fits` is the one place that difference changes the plan: an
// orientation is being asked whether its hash will be refused, and it is refused against the
// budget the join actually has. Pricing it against the store's default instead answered a
// question about a join that was not the one running -- so a caller who lowered MaxHashRows
// got a plan chosen for a limit it was not held to, and then the failure its own limit
// caused.
func (cat *Catalog) orientationCost(stream, hash JoinSide, equi []JoinEqui, swapped, probeable bool, limits JoinLimits) (orientation, bool) {
	streamRows, ok := cat.sideReadSize(stream)
	if !ok {
		return orientation{}, false
	}
	hashRows, ok := cat.sideReadSize(hash)
	if !ok {
		return orientation{}, false
	}
	read := hashRows
	if probeable {
		for _, attr := range equiHashAttrs(equi, swapped) {
			_, segments, indexed, _, statsOK := cat.joinSideStats(hash.Table, attr)
			if !statsOK || !indexed {
				continue
			}
			// The streamed side's ROWS bound its distinct keys, so this overstates the
			// probe and understates its benefit -- the safe direction, since the gate
			// that actually decides errs towards reading.
			keys := min(streamRows, maxProbeKeys)
			if lookups := segments * keys * probeLookupCost; lookups < read {
				read = lookups
			}
		}
	}
	return orientation{
		total:    streamRows + read,
		hashRows: hashRows,
		fits:     hashRows <= limits.MaxHashRows,
	}, true
}

// sideReadSize is how many records reading a side touches, whole table or constrained.
func (cat *Catalog) sideReadSize(side JoinSide) (int, bool) {
	total, _, _, _, ok := cat.joinSideStats(side.Table, "")
	if !ok {
		return 0, false
	}
	rows, why := cat.sideRows(side, total)
	return rows, why == ""
}

// equiHashAttrs names the attributes the HASHED side's halves read, for every equality
// whose half is a bare reference. A computed half names nothing an index could be probed
// on and is skipped; a join with none is priced at a full read.
//
// Every equality, not just one, because a probe narrows on all of them (joinprobe.go) and
// pricing has to see the same plan the probe would build. Pricing only single-equality
// joins said a composite key could never be narrowed, which was true until it wasn't, and
// would have left the cost model recommending against the orientation that now wins.
func equiHashAttrs(equi []JoinEqui, swapped bool) []string {
	out := make([]string, 0, len(equi))
	for _, e := range equi {
		src := e.Right
		if swapped {
			src = e.Left
		}
		ex, err := classad.ParseExpr(src)
		if err != nil {
			continue
		}
		if a, ok := bareAttr(ex); ok {
			out = append(out, a)
		}
	}
	return out
}

// orderFree reports that a side's rows come back in no order the caller may rely on, so a
// plan that produces the same rows in a different one is still a correct answer.
//
// A mutable table promises nothing: its scan order is the order its segments happen to hold.
// An archive promises newest-first, which is condor_history's contract, unless its config
// gives that up (ArchiveConfig.Unordered). Anything else -- a view, a table this catalog does
// not have -- is treated as ordered, because a wrong guess here is a silently reordered
// answer and the cost of being wrong the other way is one unswapped join.
func (cat *Catalog) orderFree(side JoinSide) bool {
	if _, ok := cat.Table(side.Table); ok {
		return true
	}
	if a, ok := cat.ArchiveTable(side.Table); ok {
		return a.Unordered()
	}
	return false
}
