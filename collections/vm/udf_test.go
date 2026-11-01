package vm

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// TestRegisteredUDFCompilesNatively is the payoff of the registration, and it is a cross-module
// claim: nothing in this package knows a UDF from a built-in.
//
// What it buys is Native(), not call speed. A delegated call takes the whole query off the index
// probes, the column prefilter and the vector executor; a native one does not. A UDF that compiled
// to OpEvalNode would be a UDF that made every query using it slow in a way nothing reported.
func TestRegisteredUDFCompilesNatively(t *testing.T) {
	if err := classad.RegisterFunction("vmUdfDouble", 1, 1, func(args []classad.Value) classad.Value {
		n, err := args[0].IntValue()
		if err != nil {
			return classad.NewErrorValue()
		}
		return classad.NewIntValue(n * 2)
	}); err != nil {
		t.Fatal(err)
	}

	q, err := Parse(`vmUdfDouble(Cpus) > 4`)
	if err != nil {
		t.Fatal(err)
	}
	p := q.Program()
	if !usesOp(p, OpCallFunc) {
		t.Error("a registered UDF did not compile to OpCallFunc")
	}
	if !q.Native() {
		t.Error("a query calling a registered UDF is not Native, so it is off the columnar accelerators")
	}
	// The wrong arity still delegates, so it is ERROR without the arguments being evaluated.
	bad, err := Parse(`vmUdfDouble(Cpus, 2)`)
	if err != nil {
		t.Fatal(err)
	}
	if bad.Native() {
		t.Error("a wrong-arity UDF call is native; it must delegate so the arity error does not evaluate arguments")
	}

	// Every executor agrees: the tree-walker, the stack interpreter and the register form.
	ad, err := classad.Parse(`[ Cpus = 4 ]`)
	if err != nil {
		t.Fatal(err)
	}
	expr, err := parser.ParseExpr(`vmUdfDouble(Cpus)`)
	if err != nil {
		t.Fatal(err)
	}
	prog := CompileProgram(expr)
	want := refEval(ad, expr)
	if want.String() != "8" {
		t.Fatalf("the evaluator gives %s, want 8", want)
	}
	if got := Run(prog, ad); !valuesEqual(want, got) {
		t.Errorf("stack interpreter = %s, evaluator = %s", describe(got), describe(want))
	}
	if got := RunRegisters(LowerToRegisters(prog), ad); !valuesEqual(want, got) {
		t.Errorf("register form = %s, evaluator = %s", describe(got), describe(want))
	}
}

// TestRegisteredUDFVectorizes checks the vector executor serves a UDF too, elementwise through
// callFuncVec, with no knowledge that it is not a built-in.
func TestRegisteredUDFVectorizes(t *testing.T) {
	if err := classad.RegisterFunction("vmUdfIsBig", 1, 1, func(args []classad.Value) classad.Value {
		n, err := args[0].IntValue()
		if err != nil {
			return classad.NewUndefinedValue()
		}
		return classad.NewBoolValue(n > 4)
	}); err != nil {
		t.Fatal(err)
	}
	src := vecCorpus()
	n := len(src.recs)
	for _, exprSrc := range []string{`vmUdfIsBig(A)`, `vmUdfIsBig(A) && B > 1`, `vmUdfIsBig(B)`} {
		q, err := Parse(exprSrc)
		if err != nil {
			t.Fatal(err)
		}
		var scratch VecScratch
		got, ok := q.VecEval(src, n, &scratch)
		if !ok {
			t.Errorf("%s: the vector executor declined a UDF call", exprSrc)
			continue
		}
		for i := 0; i < n; i++ {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s != scalar %s", exprSrc, i, got.value(i), want)
			}
		}
	}
}
