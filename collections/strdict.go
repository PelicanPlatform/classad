package collections

import (
	"encoding/binary"
	"sort"
	"unsafe"

	"github.com/PelicanPlatform/classad/classad"
)

// sortStrDict converts the encoder's per-field string-dictionary map into the sorted-by-idx slice a
// block holds, which strDictOf binary-searches -- the slice replaced a per-block map whose bucket
// overhead was a large share of columnar metadata heap.
func sortStrDict(m map[int]strDictField) []strDictEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]strDictEntry, 0, len(m))
	for idx, f := range m {
		out = append(out, strDictEntry{idx: idx, strDictField: f})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].idx < out[j].idx })
	return out
}

// A per-block, per-field STRING DICTIONARY, so a string predicate compares integers.
//
// The string region is POSITIONAL -- uvarint(len)+bytes for each non-escaped string field in schema order
// -- so reaching field j means walking the string fields before it, once per record. That walk measured 55%
// of a string scan, and it is irreducible in that layout: there is no offset to jump to.
//
// A dictionary replaces it with two things a scan can use directly. Distinct values are stored once, and
// each record holds a fixed-width CODE into them, columnar like every other column here. Then:
//
//   - `Owner == "user3"` becomes: find the codes whose entry folds equal to "user3" (one linear pass over
//     the dictionary, per block, not per record), then compare a code column against them -- integers.
//   - If NO entry matches, no record in the block can, so the whole block is skipped without reading a
//     single code. A selective string equality prunes like a zone map.
//   - Reading a string is dict[code], O(1), instead of a walk.
//
// ORDERING WORKS TOO, because the dictionary is sorted with the evaluator's own fold comparison. Codes are
// then monotone in fold order, so `s < lit` is `code < boundary` for a boundary found by one pass over the
// dictionary. That is why sortDictFold uses classad.CompareStringsFold rather than plain byte order:
// getting this wrong would make ordering comparisons silently disagree with the reference.
//
// FOLD-EQUAL DISTINCT VALUES are the subtlety. "user3" and "USER3" are equal under == and NOT identical
// under =?=, so they must keep separate codes while both matching an == against either. Hence a code SET
// for equality rather than a single code, and exact-byte matching for =?=.
//
// AUTHORITATIVE. A dictionary-encoded field is NOT written to the positional region: the dictionary is where
// its values live. That is what turns the encoding from a cost into a saving -- storing a field's values
// twice was ~42% of the string region and 0.30% of an OSPool ad.
//
// The price is that the positional region is no longer self-describing. Anything walking it must skip the
// fields the dictionary owns, or every offset after the first omission is wrong, and reconstruct has to
// splice dictionary values back into positional order to rebuild the canonical record form. Those are the
// three readers that changed with it: reconstruct, colScope.slotString and blockVecSource.loadStr.

const (
	// dictMaxCardinality caps a dictionary at what a 2-byte code addresses.
	dictMaxCardinality = 1 << 16

	// dictMinRepeat is the average repeats per distinct value below which a field is left positional.
	// Equality would still be faster with a dictionary at cardinality 1.0 -- comparing a code beats
	// walking to a string -- but with the positional region still being written, a dictionary that
	// deduplicates nothing stores every value twice for it. Two repeats is the point where the dictionary
	// is no bigger than what it accelerates.
	dictMinRepeat = 2
)

// strDictEnabled gates the encoding. It exists so a benchmark can measure the dictionary against the
// positional walk over IDENTICAL data -- the two paths differ by which encoding a block chose, and without
// a switch the only way to compare them is to compare different fixtures, which compares cardinality
// rather than code. Never set false outside a test.
var strDictEnabled = true

// strDictField locates one field's dictionary and code column within the block's two regions.
type strDictField struct {
	codeStart int // byte offset of this field's code column in the decompressed code region
	codeWidth int // 1 or 2
	dictStart int // byte offset of this field's entries in the decompressed dictionary region
	count     int // number of entries

	// noEscape is true when EVERY record in the block has a stored value for this field, so the dictionary
	// is the complete set of values present. Only then can a block be pruned for a literal the dictionary
	// lacks: an escaped record's value lives in the cold tail, where the dictionary cannot see it, and
	// pruning on an incomplete dictionary would drop real matches.
	//
	// The numeric zones carry the same flag for the same reason, but escapeFree reads zones and zones are
	// built for numeric fields only, so a string field needs its own.
	noEscape bool
}

