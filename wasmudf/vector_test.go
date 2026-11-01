package wasmudf

import (
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// realBits and realOf move a float through the int64 payload the way both the
// engine and classad.Value already store one.
func realBits(f float64) int64 { return int64(math.Float64bits(f)) }
func realOf(x int64) float64   { return math.Float64frombits(uint64(x)) }

// dataBatch builds a data-form column from (state, payload) pairs.
func dataBatch(st []uint8, vals []int64) Batch {
	return Batch{N: len(st), St: st, I: vals}
}

// intBatch builds a data-form column of integers.
func intBatch(vals ...int64) Batch {
	st := make([]uint8, len(vals))
	for i := range st {
		st[i] = vsInt
	}
	return dataBatch(st, vals)
}

// constIntBatch builds a one-element const column standing for n records.
func constIntBatch(v int64, n int) Batch {
	return Batch{N: n, Const: true, St: []uint8{vsInt}, I: []int64{v}}
}

// maskBatch builds a mask-form column from per-record mask states.
func maskBatch(states []int) Batch {
	b := Batch{N: len(states), Mask: true,
		Hi: make([]uint64, maskWords(len(states))),
		Lo: make([]uint64, maskWords(len(states)))}
	for i, s := range states {
		w, bit := i/64, uint(i%64)
		if s&2 != 0 {
			b.Hi[w] |= 1 << bit
		}
		if s&1 != 0 {
			b.Lo[w] |= 1 << bit
		}
	}
	return b
}

// outBatch is the result column, allocated the way the engine's result vector is.
func outBatch(n int) *Batch {
	return &Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
}

func wantInts(t *testing.T, out *Batch, want []int64) {
	t.Helper()
	for i, w := range want {
		if out.St[i] != vsInt {
			t.Errorf("element %d: state %d, want integer", i, out.St[i])
			continue
		}
		if out.I[i] != w {
			t.Errorf("element %d: %d, want %d", i, out.I[i], w)
		}
	}
}

// wantAllError asserts every element is ERROR, which is what a failed vector
// call leaves behind.
func wantAllError(t *testing.T, out *Batch, n int) {
	t.Helper()
	for i := range n {
		if out.St[i] != vsError {
			t.Errorf("element %d: state %d, want ERROR", i, out.St[i])
		}
	}
}

func TestCallBatchTypedPath(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdouble", 1, 1)
	in := intBatch(1, 2, 3, 4, 5, -6, 1<<40)
	out := outBatch(in.N)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("CallBatch declined a batch it should handle")
	}
	wantInts(t, out, []int64{2, 4, 6, 8, 10, -12, 2 << 40})

	s := u.Stats()
	if s.Batches != 1 {
		t.Errorf("Batches = %d, want 1", s.Batches)
	}
	// Every element was an integer, so the typed path took all of them and the
	// escape hatch never fired. A nonzero Escalated here would mean the vector
	// entry point is being bypassed for data it claims to handle.
	if s.Escalated != 0 {
		t.Errorf("Escalated = %d, want 0", s.Escalated)
	}
	if s.Calls != 0 {
		t.Errorf("Calls = %d, want 0: no record should have gone through the scalar path", s.Calls)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestCallBatchDeclinesWithoutVectorEntry -- a module with only the scalar entry
// point is an ordinary UDF, so the batch declines and the caller falls back. It
// is not a failure and must not be counted as one: an operator reading the
// counters would otherwise see every scalar UDF reporting a problem per block.
func TestCallBatchDeclinesWithoutVectorEntry(t *testing.T) {
	u := loadFixture(t, Options{}, "identity", 1, 1)
	if u.Vectored() {
		t.Fatal("identity reports a vector entry point it does not have")
	}
	in := intBatch(1, 2, 3)
	if u.CallBatch([]Batch{in}, outBatch(3)) {
		t.Error("CallBatch handled a batch for a module with no vector entry point")
	}
	if s := u.Stats(); s.Failures != 0 || s.Batches != 0 {
		t.Errorf("a decline was counted: failures %v batches %d", s.ByReason, s.Batches)
	}
}

// TestEscapeHatchUsesTheScalarResult is the escape hatch's central property.
//
// vecdecline declines odd values and leaves a state of 7 -- not a value at all --
// in their slots, which is what a guest is entitled to do with a slot it has told
// the host to ignore. So a host that read declined slots would reject this entire
// batch as malformed, and the assertion that every element is x*2 is an assertion
// both that the declined records came from the scalar entry point and that their
// slots were never looked at.
func TestEscapeHatchUsesTheScalarResult(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdecline", 1, 1)
	in := intBatch(0, 1, 2, 3, 4, 5)
	out := outBatch(in.N)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("CallBatch declined")
	}
	wantInts(t, out, []int64{0, 2, 4, 6, 8, 10})
	for i := range in.N {
		if out.I[i] == 999 {
			t.Errorf("element %d came from the declined slot", i)
		}
	}

	s := u.Stats()
	if s.Escalated != 3 {
		t.Errorf("Escalated = %d, want 3 (the odd values)", s.Escalated)
	}
	if s.Calls != 3 {
		t.Errorf("Calls = %d, want 3: one scalar call per escalated record", s.Calls)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestEscapeHatchServesNonNumericStates -- the states a typed vector path would
// have to contort itself to express. vecdouble's vector entry handles integers
// only, so a real, an UNDEFINED and an ERROR all escalate, and each gets what the
// complete scalar implementation says: 2.5 doubles, UNDEFINED passes through,
// ERROR passes through.
//
// This is what the escape hatch buys. Without it the guest would have to
// implement every operand state in branch-free code to vectorize any of them.
func TestEscapeHatchServesNonNumericStates(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdouble", 1, 1)
	in := dataBatch(
		[]uint8{vsInt, vsReal, vsUndef, vsError, vsInt},
		[]int64{21, realBits(2.5), 0, 0, 3})
	out := outBatch(in.N)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("CallBatch declined")
	}
	if out.St[0] != vsInt || out.I[0] != 42 {
		t.Errorf("element 0 = (%d,%d), want integer 42", out.St[0], out.I[0])
	}
	if out.St[1] != vsReal || realOf(out.I[1]) != 5.0 {
		t.Errorf("element 1 = (%d,%v), want real 5", out.St[1], realOf(out.I[1]))
	}
	if out.St[2] != vsUndef {
		t.Errorf("element 2 state %d, want UNDEFINED", out.St[2])
	}
	if out.St[3] != vsError {
		t.Errorf("element 3 state %d, want ERROR", out.St[3])
	}
	if out.St[4] != vsInt || out.I[4] != 6 {
		t.Errorf("element 4 = (%d,%d), want integer 6", out.St[4], out.I[4])
	}
	if s := u.Stats(); s.Escalated != 3 {
		t.Errorf("Escalated = %d, want 3", s.Escalated)
	}
}

// TestDeclineMaskIsClearedBetweenBatches -- vecdecline declines by VALUE, so the
// declined set changes from batch to batch. A mask left over from the previous
// call escalates records this guest handled: not a wrong answer, since the scalar
// path computes the same thing, but a different PATH taken on the strength of
// history, and a stale full mask turns the vector entry point into dead weight.
//
// The counter is what sees it. The values cannot.
func TestDeclineMaskIsClearedBetweenBatches(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdecline", 1, 1)

	odd := intBatch(1, 3, 5, 7)
	if !u.CallBatch([]Batch{odd}, outBatch(4)) {
		t.Fatal("CallBatch declined the odd batch")
	}
	if s := u.Stats(); s.Escalated != 4 {
		t.Fatalf("Escalated = %d after four odd values, want 4", s.Escalated)
	}

	even := intBatch(2, 4, 6, 8)
	out := outBatch(4)
	if !u.CallBatch([]Batch{even}, out) {
		t.Fatal("CallBatch declined the even batch")
	}
	wantInts(t, out, []int64{4, 8, 12, 16})
	if s := u.Stats(); s.Escalated != 4 {
		t.Errorf("Escalated = %d after a batch that declines nothing, want still 4", s.Escalated)
	}
}

// TestDeclineEverything -- the escape hatch at its limit. Correct, and the
// counters say plainly that the vector entry point earned nothing.
func TestDeclineEverything(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdeclineall", 1, 1)
	in := intBatch(1, 2, 3, 4)
	out := outBatch(4)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("CallBatch declined")
	}
	wantInts(t, out, []int64{101, 102, 103, 104})
	s := u.Stats()
	if s.Escalated != 4 || s.Calls != 4 {
		t.Errorf("Escalated %d Calls %d, want 4 and 4", s.Escalated, s.Calls)
	}
}

// TestMaskFormArgumentPassesThrough -- the bitplanes reach the guest as
// bitplanes. The states are chosen to cover all four, so a host that converted
// the mask form into the data form on the way in would be caught by the guest
// refusing the call (vecmaskarg requires form 1) rather than by a wrong answer.
func TestMaskFormArgumentPassesThrough(t *testing.T) {
	u := loadFixture(t, Options{}, "vecmaskarg", 1, 1)
	in := maskBatch([]int{mkTrue, mkFalse, mkUndef, mkError, mkTrue, mkFalse})
	out := outBatch(in.N)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatalf("CallBatch declined a mask batch: %v", u.Stats().ByReason)
	}
	want := []int64{1, 0, 0, 0, 1, 0}
	for i, w := range want {
		if out.St[i] != vsBool || out.I[i] != w {
			t.Errorf("element %d = (%d,%d), want boolean %d", i, out.St[i], out.I[i], w)
		}
	}
	if s := u.Stats(); s.Escalated != 0 {
		t.Errorf("Escalated = %d, want 0", s.Escalated)
	}
}

