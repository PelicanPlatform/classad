package dbrpc

// Reducing a join's rows before they cross the wire.
//
// db.Catalog.JoinAggregate has existed for some time and reached no network caller: there was
// no opcode, so a client wanting one row per job out of a history join had to ask for one row
// per RUN ATTEMPT and reduce them itself. That is the work the reduction exists to avoid, done
// in the one place where it also costs a network.
//
// What it costs to leave it undone, measured by the caller that asked for this: a page joining
// history to epoch_history to show how one job's requested memory moved across its attempts
// timed the join at 5.25 s / 69.3 MiB against 1.52-1.61 s / 47.3 MiB for two plain reads
// paired at the far end. The join LOST, on every shape, because it ships one row per attempt
// where the caller wants one row per job. Nothing about the join was slow; the reply was the
// wrong shape.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/PelicanPlatform/classad/db"
)

// ErrJoinAggUnsupported reports that the server cannot reduce a join, so the caller should
// take the join's rows (or the two sides) and reduce them itself. Four causes, one response: a
// server too old to know opJoinAgg, a server whose catalog has no reduce engine wired to the
// join, a chain sent to a server whose join is two tables, and a client that already knows
// from the handshake that the server lacks the feature and so sends nothing at all.
//
// It is delivered BEFORE any group, always: a reduction is one answer, so there is no partial
// result it can arrive after. That is the one way this is easier than the row-streaming join,
// where an error can follow rows that were already yielded.
var ErrJoinAggUnsupported = errors.New("dbrpc: the server does not implement a server-side join aggregate")

// JoinAggRequest is a planned join together with the reduction over its rows.
//
// Limits is the JOIN's budget, asked for and clamped exactly as JoinRequest's is. The caps on
// the ANSWER -- how many groups, how long a list-valued aggregate's list -- live on the spec
// (db.JoinAggSpec), because they describe the shape of the reply rather than the work.
type JoinAggRequest struct {
	Spec   db.JoinAggSpec
	Limits db.JoinLimits
}

// JoinAggregate runs a join inside the store and returns one row per GROUP.
//
// The groups come back in the order the server produced them, which for the reduce engine is
// first-seen order over the join's rows. A GROUP BY defines no order, so a caller that wants
// one sorts.
//
// Each row's Group and GroupValues are both filled, as the typed single-table aggregate's are,
// and each row's Elided says per aggregate how many values a bounded aggregate left out. A
// caller that asks for no list-valued aggregate never sees a non-nil Elided.
//
// It reports ErrJoinAggUnsupported WITHOUT sending anything when the handshake already says
// the server cannot serve this, and maps the server's own refusal to the same sentinel. A
// caller distinguishes with errors.Is rather than by matching prose.
func (c *Client) JoinAggregate(ctx context.Context, jr JoinAggRequest) ([]db.AggRow, error) {
	if !c.SupportsNewer(FeatJoinAgg) {
		return nil, ErrJoinAggUnsupported
	}
	chain := len(jr.Spec.Join.Steps) > 0
	// A chain rides this opcode under a flag, but it still needs a server whose join folds
	// in a third table, and that is what FeatJoinChain says. Asking for both is what keeps
	// a chain from reaching a two-table server as a request it will refuse after a round
	// trip -- the same check Join and JoinWire make by picking a chain opcode.
	if chain && !c.SupportsNewer(FeatJoinChain) {
		return nil, ErrJoinAggUnsupported
	}
	if err := refuseJoinAggSpec(jr.Spec); err != nil {
		return nil, err
	}
	nGroup, nAgg := len(jr.Spec.GroupCols), len(jr.Spec.Aggs)
	_, frames, strm, err := c.callStream(joinAggFrame(jr, chain))
	if err != nil {
		return nil, err
	}
	var out []db.AggRow
	for {
		select {
		case <-ctx.Done():
			drain(frames)
			return nil, ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				if err := c.streamEnd(strm); err != nil {
					return nil, err
				}
				return out, nil
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return nil, errShort
			}
			switch status {
			case stStream:
				n := int(body.i32())
				for i := 0; i < n; i++ {
					row, ok := readJoinAggRow(body, nGroup, nAgg)
					if !ok {
						return nil, errShort
					}
					out = append(out, row)
				}
			case stBadReq:
				// A server too old to know the opcode refuses this way, and so does one
				// whose catalog cannot reduce a join, and so does one asked for a chain
				// it cannot fold. Same fallback for all of them.
				return nil, ErrJoinAggUnsupported
			default:
				return nil, statusErr(status, body)
			}
		}
	}
}

