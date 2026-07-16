package vm

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
)

// ReadPlan describes what a query reads from an ad, so a store can evaluate the
// query against a partially-decoded ad (only the needed attributes) instead of
// decoding every attribute of every ad.
type ReadPlan struct {
	// Seeds are the distinct attribute names the query reads directly from the
	// current ad — unscoped and MY-scoped references (TARGET references read the
	// match target, which is absent during a collection scan). Resolving these
	// may pull in further attributes they reference; the store expands the set
	// transitively while decoding.
	Seeds []string

	// PartialSafe is true when the query contains no construct that reads an
	// attribute whose name is not statically visible — specifically eval(), which
	// parses a runtime string into an arbitrary expression. When false, a partial
	// decode could miss an attribute, so the store must fully decode the ad.
	PartialSafe bool
}

// ReadPlan computes the query's read plan from its source expression.
func (q *Query) ReadPlan() ReadPlan {
	seen := map[string]bool{}
	rp := ReadPlan{PartialSafe: true}
	walkPlan(q.prog.expr, seen, &rp)
	return rp
}

// walkPlan collects self-scoped attribute names and clears PartialSafe if it
// encounters an eval() call.
func walkPlan(expr ast.Expr, seen map[string]bool, rp *ReadPlan) {
	switch v := expr.(type) {
	case nil:
		return
	case *ast.AttributeReference:
		if v.Scope == ast.NoScope || v.Scope == ast.MyScope {
			if !seen[v.Name] {
				seen[v.Name] = true
				rp.Seeds = append(rp.Seeds, v.Name)
			}
		}
	case *ast.ParenExpr:
		walkPlan(v.Inner, seen, rp)
	case *ast.UnaryOp:
		walkPlan(v.Expr, seen, rp)
	case *ast.BinaryOp:
		walkPlan(v.Left, seen, rp)
		walkPlan(v.Right, seen, rp)
	case *ast.ElvisExpr:
		walkPlan(v.Left, seen, rp)
		walkPlan(v.Right, seen, rp)
	case *ast.ConditionalExpr:
		walkPlan(v.Condition, seen, rp)
		walkPlan(v.TrueExpr, seen, rp)
		walkPlan(v.FalseExpr, seen, rp)
	case *ast.ListLiteral:
		for _, el := range v.Elements {
			walkPlan(el, seen, rp)
		}
	case *ast.FunctionCall:
		if strings.EqualFold(v.Name, "eval") {
			rp.PartialSafe = false
		}
		for _, a := range v.Args {
			walkPlan(a, seen, rp)
		}
	case *ast.SelectExpr:
		walkPlan(v.Record, seen, rp)
	case *ast.SubscriptExpr:
		walkPlan(v.Container, seen, rp)
		walkPlan(v.Index, seen, rp)
	case *ast.RecordLiteral:
		if v.ClassAd != nil {
			for _, attr := range v.ClassAd.Attributes {
				walkPlan(attr.Value, seen, rp)
			}
		}
	}
}

// SelfRefs returns the self-scoped attribute names referenced directly by expr
// (unscoped or MY-scoped), for the store to expand the transitive read set as it
// decodes attribute expressions. It does not recurse into nested records.
func SelfRefs(expr ast.Expr) []string {
	seen := map[string]bool{}
	rp := ReadPlan{PartialSafe: true}
	walkPlan(expr, seen, &rp)
	return rp.Seeds
}

// SelfRefsSafe is SelfRefs plus whether partial decode is sound for expr:
// partialSafe is false when expr calls eval(), whose referenced attributes cannot be
// determined statically, so a closure built from the static refs could miss one. A
// caller building a partial ad must fall back to a full decode when this is false.
func SelfRefsSafe(expr ast.Expr) (refs []string, partialSafe bool) {
	seen := map[string]bool{}
	rp := ReadPlan{PartialSafe: true}
	walkPlan(expr, seen, &rp)
	return rp.Seeds, rp.PartialSafe
}
