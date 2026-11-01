package wasmudf

import (
	"encoding/binary"
	"fmt"
	"sync"
	"testing"
)

// mixedBatch builds a data-form column of integers with one element put into
// another state, which is the shape the columnar form exists to deal with: a
// column that is one type except where it is not.
func mixedBatch(n int, at int, st uint8, payload int64) Batch {
	b := intBatch(make([]int64, n)...)
	for i := range n {
		b.I[i] = int64(i)
	}
	b.St[at], b.I[at] = st, payload
	return b
}

func TestColumnarVersionIsReported(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	if u.abi != abiVersion2 {
		t.Fatalf("abi = %d, want %d", u.abi, abiVersion2)
	}
	v1 := loadFixture(t, Options{}, "vecdouble", 1, 1)
	if v1.abi != abiVersion1 {
		t.Fatalf("vecdouble abi = %d, want %d", v1.abi, abiVersion1)
	}
}

// TestColumnarUniformColumn is the whole point in one case: a column every
// element of which is an integer crosses as payloads plus one state byte, the
// guest's loop has no per-element branch in it, and nothing is escalated.
func TestColumnarUniformColumn(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	in := intBatch(1, 2, 3, 4, 5, -6, 1<<40)
	out := outBatch(in.N)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined a uniform integer column")
	}
	wantInts(t, out, []int64{2, 4, 6, 8, 10, -12, 2 << 40})
	if s := u.Stats(); s.Escalated != 0 || s.Calls != 0 {
		t.Errorf("Escalated = %d, Calls = %d, want 0 and 0", s.Escalated, s.Calls)
	}
}

// TestColumnarRealColumn -- a column of reals is just as uniform as a column of
// integers, and the guest picks its loop from the one state byte rather than
// deciding per element.
func TestColumnarRealColumn(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	st := []uint8{vsReal, vsReal, vsReal}
	in := dataBatch(st, []int64{realBits(1.5), realBits(-2), realBits(0.25)})
	out := outBatch(3)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined a uniform real column")
	}
	for i, want := range []float64{3, -4, 0.5} {
		if out.St[i] != vsReal || realOf(out.I[i]) != want {
			t.Errorf("element %d: state %d value %v, want real %v", i, out.St[i], realOf(out.I[i]), want)
		}
	}
	if s := u.Stats(); s.Escalated != 0 {
		t.Errorf("Escalated = %d, want 0", s.Escalated)
	}
}

// TestColumnarEscalatesTheException is the user-visible half of the trade: a
// column of integers with one UNDEFINED in it is still a column of integers, and
// the one record that is not gets the scalar entry point's answer rather than a
// state byte of its own.
func TestColumnarEscalatesTheException(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	const n = 200
	in := mixedBatch(n, 7, vsUndef, 0)
	in.St[150], in.I[150] = vsReal, realBits(2.5)
	out := outBatch(n)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined a column with two exceptions in it")
	}
	for i := range n {
		switch i {
		case 7:
			if out.St[i] != vsUndef {
				t.Errorf("element 7: state %d, want UNDEFINED", out.St[i])
			}
		case 150:
			if out.St[i] != vsReal || realOf(out.I[i]) != 5 {
				t.Errorf("element 150: state %d value %v, want real 5", out.St[i], realOf(out.I[i]))
			}
		default:
			if out.St[i] != vsInt || out.I[i] != int64(i)*2 {
				t.Errorf("element %d: state %d value %d, want integer %d", i, out.St[i], out.I[i], int64(i)*2)
			}
		}
	}
	s := u.Stats()
	if s.Escalated != 2 {
		t.Errorf("Escalated = %d, want 2", s.Escalated)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestColumnarPicksTheDominantState -- the state a column is given is the one
// most of its records are in, not the one element zero happens to be in. A single
// UNDEFINED at the head of a column of integers would otherwise escalate every
// record but that one: the per-record path, with a vector call in front of it.
func TestColumnarPicksTheDominantState(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	const n = 64
	in := mixedBatch(n, 0, vsUndef, 0)
	out := outBatch(n)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	if s := u.Stats(); s.Escalated != 1 {
		t.Fatalf("Escalated = %d, want 1: the column took its state from element 0", s.Escalated)
	}
	if out.St[0] != vsUndef {
		t.Errorf("element 0: state %d, want UNDEFINED", out.St[0])
	}
	for i := 1; i < n; i++ {
		if out.I[i] != int64(i)*2 {
			t.Errorf("element %d: %d, want %d", i, out.I[i], int64(i)*2)
		}
	}
}

// TestColumnarWholeColumnDeclined -- a state the guest has no loop for is handed
// back entirely, and every record still gets its scalar answer. The result column
// is never written, which a columnar result has to allow: there is no column to
// describe.
func TestColumnarWholeColumnDeclined(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	st := []uint8{vsBool, vsBool, vsBool}
	in := dataBatch(st, []int64{1, 0, 1})
	out := outBatch(3)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined the batch rather than escalating it")
	}
	// The scalar fixture refuses a boolean with a nonzero status, which is ERROR
	// for that record and a counted failure -- not a crash and not a wrong value.
	wantAllError(t, out, 3)
	if s := u.Stats(); s.Escalated != 3 {
		t.Errorf("Escalated = %d, want 3", s.Escalated)
	}
}

