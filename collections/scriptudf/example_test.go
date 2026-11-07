package scriptudf_test

import (
	"fmt"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/scriptudf"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Example registers a function written in ClassAd Script and calls it from an ordinary expression.
//
// The module is the same `pctUsed` the Go and WebAssembly examples implement, written as statements
// instead: the two absence cases are separate `if`s rather than a condition, which is what a
// statement language buys. After registration the name behaves exactly as a built-in does -- it
// folds case, it is compiled in place, and the three answers a function can give are values.
func Example() {
	mod, err := vm.ParseScriptModule(`
# What fraction of a resource is in use, as a percentage.
def examplePct(used, total):
    # Undefined in, undefined out: an attribute the ad does not carry is not a mistake, and a
    # Requirements expression that treated it as one would reject the machine rather than ignore
    # the term.
    if used is undefined:
        return undefined
    if total is undefined:
        return undefined
    # A division with no answer. Report it by RETURNING error.
    if total <= 0:
        return error
    return used * 100.0 / total
`)
	if err != nil {
		panic(err)
	}
	fmt.Println("exports:", mod.Functions())

	// Compiling comes first and registration second: a name is only ever registered against a
	// function that compiled and that the compiler agrees reads nothing but its arguments.
	f, err := scriptudf.Register(mod, scriptudf.ClassAdRegistry(), "examplePct")
	if err != nil {
		panic(err)
	}
	// The arity is the def's own, so there is no min/max to pass and none to get wrong.
	fmt.Println("arity:", f.Arity())

	ad, err := classad.Parse(`[ Used = 3; Total = 8; Label = "eight" ]`)
	if err != nil {
		panic(err)
	}
	for _, src := range []string{
		`examplePct(Used, Total)`,  // both numbers
		`EXAMPLEPCT(Used, Total)`,  // names fold, as every function name does
		`examplePct(Used, Absent)`, // an attribute the ad does not carry
		`examplePct(Used, Label)`,  // a string where a number belongs
	} {
		expr, perr := classad.ParseExpr(src)
		if perr != nil {
			panic(perr)
		}
		fmt.Printf("%-27s %s\n", src, expr.Eval(ad).String())
	}
	fmt.Println("compilable:", classad.StrictFunction("examplePct", 2))

	// A registered function may not read the ad. Inside a script `Optimistic` would be an
	// attribute of the subject ad; a registration has no subject, so this is refused rather than
	// registered and left to answer UNDEFINED forever.
	impure, err := vm.ParseScriptModule("def exampleFits(requested, cap):\n    return Optimistic\n")
	if err != nil {
		panic(err)
	}
	_, err = scriptudf.Register(impure, scriptudf.ClassAdRegistry(), "exampleFits")
	fmt.Println("reaches the ad:", err != nil)

	// Output:
	// exports: [examplePct]
	// arity: 2
	// examplePct(Used, Total)     37.5
	// EXAMPLEPCT(Used, Total)     37.5
	// examplePct(Used, Absent)    undefined
	// examplePct(Used, Label)     error
	// compilable: true
	// reaches the ad: true
}
