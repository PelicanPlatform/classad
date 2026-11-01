package wasmudf

import (
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// TestTrapBecomesError -- a trap is not a crash and not a Go error, it is the
// value ERROR. A UDF is called from inside expression evaluation, which a daemon
// runs with no top-level recover.
func TestTrapBecomesError(t *testing.T) {
	u := loadFixture(t, Options{}, "trap", 0, -1)
	if got := u.Call(nil); !got.IsError() {
		t.Fatalf("trap returned %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonTrap)
}

// TestTrapDiscardsInstance -- a guest stopped mid-instruction has whatever state
// that instruction left in its memory, so its instance is dropped rather than
// handed to the next record. The instance counter is what proves it: three failed
// calls means three instantiations, where a reused instance would mean one.
func TestTrapDiscardsInstance(t *testing.T) {
	u := loadFixture(t, Options{}, "trap", 0, -1)
	for range 3 {
		u.Call(nil)
	}
	if s := u.Stats(); s.Instances != 3 {
		t.Errorf("created %d instances for 3 trapping calls, want 3 (a trapped instance was reused)", s.Instances)
	}
	requireFullSemaphore(t, u)
}

// TestSuccessfulCallsReuseInstance is the other half of the same property, and
// the one the design is for: a compiled module is instantiated once and reused
// across records. If this ever reads 200, every record is paying an
// instantiation.
func TestSuccessfulCallsReuseInstance(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)
	for i := range 200 {
		u.Call([]classad.Value{classad.NewIntValue(int64(i))})
	}
	s := u.Stats()
	if s.Instances != 1 {
		t.Errorf("created %d instances for 200 calls, want 1", s.Instances)
	}
	if s.Calls != 200 {
		t.Errorf("counted %d calls, want 200", s.Calls)
	}
}

// TestStaleResultSlotIsNotReturned is the poison tag's reason to exist, and it
// has to be written as a SEQUENCE on one instance: the first call leaves a
// perfectly good integer in the result slot, and the second -- a string argument,
// for which the guest writes nothing and reports success -- must not come back
// with it.
//
// Without the poison the second call returns the integer 7 for the input "x",
// reports success, and counts no failure. The assertion that it is ERROR is
// therefore an assertion about the shape of bug this guards, not about ERROR.
func TestStaleResultSlotIsNotReturned(t *testing.T) {
	u := loadFixture(t, Options{}, "stale", 1, 1)

	if n := mustInt(t, u.Call([]classad.Value{classad.NewIntValue(7)})); n != 7 {
		t.Fatalf("precondition: stale(7) = %d, want 7", n)
	}
	got := u.Call([]classad.Value{classad.NewStringValue("x")})
	if !got.IsError() {
		t.Fatalf(`stale("x") = %v, want ERROR -- the previous call's answer was returned for this record`, got)
	}
	requireReason(t, u, ReasonBadResult)

	// And the instance was reused, so the slot really was the same bytes: a
	// fresh instance per call would have made this pass for the wrong reason.
	if s := u.Stats(); s.Instances != 1 {
		t.Errorf("created %d instances, want 1", s.Instances)
	}
}

// TestNonzeroStatusIsError -- the status says whether a value was written. A
// guest that declines does not have to encode ERROR itself.
func TestNonzeroStatusIsError(t *testing.T) {
	u := loadFixture(t, Options{}, "status", 0, -1)
	if got := u.Call(nil); !got.IsError() {
		t.Fatalf("got %v, want ERROR", got.Type())
	}
	requireReason(t, u, ReasonStatus)
}

// TestStatusInstanceIsKept -- a guest that merely declined is not a guest that
// was interrupted, so its instance stays in the pool. Discarding it would make a
// UDF that returns "no answer" for most records pay an instantiation for each.
func TestStatusInstanceIsKept(t *testing.T) {
	u := loadFixture(t, Options{}, "status", 0, -1)
	for range 5 {
		u.Call(nil)
	}
	if s := u.Stats(); s.Instances != 1 {
		t.Errorf("created %d instances, want 1", s.Instances)
	}
}

func TestMalformedResults(t *testing.T) {
	for _, tc := range []struct {
		fixture string
		why     string
	}{
		{"badtag", "a tag that is not a value"},
		{"badbool", "a boolean payload above 1"},
		{"oobstring", "a string pointer past the end of linear memory"},
		{"highptr", "a string pointer with bits above 32"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			u := loadFixture(t, Options{}, tc.fixture, 0, -1)
			if got := u.Call(nil); !got.IsError() {
				t.Fatalf("%s returned %v, want ERROR (%s)", tc.fixture, got, tc.why)
			}
			requireReason(t, u, ReasonBadResult)
		})
	}
}

// TestUnsupportedArgumentsRefusedBeforeTheGuestRuns uses the TRAPPING fixture on
// purpose. If the host passed a list to the guest, the call would come back as
// ReasonTrap; the assertion that it is ReasonUnsupportedArg is therefore an
// assertion that the guest never ran.
//
// That ordering is the point. A UDF receives no evaluator precisely so it cannot
// materialize a lazy list, and an ABI that flattened one on the way in would put
// the materialization back where the signature removed it.
func TestUnsupportedArgumentsRefusedBeforeTheGuestRuns(t *testing.T) {
	ad := classad.New()
	for _, arg := range []classad.Value{
		classad.NewListValue([]classad.Value{classad.NewIntValue(1), classad.NewIntValue(2)}),
		classad.NewListValue(nil),
		classad.NewClassAdValue(ad),
	} {
		u := loadFixture(t, Options{}, "trap", 1, 1)
		if got := u.Call([]classad.Value{arg}); !got.IsError() {
			t.Errorf("%v returned %v, want ERROR", arg.Type(), got.Type())
		}
		requireReason(t, u, ReasonUnsupportedArg)
		if s := u.Stats(); s.Instances != 1 {
			t.Errorf("an argument refused before the call cost %d instances, want 1", s.Instances)
		}
	}
}

// TestArityRefused -- Call is exported, so it checks the range the registry
// would have checked. The instance counter asserts the guest was not reached.
func TestArityRefused(t *testing.T) {
	u := loadFixture(t, Options{}, "trap", 2, 3)
	for _, n := range []int{0, 1, 4} {
		args := make([]classad.Value, n)
		for i := range args {
			args[i] = classad.NewIntValue(1)
		}
		before := u.Stats().ByReason[ReasonArity]
		if got := u.Call(args); !got.IsError() {
			t.Errorf("%d arguments returned %v, want ERROR", n, got.Type())
		}
		if after := u.Stats().ByReason[ReasonArity]; after != before+1 {
			t.Errorf("%d arguments did not count as an arity failure", n)
		}
	}
	if s := u.Stats(); s.Instances != 1 {
		t.Errorf("arity-refused calls cost %d instances, want 1", s.Instances)
	}
}

// TestVariadicAcceptsAnyCount -- maxArgs < 0 is variadic, which the arity check
// must not read as "at most -1 arguments".
func TestVariadicAcceptsAnyCount(t *testing.T) {
	u := loadFixture(t, Options{}, "fuelcount", 0, -1)
	for _, n := range []int{0, 1, 5} {
		args := make([]classad.Value, n)
		for i := range args {
			args[i] = classad.NewIntValue(1)
		}
		if got := u.Call(args); got.IsError() {
			t.Errorf("%d arguments returned ERROR", n)
		}
	}
	if s := u.Stats(); s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestOnFailureNamesTheReason -- the counters say how often and this says which.
// A UDF failing on every record is otherwise a column of ERRORs and a number.
func TestOnFailureNamesTheReason(t *testing.T) {
	type report struct {
		name string
		r    Reason
		err  error
	}
	var got []report
	u := loadFixture(t, Options{OnFailure: func(name string, r Reason, err error) {
		got = append(got, report{name, r, err})
	}}, "trap", 0, -1)

	u.Call(nil)
	if len(got) != 1 {
		t.Fatalf("OnFailure called %d times, want 1", len(got))
	}
	if got[0].name != "trap" || got[0].r != ReasonTrap {
		t.Errorf("OnFailure got %q, %v", got[0].name, got[0].r)
	}
	if got[0].err == nil {
		t.Error("OnFailure got a nil error for a trap; the trap's message is the only thing that says which trap")
	}
}

// TestOnFailureSilentForGuestError -- a guest returning the ClassAd value ERROR
// decided its answer. Reporting that as a failure would send an operator looking
// for a broken module that is working as written.
func TestOnFailureSilentForGuestError(t *testing.T) {
	calls := 0
	u := loadFixture(t, Options{OnFailure: func(string, Reason, error) { calls++ }}, "identity", 1, 1)
	if got := u.Call([]classad.Value{classad.NewErrorValue()}); !got.IsError() {
		t.Fatalf("got %v", got.Type())
	}
	if calls != 0 {
		t.Errorf("OnFailure fired %d times for a guest-returned ERROR", calls)
	}
}

func TestReasonStrings(t *testing.T) {
	for r := range reasonCount {
		if Reason(r).String() == "" {
			t.Errorf("Reason(%d) has no name", r)
		}
	}
	if got := Reason(250).String(); got != "unknown" {
		t.Errorf("Reason(250) = %q, want %q", got, "unknown")
	}
}
