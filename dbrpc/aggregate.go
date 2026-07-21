package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"math"
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
func (c *Client) Aggregate(ctx context.Context, constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	return c.AggregateTable(ctx, DefaultTable, constraint, groupBy, aggs)
}

// AggregateTable is Aggregate on the named table.
func (c *Client) AggregateTable(ctx context.Context, table, constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	build := func(id uint64) []byte {
		b := putStr(putStr(req(id, opAggregate), table), constraint)
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
	for {
		select {
		case <-ctx.Done():
			drain(frames)
			return out, ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return out, nil
			}
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
	}
}

// GroupCol is one GROUP BY column for a (possibly bucketed) aggregate: the
// attribute Attr, optionally floored into fixed-width buckets. BucketWidth == 0
// groups by the raw attribute value; BucketWidth > 0 (seconds) groups by the
// epoch-aligned bucket floor(number(Attr)/BucketWidth)*BucketWidth, and a row whose
// Attr is not a finite number drops out of the result. This is the shared shape a
// bucketed aggregate (here) and a bucketed materialized view can both group by.
type GroupCol struct {
	Attr        string
	BucketWidth int64
}

// ErrBucketedUnsupported is returned by AggregateBucketed against a server too old
// to implement the opcode (it rejects the request), so a caller can fall back to
// client-side bucketing.
var ErrBucketedUnsupported = errors.New("dbrpc: server does not support bucketed aggregation")

// AggregateBucketed is Aggregate with group columns that may be time-bucketed (see
// GroupCol), pushing the bucketing to the server so only the grouped rows cross the
// wire. Against a server that does not implement the opcode it returns an error that
// wraps ErrBucketedUnsupported.
func (c *Client) AggregateBucketed(ctx context.Context, constraint string, groups []GroupCol, aggs []AggSpec) ([]AggRow, error) {
	return c.AggregateBucketedTable(ctx, DefaultTable, constraint, groups, aggs)
}

// AggregateBucketedTable is AggregateBucketed on the named table.
func (c *Client) AggregateBucketedTable(ctx context.Context, table, constraint string, groups []GroupCol, aggs []AggSpec) ([]AggRow, error) {
	build := func(id uint64) []byte {
		b := putStr(putStr(req(id, opAggregateBucketed), table), constraint)
		b = putI32(b, int32(len(groups)))
		for _, g := range groups {
			b = putU64(putStr(b, g.Attr), uint64(g.BucketWidth))
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
	for {
		select {
		case <-ctx.Done():
			drain(frames)
			return out, ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return out, nil
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return out, errShort
			}
			switch status {
			case stStream:
				row := AggRow{Group: make([]string, len(groups)), Values: make([]string, len(aggs))}
				for i := range row.Group {
					row.Group[i] = body.str()
				}
				for i := range row.Values {
					row.Values[i] = body.str()
				}
				out = append(out, row)
			case stErr:
				return out, statusErr(status, body)
			case stBadReq:
				// A server too old to know opAggregateBucketed rejects it as a bad
				// request; signal the caller to fall back to client-side bucketing.
				return nil, ErrBucketedUnsupported
			default:
				return out, fmt.Errorf("dbrpc: unexpected aggregate status %d", status)
			}
		}
	}
}

// streamAggregate performs a server-side GROUP BY (raw group columns) and streams
// one frame per group.
func (s *Server) streamAggregate(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	table := r.str()
	constraint := r.str()
	nGroup := int(r.i32())
	if nGroup < 0 || nGroup > 1024 {
		write(respBad(reqID))
		return
	}
	groups := make([]GroupCol, nGroup)
	for i := range groups {
		groups[i] = GroupCol{Attr: r.str()}
	}
	aggs, ok := readAggSpecs(r, reqID, write)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, write)
}

// streamAggregateBucketed is streamAggregate where each group column may carry a
// bucket width (opAggregateBucketed).
func (s *Server) streamAggregateBucketed(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	table := r.str()
	constraint := r.str()
	nGroup := int(r.i32())
	if nGroup < 0 || nGroup > 1024 {
		write(respBad(reqID))
		return
	}
	groups := make([]GroupCol, nGroup)
	for i := range groups {
		attr := r.str()
		groups[i] = GroupCol{Attr: attr, BucketWidth: int64(r.u64())}
	}
	aggs, ok := readAggSpecs(r, reqID, write)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, write)
}

// readAggSpecs reads the [nAgg]{[func u8][arg]} tail shared by both aggregate
// opcodes, writing respBad and returning ok=false on a malformed frame.
func readAggSpecs(r *reader, reqID uint64, write func([]byte)) ([]AggSpec, bool) {
	nAgg := int(r.i32())
	if nAgg < 0 || nAgg > 1024 {
		write(respBad(reqID))
		return nil, false
	}
	aggs := make([]AggSpec, nAgg)
	for i := range aggs {
		aggs[i] = AggSpec{Func: AggFunc(r.u8()), Arg: r.str()}
	}
	if r.err != nil {
		write(respBad(reqID))
		return nil, false
	}
	return aggs, true
}

// aggregate is the shared GROUP BY core for both aggregate opcodes: it refuses
// private attributes for an unprivileged connection, scans the projected columns,
// buckets by the (possibly time-bucketed) group tuple, and streams one frame per
// group.
func (s *Server) aggregate(ctx context.Context, reqID uint64, table, constraint string, groupCols []GroupCol, aggs []AggSpec, includePrivate bool, write func([]byte)) {
	if !includePrivate {
		for _, g := range groupCols {
			if classad.IsPrivateAttribute(g.Attr) {
				write(respErr(reqID, "cannot group by private attribute "+g.Attr))
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
	nGroup := len(groupCols)
	attrs, groupCol, aggCol := projectionFor(groupCols, aggs)
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	seq, err := d.QueryProject(constraint, attrs)
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
		if cancelled(ctx) {
			return // client gone: stop the scan
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
				scratch[i] = valueText(vals[groupCol[i]])
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
		gs := &groupState{accs: make([]aggAcc, len(aggs))}
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
func projectionFor(groupCols []GroupCol, aggs []AggSpec) (attrs []string, groupCol, aggCol []int) {
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
