package dbrpc

import (
	"context"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
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

// streamQueryRaw streams matching ads as old-ClassAd wire text, rendered from the
// db QueryRaw pushdown (no AST decode), one frame per ad like streamQuery.
func (s *Server) streamQueryRaw(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte)) {
	table := r.str()
	limit := int(r.i32())
	constraint := r.str()
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	d, ok := s.tableOr(reqID, table, write)
	if !ok {
		return
	}
	seq, err := d.QueryRaw(constraint)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	n := 0
	for ra := range seq {
		if cancelled(ctx) {
			return
		}
		write(putStr(respHead(reqID, stStream), rawAdToOldText(ra, includePrivate)))
		n++
		if limit > 0 && n >= limit {
			break
		}
	}
	write(respHead(reqID, stStreamEnd))
}

// rawAdToOldText renders a RawAd as old-ClassAd wire text: the type tags as their
// own lines followed by the attribute expression lines, dropping private
// attributes unless includePrivate. It touches no AST -- the expression bytes are
// copied straight from the stored form.
func rawAdToOldText(ra collections.RawAd, includePrivate bool) string {
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
		if !includePrivate && classad.IsPrivateAttribute(rawExprName(e)) {
			continue
		}
		b.Write(e)
		b.WriteByte('\n')
	}
	return b.String()
}

// rawExprName returns the attribute name of a rendered "Name = value" expression
// line (leading whitespace trimmed, up to the first '=' or space).
func rawExprName(expr []byte) string {
	i := 0
	for i < len(expr) && (expr[i] == ' ' || expr[i] == '\t') {
		i++
	}
	start := i
	for i < len(expr) && expr[i] != '=' && expr[i] != ' ' && expr[i] != '\t' {
		i++
	}
	return string(expr[start:i])
}
