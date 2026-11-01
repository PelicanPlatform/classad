package wasmudf

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// loadTrusted loads a fixture with the admin's trust label set.
func loadTrusted(t *testing.T, opts Options, name string, minArgs, maxArgs int) *UDF {
	t.Helper()
	l := newLoader(t, opts)
	u, err := l.LoadWith(context.Background(), name, minArgs, maxArgs,
		fixture(t, name), LoadOptions{Trusted: true})
	if err != nil {
		t.Fatalf("loading %s as trusted: %v", name, err)
	}
	if !u.Trusted() {
		t.Fatal("a module loaded as trusted does not report itself trusted")
	}
	return u
}

func TestTrustedModuleRuns(t *testing.T) {
	u := loadTrusted(t, Options{}, "identity", 1, 1)
	for _, in := range []int64{0, 7, -1, 1 << 62} {
		if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(in)})); n != in {
			t.Errorf("identity(%d) = %d", in, n)
		}
	}
	if s := u.Stats(); s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestTrustedKeepsTheStepBudget is the design decision, written as a test.
//
// Trust turns off the WALL-CLOCK backstop and wazero's interruptibility -- the
// ~340ns per call that the measurement says is the cost. It does NOT turn off the
// deterministic step budget, for two reasons the benchmark settles rather than
// argues: the budget is not where the time goes (a guest that declares no steps
// pays nothing for it at all), and it is the only bound whose failures are
// reproducible. A trusted module and an untrusted one therefore fail on the same
// inputs at the same iteration, which is what makes them comparable.
//
// fuelspin loops forever declaring a step per iteration. Trusted, with no clock
// able to stop it, the budget is the only thing that can -- so this test hangs if
// the budget is ever made part of what trust relaxes.
func TestTrustedKeepsTheStepBudget(t *testing.T) {
	u := loadTrusted(t, Options{Steps: 10_000}, "fuelspin", 0, -1)
	got := u.Call(nil)
	if !got.IsError() {
		t.Fatalf("fuelspin returned %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonStepLimit)
}

// TestTrustedKeepsTheMemoryCap -- the memory limit is a property of the runtime
// and applies to both of them. A module wanting more than the cap is refused at
// load whether or not an admin trusts it: trust is about liveness, not about how
// much of the host's memory a guest may take.
func TestTrustedKeepsTheMemoryCap(t *testing.T) {
	l := newLoader(t, Options{MemoryLimitPages: 128})
	_, err := l.LoadWith(context.Background(), "bigmem", 0, -1,
		fixture(t, "bigmem"), LoadOptions{Trusted: true})
	if err == nil {
		t.Fatal("a trusted module was allowed past the memory cap")
	}
}

// TestTrustedKeepsThePoison -- the poisoned result slot is a CORRECTNESS
// mechanism, not a safety one: it is what stops a guest that writes nothing from
// returning the previous record's answer. It costs one 4-byte store per call, so
// there is nothing to buy by dropping it, and what it prevents is a wrong value
// rather than a hang.
func TestTrustedKeepsThePoison(t *testing.T) {
	u := loadTrusted(t, Options{}, "stale", 1, 1)
	if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(7)})); n != 7 {
		t.Fatalf("precondition: stale(7) = %d", n)
	}
	if got := u.Call([]classad.Value{classad.NewStringValue("x")}); !got.IsError() {
		t.Fatalf(`stale("x") = %v, want ERROR`, got)
	}
	requireReason(t, u, ReasonBadResult)
}

// TestTrustedStillDiscardsATrappedInstance -- a trapped guest's memory is in an
// unknown state whoever vouched for it, and reusing it risks a wrong answer for
// the next record rather than a second failure.
func TestTrustedStillDiscardsATrappedInstance(t *testing.T) {
	u := loadTrusted(t, Options{}, "trap", 0, -1)
	for range 3 {
		if got := u.Call(nil); !got.IsError() {
			t.Fatalf("trap returned %v", got)
		}
	}
	if s := u.Stats(); s.Instances != 3 {
		t.Errorf("created %d instances for 3 trapping calls, want 3", s.Instances)
	}
	requireFullSemaphore(t, u)
}

