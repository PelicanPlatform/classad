package wire

import (
	"encoding/binary"
	"math"

	"github.com/PelicanPlatform/classad/ast"
)

// hotPair indexes a hot attribute by id and its entries-relative node offset.
type hotPair struct{ id, off uint32 }

// StreamEncoder builds a wire ad one attribute at a time, so a caller decoding an
// external serialization (e.g. old-ClassAd lines from a socket) can emit wire form
// directly without first materializing an ast.ClassAd. Scalar literals are written
// with no AST at all (Int/Real/String/Bool/Undefined/Error); only genuinely
// computed values need Expr (an ast.Expr subtree).
//
// A StreamEncoder is reusable: Reset() clears it for the next ad, retaining the
// backing buffers.
type StreamEncoder struct {
	t       *InternTable
	hot     map[uint32]struct{}
	entries []byte    // (key, node)* — the attribute-entries region
	hots    []hotPair // present hot attributes
	count   int

	// Inline mode (see flagInlineNames): keys and node names are stored verbatim,
	// t is unused, and hotNames (folded) drives the hot header (by name hash).
	inline      bool
	hotNames    map[string]struct{}
	curName     string // set by begin, used by end (inline hot check)
	curEntryOff int    // set by begin, used by end (inline hot offset points at the entry)
}

// NewStreamEncoder returns an encoder interning names into t. hot (may be nil) is
// the set of interned ids to front-load in the hot header.
func NewStreamEncoder(t *InternTable, hot map[uint32]struct{}) *StreamEncoder {
	return &StreamEncoder{t: t, hot: hot}
}

// NewInlineStreamEncoder returns a StreamEncoder that writes inline names (no
// interning), for the persistent store. hotNames (may be nil) are the case-folded
// names to front-load in the hot header.
func NewInlineStreamEncoder(hotNames map[string]struct{}) *StreamEncoder {
	return &StreamEncoder{inline: true, hotNames: hotNames}
}

// Reset clears the encoder for another ad, keeping its buffers.
func (s *StreamEncoder) Reset() {
	s.entries = s.entries[:0]
	s.hots = s.hots[:0]
	s.count = 0
}

// SetHot replaces the hot-id set (e.g. after the store refreshes popularity).
func (s *StreamEncoder) SetHot(hot map[uint32]struct{}) { s.hot = hot }

// begin writes the attribute key and returns (id, node-start offset). In inline
// mode it stashes the name + entry offset for end (which keys the hot header by
// name); the returned id is unused.
func (s *StreamEncoder) begin(name string) (uint32, int) {
	if s.inline {
		s.curEntryOff = len(s.entries)
		s.putString(name)
		s.curName = name
		return 0, len(s.entries)
	}
	id := s.t.Intern(name)
	s.entries = binary.AppendUvarint(s.entries, uint64(id))
	return id, len(s.entries)
}

// end records hot-header/count bookkeeping after a node has been written.
func (s *StreamEncoder) end(id uint32, nodeStart int) {
	if s.inline {
		if _, ok := s.hotNames[foldASCII(s.curName)]; ok {
			s.hots = append(s.hots, hotPair{nameHash32(s.curName), uint32(s.curEntryOff)})
		}
	} else if _, ok := s.hot[id]; ok {
		s.hots = append(s.hots, hotPair{id, uint32(nodeStart)})
	}
	s.count++
}

// putString writes a uvarint length prefix + bytes into the entries buffer.
func (s *StreamEncoder) putString(str string) {
	s.entries = binary.AppendUvarint(s.entries, uint64(len(str)))
	s.entries = append(s.entries, str...)
}

// Int writes an integer attribute.
func (s *StreamEncoder) Int(name string, v int64) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nInt)
	s.entries = binary.AppendVarint(s.entries, v)
	s.end(id, off)
}

// Real writes a real attribute.
func (s *StreamEncoder) Real(name string, v float64) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nReal)
	s.entries = binary.LittleEndian.AppendUint64(s.entries, math.Float64bits(v))
	s.end(id, off)
}

// String writes a string attribute (v is the unescaped value).
func (s *StreamEncoder) String(name, v string) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nString)
	s.entries = binary.AppendUvarint(s.entries, uint64(len(v)))
	s.entries = append(s.entries, v...)
	s.end(id, off)
}

// StringBytes writes a string attribute whose (already unescaped) value is in v.
// It is String for a caller holding the value in a reused byte buffer, so no
// intermediate string is allocated.
func (s *StreamEncoder) StringBytes(name string, v []byte) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nString)
	s.entries = binary.AppendUvarint(s.entries, uint64(len(v)))
	s.entries = append(s.entries, v...)
	s.end(id, off)
}