// refuseJoinAggSpec catches the requests the server can only answer as a malformed frame,
// which a client cannot tell from "too old to know the opcode".
func refuseJoinAggSpec(spec db.JoinAggSpec) error {
	for _, a := range spec.Aggs {
		if a.Func == db.AggUser && a.UserFunc == "" {
			return fmt.Errorf("dbrpc: a user-defined aggregate over %q names no aggregate", a.Arg)
		}
	}
	return nil
}

// joinAggFrame encodes the request: the join plan through the SAME encoder the row-streaming
// join opcodes use, then the reduction. Sharing the encoder is what keeps the plan from
// acquiring a second spelling here -- the decoder is likewise readJoinRequest, unchanged.
func joinAggFrame(jr JoinAggRequest, chain bool) func(uint64) []byte {
	return func(id uint64) []byte {
		b := putU8(req(id, opJoinAgg), boolByte(chain))
		b = appendJoinPlan(b, JoinRequest{Spec: jr.Spec.Join, Limits: jr.Limits}, chain)
		b = putI32(b, int32(jr.Spec.MaxGroups))
		b = putI32(b, int32(jr.Spec.MaxListValues))
		b = putI32(b, int32(len(jr.Spec.GroupCols)))
		for _, g := range jr.Spec.GroupCols {
			b = putU64(putStr(b, g.Attr), uint64(g.BucketWidth))
		}
		// The widest spec tail, unconditionally. The older aggregate opcodes pick a
		// narrower one so that a request expressible on an older frame stays byte-identical
		// on it; this opcode has no older frame to stay identical to, so one tail serves
		// every shape and a user-defined aggregate works over a join without a second
		// opcode for the name.
		return putAggSpecs(b, jr.Spec.Aggs, aggSpecNamed)
	}
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

// joinAggRowElided marks a row that is followed by its elision counts. A flags byte rather
// than an unconditional block, because the counts are absent from every row of every request
// that asks for no list-valued aggregate and from most rows of one that does -- and a reply
// that may legitimately carry a million groups should not spend eight bytes a row saying
// nothing happened.
const joinAggRowElided byte = 1

// maxJoinAggGroupCols bounds the group columns read off the wire before anything is allocated
// for them, matching what the single-table aggregate opcodes allow.
const maxJoinAggGroupCols = 1024

// streamJoinAggregate serves opJoinAgg.
func (s *Server) streamJoinAggregate(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	chainByte := r.u8()
	if r.err != nil || chainByte > 1 {
		write(respBad(reqID))
		return
	}
	chain := chainByte == 1
	jr, ok := readJoinRequest(r, chain)
	if !ok {
		write(respBad(reqID))
		return
	}
	spec := db.JoinAggSpec{
		Join:          jr.Spec,
		MaxGroups:     int(r.i32()),
		MaxListValues: int(r.i32()),
	}
	nGroup := int(r.i32())
	if r.err != nil || nGroup < 0 || nGroup > maxJoinAggGroupCols {
		write(respBad(reqID))
		return
	}
	spec.GroupCols = make([]db.GroupCol, nGroup)
	for i := range spec.GroupCols {
		attr := r.str()
		spec.GroupCols[i] = db.GroupCol{Attr: attr, BucketWidth: int64(r.u64())}
	}
	aggs, ok := readAggSpecs(r, reqID, write, aggSpecNamed)
	if !ok {
		return
	}
	spec.Aggs = aggs

	var stats db.JoinStats
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: joinOpName("JoinAggregate", chain), Table: joinTables(jr.Spec),
				Constraint: joinLogConstraint(jr.Spec), Limit: jr.Spec.Limit,
				Rows: stats.Rows, Duration: time.Since(start)})
		}()
	}

	// BOTH private-attribute gates the row-streaming join applies, over BOTH sets of
	// expressions the server will evaluate. The plan's are the join's own; the reduction's
	// are the group columns and each aggregate's argument and filter, which are expressions
	// over the same composite row and are not in the plan. Leaving the second set ungated
	// would make a GROUP BY the easiest way to read a private attribute out of this server,
	// since a group's label IS what its column evaluated to.
	for _, src := range append(joinExprs(jr.Spec), joinAggExprs(spec)...) {
		if refusePrivateConstraint(reqID, src, includePrivate, write) {
			return
		}
	}
	aliases := joinAliases(jr.Spec)
	if refusePrivateAliasExprs(reqID, aliases, joinExprs(jr.Spec), includePrivate, write) {
		return
	}
	if refusePrivateAliasExprs(reqID, aliases, joinAggExprs(spec), includePrivate, write) {
		return
	}

	jc, ok := s.cat.(joinAggCatalog)
	if !ok || (chain && !catalogJoinsChains(s.cat)) {
		write(respBad(reqID)) // this catalog serves no such aggregate; the client reduces for itself
		return
	}
	rows, err := jc.JoinAggregate(spec, jr.Limits, &stats, func() bool { return cancelled(ctx) })
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	if cancelled(ctx) {
		return // client gone: nothing left to send
	}
	// No error can follow the groups: a reduction is produced whole before any of it is
	// written, so a limit met part way through has already been reported above. That is
	// what lets a caller treat a non-nil error as "no answer" rather than "some answer".
	writeJoinAggRows(reqID, rows, write)
}

