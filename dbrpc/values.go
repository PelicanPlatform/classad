package dbrpc

import (
	"context"
	"errors"
	"iter"
	"math"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections"
)

// TYPED VALUE BATCHES: a read that ships EVALUATED values instead of ClassAd source text.
//
// Every other read op relays the stored form. The text projections render each record to
// `Owner = "u3"\nClusterId = 9\n` for the consumer to parse straight back, and the wire-row ops
// ship the stored nodes for the consumer to decode and evaluate. Both put the work of turning
// storage into values on the far side of the connection, and for a consumer that wants COLUMNS
// -- an Arrow encoder, a dataframe, a chart -- the relay is pure overhead: rendering a record to
// text and parsing it back is the large majority of what such a read costs, and neither half is
// work anyone asked for.
//
// This op evaluates server-side, where the record's own reference closure and a columnarized
// segment's precomputed values are both to hand, and ships the results column-major. The
// consumer appends them; it never parses.
//
// WHY A BITMAP AND AN EXCEPTIONS LIST, rather than one tagged value per cell. A column of job
// ads is overwhelmingly one kind -- every ClusterId an integer, every Owner a string -- so the
// common case should cost one byte of kind and one bit of presence per cell, with the values
// dense and in order. But ClassAd attributes are not uniform by construction: a cell can be
// UNDEFINED (the attribute is absent, which is a legitimate answer and not a failure), ERROR, a
// different scalar kind, or a list or nested ad that has no column form at all. Collapsing those
// to a null would lose distinctions the text path preserved -- UNDEFINED and ERROR are different
// answers, and a Requirements expression's TEXT is the only useful form of it.
//
// So a column names its kind, marks which rows hold a value OF THAT KIND, packs those densely,
// and carries everything else as an exceptions list keyed by row. A uniform column has an empty
// exceptions list and an all-ones bitmap, which is the shape an Arrow buffer wants. A column with
// no codable kind at all degrades to KindNone, where every cell is an exception -- slower, and
// still exactly right.
//
// WHAT THIS OP DOES NOT DECIDE is the column's declared TYPE. It reports what each batch holds.
// A consumer that must publish one type per column across a whole stream (an Arrow schema) still
// derives that separately and coerces; this op exists so that derivation is handed values rather
// than text.

// ValueKind is the kind a value column packs densely. A cell of any other kind, and a cell that
// is undefined or error, is carried in the column's exceptions list instead.
type ValueKind uint8

const (
	// KindNone is a column with no densely-codable kind: every cell is an exception. It is the
	// honest answer for a column of lists, nested ads or a free mix, not a failure.
	KindNone ValueKind = 0
	// KindInt, KindReal, KindBool and KindString are the scalar kinds a column packs.
	KindInt    ValueKind = 1
	KindReal   ValueKind = 2
	KindBool   ValueKind = 3
	KindString ValueKind = 4
)

func (k ValueKind) String() string {
	switch k {
	case KindInt:
		return "int"
	case KindReal:
		return "real"
	case KindBool:
		return "bool"
	case KindString:
		return "string"
	}
	return "none"
}

// Exception tags. They are a superset of ValueKind on purpose: an exception may be a scalar of a
// kind other than the column's, and the two sets are read in different places, so sharing the
// numbering would couple them for no gain.
const (
	excUndefined uint8 = 0
	excError     uint8 = 1
	excInt       uint8 = 2
	excReal      uint8 = 3
	excBool      uint8 = 4
	excString    uint8 = 5
	// excExpr carries a value with no scalar form -- a list or a nested ad -- as the text the
	// reference unparser produces. The text IS the useful form of such a value, so it is kept
	// rather than reported as a null.
	excExpr uint8 = 6
)

// ValueException is one cell that a column could not pack: its row within the batch, and its
// value. Exceptions are in ascending Row order.
type ValueException struct {
	Row int
	// Value is the cell. For a value with NO scalar form -- a list, a nested ad -- it is the
	// unparser's text of that value held as a string, and NonScalar says so.
	//
	// The flag is not decoration. Without it a list arrives indistinguishable from a string
	// that happens to look like one, and a consumer has no way back to the difference. Text is
	// the right carrier -- it is the only form such a value is useful in, and it is what a
	// schema's text tail column wants -- but it has to be labelled as a rendering rather than
	// as the value itself.
	Value     classad.Value
	NonScalar bool
}