// Bool writes a boolean attribute.
func (s *StreamEncoder) Bool(name string, v bool) {
	id, off := s.begin(name)
	if v {
		s.entries = append(s.entries, nBoolTrue)
	} else {
		s.entries = append(s.entries, nBoolFalse)
	}
	s.end(id, off)
}

// Undefined writes an undefined attribute.
func (s *StreamEncoder) Undefined(name string) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nUndefined)
	s.end(id, off)
}

// Error writes an error attribute.
func (s *StreamEncoder) Error(name string) {
	id, off := s.begin(name)
	s.entries = append(s.entries, nError)
	s.end(id, off)
}

// Expr writes a computed (non-literal) attribute from an ast.Expr subtree.
func (s *StreamEncoder) Expr(name string, e ast.Expr) {
	id, off := s.begin(name)
	enc := encoder{buf: s.entries, t: s.t, inline: s.inline}
	enc.node(e)
	s.entries = enc.buf
	s.end(id, off)
}

// ExprWire writes a computed (non-literal) attribute by parsing its expression text
// straight to wire via the native parser -- no ast.Expr. It leaves no partial entry
// if the native parser cannot handle the expression (a record, an int64-min
// magnitude, ...): it rolls the buffer back and returns the error, so the caller can
// fall back to Expr(name, parser.ParseExpr(val)). Interned names added before a
// rollback are harmless (unused ids in a shared table).
func (s *StreamEncoder) ExprWire(name, val string) error {
	start := len(s.entries)
	id, off := s.begin(name)
	buf, err := parseExprToWire(val, s.t, s.inline, s.entries)
	if err != nil {
		s.entries = s.entries[:start]
		return err
	}
	s.entries = buf
	s.end(id, off)
	return nil
}

// Count returns the number of attributes written so far.
func (s *StreamEncoder) Count() int { return s.count }

// Bytes assembles the encoded ad (appending to dst) from the attributes written
// since the last Reset. The result is byte-identical to EncodeWithHot of the same
// attributes and decodes with Decode.
func (s *StreamEncoder) Bytes(dst []byte) []byte {
	var flags byte
	if s.inline {
		flags = flagInlineNames
		// Reorder the entries region hot-first so the hot region is a physical
		// prefix of the record (prefix-decompressable; see EncodeInlineWithHotEnc).
		// Entries were streamed in arrival order, so the hot entries' extents are
		// parsed out and moved to the front here, with their header offsets
		// recomputed. Order within each class is preserved (stable), keeping
		// first-occurrence-wins duplicate semantics intact.
		s.reorderHotFirstInline()
	}
	out := append(dst, magicByte, formatVer, flags)
	out = binary.AppendUvarint(out, uint64(len(s.hots)))
	for _, h := range s.hots {
		out = binary.AppendUvarint(out, uint64(h.id))
		out = binary.AppendUvarint(out, uint64(h.off))
	}
	out = binary.AppendUvarint(out, uint64(s.count))
	out = append(out, s.entries...)
	return out
}

// reorderHotFirstInline rewrites s.entries with the hot entries first and
// updates s.hots' offsets. A no-op when nothing is hot or everything already
// sits at the front. Hot pairs are recorded in write order, so their extents
// are ascending and the cold remainder is the gaps between them, emitted in
// order.
func (s *StreamEncoder) reorderHotFirstInline() {
	if len(s.hots) == 0 {
		return
	}
	// Parse each hot entry's extent from its recorded start.
	type extent struct{ start, end int }
	exts := make([]extent, len(s.hots))
	contiguousPrefix := true
	prevEnd := 0
	for i, h := range s.hots {
		c := &cursor{b: s.entries, pos: int(h.off), ok: true, inline: true}
		c.skip(int(c.uvarint())) // name
		skipNode(c, 0)
		if !c.ok {
			return // malformed entry: leave the region untouched
		}
		exts[i] = extent{int(h.off), c.pos}
		if int(h.off) != prevEnd {
			contiguousPrefix = false
		}
		prevEnd = c.pos
	}
	if contiguousPrefix {
		return // hot entries already occupy the front, in order
	}
	reordered := make([]byte, 0, len(s.entries))
	for i := range exts {
		s.hots[i].off = uint32(len(reordered))
		reordered = append(reordered, s.entries[exts[i].start:exts[i].end]...)
	}
	prev := 0
	for _, ex := range exts {
		reordered = append(reordered, s.entries[prev:ex.start]...)
		prev = ex.end
	}
	reordered = append(reordered, s.entries[prev:]...)
	s.entries = reordered
}