// joinAggExprs is every expression the REDUCTION will evaluate against a composite row: the
// group columns, and each aggregate's argument and filter. COUNT(*)'s "*" is not an
// expression and is left out, which keeps it from being parsed and refused as one.
func joinAggExprs(spec db.JoinAggSpec) []string {
	out := make([]string, 0, len(spec.GroupCols)+2*len(spec.Aggs))
	for _, g := range spec.GroupCols {
		out = append(out, g.Attr)
	}
	for _, a := range spec.Aggs {
		if a.Arg != "*" {
			out = append(out, a.Arg)
		}
		out = append(out, a.Filter)
	}
	return out
}

// writeJoinAggRows streams the groups, batched into frames of up to WireBatchBudget payload
// bytes, then the terminator.
//
// Batched, where the single-table aggregate opcodes send one frame per group, because the
// group counts differ by orders of magnitude. Those opcodes group by something that
// classifies -- an owner, a status -- and a reply with more groups than that has gone wrong.
// Here one group per JOB is the reason the opcode exists, so a frame per group would
// reintroduce exactly the per-row framing cost opJoinWire was added to remove.
//
// A single over-budget row still gets its own frame, so a group whose list-valued aggregate
// is long is never unshippable.
func writeJoinAggRows(reqID uint64, rows []db.AggRow, write func([]byte)) {
	head := respHead(reqID, stStream)
	countAt := len(head)
	frame := make([]byte, 0, WireBatchBudget+countAt+64)
	begin := func() {
		frame = append(frame[:0], head...)
		frame = putI32(frame, 0)
	}
	begin()
	n := 0
	flush := func() {
		if n == 0 {
			return
		}
		binary.LittleEndian.PutUint32(frame[countAt:countAt+4], uint32(n))
		write(frame)
		begin()
		n = 0
	}
	for _, row := range rows {
		// The length of a rendered row is not known before it is written -- a list-valued
		// cell is rendered here -- so the budget is checked AFTER appending rather than
		// before, and a frame may exceed it by one row. opJoinWire, which is handed rows
		// already encoded, checks before; the difference is where the bytes come from.
		frame = appendJoinAggRow(frame, row)
		n++
		if len(frame)-countAt-4 >= WireBatchBudget {
			flush()
		}
	}
	flush()
	write(respHead(reqID, stStreamEnd))
}

