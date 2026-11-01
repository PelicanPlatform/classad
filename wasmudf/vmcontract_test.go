package wasmudf

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"testing"
)

// The vector ABI mirrors collections/vm's representation rather than converting
// to one of its own, which is what makes a column pass through without a pass
// over n elements first. The price of mirroring is that the two declarations have
// to agree, and nothing the compiler does will tell us when they stop: this
// package cannot import collections/vm, because the engine is the caller and the
// import would be a cycle.
//
// So these tests read the engine's SOURCE and check. They parse it rather than
// grepping it, so a comment mentioning VsReal or a constant moved between const
// blocks does not change the answer.
//
// A renumbering is exactly the kind of change that looks harmless and is not: a
// real read as an integer is a wrong answer with no error anywhere, and it would
// pass every test in this package, all of which use this package's own numbers on
// both sides of the comparison. These two tests are the only place the engine's
// numbers enter.

const enginePkgDir = "../collections/vm"

func parseEngineFile(t *testing.T, name string) *ast.File {
	t.Helper()
	path := filepath.Join(enginePkgDir, name)
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
	if err != nil {
		// Not a skip. A suite that passes because it could not find the thing it
		// was checking is worse than no suite: this file is in the same
		// repository and is always there, so failing to read it is a finding.
		t.Fatalf("cannot read the engine's %s, which this ABI mirrors: %v", name, err)
	}
	return f
}

// engineConsts returns every constant in a file with its integer value,
// evaluating iota by position within its ConstSpec list the way the compiler
// does.
func engineConsts(t *testing.T, f *ast.File) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	for _, d := range f.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		var last ast.Expr
		for i, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			// A spec with no value repeats the previous expression, which is how
			// an iota block after its first line is written.
			if len(vs.Values) > 0 {
				last = vs.Values[0]
			}
			for _, nm := range vs.Names {
				v, ok := constValue(last, i)
				if !ok {
					continue
				}
				out[nm.Name] = v
			}
		}
	}
	return out
}

// constValue evaluates the handful of expression shapes these const blocks use:
// a literal, iota, and a conversion wrapping either.
func constValue(e ast.Expr, iotaAt int) (int64, bool) {
	switch x := e.(type) {
	case *ast.BasicLit:
		if x.Kind != token.INT {
			return 0, false
		}
		n, err := strconv.ParseInt(x.Value, 0, 64)
		return n, err == nil
	case *ast.Ident:
		if x.Name == "iota" {
			return int64(iotaAt), true
		}
		return 0, false
	case *ast.CallExpr:
		if len(x.Args) == 1 {
			return constValue(x.Args[0], iotaAt)
		}
	}
	return 0, false
}

// TestVectorStatesMatchEngine pins the data-form state codes and the mask-form
// bit pairs against the engine's own.
func TestVectorStatesMatchEngine(t *testing.T) {
	states := engineConsts(t, parseEngineFile(t, "vecexec.go"))
	for _, tc := range []struct {
		engine string
		mine   uint8
	}{
		{"VsInt", vsInt},
		{"VsReal", vsReal},
		{"VsBool", vsBool},
		{"VsUndef", vsUndef},
		{"VsError", vsError},
		{"VsString", vsString},
		{"VsDictCode", vsDictCode},
	} {
		got, ok := states[tc.engine]
		if !ok {
			t.Errorf("the engine no longer declares %s; this ABI's state codes are derived from it", tc.engine)
			continue
		}
		if got != int64(tc.mine) {
			t.Errorf("%s is %d in the engine and %d here: a column would be read as the wrong type",
				tc.engine, got, tc.mine)
		}
	}

	// statePoison must not collide with any real state, or a guest writing a
	// legitimate value would be read as having written nothing.
	for name, v := range states {
		if len(name) > 2 && name[:2] == "Vs" && v == int64(statePoison) {
			t.Errorf("statePoison collides with the engine's %s", name)
		}
	}

	masks := engineConsts(t, parseEngineFile(t, "vecmask.go"))
	for _, tc := range []struct {
		engine string
		mine   int
	}{
		{"mFalse", mkFalse},
		{"mTrue", mkTrue},
		{"mUndef", mkUndef},
		{"mError", mkError},
	} {
		got, ok := masks[tc.engine]
		if !ok {
			t.Errorf("the engine no longer declares %s", tc.engine)
			continue
		}
		if got != int64(tc.mine) {
			t.Errorf("%s is %d in the engine and %d here: three-valued logic would be read inverted",
				tc.engine, got, tc.mine)
		}
	}
}

// TestBatchMirrorsVecFields pins the FIELD NAMES Batch aliases.
//
// The adapter on the engine's side is `Batch{St: v.St, I: v.I, Hi: v.Hi, Lo: v.Lo,
// ...}`, which only compiles while Vec has those fields with those types. A rename
// over there is a compile error in an adapter that does not exist yet, so this
// test is what stands in for the compiler until it does.
func TestBatchMirrorsVecFields(t *testing.T) {
	f := parseEngineFile(t, "vecexec.go")

	want := map[string]string{
		"I":     "[]int64",
		"St":    "[]uint8",
		"Hi":    "[]uint64",
		"Lo":    "[]uint64",
		"Const": "bool",
		"Mask":  "bool",
	}
	found := map[string]string{}

	ast.Inspect(f, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || ts.Name.Name != "Vec" {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return false
		}
		for _, fld := range st.Fields.List {
			for _, nm := range fld.Names {
				if _, interesting := want[nm.Name]; interesting {
					found[nm.Name] = types(fld.Type)
				}
			}
		}
		return false
	})

	if len(found) == 0 {
		t.Fatal("no Vec struct in the engine's vecexec.go; Batch is declared to mirror it")
	}
	for name, wantType := range want {
		got, ok := found[name]
		if !ok {
			t.Errorf("Vec has no field %s, which Batch aliases", name)
			continue
		}
		if got != wantType {
			t.Errorf("Vec.%s is %s and Batch.%s is %s", name, got, name, wantType)
		}
	}
}

// types renders the handful of type expressions these fields use.
func types(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.ArrayType:
		if x.Len == nil {
			return "[]" + types(x.Elt)
		}
	}
	return "?"
}

// TestMaskWordsMatchesEngine -- the bitplane length. Disagreeing by one word
// means the host copies one word too few and the last up-to-64 records of every
// batch carry whatever was in guest memory.
func TestMaskWordsMatchesEngine(t *testing.T) {
	// The engine's maskWords is (n + 63) / 64, and MaskWords is exported for
	// exactly this kind of caller. Checked against a table rather than against a
	// reimplementation, so this does not merely compare one copy of the formula
	// with another.
	for _, tc := range []struct{ n, want int }{
		{0, 0}, {1, 1}, {63, 1}, {64, 1}, {65, 2}, {128, 2}, {129, 3}, {4096, 64},
	} {
		if got := maskWords(tc.n); got != tc.want {
			t.Errorf("maskWords(%d) = %d, want %d", tc.n, got, tc.want)
		}
	}
}
