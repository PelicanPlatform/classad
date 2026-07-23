package dbrpc

import (
	"context"
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
