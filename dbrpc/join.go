package dbrpc

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"iter"
	"strings"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
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
	return c.JoinStats(ctx, jr, yield, nil)
}

// joinOpFor picks the opcode and the feature a plan needs, from the plan alone.
//
// A spec carrying steps is a chain and can only go to the chain opcode; anything else is the
// two-table request that has always been sent, byte for byte. Deciding here rather than at
// each call site is what keeps a caller from having to know there are two.
func joinOpFor(jr JoinRequest, pair, chain op, pairFeat, chainFeat Feature) (op, Feature) {
	if len(jr.Spec.Steps) > 0 {
		return chain, chainFeat
	}
	return pair, pairFeat
}

// JoinRows is Join collected into a slice, for a caller small enough not to care.
func (c *Client) JoinRows(ctx context.Context, jr JoinRequest) ([]string, error) {
	var out []string
	err := c.Join(ctx, jr, func(row string) bool { out = append(out, row); return true })
	return out, err
}

// joinFrameOp encodes a join request for either opcode. The plan is encoded ONCE here, so the
// two ops cannot drift into two spellings of the same statement; opJoinWire's frame is this
// one with a redact byte on the end.
//
// wantStats appends the statistics byte, and only when the caller asked for them: a request
// that does not ask is byte-for-byte the request every client has always sent, so a server
// cannot tell a build that knows about the trailer from one that does not.
func joinFrameOp(jr JoinRequest, o op, withRedact, redact, wantStats bool) func(uint64) []byte {
	return func(id uint64) []byte {
		b := appendJoinPlan(req(id, o), jr, joinOpCarriesChain(o))
		if withRedact {
			b = putU8(b, boolByte(redact))
		}
		if wantStats {
			b = putU8(b, 1)
		}
		return b
	}
}

// readJoinStatsByte reads the trailing byte that asks for the statistics trailer. Absent on
// every request from a client that predates it, which is why it is read as optional rather
// than as part of the plan: a shorter frame is a well-formed request, not a malformed one.
func readJoinStatsByte(r *reader) bool {
	v, ok := r.optU8()
	return ok && v != 0
}

// appendJoinPlan writes the plan itself, with the body layout named rather than derived from
// an opcode. It is the mirror of readJoinRequest, which takes the same bool for the same
// reason: an opcode that carries a plan as part of a LARGER request -- opJoinAgg, whose body
// continues with the reduction -- has no entry in joinOpCarriesChain and says which layout it
// is sending instead.
func appendJoinPlan(b []byte, jr JoinRequest, chain bool) []byte {
	b = append(b, byte(jr.Spec.Kind))
	b = putI32(b, int32(jr.Spec.Limit))
	b = putI32(b, int32(jr.Limits.MaxHashRows))
	b = putI32(b, int32(jr.Limits.MaxPairs))
	b = putI32(b, int32(jr.Limits.MaxRows))
	b = putJoinSide(b, jr.Spec.Left)
	if chain {
		b = putStrs(b, jr.Spec.Post)
		b = putI32(b, int32(len(jr.Spec.Steps)))
		for i := range jr.Spec.Steps {
			st := &jr.Spec.Steps[i]
			b = append(b, byte(st.Kind))
			b = putJoinSide(b, st.Side)
			b = putStrs(b, st.Cond)
			b = putJoinEqui(b, st.Equi)
		}
		return b
	}
	b = putJoinSide(b, jr.Spec.Right)
	b = putStrs(b, jr.Spec.Cond)
	b = putStrs(b, jr.Spec.Post)
	return putJoinEqui(b, jr.Spec.Equi)
}

