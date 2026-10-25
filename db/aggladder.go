package db

import "fmt"

// One shape for the two aggregate ladders.
//
// A grouped aggregate is answered by whichever of several pieces of code can serve it, tried in a
// fixed order, each either serving the whole request or standing aside for the next. That
// discipline is what makes the fast paths trustworthy -- none of them ever answers a question it
// only half understands -- but it was written out twice, once for an archive table and once for a
// mutable one, as two chains of `if rows, ok := ...; ok { return rows, nil }`. The request itself
// had no name: it was three parameters that every rung re-read and re-interpreted, so whether a
// bucket width or a per-aggregate filter could be served was a property of which function the
// caller happened to land in rather than of the question asked.
//
// This file gives both chains one vocabulary. AggRequest says what is being asked; AggStrategy
// answers it completely or declines. Nothing about which rung serves what changes: the two ladders
// list the same strategies they always tried, in the same order, under the same conditions, report
// the same tier, and return the same rows.

// AggRequest is one grouped aggregate, stated independently of whatever answers it.
//
// Every field a strategy may have to decline over lives here rather than in the strategy, which is
// the point of naming the type at all: a bucket width rides on its group column and a FILTER rides
// on its aggregate, so "can this be served" is a question about the request.
//
// What is deliberately NOT here is execution: the caller's scan-stats block and its cancellation
// callback are not part of the question asked, they are how this particular run of it is being
// watched, so a ladder captures them when it is built rather than carrying them in the request.
type AggRequest struct {
	// Constraint filters rows before grouping. Empty, or any expression that references no
	// attribute and evaluates TRUE, matches every row (IsMatchAll); one that references no
	// attribute and does not evaluate TRUE matches none (IsMatchNone).
	Constraint string
	// GroupCols is the GROUP BY, in output order. Empty means a scalar aggregate over the whole
	// match, which still yields exactly one row. A column may carry a bucket width, flooring a
	// numeric attribute into fixed-width buckets.
	GroupCols []GroupCol
	// Aggs are the outputs, in output order. A spec may carry its own FILTER, restricting that
	// one aggregate to the rows of its group matching it.
	Aggs []AggSpec
}

// isPlainCount reports whether the request is a single unfiltered COUNT(*) with no grouping -- the
// shape every count fast path is gated on, because it is the only one answerable from a record
// count alone. A FILTER disqualifies it: a stored count knows nothing about the filter, so serving
// it would report an unfiltered total as a filtered one.
func (r AggRequest) isPlainCount() bool {
	return len(r.GroupCols) == 0 && len(r.Aggs) == 1 &&
		r.Aggs[0].Func == AggCount && r.Aggs[0].Arg == "*" && r.Aggs[0].Filter == ""
}

// AggStrategy is one rung of an aggregate ladder: it answers an AggRequest, or declines so the
// next rung may try.
//
// Three rules hold for every implementation.
//
// Decline freely, never answer partially. ok=false means "not me", for any reason and at any
// point -- a strategy that gets halfway and finds it cannot finish throws its partial result away
// and declines. The runner enforces the second half of that by ignoring the rows of a declining
// strategy outright, so a half-built result cannot reach a caller even by accident.
//
// The last rung never declines. The record scan is the reference and always has an answer, so a
// ladder whose last rung declines is a programming error rather than a query the system cannot
// serve, and the runner reports it as one.
//
// Every rung's answer equals the reference's for the same request, as a row SET: the index rung
// returns groups sorted by value and every other rung in first-seen order, and GROUP BY without
// ORDER BY defines neither. A rung that would render a value, partition a group or drop a row
// differently from the scan must decline instead. That is why a boolean in an aggregated column, a
// string argument, a per-aggregate FILTER, a predicate not proven free of CurrentTime and a
// chained table all fall through to the scan rather than being approximated.
//
// err is separate from the decline: it reports a request that no rung can serve because it is
// malformed (COUNT(DISTINCT *), a filter that does not parse) or a read that failed, and it stops
// the ladder rather than advancing it. Today only the rungs that run the reduce engine -- the
// archive's match-none shortcut and both record scans -- can produce one; the rest answer or
// decline.
type AggStrategy interface {
	// Name identifies the rung. It is descriptive -- it names the rung in the runner's error
	// and lets a test assert which rung served a shape -- and is deliberately not reported to
	// a caller, which would be a behaviour change rather than a description of one. The tier a
	// rung reports into the caller's ScanStats is the caller-visible statement, and several
	// rungs share one tier.
	Name() string
	// Answer serves req completely (ok=true) or declines it (ok=false, rows ignored).
	Answer(req AggRequest) (rows []AggRow, ok bool, err error)
}

// aggRung adapts a named closure to AggStrategy, so a ladder reads as the ordered list of
// conditions it is rather than as one type per rung.
type aggRung struct {
	name string
	fn   func(AggRequest) ([]AggRow, bool, error)
}

func (r aggRung) Name() string { return r.name }

func (r aggRung) Answer(req AggRequest) ([]AggRow, bool, error) { return r.fn(req) }

// runAggLadder tries each strategy in order and returns the first complete answer.
//
// A declining strategy's rows are dropped on the floor here rather than trusted to be nil: the
// no-partial-answers rule is the one the fast paths' correctness rests on, and it costs nothing to
// make it structural instead of a convention each rung has to remember.
//
// Reaching the end means the last rung declined, which it is not allowed to do. That is a bug in
// the ladder, not an unanswerable query, so the error says so and names the rung rather than
// returning an empty result that would read as "no matching rows".
func runAggLadder(ladder []AggStrategy, req AggRequest) ([]AggRow, error) {
	for _, s := range ladder {
		rows, ok, err := s.Answer(req)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		return rows, nil
	}
	last := "an empty ladder"
	if n := len(ladder); n > 0 {
		last = ladder[n-1].Name()
	}
	return nil, fmt.Errorf("aggregate: every strategy declined (%s must always answer)", last)
}

// matchAllMemo settles IsMatchAll once per constraint across the rungs of one ladder.
//
// IsMatchAll parses the constraint and evaluates it against an empty ad, and the archive gates two
// separate rungs on it -- which is why the code this describes hoisted it above the chain into a
// local. Splitting the chain into independent rungs would otherwise quietly pay for that twice,
// which is a regression hidden behind a refactor that claims to change nothing.
//
// Keyed on the constraint rather than computed once, because a ladder is a list a test may drive
// over several requests even though a query drives it over exactly one.
type matchAllMemo struct {
	constraint string
	val, set   bool
}

func (m *matchAllMemo) is(constraint string) bool {
	if !m.set || m.constraint != constraint {
		m.constraint, m.val, m.set = constraint, IsMatchAll(constraint), true
	}
	return m.val
}
