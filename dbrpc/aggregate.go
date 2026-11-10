package dbrpc

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
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
	// AggApproxCountDistinct is COUNT(DISTINCT col) answered from a per-group HyperLogLog:
	// an ESTIMATE, within db.ApproxDistinctRelErr of the count as a standard deviation. It
	// rides the extended opcodes for the same reason AggCountDistinct does, so a server that
	// does not know it refuses rather than answering the column with something else.
	AggApproxCountDistinct = db.AggApproxCountDistinct
)

// Aggregate runs a server-side GROUP BY: the server buckets the constraint match
// by the group-by column tuple in a hash map and returns one AggRow per group.
// With no group columns it returns a single row aggregating the whole match. The
// aggregation happens on the server, so only the (small) grouped result crosses
// the wire, not every matched ad.
//
// A spec may name a USER-DEFINED aggregate (db.AggUser plus db.AggSpec.UserFunc). It is
// resolved in the SERVER's process, against what that server registered with
// db.RegisterAggregate -- registering it in this process registers it for queries this
// process answers, which is a different thing. A name the server does not know fails the
// query with db.ErrNoSuchAggregate; a server too old to carry a name at all refuses with
// ErrNamedAggregateUnsupported. Neither is ever answered as an undefined column.
func (c *Client) Aggregate(ctx context.Context, constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	return c.AggregateTable(ctx, DefaultTable, constraint, groupBy, aggs)
}

// AggregateTable is Aggregate on the named table.
func (c *Client) AggregateTable(ctx context.Context, table, constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	form := aggSpecFormFor(aggs)
	if err := c.refuseUnservableAggregate(aggs, form); err != nil {
		return nil, err
	}
	// A server that can send typed group cells is asked to: the reply then carries the
	// groups as values and nothing parses a label. The typed opcode's frame is the NAMED
	// one, so asking for it widens the request form to match.
	typed := c.SupportsNewer(FeatTypedGroups)
	if typed {
		form = aggSpecNamed
	}
	build := func(id uint64) []byte {
		// A request naming a user-defined aggregate goes out on the named opcode, a filtered
		// one on the extended opcode, and each of those group tuples carries a bucket width;
		// a request neither names nor filters keeps the original frame byte for byte.
		if form >= aggSpecFiltered {
			o := opAggregateFiltered
			if form >= aggSpecNamed {
				o = opAggregateNamed
			}
			if typed {
				o = opAggregateTyped
			}
			b := putStr(putStr(req(id, o), table), constraint)
			b = putI32(b, int32(len(groupBy)))
			for _, g := range groupBy {
				b = putU64(putStr(b, g), 0)
			}
			return putAggSpecs(b, aggs, form)
		}
		b := putStr(putStr(req(id, opAggregate), table), constraint)
		b = putI32(b, int32(len(groupBy)))
		for _, g := range groupBy {
			b = putStr(b, g)
		}
		return putAggSpecs(b, aggs, form)
	}
	_, frames, strm, err := c.callStream(build)
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
				return out, c.streamEnd(strm)
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return out, errShort
			}
			switch status {
			case stStream:
				out = append(out, readAggRow(body, len(groupBy), len(aggs), typed))
			case stErr:
				return out, statusErr(status, body)
			case stBadReq:
				// Only reachable for an extended request: a server too old to know
				// opAggregateFiltered (or opAggregateNamed) refuses the opcode. There is
				// deliberately no fallback -- retrying without the filters, or without the
				// aggregate's name, would answer a different question and report it as
				// success.
				if form >= aggSpecNamed {
					return nil, ErrNamedAggregateUnsupported
				}
				if form >= aggSpecFiltered {
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
	form := aggSpecFormFor(aggs)
	if err := c.refuseUnservableAggregate(aggs, form); err != nil {
		return nil, err
	}
	// See AggregateTable: a server that can type the group cells is asked to, which widens
	// the request to the named frame the typed opcode carries.
	typed := c.SupportsNewer(FeatTypedGroups)
	if typed {
		form = aggSpecNamed
	}
	build := func(id uint64) []byte {
		o := opAggregateBucketed
		switch {
		case typed:
			o = opAggregateTyped // the named request shape, with typed group cells back
		case form >= aggSpecNamed:
			o = opAggregateNamed // same request shape, specs carry a filter and a name
		case form >= aggSpecFiltered:
			o = opAggregateFiltered // same request shape, specs carry a filter
		}
		b := putStr(putStr(req(id, o), table), constraint)
		b = putI32(b, int32(len(groups)))
		for _, g := range groups {
			b = putU64(putStr(b, g.Attr), uint64(g.BucketWidth))
		}
		return putAggSpecs(b, aggs, form)
	}
	_, frames, strm, err := c.callStream(build)
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
				return out, c.streamEnd(strm)
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return out, errShort
			}
			switch status {
			case stStream:
				out = append(out, readAggRow(body, len(groups), len(aggs), typed))
			case stErr:
				return out, statusErr(status, body)
			case stBadReq:
				// A server too old to know the opcode rejects it as a bad request. The
				// caller can fall back to client-side bucketing -- but NOT for a filtered
				// or named request, where dropping the filters or the aggregate's name
				// would change the answer.
				if form >= aggSpecNamed {
					return nil, ErrNamedAggregateUnsupported
				}
				if form >= aggSpecFiltered {
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
func (s *Server) streamAggregate(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	const typed = false
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
	aggs, ok := readAggSpecs(r, reqID, write, aggSpecBase)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, typed, write, qlog)
}

// streamAggregateBucketed is streamAggregate where each group column may carry a
// bucket width (opAggregateBucketed).
func (s *Server) streamAggregateBucketed(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, aggSpecBase, false, write, qlog)
}

// streamAggregateFiltered is streamAggregateBucketed whose specs carry a per-aggregate
// filter (opAggregateFiltered). The request shape is otherwise identical, so a filtered
// plain aggregate rides the bucketed frame with every width zero.
func (s *Server) streamAggregateFiltered(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, aggSpecFiltered, false, write, qlog)
}