// TestColumnarAllDeclinedWritesNothing -- the result's single state byte is left
// poisoned when the guest handled no record, and that is not a malformed result.
func TestColumnarAllDeclinedWritesNothing(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdeclineall", 1, 1)
	in := intBatch(1, 2, 3, 4)
	out := outBatch(4)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	wantInts(t, out, []int64{2, 4, 6, 8})
	s := u.Stats()
	if s.Escalated != 4 {
		t.Errorf("Escalated = %d, want 4", s.Escalated)
	}
	if s.Failures != 0 {
		t.Errorf("unexpected failures: %v", s.ByReason)
	}
}

// TestColumnarSilentResultIsCaught -- one poisoned state byte is still enough to
// catch a guest that returns success having described no column. It writes
// payloads, so only the poison distinguishes it from a guest that worked.
func TestColumnarSilentResultIsCaught(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecsilent", 1, 1)
	in := intBatch(1, 2, 3)
	out := outBatch(3)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	wantAllError(t, out, 3)
	if s := u.Stats(); s.ByReason[ReasonBadResult] != 1 {
		t.Errorf("ByReason[BadResult] = %d, want 1 (%v)", s.ByReason[ReasonBadResult], s.ByReason)
	}
}

func TestColumnarBadResultState(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecbadstate", 1, 1)
	out := outBatch(3)
	if !u.CallBatch([]Batch{intBatch(1, 2, 3)}, out) {
		t.Fatal("declined")
	}
	wantAllError(t, out, 3)
	if s := u.Stats(); s.ByReason[ReasonBadResult] != 1 {
		t.Errorf("ByReason[BadResult] = %d, want 1", s.ByReason[ReasonBadResult])
	}
}

// TestColumnarBadBoolPayload -- the 0-or-1 rule the scalar slot is held to,
// checked over a whole column. The fixture writes 1 everywhere except the last
// lane, so a check that looked only at the first element would pass it.
func TestColumnarBadBoolPayload(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecbadbool", 1, 1)
	const n = 70
	out := outBatch(n)
	in := intBatch(make([]int64, n)...)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	wantAllError(t, out, n)
	if s := u.Stats(); s.ByReason[ReasonBadResult] != 1 {
		t.Errorf("ByReason[BadResult] = %d, want 1", s.ByReason[ReasonBadResult])
	}
}

// TestColumnarGuestCannotClaimTheHostsDeclines -- a guest that stores over the
// decline mask rather than ORing into it wipes the host's own bits. The host ORs
// them back, so the records whose value was not of the column's state still get
// the scalar answer instead of one computed from the filler they were given.
func TestColumnarGuestCannotClaimTheHostsDeclines(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecstomp", 1, 1)
	const n = 10
	in := mixedBatch(n, 4, vsUndef, 0)
	out := outBatch(n)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	if out.St[4] != vsUndef {
		t.Errorf("element 4: state %d payload %d, want UNDEFINED -- the guest was credited with a record it was never given",
			out.St[4], out.I[4])
	}
	for i := range n {
		if i == 4 {
			continue
		}
		if out.I[i] != int64(i)*2 {
			t.Errorf("element %d: %d, want %d", i, out.I[i], int64(i)*2)
		}
	}
	if s := u.Stats(); s.Escalated != 1 {
		t.Errorf("Escalated = %d, want 1", s.Escalated)
	}
}

