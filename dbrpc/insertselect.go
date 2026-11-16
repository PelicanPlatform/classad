package dbrpc

import (
	"context"
	"errors"
	"fmt"

	"github.com/PelicanPlatform/classad/db"
)

// Declaring a table's primary key, and running a roll-up entirely on the server.
//
// These travel together because one is useless without the other: a server-side
// INSERT ... SELECT has to decide each written row's storage key, and the client-side
// convention it would otherwise borrow (an attribute named "Key", settable per process)
// is a REPL setting the server has never been told about. The key rule has to be
// something the server knows, which means it has to be on the table.

// PrimaryKey returns the attributes whose values derive table's storage keys, or nil if
// the table declares none.
func (c *Client) PrimaryKey(ctx context.Context, table string) ([]string, error) {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		return putStr(req(id, opPrimaryKey), table)
	})
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	n := int(body.i32())
	if n < 0 || body.err != nil {
		return nil, errShort
	}
	cols := make([]string, n)
	for i := range cols {
		cols[i] = body.str()
	}
	if body.err != nil {
		return nil, errShort
	}
	return cols, nil
}

// SetPrimaryKey declares the attributes whose values derive table's storage keys. An empty
// cols clears the declaration.
//
// It does not rewrite existing rows; see db.DB.SetPrimaryKey for why a declaration governs
// later writes rather than reorganising what is already stored.
func (c *Client) SetPrimaryKey(ctx context.Context, table string, cols []string) error {
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putI32(putStr(req(id, opSetPrimaryKey), table), int32(len(cols)))
		for _, col := range cols {
			b = putStr(b, col)
		}
		return b
	})
	if err != nil {
		return err
	}
	if status != stOK {
		return statusErr(status, body)
	}
	return nil
}

// InsertSelect runs a grouped aggregate over source and writes its rows into target, all on
// the server, and reports how many rows were written.
//
// Only the COUNT crosses the wire. That is the point of serving it here rather than
// composing a SELECT and a batch of writes in the client: the reduction already happened on
// the server, and shipping its rows out only to ship them back differs from this by two
// network traversals of the whole result and a round trip per row.
//
// names has one entry per group column followed by one per aggregate: the attribute each
// value is written under in target. They are explicit because a SQL column header is not an
// attribute name -- `COUNT(*)` is a legal header and cannot be an attribute -- so the
// caller aliases them and sends the aliases.
//
// target must declare a primary key covering columns that names includes; the server
// refuses otherwise rather than inventing keys. See db/primarykey.go.
func (c *Client) InsertSelect(ctx context.Context, target, source, constraint string,
	groups []db.GroupCol, aggs []db.AggSpec, names []string) (int, error) {

	if err := c.refuseUnservableAggregate(aggs, aggSpecNamed); err != nil {
		return 0, err
	}
	status, body, err := c.callCtx(ctx, func(id uint64) []byte {
		b := putStr(putStr(putStr(req(id, opInsertSelect), target), source), constraint)
		b = putI32(b, int32(len(groups)))
		for _, g := range groups {
			b = putU64(putStr(b, g.Attr), uint64(g.BucketWidth))
		}
		b = putAggSpecs(b, aggs, aggSpecNamed)
		b = putI32(b, int32(len(names)))
		for _, n := range names {
			b = putStr(b, n)
		}
		return b
	})
	if err != nil {
		return 0, err
	}
	if status == stBadReq {
		// No fallback. A client-side composition would answer the same question by a
		// different route, but it would also write through a different key rule -- the
		// REPL's generated keys rather than the table's declared one -- so a roll-up that
		// quietly fell back would stop being idempotent and nothing would say so.
		return 0, ErrInsertSelectUnsupported
	}
	if status != stOK {
		return 0, statusErr(status, body)
	}
	n := body.u64()
	if body.err != nil {
		return 0, errShort
	}
	return int(n), nil
}

// ErrInsertSelectUnsupported is returned against a server too old to implement the
// roll-up opcode. There is deliberately no fallback: see InsertSelect.
var ErrInsertSelectUnsupported = errors.New(
	"dbrpc: the server does not implement a server-side INSERT ... SELECT")

// --- server ---