// TestMaskFormWideBatch runs a mask batch past a word boundary, because the
// bitplane indexing is the one place in the ABI where 64 records share a word and
// an off-by-one in the shift is invisible in a batch smaller than one.
func TestMaskFormWideBatch(t *testing.T) {
	const n = 200
	states := make([]int, n)
	for i := range states {
		switch i % 4 {
		case 0:
			states[i] = mkTrue
		case 1:
			states[i] = mkFalse
		case 2:
			states[i] = mkUndef
		default:
			states[i] = mkError
		}
	}
	u := loadFixture(t, Options{}, "vecmaskarg", 1, 1)
	out := outBatch(n)
	if !u.CallBatch([]Batch{maskBatch(states)}, out) {
		t.Fatalf("CallBatch declined: %v", u.Stats().ByReason)
	}
	for i := range n {
		want := int64(0)
		if states[i] == mkTrue {
			want = 1
		}
		if out.I[i] != want {
			t.Errorf("element %d = %d, want %d", i, out.I[i], want)
		}
	}
}

// TestConstArgumentIsNotMaterialized -- vecconst refuses the call unless argument
// 0 arrives with the const flag set, so this asserts a property of the HOST that
// no result value could show: that a literal argument crosses the boundary as one
// element and a flag rather than n identical copies.
func TestConstArgumentIsNotMaterialized(t *testing.T) {
	u := loadFixture(t, Options{}, "vecconst", 2, 2)
	k := constIntBatch(10, 5)
	xs := intBatch(1, 2, 3, 4, 5)
	out := outBatch(5)

	if !u.CallBatch([]Batch{k, xs}, out) {
		t.Fatalf("CallBatch declined: %v", u.Stats().ByReason)
	}
	wantInts(t, out, []int64{11, 12, 13, 14, 15})
	if s := u.Stats(); s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestAllConstBatchIsOneScalarCall -- when nothing varies per record there is one
// answer, so there is one call, and the result column is left const. The guest's
// vector entry point is never reached, which is why vecconst (whose vector path
// refuses a const argument 1) can serve this at all.
func TestAllConstBatchIsOneScalarCall(t *testing.T) {
	u := loadFixture(t, Options{}, "vecconst", 2, 2)
	out := outBatch(1000)
	if !u.CallBatch([]Batch{constIntBatch(10, 1000), constIntBatch(5, 1000)}, out) {
		t.Fatalf("CallBatch declined: %v", u.Stats().ByReason)
	}
	if !out.Const {
		t.Error("the result of an all-const batch is not const")
	}
	if out.St[0] != vsInt || out.I[0] != 15 {
		t.Errorf("element 0 = (%d,%d), want integer 15", out.St[0], out.I[0])
	}
	s := u.Stats()
	if s.Calls != 1 {
		t.Errorf("Calls = %d, want 1 for a thousand records that cannot differ", s.Calls)
	}
	if s.Batches != 0 {
		t.Errorf("Batches = %d, want 0: the vector entry point should not have been reached", s.Batches)
	}
}

// TestVectorTrapIsErrorForTheWholeBatchAndEscalatesNothing pins the rule that a
// dead guest's decline mask is not evidence. vectrap's scalar entry point works
// perfectly and returns 42, so a host that escalated on the strength of the mask
// would produce 42s here; every element must be ERROR instead.
func TestVectorTrapIsErrorForTheWholeBatchAndEscalatesNothing(t *testing.T) {
	u := loadFixture(t, Options{}, "vectrap", 1, 1)
	in := intBatch(1, 2, 3, 4)
	out := outBatch(4)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("a trapping vector call declined instead of answering ERROR")
	}
	wantAllError(t, out, 4)
	requireReason(t, u, ReasonTrap)
	s := u.Stats()
	if s.Escalated != 0 {
		t.Errorf("Escalated = %d, want 0: the decline mask of a dead guest is not evidence", s.Escalated)
	}
	if s.Calls != 0 {
		t.Errorf("Calls = %d, want 0", s.Calls)
	}
}

