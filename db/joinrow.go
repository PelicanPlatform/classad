package db

import (
	"iter"
	"strings"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
	"github.com/PelicanPlatform/classad/parser"
)

// CARRYING A SIDE'S ROWS AS THE STORE HANDED THEM OVER.
//
// The previous step narrowed both sides of a join to the attributes it actually reads, and
// the store serves those rows in wire form. They were then decoded into ClassAds before the
// pair loop saw them, because that is what everything downstream expected.
//
// That decode is not free and it is paid on EVERY row of both sides, before a single pair is
// examined. Measured on the shape this is wanted for -- history joined to epoch_history, a
// handful of attributes out of eighty -- reading both sides wire-form costs 15.5 ms and ten
// thousand allocations, and decoding those same rows into ads costs 20.3 ms and two hundred
// thousand. A third of the read, and twenty times the allocations, for objects most of which
// no surviving row ever carries.
//
// So a row is carried as the bytes, and becomes an ad only when something needs more than
// the bytes can answer. For the comparisons a join actually writes -- an attribute against a
// literal, an attribute against the other side's attribute -- the bytes answer, and the ad is
// never built.
//
// WHAT "MORE THAN THE BYTES CAN ANSWER" MEANS, and why the fallback is not a rare path to be
// tidied away later. An attribute's stored node is a literal only when the attribute holds a
// value; job ads are full of attributes holding EXPRESSIONS, and one of those has to be
// evaluated in the context of its own ad, which is exactly what an ad is for. A sealed value
// is not a literal either. Both decode, and decoding is the same answer the join gave before
// this existed -- which is what makes every case this does not handle correct rather than
// merely absent.

// joinRow is one side's row: the wire bytes the store produced, the decoded ad, or both.
//
// A nil *joinRow is the absent right side of an unmatched LEFT row, so every method here is
// nil-safe and reads as undefined, matching the absent member of a composite.
type joinRow struct {
	// w is the row as the store handed it over, or nil when it arrived already decoded --
	// which is what a side read whole does, since only a narrowed read serves wire.
	w wire.Ad
	// ad is the decoded form. It is built at most once, by record.
	//
	// ATOMIC because a parallel walk shares the hashed sides' rows across walkers. The ad
	// itself is safe to read concurrently -- the classad package documents that reads do not
	// modify an ad and need no locking -- but THIS field is a write on a read path, and two
	// walkers reaching an undecoded row at once would race on it. Decoding is pure, so a
	// losing racer's ad is simply dropped: both goroutines built equal ads and either will do.
	ad atomic.Pointer[classad.ClassAd]
}

// decodedRow is a joinRow that arrived already decoded, as a side read whole does.
func decodedRow(ad *classad.ClassAd) *joinRow {
	r := &joinRow{}
	r.ad.Store(ad)
	return r
}

// record returns the row as a ClassAd, decoding it the first time it is asked for.
//
// A row that will not decode becomes an empty ad rather than an error. It cannot happen:
// these bytes were assembled by the store from a record it had already read. Returning an
// empty ad rather than nil keeps every caller from needing a second nil check for a case
// that does not arise.
func (r *joinRow) record() *classad.ClassAd {
	if r == nil {
		return nil
	}
	if a := r.ad.Load(); a != nil {
		return a
	}
	built := classad.New()
	if a, err := wire.DecodeInline(r.w); err == nil {
		built = classad.FromAST(a)
	}
	if r.ad.CompareAndSwap(nil, built) {
		return built
	}
	return r.ad.Load() // another walker decoded it first; its ad is equal to ours
}

// attr returns the value of one attribute, reading the wire bytes when they hold the answer
// and decoding when they do not.
//
// ok=false means only "the bytes could not answer this"; the caller decodes and asks the ad.
// It is never a failure and never a reason to drop a row.
//
// An attribute ABSENT from the row is answered here, as undefined, rather than deferred: the
// ad would be decoded from these same bytes and would not have it either, so the two agree by
// construction, and deferring would decode every ad that a condition tests for absence.
func (r *joinRow) attr(name string) (classad.Value, bool) {
	if r == nil {
		return classad.NewUndefinedValue(), true
	}
	if a := r.ad.Load(); a != nil {
		return a.EvaluateAttr(name), true // already decoded: no reason to read bytes
	}
	node, found := r.w.LookupByName(name)
	if !found {
		if r.w == nil {
			return classad.Value{}, false // no bytes to read: it has to be the ad
		}
		return classad.NewUndefinedValue(), true
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return classad.Value{}, false // an expression, a list, a record or a sealed value
	}
	return litValue(lit), true
}

// eval is attr with the fallback taken, so a caller that has nothing better to do than decode
// does not have to write the decode out.
func (r *joinRow) eval(name string) classad.Value {
	if v, ok := r.attr(name); ok {
		return v
	}
	return r.record().EvaluateAttr(name)
}

// litValue converts a wire literal to a ClassAd value. It mirrors the same conversion in
// collections, which is not exported and should not be: this is three lines of switch, and
// sharing it would couple db to that package's evaluation internals to save them.
func litValue(l wire.Literal) classad.Value {
	switch l.Kind {
	case wire.LitError:
		return classad.NewErrorValue()
	case wire.LitBool:
		return classad.NewBoolValue(l.Bool)
	case wire.LitInt:
		return classad.NewIntValue(l.Int)
	case wire.LitReal:
		return classad.NewRealValue(l.Real)
	case wire.LitString:
		return classad.NewStringValue(l.Str)
	default: // LitUndef
		return classad.NewUndefinedValue()
	}
}

