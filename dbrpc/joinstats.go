package dbrpc

// Carrying a join's execution statistics back to the client.
//
// db.Join already records how it ran: which attribute each step's scan was narrowed on, how
// many keys it was narrowed to, and -- when it was not narrowed -- why not. That last field
// exists because a plan that silently did not use an index reads exactly like one that had
// none, and the two want different fixes.
//
// None of it crossed. The server filled a db.JoinStats, wrote its row count into the query
// log and dropped the rest, so a client that asked the store to run a join could not find out
// what the store did. The cost rule that chooses a store-side join is the thing that most
// needs watching, and its outcome was the one thing unobservable from the side that invokes
// it -- the same defect ProbeWhy was written to prevent, one layer up.
//
// The statistics are final only when the join finishes, so they ride a trailer after the last
// row rather than a header before the first, and a stream that ends early carries none. That
// is the second half of the contract: partial statistics that read as complete would be worse
// than none, because a reader would draw conclusions from a count that stopped at a limit.

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/PelicanPlatform/classad/db"
)

// JoinStatsReport is what a server said about how it ran a join.
//
// The three booleans are the point of the type. A db.JoinStats on its own cannot say that it
// was never filled: a zero ProbeKeys with an empty ProbeWhy is what a server that did not
// report anything leaves behind, and it is also what a join that narrowed on nothing and gave
// no reason would leave behind. A caller that reads Stats without reading Reported first is
// back to the ambiguity this exists to remove.
type JoinStatsReport struct {
	// Stats is the join's own record of what it did. It means something only when Reported
	// is true; until then it is a zero value that was never written to.
	Stats db.JoinStats

	// Reported says the server sent its statistics and they were decoded. False means the
	// statistics did not cross -- NOT that the join used no index.
	//
	// Three things make it false: the peer does not have the feature (Unsupported), the
	// stream ended before the trailer, and a consumer that stopped yielding early, which
	// abandons the rest of the reply including the trailer.
	Reported bool

	// Complete says the statistics describe the WHOLE join: the trailer arrived and the
	// stream ran to its end without an error.
	//
	// A join that met a limit part way through still reports -- its access path is exactly
	// what a reader wants to see -- but its counts stopped where the limit did, and
	// presenting those as a finished join's would overstate what was read.
	Complete bool

	// Unsupported says the peer does not advertise FeatJoinStats, so nothing was asked for
	// and nothing came back. It is the one cause of a missing report that is a property of
	// the peer rather than of this call, and it is the answer a diagnostic should print:
	// "this store does not report join statistics" is a different sentence from "the join
	// was cut short before they arrived".
	//
	// It is never set alongside Reported. A trailer that arrives from a peer that advertised
	// nothing is still a report, and the arrival is the better evidence.
	Unsupported bool

	// Err is the text of a failure the join met WHILE streaming, empty when it met none.
	//
	// Text, not an error: the typed error is what the call returns, and reconstructing a
	// second copy here would be one that errors.Is cannot match -- a caller comparing it
	// against a sentinel would get a wrong answer rather than no answer.
	Err string
}

// begin resets a report at the start of a call. The nil receiver is the caller that asked for
// no statistics, which keeps every call site free of a branch.
func (r *JoinStatsReport) begin(supported bool) {
	if r == nil {
		return
	}
	*r = JoinStatsReport{Unsupported: !supported}
}

// take decodes a trailer. A trailer that does not decode leaves the report unreported, which
// is the honest answer: half a record is not a record.
//
// It clears Unsupported, because a trailer that arrived settles the question the handshake was
// only predicting: whatever the peer advertised, it reported. Leaving both set would hand a
// reader a report that says it both arrived and could not have.
func (r *JoinStatsReport) take(rd *reader) {
	if r == nil {
		return
	}
	st, errText, ok := readJoinStats(rd)
	if !ok {
		return
	}
	r.Stats, r.Err, r.Reported, r.Unsupported = st, errText, true, false
}

// finish records whether the stream reached its end. Complete is set here and nowhere else,
// so there is one place that decides what "these describe the whole join" means.
func (r *JoinStatsReport) finish(err error) {
	if r == nil {
		return
	}
	r.Complete = r.Reported && err == nil
}