// TestVectorTrapDiscardsTheInstance -- the same rule as the scalar path: a guest
// stopped mid-instruction does not get to compute the next batch.
func TestVectorTrapDiscardsTheInstance(t *testing.T) {
	u := loadFixture(t, Options{}, "vectrap", 1, 1)
	for range 3 {
		u.CallBatch([]Batch{intBatch(1)}, outBatch(1))
	}
	if s := u.Stats(); s.Instances != 3 {
		t.Errorf("created %d instances for 3 trapping batches, want 3", s.Instances)
	}
	requireFullSemaphore(t, u)
}

// TestVectorSilentGuestIsCaught -- the poison, batch-wide. Nothing was written,
// so nothing may be read, and the column must not be whatever the last batch put
// there.
func TestVectorSilentGuestIsCaught(t *testing.T) {
	u := loadFixture(t, Options{}, "vecsilent", 1, 1)
	out := outBatch(4)
	if !u.CallBatch([]Batch{intBatch(1, 2, 3, 4)}, out) {
		t.Fatal("declined")
	}
	wantAllError(t, out, 4)
	requireReason(t, u, ReasonBadResult)
}

// TestVectorStatusIgnoresTheDeclineMask -- vecstatus writes a complete result
// column, sets every decline bit, and then says the call failed. Nothing may be
// escalated and nothing read: the status is what says whether any of it means
// anything.
func TestVectorStatusIgnoresTheDeclineMask(t *testing.T) {
	u := loadFixture(t, Options{}, "vecstatus", 1, 1)
	out := outBatch(4)
	if !u.CallBatch([]Batch{intBatch(1, 2, 3, 4)}, out) {
		t.Fatal("declined")
	}
	wantAllError(t, out, 4)
	requireReason(t, u, ReasonStatus)
	if s := u.Stats(); s.Escalated != 0 || s.Calls != 0 {
		t.Errorf("Escalated %d Calls %d, want 0 and 0", s.Escalated, s.Calls)
	}
}

