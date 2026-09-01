//go:build libclassad

package differ

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/fuzz/canon"
	"github.com/PelicanPlatform/classad/parser"
)

// explainedByListIsQuirk reports whether every top-level attribute on which the
// two engines disagree is accounted for by CPP_QUIRKS #9: `=?=`/`=!=` between a
// list literal (ExprList) and a function-produced list (SList) is `false`/`true`
// in libclassad but `error` in the Go engine (which has a single list type).
//
// The value signature alone is decisive: a `list =?= list` is `error` in
// libclassad for two lists of the SAME internal kind, so the ONLY way it yields
// a boolean is the cross-kind case -- which the Go engine consistently reports
// as error. So a divergence of the form (go=error, cpp=bool) whose attribute
// expression contains a list-operand `is`/`isnt` cannot be anything but this
// quirk, and downgrading it does not mask a real Go bug.
func explainedByListIsQuirk(src string, goVal, cppVal canon.Value) bool {
	if goVal.Kind != canon.KClassad || cppVal.Kind != canon.KClassad {
		return false
	}
	node, err := parser.Parse(src)
	if err != nil {
		return false
	}
	adNode, ok := node.(*ast.ClassAd)
	if !ok {
		return false
	}
	exprs := map[string]ast.Expr{}
	for _, a := range adNode.Attributes {
		exprs[strings.ToLower(a.Name)] = a.Value
	}

	cpp := map[string]canon.Value{}
	for _, kv := range cppVal.Map {
		cpp[kv.Key] = kv.Val
	}
	go_ := map[string]canon.Value{}
	for _, kv := range goVal.Map {
		go_[kv.Key] = kv.Val
	}

	// Gather the set of keys and require at least one genuine difference.
	sawDiff := false
	for k := range unionKeys(go_, cpp) {
		gv, gok := go_[k]
		cv, cok := cpp[k]
		if gok && cok && canon.Equal(gv, cv, canon.DefaultTolerance) {
			continue // this attribute matches
		}
		sawDiff = true
		// Every differing attribute must fit the quirk: go=error, cpp=bool, and
		// an `is`/`isnt` on two list-producing operands somewhere in its expr.
		if !gok || !cok || gv.Kind != canon.KError || cv.Kind != canon.KBool {
			return false
		}
		e, ok := exprs[strings.ToLower(k)]
		if !ok || !containsListIsIsnt(e, exprs) {
			return false
		}
	}
	return sawDiff
}

// explainedByCyclicRefQuirk reports whether every top-level attribute on which the two engines
// disagree is accounted for by CPP_QUIRKS #2/#3: a cyclic self-reference evaluates to `undefined`
// in libclassad but `error` in the Go engine (which detects the cycle). Historically libclassad
// infinite-looped on some of these (reported upstream); on a libclassad that still hangs the differ
// times out and reports CppTimeout instead, so both are non-divergences -- this recognizer covers
// the fixed libclassad that now terminates with `undefined`.
//
// The signature is decisive and cannot mask a real Go bug: the differing attribute is `error` in Go
// and `undefined` in C++, AND its value genuinely depends on itself (a reference cycle back to the
// attribute through same-scope references). Returning `error` on a detected cycle is the Go engine's
// deliberate, safe choice; the divergence is purely libclassad's undefined-vs-error spelling.
func explainedByCyclicRefQuirk(src string, goVal, cppVal canon.Value) bool {
	if goVal.Kind != canon.KClassad || cppVal.Kind != canon.KClassad {
		return false
	}
	node, err := parser.Parse(src)
	if err != nil {
		return false
	}
	adNode, ok := node.(*ast.ClassAd)
	if !ok {
		return false
	}
	attrs := map[string]ast.Expr{}
	for _, a := range adNode.Attributes {
		attrs[strings.ToLower(a.Name)] = a.Value
	}

	go_ := map[string]canon.Value{}
	for _, kv := range goVal.Map {
		go_[kv.Key] = kv.Val
	}
	cpp := map[string]canon.Value{}
	for _, kv := range cppVal.Map {
		cpp[kv.Key] = kv.Val
	}

	sawDiff := false
	for k := range unionKeys(go_, cpp) {
		gv, gok := go_[k]
		cv, cok := cpp[k]
		if gok && cok && canon.Equal(gv, cv, canon.DefaultTolerance) {
			continue // this attribute matches
		}
		sawDiff = true
		// Every differing attribute must fit the quirk: go=error, cpp=undefined, and a genuine
		// reference cycle back to the attribute.
		if !gok || !cok || gv.Kind != canon.KError || cv.Kind != canon.KUndef {
			return false
		}
		if !isCyclicSelfRef(k, attrs) {
			return false
		}
	}
	return sawDiff
}