// buildStrDicts decides which string fields of this block to dictionary-encode and builds them.
//
// Returns the per-field locations plus the two raw regions, uncompressed; the caller compresses them with
// the region codec like every other stream.
func buildStrDicts(s *adSchema, recs [][]byte, n int) (map[int]strDictField, []byte, []byte) {
	if !strDictEnabled {
		return nil, nil, nil
	}
	var fields []int
	for i := range s.fields {
		if s.fields[i].kind == akString {
			fields = append(fields, i)
		}
	}
	if len(fields) == 0 {
		return nil, nil, nil
	}
	// One pass over the records collecting each string field's values, so the walk happens once here
	// rather than once per query.
	vals := make(map[int][][]byte, len(fields))
	present := make(map[int][]bool, len(fields))
	for _, i := range fields {
		vals[i] = make([][]byte, n)
		present[i] = make([]bool, n)
	}
	for k, r := range recs {
		esc := r[:s.escBytes]
		p := s.escBytes + s.fixedLen
		for i := range s.fields {
			if s.fields[i].kind != akString || testBit(esc, i) {
				continue
			}
			l, m := binary.Uvarint(r[p:])
			if m <= 0 || p+m+int(l) > len(r) {
				return nil, nil, nil // malformed; leave every field positional
			}
			vals[i][k] = r[p+m : p+m+int(l)]
			present[i][k] = true
			p += m + int(l)
		}
	}

	out := map[int]strDictField{}
	var dictRaw, codeRaw []byte
	for _, i := range fields {
		uniq := distinctFold(vals[i], present[i])
		if !dictWorthwhile(len(uniq), n) {
			continue
		}
		info := strDictField{
			codeStart: len(codeRaw),
			codeWidth: 1,
			dictStart: len(dictRaw),
			count:     len(uniq),
			noEscape:  true,
		}
		for k := 0; k < n; k++ {
			if !present[i][k] {
				info.noEscape = false
				break
			}
		}
		if len(uniq) > 1<<8 {
			info.codeWidth = 2
		}
		// The dictionary: uvarint length then bytes, in fold-sorted order so codes are monotone.
		index := make(map[string]int, len(uniq))
		for c, v := range uniq {
			index[string(v)] = c
			dictRaw = binary.AppendUvarint(dictRaw, uint64(len(v)))
			dictRaw = append(dictRaw, v...)
		}
		// The code column, columnar. An escaped or absent value gets code 0; nothing reads it, because the
		// escape bitmap is consulted first, exactly as for a numeric column.
		codeRaw = append(codeRaw, make([]byte, info.codeWidth*n)...)
		for k := 0; k < n; k++ {
			if !present[i][k] {
				continue
			}
			c := index[string(vals[i][k])]
			off := info.codeStart + k*info.codeWidth
			codeRaw[off] = byte(c)
			if info.codeWidth == 2 {
				codeRaw[off+1] = byte(c >> 8)
			}
		}
		out[i] = info
	}
	if len(out) == 0 {
		return nil, nil, nil
	}
	return out, dictRaw, codeRaw
}

// dictWorthwhile reports whether a field with this cardinality is worth a dictionary. See dictMinRepeat.
func dictWorthwhile(distinct, n int) bool {
	return distinct > 0 && distinct < dictMaxCardinality && distinct*dictMinRepeat <= n
}

// distinctFold returns the field's distinct values, sorted by the evaluator's fold comparison with an
// exact-byte tiebreak.
//
// The fold sort is what lets ordering comparisons become code range checks; the tiebreak keeps the order
// total, so two values that fold equal still get distinct, adjacent codes -- which is what =?= needs.
func distinctFold(vals [][]byte, present []bool) [][]byte {
	seen := make(map[string]struct{}, 16)
	var uniq [][]byte
	for k, v := range vals {
		if !present[k] {
			continue
		}
		if _, ok := seen[string(v)]; ok {
			continue
		}
		seen[string(v)] = struct{}{}
		uniq = append(uniq, v)
	}
	sortDictFold(uniq)
	return uniq
}

func sortDictFold(uniq [][]byte) {
	// Insertion sort: a block's cardinality is bounded by dictMaxCardinality and in practice small, and
	// this runs once per block at seal time rather than per query.
	for i := 1; i < len(uniq); i++ {
		for j := i; j > 0 && dictLess(uniq[j], uniq[j-1]); j-- {
			uniq[j], uniq[j-1] = uniq[j-1], uniq[j]
		}
	}
}

