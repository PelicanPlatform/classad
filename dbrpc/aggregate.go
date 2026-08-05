package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The aggregate spec/result types and the GROUP BY / reduce engine live in the db module
// (db/aggregate.go) so the mutable-table aggregate here and the archive aggregate share one
// implementation. These aliases keep dbrpc's public API and wire encoding unchanged.
type (
	AggFunc  = db.AggFunc  // COUNT/SUM/AVG/MIN/MAX selector
	AggSpec  = db.AggSpec  // one aggregate: a function over an argument attribute
	AggRow   = db.AggRow   // one group's result: group values then aggregate values
	GroupCol = db.GroupCol // one GROUP BY column, optionally time-bucketed
)

const (
	AggCount = db.AggCount // COUNT(*) or COUNT(col)
	AggSum   = db.AggSum
	AggAvg   = db.AggAvg
	AggMin   = db.AggMin
	AggMax   = db.AggMax
	// AggCountDistinct is COUNT(DISTINCT col). It rides the extended opcodes (see
	// anyFiltered), since an older server would not recognize the function.
	AggCountDistinct = db.AggCountDistinct
)

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
	filtered := anyFiltered(aggs)
	build := func(id uint64) []byte {
		// A filtered request goes out on the newer opcode, whose group tuple carries a
		// bucket width; an unfiltered one keeps the original frame byte for byte.
		if filtered {
			b := putStr(putStr(req(id, opAggregateFiltered), table), constraint)
			b = putI32(b, int32(len(groupBy)))
			for _, g := range groupBy {
				b = putU64(putStr(b, g), 0)
			}
			return putAggSpecs(b, aggs, true)
		}
		b := putStr(putStr(req(id, opAggregate), table), constraint)
		b = putI32(b, int32(len(groupBy)))
		for _, g := range groupBy {
			b = putStr(b, g)
		}
		return putAggSpecs(b, aggs, false)
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
			case stBadReq:
				// Only reachable for a filtered request: a server too old to know
				// opAggregateFiltered refuses the opcode. There is deliberately no
				// fallback -- retrying without the filters would answer a different
				// question and report it as success.
				if filtered {
					return nil, ErrExtendedAggregateUnsupported
				}
				return out, fmt.Errorf("dbrpc: aggregate rejected as a bad request")
			}
		}
	}
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
	filtered := anyFiltered(aggs)
	build := func(id uint64) []byte {
		o := opAggregateBucketed
		if filtered {
			o = opAggregateFiltered // same request shape, specs carry a filter
		}
		b := putStr(putStr(req(id, o), table), constraint)
		b = putI32(b, int32(len(groups)))
		for _, g := range groups {
			b = putU64(putStr(b, g.Attr), uint64(g.BucketWidth))
		}
		return putAggSpecs(b, aggs, filtered)
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
				// A server too old to know the opcode rejects it as a bad request. The
				// caller can fall back to client-side bucketing -- but NOT for a filtered
				// request, where dropping the filters would change the answer.
				if filtered {
					return nil, ErrExtendedAggregateUnsupported
				}
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
	aggs, ok := readAggSpecs(r, reqID, write, false)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, write)
}

// streamAggregateBucketed is streamAggregate where each group column may carry a
// bucket width (opAggregateBucketed).
func (s *Server) streamAggregateBucketed(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, false, write)
}

// streamAggregateFiltered is streamAggregateBucketed whose specs carry a per-aggregate
// filter (opAggregateFiltered). The request shape is otherwise identical, so a filtered
// plain aggregate rides the bucketed frame with every width zero.
func (s *Server) streamAggregateFiltered(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, true, write)
}

func (s *Server) streamAggregateWidths(ctx context.Context, reqID uint64, r *reader, includePrivate, filtered bool, write func([]byte)) {
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
	aggs, ok := readAggSpecs(r, reqID, write, filtered)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, write)
}

// readAggSpecs reads the [nAgg]{[func u8][arg]} tail shared by the aggregate opcodes,
// writing respBad and returning ok=false on a malformed frame. filtered selects the
// [func u8][arg][filter] form the *Filtered opcodes carry.
func readAggSpecs(r *reader, reqID uint64, write func([]byte), filtered bool) ([]AggSpec, bool) {
	nAgg := int(r.i32())
	if nAgg < 0 || nAgg > 1024 {
		write(respBad(reqID))
		return nil, false
	}
	aggs := make([]AggSpec, nAgg)
	for i := range aggs {
		aggs[i] = AggSpec{Func: AggFunc(r.u8()), Arg: r.str()}
		if filtered {
			aggs[i].Filter = r.str()
		}
	}
	if r.err != nil {
		write(respBad(reqID))
		return nil, false
	}
	return aggs, true
}

