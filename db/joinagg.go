package db

// Reducing a join's rows inside the store.
//
// A grouped aggregate over a join has an output that does not scale with the join's output.
// `SELECT j.Owner, COUNT(*) ... GROUP BY j.Owner` answers in one row per owner however many
// joined rows produced it, so streaming the rows out and reducing them at the far end sends
// the one quantity that grows for the one that does not.
//
// How much that is, measured over a jobs/history equi join with 20 owners (bytes are
// old-ClassAd text, the form the join streams), sweeping the expansion ratio -- right rows
// per left row:
//
//	exp  joined rows  whole-ad bytes  projected bytes  grouped bytes  whole:grouped  proj:grouped
//	  1        2,000         787,786          122,000            500         1575x         244x
//	  2        4,000       1,575,572          244,000            500         3151x         488x
//	  4        8,000       3,151,144          488,000            500         6302x         976x
//	  8       16,000       6,302,288          976,000            500        12605x        1952x
//	 16       32,000      12,604,576        1,952,000            520        24240x        3754x
//
// The projected column is the honest one: it is a client that already knew it was going to
// group and asked for nothing but the two attributes it would read. It still pays 244x at an
// expansion of 1 and the ratio doubles with every doubling of the expansion, because the
// numerator scales with joined rows and the denominator does not.
//
// The same sweep run the other way -- holding the join and making the grouping finer -- says
// where it stops paying: at 2,000 groups over 4,000 joined rows, where the reduction barely
// halves the rows, the projected ratio is still 5.3x and the unprojected 34x. There is no
// shape in that sweep where shipping the rows wins.
//
// WHAT THIS IS, mechanically: the existing join, with the existing single-table reduce engine
// reading its composite rows. Nothing about the join changes and no reducer is rewritten, so a
// COUNT/SUM/AVG/MIN/MAX over a join coerces, skips undefined and renders exactly as the same
// function over one table does.
//
// The one thing that genuinely differs is the NAMES. A joined row is a composite ad, so the
// group key and the aggregate argument are dotted paths over it (`j.Owner`, `h.ExitCode`), not
// bare attribute names, and they are evaluated as ClassAd expressions rather than looked up.

