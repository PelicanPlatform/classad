package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// wireScope resolves attribute references directly from an ad's wire bytes for
// wire-native query evaluation, so a match test builds no ClassAd. It handles the
// common case where the queried attributes are scalar literals; if it encounters
// an attribute whose value is a non-literal expression (or a list/record), it
// sets fellBack and the caller retries the ad with a full ClassAd decode.
//
// One wireScope is reused across a scan (single-threaded): set ad and clear
// fellBack before each evaluation.
type wireScope struct {
	ad       wire.Ad
	c        *Collection // for mode-aware attribute lookup (interned id vs inline name)
	fellBack bool
}

// resolve is the attribute resolver handed to vm.Matcher.EvalResolved.
func (ws *wireScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	// A collection ad has no match target or enclosing parent, so TARGET/PARENT
	// references are undefined — exactly as they evaluate against a stored ad.
	if scope == ast.TargetScope || scope == ast.ParentScope {
		return classad.NewUndefinedValue()
	}
	node, ok := ws.c.wireLookup(ws.ad, name)
	if !ok {
		return classad.NewUndefinedValue() // this ad lacks it
	}
	lit, ok := wire.LiteralValue(node)
	if !ok {
		// Non-literal (expression/list/record) attribute: cannot resolve without
		// evaluating in a real scope. Flag fallback; the returned value is
		// discarded once the caller sees fellBack.
		ws.fellBack = true
		return classad.NewUndefinedValue()
	}
	return litToValue(lit)
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