// TestColumnarConstArgument -- a literal crosses as one payload and a flag, and
// the guest is told so rather than being handed n copies.
func TestColumnarConstArgument(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecconst", 2, 2)
	const n = 100
	k := constIntBatch(3, n)
	xs := intBatch(make([]int64, n)...)
	for i := range n {
		xs.I[i] = int64(i)
	}
	out := outBatch(n)
	if !u.CallBatch([]Batch{k, xs}, out) {
		t.Fatal("declined a const argument it should have been told about")
	}
	for i := range n {
		if out.St[i] != vsInt || out.I[i] != int64(i)*3 {
			t.Errorf("element %d: state %d value %d, want integer %d", i, out.St[i], out.I[i], int64(i)*3)
		}
	}
}

// TestColumnarMalformedColumnIsNotReportedAsStrings -- a state code that is not
// a state is a caller that built the column wrong, and it is counted as that. An
// operator sent looking for a string column that has no strings in it would be
// looking for the wrong thing.
func TestColumnarMalformedColumnIsNotReportedAsStrings(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	in := dataBatch([]uint8{vsInt, 9, vsInt}, []int64{1, 2, 3})
	if u.CallBatch([]Batch{in}, outBatch(3)) {
		t.Error("handled a column holding a state that is not a state")
	}
	s := u.Stats()
	if s.ByReason[ReasonBatchShape] != 1 {
		t.Errorf("ByReason[BatchShape] = %d, want 1 (%v)", s.ByReason[ReasonBatchShape], s.ByReason)
	}
	if s.ByReason[ReasonBatchStrings] != 0 {
		t.Errorf("ByReason[BatchStrings] = %d, want 0", s.ByReason[ReasonBatchStrings])
	}
}

// TestColumnarStringColumnDeclines -- there is no uniform state to give a column
// of strings, and escalating them one at a time would be the per-record path with
// a vector call in front of it. The batch declines, which is not a failure.
func TestColumnarStringColumnDeclines(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	st := []uint8{vsInt, vsString, vsInt}
	in := dataBatch(st, []int64{1, 0, 3})
	if u.CallBatch([]Batch{in}, outBatch(3)) {
		t.Error("handled a column holding a string")
	}
	if s := u.Stats(); s.ByReason[ReasonBatchStrings] != 1 {
		t.Errorf("ByReason[BatchStrings] = %d, want 1", s.ByReason[ReasonBatchStrings])
	}
	// A declined batch is not a batch: the guest was never called.
	if s := u.Stats(); s.Batches != 0 {
		t.Errorf("Batches = %d, want 0", s.Batches)
	}
}

// TestColumnarMaskArgumentStillCrossesAsAMask -- a mask column is already uniform
// and already two bits per record, so the columnar form has nothing to do to it
// and it is passed through as it always was.
func TestColumnarMaskArgumentStillCrossesAsAMask(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	in := maskBatch([]int{mkTrue, mkFalse, mkTrue})
	out := outBatch(3)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined a mask column")
	}
	// colvecdouble refuses a form it does not read with a nonzero status, which
	// is the whole batch being ERROR. What is being asserted is that it SAW a
	// mask: a mask column turned into a columnar one would have reached the
	// integer loop instead and returned numbers.
	wantAllError(t, out, 3)
	if s := u.Stats(); s.ByReason[ReasonStatus] != 1 {
		t.Errorf("ByReason[Status] = %d, want 1 (%v)", s.ByReason[ReasonStatus], s.ByReason)
	}
}

