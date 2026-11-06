package collections

import (
	"math"
	"sort"
	"strings"
)

// Prefix probes over a categorical index.
//
// A "prefix" probe (vm.regexpPrefixProbe) asks for the records whose string value begins
// with a literal. A categorical index keys on the FOLDED (lower-cased) value, and the keys
// a prefix admits are a contiguous run of that key order -- exactly the shape a value
// index's sortedKeys already exploits for `<`/`>` -- so the probe is answered by boundary-
// searching to the run and unioning only those keys' postings.
//
// WHY FOLDING IS SOUND HERE. strings.ToLower maps each rune independently, so folding a
// value folds its prefix: a value beginning with P has a folded form beginning with
// fold(P). The converse does not hold -- "AP40job" folds into the fold("ap40") run while
// `regexp("^ap40", ...)` (case-sensitive by default) does not match it -- which makes the
// run a SUPERSET of the matching records. That is what a probe is for, and the store
// re-evaluates the pattern over every candidate. It is also why the "i" regexp option
// needs no separate handling: the folded run already covers the case-insensitive match.
//
// The exception set joins the candidates as it does for equality: a record whose attribute
// holds an expression rather than a string literal is in exc, in no key's postings, and can
// still evaluate to a matching string.

// prefixUpperKey returns the least string greater than every string beginning with p -- p
// with its last non-0xFF byte incremented and the rest dropped -- so [p, prefixUpperKey(p))
// is exactly the prefixed range under bytewise order (which is Go's string order, and which
// agrees with code point order for UTF-8).
//
// ok is false when p is entirely 0xFF bytes and no such string exists; the range then runs
// to the end of the keys. A key that came from a Go string is valid UTF-8 and so contains no
// 0xFF at all, but the caller must not have to know that.
func prefixUpperKey(p string) (string, bool) {
	b := []byte(p)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0xFF {
			continue
		}
		b[i]++
		return string(b[:i+1]), true
	}
	return "", false
}

// strPrefixSpan returns the [from, to) window of an ascending key run whose keys begin with
// p. Two boundary searches, so it costs O(log n) whether the run holds one key or a million
// -- which is what lets the planner ask how much a prefix prunes without paying to find out.
func strPrefixSpan(keys []string, p string) (from, to int) {
	from = sort.SearchStrings(keys, p)
	hi, ok := prefixUpperKey(p)
	if !ok {
		return from, len(keys)
	}
	return from, sort.SearchStrings(keys, hi)
}

// A prefix whose run covers EVERY key of the attribute prunes nothing, and unioning one
// bitmap per key to discover that is the most expensive way to arrive at it -- thousands of
// postings on an attribute with a distinct value per record. The union of every key's
// postings is by definition the records that posted a literal value, which both tiers
// already keep as a single ready bitmap, so the degenerate case costs what a presence probe
// costs instead. The over-read gate declines such a probe before execution on an append-only
// collection; on a mutable one it does not (see overReadGate), which is exactly where this
// matters.

// catPrefixSpan is strPrefixSpan over one attribute's folded keys, or ok=false when the
// attribute is not indexed categorically in this segment.
func (si *segIndex) catPrefixSpan(up usableProbe) (from, to int, ok bool) {
	cp := si.cat[up.attrID]
	if cp == nil || len(up.svals) != 1 {
		return 0, 0, false
	}
	from, to = strPrefixSpan(cp.sortedKeys(), up.svals[0])
	return from, to, true
}

func (si *segIndex) prefixKeyCount(up usableProbe) (int, bool) {
	from, to, ok := si.catPrefixSpan(up)
	return to - from, ok
}

// estPrefixCandidates estimates the records a prefix probe admits, mirroring how an
// equality is estimated: the heavy hitters inside the run are counted exactly, and the
// remaining keys of the run at the average tail count. The key count comes from a boundary
// search, never from reading postings, so a planner that decides against the index has not
// paid to decide.
func estPrefixCandidates(ix indexPrimitives, up usableProbe) float64 {
	s := ix.statsFor(up)
	if s == nil {
		return math.MaxFloat64 // unknown: apply last
	}
	n, ok := ix.prefixKeyCount(up)
	if !ok {
		return float64(s.covered)
	}
	p := up.svals[0]
	var sum float64
	top := 0
	for _, e := range s.top {
		if strings.HasPrefix(e.skey, p) {
			sum += float64(e.count)
			top++
		}
	}
	if rest := n - top; rest > 0 {
		sum += float64(rest) * s.avgTailCount()
	}
	return sum + float64(s.exc)
}