func TestVectorMalformedResults(t *testing.T) {
	for _, tc := range []struct{ fixture, why string }{
		{"vecbadstate", "a state code the engine does not have"},
		{"vecstring", "a string state with no string storage behind it"},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			u := loadFixture(t, Options{}, tc.fixture, 1, 1)
			out := outBatch(3)
			if !u.CallBatch([]Batch{intBatch(1, 2, 3)}, out) {
				t.Fatal("declined")
			}
			wantAllError(t, out, 3)
			requireReason(t, u, ReasonBadResult)
		})
	}
}

// TestStringArgumentDeclinesTheBatch -- the vector ABI carries no strings, so a
// string column declines up front. Escalating them one at a time instead would be
// the elementwise path with a vector call in front of it, since a string column is
// strings for every record.
func TestStringArgumentDeclinesTheBatch(t *testing.T) {
	for _, st := range []uint8{vsString, vsDictCode} {
		u := loadFixture(t, Options{}, "vecdouble", 1, 1)
		in := dataBatch([]uint8{vsInt, st, vsInt}, []int64{1, 0, 3})
		if u.CallBatch([]Batch{in}, outBatch(3)) {
			t.Errorf("state %d: CallBatch handled a batch holding strings", st)
		}
		requireReason(t, u, ReasonBatchStrings)
	}
}

// TestBatchTooLargeDeclines -- a batch wider than the region was sized for is a
// decline with a counted reason, never a silent fallback. A vector path that has
// turned itself off is otherwise visible only as a query being slow.
func TestBatchTooLargeDeclines(t *testing.T) {
	u := loadFixture(t, Options{MaxBatch: 8}, "vecdouble", 1, 1)
	if u.CallBatch([]Batch{intBatch(1, 2, 3, 4, 5, 6, 7, 8, 9)}, outBatch(9)) {
		t.Error("CallBatch handled a batch above MaxBatch")
	}
	requireReason(t, u, ReasonBatchTooLarge)

	// Exactly at the limit still works, so MaxBatch is a boundary and not an
	// approximate refusal.
	out := outBatch(8)
	if !u.CallBatch([]Batch{intBatch(1, 2, 3, 4, 5, 6, 7, 8)}, out) {
		t.Fatalf("a batch of exactly MaxBatch declined: %v", u.Stats().ByReason)
	}
	wantInts(t, out, []int64{2, 4, 6, 8, 10, 12, 14, 16})
}