// wantJoinStats reports whether this call should ask for the trailer: the caller wants it and
// the peer can send it. Asking a peer that cannot would cost nothing on the wire -- the byte
// is ignored -- and would be read back as a join that reported nothing, which is the confusion
// Unsupported exists to prevent.
func (c *Client) wantJoinStats(rep *JoinStatsReport) bool {
	supported := c.SupportsNewer(FeatJoinStats)
	rep.begin(supported)
	return rep != nil && supported
}

// joinStatsWire is what the trailer carries: db.JoinStats as the join filled it, with Err
// rendered as text.
//
// The statistics are embedded and marshalled WHOLE rather than copied field by field, so a
// field added to db.JoinStats crosses without this file being edited. A mirror struct here
// would go stale silently, and a statistic that quietly stopped crossing is precisely the
// failure the trailer exists to prevent.
//
// Err is the one field that cannot ride that way, because an error does not unmarshal back
// into an interface. The outer field SHADOWS the embedded one -- encoding/json resolves a name
// collision in favour of the shallower field, which is why this one must carry the same JSON
// name -- and carries the text instead. The embedded Err is left as the join set it and is
// simply never visited.
type joinStatsWire struct {
	db.JoinStats
	Err string `json:"Err,omitempty"`
}

// joinStatsTrailer builds the trailer frame, or nil when the statistics cannot be encoded.
//
// Nil rather than an error frame: rows have already been written by the time this is called,
// so failing the query over a diagnostic would turn a complete answer into a failed one. The
// client sees no trailer and reports nothing, which is the truth.
func joinStatsTrailer(reqID uint64, st db.JoinStats) []byte {
	w := joinStatsWire{JoinStats: st}
	if st.Err != nil {
		w.Err = st.Err.Error()
	}
	data, err := json.Marshal(w)
	if err != nil {
		return nil
	}
	return putStr(respHead(reqID, stStreamJoinStats), string(data))
}

// readJoinStats decodes a trailer written by joinStatsTrailer, reporting whether it decoded.
func readJoinStats(r *reader) (db.JoinStats, string, bool) {
	body := r.bytesRef()
	if r.err != nil {
		return db.JoinStats{}, "", false
	}
	var w joinStatsWire
	if err := json.Unmarshal(body, &w); err != nil {
		return db.JoinStats{}, "", false
	}
	return w.JoinStats, w.Err, true
}

// JoinStats is Join that also fills rep with how the server ran the join.
//
// rep is filled whatever happens, including when nothing crossed: a caller reads Reported
// before Stats, and Unsupported tells it which kind of nothing it got. Stats are asked for
// only when the peer advertises FeatJoinStats, so this costs a silent peer nothing at all and
// an advertising one one frame per query -- not per row.
//
// Everything else is Join's contract, including that an error can arrive after rows have been
// yielded. When one does, rep.Complete is false and rep.Stats describes the join up to the
// point it stopped.
func (c *Client) JoinStats(ctx context.Context, jr JoinRequest, yield func(row string) bool, rep *JoinStatsReport) error {
	o, layout, err := c.joinSend(jr, opJoin, opJoinChain, opJoinBand, FeatJoin, FeatJoinChain,
		ErrJoinUnsupported)
	if err != nil {
		return err
	}
	want := c.wantJoinStats(rep)
	err = c.streamEachTrailer(ctx, joinFrameOp(jr, o, layout, false, false, want), yield,
		stStreamJoinStats, rep.take)
	if errors.Is(err, ErrBadRequest) {
		err = ErrJoinUnsupported
	}
	rep.finish(err)
	return err
}

// JoinWireStats is JoinWire that also fills rep. See JoinStats for what the report says and
// what each of its answers means.
func (c *Client) JoinWireStats(ctx context.Context, jr JoinRequest, redact bool, yield func(row []byte) bool, rep *JoinStatsReport) error {
	err := c.joinWire(ctx, jr, redact, yield, rep)
	rep.finish(err)
	return err
}

// JoinAggregateStats is JoinAggregate that also fills rep with how the server ran the join
// under the reduction. The join's statistics are the same ones the row-streaming join reports;
// Groups additionally says how many groups the reduction produced.
//
// A reduction is produced whole before any of it is written, so a failure arrives before the
// groups and before the trailer: an error from this call means Reported is false, where the
// row-streaming join can report a join that failed part way.
func (c *Client) JoinAggregateStats(ctx context.Context, jr JoinAggRequest, rep *JoinStatsReport) ([]db.AggRow, error) {
	rows, err := c.joinAggregate(ctx, jr, rep)
	rep.finish(err)
	return rows, err
}