// ValueColumn is one attribute's values across a batch.
//
// Valid is a bitmap over the batch's rows, LSB-first within each byte: a set bit means that row
// holds a value of Kind, and its value is the next one in the dense slice for that kind. A clear
// bit means the row is in Exceptions. Exactly one of Ints/Reals/Bools/Strs is populated,
// according to Kind, and each holds popcount(Valid) entries -- not one per row.
type ValueColumn struct {
	Kind  ValueKind
	Valid []byte
	Ints  []int64
	Reals []float64
	Bools []bool
	Strs  []string

	Exceptions []ValueException
}

// ValueBatch is one column-major batch: Rows rows, one column per requested attribute, in the
// order the attributes were requested.
type ValueBatch struct {
	Rows int
	Cols []ValueColumn
}

// ValueAt reassembles one cell, for a caller that wants a value rather than a column -- a test
// oracle, or a consumer reading a handful of cells. It is deliberately the slow way round: the
// op exists to hand out columns, and a caller using this for every cell has given up the point
// of it.
func (b *ValueBatch) ValueAt(col, row int) classad.Value {
	if col < 0 || col >= len(b.Cols) || row < 0 || row >= b.Rows {
		return classad.NewErrorValue()
	}
	c := &b.Cols[col]
	if !bitSet(c.Valid, row) {
		for i := range c.Exceptions {
			if c.Exceptions[i].Row == row {
				return c.Exceptions[i].Value
			}
		}
		return classad.NewUndefinedValue()
	}
	// The dense index is how many valid rows precede this one.
	dense := 0
	for i := 0; i < row; i++ {
		if bitSet(c.Valid, i) {
			dense++
		}
	}
	switch c.Kind {
	case KindInt:
		return classad.NewIntValue(c.Ints[dense])
	case KindReal:
		return classad.NewRealValue(c.Reals[dense])
	case KindBool:
		return classad.NewBoolValue(c.Bools[dense])
	case KindString:
		return classad.NewStringValue(c.Strs[dense])
	}
	return classad.NewErrorValue()
}

func bitSet(b []byte, i int) bool {
	if i>>3 >= len(b) {
		return false
	}
	return b[i>>3]&(1<<(uint(i)&7)) != 0
}

func setBit(b []byte, i int) { b[i>>3] |= 1 << (uint(i) & 7) }

// ValueBatchRows caps how many rows one batch frame carries. It bounds the server's buffer (it
// holds one batch before writing) and the client's, and it is a row count rather than a byte
// budget because the buffering here is per COLUMN: the server cannot know a batch's encoded size
// until it has decided each column's kind, which it can only do once the batch is complete.
// A string column makes a batch large, so the count is modest.
var ValueBatchRows = 2048

// ErrValuesUnsupported reports that the typed value stream is not available for this request:
// a server too old to know the opcode, or a flag it does not understand. Delivered before any
// batch, so a caller that has already read one can treat it as a hard failure.
//
// The fallback is the text projection, which every server and every table serves.
var ErrValuesUnsupported = errors.New("dbrpc: typed value batches unavailable for this request")

// valueFlagsKnown is the set of request flag bits this server understands. It is currently
// empty: the word is reserved so the first option does not cost an opcode. A request carrying
// an unknown bit is REFUSED rather than served without the option, for the same reason the
// wire-row ops refuse one -- a stream that silently lacks what was asked for is the dangerous
// shape, not the loud failure.
const valueFlagsKnown uint32 = 0

// QueryValuesStream streams matching records as typed, EVALUATED value batches, column-major and
// aligned with attrs. Each batch is handed to yield and is valid only until yield returns: its
// slices alias the decode buffer.
//
// redact requests source-side redaction even on a privileged connection; an unprivileged
// connection is redacted whatever it asks.
//
// Returns ErrValuesUnsupported, before any batch, when the stream is unavailable.
func (c *Client) QueryValuesStream(ctx context.Context, table, constraint string, attrs []string, limit int, redact bool, yield func(*ValueBatch) bool) error {
	if len(attrs) == 0 {
		// A projection of nothing has no columns to return, and "every attribute" is not a
		// shape this op can give: it packs one column per REQUESTED name, and a record's
		// attribute set varies row to row. Refused here rather than at the server, so the
		// message names the caller's mistake.
		return errors.New("dbrpc: QueryValues needs at least one attribute")
	}
	build := func(id uint64) []byte {
		b := putStr(req(id, opQueryValues), table)
		b = putI32(b, int32(limit))
		b = putI32(b, 0) // flags
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
	var batch ValueBatch
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
				if !decodeValueBatch(body, &batch) {
					return errShort
				}
				if !yield(&batch) {
					drain(ch)
					return nil
				}
			case stBadReq:
				return ErrValuesUnsupported
			default:
				return statusErr(status, body)
			}
		}
	}
}

