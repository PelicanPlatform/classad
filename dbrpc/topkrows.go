package dbrpc

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// THE K BEST ROWS OF EACH GROUP, RANKED AT THE STORE.
//
// This is the pushdown for a ranking window -- ROW_NUMBER() OVER (PARTITION BY Owner ORDER BY
// QDate DESC) with a QUALIFY. Without it a client fetches every matching row and ranks them,
// which is the term that scales with the network rather than with the scan: 100 owners holding
// a thousand jobs each is 100,000 rows crossing to keep 500.
//
// The rows cross in WIRE FORM, so neither side builds a value it is only going to re-encode.

// ErrTopKRowsUnsupported reports that the server does not implement opTopKRows -- it is older
// than the opcode. It is always delivered BEFORE any group, so a caller can fall back to
// fetching every matching row and ranking them itself, which is what it did before this
// existed. An empty answer would be indistinguishable from a query that matched nothing.
var ErrTopKRowsUnsupported = errors.New("dbrpc: server does not support top-k rows per group")

// TopKRowsSpec, TopKOrderTerm and GroupCol re-export db's, so a caller writes one spec and
// hands it to either the embedded rung or this one.
type (
	TopKRowsSpec  = db.TopKRowsSpec
	TopKOrderTerm = db.TopKOrderTerm
)

// TopKWireGroup is one group's answer: the group columns' values, and its best rows in wire
// form, ordered best first.
//
// Rows are self-contained wire ads, owned by the caller -- decode one with
// wire.DecodeInline, or relay it. They are NOT decoded here, because a caller that is going
// to re-render them (a REPL printing a table, a relay forwarding them) would pay for a
// decode it throws away.
type TopKWireGroup struct {
	Values []classad.Value
	Rows   [][]byte
}

// TopKRowsPerGroup asks the server for the K best rows of each group over the default table.
func (c *Client) TopKRowsPerGroup(ctx context.Context, constraint string, spec TopKRowsSpec) ([]TopKWireGroup, error) {
	return c.TopKRowsPerGroupTable(ctx, DefaultTable, constraint, spec)
}

// TopKRowsPerGroupTable is TopKRowsPerGroup against a named table, history included.
//
// Against a server without FeatTopKRows it returns ErrTopKRowsUnsupported before any group, so
// a caller falls back to fetching and ranking itself rather than seeing an empty answer and
// believing nothing matched.
func (c *Client) TopKRowsPerGroupTable(ctx context.Context, table, constraint string, spec TopKRowsSpec) ([]TopKWireGroup, error) {
	if c.Supports(FeatTopKRows) == SupportNo {
		return nil, ErrTopKRowsUnsupported
	}
	build := func(id uint64) []byte {
		b := putStr(req(id, opTopKRows), table)
		b = putStr(b, constraint)
		b = append(b, 0) // redact: the server forces it on for an unprivileged session
		b = putI32(b, int32(len(spec.GroupCols)))
		for _, g := range spec.GroupCols {
			b = putStr(b, g.Attr)
		}
		b = putI32(b, int32(len(spec.OrderBy)))
		for _, t := range spec.OrderBy {
			b = putStr(b, t.Attr)
			d := byte(0)
			if t.Desc {
				d = 1
			}
			b = append(b, d)
		}
		b = putI32(b, int32(spec.K))
		b = putI32(b, int32(len(spec.Attrs)))
		for _, a := range spec.Attrs {
			b = putStr(b, a)
		}
		return b
	}
	_, ch, strm, err := c.callStream(build)
	if err != nil {
		return nil, err
	}
	var out []TopKWireGroup
	for {
		select {
		case <-ctx.Done():
			drain(ch)
			return nil, ctx.Err()
		case frame, ok := <-ch:
			if !ok {
				if serr := c.streamEnd(strm); serr != nil {
					return nil, serr
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
					switch body.u8() {
					case topKItemGroup:
						nv := int(body.i32())
						if body.err != nil || nv < 0 || nv > maxTopKGroupCols {
							drain(ch)
							return nil, errShort
						}
						g := TopKWireGroup{Values: make([]classad.Value, nv)}
						for j := 0; j < nv; j++ {
							v, nonScalar, vok := readExcValue(body)
							if !vok {
								drain(ch)
								return nil, errShort
							}
							if nonScalar {
								// The cell holds the group's TEXT, for a list or a
								// nested ad the value codec has no scalar form for.
								// Reading it back is exact: a group's text is a
								// ClassAd literal. Same rule as readAggRow.
								text, _ := v.StringValue() //nolint:errcheck // excExpr decodes to a string value
								v = db.ParseGroupValue(text)
							}
							g.Values[j] = v
						}
						out = append(out, g)
					case topKItemRow:
						row := body.bytesRef()
						if body.err != nil || len(out) == 0 {
							// A row before any group header is a frame this build
							// cannot place. Refuse rather than invent a group.
							drain(ch)
							return nil, errShort
						}
						// bytesRef aliases the frame, which is reused; a kept row is
						// copied. There are at most K per group, which is the point.
						out[len(out)-1].Rows = append(out[len(out)-1].Rows,
							append([]byte(nil), row...))
					default:
						drain(ch)
						return nil, errShort
					}
				}
				if body.err != nil {
					return nil, errShort
				}
			case stStreamEnd:
				return out, c.streamEnd(strm)
			default:
				drain(ch)
				return nil, statusErr(status, body)
			}
		}
	}
}

