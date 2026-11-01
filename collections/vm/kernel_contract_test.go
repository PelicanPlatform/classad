package vm

import "testing"

// THE VECTOR KERNELS ARE DESTRUCTIVE IN THEIR OPERANDS, and these tests pin that so a caller cannot
// discover it the hard way.
//
// The stack executor is safe by construction: every OpLoadRef pushes a fresh vector, so no two kernels
// ever see the same one. Anything that stops being true of -- a register file with one register per
// value, or a common-subexpression pass that shares one loaded column between two consumers -- has to
// copy or prove ownership first. The failure mode is a WRONG VALUE and not a crash, which is why the
// contract is worth a test rather than a comment.
//
// These tests pin current behaviour, not a wish. A change that makes a kernel non-destructive is an
// improvement and should update them deliberately, which is the point.

// intVec builds a data-form vector of integers.
func intVec(vals ...int64) *Vec {
	v := newVec(len(vals))
	for i, x := range vals {
		v.St[i], v.I[i] = VsInt, x
	}
	return v
}

// TestLogicalVecWritesIntoItsLeftOperand pins where the result goes: logicalVec has no destination
// parameter, so a register-form executor cannot map Dst = op(A, B) onto it without a copy.
func TestLogicalVecWritesIntoItsLeftOperand(t *testing.T) {
	l, r := intVec(1, 1), intVec(1, 0)
	if !logicalVec("&&", l, r) {
		t.Fatal("logicalVec declined")
	}
	// The ANSWER is in l.
	l.toData()
	for i, want := range []int64{1, 0} {
		if l.St[i] != VsBool || l.I[i] != want {
			t.Errorf("l[%d] = (st %d, %d), want (VsBool, %d); the result is not in the left operand",
				i, l.St[i], l.I[i], want)
		}
	}
}

// TestLogicalVecConvertsItsRightOperand pins the half that is easy to miss: the RIGHT operand is
// converted in place too, so it is not merely read.
func TestLogicalVecConvertsItsRightOperand(t *testing.T) {
	l, r := intVec(1, 1), intVec(1, 0)
	if r.Mask {
		t.Fatal("the fixture is already in mask form; this test is not testing what it says")
	}
	if !logicalVec("&&", l, r) {
		t.Fatal("logicalVec declined")
	}
	if !r.Mask {
		t.Error("the right operand was not converted; a caller may rely on it being left alone")
	}
}

// TestMaskConversionLosesTheNumber is the hazard itself, and the reason sharing a vector is a
// correctness problem rather than an aliasing nuisance.
//
// A number converts to a mask by ClassAd operand truthiness -- nonzero is TRUE -- and converting back
// reconstructs the element FROM THE PLANES rather than restoring what was there. So 3 comes back as
// TRUE, not as 3. In `x > 1 && x`, a shared x would be clobbered by the logical operator while the
// comparison still needed it.
func TestMaskConversionLosesTheNumber(t *testing.T) {
	v := intVec(3, 0, -7)
	v.toMask()
	v.toData()
	for i := range v.n {
		if v.St[i] != VsBool {
			t.Fatalf("element %d is state %d after a round trip, want VsBool", i, v.St[i])
		}
	}
	if v.I[0] != 1 || v.I[1] != 0 || v.I[2] != 1 {
		t.Errorf("round trip gave %v, want [1 0 1]; the numbers survived and this test is stale", v.I)
	}
	// Stated as the property a caller actually needs: the value is NOT recoverable.
	if got := v.valueData(0).String(); got == "3" {
		t.Error("3 survived a mask round trip; the sharing hazard this pins would not exist")
	}
}

// TestUnaryKernelsReplaceTheirOperand covers the single-operand kernels, which have the same shape:
// the operand IS the destination.
func TestUnaryKernelsReplaceTheirOperand(t *testing.T) {
	v := intVec(5, -5)
	if !unopVec(nil, "-", v) {
		t.Fatal("unopVec declined")
	}
	if v.St[0] != VsInt || v.I[0] != -5 {
		t.Errorf("v[0] = (st %d, %d), want (VsInt, -5); the operand is not the destination",
			v.St[0], v.I[0])
	}
}
