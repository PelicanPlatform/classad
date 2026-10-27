package dbrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"iter"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// QueryRaw is Query but returns each ad as old-ClassAd wire text (the AST-free
// relay form for the caller): the server renders straight from the stored
// representation via the db QueryRaw pushdown, and the caller can forward the
// text without building a ClassAd. Private attributes are stripped unless the
// connection is privileged.
func (c *Client) QueryRaw(ctx context.Context, constraint string) ([]string, error) {
	return c.QueryRawTable(ctx, DefaultTable, constraint, 0)
}

// QueryRawTable is QueryRaw against a named table with an optional limit.
func (c *Client) QueryRawTable(ctx context.Context, table, constraint string, limit int) ([]string, error) {
	return c.streamCtx(ctx, func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opQueryRaw), table), int32(limit)), constraint)
	})
}

// QueryRawProject is QueryRawTable with a server-side projection: each returned ad
// carries only the attributes in attrs (matched case-insensitively) plus
// MyType/TargetType, so a query for a few attributes does not pull every attribute
// of every ad across the wire. An empty attrs behaves like QueryRawTable (all
// attributes). Private attributes are stripped unless the connection is privileged.
func (c *Client) QueryRawProject(ctx context.Context, table, constraint string, attrs []string, limit int) ([]string, error) {
	return c.streamCtx(ctx, func(id uint64) []byte {
		b := putStr(putI32(putStr(req(id, opQueryRawProj), table), int32(limit)), constraint)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	})
}

// ErrProjectRefsUnsupported is returned by the refs-chasing projection calls against a
// server too old to implement the opcode. A caller falls back to QueryRawProject, whose
// results are correct for literal-valued attributes and undefined for expression-valued
// ones whose references were projected away.
var ErrProjectRefsUnsupported = errors.New("dbrpc: server does not support reference-chasing projection")

// QueryRawProjectRefs is QueryRawProject whose projection also carries the attributes the
// projected expressions reference, transitively, so each returned ad EVALUATES
// self-contained.
//
// Use this whenever the caller is going to evaluate what it receives. QueryRawProject
// returns exactly the requested attributes -- correct for a relay reproducing HTCondor's
// query protocol, and wrong for an evaluator, because an attribute holding an expression
// over its siblings (Requirements, Rank, ...) loses those siblings and evaluates to
// undefined.
//
// Against a server without the opcode it returns ErrProjectRefsUnsupported.
func (c *Client) QueryRawProjectRefs(ctx context.Context, table, constraint string, attrs []string, limit int) ([]string, error) {
	rows, err := c.streamCtx(ctx, projectRefsFrame(table, constraint, attrs, limit))
	return rows, projectRefsErr(err)
}

// QueryRawProjectRefsStream is QueryRawProjectRefs with the streaming delivery of
// QueryRawTableStream.
func (c *Client) QueryRawProjectRefsStream(ctx context.Context, table, constraint string, attrs []string, limit int, yield func(row string) bool) error {
	return projectRefsErr(c.streamEach(ctx, projectRefsFrame(table, constraint, attrs, limit), yield))
}

// projectRefsFrame builds the opQueryRawProjRefs request frame (same shape as
// opQueryRawProj).
func projectRefsFrame(table, constraint string, attrs []string, limit int) func(uint64) []byte {
	return func(id uint64) []byte {
		b := putStr(putI32(putStr(req(id, opQueryRawProjRefs), table), int32(limit)), constraint)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}
}

// projectRefsErr maps the server's rejection of an unknown opcode to a sentinel the
// caller can fall back on, without matching message text.
func projectRefsErr(err error) error {
	if errors.Is(err, ErrBadRequest) {
		return ErrProjectRefsUnsupported
	}
	return err
}