// TestColumnarUniformDeclarationIsTaken -- Batch.Uniform says what the scan would
// have found, and the host believes it rather than looking. The column here holds
// states that contradict St entirely, which is how the test can tell the scan did
// not run: a scan would have found three different states and escalated two of
// them.
func TestColumnarUniformDeclarationIsTaken(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	in := Batch{N: 3, St: []uint8{vsUndef, vsError, vsBool}, I: []int64{1, 2, 3},
		Uniform: true, State: vsInt}
	out := outBatch(3)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	wantInts(t, out, []int64{2, 4, 6})
	if s := u.Stats(); s.Escalated != 0 {
		t.Errorf("Escalated = %d, want 0: the declaration was not taken", s.Escalated)
	}
}

// TestColumnarSimdAndUnrollAgree -- three guests, one function. The branch-free
// loop, the unrolled one and the vector one must return the same column for the
// same input, including at a width that is not a multiple of the unrolling.
func TestColumnarSimdAndUnrollAgree(t *testing.T) {
	for _, n := range []int{1, 7, 8, 9, 63, 64, 65, 300} {
		in := intBatch(make([]int64, n)...)
		for i := range n {
			in.I[i] = int64(i) - 3
		}
		var got [3][]int64
		for k, name := range []string{"colvecdouble", "colvecunroll", "colvecsimd"} {
			u := loadFixture(t, Options{}, name, 1, 1)
			out := outBatch(n)
			if !u.CallBatch([]Batch{in}, out) {
				t.Fatalf("%s declined n=%d", name, n)
			}
			got[k] = append([]int64(nil), out.I[:n]...)
			for i := range n {
				if out.St[i] != vsInt {
					t.Fatalf("%s n=%d element %d: state %d", name, n, i, out.St[i])
				}
			}
		}
		for i := range n {
			if got[0][i] != (int64(i)-3)*2 || got[1][i] != got[0][i] || got[2][i] != got[0][i] {
				t.Fatalf("n=%d element %d: scalar %d unrolled %d simd %d",
					n, i, got[0][i], got[1][i], got[2][i])
			}
		}
	}
}

// TestColumnarVersionOneIsUntouched -- a version 1 guest is handed the data form
// and nothing else, whatever the host can now do. The assertion is in the
// fixture: vecdouble refuses any form but 0 with a nonzero status, so a columnar
// column would make this ERROR rather than doubling.
func TestColumnarVersionOneIsUntouched(t *testing.T) {
	u := loadFixture(t, Options{}, "vecdouble", 1, 1)
	in := mixedBatch(8, 3, vsUndef, 0)
	out := outBatch(8)
	if !u.CallBatch([]Batch{in}, out) {
		t.Fatal("declined")
	}
	for i := range 8 {
		if i == 3 {
			continue
		}
		if out.St[i] != vsInt || out.I[i] != int64(i)*2 {
			t.Errorf("element %d: state %d value %d", i, out.St[i], out.I[i])
		}
	}
}

// TestColumnStateScan covers the host-side decision directly, including the cases
// a guest fixture cannot reach: an empty batch, a column of one state, a tie, and
// the word-at-a-time path's boundaries.
func TestColumnStateScan(t *testing.T) {
	exc := make([]uint64, 64)
	states := func(xs ...uint8) *Batch { return &Batch{N: len(xs), St: xs, I: make([]int64, len(xs))} }

	cases := []struct {
		name   string
		b      *Batch
		state  uint8
		ok     bool
		exc    []int
		reason Reason
	}{
		{"empty", states(), vsUndef, true, nil, ReasonNone},
		{"one", states(vsReal), vsReal, true, nil, ReasonNone},
		{"uniform short", states(vsInt, vsInt, vsInt), vsInt, true, nil, ReasonNone},
		{"uniform eight", states(vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsInt), vsInt, true, nil, ReasonNone},
		{"last of eight differs", states(vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsUndef), vsInt, true, []int{7}, ReasonNone},
		{"ninth differs", states(vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsInt, vsReal), vsInt, true, []int{8}, ReasonNone},
		{"dominant real", states(vsInt, vsReal, vsReal), vsReal, true, []int{0}, ReasonNone},
		{"tie goes to the lower state", states(vsInt, vsReal), vsInt, true, []int{1}, ReasonNone},
		{"string refuses", states(vsInt, vsString), 0, false, nil, ReasonBatchStrings},
		{"dict refuses", states(vsDictCode, vsInt), 0, false, nil, ReasonBatchStrings},
		{"unknown state refuses", states(vsInt, 9), 0, false, nil, ReasonBatchShape},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, r := tc.b.columnState(tc.b.N, exc)
			if (r == ReasonNone) != tc.ok {
				t.Fatalf("reason = %v, want ok = %v", r, tc.ok)
			}
			if r != ReasonNone {
				if r != tc.reason {
					t.Fatalf("reason = %v, want %v", r, tc.reason)
				}
				return
			}
			if state != tc.state {
				t.Errorf("state = %d, want %d", state, tc.state)
			}
			want := map[int]bool{}
			for _, i := range tc.exc {
				want[i] = true
			}
			for i := range tc.b.N {
				got := exc[i/64]&(uint64(1)<<uint(i%64)) != 0
				if got != want[i] {
					t.Errorf("element %d: exception %v, want %v", i, got, want[i])
				}
			}
		})
	}
}