// anyFiltered reports whether some spec needs the extended opcodes: a per-aggregate filter,
// or a function an older server would not recognize. Without one the request goes out on the
// original opcode, so an older server keeps serving it.
//
// COUNT DISTINCT is gated for the same reason a filter is. The function selector is a u8, so
// an old server would decode the frame happily, fail to match the unknown function, and
// return "undefined" for that column -- visibly odd rather than plausibly wrong, but still
// not an error. Refusing the opcode says what actually happened.
func anyFiltered(aggs []AggSpec) bool {
	for _, a := range aggs {
		if a.Filter != "" || a.Func > AggMax {
			return true
		}
	}
	return false
}

// putAggSpecs writes the [nAgg]{[func u8][arg]([filter])} tail.
func putAggSpecs(b []byte, aggs []AggSpec, filtered bool) []byte {
	b = putI32(b, int32(len(aggs)))
	for _, a := range aggs {
		b = putStr(putU8(b, byte(a.Func)), a.Arg)
		if filtered {
			b = putStr(b, a.Filter)
		}
	}
	return b
}

// ErrExtendedAggregateUnsupported is returned when the server is too old to know the
// extended-aggregate opcodes -- the ones carrying a per-aggregate FILTER or a function it
// would not recognize, such as COUNT DISTINCT. The caller must NOT retry without them: the
// answer would be a different aggregate, which is wrong rather than merely slower.
var ErrExtendedAggregateUnsupported = errors.New(
	"dbrpc: server does not support per-aggregate FILTER or COUNT DISTINCT")

// ErrFilteredAggregateUnsupported is the former name of ErrExtendedAggregateUnsupported,
// from when a filter was the only thing the extended opcodes carried. It is the same error
// value, so errors.Is against either still matches.
var ErrFilteredAggregateUnsupported = ErrExtendedAggregateUnsupported

// aggregate is the shared GROUP BY core for both aggregate opcodes: it refuses
// private attributes for an unprivileged connection, projects only the attributes
// the aggregation reads (so the scan stays wire-native), reduces via the db module's
// shared aggregate engine, and streams one frame per group.
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
			// A filter reads attributes too, and its count would leak them.
			for _, ref := range db.AggFilterAttrs(a.Filter) {
				if classad.IsPrivateAttribute(ref) {
					write(respErr(reqID, "cannot filter on private attribute "+ref))
					return
				}
			}
		}
	}

	// Project only the attributes the aggregation reads (group columns + the
	// non-"*" aggregate arguments), so the scan reads them wire-native instead of
	// fully decoding every matching ad.
	attrs, groupCol, aggCol := db.AggProjection(groupCols, aggs)
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}

	// Fast path: an unconstrained COUNT(*) with no grouping is the collection's live row
	// count, which it tracks in O(shards) via Len() -- no scan of every ad. This is the common
	// `SELECT COUNT(*) FROM <table>`. Only when the collection has no hidden structural
	// (parent-only) ads, which a match-all scan excludes but Len() counts (a chained job_queue
	// has them; a flat table like Startd does not). Mirrors ArchiveTable.Aggregate.
	if len(groupCols) == 0 && len(aggs) == 1 && aggs[0].Func == AggCount && aggs[0].Arg == "*" &&
		aggs[0].Filter == "" && db.IsMatchAll(constraint) && !d.Chained() {
		frame := respHead(reqID, stStream)
		frame = putStr(frame, strconv.Itoa(d.Len()))
		write(frame)
		write(respHead(reqID, stStreamEnd))
		return
	}

	// Fast path: a constrained COUNT(*) WHERE <numeric comparison on one int schema field> runs
	// as the adschema columnar scan when the table has schema-scan enabled and the predicate is
	// eligible. CountConstraint returns ok=false otherwise (schema-scan off, or a predicate the
	// columnar path can't handle), and we fall through to the projected scan.
	if len(groupCols) == 0 && len(aggs) == 1 && aggs[0].Func == AggCount && aggs[0].Arg == "*" &&
		aggs[0].Filter == "" && !db.IsMatchAll(constraint) {
		if n, ok := d.CountConstraint(constraint); ok {
			frame := respHead(reqID, stStream)
			frame = putStr(frame, strconv.Itoa(n))
			write(frame)
			write(respHead(reqID, stStreamEnd))
			return
		}
	}

	seq, err := d.QueryProject(constraint, attrs)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	rows, err := db.AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, func() bool { return cancelled(ctx) })
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	if cancelled(ctx) {
		return // client gone mid-scan: nothing left to stream
	}
	for _, row := range rows {
		frame := respHead(reqID, stStream)
		for _, gv := range row.Group {
			frame = putStr(frame, gv)
		}
		for _, v := range row.Values {
			frame = putStr(frame, v)
		}
		write(frame)
	}
	write(respHead(reqID, stStreamEnd))
}
