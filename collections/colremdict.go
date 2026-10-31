package collections

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// REPEATED NODES IN COLUMNARIZED REMNANTS.
//
// A columnarized record keeps, in its own bytes, every value no column could take: expressions, nested
// records, lists and sealed values (see storableInColumn). Those bytes are compressed one record at a
// time, so a node that every record of a block carries -- a Requirements shared by a cluster's procs, a
// MemoryUsage nearly every job carries -- is paid for in full in every record. Measured on the tail of a
// production AP's history (12.4k ads, ~180 attributes each), those repeated nodes were 469 of the 494
// expression bytes in an average record. The dictionary below cut the columnarized segments' bytes by
// 13-30% (8-16% on epoch history; the low end is a store whose records were compressed under a trained
// dictionary); Requirements alone was 85% of the repeated expression bytes.
//
// So each base block may carry a REMNANT DICTIONARY: the distinct nodes that repeat across the block's
// remnants, stored once, compressed with the block's other regions. A record holding one of them holds a
// short reference in its place -- TagExternalRef and the entry's index as a uvarint -- AT THE SAME
// POSITION, so the attribute keeps its key and its place in the record, and expanding every reference
// gives back the record's original bytes exactly. Nothing about the record's order, key or neighbours
// changes, which is what lets every reader be handed the expanded bytes and stay unchanged.
//
// WHICH ID SPACE. An entry is the node's bytes exactly as the record stored them, copied out of the
// record and copied back in -- never decoded, re-encoded or translated. Any attribute reference inside it
// is therefore in the record's own key space: a name in an inline-keyed record, the segment dictionary's
// id in an interned one, the process's intern id in an in-memory collection (whose records and payload
// live and die with the process). The entry is only ever expanded back into records of the same block,
// so it is read in exactly the space it was written in. No process-local id is involved anywhere: the
// dedupe key is the stored bytes, so two nodes that differ only in how an attribute is encoded are
// different entries, and entries are numbered in first-occurrence order over the block's records as
// stored.
//
// WHAT IS NEVER AN ENTRY. A literal never reaches a remnant (the columns take it). A SEALED node never
// becomes an entry: its ciphertext differs per record anyway, and nothing derived from a sealed value
// may be stored where another record could reach it.
//
// READERS. Exactly three places decompress a columnarized remnant -- recordWireIn (which every full-ad
// read, splice, merge, compaction, reseal, index and zone build goes through), projectFromColumns and the
// held-cell pass -- and each expands it before looking inside (expandRemnant). A reference that does not
// resolve fails the read: it is never handed on, and wire's readers reject the tag as malformed rather
// than read it as a value.
//
// FORMAT. A payload whose blocks carry a dictionary is written as payload version 2, which an older
// build refuses at Open (ErrUnsupportedColumnarVersion) rather than reading references it does not know
// as values. A segment with no dictionary is still written as version 1, readable by older builds.

// remnantDictDefault is whether columnarization writes remnant dictionaries when Options.RemnantDictionary
// is 0. On: a build that predates them refuses a collection holding one rather than misread it, and a
// caller that must stay readable by such a build sets the option negative.
const remnantDictDefault = true

// writeRemnantDict reports whether columnarization should build remnant dictionaries.
func (c *Collection) writeRemnantDict() bool {
	switch {
	case c.remDict > 0:
		return true
	case c.remDict < 0:
		return false
	}
	return remnantDictDefault
}

// colRemDictSection marks the remnant-dictionary section of a version 2 payload.
const colRemDictSection = 0x54434452 // "RDCT" on disk (little-endian)

// errRemnantRef is a columnarized record holding a dictionary reference its block cannot resolve.
var errRemnantRef = errors.New("collections: columnarized record holds a remnant dictionary reference that does not resolve")

// remnantRefFailures counts record reads refused with errRemnantRef.
var remnantRefFailures atomic.Int64

// RemnantRefFailures reports how many record reads, since the process started, found a columnarized
// record whose remnant dictionary reference did not resolve. Each such read fails rather than return an
// ad holding the reference. Nonzero means a payload's dictionary does not match its records.
func RemnantRefFailures() int64 { return remnantRefFailures.Load() }

// remnantDictEntries counts dictionary entries written, so a test can see the dictionary was built.
var remnantDictEntries atomic.Int64

// remnantEntries walks the n attribute entries of a remnant's body, calling fn with each key's bytes and
// node's bytes, and reports whether they parse to exactly the body's end. A node holding a dictionary
// reference is TagExternalRef and a uvarint; any other node is measured in the record's key encoding.
func remnantEntries(ents []byte, n int, inline bool, fn func(key, node []byte) bool) bool {
	p := 0
	for range n {
		ks := p
		l, w := binary.Uvarint(ents[p:])
		if w <= 0 {
			return false
		}
		p += w
		if inline {
			if l > uint64(len(ents)-p) {
				return false
			}
			p += int(l)
		}
		if p >= len(ents) {
			return false
		}
		ns := p
		if ents[p] == wire.TagExternalRef {
			_, w := binary.Uvarint(ents[p+1:])
			if w <= 0 {
				return false
			}
			p += 1 + w
		} else {
			nl, ok := wire.NodeLenKeyed(ents[p:], inline)
			if !ok {
				return false
			}
			p += nl
		}
		if !fn(ents[ks:ns], ents[ns:p]) {
			return false
		}
	}
	return p == len(ents)
}