// isCyclicSelfRef reports whether attribute name's value depends on itself: name is reachable from
// its own expression by following same-scope (unscoped) attribute references through the ad's other
// attributes. The seen set bounds the walk and prevents infinite recursion on the very cycles it
// detects.
func isCyclicSelfRef(name string, attrs map[string]ast.Expr) bool {
	start, ok := attrs[strings.ToLower(name)]
	if !ok {
		return false
	}
	target := strings.ToLower(name)
	seen := map[string]bool{}
	var reaches func(e ast.Expr) bool
	reaches = func(e ast.Expr) bool {
		found := false
		walkExpr(e, func(n ast.Expr) {
			if found {
				return
			}
			ref, ok := n.(*ast.AttributeReference)
			if !ok || ref.Scope != ast.NoScope {
				return
			}
			rn := strings.ToLower(ref.Name)
			if rn == target {
				found = true
				return
			}
			if !seen[rn] {
				seen[rn] = true
				if inner, ok := attrs[rn]; ok && reaches(inner) {
					found = true
				}
			}
		})
		return found
	}
	return reaches(start)
}

func unionKeys(a, b map[string]canon.Value) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}

// containsListIsIsnt reports whether e contains an `is`/`isnt` operation both of
// whose operands are list-producing expressions.
func containsListIsIsnt(e ast.Expr, attrs map[string]ast.Expr) bool {
	found := false
	walkExpr(e, func(n ast.Expr) {
		if b, ok := n.(*ast.BinaryOp); ok && (b.Op == "is" || b.Op == "isnt") {
			if listProducing(b.Left, attrs, 4) && listProducing(b.Right, attrs, 4) {
				found = true
			}
		}
	})
	return found
}

// listProducing reports whether e (syntactically) yields a list: a list literal,
// a list-returning builtin, a parenthesized such expression, or a same-ad
// reference (bounded by depth) to an attribute whose value is list-producing.
func listProducing(e ast.Expr, attrs map[string]ast.Expr, depth int) bool {
	switch v := e.(type) {
	case *ast.ListLiteral:
		return true
	case *ast.ParenExpr:
		return listProducing(v.Inner, attrs, depth)
	case *ast.FunctionCall:
		switch strings.ToLower(v.Name) {
		case "split", "splitslotname", "splitusername":
			return true
		}
	case *ast.AttributeReference:
		if depth > 0 && v.Scope == ast.NoScope {
			if inner, ok := attrs[strings.ToLower(v.Name)]; ok {
				return listProducing(inner, attrs, depth-1)
			}
		}
	}
	return false
}

// walkExpr calls visit on e and every descendant expression.
func walkExpr(e ast.Expr, visit func(ast.Expr)) {
	if e == nil {
		return
	}
	visit(e)
	switch v := e.(type) {
	case *ast.BinaryOp:
		walkExpr(v.Left, visit)
		walkExpr(v.Right, visit)
	case *ast.UnaryOp:
		walkExpr(v.Expr, visit)
	case *ast.ParenExpr:
		walkExpr(v.Inner, visit)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			walkExpr(el, visit)
		}
	case *ast.FunctionCall:
		for _, a := range v.Args {
			walkExpr(a, visit)
		}
	case *ast.ConditionalExpr:
		walkExpr(v.Condition, visit)
		walkExpr(v.TrueExpr, visit)
		walkExpr(v.FalseExpr, visit)
	case *ast.ElvisExpr:
		walkExpr(v.Left, visit)
		walkExpr(v.Right, visit)
	case *ast.SelectExpr:
		walkExpr(v.Record, visit)
	case *ast.SubscriptExpr:
		walkExpr(v.Container, visit)
		walkExpr(v.Index, visit)
	case *ast.RecordLiteral:
		if v.ClassAd != nil {
			for _, a := range v.ClassAd.Attributes {
				walkExpr(a.Value, visit)
			}
		}
	}
}
