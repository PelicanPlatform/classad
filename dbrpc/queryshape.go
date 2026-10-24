package dbrpc

import (
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Sentinel shapes for the constraints that have no expression structure to render.
const (
	shapeMatchAll  = "*"          // "" or "true": no WHERE at all
	shapeUnparsed  = "<unparsed>" // the server could not parse it (it refused the query)
	shapeTruncated = "…"          // marks a fingerprint cut at shapeMaxLen
)

// shapeMaxLen bounds one fingerprint. A pathological constraint (a thousand-term OR
// pasted by a tool) would otherwise sit in the shape map at full length, and the map
// holds shapeMaxShapes of them. Two constraints that agree for this many characters
// land in one bucket, which is the right trade: their exemplars still carry the full
// text, so nothing needed to reproduce the query is lost.
const shapeMaxLen = 512

// shapeMaxDepth bounds recursion into a deeply nested expression. Below it the
// subtree renders as a single opaque token rather than being walked.
const shapeMaxDepth = 24

// queryShape renders a constraint as a canonical fingerprint: the expression with every
// literal replaced by "?" and every attribute name lowercased, so queries that differ
// only in their values or their spelling share one fingerprint.
//
// It works on the parsed, constant-folded expression rather than on the constraint text
// because the text is not a reliable identity for a query here. Three things make it
// unreliable: the engine constant-folds before planning, so `Memory > 2048*1024` and
// `Memory > 2097152` are the same query; ClassAd attribute names are case-insensitive,
// so `owner ==` and `Owner ==` are the same attribute; and whitespace, parenthesization
// and quoting vary with whichever client composed the string.
//
// Canonicalization beyond "drop the literals":
//
//   - An && or || spine is flattened, its operands are rendered, sorted and
//     de-duplicated. Sorting makes operand order irrelevant (`A>1 && B==2` ==
//     `B==2 && A>1`); de-duplication collapses a variable-length membership test, so
//     `Owner=="a"||Owner=="b"||Owner=="c"` and `Owner=="a"||Owner=="b"` are ONE shape
//     rather than one shape per list length.
//   - A list literal is de-duplicated the same way, so `member(Owner,{"a","b","c"})`
//     and `member(Owner,{"a"})` also agree.
//   - A comparison with the literal on the left is flipped to put the attribute first,
//     so `500 < Memory` and `Memory > 500` agree.
//
// A constraint the parser rejects yields shapeUnparsed: the server refused that query, and
// bucketing every malformed constraint together is what an operator wants to see.
func queryShape(constraint string) string {
	s := strings.TrimSpace(constraint)
	if s == "" || strings.EqualFold(s, "true") {
		return shapeMatchAll
	}
	e, err := parser.ParseExpr(s)
	if err != nil {
		return shapeUnparsed
	}
	out := shapeExpr(classad.FoldConstants(e), 0)
	if len(out) > shapeMaxLen {
		out = out[:shapeMaxLen] + shapeTruncated
	}
	return out
}

// shapeExpr renders one node of the folded AST.
func shapeExpr(e ast.Expr, depth int) string {
	if e == nil {
		return "?"
	}
	if depth > shapeMaxDepth {
		return "<deep>"
	}
	switch n := e.(type) {
	case *ast.ParenExpr:
		// Transparent: parenthesization is a property of how the constraint was written,
		// not of what it asks for. The spine flattening below re-groups anyway.
		return shapeExpr(n.Inner, depth)

	case *ast.IntegerLiteral, *ast.RealLiteral, *ast.StringLiteral,
		*ast.BooleanLiteral, *ast.UndefinedLiteral, *ast.ErrorLiteral:
		return "?"

	case *ast.AttributeReference:
		return scopePrefix(n.Scope) + n.NormalizedName()

	case *ast.BinaryOp:
		if n.Op == "&&" || n.Op == "||" {
			return shapeSpine(n, depth)
		}
		l, r := shapeExpr(n.Left, depth+1), shapeExpr(n.Right, depth+1)
		op := n.Op
		if l == "?" && r != "?" && isCompare(op) {
			// Put the attribute side first so `500 < Memory` and `Memory > 500` agree.
			// Comparisons only: swapping the operands of `5 - Memory` would name a
			// different expression.
			l, r, op = r, l, mirrorCompare(op)
		}
		return "(" + l + " " + op + " " + r + ")"

	case *ast.UnaryOp:
		return "(" + n.Op + shapeExpr(n.Expr, depth+1) + ")"

	case *ast.ListLiteral:
		elems := make([]string, 0, len(n.Elements))
		for _, el := range n.Elements {
			elems = append(elems, shapeExpr(el, depth+1))
		}
		return "{" + strings.Join(sortUnique(elems), ",") + "}"

	case *ast.FunctionCall:
		args := make([]string, 0, len(n.Args))
		for _, a := range n.Args {
			args = append(args, shapeExpr(a, depth+1))
		}
		// Argument order is meaningful, so these are NOT sorted.
		return strings.ToLower(n.Name) + "(" + strings.Join(args, ",") + ")"

	case *ast.ConditionalExpr:
		return "(" + shapeExpr(n.Condition, depth+1) + "?" +
			shapeExpr(n.TrueExpr, depth+1) + ":" + shapeExpr(n.FalseExpr, depth+1) + ")"

	case *ast.ElvisExpr:
		return "(" + shapeExpr(n.Left, depth+1) + "?:" + shapeExpr(n.Right, depth+1) + ")"

	case *ast.SelectExpr:
		return shapeExpr(n.Record, depth+1) + "." + strings.ToLower(n.Attr)

	case *ast.SubscriptExpr:
		return shapeExpr(n.Container, depth+1) + "[" + shapeExpr(n.Index, depth+1) + "]"

	case *ast.RecordLiteral:
		return shapeClassAd(n.ClassAd, depth)

	case *ast.ClassAd:
		return shapeClassAd(n, depth)

	default:
		// A node kind added after this walk was written: render it opaquely rather than
		// silently folding it into a neighbouring shape.
		return "<expr>"
	}
}

// shapeClassAd renders a nested ad literal with its attribute names lowercased and
// sorted (attribute order within an ad carries no meaning).
func shapeClassAd(c *ast.ClassAd, depth int) string {
	if c == nil {
		return "[]"
	}
	parts := make([]string, 0, len(c.Attributes))
	for _, a := range c.Attributes {
		parts = append(parts, strings.ToLower(a.Name)+"="+shapeExpr(a.Value, depth+1))
	}
	sort.Strings(parts)
	return "[" + strings.Join(parts, ";") + "]"
}

// shapeSpine flattens a run of one boolean operator, renders each operand, then sorts and
// de-duplicates them: operand order stops mattering and a variable-length OR-of-equalities
// collapses to a single term.
func shapeSpine(n *ast.BinaryOp, depth int) string {
	var operands []string
	var walk func(ast.Expr)
	walk = func(e ast.Expr) {
		if p, ok := e.(*ast.ParenExpr); ok {
			walk(p.Inner)
			return
		}
		if b, ok := e.(*ast.BinaryOp); ok && b.Op == n.Op {
			walk(b.Left)
			walk(b.Right)
			return
		}
		operands = append(operands, shapeExpr(e, depth+1))
	}
	walk(n)
	return "(" + strings.Join(sortUnique(operands), " "+n.Op+" ") + ")"
}

// sortUnique sorts ss and removes duplicates, in place.
func sortUnique(ss []string) []string {
	sort.Strings(ss)
	out := ss[:0]
	for i, s := range ss {
		if i == 0 || s != ss[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// isCompare reports whether op is a comparison, i.e. one whose operands may be
// swapped (with mirrorCompare) without changing what it asks.
func isCompare(op string) bool {
	switch op {
	case "==", "!=", "<", "<=", ">", ">=", "=?=", "=!=":
		return true
	}
	return false
}

// mirrorCompare returns the comparison that means the same thing with the operands
// swapped. A symmetric comparison is returned unchanged.
func mirrorCompare(op string) string {
	switch op {
	case "<":
		return ">"
	case ">":
		return "<"
	case "<=":
		return ">="
	case ">=":
		return "<="
	default: // ==, !=, =?=, =!= are symmetric
		return op
	}
}

// scopePrefix renders an attribute reference's explicit scope. MY.Foo and Foo are
// different references to the engine, so the prefix is part of the shape.
func scopePrefix(s ast.AttributeScope) string {
	switch s {
	case ast.MyScope:
		return "my."
	case ast.TargetScope:
		return "target."
	case ast.ParentScope:
		return "parent."
	default:
		return ""
	}
}