// QueryRawProjectRefsStreamStats is QueryRawProjectRefsStream that also fills stats (may be nil)
// with the server's per-scan work breakdown, for EXPLAIN ANALYZE. Against a server too old to
// implement the opcode it returns ErrProjectRefsUnsupported with stats untouched, so the caller
// falls back to the plain stream and simply reports no server-side breakdown.
func (c *Client) QueryRawProjectRefsStreamStats(ctx context.Context, table, constraint string, attrs []string, limit int, yield func(row string) bool, stats *collections.ScanStats) error {
	onStats := func(r *reader) {
		if stats != nil {
			*stats = readScanStats(r)
		}
	}
	return projectRefsErr(c.streamEachStats(ctx, statsFrame(table, constraint, attrs, limit), yield, onStats))
}

// statsFrame builds the opQueryRawProjRefsStats request frame (same shape as opQueryRawProjRefs).
func statsFrame(table, constraint string, attrs []string, limit int) func(uint64) []byte {
	return func(id uint64) []byte {
		b := putStr(putI32(putStr(req(id, opQueryRawProjRefsStats), table), int32(limit)), constraint)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}
}

// QueryRawTableStream is QueryRawTable that hands each matching ad's old-ClassAd wire
// text to yield as it arrives, instead of collecting the whole result into a slice --
// so a relay (e.g. the collector) can forward each ad to its own client without
// buffering the entire result set. yield returns false to stop early. See streamEach for
// the error contract (a failure can arrive after some rows have been yielded).
func (c *Client) QueryRawTableStream(ctx context.Context, table, constraint string, limit int, yield func(row string) bool) error {
	return c.streamEach(ctx, func(id uint64) []byte {
		return putStr(putI32(putStr(req(id, opQueryRaw), table), int32(limit)), constraint)
	}, yield)
}

// QueryKeys streams the storage keys of the rows matching constraint on the default table.
func (c *Client) QueryKeys(ctx context.Context, constraint string, yield func(key string) bool) error {
	return c.QueryKeysTableStream(ctx, DefaultTable, constraint, yield)
}

// QueryKeysTableStream streams the storage KEYS of the rows matching constraint on the named
// table (not their ads), so a caller can address matched rows for UPDATE/DELETE by their real db
// key regardless of whether the ad carries a self-reported key attribute. Read-only.
func (c *Client) QueryKeysTableStream(ctx context.Context, table, constraint string, yield func(key string) bool) error {
	return c.streamEach(ctx, func(id uint64) []byte {
		return putStr(putStr(req(id, opQueryKeys), table), constraint)
	}, yield)
}

// QueryKeysTable collects all matching storage keys into a slice (streamed under the hood, so the
// server never buffers the whole set). Suitable for the bounded match sets of an UPDATE/DELETE.
func (c *Client) QueryKeysTable(ctx context.Context, table, constraint string) ([]string, error) {
	var keys []string
	err := c.QueryKeysTableStream(ctx, table, constraint, func(key string) bool {
		keys = append(keys, key)
		return true
	})
	if err != nil {
		return nil, err
	}
	return keys, nil
}

// QueryRawProjectStream is QueryRawProject (server-side projection) with the streaming
// delivery of QueryRawTableStream.
func (c *Client) QueryRawProjectStream(ctx context.Context, table, constraint string, attrs []string, limit int, yield func(row string) bool) error {
	return c.streamEach(ctx, func(id uint64) []byte {
		b := putStr(putI32(putStr(req(id, opQueryRawProj), table), int32(limit)), constraint)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}, yield)
}

