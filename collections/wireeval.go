package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireCtx supplies the mode-aware wire touchpoints (interned-id vs inline-name
// attribute lookup and decode) that wire-native matching needs. Both *Collection
// and *Archive implement it, so they share one match path (matchWire).
//
// The dict-aware decoders belong to the context, not to free functions over a segment's resolver,
// because only the context holds the key that opens a sealed value. An interned segment of an
// encrypted collection carries sealed value nodes just as an inline one does, and a decode that
// resolved its ids without the key failed on every sealed value -- read as absent, so a filter on
// a sealed attribute silently dropped every record stored in an interned segment.
type wireCtx interface {
	decodeWire(w []byte) (*ast.ClassAd, error)
	wireLookup(a wire.Ad, name string) ([]byte, bool)
	decodeNode(node []byte) (ast.Expr, error)
	decodeWireDict(dict *segDictHandle, w []byte) (*ast.ClassAd, error)
	decodeNodeDict(dict *segDictHandle, node []byte) (ast.Expr, error)
}

// keylessCtx is the wire context of an evaluation for a reader NOT entitled to sealed values. It names
// attributes exactly as the collection does but holds no key, so a sealed value decodes to undefined --
// what that reader is shown if it projects the attribute -- and is never opened.
//
// Redacting only what a read RETURNS is not enough. Evaluation decides which records match, which
// group a record lands in and where it sorts, and each of those is itself an answer: `count(*) where
// Secret == "guess"` confirms a guess, a range constraint binary-searches a number, and GROUP BY
// Secret hands back the distinct values as group keys. A reader that may not see a value must not be
// able to evaluate it either, so its evaluation reads through this context and never through the
// collection's key.
type keylessCtx struct{ c *Collection }

func (k keylessCtx) decodeWire(w []byte) (*ast.ClassAd, error) {
	return k.c.decodeWireDictAs(nil, w, true)
}

func (k keylessCtx) wireLookup(a wire.Ad, name string) ([]byte, bool) { return k.c.wireLookup(a, name) }

func (k keylessCtx) decodeNode(node []byte) (ast.Expr, error) { return k.decodeNodeDict(nil, node) }

func (k keylessCtx) decodeWireDict(dict *segDictHandle, w []byte) (*ast.ClassAd, error) {
	return k.c.decodeWireDictAs(dict, w, true)
}

// decodeNodeDict decodes one attribute's value node. A sealed node is undefined, as the redacting ad
// decoders make it; anything else carries no sealed value inside it (only a whole attribute value is
// ever sealed), so it decodes without a key.
func (k keylessCtx) decodeNodeDict(dict *segDictHandle, node []byte) (ast.Expr, error) {
	if wire.IsEncryptedNode(node) {
		return &ast.UndefinedLiteral{}, nil
	}
	switch {
	case dict != nil:
		return wire.DecodeNodeResolveEnc(node, dict.resolve, nil)
	case k.c.inline:
		return wire.DecodeNodeInline(node)
	default:
		return wire.DecodeNode(node, k.c.intern)
	}
}

// evalCtx is the wire context an evaluation runs in: the collection itself (which holds the key) for an
// entitled read, keylessCtx for a redacted one. A collection that seals nothing (SealsValues) has
// nothing to open, so its redacted reads take the same context as its entitled ones and cost nothing
// extra.
//
// Every evaluating read path takes its context from here, with the same redact flag that picks the
// renderer for what it returns (renderKey), so what a reader can match on and what it is shown cannot
// disagree.
func (c *Collection) evalCtx(redact bool) wireCtx {
	if redact && c.SealsValues() {
		return keylessCtx{c}
	}
	return c
}

// wireScope resolves attribute references directly from an ad's wire bytes for
// wire-native query evaluation, so a match test builds no ClassAd. It handles the
// common case where the queried attributes are scalar literals; if it encounters
// an attribute whose value is a non-literal expression (or a list/record), it
// sets fellBack and the caller retries the ad with a full ClassAd decode.
//
// One wireScope is reused across a scan (single-threaded): set ad (and, for a
// chained child, parent) and clear fellBack before each evaluation.
type wireScope struct {
	ad       wire.Ad
	parent   wire.Ad // the chained parent ad's wire bytes, or nil (no parent)
	ctx      wireCtx // for mode-aware attribute lookup (interned id vs inline name)
	fellBack bool
	// dict / parentDict are the segment dictionaries of ad / parent when those come from an
	// INTERNED segment (records carry segment-local ids); nil => inline/global via ctx. ad and
	// its chained parent may live in different segments, hence two handles. Set per record by
	// the scan alongside ad/parent; the dict-aware touchpoints (wireLookupDictCtx etc.) resolve
	// through them so an interned segment filters/decodes correctly.
	dict       *segDictHandle
	parentDict *segDictHandle
}

// wireLookupDictCtx / decodeNodeDictCtx / decodeWireDictCtx are the dict-aware wire touchpoints
// used across the wire-native match path. When dict != nil the ad is interned with
// segment-local ids (resolve name->id via the dict, then Lookup(id); decode via the resolver);
// else they fall to the mode-aware ctx (inline/global) -- byte-for-byte the prior behavior.
func wireLookupDictCtx(ctx wireCtx, dict *segDictHandle, a wire.Ad, name string) ([]byte, bool) {
	if dict != nil {
		id, ok := dict.segIDOf(name)
		if !ok {
			return nil, false
		}
		return a.Lookup(uint32(id)) //idspace:translate a dictionary-keyed record is keyed by the segID
	}
	return ctx.wireLookup(a, name)
}

// decodeNodeDictCtx and decodeWireDictCtx open sealed values with the context's key whichever
// layout the record is in (see wireCtx).
func decodeNodeDictCtx(ctx wireCtx, dict *segDictHandle, node []byte) (ast.Expr, error) {
	return ctx.decodeNodeDict(dict, node)
}

func decodeWireDictCtx(ctx wireCtx, dict *segDictHandle, w []byte) (*ast.ClassAd, error) {
	return ctx.decodeWireDict(dict, w)
}

// resolve is the attribute resolver handed to vm.Matcher.EvalResolved.
func (ws *wireScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	switch scope {
	case ast.TargetScope:
		// A collection ad has no match target, so TARGET references are undefined.
		return classad.NewUndefinedValue()
	case ast.ParentScope:
		// PARENT.attr reads from the chained parent ad (undefined if none).
		if ws.parent == nil {
			return classad.NewUndefinedValue()
		}
		v, found := ws.tryResolve(ws.parent, ws.parentDict, name)
		if !found && classad.IsCurrentTimeName(name) {
			ws.fellBack = true // a parent without CurrentTime gives the time (see SetResolver)
		}
		return v
	case ast.RootScope:
		// .attr - the outermost ad of the chain, which here is the parent when
		// the ad is chained and the ad itself otherwise. No fallthrough: an
		// absolute reference names one ad.
		if ws.parent != nil {
			v, _ := ws.tryResolve(ws.parent, ws.parentDict, name)
			return v
		}
		v, _ := ws.tryResolve(ws.ad, ws.dict, name)
		return v
	default:
		// Unscoped: this ad, then fall through to its parent (chaining), matching
		// the ClassAd evaluator's parent walk.
		if v, found := ws.tryResolve(ws.ad, ws.dict, name); found {
			return ws.definedValue(name, v)
		}
		if ws.parent != nil {
			if v, found := ws.tryResolve(ws.parent, ws.parentDict, name); found {
				return ws.definedValue(name, v)
			}
		}
		return classad.NewUndefinedValue()
	}
}

// definedValue returns v, the value of an attribute the ad defines. The evaluator reads
// an undefined CurrentTime from the resolver as one the ad lacks, and substitutes the
// time, so a CurrentTime defined as undefined is left to the ordinary evaluator.
func (ws *wireScope) definedValue(name string, v classad.Value) classad.Value {
	if v.IsUndefined() && classad.IsCurrentTimeName(name) {
		ws.fellBack = true
	}
	return v
}

// tryResolve looks name up in ad. found reports whether ad has the attribute at
// all (so an unscoped resolve knows whether to fall through to the parent). A
// non-literal value sets fellBack (the caller retries with a full decode) and
// still counts as found -- the ad has the attribute, it just can't be read from
// wire alone.
func (ws *wireScope) tryResolve(ad wire.Ad, dict *segDictHandle, name string) (classad.Value, bool) {
	node, ok := wireLookupDictCtx(ws.ctx, dict, ad, name)
	if !ok {
		return classad.NewUndefinedValue(), false
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		ws.fellBack = true
		return classad.NewUndefinedValue(), true
	}
	return litToValue(lit), true
}

func litToValue(l wire.Literal) classad.Value {
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

// isTrueValue reports whether v is boolean true (a query match), matching
// vm.Query.Matches.
func isTrueValue(v classad.Value) bool {
	b, err := v.BoolValue()
	return err == nil && b
}
