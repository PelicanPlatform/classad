package dbrpc

import (
	"context"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// ErrJoinUnsupported reports that the server does not implement the join opcode, so the
// caller should join the two sides itself. A server that predates opJoin rejects it as a bad
// request, which is what this maps -- a caller classifies with errors.Is rather than by
// matching prose.
var ErrJoinUnsupported = errors.New("dbrpc: the server does not implement a server-side join")

// JoinRequest is a planned two-table join to execute inside the store.
//
// Spec carries the plan the caller already made: which conjuncts went to which side, which
// equalities drive the hash, and what each side contributes to the joined row. Limits asks
// for bounds no larger than the server's own -- the server clamps, so asking for more gets
// the server's.
type JoinRequest struct {
	Spec   db.JoinSpec
	Limits db.JoinLimits
}

// Join streams the joined rows as old-ClassAd text, one per row, each a composite ad whose
// members are the two sides under their aliases.
//
// It reports ErrJoinUnsupported WITHOUT sending anything when the handshake already says the
// server cannot join. FeatJoin was introduced after feature advertisement existed, so a peer
// that advertised nothing cannot have the opcode and a peer that advertised a list without it
// definitely lacks it -- in both cases the request would buy a guaranteed round-trip
// rejection, on exactly the mixed-version link where that costs most.
//
// The rejection path is kept for the case advertisement cannot cover: a server that names the
// feature and then cannot honour it. That should not happen, since a server advertises
// FeatJoin only when it holds a catalog, but a definite yes that turns out wrong must still
// reach the caller as the same sentinel rather than as an opaque bad request.
//
// An error can arrive AFTER rows have been yielded, which is the other point: a join that met
// a limit part way through has produced a partial answer, and reading that as a complete one
// is the failure this reports. A caller that must not emit a partial result should not commit
// to its output until Join returns nil.
func (c *Client) Join(ctx context.Context, jr JoinRequest, yield func(row string) bool) error {
	if !c.SupportsNewer(FeatJoin) {
		return ErrJoinUnsupported
	}
	err := c.streamEach(ctx, joinFrame(jr), yield)
	if errors.Is(err, ErrBadRequest) {
		return ErrJoinUnsupported
	}
	return err
}

// JoinRows is Join collected into a slice, for a caller small enough not to care.
func (c *Client) JoinRows(ctx context.Context, jr JoinRequest) ([]string, error) {
	var out []string
	err := c.Join(ctx, jr, func(row string) bool { out = append(out, row); return true })
	return out, err
}

func joinFrame(jr JoinRequest) func(uint64) []byte {
	return func(id uint64) []byte {
		b := append(req(id, opJoin), byte(jr.Spec.Kind))
		b = putI32(b, int32(jr.Spec.Limit))
		b = putI32(b, int32(jr.Limits.MaxHashRows))
		b = putI32(b, int32(jr.Limits.MaxPairs))
		b = putI32(b, int32(jr.Limits.MaxRows))
		b = putJoinSide(b, jr.Spec.Left)
		b = putJoinSide(b, jr.Spec.Right)
		b = putStrs(b, jr.Spec.Cond)
		b = putStrs(b, jr.Spec.Post)
		b = putI32(b, int32(len(jr.Spec.Equi)))
		for _, e := range jr.Spec.Equi {
			b = putStr(putStr(b, e.Left), e.Right)
		}
		return b
	}
}

func putJoinSide(b []byte, s db.JoinSide) []byte {
	b = putStr(putStr(putStr(b, s.Table), s.Alias), s.Constraint)
	return putStrs(b, s.Project)
}

func putStrs(b []byte, ss []string) []byte {
	b = putI32(b, int32(len(ss)))
	for _, s := range ss {
		b = putStr(b, s)
	}
	return b
}

func readStrs(r *reader) []string {
	n := int(r.i32())
	if n <= 0 {
		return nil
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, r.str())
	}
	return out
}

func readJoinSide(r *reader) db.JoinSide {
	return db.JoinSide{
		Table:      r.str(),
		Alias:      r.str(),
		Constraint: r.str(),
		Project:    readStrs(r),
	}
}

// streamJoin serves opJoin.
func (s *Server) streamJoin(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	var jr JoinRequest
	jr.Spec.Kind = db.JoinKind(r.u8())
	jr.Spec.Limit = int(r.i32())
	jr.Limits.MaxHashRows = int(r.i32())
	jr.Limits.MaxPairs = int(r.i32())
	jr.Limits.MaxRows = int(r.i32())
	jr.Spec.Left = readJoinSide(r)
	jr.Spec.Right = readJoinSide(r)
	jr.Spec.Cond = readStrs(r)
	jr.Spec.Post = readStrs(r)
	nEqui := int(r.i32())
	for i := 0; i < nEqui; i++ {
		jr.Spec.Equi = append(jr.Spec.Equi, db.JoinEqui{Left: r.str(), Right: r.str()})
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}

	var stats db.JoinStats
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: "Join", Table: jr.Spec.Left.Table + "," + jr.Spec.Right.Table,
				Constraint: joinLogConstraint(jr.Spec), Limit: jr.Spec.Limit,
				Rows: stats.Rows, Duration: time.Since(start)})
		}()
	}

	// A private attribute must not leak through either side's constraint, the condition or
	// the residual filter -- each is an expression the server evaluates over real ads, so the
	// same gate the other read ops apply applies to all of them.
	for _, src := range joinExprs(jr.Spec) {
		if refusePrivateConstraint(reqID, src, includePrivate, write) {
			return
		}
	}

	jc, ok := s.cat.(joinCatalog)
	if !ok {
		write(respBad(reqID)) // this catalog serves no join; the client joins for itself
		return
	}
	seq, err := jc.Join(jr.Spec, jr.Limits, &stats)
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for ad := range seq {
		if cancelled(ctx) {
			return // client gone: stop the join
		}
		write(putStr(respHead(reqID, stStream), adString(ad, includePrivate)))
	}
	// A limit met part way through has already had rows sent, so it cannot be reported as a
	// refusal. It goes out as an error AFTER them, which the streaming client surfaces on a
	// partial result -- the alternative is a truncated join that reads as a complete one.
	if stats.Err != nil {
		write(respErr(reqID, stats.Err.Error()))
		return
	}
	write(respHead(reqID, stStreamEnd))
}

// joinCatalog is the optional Catalog capability to run a join. Optional for the same reason
// the view capabilities are: an implementation that cannot serve one reports so and the
// client joins the two sides itself, which is what it did before this opcode existed.
type joinCatalog interface {
	Join(spec db.JoinSpec, want db.JoinLimits, stats *db.JoinStats) (iter.Seq[*classad.ClassAd], error)
}

// joinExprs is every expression in the plan that the server will evaluate.
func joinExprs(spec db.JoinSpec) []string {
	out := make([]string, 0, len(spec.Cond)+len(spec.Post)+2*len(spec.Equi)+2)
	out = append(out, spec.Left.Constraint, spec.Right.Constraint)
	out = append(out, spec.Cond...)
	out = append(out, spec.Post...)
	for _, e := range spec.Equi {
		out = append(out, e.Left, e.Right)
	}
	return out
}

// joinLogConstraint renders the plan's predicates for the query log, so a slow join is
// identifiable by what it asked rather than only by which tables it touched.
func joinLogConstraint(spec db.JoinSpec) string {
	parts := make([]string, 0, 4)
	for _, p := range [...]string{spec.Left.Constraint, spec.Right.Constraint} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	parts = append(parts, spec.Cond...)
	parts = append(parts, spec.Post...)
	return strings.Join(parts, " && ")
}