// TestTrustIsNotSelfAsserted is the security property.
//
// trustclaim says it is trusted three ways: an exported function, an exported
// global, and a custom section named for the flag. All three must be ignored,
// because a capability a module can assert is not a capability. The only input is
// the admin's LoadOptions, and there is deliberately no ABI through which a guest
// could ask.
func TestTrustIsNotSelfAsserted(t *testing.T) {
	l := newLoader(t, Options{})
	u, err := l.Load(context.Background(), "claims", 1, 1, fixture(t, "trustclaim"))
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if u.Trusted() {
		t.Error("a module talked the host into trusting it")
	}
	// And it still works -- the claims are ignored, not treated as an error, so a
	// module that happens to export a name like that is not refused.
	if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(1)})); n != 7 {
		t.Errorf("claims(1) = %d, want 7", n)
	}

	// The same bytes, trusted, because an ADMIN said so.
	u2, err := l.LoadWith(context.Background(), "labelled", 1, 1,
		fixture(t, "trustclaim"), LoadOptions{Trusted: true})
	if err != nil {
		t.Fatalf("loading as trusted: %v", err)
	}
	if !u2.Trusted() {
		t.Error("the admin's label was not applied")
	}
}

// TestTrustedAndUntrustedCoexist -- the two live in separate wazero runtimes,
// because interruptibility is a property of the compiled code rather than of the
// call. One Loader has to serve both, and a module in one must not be affected by
// the other's existence.
func TestTrustedAndUntrustedCoexist(t *testing.T) {
	l := newLoader(t, Options{})

	plain, err := l.Load(context.Background(), "plain", 1, 1, fixture(t, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := l.LoadWith(context.Background(), "fast", 1, 1,
		fixture(t, "identity"), LoadOptions{Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Trusted() || !fast.Trusted() {
		t.Fatalf("trust labels crossed: plain=%v fast=%v", plain.Trusted(), fast.Trusted())
	}

	// Interleaved, so neither is merely working because it ran first.
	for i := range 20 {
		v := int64(i)
		if n := mustInt(t, plain.Call([]classad.Value{classad.NewIntValue(v)})); n != v {
			t.Fatalf("plain(%d) = %d", v, n)
		}
		if n := mustInt(t, fast.Call([]classad.Value{classad.NewIntValue(v)})); n != v {
			t.Fatalf("fast(%d) = %d", v, n)
		}
	}

	// The untrusted one is still interruptible, which is the thing the two
	// runtimes exist to keep apart. hardspin never yields, so only the clock can
	// end it -- and it ends, which it could not if the untrusted module had been
	// compiled into the trusted runtime.
	spin, err := l.Load(context.Background(), "spin", 0, -1, fixture(t, "hardspin"))
	if err != nil {
		t.Fatal(err)
	}
	if got := spin.Call(nil); !got.IsError() {
		t.Fatalf("hardspin returned %v", got)
	}
	requireReason(t, spin, ReasonTimeout)
}

// TestTrustedTimeoutIsDisabledNotMerelyUnused -- a deadline over a guest that
// cannot be interrupted is a timer firing into nothing, so the two have to be
// turned off together. A Loader configured with a one-nanosecond timeout would
// fail every untrusted call; a trusted module must be unaffected, which is only
// true if no deadline context is derived for it at all.
func TestTrustedTimeoutIsDisabledNotMerelyUnused(t *testing.T) {
	l := newLoader(t, Options{Timeout: time.Nanosecond})

	fast, err := l.LoadWith(context.Background(), "fast", 1, 1,
		fixture(t, "identity"), LoadOptions{Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 10 {
		if got := fast.Call([]classad.Value{classad.NewIntValue(3)}); got.IsError() {
			t.Fatalf("a trusted call hit a deadline it should not have: %v", fast.Stats().ByReason)
		}
	}
}

// TestTrustedVectorBatch -- the two features compose: a trusted module with a
// vector entry point gets the non-interruptible call path AND one deadline-free
// call per batch, which is the configuration the whole exercise is aiming at.
func TestTrustedVectorBatch(t *testing.T) {
	u := loadTrusted(t, Options{}, "vecdouble", 1, 1)
	in := intBatch(1, 2, 3, 4, 5)
	out := outBatch(5)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatalf("declined: %v", u.Stats().ByReason)
	}
	wantInts(t, out, []int64{2, 4, 6, 8, 10})
}

func TestVectorBadSignatureIsALoadError(t *testing.T) {
	err := loadErr(t, Options{}, "vecbadsig")
	if !strings.Contains(err.Error(), exportCallVec) {
		t.Errorf("error does not name the vector entry point: %v", err)
	}
	if !strings.Contains(err.Error(), "i32 i32 i32 i32 i32") {
		t.Errorf("error does not say what the signature should be: %v", err)
	}
}
