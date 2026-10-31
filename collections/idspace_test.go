package collections

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The attribute id types (ids.go) are only as good as the places that may convert between them. Go
// lets any conversion between integer types compile, lets a slice be indexed by any integer type, and
// lets an untyped constant become any of them silently, so the compiler alone does not stop
// storedID(g) or fields[g]. This test does: it type-checks the package and fails on
//
//   - a conversion into or out of an id type outside the functions allowed to translate;
//   - a slice or array indexed by an id type (an id is a name, not a position);
//   - a constant of an id type other than in an allowed function.
//
// The allowed functions are the translation points, listed with the reason each may convert.
var idspaceTranslators = map[string]string{
	// Where an id is born from bytes or leaves to them.
	"segDictLookup":                   "a dictionary id read off the mapping",
	"segDictSearch":                   "a dictionary id read off the mapping",
	"(*segDictHandle).name":           "the format layer takes the raw u32",
	"(*segDictHandle).resolve":        "wire's resolver signature is a bare uint32 record key",
	"(*segDictHandle).buildNameCache": "enumerates the dictionary's ids",
	"unmarshalColSegment":             "the stored id a globally-keyed payload's section names, and its re-interned global id",

	"(*columnarBlock).escapedNode":     "a cold-tail key decoded from its uvarint",
	"(*columnarBlock).escapedNumCell":  "a cold-tail key decoded from its uvarint",
	"(*Collection).projectFromColumns": "a cold-tail key decoded from its uvarint, re-emitted as the record key it is",
	// Between spaces.

	"(*idRemap).storedID": "a nil remap: the payload was written in this process's space",
	"(*idRemap).current":  "a nil remap: the payload was written in this process's space",
	"segIDRemap":          "a dictionary-keyed payload is written in its segment's dictionary ids",
	"newColBuild":         "a dictionary-keyed payload is written in its segment's dictionary ids",
	"appendColdEntry":     "writes the cold-tail key",
}

// idspaceBoundary lists the conversions that exist only because the surrounding code is not typed
// yet (an adField.id or a record key still a bare uint32). Each disappears when its source is typed.
var idspaceBoundary = map[string]bool{
	"(*Collection).adClosed":                true,
	"(*Collection).columnarizeSegment":      true,
	"(*Collection).globalIDOf":              true,
	"(*Collection).recordHasUnsealedValue":  true,
	"(*Collection).scanNumValues":           true,
	"(*Collection).schemaScanPresenceCount": true,
	"(*Collection).wireLookupDict":          true,
	"(*blockVecSource).fillGroupExceptions": true,
	"(*blockVecSource).fixEscapes":          true,
	"(*blockVecSource).loadGroupColumn":     true,
	"(*colNative).appendGroupValues":        true,
	"(*colNative).spliceInto":               true,
	"(*colScope).fromColdTail":              true,
	"(*colScope).groupAbsentAt":             true,
	"(*colScope).resolveGroup":              true,
	"(*colSegment).scanHeld":                true,
	"(*columnarBlock).heldValueNode":        true,
	"(*indexSpec).attrNode":                 true,
	"(*numCol).cell":                        true,
	"(*projPlan).localWanted":               true,
	"(*projectScratch).coldWanted":          true,
	"(*wireNarrower).localWanted":           true,
	"(*wireRowAssembler).markSealed":        true,
	"computeSegZones":                       true,
	"escapedGroupComp":                      true,
	"fixEscapedPlane":                       true,
	"groupColdToField":                      true,
	"namesCurrentTimeIn":                    true,
	"narrowByRecord":                        true,
	"narrowByScan":                          true,
	"publishColRecord":                      true,
	"wireLookupDictCtx":                     true,
}

func TestIDSpaceConversions(t *testing.T) {
	fset := token.NewFileSet()
	var files []*ast.File
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		n := e.Name()
		if !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		if ok, _ := build.Default.MatchFile(".", n); !ok {
			continue
		}
		f, perr := parser.ParseFile(fset, filepath.Join(".", n), nil, 0)
		if perr != nil {
			t.Fatal(perr)
		}
		files = append(files, f)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "source", nil)}
	pkg, err := conf.Check("github.com/PelicanPlatform/classad/collections", fset, files, info)
	if err != nil {
		t.Fatal(err)
	}
	idTypes := map[types.Type]bool{}
	for _, n := range []string{"globalID", "segID", "storedID"} {
		idTypes[pkg.Scope().Lookup(n).Type()] = true
	}
	isID := func(t types.Type) bool { return t != nil && idTypes[t] }

	var bad []string
	report := func(fn string, pos token.Pos, what string) {
		if _, ok := idspaceTranslators[fn]; ok {
			return
		}
		if idspaceBoundary[fn] {
			return
		}
		bad = append(bad, fmt.Sprintf("%s: %s in %s", fset.Position(pos), what, fn))
	}
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			fn := funcKey(fd)
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				switch x := n.(type) {
				case *ast.CallExpr:
					tv, ok := info.Types[x.Fun]
					if !ok || !tv.IsType() || len(x.Args) != 1 {
						return true
					}
					to, from := tv.Type, info.Types[x.Args[0]].Type
					if (isID(to) || isID(from)) && !types.Identical(to, from) {
						report(fn, x.Pos(), fmt.Sprintf("conversion %s(%s)", types.TypeString(to, types.RelativeTo(pkg)), types.TypeString(from, types.RelativeTo(pkg))))
					}
				case *ast.IndexExpr:
					st := info.Types[x.X].Type
					if st == nil {
						return true
					}
					switch st.Underlying().(type) {
					case *types.Slice, *types.Array:
						if isID(info.Types[x.Index].Type) {
							report(fn, x.Pos(), "slice indexed by an id")
						}
					}
				case *ast.BasicLit:
					// Zero is allowed: it is the value returned beside ok=false on a miss.
					if tv := info.Types[x]; isID(tv.Type) && x.Value != "0" {
						report(fn, x.Pos(), "constant id")
					}
				}
				return true
			})
		}
	}
	sort.Strings(bad)
	for _, b := range bad {
		t.Error(b)
	}
}

func funcKey(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	rt := fd.Recv.List[0].Type
	star := ""
	if s, ok := rt.(*ast.StarExpr); ok {
		star, rt = "*", s.X
	}
	if ix, ok := rt.(*ast.IndexExpr); ok {
		rt = ix.X
	}
	return fmt.Sprintf("(%s%s).%s", star, rt.(*ast.Ident).Name, fd.Name.Name)
}
