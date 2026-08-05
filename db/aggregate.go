package db

import (
	"fmt"
	"iter"
	"math"
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// This file holds the shared server-side GROUP BY / reduce engine. Both the mutable-table
// aggregate (dbrpc's opAggregate) and the archive aggregate (ArchiveTable.Aggregate) drive
// it, so their grouping and reduction are byte-for-byte identical -- an archive COUNT/SUM/
// AVG/MIN/MAX over a history table behaves exactly like the same aggregate over a live one.
// The RPC layer aliases the public types here (AggFunc/AggSpec/AggRow/GroupCol) so its wire
// API is unchanged.

// AggFunc is a SQL aggregate function.
type AggFunc uint8

const (
	AggCount AggFunc = iota // COUNT(*) or COUNT(col)
	AggSum
	AggAvg
	AggMin
	AggMax
	// AggCountDistinct is COUNT(DISTINCT col): the number of distinct defined values of
	// the argument in the group. It is EXACT, and therefore keeps one entry per distinct
	// value per group while the scan runs -- fine for the attributes people group and
	// count by (Owner, JobStatus, a host name), but not something to point at a
	// unique-per-row attribute over a large history. There is deliberately no silent
	// switch to a sketch: a query written COUNT(DISTINCT ...) gets a true count, and an
	// approximate one would have to be asked for by name.
	AggCountDistinct
)

// AggSpec is one aggregate in a query: a function over an argument attribute.
// Arg "*" (only meaningful for plain COUNT) counts every row in the group; otherwise
// Arg is an attribute name evaluated per ad.
//
// Filter, when non-empty, is a ClassAd expression restricting this aggregate -- and only
// this one -- to the rows of its group where the expression is true (SQL's
// `COUNT(*) FILTER (WHERE ...)`). It is what lets one pass over the data answer several
// differently-conditioned questions at once:
//
//	SELECT Owner, COUNT(*), COUNT(*) FILTER (WHERE JobStatus == 2) FROM jobs GROUP BY Owner
//
// Without it that is one scan per condition. The filter narrows an aggregate, never the
// group: a group whose rows all fail every filter still appears, with COUNT 0 and the other
// functions undefined, exactly as SQL has it.
type AggSpec struct {
	Func   AggFunc
	Arg    string
	Filter string
}

// AggRow is one group's result: the group-by column values followed by the
// aggregate values, all rendered as strings (aligned with the request's group
// columns and aggregate specs).
type AggRow struct {
	Group  []string
	Values []string
}

// GroupCol is one GROUP BY column for a (possibly bucketed) aggregate: the
// attribute Attr, optionally floored into fixed-width buckets. BucketWidth == 0
// groups by the raw attribute value; BucketWidth > 0 (seconds) groups by the
// epoch-aligned bucket floor(number(Attr)/BucketWidth)*BucketWidth, and a row whose
// Attr is not a finite number drops out of the result. This is the shared shape a
// bucketed aggregate and a bucketed materialized view can both group by.
type GroupCol struct {
	Attr        string
	BucketWidth int64
}

// AggProjection builds the deduplicated list of attributes the aggregation reads
// (group columns then non-"*" aggregate arguments) and the index of each group
// column / aggregate argument within that list. An aggregate whose argument is
// "*" (COUNT(*)) gets index -1. A caller projects a scan to attrs and feeds the
// resulting value rows, groupCol, and aggCol to AggregateValues.
func AggProjection(groupCols []GroupCol, aggs []AggSpec) (attrs []string, groupCol, aggCol []int) {
	idx := map[string]int{}
	intern := func(name string) int {
		if i, ok := idx[name]; ok {
			return i
		}
		i := len(attrs)
		idx[name] = i
		attrs = append(attrs, name)
		return i
	}
	groupCol = make([]int, len(groupCols))
	for i, g := range groupCols {
		groupCol[i] = intern(g.Attr)
	}
	aggCol = make([]int, len(aggs))
	for i, a := range aggs {
		if a.Arg == "*" {
			aggCol[i] = -1
		} else {
			aggCol[i] = intern(a.Arg)
		}
		// A filtered aggregate also reads whatever its filter tests, so those attributes
		// have to be projected too or the filter would see them all undefined.
		for _, ref := range filterAttrs(a.Filter) {
			intern(ref)
		}
	}
	return attrs, groupCol, aggCol
}

// AggFilterAttrs returns the attributes a per-aggregate filter reads. Callers use it to hold
// a filter to the same rules as the rest of the request -- notably the RPC layer's refusal to
// let an unprivileged reader touch a private attribute, which a filter could otherwise turn
// into an oracle (`COUNT(*) FILTER (WHERE Secret == "guess")` leaks by its count).
func AggFilterAttrs(filter string) []string { return filterAttrs(filter) }

// filterAttrs returns the attributes a per-aggregate filter reads, or nil for no filter (or
// one that does not compile -- compileFilters reports that as an error later, where it can
// be returned rather than silently dropped).
func filterAttrs(filter string) []string {
	if filter == "" {
		return nil
	}
	q, err := vm.Parse(filter)
	if err != nil {
		return nil
	}
	return q.ReadAttrs()
}

// aggFilter is one compiled per-aggregate filter, bound to the projected value row: expr is
// evaluated against a scope holding just the attributes it reads, at the positions the
// projection put them.
type aggFilter struct {
	expr  *classad.Expr
	names []string // attribute names the filter reads
	cols  []int    // their indices in the projected value row, aligned with names
}

// compileFilters compiles each spec's filter against the projected attribute list. It
// returns one entry per spec (nil where the spec is unfiltered) and whether any spec has
// one, so the reduction can skip the whole mechanism for the common unfiltered query.
func compileFilters(attrs []string, aggs []AggSpec) ([]*aggFilter, bool, error) {
	out := make([]*aggFilter, len(aggs))
	col := make(map[string]int, len(attrs))
	for i, a := range attrs {
		col[strings.ToLower(a)] = i
	}
	any := false
	for i, a := range aggs {
		if a.Filter == "" {
			continue
		}
		q, err := vm.Parse(a.Filter)
		if err != nil {
			return nil, false, fmt.Errorf("aggregate filter %q: %w", a.Filter, err)
		}
		ex, err := classad.ParseExpr(a.Filter)
		if err != nil {
			return nil, false, fmt.Errorf("aggregate filter %q: %w", a.Filter, err)
		}
		f := &aggFilter{expr: ex}
		for _, ref := range q.ReadAttrs() {
			if c, ok := col[strings.ToLower(ref)]; ok {
				f.names = append(f.names, ref)
				f.cols = append(f.cols, c)
			}
		}
		out[i] = f
		any = true
	}
	return out, any, nil
}

// keeps reports whether a row passes this filter. The scope ad is reused across rows and
// filters, so a filtered aggregate costs one rebind plus one evaluation per row rather than
// an allocation. A filter that does not evaluate to true excludes the row, so undefined and
// error behave as they do everywhere else in ClassAd.
func (f *aggFilter) keeps(scope *classad.ClassAd, vals []classad.Value) bool {
	for k, c := range f.cols {
		bindValue(scope, f.names[k], vals[c])
	}
	b, err := f.expr.Eval(scope).BoolValue()
	return err == nil && b
}

// bindValue binds one projected value into the reused filter scope. Every attribute the
// filter reads is rebound on every row -- including as undefined -- so nothing carries over
// from the previous row.
//
// Scalars bind natively. A list or nested ad binds as undefined: the projected scan hands
// back evaluated values, and there is no exported way to turn one back into an expression,
// so a filter over a list-valued attribute cannot be answered on this path. Filters in
// practice test scalars (JobStatus, ExitCode, Owner), and undefined excludes the row rather
// than miscounting it.
func bindValue(ad *classad.ClassAd, name string, v classad.Value) {
	switch {
	case v.IsInteger():
		if i, err := v.IntValue(); err == nil {
			ad.InsertAttr(name, i)
			return
		}
	case v.IsReal():
		if f, err := v.RealValue(); err == nil {
			ad.InsertAttrFloat(name, f)
			return
		}
	case v.IsBool():
		if b, err := v.BoolValue(); err == nil {
			ad.InsertAttrBool(name, b)
			return
		}
	case v.IsString():
		if s, err := v.StringValue(); err == nil {
			ad.InsertAttrString(name, s)
			return
		}
	}
	ad.Insert(name, &ast.UndefinedLiteral{})
}

// AggregateValues is the shared GROUP BY core: it buckets a sequence of projected
// value rows (each aligned to the attribute list AggProjection returned) by the
// (possibly time-bucketed) group tuple and reduces each group with the COUNT/SUM/
// AVG/MIN/MAX accumulators, returning one AggRow per group in first-seen order.
// groupCol[i]/aggCol[i] index into each value row (aggCol[i] < 0 means COUNT(*)).
// With no group columns it returns a single row aggregating the whole sequence,
// still yielding one row over an empty sequence (SQL semantics: COUNT is 0, others
// undefined). If stop is non-nil it is polled per row and, when it returns true,
// the scan halts early with the groups accumulated so far -- used to abandon a
// scan whose client has gone away.
func AggregateValues(seq iter.Seq[[]classad.Value], attrs []string, groupCols []GroupCol, aggs []AggSpec, groupCol, aggCol []int, stop func() bool) ([]AggRow, error) {
	nGroup := len(groupCols)
	for _, a := range aggs {
		if a.Func == AggCountDistinct && a.Arg == "*" {
			return nil, fmt.Errorf("COUNT(DISTINCT *) is not meaningful; name an attribute")
		}
	}
	filters, anyFilter, err := compileFilters(attrs, aggs)
	if err != nil {
		return nil, err
	}
	// One reused scope for every filter on every row; only touched when a filter exists, so
	// an unfiltered aggregate keeps its allocation-free reduction.
	var scope *classad.ClassAd
	if anyFilter {
		scope = classad.New()
	}

	// Hash-map aggregation: bucket by the joined group-value tuple, preserving
	// first-seen order for stable output. scratch builds each row's key without
	// allocating a slice per row; a group keeps its own copy only when created.
	groups := map[string]*groupState{}
	var order []string
	scratch := make([]string, nGroup)
	for vals := range seq {
		if stop != nil && stop() {
			break // client gone: stop the scan
		}
		drop := false
		for i, g := range groupCols {
			if g.BucketWidth > 0 {
				kt, ok := bucketKeyText(vals[groupCol[i]], g.BucketWidth)
				if !ok {
					drop = true // non-numeric bucket attribute: row leaves the series
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
		gs := groups[key]
		if gs == nil {
			gs = &groupState{gvals: append([]string(nil), scratch...), accs: make([]aggAcc, len(aggs))}
			groups[key] = gs
			order = append(order, key)
		}
		for i, a := range aggs {
			if filters[i] != nil && !filters[i].keeps(scope, vals) {
				continue // this row is outside THIS aggregate's filter, not the group
			}
			var v classad.Value
			if aggCol[i] >= 0 {
				v = vals[aggCol[i]]
			}
			gs.accs[i].update(a, v)
		}
	}

	// A group-less aggregate over an empty match still yields one row.
	if nGroup == 0 && len(order) == 0 {
		gs := &groupState{accs: make([]aggAcc, len(aggs))}
		row := AggRow{Values: make([]string, len(aggs))}
		for i, a := range aggs {
			row.Values[i] = gs.accs[i].result(a)
		}
		return []AggRow{row}, nil
	}

	out := make([]AggRow, 0, len(order))
	for _, key := range order {
		gs := groups[key]
		row := AggRow{Group: gs.gvals, Values: make([]string, len(aggs))}
		for i, a := range aggs {
			row.Values[i] = gs.accs[i].result(a)
		}
		out = append(out, row)
	}
	return out, nil
}

// groupState holds one group's key values and per-aggregate accumulators.
type groupState struct {
	gvals []string
	accs  []aggAcc
}

// aggAcc accumulates one group's data for one aggregate. COUNT needs only
// counters; SUM/AVG/MIN/MAX collect the evaluated argument values and hand them
// to the ClassAd library's aggregate functions at the end, so type coercion
// (int64-exact sums, int+real promotion, boolean and undefined handling, error
// propagation, numeric-only min/max) matches HTCondor's sum()/avg()/min()/max()
// exactly rather than a re-implementation.
type aggAcc struct {
	rows int                 // all rows in the group (COUNT(*))
	defN int                 // rows where the argument is defined (COUNT(col))
	vals []classad.Value     // argument values for SUM/AVG/MIN/MAX
	seen map[string]struct{} // distinct defined argument values (COUNT DISTINCT)
}

// update folds one row's already-resolved argument value v into the accumulator.
// For COUNT(*) (spec.Arg == "*") v is ignored.
func (a *aggAcc) update(spec AggSpec, v classad.Value) {
	a.rows++
	if spec.Arg == "*" {
		return
	}
	defined := !v.IsUndefined() && !v.IsError()
	if defined {
		a.defN++
	}
	switch spec.Func {
	case AggCount:
	case AggCountDistinct:
		// Keyed on the same rendering the group tuple uses, so two rows count as one
		// value exactly when they would group together.
		if defined {
			if a.seen == nil {
				a.seen = map[string]struct{}{}
			}
			a.seen[ValueText(v)] = struct{}{}
		}
	default:
		a.vals = append(a.vals, v) // the library aggregates skip undefined / coerce
	}
}

func (a *aggAcc) result(spec AggSpec) string {
	switch spec.Func {
	case AggCount:
		// COUNT(*) counts every row; COUNT(col) counts rows where col is defined.
		if spec.Arg == "*" {
			return strconv.Itoa(a.rows)
		}
		return strconv.Itoa(a.defN)
	case AggCountDistinct:
		return strconv.Itoa(len(a.seen))
	case AggSum:
		return ValueText(classad.Sum(a.vals))
	case AggAvg:
		return ValueText(classad.Avg(a.vals))
	case AggMin:
		return ValueText(classad.Min(a.vals))
	case AggMax:
		return ValueText(classad.Max(a.vals))
	}
	return "undefined"
}

// --- small value helpers (server-side, string-rendering) ---

// bucketKeyText floors a numeric value into an epoch-aligned bucket of the given
// width (seconds) and returns its integer-seconds text. ok is false when v is not a
// finite number (undefined/error/non-numeric string), so the row drops out of the
// series -- matching the client-side time_bucket semantics.
func bucketKeyText(v classad.Value, width int64) (string, bool) {
	f, ok := numberOf(v)
	if !ok {
		return "", false
	}
	b := int64(math.Floor(f/float64(width))) * width
	return strconv.FormatInt(b, 10), true
}

// numberOf returns the numeric value of v (integer or real) and whether it is one.
func numberOf(v classad.Value) (float64, bool) {
	switch {
	case v.IsInteger():
		i, _ := v.IntValue()
		return float64(i), true
	case v.IsReal():
		r, _ := v.RealValue()
		return r, true
	}
	return 0, false
}

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// ValueText renders a value as a group-key/display string.
func ValueText(v classad.Value) string {
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsError():
		return "error"
	case v.IsBool():
		b, _ := v.BoolValue()
		return strconv.FormatBool(b)
	case v.IsString():
		s, _ := v.StringValue()
		return s
	case v.IsInteger():
		i, _ := v.IntValue()
		return strconv.FormatInt(i, 10)
	case v.IsReal():
		r, _ := v.RealValue()
		return trimFloat(r)
	default:
		return v.String()
	}
}