// remnantDictCandidate reports whether a remnant node may become a dictionary entry: a value no column
// took (not a literal), not sealed, and not already a reference.
func remnantDictCandidate(node []byte) bool {
	if len(node) == 0 || node[0] == wire.TagExternalRef || wire.IsEncryptedNode(node) {
		return false
	}
	return !storableInColumn(node)
}

// uvarintLen is the encoded length of v as a uvarint.
func uvarintLen(v uint64) int {
	n := 1
	for v >= 0x80 {
		v >>= 7
		n++
	}
	return n
}

// buildRemnantDict builds one block's dictionary from its records' remnant bodies, rewriting in place
// every body that holds an entry. It returns the entries, in first-occurrence order, or none when no node
// repeats. A body that does not parse, or whose rewrite does not expand back to exactly its original
// bytes, is left as it was: the dictionary is a saving, never a dependency.
//
// The bodies are the remnants exactly as they will be stored (see the id-space note above): each entry
// is the stored node's bytes, keyed by those bytes and nothing else.
func buildRemnantDict(bodies [][]byte) [][]byte {
	type parsed struct {
		hdr    []byte
		n      int
		inline bool
		ok     bool
	}
	ps := make([]parsed, len(bodies))
	count := map[string]int{}
	for i, b := range bodies {
		hdr, ents, n, inline, ok := wire.Ad(b).SplitBody()
		if !ok {
			continue
		}
		ok = remnantEntries(ents, n, inline, func(_, node []byte) bool {
			if node[0] == wire.TagExternalRef {
				return false // a body that already holds a reference is not one this build wrote
			}
			if remnantDictCandidate(node) {
				count[string(node)]++
			}
			return true
		})
		ps[i] = parsed{hdr: hdr, n: n, inline: inline, ok: ok}
	}
	idx := map[string]int{}
	var entries [][]byte
	for i, b := range bodies {
		if !ps[i].ok {
			continue
		}
		_, ents, _, _, _ := wire.Ad(b).SplitBody()
		remnantEntries(ents, ps[i].n, ps[i].inline, func(_, node []byte) bool {
			s := string(node)
			if _, done := idx[s]; done || count[s] < 2 {
				return true
			}
			// Only where the reference is shorter than the node it replaces.
			if len(node) <= 1+uvarintLen(uint64(len(entries))) {
				return true
			}
			idx[s] = len(entries)
			entries = append(entries, append([]byte(nil), node...))
			return true
		})
	}
	if len(entries) == 0 {
		return nil
	}
	region := remnantDictRegion(entries)
	var out, exp []byte
	for i, b := range bodies {
		if !ps[i].ok {
			continue
		}
		_, ents, _, _, _ := wire.Ad(b).SplitBody()
		out = out[:0]
		refs := 0
		remnantEntries(ents, ps[i].n, ps[i].inline, func(key, node []byte) bool {
			out = append(out, key...)
			if j, ok := idx[string(node)]; ok {
				out = append(out, wire.TagExternalRef)
				out = binary.AppendUvarint(out, uint64(j))
				refs++
				return true
			}
			out = append(out, node...)
			return true
		})
		if refs == 0 {
			continue
		}
		rw := wire.BuildAd(nil, ps[i].hdr, ps[i].n, out)
		// Proved, not assumed: the rewrite must expand to the original bytes, or the record keeps them.
		var err error
		var expanded bool
		exp, expanded, err = expandRemnantWith(rw, region, len(entries), exp[:0])
		if err != nil || !expanded || !bytes.Equal(exp, b) {
			continue
		}
		bodies[i] = rw
	}
	remnantDictEntries.Add(int64(len(entries)))
	return entries
}

// remnantDictRegion lays out a block's dictionary entries as its uncompressed region: (n+1) little-endian
// u32 offsets, then the entries back to back.
func remnantDictRegion(entries [][]byte) []byte {
	n := len(entries)
	hdr := 4 * (n + 1)
	total := 0
	for _, e := range entries {
		total += len(e)
	}
	out := make([]byte, hdr, hdr+total)
	off := 0
	for i, e := range entries {
		binary.LittleEndian.PutUint32(out[4*i:], uint32(off))
		off += len(e)
	}
	binary.LittleEndian.PutUint32(out[4*n:], uint32(off))
	for _, e := range entries {
		out = append(out, e...)
	}
	return out
}

// remnantDictEntry returns entry j of a decompressed dictionary region holding n entries. Bounds are
// checked rather than trusted, and an entry that is empty or is itself a reference is refused.
func remnantDictEntry(region []byte, n, j int) ([]byte, bool) {
	hdr := 4 * (n + 1)
	if j < 0 || j >= n || hdr > len(region) {
		return nil, false
	}
	o1 := int(binary.LittleEndian.Uint32(region[4*j:]))
	o2 := int(binary.LittleEndian.Uint32(region[4*j+4:]))
	if o1 >= o2 || hdr+o2 > len(region) {
		return nil, false
	}
	e := region[hdr+o1 : hdr+o2]
	if e[0] == wire.TagExternalRef {
		return nil, false
	}
	return e, true
}