// maxTopKGroupCols bounds a reply's group arity so a malformed frame cannot make the client
// allocate from a length it was handed. The request's own arity is bounded on the server.
const maxTopKGroupCols = 1024

// streamTopKRows serves opTopKRows.
func (s *Server) streamTopKRows(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	constraint := r.str()
	redact := r.u8() != 0 || !includePrivate
	nGroup := int(r.i32())
	if nGroup < 0 || nGroup > maxTopKGroupCols {
		write(respBad(reqID))
		return
	}
	groups := make([]db.GroupCol, nGroup)
	for i := range groups {
		groups[i] = db.GroupCol{Attr: r.str()}
	}
	nOrder := int(r.i32())
	if nOrder <= 0 || nOrder > maxTopKGroupCols {
		write(respBad(reqID))
		return
	}
	order := make([]TopKOrderTerm, nOrder)
	for i := range order {
		order[i] = TopKOrderTerm{Attr: r.str(), Desc: r.u8() != 0}
	}
	k := int(r.i32())
	nAttrs := int(r.i32())
	if nAttrs < 0 || nAttrs > maxTopKGroupCols {
		write(respBad(reqID))
		return
	}
	attrs := make([]string, nAttrs)
	for i := range attrs {
		attrs[i] = r.str()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	spec := TopKRowsSpec{GroupCols: groups, OrderBy: order, K: k, Attrs: attrs}

	var stats *collections.ScanStats
	if s.queryStatsOn() {
		stats = &collections.ScanStats{}
	}
	obs := QueryLog{Op: "TopKRows", Table: table, Constraint: constraint, Groups: groups,
		Limit: k, Projection: len(attrs)}
	if qlog != nil {
		defer func() {
			if stats != nil {
				obs.Scan = *stats
			}
			obs.Duration = time.Since(start)
			qlog(obs)
		}()
	}

	// The private-attribute gates the aggregate path applies, for the same reasons. Note
	// they are NOT what keeps private attributes out of the ROWS -- the rows are stored
	// bytes and carry attributes nobody named, so redaction below does that. These refuse
	// the request outright when it ASKS for a private attribute, which is the clearer
	// answer than returning it as undefined.
	if !includePrivate {
		if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
			return
		}
		for _, g := range groups {
			if classad.IsPrivateAttribute(g.Attr) {
				write(respErr(reqID, "cannot group by private attribute "+g.Attr))
				return
			}
		}
		for _, t := range order {
			if classad.IsPrivateAttribute(t.Attr) {
				write(respErr(reqID, "cannot order by private attribute "+t.Attr))
				return
			}
		}
	}

	ref := s.resolveTable(table)
	var rows []db.TopKRowsWireGroup
	var err error
	switch ref.kind {
	case kindTable, kindView:
		if redact {
			rows, err = ref.db.TopKRowsPerGroupWireRedactedStats(constraint, spec, stats)
		} else {
			rows, err = ref.db.TopKRowsPerGroupWireStats(constraint, spec, stats)
		}
	case kindArchive:
		obs.Op = "ArchiveTopKRows"
		if redact {
			rows, err = ref.arch.TopKRowsPerGroupWireRedactedStats(constraint, spec, stats)
		} else {
			rows, err = ref.arch.TopKRowsPerGroupWireStats(constraint, spec, stats)
		}
	default:
		write(respErr(reqID, "no such table: "+table))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	obs.Rows = len(rows)
	if cancelled(ctx) {
		return
	}
	writeTopKRows(reqID, rows, write)
}

// writeTopKRows streams the groups and terminates the stream.
//
// Items batch to WireBatchBudget rather than one frame per group, because a group holds up to
// K rows and K is the caller's number: one frame per group would overrun MaxMessageSize for a
// large K, and one frame per ROW would send a frame per row for a small one. Batching by bytes
// is right at both ends, and a group simply continues into the next frame.
func writeTopKRows(reqID uint64, groups []db.TopKRowsWireGroup, write func([]byte)) {
	head := respHead(reqID, stStream)
	countAt := len(head)
	frame := make([]byte, 0, WireBatchBudget+countAt+64)
	items := 0
	begin := func() {
		frame = append(frame[:0], head...)
		frame = putI32(frame, 0) // item count, patched by flush
		items = 0
	}
	flush := func() {
		if items == 0 {
			return
		}
		binary.LittleEndian.PutUint32(frame[countAt:countAt+4], uint32(items))
		write(frame)
		begin()
	}
	payloadLen := func() int { return len(frame) - countAt - 4 }
	begin()
	for _, g := range groups {
		frame = append(frame, topKItemGroup)
		frame = putI32(frame, int32(len(g.Values)))
		for _, v := range g.Values {
			if isScalarGroup(v) {
				frame = appendExcValue(frame, v)
			} else {
				// A list or a nested ad crosses as its group TEXT, tagged excExpr,
				// exactly as appendAggRow sends one: the value codec has no form that
				// keeps such a value whole, and the group's text is a ClassAd literal
				// the client reads back exactly.
				frame = putStr(append(frame, excExpr), db.GroupValueText(v))
			}
		}
		items++
		for _, row := range g.Rows {
			// A group HEADER is never split from its own frame's start, but its rows
			// may land in later frames -- the client carries the open group across.
			if items > 0 && payloadLen()+5+len(row) > WireBatchBudget {
				flush()
			}
			frame = append(frame, topKItemRow)
			frame = putBytes(frame, row)
			items++
		}
	}
	flush()
	write(respHead(reqID, stStreamEnd))
}