func putJoinEqui(b []byte, eq []db.JoinEqui) []byte {
	b = putI32(b, int32(len(eq)))
	for _, e := range eq {
		b = putStr(putStr(b, e.Left), e.Right)
	}
	return b
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
func (s *Server) streamJoin(ctx context.Context, reqID uint64, r *reader, includePrivate, chain bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	jr, ok := readJoinRequest(r, chain)
	if !ok {
		write(respBad(reqID))
		return
	}

	wantStats := readJoinStatsByte(r)

	var stats db.JoinStats
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: joinOpName("Join", chain), Table: joinTables(jr.Spec),
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
	if refusePrivateJoinRef(reqID, jr.Spec, includePrivate, write) {
		return
	}

	jc, ok := s.cat.(joinCatalog)
	if !ok || (chain && !catalogJoinsChains(s.cat)) {
		write(respBad(reqID)) // this catalog serves no such join; the client joins for itself
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
	// The statistics go out BEFORE whatever ends the stream, including before the error
	// below: a client stops reading at an error, so a trailer written after one would never
	// be seen -- and a join that stopped at a limit is the one whose access path a reader
	// most wants to know.
	writeJoinStats(reqID, wantStats, stats, write)
	// A limit met part way through has already had rows sent, so it cannot be reported as a
	// refusal. It goes out as an error AFTER them, which the streaming client surfaces on a
	// partial result -- the alternative is a truncated join that reads as a complete one.
	if stats.Err != nil {
		write(respErr(reqID, stats.Err.Error()))
		return
	}
	write(respHead(reqID, stStreamEnd))
}

// writeJoinStats sends the statistics trailer when the request asked for one. It is called at
// the point the join has stopped producing rows and nowhere earlier: the counts are not final
// until then, and a reader cannot tell an early reading from a final one.
func writeJoinStats(reqID uint64, want bool, stats db.JoinStats, write func([]byte)) {
	if !want {
		return
	}
	if frame := joinStatsTrailer(reqID, stats); frame != nil {
		write(frame)
	}
}

// streamJoinWire serves opJoinWire: the same plan as opJoin, answered with wire-form rows
// batched into frames of up to WireBatchBudget payload bytes.
//
// A single over-budget row still gets its own frame, which is exactly opJoin's
// one-frame-per-row behaviour -- so a jumbo joined row is never unshippable.
func (s *Server) streamJoinWire(ctx context.Context, reqID uint64, r *reader, includePrivate, chain bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	jr, ok := readJoinRequest(r, chain)
	if !ok {
		write(respBad(reqID))
		return
	}
	// An unprivileged connection is redacted whatever it asked for; a privileged one gets
	// what it asked for. Same rule the text reply's render follows, stated here because this
	// reply does not go through a render.
	redact := r.u8() != 0 || !includePrivate
	if r.err != nil {
		write(respBad(reqID))
		return
	}
	wantStats := readJoinStatsByte(r)

	var stats db.JoinStats
	if qlog != nil {
		defer func() {
			qlog(QueryLog{Op: joinOpName("JoinWire", chain), Table: joinTables(jr.Spec),
				Constraint: joinLogConstraint(jr.Spec), Limit: jr.Spec.Limit,
				Rows: stats.Rows, Duration: time.Since(start)})
		}()
	}
	for _, src := range joinExprs(jr.Spec) {
		if refusePrivateConstraint(reqID, src, includePrivate, write) {
			return
		}
	}
	if refusePrivateJoinRef(reqID, jr.Spec, includePrivate, write) {
		return
	}
	jc, ok := s.cat.(joinWireCatalog)
	if !ok || (chain && !catalogJoinsChains(s.cat)) {
		write(respBad(reqID)) // this catalog serves no such join; the client takes the text one
		return
	}
	seq, err := jc.JoinWire(jr.Spec, jr.Limits, redact, &stats)
	if errors.Is(err, db.ErrJoinWireRedactNeedsProjection) {
		// Not a failure: there is no projection to withhold the private attributes from, so
		// this refuses rather than shipping them. Refused the way an older server refuses
		// the opcode, so the client takes the text reply, whose render withholds them.
		write(respBad(reqID))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}

	// Rows build directly into one reused frame buffer behind a reserved header (the row
	// count is patched at flush), so a steady stream allocates nothing and copies each row
	// exactly once -- write hands the buffer to a synchronous, non-retaining WriteMsg.
	head := respHead(reqID, stStream)
	countAt := len(head)
	frame := make([]byte, 0, WireBatchBudget+countAt+64)
	begin := func() {
		frame = append(frame[:0], head...)
		frame = putI32(frame, 0)
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
	}
	flush()
	writeJoinStats(reqID, wantStats, stats, write)
	// A limit met part way through has already had rows sent, so it cannot be a refusal. It
	// goes out as an error AFTER them, which the streaming client surfaces on a partial
	// result -- the alternative is a truncated join that reads as a complete one.
	if stats.Err != nil {
		write(respErr(reqID, stats.Err.Error()))
		return
	}
	write(respHead(reqID, stStreamEnd))
}

// readJoinRequest decodes the plan every join opcode carries. ONE decoder, so no two of them
// can drift into reading the same bytes differently, and it is the mirror of joinFrameOp --
// the two share both the prefix and the test that picks the body.
//
// maxJoinChainSides bounds what one request may ask to be joined, because the step count is
// read off the wire before anything is allocated for it.
func readJoinRequest(r *reader, chain bool) (JoinRequest, bool) {
	var jr JoinRequest
	jr.Spec.Kind = db.JoinKind(r.u8())
	jr.Spec.Limit = int(r.i32())
	jr.Limits.MaxHashRows = int(r.i32())
	jr.Limits.MaxPairs = int(r.i32())
	jr.Limits.MaxRows = int(r.i32())
	jr.Spec.Left = readJoinSide(r)
	if !chain {
		jr.Spec.Right = readJoinSide(r)
		jr.Spec.Cond = readStrs(r)
		jr.Spec.Post = readStrs(r)
		jr.Spec.Equi = readJoinEqui(r)
		return jr, r.err == nil
	}
	// A chain's kind byte is written and ignored; the kinds that matter are per step.
	jr.Spec.Kind = db.JoinInner
	jr.Spec.Post = readStrs(r)
	nSteps := int(r.i32())
	if r.err != nil || nSteps < 1 || nSteps > maxJoinChainSides-1 {
		return jr, false
	}
	for i := 0; i < nSteps; i++ {
		st := db.JoinStep{Kind: db.JoinKind(r.u8())}
		st.Side = readJoinSide(r)
		st.Cond = readStrs(r)
		st.Equi = readJoinEqui(r)
		jr.Spec.Steps = append(jr.Spec.Steps, st)
	}
	return jr, r.err == nil
}

// maxJoinChainSides caps the tables one request may join. It bounds an allocation made from a
// count read off the wire, and it is generous: a chain past this is a query shape nobody
// writes, and the pair budget would stop it long before the side count did.
const maxJoinChainSides = 16

func readJoinEqui(r *reader) []db.JoinEqui {
	n := int(r.i32())
	if n <= 0 {
		return nil
	}
	out := make([]db.JoinEqui, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, db.JoinEqui{Left: r.str(), Right: r.str()})
	}
	return out
}

// joinOpName labels the query log, so a slow chain is not filed under the two-table op.
func joinOpName(base string, chain bool) string {
	if chain {
		return base + "Chain"
	}
	return base
}

// joinChainCatalog is the optional capability to join MORE THAN TWO tables.
//
// It is a separate interface, and a marker method rather than a second Join, for the reason
// FeatJoinWire has its own: the chain opcode must be advertised only by a catalog that can
// really serve one. A catalog from a build whose join predates chains satisfies joinCatalog
// perfectly well and would accept the request and then answer the wrong query -- a definite
// yes that is wrong, which is the situation advertisement exists to prevent.
type joinChainCatalog interface {
	JoinsChains() bool
}

// catalogJoinsChains reports whether this server's catalog can join more than two tables.
func catalogJoinsChains(cat any) bool {
	jc, ok := cat.(joinChainCatalog)
	return ok && jc.JoinsChains()
}

// joinCatalog is the optional Catalog capability to run a join. Optional for the same reason
// the view capabilities are: an implementation that cannot serve one reports so and the
// client joins the two sides itself, which is what it did before this opcode existed.
type joinCatalog interface {
	Join(spec db.JoinSpec, want db.JoinLimits, stats *db.JoinStats) (iter.Seq[*classad.ClassAd], error)
}

// joinExplainCatalog is the optional Catalog capability to report how a join WOULD run.
//
// Separate from joinCatalog because running a join and describing the plan are different
// abilities: every catalog that joins makes these decisions, and only one that has been
// taught to name them can answer this.
type joinExplainCatalog interface {
	ExplainJoin(spec db.JoinSpec, want db.JoinLimits) (db.JoinExplain, error)
}

// explainJoin answers opJoinExplain / opJoinChainExplain: how the catalog would run the
// join, with no records read.
//
// It goes through the same private-attribute gate the join itself does. The report names
// the plan's expressions back to the caller, so a caller barred from referencing a private
// attribute in a condition must not be able to get it echoed out of the explainer either.
func (s *Server) explainJoin(reqID uint64, r *reader, includePrivate, chain bool) []byte {
	jr, ok := readJoinRequest(r, chain)
	if !ok {
		return respBad(reqID)
	}
	cat, ok := s.cat.(joinExplainCatalog)
	if !ok {
		return respBad(reqID)
	}
	// BOTH gates the join itself applies, for the same reasons and in the same order. The
	// first catches a bare reference, which is how a side's OWN constraint is written; the
	// second catches an alias-qualified one, which is how a condition over the joined row
	// is. Applying only the second let a private attribute through in a side constraint.
	var refused []byte
	catch := func(b []byte) { refused = b }
	for _, src := range joinExprs(jr.Spec) {
		if refusePrivateConstraint(reqID, src, includePrivate, catch) {
			return refused
		}
	}
	if refusePrivateJoinRef(reqID, jr.Spec, includePrivate, catch) {
		return refused
	}
	ex, err := cat.ExplainJoin(jr.Spec, jr.Limits)
	if err != nil {
		return respErr(reqID, err.Error())
	}
	data, err := json.Marshal(ex)
	if err != nil {
		return respErr(reqID, err.Error())
	}
	return putStr(resp(reqID, stOK), string(data))
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
	// Every step is an expression the server will evaluate too. Missing one here would let
	// a private attribute through the gate by the simple expedient of putting it in a third
	// table's constraint, which is exactly the hole this closes.
	for i := range spec.Steps {
		st := &spec.Steps[i]
		out = append(out, st.Side.Constraint)
		out = append(out, st.Cond...)
		for _, e := range st.Equi {
			out = append(out, e.Left, e.Right)
		}
	}
	return out
}

// joinAliases lists the aliases the plan's expressions may qualify a reference with.
func joinAliases(spec db.JoinSpec) []string {
	out := make([]string, 0, len(spec.Steps)+2)
	out = append(out, spec.Left.Alias)
	if spec.Right.Alias != "" {
		out = append(out, spec.Right.Alias)
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		out = append(out, st.Side.Alias)
	}
	return out
}

// refusePrivateJoinRef refuses a join expression that reaches a private attribute THROUGH AN
// ALIAS -- `h.ClaimId == "x"` -- which the ordinary constraint gate does not see.
//
// It does not see it because that gate walks the expression for attribute REFERENCES, and
// `h.ClaimId` is a select whose member name is a plain string field on the node rather than a
// reference of its own. So the name is never visited, and the check passes.
//
// What the hole is worth to a caller is an ORACLE, not a disclosure: the reply is still
// redacted, so the value does not come back, but the caller chooses which rows come back
// using it and can narrow a private attribute down by asking repeatedly. Nothing about the
// answer looks wrong, which is why this needs a test of its own rather than a functional one.
//
// It covers the terms the alias compiler can account for in full. A term it delegates can
// still reach a side in ways no list of names describes -- that residue is not closed here,
// and closing it means refusing those terms outright for an unprivileged caller.
func refusePrivateJoinRef(reqID uint64, spec db.JoinSpec, includePrivate bool, write func([]byte)) bool {
	if includePrivate {
		return false
	}
	return refusePrivateAliasExprs(reqID, joinAliases(spec), joinExprs(spec), includePrivate, write)
}

// refusePrivateAliasExprs is the alias-qualified private-attribute gate over an arbitrary set
// of expressions evaluated against a composite row. refusePrivateJoinRef is this over the
// plan's own expressions; a join aggregate runs it a second time over the GROUP BY columns and
// the aggregate arguments and filters, which are expressions over the same row and are not in
// the plan.
//
// Those are worth more to a caller than a condition is. A condition that reaches a private
// attribute is an oracle -- the value does not come back, the caller narrows it down by asking
// repeatedly. A GROUP BY column that reaches one IS the value: the group's label is what the
// expression evaluated to, so the aggregate would report it directly.
func refusePrivateAliasExprs(reqID uint64, aliases, srcs []string, includePrivate bool, write func([]byte)) bool {
	if includePrivate {
		return false
	}
	for _, src := range srcs {
		if strings.TrimSpace(src) == "" {
			continue
		}
		q, err := vm.ParseAliases(src, aliases)
		if err != nil || !q.AliasNative() {
			continue
		}
		for _, r := range q.AliasRefs() {
			if classad.IsPrivateAttribute(r.Name) {
				write(respErr(reqID, "cannot reference private attribute "+r.Name+
					" in a join condition"))
				return true
			}
		}
	}
	return false
}

// joinTables names every table the plan reads, for the query log.
func joinTables(spec db.JoinSpec) string {
	names := make([]string, 0, len(spec.Steps)+2)
	names = append(names, spec.Left.Table)
	if spec.Right.Table != "" {
		names = append(names, spec.Right.Table)
	}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		names = append(names, st.Side.Table)
	}
	return strings.Join(names, ",")
}