func dictLess(a, b []byte) bool {
	if c := classad.CompareStringsFold(bytesToStr(a), bytesToStr(b)); c != 0 {
		return c < 0
	}
	return string(a) < string(b) // total order for fold-equal values, so =?= keeps distinct codes
}

// bytesToStr aliases b as a string without copying. Safe here: every caller passes a subslice of an
// immutable decompressed region or record buffer, and never retains the string past the comparison.
func bytesToStr(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(&b[0], len(b))
}

// dictEntries returns field fieldIdx's dictionary entries, aliasing the decompressed region and reusing
// the caller's buffer.
//
// Parsed per call rather than cached on the block: the parse is a uvarint walk over a small dictionary and
// happens once per BLOCK per query, against once per RECORD for the positional walk it replaces. Caching
// the parsed slices on the block instead would pin a region the block cache is entitled to evict.
//
// buf is the caller's scratch, reused across blocks. Allocating it here made a 512-entry dictionary ~12 KB
// of garbage per block -- across a segment, more garbage than the walk it replaces ever cost.
func (b *columnarBlock) dictEntries(fieldIdx int, bc *blockCache, buf *[][]byte) ([][]byte, bool) {
	info, ok := b.strDictOf(fieldIdx)
	if !ok {
		return nil, false
	}
	region, err := bc.stream(b, kindStrDict)
	if err != nil || info.dictStart > len(region) {
		return nil, false
	}
	p := region[info.dictStart:]
	out := (*buf)[:0]
	if cap(out) < info.count {
		out = make([][]byte, 0, info.count)
	}
	for j := 0; j < info.count; j++ {
		l, m := binary.Uvarint(p)
		if m <= 0 || m+int(l) > len(p) {
			return nil, false
		}
		out = append(out, p[m:m+int(l)])
		p = p[m+int(l):]
	}
	*buf = out
	return out, true
}

// dictCodes returns field fieldIdx's code column, its width, and the decompressed code region.
func (b *columnarBlock) dictCodes(fieldIdx int, bc *blockCache) ([]byte, int, bool) {
	info, ok := b.strDictOf(fieldIdx)
	if !ok {
		return nil, 0, false
	}
	region, err := bc.stream(b, kindStrCode)
	if err != nil {
		return nil, 0, false
	}
	end := info.codeStart + info.codeWidth*b.n
	if end > len(region) {
		return nil, 0, false
	}
	return region[info.codeStart:end], info.codeWidth, true
}

// appendNonDictStrings appends record r's positional string region, OMITTING the fields a dictionary owns.
//
// The dictionary is authoritative for those, so writing them here too would store every value twice. The
// omission is why every reader of this region has to consult strDict as it walks: the region holds a subset
// of the schema's string fields, in schema order, and nothing in the bytes says which.
func appendNonDictStrings(s *adSchema, r []byte, dicts map[int]strDictField, dst []byte) ([]byte, bool) {
	esc := r[:s.escBytes]
	p := s.escBytes + s.fixedLen
	for i := range s.fields {
		if s.fields[i].kind != akString || testBit(esc, i) {
			continue
		}
		l, m := binary.Uvarint(r[p:])
		if m <= 0 || p+m+int(l) > len(r) {
			return dst, false
		}
		if _, owned := dicts[i]; !owned {
			dst = append(dst, r[p:p+m+int(l)]...)
		}
		p += m + int(l)
	}
	return dst, true
}

// dictOwns reports whether the dictionary is authoritative for this field, so a positional walk must skip it.
func (b *columnarBlock) dictOwns(fieldIdx int) bool {
	_, ok := b.strDictOf(fieldIdx)
	return ok
}

// dictCodeAt reads record k's code from a code column of the given width.
func dictCodeAt(codes []byte, width, k int) int {
	if width == 1 {
		return int(codes[k])
	}
	return int(codes[2*k]) | int(codes[2*k+1])<<8
}