// streamAggregateNamed is streamAggregateFiltered whose specs also carry the NAME of a
// user-defined aggregate (opAggregateNamed). The request shape is otherwise identical, so a
// named aggregate composes with bucketing and filtering in one request. The name is resolved
// against this process's registry (db.RegisterAggregate) by the reduction engine, which fails
// the query for one it does not know rather than answering that column undefined.
func (s *Server) streamAggregateNamed(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, aggSpecNamed, false, write, qlog)
}

// streamAggregateTyped is streamAggregateNamed whose reply carries the group cells as values
// (opAggregateTyped). The request frame is the Named one, so only the reply differs.
func (s *Server) streamAggregateTyped(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamAggregateWidths(ctx, reqID, r, includePrivate, aggSpecNamed, true, write, qlog)
}

func (s *Server) streamAggregateWidths(ctx context.Context, reqID uint64, r *reader, includePrivate bool, form aggSpecForm, typed bool, write func([]byte), qlog func(QueryLog)) {
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
	aggs, ok := readAggSpecs(r, reqID, write, form)
	if !ok {
		return
	}
	s.aggregate(ctx, reqID, table, constraint, groups, aggs, includePrivate, typed, write, qlog)
}

// aggSpecForm selects which of the three aggregate-spec tails a frame carries. It is one
// value rather than a pair of booleans so that the encoder and the decoder cannot disagree
// about a combination that does not exist: a named frame always carries filters too, because
// the named opcodes are the extended frame plus a name, and nothing writes a name without one.
type aggSpecForm int

const (
	aggSpecBase     aggSpecForm = iota // [func u8][arg]            -- opAggregate, opArchiveAggregate
	aggSpecFiltered                    // [func u8][arg][filter]    -- the *Filtered opcodes
	aggSpecNamed                       // [func u8][arg][filter][name] -- the *Named opcodes
)

