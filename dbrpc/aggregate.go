package dbrpc

import (
	"strconv"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// AggFunc is a SQL aggregate function.
type AggFunc uint8

const (
	AggCount AggFunc = iota // COUNT(*) or COUNT(col)
	AggSum
	AggAvg
	AggMin
	AggMax
)

// AggSpec is one aggregate in a query: a function over an argument attribute.
// Arg "*" (only meaningful for COUNT) counts every row in the group; otherwise
// Arg is an attribute name evaluated per ad.
type AggSpec struct {
	Func AggFunc
	Arg  string
}

// AggRow is one group's result: the group-by column values followed by the
// aggregate values, all rendered as strings (aligned with the request's group
// columns and aggregate specs).
type AggRow struct {
	Group  []string
	Values []string
}

// Aggregate runs a server-side GROUP BY: the server buckets the constraint match
// by the group-by column tuple in a hash map and returns one AggRow per group.
// With no group columns it returns a single row aggregating the whole match. The
// aggregation happens on the server, so only the (small) grouped result crosses
// the wire, not every matched ad.
func (c *Client) Aggregate(constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	build := func(id uint64) []byte {
		b := putStr(req(id, opAggregate), constraint)
		b = putI32(b, int32(len(groupBy)))
		for _, g := range groupBy {
			b = putStr(b, g)
		}
		b = putI32(b, int32(len(aggs)))
		for _, a := range aggs {
			b = putStr(putU8(b, byte(a.Func)), a.Arg)
		}
		return b
	}
	_, frames, err := c.callStream(build)
	if err != nil {
		return nil, err
	}
	var out []AggRow
	for frame := range frames {
		_, status, body, ok := respHeader(frame)
		if !ok {
			return out, errShort
		}
		switch status {
		case stStream:
			row := AggRow{Group: make([]string, len(groupBy)), Values: make([]string, len(aggs))}
			for i := range row.Group {
				row.Group[i] = body.str()
			}
			for i := range row.Values {
				row.Values[i] = body.str()
			}
			out = append(out, row)
		case stErr:
			return out, statusErr(status, body)
		}
	}
	return out, nil
}

// streamAggregate performs the server-side aggregation and streams one frame per
// group. It refuses to group or aggregate on a private attribute for a
// connection that may not see private data.
func (s *Server) streamAggregate(reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	constraint := r.str()
	nGroup := int(r.i32())
	if nGroup < 0 || nGroup > 1024 {
		write(respBad(reqID))
		return
	}
	groupBy := make([]string, nGroup)
	for i := range groupBy {
		groupBy[i] = r.str()
	}
	nAgg := int(r.i32())
	if nAgg < 0 || nAgg > 1024 {
		write(respBad(reqID))
		return
	}
	aggs := make([]AggSpec, nAgg)
	for i := range aggs {
		aggs[i] = AggSpec{Func: AggFunc(r.u8()), Arg: r.str()}
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}

	if !includePrivate {
		for _, name := range groupBy {
			if classad.IsPrivateAttribute(name) {
				write(respErr(reqID, "cannot group by private attribute "+name))
				return
			}
		}
		for _, a := range aggs {
			if a.Arg != "*" && classad.IsPrivateAttribute(a.Arg) {
				write(respErr(reqID, "cannot aggregate private attribute "+a.Arg))
				return
			}
		}
	}

	// Project only the attributes the aggregation reads (group columns + the
	// non-"*" aggregate arguments), so the scan reads them wire-native instead of
	// fully decoding every matching ad. attrs is deduplicated; groupCol[i]/aggCol[i]
	// index into the projected value slice.
	attrs, groupCol, aggCol := projectionFor(groupBy, aggs)
	seq, err := s.db.QueryProject(constraint, attrs)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}

	// Hash-map aggregation: bucket by the joined group-value tuple, preserving
	// first-seen order for stable output. scratch builds each row's key without
	// allocating a slice per ad; a group keeps its own copy only when created.
	groups := map[string]*groupState{}
	var order []string
	scratch := make([]string, nGroup)
	for vals := range seq {
		for i := range groupBy {
			scratch[i] = valueText(vals[groupCol[i]])
		}
		key := strings.Join(scratch, "\x00")
		gs := groups[key]
		if gs == nil {
			gs = &groupState{gvals: append([]string(nil), scratch...), accs: make([]aggAcc, nAgg)}
			groups[key] = gs
			order = append(order, key)
		}
		for i, a := range aggs {
			var v classad.Value
			if aggCol[i] >= 0 {
				v = vals[aggCol[i]]
			}
			gs.accs[i].update(a, v)
		}
	}

	// A group-less aggregate over an empty match still yields one row (SQL
	// semantics: COUNT is 0, others undefined).
	if nGroup == 0 && len(order) == 0 {
		gs := &groupState{accs: make([]aggAcc, nAgg)}
		frame := respHead(reqID, stStream)
		for i, a := range aggs {
			frame = putStr(frame, gs.accs[i].result(a))
		}
		write(frame)
		write(respHead(reqID, stStreamEnd))
		return
	}

	for _, key := range order {
		gs := groups[key]
		frame := respHead(reqID, stStream)
		for _, gv := range gs.gvals {
			frame = putStr(frame, gv)
		}
		for i, a := range aggs {
			frame = putStr(frame, gs.accs[i].result(a))
		}
		write(frame)
	}
	write(respHead(reqID, stStreamEnd))
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
	rows int             // all rows in the group (COUNT(*))
	defN int             // rows where the argument is defined (COUNT(col))
	vals []classad.Value // argument values for SUM/AVG/MIN/MAX
}

// update folds one row's already-resolved argument value v into the accumulator.
// For COUNT(*) (spec.Arg == "*") v is ignored.
func (a *aggAcc) update(spec AggSpec, v classad.Value) {
	a.rows++
	if spec.Arg == "*" {
		return
	}
	if !v.IsUndefined() && !v.IsError() {
		a.defN++
	}
	if spec.Func != AggCount {
		a.vals = append(a.vals, v) // the library aggregates skip undefined / coerce
	}
}

// projectionFor builds the deduplicated list of attributes the aggregation reads
// (group columns then non-"*" aggregate arguments) and the index of each group
// column / aggregate argument within that list. An aggregate whose argument is
// "*" (COUNT(*)) gets index -1.
func projectionFor(groupBy []string, aggs []AggSpec) (attrs []string, groupCol, aggCol []int) {
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
	groupCol = make([]int, len(groupBy))
	for i, g := range groupBy {
		groupCol[i] = intern(g)
	}
	aggCol = make([]int, len(aggs))
	for i, a := range aggs {
		if a.Arg == "*" {
			aggCol[i] = -1
			continue
		}
		aggCol[i] = intern(a.Arg)
	}
	return attrs, groupCol, aggCol
}

func (a *aggAcc) result(spec AggSpec) string {
	switch spec.Func {
	case AggCount:
		// COUNT(*) counts every row; COUNT(col) counts rows where col is defined.
		if spec.Arg == "*" {
			return strconv.Itoa(a.rows)
		}
		return strconv.Itoa(a.defN)
	case AggSum:
		return valueText(classad.Sum(a.vals))
	case AggAvg:
		return valueText(classad.Avg(a.vals))
	case AggMin:
		return valueText(classad.Min(a.vals))
	case AggMax:
		return valueText(classad.Max(a.vals))
	}
	return "undefined"
}

// --- small value helpers (server-side, string-rendering) ---

func trimFloat(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// valueText renders a value as a group-key/display string.
func valueText(v classad.Value) string {
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
