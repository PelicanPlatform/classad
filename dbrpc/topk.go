package dbrpc

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// ErrTopKUnsupported is returned by TopK against a server too old to implement the opcode, so the
// caller falls back to fetching the match set and ordering it client-side.
var ErrTopKUnsupported = errors.New("dbrpc: server does not support server-side TopK")

// TopK returns the k rows matching constraint ordered by orderAttr -- the largest k when desc, the
// smallest k otherwise -- each projected to attrs and rendered as old-ClassAd text, in sorted order
// (best first). It is the wire form of db.DB/ArchiveTable.TopK: the server holds and returns only k
// rows, so ORDER BY <orderAttr> {DESC|ASC} LIMIT k does not ship the whole match set to be sorted at
// the far end. Parse each returned row with classad.ParseOld.
//
// orderAttr must resolve to a number; a matching row whose order value is not numeric is skipped. If
// orderAttr is not among attrs it is used only for the ordering and is not included in the returned
// rows. Against a server without the opcode it returns ErrTopKUnsupported.
func (c *Client) TopK(ctx context.Context, table, constraint string, attrs []string, orderAttr string, desc bool, k int) ([]string, error) {
	rows, err := c.streamCtx(ctx, topKFrame(table, constraint, attrs, orderAttr, desc, k))
	return rows, topKErr(err)
}

// topKFrame builds the opTopK request frame.
func topKFrame(table, constraint string, attrs []string, orderAttr string, desc bool, k int) func(uint64) []byte {
	return func(id uint64) []byte {
		b := putStr(putStr(req(id, opTopK), table), constraint)
		b = putStr(b, orderAttr)
		var d byte
		if desc {
			d = 1
		}
		b = append(b, d)
		b = putI32(b, int32(k))
		b = putI32(b, int32(len(attrs)))
		for _, a := range attrs {
			b = putStr(b, a)
		}
		return b
	}
}

// topKErr maps the server's rejection of an unknown opcode to a sentinel the caller can fall back
// on, without matching message text.
func topKErr(err error) error {
	if errors.Is(err, ErrBadRequest) {
		return ErrTopKUnsupported
	}
	return err
}

// streamTopK serves opTopK: it resolves the table (mutable, view backing, or archive), runs the
// server-side top-K, and streams the k projected rows as old-ClassAd text, best-first.
func (s *Server) streamTopK(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	constraint := r.str()
	orderAttr := r.str()
	desc := r.u8() != 0
	k := int(r.i32())
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "TopK", Table: table, Constraint: constraint, Limit: k, Rows: n, Duration: time.Since(start)})
		}()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	// Same private-attribute protection as the projection ops: no private ref in the constraint,
	// and -- unprivileged -- no private attribute projected or used as the order key (TopK's
	// underlying QueryProject does not redact, so the gate is here).
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}
	if !includePrivate {
		if bad := firstPrivateAttr(attrs, orderAttr); bad != "" {
			write(respErr(reqID, "cannot reference private attribute "+bad))
			return
		}
	}
	var rows [][]classad.Value
	var err error
	if d, ok := s.cat.Table(table); ok {
		rows, err = d.TopK(constraint, attrs, orderAttr, desc, k)
	} else if d, ok := s.cat.ViewBacking(table); ok {
		rows, err = d.TopK(constraint, attrs, orderAttr, desc, k)
	} else if a, ok := s.cat.ArchiveTable(table); ok {
		rows, err = a.TopK(constraint, attrs, orderAttr, desc, k)
	} else {
		write(respErr(reqID, "no such table: "+table))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for _, row := range rows {
		if cancelled(ctx) {
			return
		}
		write(putStr(respHead(reqID, stStream), oldClassAdRow(attrs, row)))
		n++
	}
	write(respHead(reqID, stStreamEnd))
}

// firstPrivateAttr returns the first private attribute name among attrs or orderAttr, or "".
func firstPrivateAttr(attrs []string, orderAttr string) string {
	for _, a := range attrs {
		if classad.IsPrivateAttribute(a) {
			return a
		}
	}
	if classad.IsPrivateAttribute(orderAttr) {
		return orderAttr
	}
	return ""
}

// oldClassAdRow renders a projected top-K row (values aligned to attrs) as old-ClassAd text, one
// "Attr = value" line per attribute. An undefined or error value (an attribute absent from the
// row) is omitted, exactly as a projection omits attributes an ad does not carry, so the parsed ad
// simply lacks it. classad.Value.String() emits ClassAd-parseable literals for the scalar types a
// projection column holds (integer, real, boolean, quoted string).
func oldClassAdRow(attrs []string, row []classad.Value) string {
	var b strings.Builder
	for i, a := range attrs {
		if i >= len(row) {
			break
		}
		v := row[i]
		if v.IsUndefined() || v.IsError() {
			continue
		}
		b.WriteString(a)
		b.WriteString(" = ")
		b.WriteString(v.String())
		b.WriteByte('\n')
	}
	return b.String()
}
