package wasmudf

import (
	"context"
	"strings"
	"testing"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"

	"github.com/PelicanPlatform/classad/classad"
)

// These benchmarks exist to answer one question with a number instead of an
// argument: is a WASM UDF called once per record worth having, or does the
// per-call overhead swamp whatever the guest computes?
//
// The comparisons that matter are laid out so they can be read against each
// other:
//
//	BenchmarkGuestCall           a whole UDF call, the cheapest possible guest
//	BenchmarkNativeUDF           the same thing as a Go function -- the floor
//	BenchmarkBuiltinThroughSeam  a built-in through the same strict seam
//	BenchmarkEvaluateExpr        one record's worth of ordinary evaluation
//
// The last one is the denominator. A UDF's cost only matters relative to what
// the record costs anyway.

func benchFixture(b *testing.B, opts Options, name string, minArgs, maxArgs int) *UDF {
	b.Helper()
	l, err := NewLoader(context.Background(), opts)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = l.Close(context.Background()) })
	wasm, err := readFixture(name)
	if err != nil {
		b.Fatal(err)
	}
	u, err := l.Load(context.Background(), name, minArgs, maxArgs, wasm)
	if err != nil {
		b.Fatal(err)
	}
	return u
}

// BenchmarkGuestCall is the full per-call cost: argument encoding, the deadline
// context, the wasm call, the result decode. identity is the cheapest guest that
// can exist, so this is overhead with no computation under it.
func BenchmarkGuestCall(b *testing.B) {
	u := benchFixture(b, Options{}, "identity", 1, 1)
	args := []classad.Value{classad.NewIntValue(42)}
	for b.Loop() {
		if v := u.Call(args); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkGuestCallNoDeadline removes the per-call context.WithTimeout, which is
// a timer insertion and two allocations on a path that runs once per record. The
// gap between this and BenchmarkGuestCall is what the wall-clock backstop costs
// at the API level -- it does NOT remove the termination checks the compiler
// emitted into the guest, which are charged whatever the deadline setting is.
func BenchmarkGuestCallNoDeadline(b *testing.B) {
	u := benchFixture(b, Options{Timeout: -1}, "identity", 1, 1)
	args := []classad.Value{classad.NewIntValue(42)}
	for b.Loop() {
		if v := u.Call(args); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkGuestCallString adds a string in and a string out: bytes copied into
// the arena, a pointer validated, bytes copied back out.
func BenchmarkGuestCallString(b *testing.B) {
	u := benchFixture(b, Options{}, "identity", 1, 1)
	args := []classad.Value{classad.NewStringValue(strings.Repeat("x", 32))}
	for b.Loop() {
		if v := u.Call(args); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkGuestCallFuel is 100 charged steps against BenchmarkGuestCall's zero,
// so the difference over 100 is the per-step cost of instrumentation -- the
// number a gas-injecting toolchain would multiply by the guest's basic-block
// count, and therefore the thing that decides whether making the step budget
// involuntary is affordable at all.
//
// Both deadline settings are measured because a host call crosses the same
// boundary an exported call does, so the interruption support is charged per
// CHARGE and not only per call.
func BenchmarkGuestCallFuel(b *testing.B) {
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"deadline", Options{}},
		{"nodeadline", Options{Timeout: -1}},
	} {
		b.Run(tc.name, func(b *testing.B) {
			u := benchFixture(b, tc.opts, "fuelcount", 0, -1)
			for b.Loop() {
				if v := u.Call(nil); v.IsError() {
					b.Fatal("ERROR")
				}
			}
		})
	}
}

// BenchmarkNativeUDF is the same registration done in Go: the floor any wasm
// number is measured against.
func BenchmarkNativeUDF(b *testing.B) {
	fn := func(args []classad.Value) classad.Value { return args[0] }
	args := []classad.Value{classad.NewIntValue(42)}
	for b.Loop() {
		if v := fn(args); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkBuiltinThroughSeam calls a built-in through ApplyFunction, the seam a
// compiled program dispatches a strict call through. It is the fairest comparison
// available: same call site, same boxed arguments, different implementation.
//
// Note that it needs an evaluator and a UDF does not -- ApplyFunction takes one
// solely to materialize a lazy list argument, which is the thing a UDF is
// forbidden to do.
func BenchmarkBuiltinThroughSeam(b *testing.B) {
	ev := classad.NewEvaluator(nil)
	args := []classad.Value{classad.NewIntValue(2), classad.NewIntValue(10)}
	for b.Loop() {
		if v := classad.ApplyFunction(ev, "pow", args); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkEvaluateExpr is one record of ordinary evaluation -- a small
// Requirements-shaped expression over a small ad. Every UDF number above is only
// meaningful next to this one, because a UDF is called from inside this.
func BenchmarkEvaluateExpr(b *testing.B) {
	ad := classad.New()
	for k, v := range map[string]any{
		"Memory": 4096, "Cpus": 8, "Disk": 1000000, "OpSys": "LINUX", "Arch": "X86_64",
	} {
		if err := ad.Set(k, v); err != nil {
			b.Fatal(err)
		}
	}
	expr, err := classad.ParseExpr(`Memory > 2048 && Cpus >= 2 && OpSys == "LINUX"`)
	if err != nil {
		b.Fatal(err)
	}
	for b.Loop() {
		if v := ad.EvaluateExprWithTarget(expr, nil); v.IsError() {
			b.Fatal("ERROR")
		}
	}
}

// BenchmarkRawGuestInterruptible isolates what WithCloseOnContextDone costs
// inside the guest, as opposed to at the API: the same module, called the same
// way, on two runtimes differing only in that flag. The compiler emits a
// termination check at loop back-edges and calls to make interruption possible,
// and every guest pays for it whether or not it would ever need stopping.
//
// It bypasses UDF.Call deliberately -- including it would mix the API's own cost
// back in, which the two benchmarks above already separate.
func BenchmarkRawGuestInterruptible(b *testing.B) {
	for _, tc := range []struct {
		name          string
		interruptible bool
	}{
		{"interruptible", true},
		{"uninterruptible", false},
	} {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			cfg := wazero.NewRuntimeConfig().
				WithMemoryLimitPages(128).
				WithCoreFeatures(api.CoreFeaturesV2).
				WithCloseOnContextDone(tc.interruptible)
			rt := wazero.NewRuntimeWithConfig(ctx, cfg)
			defer func() { _ = rt.Close(ctx) }()

			wasm, err := readFixture("identity")
			if err != nil {
				b.Fatal(err)
			}
			compiled, err := rt.CompileModule(ctx, wasm)
			if err != nil {
				b.Fatal(err)
			}
			mod, err := rt.InstantiateModule(ctx, compiled,
				wazero.NewModuleConfig().WithName("").WithStartFunctions())
			if err != nil {
				b.Fatal(err)
			}
			fn := mod.ExportedFunction(exportCall)
			stack := make([]uint64, 3)
			for b.Loop() {
				stack[0], stack[1], stack[2] = 0, 1, 32
				if err := fn.CallWithStack(ctx, stack); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