// streamQueryRaw streams matching ads as old-ClassAd wire text, rendered from the
// db QueryRaw pushdown (no AST decode), one frame per ad like streamQuery.
func (s *Server) streamQueryRaw(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	constraint := r.str()
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryRaw", Table: table, Constraint: constraint, Limit: limit, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	d, ok := s.tableOr(reqID, opQueryRaw, table, write)
	if !ok {
		return
	}
	// Redaction is pushed into the collection's decode walk: an unprivileged
	// stream never renders a private value, and no per-attribute name
	// re-classification happens here.
	var seq iter.Seq[collections.RawAd]
	var err error
	if includePrivate {
		seq, err = d.QueryRaw(constraint)
	} else {
		seq, err = d.QueryRawRedacted(constraint)
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ra := range seq {
		if cancelled(ctx) {
			return
		}
		write(putStr(respHead(reqID, stStream), rawAdText(ra)))
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	write(respHead(reqID, stStreamEnd))
}

// rawAdText renders a RawAd as old-ClassAd wire text: the type tags as their own
// lines followed by the attribute expression lines verbatim. Filtering -- both
// projection and private-attribute redaction -- already happened inside the
// collection's decode walk, so nothing is re-classified here.
func rawAdText(ra collections.RawAd) string {
	var b strings.Builder
	if ra.MyType != "" {
		b.WriteString("MyType = \"")
		b.WriteString(ra.MyType)
		b.WriteString("\"\n")
	}
	if ra.TargetType != "" {
		b.WriteString("TargetType = \"")
		b.WriteString(ra.TargetType)
		b.WriteString("\"\n")
	}
	for _, e := range ra.Exprs {
		b.Write(e)
		b.WriteByte('\n')
	}
	return b.String()
}

// streamQueryRawProject is streamQueryRaw with a projection: it streams each
// matching ad rendered to only the requested attributes (plus MyType/TargetType),
// so a client that needs a handful of attributes does not pull every attribute of
// every ad across the wire. The projection is applied server-side; matching is
// case-insensitive (ClassAd attribute names are).
func (s *Server) streamQueryRawProject(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamQueryRawProjectOpt(ctx, reqID, r, includePrivate, write, qlog, false, false)
}

// streamQueryRawProjectRefs is streamQueryRawProject whose projection also carries the
// attributes the projected expressions reference, so each streamed ad evaluates
// self-contained at the far end. See db.DB.QueryRawProjectedRefs.
func (s *Server) streamQueryRawProjectRefs(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamQueryRawProjectOpt(ctx, reqID, r, includePrivate, write, qlog, true, false)
}

// streamQueryRawProjectRefsStats is streamQueryRawProjectRefs that also streams a ScanStats
// trailer (stStreamStats) before the terminator, for EXPLAIN ANALYZE.
func (s *Server) streamQueryRawProjectRefsStats(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamQueryRawProjectOpt(ctx, reqID, r, includePrivate, write, qlog, true, true)
}

// streamQueryRawProjectOpt is the shared body of the projection ops; chaseRefs picks which db
// projection they use, and wantStats (chaseRefs only) appends a scan-stats trailer.
func (s *Server) streamQueryRawProjectOpt(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog), chaseRefs, wantStats bool) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	constraint := r.str()
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	// The scan breakdown is computed when the client asked for it (EXPLAIN ANALYZE) and
	// ALSO whenever the server is aggregating queries, so the aggregate can report what
	// pruned and what did not instead of leaving its most diagnostic column blank.
	var stats *collections.ScanStats
	if wantStats || s.queryStatsOn() {
		stats = &collections.ScanStats{}
	}
	if qlog != nil {
		defer func() {
			q := QueryLog{Op: projectOpName(chaseRefs), Table: table, Constraint: constraint, Limit: limit,
				Rows: n, Projection: len(attrs), Duration: time.Since(start)}
			if stats != nil {
				q.Scan = *stats
			}
			qlog(q)
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	// The projection (and, unprivileged, redaction) is applied inside the
	// collection's decode walk: non-projected attributes are never rendered, and a
	// hot-header-covered projection is served from the hot header alone. Resolve the
	// source uniformly across a mutable table, a materialized view's backing, and an
	// append-only archive (history) table -- so the one projection op serves them all
	// (an archive streams newest-first, like its plain query).
	var seq iter.Seq[collections.RawAd]
	var err error
	redact := !includePrivate
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	switch ref := s.resolveTable(table); ref.kind {
	case kindTable, kindView:
		switch {
		case stats != nil && chaseRefs:
			seq, err = ref.db.QueryRawProjectedRefsStats(constraint, attrs, redact, stats)
		case stats != nil:
			seq, err = ref.db.QueryRawProjectedStats(constraint, attrs, redact, stats)
		case chaseRefs:
			seq, err = ref.db.QueryRawProjectedRefs(constraint, attrs, redact)
		default:
			seq, err = ref.db.QueryRawProjected(constraint, attrs, redact)
		}
	case kindArchive:
		switch {
		case stats != nil && chaseRefs:
			seq, err = ref.arch.QueryRawProjectedRefsStats(constraint, attrs, redact, stats)
		case stats != nil:
			seq, err = ref.arch.QueryRawProjectedStats(constraint, attrs, redact, stats)
		case chaseRefs:
			seq, err = ref.arch.QueryRawProjectedRefs(constraint, attrs, redact)
		default:
			seq, err = ref.arch.QueryRawProjected(constraint, attrs, redact)
		}
	default:
		write(respErr(reqID, "no such table: "+table))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ra := range seq {
		if cancelled(ctx) {
			return
		}
		write(putStr(respHead(reqID, stStream), rawAdText(ra)))
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	if wantStats {
		write(putScanStats(respHead(reqID, stStreamStats), *stats))
	}
	write(respHead(reqID, stStreamEnd))
}

// streamQueryRawProjectSeq is streamQueryRawProject resumed from a cursor: it
// streams at most limit matching ads after the cursor position and closes with a
// cursor trailer saying where to continue.
//
// Mutable tables only. An archive already answers "the newest K" in one shot,
// and a materialized view's backing is a mutable table, so both other kinds fall
// through to the plain projection op rather than pretending to paginate.
func (s *Server) streamQueryRawProjectSeq(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	constraint := r.str()
	var after collections.SeqCursor
	after.Shard = uint32(r.u64())
	after.Snapshot = r.u64()
	after.Seq = r.u64()
	after.Key = r.str()
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryRawProjSeq", Table: table, Constraint: constraint, Limit: limit,
				Rows: n, Projection: len(attrs), Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}

	ref := s.resolveTable(table)
	switch ref.kind {
	case kindTable:
	case kindView:
		write(respErr(reqID, "cursor pagination is not available for a view: "+table))
		return
	case kindArchive:
		write(respArchiveTable(reqID, table, "cursor pagination is not needed; an archive answers newest-first with the limit pushed down"))
		return
	default:
		write(respErr(reqID, "no such table: "+table))
		return
	}
	d := ref.db

	seq, page, err := d.QueryRawProjectedFromSeq(constraint, attrs, after, limit)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ra := range seq {
		if cancelled(ctx) {
			return
		}
		write(putStr(respHead(reqID, stStream), rawAdText(ra)))
		n++
	}
	if page.Err != nil {
		// Set before any row, so nothing above was written: the caller gets the refusal in
		// place of the page, never a page that silently ends the listing.
		msg := page.Err.Error()
		if errors.Is(page.Err, collections.ErrSeqCursorExpired) {
			msg = cursorExpiredPrefix + msg
		}
		write(respErr(reqID, msg))
		return
	}
	cur := respHead(reqID, stStreamCursor)
	more := byte(0)
	if page.More {
		more = 1
	}
	cur = append(cur, more)
	cur = putU64(cur, uint64(page.Next.Shard))
	cur = putU64(cur, page.Next.Snapshot)
	cur = putU64(cur, page.Next.Seq)
	write(putStr(cur, page.Next.Key))
	write(respHead(reqID, stStreamEnd))
}

// WireBatchBudget caps one wire-row batch frame's payload bytes. It bounds the
// per-stream buffer on BOTH sides (each holds ~one frame), amortizes the
// per-frame syscall/wakeup cost, and stays well under the transport's 1MB
// message ceiling. Measured sensitivity (2000x21KB-ad scans over TCP loopback):
// 16KB->64KB gains ~6%, 64KB->256KB ~4%, beyond 256KB flat -- so the default
// stays memory-lean at 64KB (~128KB per active stream across both sides) and a
// deployment can raise it for the last few percent on whole-ad scans (projected
// streams are insensitive: their rows are small enough that every budget
// batches deeply).
var WireBatchBudget = 64 << 10

// ErrRawWireUnsupported reports that the wire-form row stream is not available for
// this request, and the caller should fall back to a text row stream (which every
// server and every table can serve). Two causes, one response: a server too old to
// know opQueryRawWire, and a table that cannot produce self-contained rows (an
// in-memory table -- see db.ErrRawWireUnsupported).
//
// It is always delivered BEFORE the first row, so a caller that has already relayed
// rows can treat it as a hard failure rather than retrying.
var ErrRawWireUnsupported = errors.New("dbrpc: wire-form rows unavailable for this request")

// QueryRawWireStream streams matching ads as wire-form rows (self-contained
// inline-names subset ads -- render them with collections.RenderRawAdInline, or
// decode them with wire.DecodeInline), batched many rows per frame
// (WireBatchBudget on the server side). The row slice passed to yield aliases the
// frame buffer and is valid only until yield returns; redact requests source-side
// private-attribute stripping even on a privileged connection.
//
// It returns ErrRawWireUnsupported, before any row, when the stream is unavailable
// -- an older server, or a table that cannot serve wire rows. Callers fall back to
// the text row stream. An empty stream means the query matched nothing; it never
// means the table could not answer.
func (c *Client) QueryRawWireStream(ctx context.Context, table, constraint string, attrs []string, limit int, redact bool, yield func(row []byte) bool) error {
	return c.queryRawWireStream(ctx, opQueryRawWire, 0, table, constraint, attrs, limit, redact, yield)
}

// ErrRawWireRefsUnsupported reports that the REFERENCE-CHASING wire-row stream is not
// available for this request. Two causes, one response, exactly as with ErrRawWireUnsupported
// (which it wraps, so a caller matching only the general case still matches): a server too old
// to know the opcode, and a table that cannot produce self-contained rows.
//
// It wraps rather than replaces because the two have different best fallbacks. A caller that
// wanted the closure can retry the PLAIN wire stream for the whole ad and compute the closure
// itself -- which works against a server that has opQueryRawWire but not this one, and is what
// the mirror did before this op existed. That retry will itself return
// ErrRawWireUnsupported for a table that serves no wire rows at all, at which point the text
// stream is the only answer left.
//
// Like ErrRawWireUnsupported it is always delivered BEFORE the first row.
var ErrRawWireRefsUnsupported = fmt.Errorf("%w: reference-chasing wire rows", ErrRawWireUnsupported)

// QueryRawWireRefsStream is QueryRawWireStream whose projection also carries the attributes
// the projected expressions reference, transitively, so each row EVALUATES self-contained --
// the wire-row form of QueryRawProjectRefsStream, returning the same attributes it does.
//
// Use it whenever the rows will be evaluated rather than relayed verbatim. The closure is
// computed at the store, per record, which is the only place it can be: whether a projected
// attribute holds an expression is a property of the stored ad, so a client cannot decide it
// for the query and cannot discover it from rows it has already been sent.
//
// An empty attrs is a whole-ad request, which has no closure to compute and behaves exactly
// like QueryRawWireStream. Against a server without the opcode it returns
// ErrRawWireRefsUnsupported before any row; see there for the fallback.
func (c *Client) QueryRawWireRefsStream(ctx context.Context, table, constraint string, attrs []string, limit int, redact bool, yield func(row []byte) bool) error {
	err := c.queryRawWireStream(ctx, opQueryRawWireOpts, wireFlagRefs, table, constraint, attrs, limit, redact, yield)
	if errors.Is(err, ErrRawWireUnsupported) {
		return ErrRawWireRefsUnsupported
	}
	return err
}

func (c *Client) queryRawWireStream(ctx context.Context, o op, flags uint32, table, constraint string, attrs []string, limit int, redact bool, yield func(row []byte) bool) error {
	build := func(id uint64) []byte {
		b := putStr(req(id, o), table)
		b = putI32(b, int32(limit))
		if o == opQueryRawWireOpts {
			b = putI32(b, int32(flags))
		}
		r := byte(0)
		if redact {
			r = 1
		}
		b = append(b, r)
		b = putStr(b, constraint)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}
	_, ch, strm, err := c.callStream(build)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			drain(ch)
			return ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				return c.streamEnd(strm)
			}
			_, status, body, ok := respHeader(frame)
			if !ok {
				return errShort
			}
			switch status {
			case stStream:
				n := int(body.i32())
				for i := 0; i < n; i++ {
					row := body.bytesRef()
					if body.err != nil {
						return errShort
					}
					if !yield(row) {
						drain(ch)
						return nil
					}
				}
			case stBadReq:
				// A server too old to know the opcode rejects the request this way,
				// and so does a table that cannot serve wire rows: same fallback.
				return ErrRawWireUnsupported
			case stErr:
				return statusErr(status, body)
			default:
				return statusErr(status, body)
			}
		}
	}
}

