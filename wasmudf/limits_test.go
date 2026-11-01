package wasmudf

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// TestStepBudgetStopsRunaway is the deterministic bound doing its job: fuelspin
// never terminates on its own and never returns, yet the call comes back.
//
// The budget is set far below what the wall-clock backstop would allow, so the
// assertion that the reason is ReasonStepLimit and not ReasonTimeout is the real
// content: it says the REPRODUCIBLE limit fired, which is the one whose failures
// can be tested against another implementation of the same function.
func TestStepBudgetStopsRunaway(t *testing.T) {
	u := loadFixture(t, Options{Steps: 10_000}, "fuelspin", 0, -1)
	got := u.Call(nil)
	if !got.IsError() {
		t.Fatalf("fuelspin returned %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonStepLimit)
}

// TestStepBudgetBoundary pins the budget's arithmetic at the exact iteration.
// fuelcount declares 100 steps and then returns 100, so 100 is enough and 99 is
// one short -- a boundary rather than "a big number works and a small one does
// not", which would pass for a budget off by any amount.
func TestStepBudgetBoundary(t *testing.T) {
	t.Run("exactly enough", func(t *testing.T) {
		u := loadFixture(t, Options{Steps: 100}, "fuelcount", 0, -1)
		if n := mustInt(t, u.Call(nil)); n != 100 {
			t.Errorf("fuelcount = %d, want 100", n)
		}
		if s := u.Stats(); s.Failures != 0 {
			t.Errorf("unexpected failures: %v", s.ByReason)
		}
	})
	t.Run("one short", func(t *testing.T) {
		u := loadFixture(t, Options{Steps: 99}, "fuelcount", 0, -1)
		if got := u.Call(nil); !got.IsError() {
			t.Errorf("fuelcount with 99 steps = %v, want ERROR", got)
		}
		requireReason(t, u, ReasonStepLimit)
	})
}

// TestStepBudgetIsPerCall catches a budget that is spent once and never refilled.
// A UDF is called once per record, so a budget that leaked across calls would
// work for a while and then turn every remaining record into ERROR -- a failure
// that depends on how many records came before it.
func TestStepBudgetIsPerCall(t *testing.T) {
	u := loadFixture(t, Options{Steps: 100}, "fuelcount", 0, -1)
	for i := range 50 {
		if n := mustInt(t, u.Call(nil)); n != 100 {
			t.Fatalf("call %d: fuelcount = %d, want 100", i, n)
		}
	}
}

// TestZeroCostIsStillCharged -- a guest cannot declare its way out of the budget.
// fuelzero charges 0 on every iteration of a loop that never ends, so a budget
// that believed the declared cost would never stop it and the only thing that
// could is the clock, which is the limit this one is supposed to replace.
func TestZeroCostIsStillCharged(t *testing.T) {
	u := loadFixture(t, Options{Steps: 10_000}, "fuelzero", 0, -1)
	if got := u.Call(nil); !got.IsError() {
		t.Fatalf("fuelzero returned %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonStepLimit)
}

// TestWallClockBackstop covers the guest the step budget cannot see: hardspin
// loops forever and never yields, so nothing cooperative can stop it.
//
// The assertion is that the call RETURNS, with no upper bound asserted on how
// long it took -- the timing is the part that is not reproducible, which is the
// whole reason this is the backstop and not the contract. It also asserts the
// reason is the clock and not the budget, so the two can never be conflated by a
// later change that routes one into the other.
func TestWallClockBackstop(t *testing.T) {
	u := loadFixture(t, Options{Timeout: 250 * time.Millisecond}, "hardspin", 0, -1)
	got := u.Call(nil)
	if !got.IsError() {
		t.Fatalf("hardspin returned %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonTimeout)
}

// TestTimeoutDisabledStillCalls covers the Timeout < 0 branch, where no deadline
// context is derived at all. A guest that terminates is unaffected; one that does
// not would hang, which is what that setting says the caller accepts.
func TestTimeoutDisabledStillCalls(t *testing.T) {
	u := loadFixture(t, Options{Timeout: -1}, "tagecho", 1, 1)
	if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(21)})); n != 42 {
		t.Errorf("tagecho(21) = %d, want 42", n)
	}
}

// TestMemoryLimitRejectsAtLoad -- a module wanting more memory than the cap is
// refused before it is ever called, so the limit costs nothing per record and the
// operator finds out at configuration time.
func TestMemoryLimitRejectsAtLoad(t *testing.T) {
	err := loadErr(t, Options{MemoryLimitPages: 128}, "bigmem")
	if !strings.Contains(err.Error(), "bigmem") {
		t.Errorf("error does not name the function: %v", err)
	}
}

// TestRegionMustFitInMemoryLimit -- the host's call frame needs pages too, and a
// cap that leaves no room for it is a load error rather than an ERROR per call.
func TestRegionMustFitInMemoryLimit(t *testing.T) {
	l := newLoader(t, Options{MemoryLimitPages: 1, RegionBytes: 128 * 1024})
	_, err := l.Load(context.Background(), "identity", 1, 1, fixture(t, "identity"))
	if err == nil {
		t.Fatal("loading succeeded with no room for the call region")
	}
	if !strings.Contains(err.Error(), "grow memory") {
		t.Errorf("error does not say what went wrong: %v", err)
	}
}

// TestArgumentsTooLargeForRegion -- a string argument bigger than the host's
// window is ERROR, not a truncated argument. Truncation would hand the guest a
// different string than the expression contained and return a confident wrong
// answer.
func TestArgumentsTooLargeForRegion(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 1, 1)
	big := strings.Repeat("x", 200_000)
	if got := u.Call([]classad.Value{classad.NewStringValue(big)}); !got.IsError() {
		t.Errorf("got %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonArgsTooLarge)
}

// TestTooManyArgumentsForRegion covers the other way the frame can overflow: the
// slots themselves, before any string bytes.
func TestTooManyArgumentsForRegion(t *testing.T) {
	u := loadFixture(t, Options{}, "tagecho", 0, -1)
	args := make([]classad.Value, 20_000)
	for i := range args {
		args[i] = classad.NewIntValue(int64(i))
	}
	if got := u.Call(args); !got.IsError() {
		t.Errorf("got %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonArgsTooLarge)
}

// TestResultStringCapped -- a result over MaxResultBytes is ERROR. concat builds
// a string as long as its inputs, so the cap is tested against a guest that
// genuinely produced the bytes rather than one that lied about a length.
func TestResultStringCapped(t *testing.T) {
	u := loadFixture(t, Options{MaxResultBytes: 8}, "concat", 2, 2)
	got := u.Call([]classad.Value{
		classad.NewStringValue("12345"),
		classad.NewStringValue("67890"),
	})
	if !got.IsError() {
		t.Errorf("got %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonBadResult)

	// Just under the cap still works, so the cap is a boundary and not a
	// blanket refusal of strings.
	u2 := loadFixture(t, Options{MaxResultBytes: 10}, "concat", 2, 2)
	if s := mustString(t, u2.Call([]classad.Value{
		classad.NewStringValue("12345"),
		classad.NewStringValue("67890"),
	})); s != "1234567890" {
		t.Errorf("got %q", s)
	}
}
