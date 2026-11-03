package db

import (
	"encoding/binary"
	"errors"
	"iter"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// EMITTING A JOINED ROW AS BYTES.
//
// A joined row that crosses a wire is rendered to old-ClassAd text by the relay and parsed
// back by whoever receives it. Measured on the shape this is wanted for -- history joined to
// epoch_history, 8000 joined rows -- the join itself costs 450,757 allocations, rendering
// those rows costs another 350,978, and the consumer's parse costs 536,132 more. The render
// and the parse together are about twice the join, and neither depends on the transport.
//
// Both disappear if the row leaves as the bytes it was assembled from. The two sides are
// already carried as wire rows (see joinRow); a composite is those two bodies spliced under
// their aliases, which is a length prefix and two copies rather than a decode, a rebuild and
// a render.
//
// WHAT A COMPOSITE IS, in the wire format. An ad is a header (magic, version, flags) then a
// hot index, an attribute count, and the attribute entries. A nested ad is a node with tag
// nRecord whose payload is "the same layout, never standalone" -- that is, a body with no
// header of its own. So the composite is a header of its own, two entries, and each entry is
// a key plus an nRecord node wrapping one side's body. The side's body is copied verbatim:
// its attributes keep their stored form, their names and their order, which is what makes
// this the same answer the decode-and-rebuild path gives rather than a similar one.
//
// WHY THE HOT INDEX IS DROPPED. It maps a name hash to an absolute offset into the entry
// region, and a spliced region moves every offset. BuildAd writes an empty one for the same
// reason, and nothing is lost: it is a read accelerator the reader repopulates.
//
// WHAT DECLINES. A side read whole arrives decoded, with no bytes to splice, and a projection
// of "the whole ad" has no narrowed body to copy. Both fall back to building the composite as
// a ClassAd and encoding it, which is slower than splicing and identical in result. Declining
// costs the speed; getting it wrong costs the answer.

// joinWireRow is a joined row in wire form: a self-contained inline-names ad whose two
// members are the sides under their aliases.
//
// The bytes alias a buffer this reuses between rows, exactly as the store's own raw scans do,
// so a caller that keeps one must copy it. Every caller here either writes it to a frame or
// decodes it immediately.

// spliceComposite builds the joined row's wire bytes into dst, returning the extended slice
// and whether it could be done at all.
//
// ok=false is never an error: it means one of the sides cannot be spliced -- it arrived
// decoded, or the caller wants its whole ad -- and the caller forms the row the ordinary way.
func (p *joinPlan) spliceComposite(dst []byte, l, r *joinRow) ([]byte, bool) {
	lb, lok := l.memberBody(p.leftProject)
	if !lok {
		return dst, false
	}
	rb, rok := r.memberBody(p.rightProject)
	if !rok {
		return dst, false
	}
	n := 0
	if lb.ok {
		n++
	}
	if rb.ok {
		n++
	}
	// An inline-names header: self-contained, so the row decodes with no intern table, which
	// is what a row crossing a wire has to be.
	dst = append(dst, wireMagic, wireVersion, wireFlagInlineNames)
	dst = binary.AppendUvarint(dst, 0)         // hot index: empty, see above
	dst = binary.AppendUvarint(dst, uint64(n)) // attribute count
	if lb.ok {
		dst = appendMemberEntry(dst, p.leftAlias, lb)
	}
	if rb.ok {
		dst = appendMemberEntry(dst, p.rightAlias, rb)
	}
	return dst, true
}

// appendMemberEntry writes one side as `alias = [ ... ]`: an inline name, then an nRecord
// node carrying that side's body.
func appendMemberEntry(dst []byte, alias string, m memberBody) []byte {
	dst = wire.AppendKey(dst, true, 0, alias)
	dst = append(dst, wireNodeRecord)
	dst = binary.AppendUvarint(dst, 0) // the nested body's own hot index: empty
	dst = binary.AppendUvarint(dst, uint64(m.n))
	return append(dst, m.entries...)
}

// memberBody is one side's attribute entries and how many there are, ready to be wrapped in
// an nRecord node. ok is false for an absent side -- the unmatched right half of a LEFT row --
// which contributes no member at all, exactly as it contributes none to a composite ClassAd.
type memberBody struct {
	entries []byte
	n       int
	ok      bool
}

// memberBody narrows a side to the attributes the output carries, as bytes.
//
// It is the wire twin of member, and the pair has to agree: an attribute absent from the row
// is absent from the member, a name is carried in the projection's own spelling, and the
// order is the projection's. The one thing it does NOT do is decode a node, which is the
// whole point -- an attribute holding an expression is copied as the expression it is, with
// no AST built for it.
func (r *joinRow) memberBody(attrs []string) (memberBody, bool) {
	if r == nil {
		return memberBody{}, true // an absent side splices as no member
	}
	if attrs == nil || r.w == nil {
		// The whole ad, or a side with no bytes: there is nothing to copy from.
		return memberBody{}, false
	}
	// The row has to be inline-names, and this has to be checked rather than inferred.
	// LookupByName answers (nil, false) for an absent attribute AND for an ad that is not
	// inline, so a row keyed by interned ids would report every attribute missing and splice
	// an EMPTY member -- a joined row silently short of everything the caller asked for.
	// Worse, its nodes would carry interned ids that the composite's inline-names header
	// tells a decoder to read as inline names.
	if _, _, _, inline, ok := r.w.SplitBody(); !ok || !inline {
		return memberBody{}, false
	}
	var out memberBody
	for _, name := range attrs {
		node, found := r.w.LookupByName(name)
		if !found {
			continue
		}
		out.entries = wire.AppendKey(out.entries, true, 0, name)
		out.entries = append(out.entries, node...)
		out.n++
	}
	out.ok = true
	return out, true
}

// Wire-format constants this splices with. They are restated rather than exported from the
// wire package because that package keeps them unexported deliberately -- the format is its
// business -- and because a splice that silently followed a format change would produce rows
// that decode to the wrong thing rather than failing. TestSplicedRowDecodesAsTheCompositeDoes
// is what ties them to the real format: it decodes every spliced row with the real decoder
// and compares it against the composite the ordinary path builds.
const (
	wireMagic           = 0xCA
	wireVersion         = 1
	wireFlagInlineNames = 1 << 1
	wireNodeRecord      = 0x0B
)

// encodeCompositeInline renders a composite ad to the same self-contained wire form the
// splice produces, for the rows the splice declines. The projections it is given are already
// redacted, so which path a row took cannot change what the row carries.
func encodeCompositeInline(dst []byte, row *classad.ClassAd) []byte {
	return wire.EncodeInline(dst, row.AST())
}

// publicProjection is attrs without the private names, or attrs itself when nothing is being
// withheld.
//
// Redacting the PROJECTION rather than the finished row is what makes the two paths agree: a
// composite's own attributes are the two aliases, which are never private, so the private
// names live one level down inside each member -- and both the splice and the fallback build
// a member from this list. Filtering it once covers them both.
func publicProjection(attrs []string, redact bool) []string {
	if !redact || attrs == nil {
		return attrs
	}
	out := make([]string, 0, len(attrs))
	for _, a := range attrs {
		if !classad.IsPrivateAttribute(a) {
			out = append(out, a)
		}
	}
	return out
}

// ErrJoinWireRedactNeedsProjection reports that a redacted wire-form join was asked for over
// a side whose whole ad was requested.
//
// A whole-ad member has no projection to filter, and the private names are inside the member
// rather than at the top of the row, so there is nothing here that can withhold them. It
// refuses rather than shipping the member intact -- the caller falls back to the text row
// stream, where the render withholds them. A refusal costs the fallback; guessing costs the
// private attributes of every row.
var ErrJoinWireRedactNeedsProjection = errors.New(
	"db: a redacted wire-form join needs a projection on each side")

// JoinWire is Join with its rows in wire form: each row a self-contained inline-names ad,
// ready to be written to a frame or decoded by whoever wants an object.
//
// redact withholds the private attributes of each side, which is what the text path's render
// does and what a row leaving the store for an unprivileged reader needs. The join itself
// still runs over the FULL rows -- redacting before the condition would make which rows match
// depend on who is asking.
//
// The bytes handed to yield alias a buffer this reuses. A caller that keeps a row past the
// next iteration must copy it -- the same contract the store's own raw scans have.
//
// Join is unchanged and remains the entry point for a caller that wants ads. This exists
// beside it rather than replacing it because a joined row's consumer decides what it wants:
// one that sorts or filters wants an object, and one that relays wants bytes.
func (cat *Catalog) JoinWire(spec JoinSpec, want JoinLimits, redact bool, stats *JoinStats) (iter.Seq[[]byte], error) {
	if redact && (spec.Left.Project == nil || spec.Right.Project == nil) {
		return nil, ErrJoinWireRedactNeedsProjection
	}
	// Redact the PROJECTIONS, not the finished rows. spec is a value, so narrowing them here
	// narrows what each member carries without touching what the join reads: planJoinRead
	// unions the condition's own references into the read, so a term over a private
	// attribute still decides which rows match -- it just does not leave the store.
	spec.Left.Project = publicProjection(spec.Left.Project, redact)
	spec.Right.Project = publicProjection(spec.Right.Project, redact)
	limits := DefaultJoinLimits().clamp(want)
	var buf []byte
	return joinStream(cat, spec, limits, stats, func(p *joinPlan, l, r *joinRow) ([]byte, bool) {
		if b, ok := p.spliceComposite(buf[:0], l, r); ok {
			buf = b
			return b, true
		}
		// A side this cannot splice: build the row and encode it, which is the same ad by a
		// longer road.
		buf = encodeCompositeInline(buf[:0], p.composite(l, r))
		return buf, true
	})
}