// expandRemnantWith expands every dictionary reference in remnant raw against a decompressed region of n
// entries, appending the result to dst. expanded is false when raw holds no reference, in which case raw
// itself is the answer and dst is untouched. A reference that does not resolve, or a body that does not
// parse, is errRemnantRef.
func expandRemnantWith(raw, region []byte, n int, dst []byte) ([]byte, bool, error) {
	hdr, ents, cnt, inline, ok := wire.Ad(raw).SplitBody()
	if !ok {
		return nil, false, errRemnantRef
	}
	// One walk to find out whether there is anything to do: most records of a block with a dictionary
	// hold a reference, but copying a record that holds none would be pure waste.
	refs := 0
	if !remnantEntries(ents, cnt, inline, func(_, node []byte) bool {
		if node[0] == wire.TagExternalRef {
			refs++
		}
		return true
	}) {
		return nil, false, errRemnantRef
	}
	if refs == 0 {
		return raw, false, nil
	}
	dst = append(dst, hdr...)
	dst = binary.AppendUvarint(dst, 0) // hot header: empty, as every remnant's is (see wire.BuildAd)
	dst = binary.AppendUvarint(dst, uint64(cnt))
	bad := false
	remnantEntries(ents, cnt, inline, func(key, node []byte) bool {
		dst = append(dst, key...)
		if node[0] != wire.TagExternalRef {
			dst = append(dst, node...)
			return true
		}
		j, _ := binary.Uvarint(node[1:])
		if j >= uint64(n) {
			bad = true
			return false
		}
		e, ok := remnantDictEntry(region, n, int(j))
		if !ok {
			bad = true
			return false
		}
		dst = append(dst, e...)
		return true
	})
	if bad {
		return nil, false, errRemnantRef
	}
	return dst, true, nil
}

// expandRemnant expands record raw's dictionary references against block b's dictionary, read through
// bc. With no dictionary on the block the record holds no reference and is returned as it is (expanded
// false); otherwise see expandRemnantWith. A failure is counted.
func (b *columnarBlock) expandRemnant(raw []byte, bc *blockCache, dst []byte) ([]byte, bool, error) {
	if b == nil || b.remDictN == 0 {
		return raw, false, nil
	}
	region, err := bc.stream(b, kindRemDict)
	if err != nil {
		remnantRefFailures.Add(1)
		return nil, false, errRemnantRef
	}
	out, expanded, err := expandRemnantWith(raw, region, b.remDictN, dst)
	if err != nil {
		remnantRefFailures.Add(1)
	}
	return out, expanded, err
}

// hasRemnantDict reports whether any base block of the segment carries a remnant dictionary.
func (cs *colSegment) hasRemnantDict() bool {
	for _, b := range cs.blocks {
		if b.remDictN > 0 {
			return true
		}
	}
	return false
}

// attachRemnantDicts builds each base block's dictionary over its records' remnant bodies (indexed by
// segment record position, in block order), rewriting the bodies in place, and attaches the compressed
// region to the block.
func attachRemnantDicts(blocks []*columnarBlock, bodies [][]byte, codec Codec) {
	base := 0
	for _, b := range blocks {
		if base+b.n > len(bodies) {
			return
		}
		if entries := buildRemnantDict(bodies[base : base+b.n]); len(entries) > 0 {
			b.remDictN = len(entries)
			b.remDictComp = codec.Compress(nil, remnantDictRegion(entries))
		}
		base += b.n
	}
}

// appendRemDictSection writes a version 2 payload's dictionary section: the magic, the base block count,
// then per block its compressed region and entry count.
func appendRemDictSection(dst []byte, blocks []*columnarBlock) []byte {
	dst = appendU32(dst, colRemDictSection)
	dst = appendU32(dst, uint32(len(blocks)))
	for _, b := range blocks {
		dst = appendBytes(dst, b.remDictComp)
		dst = appendU32(dst, uint32(b.remDictN))
	}
	return dst
}

// readRemDictSection reads a dictionary section from c onto cs's base blocks. Unlike the trailers, the
// section is not optional in a payload that declares it: its records hold references only it resolves,
// so a section that does not parse makes the whole payload malformed.
func readRemDictSection(c *cursor, cs *colSegment) bool {
	if c.u32() != colRemDictSection {
		return false
	}
	nb := int(c.u32())
	if c.err != nil || nb != len(cs.blocks) {
		return false
	}
	for _, b := range cs.blocks {
		comp := c.bytes()
		n := int(c.u32())
		if c.err != nil || n < 0 || n > 1<<24 || (n > 0) != (len(comp) > 0) {
			return false
		}
		b.remDictComp, b.remDictN = comp, n
	}
	return c.err == nil
}