// TestColumnarFullWidthBatchFitsItsRegion -- the region is sized at load from the
// version 1 frame, before the guest has said which version it speaks, so a
// version 2 frame at the full configured width has to fit in it. If it did not,
// nothing would fail: the batch would decline, the engine would fall back, and
// the only symptom would be that the vector path was off.
func TestColumnarFullWidthBatchFitsItsRegion(t *testing.T) {
	const n = 4096
	u := loadFixture(t, Options{MaxBatch: n}, "colvecconst", 2, 2)
	k := constIntBatch(3, n)
	xs := intBatch(make([]int64, n)...)
	for i := range n {
		xs.I[i] = int64(i)
	}
	out := outBatch(n)
	if !u.CallBatch([]Batch{k, xs}, out) {
		t.Fatalf("declined a full-width batch: %v", u.Stats().ByReason)
	}
	for _, i := range []int{0, 1, n / 2, n - 1} {
		if out.St[i] != vsInt || out.I[i] != int64(i)*3 {
			t.Errorf("element %d: state %d value %d, want integer %d", i, out.St[i], out.I[i], int64(i)*3)
		}
	}
}

// TestColumnarRegionFitsInsideTheVersionOneSizing -- the host region is sized at
// load, before the guest has reported its version, so a version 2 frame has to be
// no larger than the version 1 frame it was sized for. It is, because a columnar
// argument is eight bytes per record against nine, but nothing enforces that
// except this.
func TestColumnarRegionFitsInsideTheVersionOneSizing(t *testing.T) {
	for _, n := range []int{1, 7, 64, 1000, 4096} {
		for argc := 1; argc <= 4; argc++ {
			if got, want := colRegionBytes(n, argc), vectorRegionBytes(n, argc); got > want {
				t.Errorf("n=%d argc=%d: columnar frame %d bytes, version 1 sizing %d", n, argc, got, want)
			}
		}
	}
}

// TestColumnarDeclinedLanesHoldABenignValue -- a branch-free guest computes the
// lanes it was told to skip, so what is in them must not be able to trap it. The
// filler is a 1 in an integer column and 1.0 in a real one, both of which divide.
//
// It goes through writeColBatchArgs rather than calling the filler directly,
// because the question is not whether the filler works but whether a column the
// host hands over has been filled.
func TestColumnarDeclinedLanesHoldABenignValue(t *testing.T) {
	u := loadFixture(t, Options{}, "colvecdouble", 1, 1)
	in, err := u.acquire()
	if err != nil {
		t.Fatal(err)
	}
	defer u.release(in)

	for _, tc := range []struct {
		state uint8
		other uint8
		want  int64
	}{{vsInt, vsUndef, 1}, {vsReal, vsUndef, realBits(1)}} {
		const n = 6
		col := Batch{N: n, St: make([]uint8, n), I: make([]int64, n)}
		for i := range n {
			col.St[i], col.I[i] = tc.state, -7
		}
		col.St[2] = tc.other
		args := []Batch{col}
		if !u.planColumns(in, n, args) {
			t.Fatal("refused")
		}
		f, ok := writeColBatchArgs(in.mem, in.base, in.size, n, args, in.plan, in.hostDeclined)
		if !ok {
			t.Fatal("does not fit")
		}
		aPtr, _ := in.mem.ReadUint32Le(f.args + offDescB)
		raw, ok := in.mem.Read(aPtr, n*8)
		if !ok {
			t.Fatal("cannot read the column back")
		}
		if got := int64(binary.LittleEndian.Uint64(raw[2*8:])); got != tc.want {
			t.Errorf("state %d: the declined lane holds %d, want the filler %d", tc.state, got, tc.want)
		}
		if got := int64(binary.LittleEndian.Uint64(raw[8:])); got != -7 {
			t.Errorf("state %d: lane 1 holds %d, want -7: only declined lanes are filled", tc.state, got)
		}
	}
}