import (
	"fmt"
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// maxJoinGroups caps the distinct groups one join aggregate may return.
//
// It bounds the ANSWER, not the peak: the reduce engine holds one entry per group while it
// runs and this is checked once it has finished, so the memory is already spent by the time
// it fires. That is deliberate rather than overlooked -- bounding the peak would mean
// counting groups a second time alongside the engine that is already counting them, and the
// peak is bounded anyway by the join's own MaxPairs budget.
//
// What it is actually for is the request that is not an aggregate. A hundred thousand groups
// is a row stream with a GROUP BY written on it, and answering it would hand back something
// the size of the rows this exists to avoid sending.
var maxJoinGroups = 100_000

// DefaultJoinAggLimits are the limits a join aggregate runs under when a caller names none.
//
// They differ from DefaultJoinLimits in exactly one place, and for one reason: MaxRows bounds
// the joined rows PRODUCED, which in a streaming join is the bound on what crosses the wire.
// Here the rows are reduced where they are made and none of them crosses anything, so that
// bound is being applied to a quantity it was not written about -- and applying it would
// refuse the queries this is for, since a history join big enough to be worth reducing in the
// store is routinely past a million rows.
//
// The budgets that bound real work are unchanged. MaxHashRows still bounds what is held, and
// MaxPairs -- which is what actually prices a join, since one bucket can be a cross product --
// still bounds what is examined. MaxRows is raised to meet MaxPairs rather than removed, so
// the reduction's input is still bounded by something.
func DefaultJoinAggLimits() JoinLimits {
	l := DefaultJoinLimits()
	l.MaxRows = l.MaxPairs
	return l
}

// JoinAggSpec is a grouped aggregate over a join's rows.
//
// It is an AggRequest whose rows come from a join instead of a table, which is why it has no
// Constraint of its own: a join already carries three places to filter (each side's own
// Constraint, the ON condition, and the Post clause over the composite row) and a fourth
// spelling of the same thing would only raise the question of where it ran.
type JoinAggSpec struct {
	// Join is the join whose rows are reduced. Its Limit, if set, truncates the reduction's
	// INPUT, so a limited join aggregates a prefix of itself -- which is a different answer,
	// not a smaller one. Callers that want a bounded answer bound the groups, not the rows.
	Join JoinSpec
	// GroupCols is the GROUP BY, in output order. Each Attr is a ClassAd expression over the
	// composite row -- in practice a dotted path like `j.Owner`, but any expression the
	// evaluator accepts works, so `j.Owner` and `split(j.Cmd, "/")[0]` are the same kind of
	// thing here. Empty means a scalar aggregate over the whole join, which still yields
	// exactly one row.
	GroupCols []GroupCol
	// Aggs are the outputs, in output order. Arg is likewise an expression over the composite
	// row, except for "*" which counts rows. A spec's Filter, when set, is also an expression
	// over the composite row, and narrows that one aggregate rather than the group.
	Aggs []AggSpec
}

// JoinAggregate runs a join and reduces its rows to one row per group, returning only the
// groups.
//
// Setup problems -- an unknown table, a path that does not parse -- are returned before any
// row is read. A join that met a limit part way through is returned as an ERROR with no rows,
// which is the one place this deliberately differs from the row-streaming join: a truncated
// stream still handed the caller real rows and only had to say it stopped, whereas a truncated
// reduction is not a partial answer but a wrong one -- a COUNT that stopped early looks exactly
// like a smaller COUNT.
//
// stats (may be nil) is filled with the join's work breakdown, including Groups, so a caller
// can see the reduction ratio it bought.
//
// stop (may be nil) is polled per joined row by the reduce engine; returning true abandons the
// scan. It is checked again afterwards, so a cancelled aggregate is reported as cancelled
// rather than returned as the groups that happened to accumulate first.
func (cat *Catalog) JoinAggregate(spec JoinAggSpec, want JoinLimits, stats *JoinStats, stop func() bool) ([]AggRow, error) {
	plan, err := compileJoinAgg(spec)
	if err != nil {
		return nil, err
	}
	if stats == nil {
		stats = &JoinStats{}
	}
	seq, err := cat.joinWith(spec.Join, DefaultJoinAggLimits().clamp(want), stats)
	if err != nil {
		return nil, err
	}
	rows, err := AggregateValues(plan.values(seq), plan.attrs, spec.GroupCols, plan.aggs,
		plan.groupCol, plan.aggCol, stop)
	if err != nil {
		return nil, err
	}
	// The join reports a limit it met in stats.Err, because a sequence that yields rows has
	// no other channel. Here there is one, and a reduction built from a truncated input must
	// not be handed back as the answer.
	if stats.Err != nil {
		return nil, stats.Err
	}
	if stop != nil && stop() {
		return nil, fmt.Errorf("the join aggregate was abandoned before it finished")
	}
	stats.Groups = len(rows)
	if len(rows) > maxJoinGroups {
		return nil, fmt.Errorf("the join aggregate produced %d groups, more than the %d it may "+
			"return; group by something coarser", len(rows), maxJoinGroups)
	}
	return rows, nil
}

// joinAggPlan is a compiled JoinAggSpec: the expressions to evaluate per composite row, and
// where each group column and aggregate argument lands in the value row the reduce engine reads.
type joinAggPlan struct {
	// exprs are evaluated against each composite row, in value-row order. attrs is their
	// source text, which is what the reduce engine's filter binding keys on.
	exprs []*classad.Expr
	attrs []string
	// aggs are the caller's specs with each Filter rewritten to name its synthetic column
	// (see compileJoinAgg). Everything else about them is untouched.
	aggs     []AggSpec
	groupCol []int
	aggCol   []int
	// filters are the caller's filter expressions, aligned with aggs; nil where a spec has
	// none. filterCol[i] is the value-row slot filters[i]'s result is written to.
	filters   []*classad.Expr
	filterCol []int
}

// compileJoinAgg compiles the paths and builds the projection.
//
// It does NOT use AggProjection, for a reason worth stating: that function interns the
// attributes a FILTER reads so the reduce engine can bind them into a flat scope, and over a
// composite row those attributes are the wrong thing. `h.ExitCode == 0` reads, as far as
// attribute analysis is concerned, the single attribute `h` -- the nested ad -- so binding it
// would hand the filter a side, not a value, and binding a nested ad yields undefined, which
// would make every filtered aggregate count zero.
//
// So a composite-row filter is evaluated where it makes sense, against the composite row, and
// its already-decided boolean is carried into the value row as a synthetic column. The spec
// the reduce engine sees then has a filter that is one bare attribute name, which is a shape
// its existing binding handles exactly: a true keeps the row, and anything else -- false,
// undefined from a missing side, an error -- excludes it, same as everywhere else.
func compileJoinAgg(spec JoinAggSpec) (*joinAggPlan, error) {
	p := &joinAggPlan{
		aggs:      append([]AggSpec(nil), spec.Aggs...),
		groupCol:  make([]int, len(spec.GroupCols)),
		aggCol:    make([]int, len(spec.Aggs)),
		filters:   make([]*classad.Expr, len(spec.Aggs)),
		filterCol: make([]int, len(spec.Aggs)),
	}
	idx := map[string]int{}
	// Paths are interned case-insensitively because ClassAd attribute names are, so `j.Owner`
	// and `j.owner` are one column rather than two evaluations of the same thing.
	intern := func(what, src string) (int, error) {
		if i, ok := idx[strings.ToLower(src)]; ok {
			return i, nil
		}
		ex, err := classad.ParseExpr(src)
		if err != nil {
			return 0, fmt.Errorf("%s %q: %w", what, src, err)
		}
		i := len(p.attrs)
		idx[strings.ToLower(src)] = i
		p.attrs = append(p.attrs, src)
		p.exprs = append(p.exprs, ex)
		return i, nil
	}
	for i, g := range spec.GroupCols {
		c, err := intern("GROUP BY", g.Attr)
		if err != nil {
			return nil, err
		}
		p.groupCol[i] = c
	}
	for i, a := range spec.Aggs {
		p.aggCol[i] = -1
		p.filterCol[i] = -1
		if a.Arg != "*" {
			c, err := intern("aggregate argument", a.Arg)
			if err != nil {
				return nil, err
			}
			p.aggCol[i] = c
		}
		if a.Filter == "" {
			continue
		}
		ex, err := classad.ParseExpr(a.Filter)
		if err != nil {
			return nil, fmt.Errorf("aggregate filter %q: %w", a.Filter, err)
		}
		name := freeFilterName(idx, i)
		c := len(p.attrs)
		idx[strings.ToLower(name)] = c
		p.attrs = append(p.attrs, name)
		p.exprs = append(p.exprs, nil) // filled per row from filters[i], not evaluated as a path
		p.filters[i] = ex
		p.filterCol[i] = c
		p.aggs[i].Filter = name
	}
	return p, nil
}

// freeFilterName picks a synthetic column name for aggregate i that no path already uses.
// A caller cannot collide with it by accident, and checking makes that structural rather than
// a bet on nobody naming a path `__joinfilter0_0`.
func freeFilterName(idx map[string]int, i int) string {
	for n := 0; ; n++ {
		name := fmt.Sprintf("__joinfilter%d_%d", i, n)
		if _, taken := idx[strings.ToLower(name)]; !taken {
			return name
		}
	}
}

// values turns the joined rows into the projected value rows the reduce engine reads.
//
// The backing slice is reused across rows: the engine renders group keys to strings and copies
// each aggregate's argument value out as it goes, so nothing it keeps points back here.
func (p *joinAggPlan) values(seq iter.Seq[*classad.ClassAd]) iter.Seq[[]classad.Value] {
	return func(yield func([]classad.Value) bool) {
		vals := make([]classad.Value, len(p.attrs))
		for row := range seq {
			for i, ex := range p.exprs {
				if ex != nil {
					vals[i] = ex.Eval(row)
				}
			}
			for i, f := range p.filters {
				if f != nil {
					vals[p.filterCol[i]] = f.Eval(row)
				}
			}
			if !yield(vals) {
				return
			}
		}
	}
}
