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
		v, _ := ws.tryResolve(ws.parent, name)
		return v
	default:
		// Unscoped: this ad, then fall through to its parent (chaining), matching
		// the ClassAd evaluator's parent walk.
		if v, found := ws.tryResolve(ws.ad, name); found {
			return v
		}
		if ws.parent != nil {
			if v, found := ws.tryResolve(ws.parent, name); found {
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
func (ws *wireScope) tryResolve(ad wire.Ad, name string) (classad.Value, bool) {
	node, ok := ws.ctx.wireLookup(ad, name)
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