// servePrimaryKey answers opPrimaryKey.
func (s *Server) servePrimaryKey(reqID uint64, r *reader, write func([]byte)) {
	table := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, opPrimaryKey, table, write)
	if !ok {
		return
	}
	cols := d.PrimaryKey()
	b := putI32(resp(reqID, stOK), int32(len(cols)))
	for _, col := range cols {
		b = putStr(b, col)
	}
	write(b)
}

// serveSetPrimaryKey answers opSetPrimaryKey.
func (s *Server) serveSetPrimaryKey(reqID uint64, r *reader, write func([]byte)) {
	table := r.str()
	n := int(r.i32())
	if r.err != nil || n < 0 || n > 64 {
		write(respBad(reqID))
		return
	}
	cols := make([]string, n)
	for i := range cols {
		cols[i] = r.str()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, opSetPrimaryKey, table, write)
	if !ok {
		return
	}
	if err := d.SetPrimaryKey(cols); err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	write(resp(reqID, stOK))
}

// serveInsertSelect answers opInsertSelect: reduce source, write the rows into target.
func (s *Server) serveInsertSelect(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	target := r.str()
	source := r.str()
	constraint := r.str()
	nGroup := int(r.i32())
	if r.err != nil || nGroup < 0 || nGroup > 64 {
		write(respBad(reqID))
		return
	}
	groups := make([]db.GroupCol, nGroup)
	for i := range groups {
		groups[i] = db.GroupCol{Attr: r.str(), BucketWidth: int64(r.u64())}
	}
	aggs, ok := readAggSpecs(r, reqID, write, aggSpecNamed)
	if !ok {
		return
	}
	nNames := int(r.i32())
	if r.err != nil || nNames < 0 || nNames > 1024 {
		write(respBad(reqID))
		return
	}
	names := make([]string, nNames)
	for i := range names {
		names[i] = r.str()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}

	// The TARGET must be a mutable table. An archive is append-only and a view is derived
	// from its base, so neither can take a roll-up's rows; saying so beats "no such table".
	targetRef := s.resolveTable(target)
	if targetRef.kind != kindTable {
		write(respErr(reqID, "INSERT ... SELECT target "+target+
			" is not a writable table (an archive and a view cannot be inserted into)"))
		return
	}

	rows, err := s.reduceForInsert(ctx, reqID, source, constraint, groups, aggs, includePrivate, write)
	if err != nil {
		return // reduceForInsert already answered
	}
	n, err := targetRef.db.InsertSelectInto(rows, db.InsertSpec{
		GroupCols: groups, Aggs: aggs, Names: names,
	})
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	write(putU64(resp(reqID, stOK), uint64(n)))
}

// errAnswered marks that the failure has already been written to the client.
var errAnswered = fmt.Errorf("answered")

// reduceForInsert runs the aggregate against whichever kind of table source names, reusing
// the existing per-kind entry points (and so their whole aggregate ladders) rather than
// growing a second reduction here.
func (s *Server) reduceForInsert(ctx context.Context, reqID uint64, source, constraint string,
	groups []db.GroupCol, aggs []db.AggSpec, includePrivate bool, write func([]byte)) ([]db.AggRow, error) {

	stop := func() bool { return ctx.Err() != nil }
	switch ref := s.resolveTable(source); ref.kind {
	case kindTable, kindView:
		var rows []db.AggRow
		var err error
		if includePrivate {
			rows, err = ref.db.AggregateColsStats(constraint, groups, aggs, nil, stop)
		} else {
			rows, err = ref.db.AggregateColsStatsRedacted(constraint, groups, aggs, nil, stop)
		}
		if err != nil {
			write(respErr(reqID, aggErrMsg(err)))
			return nil, errAnswered
		}
		return rows, nil
	case kindArchive:
		var rows []db.AggRow
		var err error
		if includePrivate {
			rows, err = ref.arch.AggregateColsStats(constraint, groups, aggs, nil)
		} else {
			rows, err = ref.arch.AggregateColsStatsRedacted(constraint, groups, aggs, nil)
		}
		if err != nil {
			write(respErr(reqID, aggErrMsg(err)))
			return nil, errAnswered
		}
		return rows, nil
	default:
		write(respErr(reqID, "no such table: "+source))
		return nil, errAnswered
	}
}
