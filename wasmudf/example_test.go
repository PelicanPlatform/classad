package wasmudf

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// The two modules these examples load are testdata/examplepct.wat and testdata/examplemean.wat,
// committed as source beside the .wasm the examples read. They are the worked examples in
// docs/user-defined-functions.md; running them here is what keeps that page honest.

// Example_scalarModule loads a WebAssembly module and makes it callable from an ordinary ClassAd
// expression. From the expression's side there is nothing to see: a guest is reached through the
// same registry a Go function is, so pctUsed below could be either and the expression cannot tell.
func Example_scalarModule() {
	ctx := context.Background()

	// The Options are the sandbox, and they are the admin's, not the module's. A module cannot
	// ask for more: the host offers it exactly one import, so a guest wanting a clock or a file
	// is refused at load naming what it asked for.
	l, err := NewLoader(ctx, Options{
		MemoryLimitPages: 16,                     // 1 MiB of linear memory
		Steps:            1 << 20,                // the deterministic budget, in units the guest charges itself
		Timeout:          100 * time.Millisecond, // the backstop for a guest that charges nothing
	})
	if err != nil {
		panic(err)
	}
	defer l.Close(ctx) //nolint:errcheck // an example, not a daemon

	wasm, err := os.ReadFile("testdata/examplepct.wasm")
	if err != nil {
		panic(err)
	}

	// Loading comes first and registration second, so a name is only ever registered against a
	// module that compiled, exports what it must, speaks this ABI, and has been called once.
	// Neither registry has an unregister, so registering something unproven cannot be undone.
	u, err := Register(ctx, l, ClassAdRegistry(), "pctUsed", 2, 2, wasm)
	if err != nil {
		panic(err)
	}

	ad, err := classad.Parse(`[ Used = 3; Total = 8; Label = "eight" ]`)
	if err != nil {
		panic(err)
	}
	for _, src := range []string{
		`pctUsed(Used, Total)`,
		`pctUsed(Used, Absent)`,
		`pctUsed(Used, Label)`,
	} {
		expr, perr := classad.ParseExpr(src)
		if perr != nil {
			panic(perr)
		}
		fmt.Printf("%-24s %s\n", src, expr.Eval(ad).String())
	}

	// What the module did is counted, which is how an operator sees a guest that is quietly
	// failing every call rather than answering.
	s := u.Stats()
	fmt.Printf("calls=%d failures=%d vectored=%v trusted=%v\n", s.Calls, s.Failures, u.Vectored(), u.Trusted())

	// Output:
	// pctUsed(Used, Total)     37.5
	// pctUsed(Used, Absent)    undefined
	// pctUsed(Used, Label)     error
	// calls=3 failures=0 vectored=false trusted=false
}

// Example_aggregateModule loads a WebAssembly aggregate and reduces with it.
//
// An aggregate is a different module rather than a different function: four exports under their
// own ABI version, over a state the host holds as bytes. A module is one or the other.
func Example_aggregateModule() {
	ctx := context.Background()
	l, err := NewLoader(ctx, Options{})
	if err != nil {
		panic(err)
	}
	defer l.Close(ctx) //nolint:errcheck // an example, not a daemon

	wasm, err := os.ReadFile("testdata/examplemean.wasm")
	if err != nil {
		panic(err)
	}
	a, err := l.LoadAggregate(ctx, "wasmMean", wasm)
	if err != nil {
		panic(err)
	}

	// Columnar reports whether the module exported the optional numeric fold -- that is, whether
	// it declared it may be fed from a column instead of from decoded records.
	fmt.Println("columnar:", a.Columnar())

	// CheckAggregate runs the merge laws over a sample of the caller's choosing. It is not a
	// proof -- the laws are universally quantified and this is one finite sample -- but an
	// order-sensitive merge is exactly the mistake it finds, and the mistake an author's own
	// tests do not, because an author tests the order their own code produces.
	samples := []classad.Value{
		classad.NewIntValue(2),
		classad.NewIntValue(4),
		classad.NewRealValue(9.0),
		classad.NewIntValue(1),
		classad.NewStringValue("not a number"),
		classad.NewUndefinedValue(),
	}
	fmt.Println("laws:", db.CheckAggregate(a.Aggregate(), samples))

	// Reducing by hand, as the engine does: one accumulator per group, merged, then rendered.
	agg := a.Aggregate()
	left, right := agg.NewAccumulator(), agg.NewAccumulator()
	for _, v := range samples[:2] {
		left.Add(v)
	}
	for _, v := range samples[2:] {
		right.Add(v)
	}
	left.Merge(right)
	fmt.Println("mean:", left.Result().String())
	fmt.Println("empty group:", agg.NewAccumulator().Result().String())

	// Output:
	// columnar: true
	// laws: <nil>
	// mean: 4
	// empty group: undefined
}