// streamQueryRawWire serves opQueryRawWire: wire-form rows from the db's
// slice-copy subset scan, batched into frames of up to WireBatchBudget payload
// bytes (a single over-budget row still gets its own frame -- exactly the old
// one-frame-per-ad behavior, so a jumbo ad is never unshippable).
func (s *Server) streamQueryRawWire(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamQueryRawWireOpt(ctx, reqID, r, includePrivate, write, qlog, false)
}

// streamQueryRawWireOpts serves opQueryRawWireOpts: streamQueryRawWire with a flags word.
// Unknown bits are refused before any row (see wireFlagsKnown), which is what lets options
// be added here instead of as new opcodes.
func (s *Server) streamQueryRawWireOpts(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	s.streamQueryRawWireOpt(ctx, reqID, r, includePrivate, write, qlog, true)
}

// streamQueryRawWireOpt is the shared body of the two wire-row ops. withFlags says whether
// the frame carries the flags word, which only opQueryRawWireOpts does -- opQueryRawWire's
// frame is frozen.
func (s *Server) streamQueryRawWireOpt(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog), withFlags bool) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	var flags uint32
	if withFlags {
		flags = uint32(r.i32())
	}
	if flags&^wireFlagsKnown != 0 {
		// stBadReq, the same refusal an unknown OPCODE gets, so a newer client falls back
		// down the path it already has. Refusing beats serving the request without the
		// option asked for: a row stream that silently lacks the closure is the failure
		// this guards against.
		write(respBad(reqID))
		return
	}
	chaseRefs := flags&wireFlagRefs != 0
	redact := r.u8() != 0 || !includePrivate
	constraint := r.str()
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	// The wire path has no EXPLAIN ANALYZE variant, so the scan breakdown is collected
	// only when the server is aggregating -- but it is collected, because this is the op
	// most clients actually read with, and leaving it blank left the aggregate's most
	// diagnostic column empty for the common case.
	var stats *collections.ScanStats
	if s.queryStatsOn() {
		stats = &collections.ScanStats{}
	}
	if qlog != nil {
		defer func() {
			q := QueryLog{Op: wireOpName(chaseRefs), Table: table, Constraint: constraint, Limit: limit,
				Rows: n, Projection: len(attrs), Duration: time.Since(start)}
			if stats != nil {
				q.Scan = *stats
			}
			qlog(q)
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	d, ok := s.tableOr(reqID, wireOp(chaseRefs), table, write)
	if !ok {
		return
	}
	var seq iter.Seq[[]byte]
	var err error
	switch {
	case chaseRefs:
		seq, err = d.QueryRawWireRefsStats(constraint, attrs, redact, stats)
	case stats != nil:
		seq, err = d.QueryRawWireStats(constraint, attrs, redact, stats)
	default:
		seq, err = d.QueryRawWire(constraint, attrs, redact)
	}
	if errors.Is(err, db.ErrRawWireUnsupported) {
		// Not a failure: this table cannot produce self-contained rows (it is in
		// memory). Reject the request the same way an older server rejects the
		// opcode, so the client takes the same text fallback rather than seeing an
		// empty stream and believing the query matched nothing.
		write(respBad(reqID))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}

	// Rows build DIRECTLY into one reused frame buffer behind a reserved header
	// (the row count is patched at flush), so a steady stream allocates nothing
	// and copies each row exactly once -- write() hands the buffer to a
	// synchronous, non-retaining WriteMsg, making reuse safe.
	head := respHead(reqID, stStream)
	countAt := len(head)
	frame := make([]byte, 0, WireBatchBudget+countAt+64)
	begin := func() {
		frame = append(frame[:0], head...)
		frame = putI32(frame, 0) // row count, patched by flush
	}
	begin()
	rows := 0
	flush := func() {
		if rows == 0 {
			return
		}
		binary.LittleEndian.PutUint32(frame[countAt:countAt+4], uint32(rows))
		write(frame)
		begin()
		rows = 0
	}
	payloadLen := func() int { return len(frame) - countAt - 4 }
	for row := range seq {
		if cancelled(ctx) {
			return
		}
		if rows > 0 && payloadLen()+4+len(row) > WireBatchBudget {
			flush()
		}
		frame = putBytes(frame, row)
		rows++
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	flush()
	write(respHead(reqID, stStreamEnd))
}

// wireOp and wireOpName name the wire-row contract a request arrived under, so the archive
// refusal and the query log both say which one the caller asked for.
func wireOp(chaseRefs bool) op {
	if chaseRefs {
		return opQueryRawWireOpts
	}
	return opQueryRawWire
}

func wireOpName(chaseRefs bool) string {
	if chaseRefs {
		return "QueryRawWireRefs"
	}
	return "QueryRawWire"
}

// projectOpName labels a projection query in the query log by which contract it ran under.
func projectOpName(chaseRefs bool) string {
	if chaseRefs {
		return "QueryRawProjectRefs"
	}
	return "QueryRawProject"
}

// SeqCursor and SeqPage re-export the storage layer's pagination types, so a
// dbrpc caller does not import collections just to hold a cursor.
type (
	SeqCursor = collections.SeqCursor
	SeqPage   = collections.SeqPage
)

// ErrPaginationUnsupported is returned when the server does not implement the
// paginated opcode (it rejects the request as a bad op), so a caller can fall
// back to an unpaginated read rather than failing.
var ErrPaginationUnsupported = errors.New("dbrpc: server does not support cursor pagination")

// QueryRawProjectedFromSeqStream is QueryRawProjectedStream a page at a time: at
// most limit rows after the cursor, in the store's commit-sequence order, plus
// the cursor to continue from.
//
// Pagination lives on the server's own ordering, so a page costs a resume rather
// than a re-scan, and the cursor names no physical position, so compaction moving
// records does not disturb it. Hand the returned page's Next back unchanged for
// the following page; the zero cursor starts at the beginning.
//
// A cursor's snapshot is kept readable for the server's lease period
// (collections.Options.SeqCursorLease). A cursor resumed after the server has
// reclaimed versions its snapshot sees fails with ErrCursorExpired before any
// row: restart the listing from the zero cursor.
func (c *Client) QueryRawProjectedFromSeqStream(ctx context.Context, table, constraint string, attrs []string, after SeqCursor, limit int, yield func(row string) bool) (*SeqPage, error) {
	page := &SeqPage{}
	err := c.streamEachCursor(ctx, func(id uint64) []byte {
		b := putStr(putI32(putStr(req(id, opQueryRawProjSeq), table), int32(limit)), constraint)
		b = putU64(b, uint64(after.Shard))
		b = putU64(b, after.Snapshot)
		b = putU64(b, after.Seq)
		b = putStr(b, after.Key)
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}, yield, func(r *reader) {
		page.More = r.u8() == 1
		page.Next.Shard = uint32(r.u64())
		page.Next.Snapshot = r.u64()
		page.Next.Seq = r.u64()
		page.Next.Key = r.str()
	})
	if err != nil {
		if errors.Is(err, ErrBadRequest) {
			// How the server answers an opcode it does not implement.
			return nil, fmt.Errorf("%w: %w", ErrPaginationUnsupported, err)
		}
		return nil, err
	}
	return page, nil
}

// QueryRawProjectedFromSeq is QueryRawProjectedFromSeqStream collected into a
// slice, for a caller that wants the page in hand.
func (c *Client) QueryRawProjectedFromSeq(ctx context.Context, table, constraint string, attrs []string, after SeqCursor, limit int) ([]string, *SeqPage, error) {
	var rows []string
	page, err := c.QueryRawProjectedFromSeqStream(ctx, table, constraint, attrs, after, limit, func(row string) bool {
		rows = append(rows, row)
		return true
	})
	if err != nil {
		return nil, nil, err
	}
	return rows, page, nil
}
