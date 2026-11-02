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
	// AggUser is a user-defined aggregate, named by AggSpec.UserFunc and registered with
	// RegisterAggregate. It sits past AggMax on purpose: that is what routes a request
	// carrying one onto the extended RPC opcodes, so a server too old to know it refuses
	// rather than answering a different aggregate.
	AggUser
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

	// UserFunc names the registered aggregate when Func is AggUser, and is empty otherwise.
	// It is a separate field rather than an overload of Arg because a user-defined aggregate
	// still takes an argument attribute: p95(RequestMemory) is UserFunc "p95", Arg
	// "RequestMemory".
	UserFunc string
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
func AggregateValues(seq iter.Seq[[]classad.Value], attrs []string, groupCols []GroupCol, aggs []AggSpec, groupCol, aggCol []int, stop func() bool) (rows []AggRow, err error) {
	nGroup := len(groupCols)
	for _, a := range aggs {
		if a.Func == AggCountDistinct && a.Arg == "*" {
			return nil, fmt.Errorf("COUNT(DISTINCT *) is not meaningful; name an attribute")
		}
	}
	// Resolve user-defined aggregates once, so the reduction does no lookup per group, and
	// contain a panic from one for the whole query rather than per row (see udafPanic).
	userAggs, err := resolveAggregates(aggs)
	if err != nil {
		return nil, err
	}
	if userAggs != nil {
		defer func() {
			if r := recover(); r != nil {
				rows, err = nil, &udafPanic{name: userAggName(aggs), val: r}
			}
		}()
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
				scratch[i] = GroupValueText(vals[groupCol[i]])
			}
		}
		if drop {
			continue
		}
		key := strings.Join(scratch, "\x00")
		gs := groups[key]
		if gs == nil {
			gs = newGroupState(append([]string(nil), scratch...), aggs, userAggs)
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
		gs := newGroupState(nil, aggs, userAggs)
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

// newGroupState builds a group's accumulators, giving each user-defined aggregate a fresh
// accumulator of its own. A group gets its own state the moment it exists rather than on its
// first contributing row, because a group every one of whose rows fails its aggregate's FILTER
// still appears in the output -- and its answer is then the aggregate's identity, which only an
// accumulator can say.
func newGroupState(gvals []string, aggs []AggSpec, userAggs []Aggregate) *groupState {
	gs := &groupState{gvals: gvals, accs: make([]aggAcc, len(aggs))}
	for i := range aggs {
		if userAggs != nil && userAggs[i] != nil {
			gs.accs[i].user = userAggs[i].NewAccumulator()
		}
	}
	return gs
}

// userAggName names the user-defined aggregate a contained panic came from. With more than one in
// the request there is no way to tell which from the recovered value, so it names them all rather
// than guessing: a report that says "one of these" is worth more than one that may finger the
// wrong aggregate.
func userAggName(aggs []AggSpec) string {
	var names []string
	for _, a := range aggs {
		if a.Func == AggUser {
			names = append(names, a.UserFunc)
		}
	}
	return strings.Join(names, ", ")
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
	user Accumulator         // a user-defined aggregate's own state (AggUser)
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
	case AggUser:
		// Every row is offered, undefined included: whether to skip one is the aggregate's
		// decision, exactly as it is for the built-ins, which drop undefined inside the
		// reference implementation rather than before it.
		a.user.Add(v)
	case AggCountDistinct:
		// Keyed on the same rendering the group tuple uses, so two rows count as one
		// value exactly when they would group together.
		if defined {
			if a.seen == nil {
				a.seen = map[string]struct{}{}
			}
			a.seen[GroupValueText(v)] = struct{}{}
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
	case AggUser:
		return ValueText(a.user.Result())
	}
	return "undefined"
}

// --- small value helpers (server-side, string-rendering) ---

// bucketKeyText floors a numeric value into an epoch-aligned bucket of the given
// width (seconds) and returns its integer-seconds text. ok is false when v is not a
// finite number -- undefined, error, a non-numeric string, a boolean, or a real that is
// a NaN or an infinity -- so the row drops out of the series, matching the client-side
// time_bucket semantics.
func bucketKeyText(v classad.Value, width int64) (string, bool) {
	f, ok := numberOf(v)
	if !ok {
		return "", false
	}
	// numberOf accepts a NaN or an infinity -- they ARE reals -- so the finiteness test lives in
	// bucketFloorInt, one floor shared with the view engine. A value in no bucket drops here just
	// as a string does.
	b, ok := bucketFloorInt(f, width)
	if !ok {
		return "", false
	}
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

// GroupValueText renders a value as its GROUP: the text that both names the group and decides
// which values share one.
//
// GROUPING IS AN EQUIVALENCE RELATION, AND `==` IS NOT ONE. Two values belong in one group
// exactly when they are the same value, which neither ClassAd operator quite says:
//
//	a          b              a == b       a =?= b     one group?
//	undefined  undefined      undefined    true        yes
//	error      error          error        true        yes
//	4          "4"            error        false       no
//	true       "true"         error        false       no
//	"alice"    "Alice"        true         false       no
//	1          1.0            true         false       YES
//	true       1              true         false       no
//
// `==` disqualifies itself twice over: under it an undefined value is not equal to ITSELF, so
// there is no group to put it in, and a cross-type pair answers `error` rather than
// "different". A relation that is neither reflexive nor total cannot partition rows. That is
// the same corner SQL turns, and it turns it the same way -- GROUP BY is defined on "is not
// distinct from", not on `=`, which is why NULL groups with NULL.
//
// `=?=` IS total and reflexive, and for strings, bools and undefined it is exactly right. It
// is wrong in one place: it separates 1 from 1.0. Those are one quantity written two ways,
// and HTCondor ads genuinely carry both spellings of the same attribute, so splitting them
// gives two rows for one number -- a surprise with nothing behind it. The storage layer keys
// by (bits, type) and separates them; this is the layer that decides they are one group.
//
// So the relation is: a number equals a number when they are numerically equal, a string
// equals a string when it is byte-identical, every other kind equals only its own kind and
// value, and undefined and error each form one group. Within numbers that is `==`; within
// strings it is `=?=`; across kinds it is neither, because neither answers.
//
// WHY ONE FUNCTION AND NOT TWO. The key and the label are the same channel: AggRow.Group is
// what a caller reads AND what it keys on. A key finer than its label yields two output rows
// with one name -- which is what a tagged key over ValueText's labels produced, and
// TestArchiveGroupedStringColumn says it plainly: "group "undefined" appears twice". The
// rendering is made FAITHFUL instead, so distinct groups always read differently: a string
// arrives quoted, which is what keeps it from reading as the number or the bool it spells.
//
// That faithfulness is also what lets 1 and 1.0 be one group WITHOUT hiding anything. They
// render alike because they ARE alike here; 4 and "4" render differently because they are not.
//
// AND WHY THIS MUST NOT SHARE A RENDERER WITH joinPlan.key, which looks like the same job.
// The two have OPPOSITE requirements. A group key has to be exact: it partitions rows, so
// merging two values that are not the same value is a wrong row. A join's hash key has to be
// COARSE: it only narrows candidates, and the condition re-checks every surviving pair, so
// putting two unequal rows in one bucket costs a comparison -- while separating two rows the
// condition WOULD match loses them. A join's condition is usually `==`, so its key folds case
// on purpose, which is exactly what this must not do.
//
// A NaN groups with other NaNs, though `NaN == NaN` is false, and an infinity with its own
// sign. Both are the same choice undefined and error get: a relation has no answer, and one
// group for all of them says more than one group per row. A bucketed series drops them
// before they reach here anyway.
//
// A LIST OR A NESTED AD is reached by no operator at all: `{1,2} =?= {1,2}` is error, so even
// reflexivity fails there. They group by canonical text, which collects identical ones
// together -- the most that can be said about values no relation reaches, and more useful than
// one group per row.
func GroupValueText(v classad.Value) string {
	switch {
	case v.IsString():
		// Quoted, which is the whole of what keeps "4" out of the integer 4's group and
		// "undefined" out of undefined's. Value.String() quotes and escapes it.
		return v.String()
	case v.IsInteger():
		if n, err := v.IntValue(); err == nil {
			return strconv.FormatInt(n, 10)
		}
	case v.IsReal():
		if f, err := v.RealValue(); err == nil {
			return numberGroupText(f)
		}
	}
	return v.String()
}

// numberGroupText renders a real so that an integral one reads as the integer it equals, and
// nothing else changes.
//
// FormatInt and not 'g', because the integer it has to match is rendered by FormatInt: 'g'
// would give a large integral real an exponent (9.007199254740992e+15) where the integer of
// the same value spells itself out, and the two would miss each other though they are one
// number. Integers are never routed through a float at all -- 9007199254740993 and
// 9007199254740992 are different numbers and one float64, so rounding them together would
// merge two groups that are not the same value.
//
// The range test is strict at the top on purpose. math.MaxInt64 converted to a float64 is
// 9223372036854775808, one MORE than MaxInt64, so `f <= math.MaxInt64` admits a value that
// int64() cannot represent -- and in Go that conversion is implementation-defined, which is
// the same trap bucketFloorInt documents. The bound here is that float, excluded.
func numberGroupText(f float64) string {
	const twoPow63 = float64(1 << 63) // the first float64 above MaxInt64
	if f == math.Trunc(f) && f >= -twoPow63 && f < twoPow63 {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// ValueText renders a value as a plain display string, for an aggregate's RESULT. It is not a
// group rendering and must not be used as one: it drops the quoting and the decimal point that
// tell two groups apart. See GroupValueText.
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
