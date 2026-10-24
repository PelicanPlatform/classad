package collections

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"strings"
	"testing"
)

// planConstructorFile is the one file allowed to build a queryPlan that carries a
// query. Everything else asks newQueryPlan for one.
const planConstructorFile = "scanplan.go"

// TestQueryPlanHasOneConstructor fails if any file but scanplan.go writes a
// queryPlan literal with a query in it.
//
// It is not a test that a particular scan path remembered a particular field --
// three of them forgot the same one in turn, and a fourth such test would only have
// waited for a fifth path. It is the rule that made those omissions possible to
// write at all: a scan path that hand-assembles its plan decides for itself what
// evaluating a query requires, and decides it from whichever sibling it was copied
// from. There is now one answer to that question and this keeps it the only one.
//
// Plans WITHOUT a query stay free-form: the zero value is the unfiltered scan, and
// a path that filters nothing has nothing to get wrong.
func TestQueryPlanHasOneConstructor(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi fs.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			base := name[strings.LastIndexByte(name, '/')+1:]
			if base == planConstructorFile {
				continue
			}
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				id, ok := lit.Type.(*ast.Ident)
				if !ok || id.Name != "queryPlan" {
					return true
				}
				for _, elt := range lit.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "q" {
						t.Errorf("%s: queryPlan literal sets q; a plan that carries a query "+
							"comes from newQueryPlan, which is what gives every scan path the "+
							"same probes, index demand and zone pruning", fset.Position(kv.Pos()))
					}
				}
				return true
			})
		}
	}
}