func TestBatchShapeRefusals(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdouble", 1, 1)
	good := intBatch(1, 2, 3)

	for _, tc := range []struct {
		name string
		args []Batch
		out  *Batch
	}{
		{"nil out", []Batch{good}, nil},
		{"mask-form out", []Batch{good}, &Batch{N: 3, Mask: true,
			Hi: make([]uint64, 1), Lo: make([]uint64, 1)}},
		{"short out", []Batch{good}, &Batch{N: 3, St: make([]uint8, 1), I: make([]int64, 1)}},
		{"argument count disagrees with out", []Batch{intBatch(1, 2)}, outBatch(3)},
		{"column shorter than it claims", []Batch{{N: 3, St: make([]uint8, 2), I: make([]int64, 3)}}, outBatch(3)},
		{"too many arguments", []Batch{good, good}, outBatch(3)},
		{"too few arguments", nil, outBatch(3)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := u.Stats().ByReason[ReasonBatchShape]
			if u.CallBatch(tc.args, tc.out) {
				t.Fatal("CallBatch accepted a malformed batch")
			}
			if after := u.Stats().ByReason[ReasonBatchShape]; after != before+1 {
				t.Errorf("not counted as a shape refusal (%d -> %d)", before, after)
			}
		})
	}
}

// TestVectorAndScalarAgree is the differential test between the two entry points
// of one module: every input that both accept must produce the same value.
//
// It is the test that makes the escape hatch trustworthy. A vectorized guest is
// two implementations of one function, and the only thing stopping them from
// disagreeing is somebody checking.
func TestVectorAndScalarAgree(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdouble", 1, 1)

	states := []uint8{vsInt, vsReal, vsBool, vsUndef, vsError}
	payloads := []int64{0, 1, -1, 7, 1 << 40, realBits(2.5), realBits(-0.5)}

	var st []uint8
	var vals []int64
	var want []classad.Value
	for _, s := range states {
		for _, p := range payloads {
			st = append(st, s)
			vals = append(vals, p)
			one := dataBatch([]uint8{s}, []int64{p})
			want = append(want, u.Call([]classad.Value{one.value(0)}))
		}
	}

	out := outBatch(len(st))
	if !u.CallBatch([]Batch{dataBatch(st, vals)}, out) {
		t.Fatalf("CallBatch declined: %v", u.Stats().ByReason)
	}
	for i := range want {
		got := (&Batch{N: len(st), St: out.St, I: out.I}).value(i)
		if got.String() != want[i].String() {
			t.Errorf("element %d (state %d payload %d): vector %v, scalar %v",
				i, st[i], vals[i], got, want[i])
		}
	}
}

// TestEscapeHatchInstanceDiesMidBatch -- an escalated record can trap, and when
// it does the instance is gone. The records already escalated keep the answers
// they got; the trapping record and everything after it become ERROR.
//
// Carrying on with a fresh instance would be the tempting thing and the wrong
// one: the remaining records would be computed by a guest that has not run the
// ones before them, which for a guest keeping state in its memory is a different
// function. Re-filling the whole column with ERROR would be wrong in the other
// direction -- it would discard answers that were correctly computed.
func TestEscapeHatchInstanceDiesMidBatch(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdeclinetrap", 1, 1)
	in := intBatch(2, 4, 3, 6)
	out := outBatch(4)

	if !u.CallBatch([]Batch{in}, out) {
		t.Fatalf("declined: %v", u.Stats().ByReason)
	}
	if out.St[0] != vsInt || out.I[0] != 102 {
		t.Errorf("element 0 = (%d,%d), want integer 102", out.St[0], out.I[0])
	}
	if out.St[1] != vsInt || out.I[1] != 104 {
		t.Errorf("element 1 = (%d,%d), want integer 104", out.St[1], out.I[1])
	}
	for i := 2; i < 4; i++ {
		if out.St[i] != vsError {
			t.Errorf("element %d state %d, want ERROR after the instance died", i, out.St[i])
		}
	}
	if s := u.Stats(); s.ByReason[ReasonTrap] != 1 {
		t.Errorf("got %v, want one trap", s.ByReason)
	}
	requireFullSemaphore(t, u)
}

