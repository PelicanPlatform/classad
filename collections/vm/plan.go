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
	// match target, which is absent during a collection scan), plus the member of
	// every `.`-path select, which resolves against this ad whenever the selected
	// record does not define it. Resolving these may pull in further attributes they
	// reference; the store expands the set transitively while decoding.
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
			addSeed(v.Name, seen, rp)
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
		// `rec.Attr` does not stop at rec. A member missing from the selected record
		// CHAINS to the enclosing scope -- this ad -- so the select reads this ad's
		// Attr as well, and a partial decode that omitted it would answer the query
		// against an ad that lacks an attribute the whole ad has. That is a WRONG
		// ANSWER, not a slow one: `Res.Mem >= 4096` over [Mem=4096; Res=[Other=1]]
		// is TRUE to the evaluator (and to the C++ reference engine), and a scan
		// seeded with Res alone drops the record.
		//
		// The consequence is that every select widens the read set by one name,
		// whether or not the record turns out to define the member. That is the
		// faithful cost of the scoping rule: which way a record goes is a question
		// about the record, and the read set is planned before any record is read.
		// Measured on a 20k-record scan of 30-attribute ads, per record: about two
		// extra allocations when the ad does not define the name (the worklist entry
		// and its folded form, then a miss in the wire index) and about five when it
		// does and the attribute is decoded. A query with no select is untouched.
		addSeed(v.Attr, seen, rp)
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

// addSeed records name as an attribute the expression reads from the current ad,
// once.
func addSeed(name string, seen map[string]bool, rp *ReadPlan) {
	if seen[name] {
		return
	}
	seen[name] = true
	rp.Seeds = append(rp.Seeds, name)
}

// SelfRefs returns the self-scoped attribute names expr reads from the ad it is
// evaluated against — unscoped or MY-scoped references, and the member of every
// `.`-path select — for the store to expand the transitive read set as it decodes
// attribute expressions. A nested record literal's members are walked too: an
// unscoped name inside one resolves against the enclosing ad when the record does
// not bind it.
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
