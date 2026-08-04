package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireCtx supplies the mode-aware wire touchpoints (interned-id vs inline-name
// attribute lookup and decode) that wire-native matching needs. Both *Collection
// and *Archive implement it, so they share one match path (matchWire).
type wireCtx interface {
	decodeWire(w []byte) (*ast.ClassAd, error)
	wireLookup(a wire.Ad, name string) ([]byte, bool)
	decodeNode(node []byte) (ast.Expr, error)
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
		id, ok := dict.lookup(name)
		if !ok {
			return nil, false
		}
		return a.Lookup(id)
	}
	return ctx.wireLookup(a, name)
}

func decodeNodeDictCtx(ctx wireCtx, dict *segDictHandle, node []byte) (ast.Expr, error) {
	if dict != nil {
		return wire.DecodeNodeResolve(node, dict.resolve)
	}
	return ctx.decodeNode(node)
}

func decodeWireDictCtx(ctx wireCtx, dict *segDictHandle, w []byte) (*ast.ClassAd, error) {
	if dict != nil {
		return wire.DecodeResolve(w, dict.resolve)
	}
	return ctx.decodeWire(w)
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
		v, _ := ws.tryResolve(ws.parent, ws.parentDict, name)
		return v
	default:
		// Unscoped: this ad, then fall through to its parent (chaining), matching
		// the ClassAd evaluator's parent walk.
		if v, found := ws.tryResolve(ws.ad, ws.dict, name); found {
			return v
		}
		if ws.parent != nil {
			if v, found := ws.tryResolve(ws.parent, ws.parentDict, name); found {
				return v
			}
		}
		return classad.NewUndefinedValue()
	}
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
