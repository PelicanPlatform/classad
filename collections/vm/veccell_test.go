package vm

import (
	"math"
	"testing"
)

// fakeDict is a DictResolver over a fixed entry list.
type fakeDict []string

func (d fakeDict) At(code int) (string, bool) {
	if code < 0 || code >= len(d) {
		return "", false
	}
	return d[code], true
}
func (d fakeDict) Len() int                      { return len(d) }
func (d fakeDict) Range(string) (int, int, bool) { return 0, 0, false }

// TestVecCellReadsEveryForm covers Cell against each form a LOADED COLUMN can arrive in.
//
// The forms are the point. A vector is a mask, a raw byte run, a constant, a dictionary code or
// plain data depending on what the block held and what the loader could do with it, and a producer
// reads whichever one it is handed. The collections tests exercise the forms their fixtures happen
// to produce -- which turned out not to include the bool mask or a constant -- so the ones a column
// scan reaches rarely are pinned here, where constructing them takes no fixture at all.
func TestVecCellReadsEveryForm(t *testing.T) {
	t.Run("mask", func(t *testing.T) {
		v := newVec(4)
		v.Mask = true
		// 0: true, 1: false, 2: undefined, 3: error. See vecmask.go for the plane encoding.
		setMaskStateAt(v.Hi, v.Lo, 0, mTrue)
		setMaskStateAt(v.Hi, v.Lo, 1, mFalse)
		setMaskStateAt(v.Hi, v.Lo, 2, mUndef)
		setMaskStateAt(v.Hi, v.Lo, 3, mError)
		for i, want := range []struct {
			st  uint8
			num int64
		}{{VsBool, 1}, {VsBool, 0}, {VsUndef, 0}, {VsError, 0}} {
			st, num, str := v.Cell(i)
			if st != want.st || num != want.num || str != "" {
				t.Errorf("Cell(%d) = (%d, %d, %q), want (%d, %d, \"\")", i, st, num, str, want.st, want.num)
			}
		}
	})

	t.Run("const reads element zero", func(t *testing.T) {
		// A constant keeps its value in element 0 and leaves the rest as garbage, so a reader that
		// forgets to collapse the index gets whatever happens to be there.
		v := newVec(4)
		v.Const = true
		v.SetInt(0, 42)
		for i := 1; i < 4; i++ {
			v.SetInt(i, -7)
		}
		for i := range 4 {
			st, num, _ := v.Cell(i)
			if st != VsInt || num != 42 {
				t.Errorf("Cell(%d) = (%d, %d), want (VsInt, 42): a constant was read off its own element", i, st, num)
			}
		}
	})

	t.Run("raw int", func(t *testing.T) {
		if !rawSupported {
			// Behind goexperiment.simd; the simd CI job is where these two cases run.
			t.Skip("no width-native engine in this build")
		}
		v := newVec(3)
		v.Raw = []byte{1, 0, 2, 0, 3, 0}
		v.RawWidth, v.RawUnsigned = 2, true
		for i, want := range []int64{1, 2, 3} {
			st, num, _ := v.Cell(i)
			if st != VsInt || num != want {
				t.Errorf("Cell(%d) = (%d, %d), want (VsInt, %d)", i, st, num, want)
			}
		}
	})

	t.Run("raw real", func(t *testing.T) {
		if !rawSupported {
			t.Skip("no width-native engine in this build")
		}
		// A real's slot holds its bits, and Cell reports the bits with VsReal rather than
		// converting -- the same convention the data form uses, so a consumer has one rule.
		v := newVec(1)
		bits := math.Float64bits(2.5)
		raw := make([]byte, 8)
		for i := range raw {
			raw[i] = byte(bits >> (8 * i))
		}
		v.Raw, v.RawWidth, v.RawReal = raw, 8, true
		st, num, _ := v.Cell(0)
		if st != VsReal {
			t.Fatalf("Cell(0) state = %d, want VsReal", st)
		}
		if got := math.Float64frombits(uint64(num)); got != 2.5 {
			t.Errorf("Cell(0) = %v, want 2.5", got)
		}
	})

	t.Run("dictionary code resolves to its string", func(t *testing.T) {
		v := newVec(2)
		v.Dict = fakeDict{"alice", "bob"}
		v.St[0], v.I[0] = VsDictCode, 1
		v.St[1], v.I[1] = VsDictCode, 0
		for i, want := range []string{"bob", "alice"} {
			st, _, str := v.Cell(i)
			if st != VsString || str != want {
				t.Errorf("Cell(%d) = (%d, %q), want (VsString, %q): a code reached the caller unresolved",
					i, st, str, want)
			}
		}
	})

	t.Run("a code with no entry is an error, not a number", func(t *testing.T) {
		v := newVec(1)
		v.Dict = fakeDict{"alice"}
		v.St[0], v.I[0] = VsDictCode, 9
		if st, num, _ := v.Cell(0); st != VsError {
			t.Errorf("Cell(0) = (%d, %d), want VsError: an unresolvable code was reported as a value", st, num)
		}
	})

	t.Run("data states", func(t *testing.T) {
		v := newVec(5)
		v.SetInt(0, 7)
		v.SetReal(1, 1.5)
		v.SetBool(2, true)
		v.SetString(3, "hi")
		v.St[4] = VsUndef
		for i, want := range []struct {
			st  uint8
			num int64
			str string
		}{
			{VsInt, 7, ""},
			{VsReal, int64(math.Float64bits(1.5)), ""},
			{VsBool, 1, ""},
			{VsString, 0, "hi"},
			{VsUndef, 0, ""},
		} {
			st, num, str := v.Cell(i)
			if st != want.st || num != want.num || str != want.str {
				t.Errorf("Cell(%d) = (%d, %d, %q), want (%d, %d, %q)", i, st, num, str, want.st, want.num, want.str)
			}
		}
	})
}

// TestNewColumnVecHasMaskPlanes pins the one reason NewColumnVec exists beside NewVec.
//
// A boolean column with no escape is delivered as a MASK, written straight into the planes. NewVec
// leaves them nil, which is right for a caller building values element by element and fatal for a
// loaded column: the load writes nothing and the read then indexes a nil slice. The failure is loud
// rather than silent, which is why this is a constructor test and not a scan one -- no fixture here
// produces an escape-free boolean column, so a scan test cannot reach the form at all.
func TestNewColumnVecHasMaskPlanes(t *testing.T) {
	const n = 100
	if got := NewColumnVec(n); len(got.Hi) != MaskWords(n) || len(got.Lo) != MaskWords(n) {
		t.Errorf("NewColumnVec(%d) planes are %d/%d words, want %d: a mask column would take none of its bits",
			n, len(got.Hi), len(got.Lo), MaskWords(n))
	}
	if got := NewVec(n); len(got.Hi) != 0 || len(got.Lo) != 0 {
		t.Errorf("NewVec(%d) now allocates planes (%d/%d); if that is intended, NewColumnVec has no reason to exist",
			n, len(got.Hi), len(got.Lo))
	}
	// Every element starts undefined, so a column that loads fewer elements than the vector holds
	// cannot read a previous block's value out of the tail.
	v := NewColumnVec(4)
	for i := range 4 {
		if st, _, _ := v.Cell(i); st != VsUndef {
			t.Errorf("NewColumnVec element %d starts as state %d, want VsUndef", i, st)
		}
	}
}