// readAggSpecs reads the [nAgg]{[func u8][arg]} tail shared by the aggregate opcodes,
// writing respBad and returning ok=false on a malformed frame. form selects the wider tails
// the *Filtered and *Named opcodes carry.
func readAggSpecs(r *reader, reqID uint64, write func([]byte), form aggSpecForm) ([]AggSpec, bool) {
	nAgg := int(r.i32())
	if nAgg < 0 || nAgg > 1024 {
		write(respBad(reqID))
		return nil, false
	}
	aggs := make([]AggSpec, nAgg)
	for i := range aggs {
		aggs[i] = AggSpec{Func: AggFunc(r.u8()), Arg: r.str()}
		if form >= aggSpecFiltered {
			aggs[i].Filter = r.str()
		}
		if form >= aggSpecNamed {
			// Empty for every built-in, and the aggregate's registered name for a
			// user-defined one. The server resolves it against its own registry; it is not
			// read as anything else, so a name sent beside a built-in selector is ignored
			// rather than refused.
			aggs[i].UserFunc = r.str()
		}
		// Only the named frame has a field to put an aggregate's name in. On the older
		// frames a selector asking for a user-defined one cannot be satisfied by anything
		// this reader produces -- it would reach the engine as a lookup of the empty name --
		// and the same goes for a named frame that left the name blank. That is a malformed
		// frame rather than a query that fails late, and a client built against this package
		// sends a named aggregate only on an opcode that can carry its name.
		if aggs[i].Func == db.AggUser && aggs[i].UserFunc == "" {
			write(respBad(reqID))
			return nil, false
		}
	}
	if r.err != nil {
		write(respBad(reqID))
		return nil, false
	}
	return aggs, true
}

// ErrNamedAggregateUnsupported is returned when a request names a user-defined aggregate
// (db.RegisterAggregate, sent as db.AggUser plus db.AggSpec.UserFunc) and the server is too
// old to know the opcode that carries the name.
//
// There is deliberately no fallback. A server that does not know the opcode refuses it before
// any row; retrying on an older opcode would drop the name and ask for a selector the server
// does not recognize, which it answers "undefined" rather than refusing -- a different
// aggregate reported as success.
var ErrNamedAggregateUnsupported = errors.New(
	"dbrpc: server does not support user-defined (named) aggregates")

// ErrUserAggregateUnsupported is the former name of ErrNamedAggregateUnsupported, from when no
// opcode here had a field to put an aggregate's NAME in and the client refused to send one at
// all. The name now crosses, so the only reason a user-defined aggregate still fails for want
// of protocol is a server too old to know the opcode that carries it -- which is the closest
// thing to what this meant, and is the same error value, so errors.Is against either matches.
//
// Deprecated: use ErrNamedAggregateUnsupported, which says which side is too old.
var ErrUserAggregateUnsupported = ErrNamedAggregateUnsupported