// QueryValues collects QueryValuesStream into whole batches the caller owns, for a caller that
// does not want to think about buffer lifetime. It copies, which is the cost of owning them.
func (c *Client) QueryValues(ctx context.Context, table, constraint string, attrs []string, limit int, redact bool) ([]ValueBatch, error) {
	var out []ValueBatch
	err := c.QueryValuesStream(ctx, table, constraint, attrs, limit, redact, func(b *ValueBatch) bool {
		out = append(out, b.clone())
		return true
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (b *ValueBatch) clone() ValueBatch {
	out := ValueBatch{Rows: b.Rows, Cols: make([]ValueColumn, len(b.Cols))}
	for i, c := range b.Cols {
		out.Cols[i] = ValueColumn{
			Kind:       c.Kind,
			Valid:      append([]byte(nil), c.Valid...),
			Ints:       append([]int64(nil), c.Ints...),
			Reals:      append([]float64(nil), c.Reals...),
			Bools:      append([]bool(nil), c.Bools...),
			Strs:       append([]string(nil), c.Strs...),
			Exceptions: append([]ValueException(nil), c.Exceptions...),
		}
	}
	return out
}

// ---------------------------------------------------------------- server side

func (s *Server) streamQueryValues(ctx context.Context, reqID uint64, r *reader, includePrivate bool, write func([]byte), qlog func(QueryLog)) {
	start := time.Now()
	table := r.str()
	limit := int(r.i32())
	flags := uint32(r.i32())
	redact := r.u8() != 0 || !includePrivate
	constraint := r.str()
	nattrs := int(r.i32())
	attrs := make([]string, 0, nattrs)
	for i := 0; i < nattrs; i++ {
		attrs = append(attrs, r.str())
	}
	n := 0
	var stats *collections.ScanStats
	if s.queryStatsOn() {
		stats = &collections.ScanStats{}
	}
	if qlog != nil {
		defer func() {
			q := QueryLog{Op: "QueryValues", Table: table, Constraint: constraint, Limit: limit,
				Rows: n, Projection: len(attrs), Duration: time.Since(start)}
			if stats != nil {
				q.Scan = *stats
			}
			qlog(q)
		}()
	}
	if r.err != nil || len(attrs) == 0 {
		write(respBad(reqID))
		return
	}
	if flags&^valueFlagsKnown != 0 {
		write(respBad(reqID))
		return
	}
	if refusePrivateConstraint(reqID, constraint, includePrivate, write) {
		return
	}

	ref := s.resolveTable(table)
	if ref.seals() {
		// A continuous aggregate that has sealed holds half its rows in its backing table and
		// half in an archive, and this op reads one source. It declines the way the wire-row
		// ops decline, sending the client to the text projection, where both halves are read.
		write(respBad(reqID))
		return
	}
	var seq iter.Seq[[]classad.Value]
	var err error
	switch ref.kind {
	case kindTable, kindView:
		if redact {
			seq, err = ref.db.QueryProjectStatsRedacted(constraint, attrs, stats)
		} else {
			seq, err = ref.db.QueryProjectStats(constraint, attrs, stats)
		}
	case kindArchive:
		// Served, not refused. An archive is the table whose readers most need this -- history
		// is what gets scanned in bulk -- and the projected scan underneath reads it the same
		// way it reads a mutable table.
		if redact {
			seq, err = ref.arch.QueryProjectStatsRedacted(constraint, attrs, stats)
		} else {
			seq, err = ref.arch.QueryProjectStats(constraint, attrs, stats)
		}
	default:
		write(respErr(reqID, "no such table: "+table))
		return
	}
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}

	enc := newValueEncoder(len(attrs))
	flush := func() {
		if enc.rows == 0 {
			return
		}
		write(enc.encode(respHead(reqID, stStream)))
		enc.reset()
	}
	for vals := range seq {
		if cancelled(ctx) {
			return
		}
		// The scan reuses the slice it yields, so the encoder copies each value in.
		enc.append(vals)
		n++
		if enc.rows >= ValueBatchRows {
			flush()
		}
		if limit > 0 && n >= limit {
			break
		}
	}
	flush()
	write(respHead(reqID, stStreamEnd))
}

// valueEncoder accumulates one batch column by column. Values are held as classad.Value until
// the batch is complete, because a column's kind is a property of the whole batch: deciding it
// from the first row would let one odd row at the front demote a column that is otherwise
// uniform.
type valueEncoder struct {
	rows int
	cols [][]classad.Value
}

func newValueEncoder(ncols int) *valueEncoder {
	e := &valueEncoder{cols: make([][]classad.Value, ncols)}
	return e
}

func (e *valueEncoder) reset() {
	e.rows = 0
	for i := range e.cols {
		e.cols[i] = e.cols[i][:0]
	}
}

func (e *valueEncoder) append(vals []classad.Value) {
	for i := range e.cols {
		if i < len(vals) {
			e.cols[i] = append(e.cols[i], vals[i])
		} else {
			e.cols[i] = append(e.cols[i], classad.NewUndefinedValue())
		}
	}
	e.rows++
}

// chooseKind picks the kind a column packs: the most common scalar kind among its cells, or
// KindNone when no cell has one. Most common rather than first-seen so a single odd row cannot
// push a uniform column onto the exceptions path.
func chooseKind(vals []classad.Value) ValueKind {
	var counts [5]int
	for _, v := range vals {
		switch v.Type() {
		case classad.IntegerValue:
			counts[KindInt]++
		case classad.RealValue:
			counts[KindReal]++
		case classad.BooleanValue:
			counts[KindBool]++
		case classad.StringValue:
			counts[KindString]++
		}
	}
	best, bestN := KindNone, 0
	for k := KindInt; k <= KindString; k++ {
		if counts[k] > bestN {
			best, bestN = k, counts[k]
		}
	}
	return best
}

func (e *valueEncoder) encode(b []byte) []byte {
	b = putI32(b, int32(e.rows))
	b = putI32(b, int32(len(e.cols)))
	validBytes := (e.rows + 7) / 8
	for _, vals := range e.cols {
		kind := chooseKind(vals)
		b = append(b, byte(kind))
		valid := make([]byte, validBytes)
		var ints []int64
		var reals []float64
		var strs []string
		var boolBits []byte
		nbool := 0
		type exc struct {
			row int
			v   classad.Value
		}
		var excs []exc
		for i, v := range vals {
			packed := false
			switch kind {
			case KindInt:
				if v.Type() == classad.IntegerValue {
					n, _ := v.IntValue()
					ints = append(ints, n)
					packed = true
				}
			case KindReal:
				if v.Type() == classad.RealValue {
					f, _ := v.RealValue()
					reals = append(reals, f)
					packed = true
				}
			case KindBool:
				if v.Type() == classad.BooleanValue {
					t, _ := v.BoolValue()
					if nbool%8 == 0 {
						boolBits = append(boolBits, 0)
					}
					if t {
						boolBits[nbool>>3] |= 1 << (uint(nbool) & 7)
					}
					nbool++
					packed = true
				}
			case KindString:
				if v.Type() == classad.StringValue {
					s, _ := v.StringValue()
					strs = append(strs, s)
					packed = true
				}
			}
			if packed {
				setBit(valid, i)
			} else {
				excs = append(excs, exc{row: i, v: v})
			}
		}
		b = putBytes(b, valid)
		// The dense count is written for EVERY kind, KindNone included, so the decoder reads
		// one fixed-shape header per column and never has to know which kinds carry a count.
		switch kind {
		case KindInt:
			b = putI32(b, int32(len(ints)))
			for _, n := range ints {
				b = putU64(b, uint64(n))
			}
		case KindReal:
			b = putI32(b, int32(len(reals)))
			for _, f := range reals {
				b = putU64(b, math.Float64bits(f))
			}
		case KindBool:
			b = putI32(b, int32(nbool))
			b = putBytes(b, boolBits)
		case KindString:
			b = putI32(b, int32(len(strs)))
			for _, s := range strs {
				b = putStr(b, s)
			}
		default:
			b = putI32(b, 0)
		}
		b = putI32(b, int32(len(excs)))
		for _, x := range excs {
			b = putI32(b, int32(x.row))
			b = appendExcValue(b, x.v)
		}
	}
	return b
}

func appendExcValue(b []byte, v classad.Value) []byte {
	switch v.Type() {
	case classad.UndefinedValue:
		return append(b, excUndefined)
	case classad.ErrorValue:
		return append(b, excError)
	case classad.IntegerValue:
		n, _ := v.IntValue()
		return putU64(append(b, excInt), uint64(n))
	case classad.RealValue:
		f, _ := v.RealValue()
		return putU64(append(b, excReal), math.Float64bits(f))
	case classad.BooleanValue:
		t, _ := v.BoolValue()
		x := byte(0)
		if t {
			x = 1
		}
		return append(append(b, excBool), x)
	case classad.StringValue:
		s, _ := v.StringValue()
		return putStr(append(b, excString), s)
	}
	// A list or a nested ad: its text is the only form that keeps it whole.
	s, ok := v.Unparse()
	if !ok {
		s = v.String()
	}
	return putStr(append(b, excExpr), s)
}

// ---------------------------------------------------------------- client decode

func decodeValueBatch(r *reader, out *ValueBatch) bool {
	rows := int(r.i32())
	ncols := int(r.i32())
	if r.err != nil || rows < 0 || ncols < 0 {
		return false
	}
	out.Rows = rows
	if cap(out.Cols) >= ncols {
		out.Cols = out.Cols[:ncols]
	} else {
		out.Cols = make([]ValueColumn, ncols)
	}
	for i := 0; i < ncols; i++ {
		c := &out.Cols[i]
		*c = ValueColumn{
			Kind:       ValueKind(r.u8()),
			Valid:      r.bytesRef(),
			Ints:       c.Ints[:0],
			Reals:      c.Reals[:0],
			Bools:      c.Bools[:0],
			Strs:       c.Strs[:0],
			Exceptions: c.Exceptions[:0],
		}
		m := int(r.i32())
		if r.err != nil || m < 0 {
			return false
		}
		switch c.Kind {
		case KindInt:
			for k := 0; k < m; k++ {
				c.Ints = append(c.Ints, int64(r.u64()))
			}
		case KindReal:
			for k := 0; k < m; k++ {
				c.Reals = append(c.Reals, math.Float64frombits(r.u64()))
			}
		case KindBool:
			bits := r.bytesRef()
			for k := 0; k < m; k++ {
				c.Bools = append(c.Bools, bitSet(bits, k))
			}
		case KindString:
			for k := 0; k < m; k++ {
				c.Strs = append(c.Strs, r.str())
			}
		case KindNone:
			// m is the count field, written as 0; nothing dense follows.
			if m != 0 {
				return false
			}
		default:
			return false
		}
		nexc := int(r.i32())
		if r.err != nil || nexc < 0 {
			return false
		}
		for k := 0; k < nexc; k++ {
			row := int(r.i32())
			v, nonScalar, ok := readExcValue(r)
			if !ok {
				return false
			}
			c.Exceptions = append(c.Exceptions, ValueException{Row: row, Value: v, NonScalar: nonScalar})
		}
		if r.err != nil {
			return false
		}
	}
	return r.err == nil
}

func readExcValue(r *reader) (v classad.Value, nonScalar, ok bool) {
	switch r.u8() {
	case excUndefined:
		return classad.NewUndefinedValue(), false, true
	case excError:
		return classad.NewErrorValue(), false, true
	case excInt:
		return classad.NewIntValue(int64(r.u64())), false, true
	case excReal:
		return classad.NewRealValue(math.Float64frombits(r.u64())), false, true
	case excBool:
		return classad.NewBoolValue(r.u8() != 0), false, true
	case excString:
		return classad.NewStringValue(r.str()), false, true
	case excExpr:
		// Kept as its text, and flagged. A consumer that wants the structure parses it; one
		// that wants to show it -- the common case for Requirements -- already has what it
		// needs, and neither has to guess which it was handed.
		return classad.NewStringValue(r.str()), true, true
	}
	return classad.NewErrorValue(), false, false
}