// appendJoinAggRow writes one group: the flags byte, the group cells and aggregate cells in
// the shared aggregate encoding, then the elision counts when there are any.
//
// The group cells are TYPED (appendAggRow's typed form). opAggregateTyped exists as a separate
// opcode from opAggregate because an older peer would misparse the typed reply; this opcode
// has no older peer, so there is one reply and no fallback path that only a mixed-version site
// would reach.
func appendJoinAggRow(frame []byte, row db.AggRow) []byte {
	flags := byte(0)
	for _, e := range row.Elided {
		if e > 0 {
			flags = joinAggRowElided
			break
		}
	}
	frame = putU8(frame, flags)
	frame = appendAggRow(frame, row, true)
	if flags&joinAggRowElided == 0 {
		return frame
	}
	// Sparse: the aggregates that actually lost values, not one entry per aggregate. A row
	// that reaches here has at least one, so the count is never zero and a reader that gets
	// a zero is reading a frame that does not mean what it says.
	at := len(frame)
	frame = putI32(frame, 0)
	cells := 0
	for i, e := range row.Elided {
		if e <= 0 {
			continue
		}
		frame = putI32(putI32(frame, int32(i)), int32(e))
		cells++
	}
	binary.LittleEndian.PutUint32(frame[at:at+4], uint32(cells))
	return frame
}

// readJoinAggRow reverses appendJoinAggRow. It reports ok=false for a frame it cannot trust,
// rather than returning a row built from whatever the reader produced after it went wrong --
// a group with the wrong aggregate values is worse than no answer.
func readJoinAggRow(body *reader, nGroup, nAgg int) (db.AggRow, bool) {
	flags := body.u8()
	if body.err != nil || flags&^joinAggRowElided != 0 {
		// A flag this build does not know: the bytes after it are whatever that flag
		// introduced, so nothing further in the frame can be read. Refused rather than
		// skipped, because a row read past an unknown extension is a row with values
		// taken from the wrong offsets.
		return db.AggRow{}, false
	}
	row := readAggRow(body, nGroup, nAgg, true)
	if body.err != nil {
		return db.AggRow{}, false
	}
	if flags&joinAggRowElided == 0 {
		return row, true
	}
	n := int(body.i32())
	if body.err != nil || n <= 0 || n > nAgg {
		return db.AggRow{}, false
	}
	elided := make([]int, nAgg)
	for i := 0; i < n; i++ {
		idx := int(body.i32())
		count := int(body.i32())
		if body.err != nil || idx < 0 || idx >= nAgg || count <= 0 {
			return db.AggRow{}, false
		}
		elided[idx] = count
	}
	row.Elided = elided
	return row, true
}

// joinAggCatalog is the optional Catalog capability to REDUCE a join's rows. Separate from
// joinCatalog for the reason joinWireCatalog and joinExplainCatalog are separate from it: a
// catalog could stream joined rows and have no reduce engine reading them, and advertising
// this on the strength of the other one is a definite yes that fails after a round trip.
//
// There is no marker method for "and this catalog's reduction knows list-valued aggregates",
// which joinChainCatalog needed for chains. It is not needed here: the capability arrived with
// the fields that carry its caps, so a catalog old enough to lack it does not satisfy this
// interface at all -- it would not compile against this signature.
type joinAggCatalog interface {
	JoinAggregate(spec db.JoinAggSpec, want db.JoinLimits, stats *db.JoinStats, stop func() bool) ([]db.AggRow, error)
}
