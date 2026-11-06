package collections

import (
	"bytes"
	"encoding/binary"
	"math"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// MOVING A LIST OUT OF ITS RECORD.
//
// A list column (collist.go) starts as a copy: the record keeps its list and the column holds the same
// values typed. Measured on pool slot ads, the lists are ~6.6% of the records' stored bytes and the copy
// costs ~1.9%, so storing each list once -- in the column only -- is the larger saving. This file does
// that, the way the remnant dictionary (colremdict.go) already takes repeated nodes out of records.
//
// A MOVED CELL LEAVES A PLACEHOLDER. Where the record held the list node it now holds wire.TagListRef and
// the field's index among its block's list fields as a uvarint, under the SAME key at the SAME position.
// The key keeps its spelling (an inline record's name, case included, or an interned record's segment
// dictionary id) and the attribute keeps its place, so putting the node back gives the record's original
// bytes exactly; nothing about the record's order, key or neighbours changes, and every reader handed the
// expanded bytes stays unchanged. The record's row in its block names the cell, so the placeholder needs
// nothing else.
//
// BYTE IDENTITY IS PROVED PER CELL, NOT ASSUMED. The encoder is canonical but the format is not (a reader
// accepts a non-minimal varint), and the column keeps values, not bytes. So at build every candidate cell
// is re-encoded from the compressed column exactly as a reader would decode it (renderListNodes over the
// decompressed body), and moved only when that equals the record's stored node byte for byte; then the
// whole rewritten record is expanded through the reader's own expansion and compared with the original.
// A cell that does not reproduce stays in its record, as a copy, which is what every cell is with moving
// off.
//
// WHAT IS NEVER MOVED. Anything the column does not hold: an attribute the sealing policy names, a sealed
// value, a list of another kind, a nested list, an expression element -- the column never carries those
// (see WHEN A CELL QUALIFIES in collist.go), so there is nothing to move them into.
//
// READERS. The placeholders sit exactly where remnant dictionary references sit, and the three places
// that decompress a columnarized remnant -- recordWireIn, projectFromColumns and the held-cell pass --
// expand both before looking inside (expandRemnant). A placeholder that does not resolve fails the read;
// wire's readers reject the tag as malformed, so a remnant that escaped expansion fails to decode rather
// than reading as a record without the attribute.
//
// FORMAT. A segment with any moved cell writes payload version 3, which carries the list columns in the
// payload BODY -- CRC-covered, required, a section that does not parse makes the payload malformed and
// the segment damaged -- because they are then the only copy of those lists. An older build refuses a
// version 3 payload at Open (ErrUnsupportedColumnarVersion) rather than read records holding
// placeholders. A segment with no moved cell is written exactly as before (version 1 or 2, any list
// columns a copy in the extension trailer), readable by older builds.
//
// GOING BACK. With Options.MoveListColumns negative, a segment holding moved cells is stale
// (payloadStale), and the routine schema review re-columnarizes it from its reassembled records with
// nothing moved, as version 1 or 2. That is the rollback path: run this build with the option negative
// until no segment holds a moved cell (FsckSegment.ColumnarVersion < 3 everywhere), then roll back.

// listMovesDefault is whether columnarization moves list cells out of records when Options.MoveListColumns
// is 0. On, as the remnant dictionary is: a build that predates moved lists refuses a collection holding
// one rather than misread it, and a caller that must stay readable by such a build sets the option
// negative (which also moves existing cells back at the next schema review).
const listMovesDefault = true

// writeListMoves reports whether columnarization should move list cells out of their records.
func (c *Collection) writeListMoves() bool {
	switch {
	case c.listMoves > 0:
		return true
	case c.listMoves < 0:
		return false
	}
	return listMovesDefault
}

// listCellsMoved, listCellsKept and listMoveProofFailures count, since the process started, list cells
// moved out of their records, cells the column holds whose re-encoding did not reproduce the record's
// bytes (kept in the record), and records whose rewrite did not expand back to their original bytes
// (left whole).
var (
	listCellsMoved        atomic.Int64
	listCellsKept         atomic.Int64
	listMoveProofFailures atomic.Int64
)

// ListCellsMoved reports how many list cells columnarization has moved out of their records, and how many
// it kept in their records because the column's re-encoding of the values was not the stored bytes, since
// the process started.
func ListCellsMoved() (moved, keptNonCanonical int64) {
	return listCellsMoved.Load(), listCellsKept.Load()
}

// hasMovedLists reports whether any base block of the segment holds a list cell no record holds.
func (cs *colSegment) hasMovedLists() bool {
	for _, b := range cs.blocks {
		if b.listMoved > 0 {
			return true
		}
	}
	return false
}

// renderListNodes re-encodes every present cell of every list field of b, from the decompressed list body
// raw, as the list node it stands for. The result is laid out for lookup by (field, row):
//
//	field count u32 | row count u32 | per field: start u32 | per field, at its start: (n+1) u32 offsets,
//	then that field's nodes
//
// where a field's offsets are relative to the end of its offset table and an absent or OTHER cell is
// empty. ok=false when a field does not parse.
func renderListNodes(b *columnarBlock, raw []byte) ([]byte, bool) {
	nf := len(b.listIDs)
	n := b.n
	hdr := 8 + 4*nf
	out := make([]byte, hdr, hdr+2*len(raw))
	binary.LittleEndian.PutUint32(out, uint32(nf))
	binary.LittleEndian.PutUint32(out[4:], uint32(n))
	var buf []string
	for j := 0; j < nf; j++ {
		lc, _, ok := b.listColumn(raw, j, &buf)
		if !ok {
			return nil, false
		}
		binary.LittleEndian.PutUint32(out[8+4*j:], uint32(len(out)))
		tab := len(out)
		out = append(out, make([]byte, 4*(n+1))...)
		base := len(out)
		for k := 0; k < n; k++ {
			binary.LittleEndian.PutUint32(out[tab+4*k:], uint32(len(out)-base))
			if !lc.Has(k) {
				continue
			}
			lo := int(binary.LittleEndian.Uint32(lc.Offs[4*k:]))
			hi := int(binary.LittleEndian.Uint32(lc.Offs[4*k+4:]))
			out = wire.AppendListHeader(out, hi-lo)
			for i := lo; i < hi; i++ {
				if lc.Valid != nil && !testBit(lc.Valid, i) {
					out = wire.AppendUndefinedNode(out)
					continue
				}
				switch lc.Kind {
				case vm.VsInt:
					out = wire.AppendIntNode(out, int64(binary.LittleEndian.Uint64(lc.Fixed[8*i:])))
				case vm.VsReal:
					out = wire.AppendRealNode(out, math.Float64frombits(binary.LittleEndian.Uint64(lc.Fixed[8*i:])))
				case vm.VsBool:
					out = wire.AppendBoolNode(out, testBit(lc.Bits, i))
				case vm.VsString:
					out = wire.AppendStringNode(out, lc.Dict[listCodeAt(lc.Codes, lc.CodeWidth, i)])
				default:
					return nil, false
				}
			}
		}
		binary.LittleEndian.PutUint32(out[tab+4*n:], uint32(len(out)-base))
	}
	return out, true
}

// listNodeAt returns field j's cell for row k from a renderListNodes region, checking bounds rather than
// trusting them. An absent or OTHER cell (empty) does not resolve: no placeholder stands for one.
func listNodeAt(nodes []byte, j uint64, k int) ([]byte, bool) {
	if len(nodes) < 8 {
		return nil, false
	}
	nf := uint64(binary.LittleEndian.Uint32(nodes))
	n := int(binary.LittleEndian.Uint32(nodes[4:]))
	if j >= nf || uint64(len(nodes)) < 8+4*nf || k < 0 || k >= n {
		return nil, false
	}
	tab := int(binary.LittleEndian.Uint32(nodes[8+4*j:]))
	base := tab + 4*(n+1)
	if tab < 0 || base > len(nodes) {
		return nil, false
	}
	lo := base + int(binary.LittleEndian.Uint32(nodes[tab+4*k:]))
	hi := base + int(binary.LittleEndian.Uint32(nodes[tab+4*k+4:]))
	if lo >= hi || hi > len(nodes) {
		return nil, false
	}
	return nodes[lo:hi], true
}

// listNodes returns b's list cells rendered as nodes (renderListNodes), from the block cache when present.
// Derived from the list stream once per block rather than per record read: a record read then copies its
// list's bytes and decodes nothing.
func (bc *blockCache) listNodes(b *columnarBlock) ([]byte, error) {
	key := streamKey(b.id, kindListNode)
	if bc != nil {
		if r, ok := bc.c.Get(key); ok {
			return r, nil
		}
	}
	raw, err := bc.stream(b, kindList)
	if err != nil {
		return nil, err
	}
	nodes, ok := renderListNodes(b, raw)
	if !ok {
		return nil, errRemnantRef
	}
	if bc != nil {
		bc.c.Set(key, nodes, int64(len(nodes)))
	}
	return nodes, nil
}

// remnantKey reads a remnant entry's key bytes: an interned record's id, or an inline record's name.
func remnantKey(key []byte, inline bool) (uint32, string, bool) {
	v, w := binary.Uvarint(key)
	if w <= 0 {
		return 0, "", false
	}
	if !inline {
		return uint32(v), "", v <= math.MaxUint32
	}
	if uint64(len(key)-w) != v {
		return 0, "", false
	}
	return 0, string(key[w:]), true
}

// listMoveTestHook, when set, is called with each rewritten remnant body before its proof, and returns the
// body the proof sees. A test seam for the proof: production leaves it nil.
var listMoveTestHook func(body []byte) []byte

// moveBlockLists moves, record by record, every list cell the blocks' list columns hold whose re-encoding
// reproduces the record's stored node, replacing the node with a placeholder (see MOVING A LIST OUT OF
// ITS RECORD). bodies are the remnants exactly as they will be stored, indexed by segment record position
// in block order, and are rewritten in place; d is the segment's dictionary (nil when its records are not
// interned); codec decompresses the blocks' list streams. Each block's listMoved is set to what it moved.
func (c *Collection) moveBlockLists(blocks []*columnarBlock, bodies [][]byte, d *segDictHandle, codec Codec) {
	base := 0
	var out, exp []byte
	for _, b := range blocks {
		lo := base
		base += b.n
		if len(b.listIDs) == 0 || base > len(bodies) {
			continue
		}
		raw, err := codec.Decompress(nil, b.listComp)
		if err != nil {
			continue
		}
		nodes, ok := renderListNodes(b, raw)
		if !ok {
			continue
		}
		fieldOf := make(map[uint32]uint64, len(b.listIDs))
		for j, id := range b.listIDs {
			fieldOf[id] = uint64(j)
		}
		moved := 0
		for k := 0; k < b.n; k++ {
			body := bodies[lo+k]
			hdr, ents, cnt, inline, ok := wire.Ad(body).SplitBody()
			if !ok {
				continue
			}
			out = out[:0]
			n, kept := 0, 0
			ok = remnantEntries(ents, cnt, inline, func(key, node []byte) bool {
				out = append(out, key...)
				if node[0] == wire.TagListRef || node[0] == wire.TagExternalRef {
					return false // not a body this build wrote: leave the record as it is
				}
				j, field := uint64(0), false
				if _, isList := wire.ScalarListElems(node, nil); isList {
					if id, name, kok := remnantKey(key, inline); kok {
						if gid, gok := c.globalIDOf(d, id, name); gok {
							if jj, has := fieldOf[gid]; has {
								j, field = jj, true
							}
						}
					}
				}
				if field {
					if cell, cok := listNodeAt(nodes, j, k); cok {
						if bytes.Equal(cell, node) {
							out = append(out, wire.TagListRef)
							out = binary.AppendUvarint(out, j)
							n++
							return true
						}
						kept++
					}
				}
				out = append(out, node...)
				return true
			})
			if !ok || n == 0 {
				listCellsKept.Add(int64(kept))
				continue
			}
			rw := wire.BuildAd(nil, hdr, cnt, out)
			proof := rw
			if listMoveTestHook != nil {
				proof = listMoveTestHook(bytes.Clone(rw))
			}
			// Proved, not assumed: the rewrite must expand, through the reader's own expansion, to the
			// record's original bytes, or the record keeps them.
			kk := k
			exp, _, err = expandRemnantWith(proof, nil, 0, func(j uint64) ([]byte, bool) { return listNodeAt(nodes, j, kk) }, exp[:0])
			if err != nil || !bytes.Equal(exp, body) {
				listMoveProofFailures.Add(1)
				listCellsKept.Add(int64(kept))
				continue
			}
			bodies[lo+k] = rw
			moved += n
			listCellsMoved.Add(int64(n))
			listCellsKept.Add(int64(kept))
		}
		b.listMoved = moved
	}
}

// PAYLOAD VERSION 3: THE LIST SECTION.
//
// A version 3 payload is version 2's body (with its remnant dictionary section, possibly all empty)
// followed by the list section, then the trailers as before. The section is
//
//	magic u32 | name count u32 | names (u32 length, bytes) |
//	block count u32 | per block: field count u32 | field count name indices u32 | moved u32 | body bytes
//
// with each block's body as in the extension trailer's list section (encodeListField, compressed). It is
// required: the records of a block with a moved cell hold placeholders only it resolves.
const colListBodySection = 0x544c4f43 // "COLT" on disk (little-endian)

func appendListBodySection(dst []byte, blocks []*columnarBlock) []byte {
	index := map[string]int{}
	var names []string
	for _, b := range blocks {
		for _, nm := range b.listNames {
			if _, ok := index[strings.ToLower(nm)]; !ok {
				index[strings.ToLower(nm)] = len(names)
				names = append(names, nm)
			}
		}
	}
	dst = appendU32(dst, colListBodySection)
	dst = appendU32(dst, uint32(len(names)))
	for _, nm := range names {
		dst = appendBytes(dst, []byte(nm))
	}
	dst = appendU32(dst, uint32(len(blocks)))
	for _, b := range blocks {
		dst = appendU32(dst, uint32(len(b.listNames)))
		for _, nm := range b.listNames {
			dst = appendU32(dst, uint32(index[strings.ToLower(nm)]))
		}
		dst = appendU32(dst, uint32(b.listMoved))
		dst = appendBytes(dst, b.listComp)
	}
	return dst
}

// readListBodySection reads a version 3 payload's list section from c onto cs's base blocks. Any part
// that does not parse fails the whole payload: unlike the trailer's copy, these lists are in no record.
func readListBodySection(c *cursor, cs *colSegment, internName func(string) uint32) bool {
	if c.u32() != colListBodySection {
		return false
	}
	nn := int(c.u32())
	if c.err != nil || nn < 0 || nn > 1<<20 {
		return false
	}
	names := make([]string, nn)
	for i := range names {
		names[i] = string(c.bytes())
	}
	nb := int(c.u32())
	if c.err != nil || nb != len(cs.blocks) {
		return false
	}
	type att struct {
		names []string
		moved int
		comp  []byte
	}
	atts := make([]att, nb)
	for i := range atts {
		nf := int(c.u32())
		if c.err != nil || nf < 0 || nf > nn {
			return false
		}
		for range nf {
			ni := int(c.u32())
			if c.err != nil || ni < 0 || ni >= nn {
				return false
			}
			atts[i].names = append(atts[i].names, names[ni])
		}
		mv := c.u32()
		atts[i].comp = c.bytes()
		if c.err != nil || (nf > 0) != (len(atts[i].comp) > 0) || (mv > 0 && nf == 0) ||
			int64(mv) > int64(nf)*int64(cs.blocks[i].n) {
			return false
		}
		atts[i].moved = int(mv)
	}
	if c.err != nil {
		return false
	}
	for i, b := range cs.blocks {
		if len(atts[i].names) == 0 {
			continue
		}
		ids := make([]uint32, len(atts[i].names))
		for j, nm := range atts[i].names {
			ids[j] = internName(nm)
		}
		b.listIDs, b.listNames, b.listComp, b.listMoved = ids, atts[i].names, atts[i].comp, atts[i].moved
	}
	cs.listsInBody = true
	return true
}