// refuseUnservableAggregate stops a named request that cannot be served, before it costs a round
// trip. A peer that advertised its features and did not name this one is a definite no, so
// issuing the request to find out would be a wasted trip to a refusal we already have. A peer
// that advertised NOTHING is unknown, not no -- it may be a build that predates advertisement
// entirely -- so that one is still probed, and the stBadReq it may answer with maps to the
// same error.
func (c *Client) refuseUnservableAggregate(aggs []AggSpec, form aggSpecForm) error {
	// A list-valued aggregate reaches only opJoinAgg, whose frame has a field for the cap
	// that bounds it. Refused here rather than sent, because the function selector is a u8
	// and a server that does not know this one answers that column "undefined" -- an
	// unbounded request would look like a successful empty one. See db.AggList.
	for _, a := range aggs {
		if a.Func == db.AggList {
			return fmt.Errorf("%w: over a single table", db.ErrListAggregateNeedsBound)
		}
	}
	if form < aggSpecNamed {
		return nil
	}
	// A spec asking for a user-defined aggregate without naming one has nothing to resolve.
	// Caught here rather than left to the server, which can only answer it as a malformed
	// frame -- indistinguishable, to the client, from a server too old to know the opcode.
	for _, a := range aggs {
		if a.Func == db.AggUser && a.UserFunc == "" {
			return fmt.Errorf("dbrpc: a user-defined aggregate over %q names no aggregate", a.Arg)
		}
	}
	if c.Supports(FeatNamedAggregate) == SupportNo {
		return fmt.Errorf("%w: the server advertised its features and this was not among them",
			ErrNamedAggregateUnsupported)
	}
	return nil
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

// anyNamed reports whether some spec names a user-defined aggregate, which is what routes a
// request onto the *Named opcodes. It implies anyFiltered -- db.AggUser sits past AggMax -- so
// a named request is an extended one carrying one more field, never a narrower frame.
func anyNamed(aggs []AggSpec) bool {
	for _, a := range aggs {
		if a.Func == db.AggUser {
			return true
		}
	}
	return false
}

// aggSpecFormFor picks the narrowest tail that expresses aggs, so a request stays
// byte-identical to what an older client would have sent whenever it can.
func aggSpecFormFor(aggs []AggSpec) aggSpecForm {
	switch {
	case anyNamed(aggs):
		return aggSpecNamed
	case anyFiltered(aggs):
		return aggSpecFiltered
	}
	return aggSpecBase
}

// putAggSpecs writes the [nAgg]{[func u8][arg]([filter]([name]))} tail.
func putAggSpecs(b []byte, aggs []AggSpec, form aggSpecForm) []byte {
	b = putI32(b, int32(len(aggs)))
	for _, a := range aggs {
		b = putStr(putU8(b, byte(a.Func)), a.Arg)
		if form >= aggSpecFiltered {
			b = putStr(b, a.Filter)
		}
		if form >= aggSpecNamed {
			b = putStr(b, a.UserFunc)
		}
	}
	return b
}

// noSuchAggregatePrefix opens the message of a query naming an aggregate the server has no
// registration for, which the client maps back to db.ErrNoSuchAggregate. A sentinel does not
// survive the wire, and this one has to: "no such aggregate" is a client's own mistake, fixed
// by registering the aggregate on the server or by spelling the name the way it was
// registered, and it must be told apart from the server failing for any other reason.
const noSuchAggregatePrefix = "no such aggregate: "

// aggErrMsg is the message an aggregate's failure carries to the client, tagged when it is an
// unknown aggregate name (the client cannot unwrap a message, only read its prefix).
func aggErrMsg(err error) string {
	if errors.Is(err, db.ErrNoSuchAggregate) {
		return noSuchAggregatePrefix + err.Error()
	}
	return err.Error()
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
func (s *Server) aggregate(ctx context.Context, reqID uint64, table, constraint string, groupCols []GroupCol, aggs []AggSpec, includePrivate, typed bool, write func([]byte), qlog func(QueryLog)) {
	// The observed query is recorded when the handler returns, however it returns: a
	// refused private attribute and a cancelled scan cost the server time too, and a
	// shape that always fails is one an operator wants to see rather than one the report
	// should hide. op and the row count are filled in as they become known.
	start := time.Now()
	obs := QueryLog{Op: "Aggregate", Table: table, Constraint: constraint, Groups: groupCols, Aggs: aggs}
	// Scan stats are collected whenever the server is aggregating queries, as on the
	// projection path: the aggregate ladder's cheap rungs answer without reading a record
	// and leave the counts zero, so a large RecordsReassembled here is precisely the
	// signature of the aggregate that fell through to the scan.
	var stats *collections.ScanStats
	if s.queryStatsOn() {
		stats = &collections.ScanStats{}
	}
	if qlog != nil {
		defer func() {
			if stats != nil {
				obs.Scan = *stats
			}
			obs.Duration = time.Since(start)
			qlog(obs)
		}()
	}
	if !includePrivate {
		// The WHERE clause reads attributes too, and whether a row matches is itself the answer.
		if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
			return
		}
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
			// A filter reads attributes too, and its count would leak them. Checked with the
			// authorization walker, not AggFilterAttrs: that is ReadAttrs-based and reports nothing
			// for a SCOPED reference, so `FILTER "MY.ClaimId == \"x\""` walked straight through.
			if ref, dynamic := db.PrivateConstraintRef(a.Filter); ref != "" {
				write(respErr(reqID, "cannot filter on private attribute "+ref))
				return
			} else if dynamic {
				write(respErr(reqID, "cannot use a dynamic attribute reference in a filter"))
				return
			}
		}
	}

	// A name resolves to whatever table it names, archives included.
	//
	// The read opcodes already work this way -- QueryRawProject on an
	// archive answers from the archive (see streamQueryRawProject) -- so
	// a caller who has been reading a history table by name reasonably
	// expects to aggregate it by name too. Before this, that one call
	// answered "no such table" while every other read of the same name
	// succeeded, which is not a difference anything in the API surface
	// suggests. It cost a downstream caller a feature that failed only
	// in production: the tests built a mutable table of the same name,
	// where the call works.
	//
	// The archive engine reduces through the same db aggregate code as
	// the mutable path, so the answer is identical -- this is a
	// dispatch fix, not a second implementation.
	ref := s.resolveTable(table)
	switch ref.kind {
	case kindTable, kindView:
	case kindArchive:
		// Named for the engine that answered rather than for the opcode the client sent:
		// a name resolving to an archive is reduced by the archive ladder, and its cost
		// has nothing in common with the mutable one below.
		obs.Op = "ArchiveAggregate"
		// The archive ladder takes no stop callback, so this reduction runs to completion
		// even once the client has gone -- unlike the mutable one below.
		archAggregate := ref.arch.AggregateColsStats
		if !includePrivate {
			archAggregate = ref.arch.AggregateColsStatsRedacted // see the mutable branch below
		}
		rows, aerr := archAggregate(constraint, groupCols, aggs, stats)
		if aerr != nil {
			write(respErr(reqID, aggErrMsg(aerr)))
			return
		}
		obs.Rows = len(rows)
		// writeAggRows terminates the stream itself.
		writeAggRows(reqID, rows, typed, write)
		return
	default:
		write(respErr(reqID, "no such table: "+table))
		return
	}
	d := ref.db

	// The aggregate fan-out -- the Len()/CountConstraint/columnar/grouped-columnar fast paths and
	// the projected-scan fall-through -- lives in db alongside the archive's, so the two ladders
	// stay one implementation and an embedded caller of db gets the same planning this handler
	// does. What stays here is the RPC layer's own job: table resolution, the private-attribute
	// gates above, and framing. The stop callback abandons a scan whose client has gone away.
	//
	// The gates above refuse private attributes by NAME. An unprivileged session also aggregates with no
	// key, so a sealed value that is not private by name -- or was sealed under an earlier policy -- is
	// undefined to the WHERE clause, the groups, the aggregates and their filters rather than evaluated.
	//
	// A continuous aggregate that has sealed holds its rows in two places, and an aggregate
	// over one of them is not an aggregate over the view. The view reduces the union in a
	// single pass, because AVG and COUNT(DISTINCT) cannot be combined from per-half results.
	// A view is built with no key for every reader, so its rows serve both alike.
	aggregate := d.AggregateColsStats
	if !includePrivate {
		aggregate = d.AggregateColsStatsRedacted
	}
	if ref.seals() && ref.view != nil {
		aggregate = ref.view.AggregateColsStats
	}
	rows, err := aggregate(constraint, groupCols, aggs, stats, func() bool { return cancelled(ctx) })
	if err != nil {
		write(respErr(reqID, aggErrMsg(err)))
		return
	}
	obs.Rows = len(rows)
	if cancelled(ctx) {
		return // client gone mid-scan: nothing left to stream
	}
	writeAggRows(reqID, rows, typed, write)
}

