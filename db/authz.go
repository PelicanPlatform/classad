package db

import (
	"reflect"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Attribute references in an expression, for AUTHORIZATION rather than planning.
//
// vm's Program.ReadAttrs already collects attribute names, and the private-attribute gate on
// per-aggregate FILTERs used it -- but it deliberately collects only UNSCOPED references
// (Scope == ast.NoScope), because that is what the query planner needs. `MY.ClaimId == "x"` therefore
// reports NO reads, so a gate built on it lets a scoped reference to a secret straight through. Verified:
// ReadAttrs("MY.ClaimId == \"x\"") is empty while the constraint matches the ad that has that ClaimId.
//
// So authorization gets its own traversal, with two properties the planner's does not need:
//
//   - It collects every reference regardless of scope.
//   - It walks by REFLECTION over the AST rather than a type switch per node kind. A type switch that
//     misses a node kind is a silent authorization hole, and the AST has 19 node types with more
//     possible later; reflection covers a new one the day it is added. This runs once per query, never
//     per record, so the cost does not matter.

// ConstraintRefs reports every attribute name an expression references, scoped or not, and whether the
// expression makes a reference this analysis cannot resolve statically (see dynamic below).
//
// A constraint that does not parse returns no references and dynamic=false: it is rejected downstream
// where the parse error can be reported properly, and refusing it here would report the wrong reason.
func ConstraintRefs(expr string) (refs []string, dynamic bool) {
	if strings.TrimSpace(expr) == "" {
		return nil, false
	}
	// vm.Parse rather than classad.ParseExpr: classad.Expr wraps the AST in an unexported field, so a
	// reflective walk cannot see through it (it silently found NOTHING, in every case, until this was
	// caught). vm.Query.Expr() hands back the ast.Expr itself.
	q, err := vm.Parse(expr)
	if err != nil {
		return nil, false
	}
	e := q.Expr()
	if e == nil {
		return nil, false
	}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			refs = append(refs, name)
		}
	}
	walkExpr(reflect.ValueOf(e), add, &dynamic, 0)
	return refs, dynamic
}

// maxWalkDepth bounds the traversal. A parsed expression is already depth-bounded by the parser, so
// this only guards against a cyclic AST -- which would otherwise hang an authorization check.
const maxWalkDepth = 512

// walkExpr collects attribute references from any AST value reachable from v.
//
// dynamic is set when the expression can name an attribute at RUNTIME, which no static walk can see:
// eval() of anything but a constant string. eval("ClaimId") is resolved here (the constant is checked as
// if it were written as a reference); eval(SomeAttr) is not resolvable and marks the expression dynamic
// so the caller can refuse it rather than pass a reference it cannot see.
func walkExpr(v reflect.Value, add func(string), dynamic *bool, depth int) {
	if depth > maxWalkDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Interface, reflect.Pointer:
		if v.IsNil() {
			return
		}
		if v.CanInterface() {
			switch n := v.Interface().(type) {
			case *ast.AttributeReference:
				// Every reference, whatever its scope: MY.X, TARGET.X and X all name X.
				add(n.Name)
			case *ast.FunctionCall:
				if strings.EqualFold(n.Name, "eval") {
					if len(n.Args) == 1 {
						if lit, ok := n.Args[0].(*ast.StringLiteral); ok {
							// eval("Attr") is a reference to Attr, written indirectly.
							add(lit.Value)
						} else {
							*dynamic = true
						}
					} else {
						*dynamic = true
					}
				}
			}
		}
		walkExpr(v.Elem(), add, dynamic, depth+1)
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Field(i)
			// Unexported fields cannot be read through reflection; the AST's expression-bearing
			// fields are all exported, and skipping the rest keeps this from panicking.
			if f.CanInterface() {
				walkExpr(f, add, dynamic, depth+1)
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			walkExpr(v.Index(i), add, dynamic, depth+1)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			walkExpr(v.MapIndex(k), add, dynamic, depth+1)
		}
	}
}

// PrivateConstraintRef reports the first private attribute an expression references, or "" if it
// references none. dynamic reports that the expression names attributes at runtime, which the caller
// must treat as unauthorized on an unprivileged connection: a reference it cannot see is one it cannot
// check.
//
// What this CANNOT see, and what closes it: an ad may store a public attribute whose value is an
// expression over a private one (`Leak = ClaimId`). A constraint on `Leak` references nothing private
// and so passes here, while evaluation resolves ClaimId. Closing that needs the private value to be
// absent at EVALUATION time for an unprivileged reader, not a check on the constraint text -- the
// redacted decode walk already does this for rendering.
func PrivateConstraintRef(expr string) (attr string, dynamic bool) {
	refs, dyn := ConstraintRefs(expr)
	for _, r := range refs {
		if classad.IsPrivateAttribute(r) {
			return r, dyn
		}
	}
	return "", dyn
}