// dictRange finds the codes whose entries compare fold-EQUAL to lit, as the half-open range [lo, hi).
//
// The range exists because dictLess sorts fold-first with an exact-byte tiebreak, so entries that fold
// equal are contiguous. Two binary searches find it in O(log cardinality) fold comparisons, against O(n)
// for comparing every record's string -- and the result is a RANGE rather than a set, which is what makes
// every comparison operator an integer test on the code:
//
//	== lit   lo <= c < hi        < lit   c < lo         > lit   c >= hi
//	!= lit   !(lo <= c < hi)     <= lit  c < hi         >= lit  c >= lo
//
// A hash of the folded value would also turn the probe into integer work, but it would give set membership
// instead of a range, would need a collision check behind every hit, and would still cost O(cardinality) --
// the sorted order gives strictly more for less. lo == hi means no entry matches at all.
func (b *columnarBlock) dictRange(fieldIdx int, lit string, bc *blockCache, buf *[][]byte) (lo, hi int, ok bool) {
	entries, ok := b.dictEntries(fieldIdx, bc, buf)
	if !ok {
		return 0, 0, false
	}
	// lo: the first entry that is not fold-less than lit. hi: the first that is fold-greater.
	lo = sort.Search(len(entries), func(j int) bool {
		return classad.CompareStringsFold(bytesToStr(entries[j]), lit) >= 0
	})
	hi = lo + sort.Search(len(entries)-lo, func(j int) bool {
		return classad.CompareStringsFold(bytesToStr(entries[lo+j]), lit) > 0
	})
	return lo, hi, true
}

// dictPrunes reports whether this block can be ruled out by any of the string comparison conjuncts.
//
// Completeness is the whole soundness condition, and noEscape records it: an escaped record's value lives in
// the cold tail where the dictionary cannot see it, so a partial dictionary must not prune.
func (b *columnarBlock) dictPrunes(probes []strProbe, bc *blockCache, buf *[][]byte) bool {
	for _, p := range probes {
		info, ok := b.strDictOf(p.fieldIdx)
		if !ok || !info.noEscape {
			continue
		}
		if b.probePrunes(p, info, bc, buf) {
			return true
		}
	}
	return false
}

// probePrunes reports whether one conjunct rules the block out.
//
// Equality asks whether ANY of its values is present. Ordering asks where the literal falls in the
// fold-ordered dictionary: everything below the literal occupies codes [0, lo) and everything above occupies
// [hi, count), so an empty side means no record in the block can satisfy that direction.
func (b *columnarBlock) probePrunes(p strProbe, info strDictField, bc *blockCache, buf *[][]byte) bool {
	entries, ok := b.dictEntries(p.fieldIdx, bc, buf)
	if !ok {
		return false
	}
	switch p.op {
	case "<", "<=", ">", ">=":
		lo, hi, ok := b.dictRange(p.fieldIdx, p.lits[0], bc, buf)
		if !ok {
			return false
		}
		switch p.op {
		case "<":
			return lo == 0 // no entry folds below the literal
		case "<=":
			return hi == 0
		case ">":
			return hi == info.count // no entry folds above it
		default: // ">="
			return lo == info.count
		}
	}
	// Equality-shaped: prune only when NONE of the values could be present.
	for _, lit := range p.lits {
		lo, hi, ok := b.dictRange(p.fieldIdx, lit, bc, buf)
		if !ok {
			return false
		}
		if lo == hi {
			continue // nothing folds equal to this value
		}
		if p.op != "is" {
			return false // a fold-equal entry exists, so the block may match
		}
		// =?= is byte-exact, and fold-equal entries are contiguous, so only the fold range can hold it.
		for j := lo; j < hi && j < len(entries); j++ {
			if bytesToStr(entries[j]) == lit {
				return false
			}
		}
	}
	return true
}

// blockDict resolves one block's dictionary for the vector executor, so a comparison against a literal
// becomes an integer range test rather than a per-record string comparison.
//
// It holds the parsed entries for exactly one (block, field) and is reused across blocks by the scan, so the
// per-block parse is paid once whether the executor asks for a range, a string, or both.
type blockDict struct {
	entries [][]byte
}

func (d *blockDict) Len() int { return len(d.entries) }

// Range returns the codes whose entries compare fold-EQUAL to lit, as [lo, hi). Contiguous because the
// dictionary is sorted fold-first; see dictLess.
func (d *blockDict) Range(lit string) (int, int, bool) {
	lo := sort.Search(len(d.entries), func(j int) bool {
		return classad.CompareStringsFold(bytesToStr(d.entries[j]), lit) >= 0
	})
	hi := lo + sort.Search(len(d.entries)-lo, func(j int) bool {
		return classad.CompareStringsFold(bytesToStr(d.entries[lo+j]), lit) > 0
	})
	return lo, hi, true
}

func (d *blockDict) At(code int) (string, bool) {
	if code < 0 || code >= len(d.entries) {
		return "", false
	}
	return bytesToStr(d.entries[code]), true
}