// TestTrustedUsesASeparateRuntime is a white-box assertion, and it is the only
// way to see the property at all: interruptibility is compiled INTO a module, so
// a trusted module put in the interruptible runtime would still compute the right
// answers and would simply be slow. There is no value any test could read that
// would tell them apart.
func TestTrustedUsesASeparateRuntime(t *testing.T) {
	l := newLoader(t, Options{})
	plain, err := l.Load(t.Context(), "plain", 1, 1, fixture(t, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := l.LoadWith(t.Context(), "fast", 1, 1, fixture(t, "identity"),
		LoadOptions{Trusted: true})
	if err != nil {
		t.Fatal(err)
	}
	if plain.rt == fast.rt {
		t.Fatal("trusted and untrusted modules were compiled into the same runtime, so one of them has the wrong interruption mode")
	}
	if plain.rt != l.rts[0] {
		t.Error("an untrusted module is not in the interruptible runtime")
	}
	if fast.rt != l.rts[1] {
		t.Error("a trusted module is not in the non-interruptible runtime")
	}

	// And no deadline is derived for it. This has to be asserted on the field
	// rather than on behaviour, because a deadline over a guest that cannot be
	// interrupted is already inert: wazero never looks at the context, so a
	// trusted module with a one-nanosecond timeout still completes. Leaving the
	// timeout set would cost a context.WithTimeout per call -- ~290ns, on the one
	// path whose whole purpose is not to pay that -- and no test reading values
	// could tell.
	if fast.timeout >= 0 {
		t.Errorf("a trusted module has timeout %v; it should derive no deadline at all", fast.timeout)
	}
	if plain.timeout < 0 {
		t.Error("an untrusted module lost its deadline")
	}
}

// TestConstArgumentSurvivesEscalation -- a literal argument in a batch where some
// records go through the escape hatch.
//
// A const column holds ONE element however many records it stands for, so boxing
// a declined record's arguments has to read element 0 of it and the record's own
// index of the others. Indexing it by record number reads off the end of a
// one-element slice, which is the kind of mistake that only shows up when both
// features are used at once.
func TestConstArgumentSurvivesEscalation(t *testing.T) {
	u := loadFixture(t, Options{}, "vecconstdecline", 2, 2)
	k := constIntBatch(10, 6)
	xs := intBatch(1, 2, 3, 4, 5, 6)
	out := outBatch(6)

	if !u.CallBatch([]Batch{k, xs}, out) {
		t.Fatalf("declined: %v", u.Stats().ByReason)
	}
	wantInts(t, out, []int64{11, 12, 13, 14, 15, 16})
	s := u.Stats()
	if s.Escalated != 3 {
		t.Errorf("Escalated = %d, want 3 (the odd values)", s.Escalated)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestLargeBatchFitsItsRegion -- the region is sized at load from MaxBatch, so a
// configuration that asks for big batches gets memory for them. Without that
// sizing the frame outgrows the default region and every batch declines: correct,
// and slower than not having a vector entry point at all.
func TestLargeBatchFitsItsRegion(t *testing.T) {
	const n = 16384
	u := loadFixture(t, Options{MaxBatch: n}, "vecdouble", 1, 1)
	st := make([]uint8, n)
	vals := make([]int64, n)
	for i := range n {
		st[i], vals[i] = vsInt, int64(i)
	}
	out := outBatch(n)
	if !u.CallBatch([]Batch{dataBatch(st, vals)}, out) {
		t.Fatalf("a batch of %d declined: %v", n, u.Stats().ByReason)
	}
	for i := range n {
		if out.St[i] != vsInt || out.I[i] != int64(i)*2 {
			t.Fatalf("element %d = (%d,%d), want %d", i, out.St[i], out.I[i], int64(i)*2)
		}
	}
}

// TestBackstopDisabledSharesTheRuntime -- with the backstop off for the whole
// process there is no deadline for the interruption machinery to deliver, so an
// untrusted module belongs in the same runtime a trusted one would: the mode is
// derived from whether a deadline exists, not from the label on its own.
func TestBackstopDisabledSharesTheRuntime(t *testing.T) {
	l := newLoader(t, Options{Timeout: -1})
	plain, err := l.Load(t.Context(), "plain", 1, 1, fixture(t, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if plain.rt != l.rts[1] {
		t.Error("with no deadline configured, an untrusted module is still paying for interruptibility")
	}
	if l.rts[0] != nil {
		t.Error("an interruptible runtime was built with no deadline to enforce")
	}
}