// isScalarGroup reports whether a group value is one the value codec carries AS a value. A
// list or a nested ad is not: see appendAggRow for why those cross as their group text.
func isScalarGroup(v classad.Value) bool {
	switch v.Type() {
	case classad.ListValue, classad.ClassAdValue:
		return false
	}
	return true
}

// readAggRow decodes one streamed aggregate row. typed says the group cells are tagged values
// (the server answered opAggregateTyped or its archive twin) rather than rendered text.
//
// Either way the row comes back with BOTH forms filled: Group is the group's identity and its
// label, GroupValues is the same group in types. Against a server without FeatTypedGroups the
// values are read back out of the text with db.ParseGroupValue, so a caller never has to ask
// which kind of server answered, and never has to parse a label itself.
func readAggRow(body *reader, nGroup, nAgg int, typed bool) AggRow {
	row := AggRow{
		Group:       make([]string, nGroup),
		GroupValues: make([]classad.Value, nGroup),
		Values:      make([]string, nAgg),
	}
	for i := range row.Group {
		if typed {
			v, nonScalar, ok := readExcValue(body)
			if !ok {
				// A tag this build does not know. The reader is now mid-cell and the
				// frame cannot be trusted further, which body.err reports to the caller.
				return row
			}
			if nonScalar {
				// excExpr: the cell holds the group's TEXT, because the group is a list
				// or a nested ad, which the value codec has no scalar form for (see
				// appendAggRow). The label is that text as it stands; the VALUE is that
				// text read back, which is exact because a group's text is a ClassAd
				// literal. Reading it here rather than leaving the string in place is
				// what keeps GroupValues the group's value for every kind.
				text, _ := v.StringValue() //nolint:errcheck // excExpr decodes to a string value, which readExcValue guarantees
				row.Group[i] = text
				row.GroupValues[i] = db.ParseGroupValue(text)
				continue
			}
			row.GroupValues[i] = v
			row.Group[i] = db.GroupValueText(v)
			continue
		}
		row.Group[i] = body.str()
		row.GroupValues[i] = db.ParseGroupValue(row.Group[i])
	}
	for i := range row.Values {
		row.Values[i] = body.str()
	}
	return row
}

