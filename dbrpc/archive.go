package dbrpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// Archive (history) table RPCs. Create/append are unary mutating ops (handled in handle);
// query streams newest-first results like opQuery. Private attributes are stripped for a
// non-privileged reader, as everywhere.

// streamArchiveQuery streams an archive's newest-first, limit-capped matches.
func (sc *serverConn) streamArchiveQuery(reqID uint64, r *reader) {
	name := r.str()
	limit := int(r.i32())
	constraint := r.str()
	if r.err != nil {
		sc.write(respBad(reqID))
		return
	}
	a, ok := sc.s.cat.ArchiveTable(name)
	if !ok {
		sc.write(respErr(reqID, "no such archive: "+name))
		return
	}
	seq, err := a.QueryLimit(constraint, limit) // limit pushed down (newest-first)
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	for ad := range seq {
		if cancelled(sc.ctx) {
			return // client gone
		}
		sc.write(putStr(respHead(reqID, stStream), adString(ad, sc.opts.IncludePrivate)))
	}
	sc.write(respHead(reqID, stStreamEnd))
}

// streamArchiveAggregate runs a server-side GROUP BY over a history table and streams one
// frame per group, mirroring streamAggregate but against an archive. It reads the raw group
// columns and aggregate specs, resolves the archive, and reduces via db's shared aggregate
// engine so the result is identical to the same aggregate over a mutable table. Private
// attributes are refused for an unprivileged reader, as with the mutable aggregate.
func (sc *serverConn) streamArchiveAggregate(reqID uint64, r *reader) {
	sc.archiveAggregate(reqID, r, false)
}

// streamArchiveAggregateFiltered is streamArchiveAggregate whose specs carry a per-aggregate
// filter (opArchiveAggregateFiltered). Only the spec encoding differs.
func (sc *serverConn) streamArchiveAggregateFiltered(reqID uint64, r *reader) {
	sc.archiveAggregate(reqID, r, true)
}

func (sc *serverConn) archiveAggregate(reqID uint64, r *reader, filtered bool) {
	name := r.str()
	constraint := r.str()
	nGroup := int(r.i32())
	if nGroup < 0 || nGroup > 1024 {
		sc.write(respBad(reqID))
		return
	}
	groupCols := make([]GroupCol, nGroup)
	for i := range groupCols {
		groupCols[i] = GroupCol{Attr: r.str()}
	}
	aggs, ok := readAggSpecs(r, reqID, sc.write, filtered)
	if !ok {
		return
	}
	if !sc.opts.IncludePrivate {
		for _, g := range groupCols {
			if classad.IsPrivateAttribute(g.Attr) {
				sc.write(respErr(reqID, "cannot group by private attribute "+g.Attr))
				return
			}
		}
		for _, a := range aggs {
			if a.Arg != "*" && classad.IsPrivateAttribute(a.Arg) {
				sc.write(respErr(reqID, "cannot aggregate private attribute "+a.Arg))
				return
			}
			for _, ref := range db.AggFilterAttrs(a.Filter) {
				if classad.IsPrivateAttribute(ref) {
					sc.write(respErr(reqID, "cannot filter on private attribute "+ref))
					return
				}
			}
		}
	}
	a, ok := sc.s.cat.ArchiveTable(name)
	if !ok {
		sc.write(respErr(reqID, "no such archive: "+name))
		return
	}
	groupBy := make([]string, len(groupCols))
	for i, g := range groupCols {
		groupBy[i] = g.Attr
	}
	rows, err := a.Aggregate(constraint, groupBy, aggs)
	if err != nil {
		sc.write(respErr(reqID, err.Error()))
		return
	}
	for _, row := range rows {
		if cancelled(sc.ctx) {
			return // client gone
		}
		frame := respHead(reqID, stStream)
		for _, gv := range row.Group {
			frame = putStr(frame, gv)
		}
		for _, v := range row.Values {
			frame = putStr(frame, v)
		}
		sc.write(frame)
	}
	sc.write(respHead(reqID, stStreamEnd))
}

// --- client ---

// CreateArchiveTable creates (or no-ops if present) an append-only history table. cfg
// configures indexes / zone maps / retention on first creation.
func (c *Client) CreateArchiveTable(ctx context.Context, name string, cfg db.ArchiveConfig) error {
	data, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putBytes(putStr(req(id, opArchiveCreate), name), data)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ArchiveAppend appends an ad (old-ClassAd text) to the named history table.
func (c *Client) ArchiveAppend(ctx context.Context, name, adText string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putStr(putStr(req(id, opArchiveAppend), name), adText)
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// ArchiveQuery returns up to limit (<= 0 = all) newest-first matches (old-ClassAd texts)
// from the named history table -- the condor_history "last K" pattern.
func (c *Client) ArchiveQuery(ctx context.Context, name, constraint string, limit int) ([]string, error) {
	return c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opArchiveQuery), name), int32(limit)), constraint)
	})
}

// ErrArchiveAggregateUnsupported is returned by ArchiveAggregate against a server too old
// to implement the opcode (it rejects the request as a bad op), so a caller can fall back to
// client-side aggregation (ArchiveQuery + reduce locally).
var ErrArchiveAggregateUnsupported = errors.New("dbrpc: server does not support archive aggregation")

// ArchiveAggregate runs a server-side GROUP BY over the named history table: the server
// applies the constraint (with zone-map pruning), groups by groupBy, and reduces each group
// with the COUNT/SUM/AVG/MIN/MAX aggs, returning one AggRow per group. With no group columns
// it returns a single row over the whole match. Only the (small) grouped result crosses the
// wire, not every matched ad -- the point of pushing a COUNT over ~200k history rows to the
// server. Against a server that does not implement the opcode it returns an error wrapping
// ErrArchiveAggregateUnsupported so the caller can fall back to client-side aggregation.
func (c *Client) ArchiveAggregate(ctx context.Context, name, constraint string, groupBy []string, aggs []AggSpec) ([]AggRow, error) {
	filtered := anyFiltered(aggs)
	build := func(id uint64) []byte {
		o := opArchiveAggregate
		if filtered {
			o = opArchiveAggregateFiltered
		}
		b := putStr(putStr(req(id, o), name), constraint)
		b = putI32(b, int32(len(groupBy)))
		for _, g := range groupBy {
			b = putStr(b, g)
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
				// A server too old to know the opcode rejects it as a bad request. A plain
				// archive aggregate can fall back to a client-side scan; a FILTERED one
				// must not be retried without its filters.
				if filtered {
					return nil, ErrFilteredAggregateUnsupported
				}
				return nil, ErrArchiveAggregateUnsupported
			default:
				return out, fmt.Errorf("dbrpc: unexpected archive aggregate status %d", status)
			}
		}
	}
}

// ArchiveRotate enforces the named archive's retention policy now (using the server's
// clock for age-based rules), returning how many sealed segments were dropped.
func (c *Client) ArchiveRotate(ctx context.Context, name string) (int, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putStr(req(id, opArchiveRotate), name)
	})
	if err != nil {
		return 0, err
	}
	if status != stOK {
		return 0, statusErr(status, body)
	}
	return int(body.i32()), nil
}

// ArchiveTables lists the history table names.
func (c *Client) ArchiveTables(ctx context.Context) ([]string, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte { return req(id, opArchiveList) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	out := make([]string, 0, n)
	for i := 0; i < n && body.err == nil; i++ {
		out = append(out, body.str())
	}
	return out, nil
}
