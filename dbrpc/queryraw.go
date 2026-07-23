package dbrpc

import (
	"context"
	"encoding/binary"
	"iter"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/collections"
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
	d, ok := s.tableOr(reqID, table, write)
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
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryRawProject", Table: table, Constraint: constraint, Limit: limit, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	// The projection (and, unprivileged, redaction) is applied inside the
	// collection's decode walk: non-projected attributes are never rendered, and a
	// hot-header-covered projection is served from the hot header alone.
	seq, err := d.QueryRawProjected(constraint, attrs, !includePrivate)
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

// QueryRawWireStream streams matching ads as wire-form rows (self-contained
// inline-names subset ads -- render them with collections.RenderRawAdInline),
// batched many rows per frame (WireBatchBudget on the server side). The row
// slice passed to yield aliases the frame buffer and is valid only until yield
// returns; redact requests source-side private-attribute stripping even on a
// privileged connection. Requires a server with opQueryRawWire; an older server
// answers respBad and the error surfaces here (callers fall back to the text
// row stream).
func (c *Client) QueryRawWireStream(ctx context.Context, table, constraint string, attrs []string, limit int, redact bool, yield func(row []byte) bool) error {
	build := func(id uint64) []byte {
		b := putStr(req(id, opQueryRawWire), table)
		b = putI32(b, int32(limit))
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
	_, ch, err := c.callStream(build)
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
				return nil // stStreamEnd
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
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	redact := r.u8() != 0 || !includePrivate
	constraint := r.str()
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "QueryRawWire", Table: table, Constraint: constraint, Limit: limit, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	seq, err := d.QueryRawWire(constraint, attrs, redact)
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