// TestColumnarUnwrittenTailDoesNotLeakThePreviousBatch -- the columnar result
// has one state byte to poison, so a guest that describes a column and then fills
// half of it is not caught. What the host can still guarantee is that the half it
// did not write does not depend on what the last batch asked: the payloads are
// zeroed before every call.
//
// Two batches on the same instance, with different values, and the second one's
// tail must not be the first one's answers.
func TestColumnarUnwrittenTailDoesNotLeakThePreviousBatch(t *testing.T) {
	u := loadFixture(t, Options{MaxInstances: 1}, "colvecpartial", 1, 1)
	const n = 8
	// A negative first element makes the fixture fill the whole column, so the
	// buffer the second batch is handed holds real answers in every lane.
	first := intBatch(-11, -12, -13, -14, -15, -16, -17, -18)
	out := outBatch(n)
	if !u.CallBatch([]Batch{first}, out) {
		t.Fatal("declined")
	}
	for i := range n {
		if want := int64(-11-i) * 2; out.I[i] != want {
			t.Fatalf("element %d of the first batch: %d, want %d", i, out.I[i], want)
		}
	}
	second := intBatch(1, 2, 3, 4, 5, 6, 7, 8)
	if !u.CallBatch([]Batch{second}, out) {
		t.Fatal("declined")
	}
	for i := n / 2; i < n; i++ {
		if out.I[i] != 0 {
			t.Errorf("element %d: %d, want 0 -- the tail is the previous batch's column", i, out.I[i])
		}
	}
	for i := range n / 2 {
		if out.I[i] != int64(i+1)*2 {
			t.Errorf("element %d: %d, want %d", i, out.I[i], int64(i+1)*2)
		}
	}
}

// TestColumnarConcurrentBatches -- the uniformity scan, the exception mask and
// the plan are all per-instance scratch, and instances are shared between
// concurrent callers. Each goroutine sends a column with its own exceptions in
// its own places, so a scan writing into another call's mask shows up as a record
// escalated or not escalated in the wrong batch rather than as a crash.
func TestColumnarConcurrentBatches(t *testing.T) {
	const goroutines, each, n = 8, 40, 70
	u := loadFixture(t, Options{MaxInstances: 3, MaxBatch: n}, "colvecsimd", 1, 1)

	var wg sync.WaitGroup
	bad := make(chan string, goroutines*each)
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			in := intBatch(make([]int64, n)...)
			for i := range n {
				in.I[i] = int64(g*1_000_000 + i)
			}
			// A different record is UNDEFINED in each goroutine's column, so a
			// mask leaking between calls lands on a record this one did not
			// decline.
			undef := g * 7
			in.St[undef], in.I[undef] = vsUndef, 0
			out := outBatch(n)
			for range each {
				if !u.CallBatch([]Batch{in}, out) {
					bad <- "declined"
					return
				}
				for i := range n {
					if i == undef {
						if out.St[i] != vsUndef {
							bad <- fmt.Sprintf("g%d element %d: state %d, want UNDEFINED", g, i, out.St[i])
							return
						}
						continue
					}
					if out.St[i] != vsInt || out.I[i] != in.I[i]*2 {
						bad <- fmt.Sprintf("g%d element %d = (%d,%d), want %d",
							g, i, out.St[i], out.I[i], in.I[i]*2)
						return
					}
				}
			}
		}()
	}
	wg.Wait()
	close(bad)
	for e := range bad {
		t.Fatal(e)
	}
	if s := u.Stats(); s.Escalated != goroutines*each {
		t.Errorf("Escalated = %d, want %d", s.Escalated, goroutines*each)
	}
}
