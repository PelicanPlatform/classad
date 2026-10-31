package collections

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"io"
	"os"
	"os/exec"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// idspaceTypes are the attribute id types (ids.go), written as values so that renaming one breaks
// this list at compile time instead of silently dropping it from the check.
var idspaceTypes = []any{globalID(0), segID(0), storedID(0)}

const idspaceDirective = "//idspace:translate"

// TestIDSpaceConversions holds the attribute id types (ids.go) to their purpose. Go compiles any
// conversion between integer types, lets a slice be indexed by any integer type, and lets an untyped
// constant become any of them, so the types alone do not stop storedID(g) or fields[g]. This test
// type-checks the package's non-test files and fails on
//
//   - a conversion into or out of an id type, or a type parameter an id type can instantiate, on a
//     line without an //idspace:translate directive (at the end of the line, or alone on the line
//     above it);
//   - a slice, array or string indexed or sliced by an id type: an id is a name, not a position;
//   - a nonzero constant of an id type (zero is what a lookup returns beside ok=false);
//   - a second conversion on a directive's line: a directive excuses one conversion;
//   - a directive with no reason, a misspelled one, or one on a line with no conversion to excuse,
//     so a directive cannot outlive the conversion it was written for.
func TestIDSpaceConversions(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	var files []*ast.File
	var srcs [][]byte
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
		src, rerr := os.ReadFile(n)
		if rerr != nil {
			t.Fatal(rerr)
		}
		f, perr := parser.ParseFile(fset, n, src, parser.ParseComments|parser.SkipObjectResolution)
		if perr != nil {
			t.Fatal(perr)
		}
		files = append(files, f)
		srcs = append(srcs, src)
	}
	// Imports come from the compiler's export data, which go list builds or finds in the build cache.
	// Type-checking them from source instead costs most of this test's time: the standard library alone
	// is several times the size of this package.
	out, err := exec.Command("go", "list", "-export", "-deps", "-f", "{{.ImportPath}}\t{{.Export}}", ".").Output()
	if err != nil {
		t.Fatalf("go list -export: %v", err)
	}
	exports := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if path, file, ok := strings.Cut(l, "\t"); ok && file != "" {
			exports[path] = file
		}
	}
	lookup := func(path string) (io.ReadCloser, error) {
		file, ok := exports[path]
		if !ok {
			return nil, fmt.Errorf("no export data for %s", path)
		}
		return os.Open(file)
	}
	info := &types.Info{Types: map[ast.Expr]types.TypeAndValue{}}
	conf := types.Config{Importer: importer.ForCompiler(fset, "gc", lookup)}
	pkg, err := conf.Check("github.com/PelicanPlatform/classad/collections", fset, files, info)
	if err != nil {
		t.Fatal(err)
	}

	idTypes := map[types.Type]bool{}
	for _, v := range idspaceTypes {
		obj := pkg.Scope().Lookup(reflect.TypeOf(v).Name())
		if obj == nil {
			t.Fatalf("id type %T not found in the package", v)
		}
		idTypes[obj.Type()] = true
	}
	isID := func(t types.Type) bool { return t != nil && idTypes[t] }
	// A conversion through a type parameter an id type can instantiate launders the id as surely as a
	// direct one, so it needs the directive too.
	viaID := func(t types.Type) bool {
		if isID(t) {
			return true
		}
		tp, ok := t.(*types.TypeParam)
		if !ok {
			return false
		}
		for it := range idTypes {
			if types.Satisfies(it, tp.Constraint().Underlying().(*types.Interface)) {
				return true
			}
		}
		return false
	}

	// Every type ids.go declares is an id type, so a new one cannot be added without the check.
	for _, f := range files {
		if fset.Position(f.Package).Filename != "ids.go" {
			continue
		}
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
				for _, s := range gd.Specs {
					if obj := pkg.Scope().Lookup(s.(*ast.TypeSpec).Name.Name); obj == nil || !idTypes[obj.Type()] {
						t.Errorf("ids.go declares %s, which is not in idspaceTypes", s.(*ast.TypeSpec).Name.Name)
					}
				}
			}
		}
	}

	type directive struct {
		pos  token.Position
		used bool
	}
	type finding struct {
		file string
		line int
		msg  string
	}
	var bad []finding
	report := func(p token.Position, format string, args ...any) {
		bad = append(bad, finding{p.Filename, p.Line, fmt.Sprintf(format, args...)})
	}
	for fi, f := range files {
		// covers maps a line to the directive that excuses a conversion on it: a directive covers
		// its own line, and the next line too when it stands alone on its line.
		covers := map[int]*directive{}
		var dirs []*directive
		for _, cg := range f.Comments {
			for _, c := range cg.List {
				if !strings.HasPrefix(c.Text, "//idspace:") {
					continue
				}
				p := fset.Position(c.Pos())
				reason, ok := strings.CutPrefix(c.Text, idspaceDirective)
				if !ok || (reason != "" && reason[0] != ' ') {
					report(p, "unknown directive %q (want %s <reason>)", c.Text, idspaceDirective)
					continue
				}
				if strings.TrimSpace(reason) == "" {
					report(p, "%s without a reason", idspaceDirective)
					continue
				}
				d := &directive{pos: p}
				dirs = append(dirs, d)
				covers[p.Line] = d
				if standsAlone(fset, srcs[fi], c.Pos()) {
					covers[p.Line+1] = d
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				tv, ok := info.Types[x.Fun]
				if !ok || !tv.IsType() || len(x.Args) != 1 {
					return true
				}
				to, from := tv.Type, info.Types[x.Args[0]].Type
				if (!viaID(to) && !viaID(from)) || types.Identical(to, from) {
					return true
				}
				p, q := fset.Position(x.Pos()), types.RelativeTo(pkg)
				conv := fmt.Sprintf("conversion %s(%s)", types.TypeString(to, q), types.TypeString(from, q))
				switch d := covers[p.Line]; {
				case d == nil:
					report(p, "%s without %s", conv, idspaceDirective)
				case d.used:
					// One reason per conversion: a second one on the line, uint32(storedID(s)) say,
					// would otherwise pass on the first one's word.
					report(p, "%s shares its line's %s with another conversion", conv, idspaceDirective)
				default:
					d.used = true
				}
			case *ast.IndexExpr:
				if indexable(info.Types[x.X].Type) && isID(info.Types[x.Index].Type) {
					report(fset.Position(x.Pos()), "indexed by an id")
				}
			case *ast.SliceExpr:
				if indexable(info.Types[x.X].Type) {
					for _, b := range []ast.Expr{x.Low, x.High, x.Max} {
						if b != nil && isID(info.Types[b].Type) {
							report(fset.Position(x.Pos()), "sliced by an id")
						}
					}
				}
			case *ast.BasicLit, *ast.Ident:
				tv := info.Types[x.(ast.Expr)]
				if isID(tv.Type) && tv.Value != nil && constant.Sign(tv.Value) != 0 {
					report(fset.Position(x.Pos()), "constant id %s", tv.Value)
				}
			}
			return true
		})
		for _, d := range dirs {
			if !d.used {
				report(d.pos, "%s excuses no conversion", idspaceDirective)
			}
		}
	}
	sort.SliceStable(bad, func(i, j int) bool {
		if bad[i].file != bad[j].file {
			return bad[i].file < bad[j].file
		}
		return bad[i].line < bad[j].line
	})
	for _, b := range bad {
		t.Errorf("%s:%d: %s", b.file, b.line, b.msg)
	}
}

// indexable reports whether t is indexed by position: a slice, an array (or pointer to one), or a
// string. A map is indexed by key, which an id may be.
func indexable(t types.Type) bool {
	if t == nil {
		return false
	}
	u := t.Underlying()
	if p, ok := u.(*types.Pointer); ok {
		u = p.Elem().Underlying()
	}
	switch b := u.(type) {
	case *types.Slice, *types.Array:
		return true
	case *types.Basic:
		return b.Info()&types.IsString != 0
	}
	return false
}

// standsAlone reports whether only blanks precede pos on its line of src.
func standsAlone(fset *token.FileSet, src []byte, pos token.Pos) bool {
	tf := fset.File(pos)
	start, off := tf.Offset(tf.LineStart(tf.Line(pos))), tf.Offset(pos)
	return strings.TrimLeft(string(src[start:off]), " \t") == ""
}