// joinLogConstraint renders the plan's predicates for the query log, so a slow join is
// identifiable by what it asked rather than only by which tables it touched.
func joinLogConstraint(spec db.JoinSpec) string {
	parts := make([]string, 0, 4)
	cons := []string{spec.Left.Constraint, spec.Right.Constraint}
	for i := range spec.Steps {
		st := &spec.Steps[i]
		cons = append(cons, st.Side.Constraint)
	}
	for _, p := range cons {
		if p != "" {
			parts = append(parts, p)
		}
	}
	parts = append(parts, spec.Cond...)
	for i := range spec.Steps {
		st := &spec.Steps[i]
		parts = append(parts, st.Cond...)
	}
	parts = append(parts, spec.Post...)
	return strings.Join(parts, " && ")
}

// ErrJoinWireUnsupported reports that the server cannot stream wire-form joined rows, so the
// caller should use Join and take the text rows. It is delivered BEFORE any row, so a caller
// that has already relayed rows can treat it as a hard failure rather than a fallback.
//
// Three causes, one response: a server too old to know opJoinWire, a catalog that cannot
// produce wire rows, and a redacted request over a side whose whole ad was asked for -- which
// has no projection to withhold from (see db.ErrJoinWireRedactNeedsProjection).
var ErrJoinWireUnsupported = errors.New("dbrpc: wire-form joined rows unavailable for this request")