// writeAggRows streams aggregate rows and the stream terminator. Shared so a fast path that
// computed its rows without scanning emits exactly what the scanning path does.
func writeAggRows(reqID uint64, rows []db.AggRow, typed bool, write func([]byte)) {
	// One buffer for every row rather than one per row. write hands the bytes to
	// conn.WriteMsg and returns, retaining nothing, which is what lets the scan's value
	// frames reuse a buffer the same way; the header is identical on each row, so only the
	// body past it is rewritten.
	frame := respHead(reqID, stStream)
	head := len(frame)
	for _, row := range rows {
		frame = appendAggRow(frame[:head], row, typed)
		write(frame)
	}
	write(respHead(reqID, stStreamEnd))
}

// appendAggRow writes one aggregate row: the group cells, then the aggregate cells. typed
// selects the group encoding -- tagged values for opAggregateTyped and its archive twin,
// rendered text for every older opcode. The aggregate cells are text either way.
//
// A LIST OR A NESTED AD CROSSES AS ITS GROUP TEXT, in a string cell tagged excExpr, because
// the value codec has no form that keeps such a value whole -- appendExcValue would send the
// unparser's text for it, and the unparser is not this renderer (it keeps a literal's
// unevaluated source and writes reals in sink form, so one group could cross under two names).
// Sending the group text instead costs nothing: a group's text is a ClassAd literal, so the
// client reads the value back out of it exactly, and the label it shows is byte-identical to
// the one an older server sends for the same group.
//
// The typed form falls back to the row's TEXT for any group column the producer left without
// a value. A producer that fills GroupValues fills all of them, so this is reachable only
// from one that fills none -- and a group is better sent as the text that names it than as a
// cell the client would read as undefined.
func appendAggRow(frame []byte, row db.AggRow, typed bool) []byte {
	for i, gv := range row.Group {
		if typed {
			if i < len(row.GroupValues) && isScalarGroup(row.GroupValues[i]) {
				frame = appendExcValue(frame, row.GroupValues[i])
			} else {
				// excExpr, the codec's existing "text, not a value" tag, so the client
				// can tell this cell from a string group whose value happens to spell
				// the same thing. appendExcValue would write the value's UNPARSE here;
				// the group text is what is wanted, so the tag is written directly.
				frame = putStr(append(frame, excExpr), gv)
			}
			continue
		}
		frame = putStr(frame, gv)
	}
	for _, v := range row.Values {
		frame = putStr(frame, v)
	}
	return frame
}