// forceDecodedRows makes every row decode as it is read, which is exactly what the join did
// before it carried the bytes: each side's rows arrive as ads, so attr, key and member all
// take their fallbacks and nothing reads a byte.
//
// It is the oracle's switch, and the A/B's. A row form that changes which pairs survive, or
// what an output row carries, is the only way this can be wrong, and running the SAME join
// both ways over the same fixture is the only thing that can see it.
//
// Like forceWholeRead beside it, nothing outside a test may set it, and a test that does must
// not call t.Parallel.
var forceDecodedRows bool

// wireRows carries wire-form rows into joinRows, copying each one.
//
// The copy is not optional. Each row the scan yields aliases a buffer the scan REUSES, and a
// join holds rows: the hashed side whole, and the streamed side too when the index probe held
// it to count keys. Keeping the slice would leave every held row pointing at whatever the
// scan last decoded into that buffer.
//
// Decoding used to hide this, because the decoder copies every string and name it reads. That
// is a property of another package, and what made it safe is gone the moment the bytes are
// what is kept.
func wireRows(rows iter.Seq[[]byte]) iter.Seq[*joinRow] {
	return func(yield func(*joinRow) bool) {
		// SLABBED, because the copy is needed and the allocation is not.
		//
		// The yielded row aliases a buffer the scan reuses, so a joinRow that outlives the
		// step -- every row of a hashed side does, they sit in its buckets for the whole
		// join -- has to own its bytes. That was `append([]byte(nil), row...)` plus a
		// `&joinRow{}`: two allocations for every row of every side the join reads, which
		// the allocation profile put at 81% of the join's objects.
		//
		// Both come out of slabs instead. Each row still gets bytes that are its own and
		// never written again -- the arena is only ever appended to, and the three-index
		// slice stops a later row's append from reaching into an earlier row's bytes -- so
		// what a joinRow may assume about its bytes is unchanged. The arena is never grown
		// by append: a row that does not fit starts a new one, because growing it would
		// move the bytes every row handed out so far is still pointing at.
		const slabRows = 512
		const arenaBytes = 64 << 10
		var structs []joinRow
		var arena []byte
		for row := range rows {
			if len(structs) == 0 {
				structs = make([]joinRow, slabRows)
			}
			if cap(arena)-len(arena) < len(row) {
				n := arenaBytes
				if len(row) > n {
					n = len(row) // one oversized row gets an arena of its own
				}
				arena = make([]byte, 0, n)
			}
			start := len(arena)
			arena = append(arena, row...)
			r := &structs[0]
			structs = structs[1:]
			r.w = wire.Ad(arena[start:len(arena):len(arena)]) // ad stays nil: a fresh slab cell
			if forceDecodedRows {
				r = decodedRow(r.record())
			}
			if !yield(r) {
				return
			}
		}
	}
}

// adRows carries already-decoded ads into joinRows, for a side read whole.
func adRows(ads iter.Seq[*classad.ClassAd]) iter.Seq[*joinRow] {
	return func(yield func(*joinRow) bool) {
		for ad := range ads {
			if !yield(decodedRow(ad)) {
				return
			}
		}
	}
}

// rowsSeq ranges a held slice of rows.
func rowsSeq(rows []*joinRow) iter.Seq[*joinRow] {
	return func(yield func(*joinRow) bool) {
		for _, r := range rows {
			if !yield(r) {
				return
			}
		}
	}
}

// bareAttrName returns the attribute an expression reads when it is nothing but a reference
// to one, and "" otherwise.
//
// It admits exactly what planJoinRead's walk admits, for the same reason and with the same
// consequence for being wrong: a half that reads MORE than this name must not be answered
// from that one attribute. A scoped reference names another ad, and CurrentTime is the clock
// rather than an attribute, so neither is a bare name here.
func bareAttrName(src string) string {
	if strings.TrimSpace(src) == "" {
		return ""
	}
	e, err := parser.ParseExpr(src)
	if err != nil {
		return ""
	}
	for {
		paren, ok := e.(*ast.ParenExpr)
		if !ok {
			break
		}
		e = paren.Inner
	}
	ref, ok := e.(*ast.AttributeReference)
	if !ok {
		return ""
	}
	if ref.Scope != ast.NoScope && ref.Scope != ast.MyScope {
		return ""
	}
	if classad.IsCurrentTimeName(ref.Name) {
		return ""
	}
	return ref.Name
}

// member builds one side's contribution to the joined row: the row narrowed to the
// attributes the output carries.
//
// This is where carrying the bytes pays. The output projection is a handful of attributes;
// the row was read with that projection PLUS whatever the condition and the equalities read,
// plus the closure of every expression among them -- so decoding the row to copy a few
// attributes out of it decodes the rest for nothing. Decoding only the attributes asked for
// skips that, and skips it on the rows that survive, which is where the work is.
//
// It falls back to the full decode whenever the bytes cannot answer -- a row that arrived
// already decoded, a whole-ad projection, a node that will not decode on its own. The
// fallback is the behaviour this replaces, so every case it covers is right rather than
// merely handled.
func (r *joinRow) member(attrs []string) *classad.ClassAd {
	if r == nil {
		return nil
	}
	if attrs == nil || r.ad.Load() != nil || r.w == nil {
		return project(r.record(), attrs)
	}
	out := &ast.ClassAd{Attributes: make([]*ast.AttributeAssignment, 0, len(attrs))}
	for _, name := range attrs {
		node, found := r.w.LookupByName(name)
		if !found {
			continue // absent is absent, exactly as a projection off a decoded ad leaves it
		}
		e, err := wire.DecodeNodeInline(node)
		if err != nil {
			return project(r.record(), attrs)
		}
		out.Attributes = append(out.Attributes, &ast.AttributeAssignment{Name: name, Value: e})
	}
	return classad.FromAST(out)
}