// joinWireCatalog is the optional Catalog capability to join into wire rows. Separate from
// joinCatalog because a catalog could implement one and not the other, and advertising on the
// strength of the wrong one is a definite yes that fails later.
type joinWireCatalog interface {
	JoinWire(spec db.JoinSpec, want db.JoinLimits, redact bool, stats *db.JoinStats) (iter.Seq[[]byte], error)
}

// JoinWire streams the joined rows as wire-form rows -- self-contained inline-names composite
// ads -- batched many rows per frame.
//
// The row slice passed to yield ALIASES THE FRAME BUFFER and is valid only until yield
// returns. A caller that keeps one must copy it. That is the same contract
// QueryRawWireStream has, and it is what lets a steady stream allocate nothing.
//
// redact withholds each side's private attributes. An unprivileged connection is always
// redacted, whatever is asked for.
//
// It returns ErrJoinWireUnsupported, before any row, when the stream is unavailable. An empty
// stream means the join produced no rows; it never means the server could not answer.
func (c *Client) JoinWire(ctx context.Context, jr JoinRequest, redact bool, yield func(row []byte) bool) error {
	return c.joinWire(ctx, jr, redact, yield, nil)
}

// joinWire is JoinWire with the statistics report threaded through it; JoinWireStats is the
// exported form. One body rather than two, so the two entry points cannot diverge in what
// they do with a short stream.
func (c *Client) joinWire(ctx context.Context, jr JoinRequest, redact bool, yield func(row []byte) bool, rep *JoinStatsReport) error {
	o, feat := joinOpFor(jr, opJoinWire, opJoinChainWire, FeatJoinWire, FeatJoinChainWire)
	wantStats := c.wantJoinStats(rep)
	if !c.SupportsNewer(feat) {
		return ErrJoinWireUnsupported
	}
	_, ch, strm, err := c.callStream(joinFrameOp(jr, o, true, redact, wantStats))
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
			case stStreamJoinStats:
				rep.take(body)
			case stBadReq:
				// A server too old to know the opcode rejects the request this way, and so
				// does one whose catalog cannot serve wire rows, and so does a redacted
				// whole-ad request: same fallback for all three.
				return ErrJoinWireUnsupported
			default:
				return statusErr(status, body)
			}
		}
	}
}
