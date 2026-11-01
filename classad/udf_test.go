package classad

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

// Registration is process-global and append-only, so every test here uses a name no other test
// uses. That is not a wart of the test: it is the contract, and a test that needed to withdraw a
// registration would be testing something the package does not offer.

func mustRegister(t *testing.T, name string, minArgs, maxArgs int, fn Func) {
	t.Helper()
	if err := RegisterFunction(name, minArgs, maxArgs, fn); err != nil {
		t.Fatalf("RegisterFunction(%q): %v", name, err)
	}
}

// TestUDFIsReachableFromTheEvaluator is the baseline: a registered function is callable from an
// ordinary expression, with the same name folding every function name gets.
func TestUDFIsReachableFromTheEvaluator(t *testing.T) {
	mustRegister(t, "udfTriple", 1, 1, func(args []Value) Value {
		n, err := args[0].IntValue()
		if err != nil {
			return NewErrorValue()
		}
		return NewIntValue(n * 3)
	})
	ad, err := Parse(`[ n = 7 ]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{`udfTriple(n)`, `UDFTRIPLE(n)`, `udftriple(n)`} {
		expr, err := ParseExpr(src)
		if err != nil {
			t.Fatalf("parse %s: %v", src, err)
		}
		if got := expr.Eval(ad).String(); got != "21" {
			t.Errorf("%s = %s, want 21", src, got)
		}
	}
}

// TestUDFIsStrictAndCompilable is the property the whole registration exists for: StrictFunction
// accepts it, which is what makes a compiled program call it natively instead of delegating -- and
// delegating is what takes a query off the columnar accelerators.
func TestUDFIsStrictAndCompilable(t *testing.T) {
	mustRegister(t, "udfLen", 1, 1, func(args []Value) Value {
		s, err := args[0].StringValue()
		if err != nil {
			return NewErrorValue()
		}
		return NewIntValue(int64(len(s)))
	})
	if !StrictFunction("udfLen", 1) {
		t.Error("StrictFunction refused a registered UDF at its declared arity")
	}
	if !StrictFunction("UDFLEN", 1) {
		t.Error("StrictFunction is case-sensitive for UDFs")
	}
	// Arity is enforced the same way it is for a built-in, so a wrong count is delegated and becomes
	// ERROR without the arguments being evaluated.
	if StrictFunction("udfLen", 0) || StrictFunction("udfLen", 2) {
		t.Error("StrictFunction accepted a UDF at the wrong arity")
	}
	// And ApplyFunction -- the compiled path -- reaches it.
	got := ApplyFunction(NewEvaluator(nil), "udfLen", []Value{NewStringValue("abcd")})
	if got.String() != "4" {
		t.Errorf("ApplyFunction = %s, want 4", got)
	}
}

// TestUDFEnginesAgree is the divergence test. A UDF visible to one engine and not the other would be
// exactly the failure this package's differential testing exists to prevent, so both paths are
// compared on the same arguments -- including the states that are values rather than failures.
func TestUDFEnginesAgree(t *testing.T) {
	mustRegister(t, "udfDescribe", 1, 1, func(args []Value) Value {
		switch {
		case args[0].IsUndefined():
			return NewStringValue("undef")
		case args[0].IsError():
			return NewStringValue("err")
		case args[0].IsString():
			return NewStringValue("str")
		default:
			return NewStringValue("other")
		}
	})
	ad, err := Parse(`[ s = "x"; n = 1 ]`)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ src, want string }{
		{`udfDescribe(s)`, `"str"`},
		{`udfDescribe(n)`, `"other"`},
		{`udfDescribe(missing)`, `"undef"`},
		{`udfDescribe(error)`, `"err"`},
		{`udfDescribe(undefined)`, `"undef"`},
	} {
		expr, err := ParseExpr(tc.src)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.src, err)
		}
		viaEvaluator := expr.Eval(ad)
		if viaEvaluator.String() != tc.want {
			t.Errorf("%s via the evaluator = %s, want %s", tc.src, viaEvaluator, tc.want)
		}
		// The compiled path evaluates the argument and then calls; same arguments, same answer.
		argExpr, err := ParseExpr(strings.TrimSuffix(strings.TrimPrefix(tc.src, "udfDescribe("), ")"))
		if err != nil {
			t.Fatalf("parse argument of %s: %v", tc.src, err)
		}
		viaApply := ApplyFunction(NewEvaluator(ad), "udfDescribe", []Value{argExpr.Eval(ad)})
		if viaApply.String() != viaEvaluator.String() {
			t.Errorf("%s: evaluator = %s, ApplyFunction = %s", tc.src, viaEvaluator, viaApply)
		}
	}
}

// TestUDFArityIsCheckedBeforeArguments matches the rule built-ins follow: a wrong argument count is
// ERROR and the arguments are NOT evaluated, so a cyclic argument is never reached.
func TestUDFArityIsCheckedBeforeArguments(t *testing.T) {
	mustRegister(t, "udfTwoArgs", 2, 2, func(args []Value) Value { return NewIntValue(1) })
	ad, err := Parse(`[ A0 = 0 =!= udfTwoArgs((A0)) ]`)
	if err != nil {
		t.Fatal(err)
	}
	if got := ad.EvaluateAttr("A0"); got.String() != "true" {
		t.Errorf("A0 = %s, want true: a wrong-arity call must not evaluate its arguments", got)
	}
}

// TestUDFVariadic covers a negative maxArgs.
func TestUDFVariadic(t *testing.T) {
	mustRegister(t, "udfCount", 1, -1, func(args []Value) Value { return NewIntValue(int64(len(args))) })
	for _, tc := range []struct{ src, want string }{
		{`udfCount(1)`, "1"},
		{`udfCount(1, 2, 3)`, "3"},
		{`udfCount()`, "error"}, // below the minimum
	} {
		expr, err := ParseExpr(tc.src)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.src, err)
		}
		if got := expr.Eval(nil).String(); got != tc.want {
			t.Errorf("%s = %s, want %s", tc.src, got, tc.want)
		}
	}
}

// TestUDFPanicIsContained pins that a misbehaving UDF cannot take down a daemon. This library runs
// inside processes with no top-level recovery, where an escaping panic from one record's arithmetic
// would end a process serving everything else.
//
// Containment is not an invitation to panic: a UDF reports failure by returning ERROR.
func TestUDFPanicIsContained(t *testing.T) {
	mustRegister(t, "udfExplode", 0, 0, func(args []Value) Value { panic("boom") })
	expr, err := ParseExpr(`udfExplode()`)
	if err != nil {
		t.Fatal(err)
	}
	if got := expr.Eval(nil).String(); got != "error" {
		t.Errorf("a panicking UDF produced %s, want error", got)
	}
	// Through the compiled path as well.
	if got := ApplyFunction(NewEvaluator(nil), "udfExplode", nil).String(); got != "error" {
		t.Errorf("ApplyFunction on a panicking UDF produced %s, want error", got)
	}
}

// TestRegisterFunctionErrorsAreDistinguishable pins the sentinels. A loader registering a batch of
// functions acts differently on each: colliding with a built-in is a mistake in what it was asked to
// load, already-registered is usually a double-load it can ignore, and a malformed name or arity is a
// bug in whatever produced them. Matching on the message text instead is not an interface.
func TestRegisterFunctionErrorsAreDistinguishable(t *testing.T) {
	ok := func(args []Value) Value { return NewUndefinedValue() }
	mustRegister(t, "udfSentinelTaken", 0, 0, ok)
	for _, tc := range []struct {
		name    string
		minArgs int
		maxArgs int
		fn      Func
		want    error
	}{
		{"strcat", 0, 1, ok, ErrBuiltinName},
		{"SUBSTR", 1, 1, ok, ErrBuiltinName},
		{"udfSentinelTaken", 0, 0, ok, ErrFunctionExists},
		{"UDFSENTINELTAKEN", 0, 0, ok, ErrFunctionExists},
		{"udfSentinelNil", 0, 0, nil, ErrBadFunction},
		{"9bad", 0, 0, ok, ErrBadFunction},
		{"udfSentinelArity", 3, 1, ok, ErrBadFunction},
	} {
		err := RegisterFunction(tc.name, tc.minArgs, tc.maxArgs, tc.fn)
		if !errors.Is(err, tc.want) {
			t.Errorf("RegisterFunction(%q): err = %v, want errors.Is(..., %v)", tc.name, err, tc.want)
		}
		// The message still names the function, so a log line is useful on its own.
		if err != nil && !strings.Contains(err.Error(), tc.name) {
			t.Errorf("RegisterFunction(%q): message %q does not name the function", tc.name, err)
		}
	}
	// The three are distinct, so a caller can switch on them.
	if errors.Is(ErrBuiltinName, ErrFunctionExists) || errors.Is(ErrFunctionExists, ErrBadFunction) {
		t.Error("the sentinels are not distinct")
	}
}

// TestRegisterFunctionRefuses pins every rejection, each of which would otherwise be a surprise at
// some later call site.
func TestRegisterFunctionRefuses(t *testing.T) {
	ok := func(args []Value) Value { return NewUndefinedValue() }
	for _, tc := range []struct {
		name             string
		minArgs, maxArgs int
		fn               Func
		why              string
	}{
		{"udfNil", 0, 0, nil, "a nil function"},
		{"", 0, 0, ok, "an empty name"},
		{"9lives", 0, 0, ok, "a name starting with a digit"},
		{"has space", 0, 0, ok, "a name an expression cannot call"},
		{"has-dash", 0, 0, ok, "likewise"},
		{"udfBadArity", -1, 1, ok, "a negative minimum"},
		{"udfBadArity2", 3, 1, ok, "a maximum below the minimum"},
		{"strcat", 0, 1, ok, "a built-in's name"},
		{"SUBSTR", 1, 1, ok, "a built-in's name, folded"},
		{"ifThenElse", 3, 3, ok, "a built-in that is not even strict"},
	} {
		if err := RegisterFunction(tc.name, tc.minArgs, tc.maxArgs, tc.fn); err == nil {
			t.Errorf("RegisterFunction(%q) succeeded; want a rejection (%s)", tc.name, tc.why)
		}
	}
	// A duplicate is refused rather than overwriting: silently replacing one would make a call's
	// behaviour depend on package initialization order.
	mustRegister(t, "udfOnce", 0, 0, ok)
	if err := RegisterFunction("udfOnce", 0, 0, ok); err == nil {
		t.Error("re-registering a name succeeded; want a rejection")
	}
	if err := RegisterFunction("UDFONCE", 0, 0, ok); err == nil {
		t.Error("re-registering a name in another case succeeded; want a rejection")
	}
}

// TestUDFRegistryIsConcurrentSafe exercises the copy-on-write publication under the race detector:
// registration while other goroutines are both registering and calling.
func TestUDFRegistryIsConcurrentSafe(t *testing.T) {
	mustRegister(t, "udfConcurrentBase", 0, 0, func(args []Value) Value { return NewIntValue(5) })
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Each goroutine registers its own name; collisions are not the thing under test.
			_ = RegisterFunction("udfRacer"+string(rune('A'+i)), 0, 0,
				func(args []Value) Value { return NewIntValue(int64(i)) })
			for j := 0; j < 200; j++ {
				if got := ApplyFunction(NewEvaluator(nil), "udfConcurrentBase", nil); got.String() != "5" {
					t.Errorf("concurrent call = %s, want 5", got)
					return
				}
				_ = StrictFunction("udfConcurrentBase", 0)
				_ = RegisteredFunctions()
			}
		}(i)
	}
	wg.Wait()
}

// TestRegisteredFunctionsReportsAsRegistered checks the diagnostic surface keeps the spelling the
// caller used, since the folded name is an implementation detail of lookup.
func TestRegisteredFunctionsReportsAsRegistered(t *testing.T) {
	mustRegister(t, "udfMixedCaseName", 0, 0, func(args []Value) Value { return NewIntValue(1) })
	for _, n := range RegisteredFunctions() {
		if n == "udfMixedCaseName" {
			return
		}
	}
	t.Error("RegisteredFunctions did not report the name as registered")
}
