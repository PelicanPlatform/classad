package vm

import (
	"fmt"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// Example_script builds and runs a ClassAd script: local bindings, a branch, and a result.
//
// A script is the statement language over the same engine an expression compiles to -- it exists
// for the things that are awkward as one expression. There is no parser yet, so a script is built
// through this API rather than written down.
//
// The binding rule is Python's. A bare name that has been bound is the local; anything else is an
// attribute. So `want = want` on the first line reads the ATTRIBUTE and binds the LOCAL, and every
// later mention of want is the local. A scoped reference (MY.want) always means the attribute.
func Example_script() {
	must := func(src string) ast.Expr {
		e, err := parser.ParseExpr(src)
		if err != nil {
			panic(err)
		}
		return e
	}

	// The effective memory request: whatever the ad asks for, with a default and a floor.
	sp, err := NewScript().
		Let("want", must("RequestMemory")).
		If(must("want is undefined"),
			func(s *Script) { s.Let("want", must("2048")) },
			nil).
		If(must("want < 512"),
			func(s *Script) { s.Let("want", must("512")) },
			nil).
		Result(must("want")).
		Compile()
	if err != nil {
		panic(err)
	}

	for _, src := range []string{
		`[ RequestMemory = 4096 ]`,
		`[ RequestMemory = 100 ]`,
		`[ Other = 1 ]`,
	} {
		ad, perr := classad.Parse(src)
		if perr != nil {
			panic(perr)
		}
		fmt.Printf("%-26s %s\n", src, sp.Run(ad).String())
	}

	// Output:
	// [ RequestMemory = 4096 ]   4096
	// [ RequestMemory = 100 ]    512
	// [ Other = 1 ]              2048
}
