package classad_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
)

// Example_userDefinedFunction registers a scalar function and calls it from an ordinary
// expression. A registered name is a function like any built-in: the parser does not know it is
// user-defined, name lookup folds case, and the compiler lowers a call to it natively rather than
// delegating the expression to the tree-walking evaluator.
//
// The three calls below are the three answers a UDF has to be able to give, and the reason the
// signature returns a Value rather than (Value, error): UNDEFINED and ERROR are values in this
// language, and which one a missing argument deserves is the function's decision, not the caller's.
func Example_userDefinedFunction() {
	// pctUsed(used, total) -- what fraction of a resource is in use, as a percentage.
	err := classad.RegisterFunction("pctUsed", 2, 2, func(args []classad.Value) classad.Value {
		// Undefined in, undefined out. An attribute the ad does not carry is not a
		// mistake, and a Requirements expression that treated it as one would reject
		// the machine instead of ignoring the term.
		if args[0].IsUndefined() || args[1].IsUndefined() {
			return classad.NewUndefinedValue()
		}
		used, uerr := args[0].NumberValue()
		total, terr := args[1].NumberValue()
		if uerr != nil || terr != nil || total == 0 {
			// A string where a number belongs, or a division that has no answer.
			// Report it by RETURNING ERROR; do not panic. A panic here is
			// contained, but containment is a backstop and not an interface.
			return classad.NewErrorValue()
		}
		return classad.NewRealValue(used / total * 100)
	})
	if err != nil {
		panic(err)
	}

	ad, err := classad.Parse(`[ Used = 3; Total = 8; Label = "eight" ]`)
	if err != nil {
		panic(err)
	}
	for _, src := range []string{
		`pctUsed(Used, Total)`,  // both numbers
		`PCTUSED(Used, Total)`,  // names fold, as every function name does
		`pctUsed(Used, Absent)`, // an attribute the ad does not carry
		`pctUsed(Used, Label)`,  // a string where a number belongs
	} {
		expr, err := classad.ParseExpr(src)
		if err != nil {
			panic(err)
		}
		fmt.Printf("%-24s %s\n", src, expr.Eval(ad).String())
	}

	// A UDF stays eligible for the compiled and columnar paths because it promised purity by
	// its signature: it is handed values, so it cannot read an attribute the compiler did not
	// know the expression would read.
	fmt.Println("compilable:", classad.StrictFunction("pctUsed", 2))

	// Output:
	// pctUsed(Used, Total)     37.5
	// PCTUSED(Used, Total)     37.5
	// pctUsed(Used, Absent)    undefined
	// pctUsed(Used, Label)     error
	// compilable: true
}
