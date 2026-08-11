package vm

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// The mask form's tables are verified against the REFERENCE evaluator, not against the executor that uses
// them -- comparing plane operations to the kernels built on plane operations would be circular.
//
// The spike that chose this representation recorded its numbers in its own commit; what stays here is the
// correctness, because the tables are where this can silently diverge. Two ways it nearly did:
//
//	ERROR && FALSE = ERROR   but   FALSE && ERROR = FALSE     -- && does not commute
//	1 && true = TRUE, 0 && true = FALSE, "x" && true = ERROR  -- numbers are TRUTHY, strings are not
//
// Both look like rules a reasonable person would state the other way, and a corpus that never puts an
// ERROR on the left, or never puts a bare number in a logical position, passes either way.

// The spike's names for the mask states, kept so these tests read as tables.
const (
	stF = mFalse
	stT = mTrue
	stU = mUndef
	stE = mError
)

func setPlaneState(v *Vec, i, st int) {
	switch st {
	case stF:
		v.SetBool(i, false)
	case stT:
		v.SetBool(i, true)
	case stU:
		v.St[i] = VsUndef
	case stE:
		v.St[i] = VsError
	}
}

func maskName(st int) string {
	return map[int]string{stF: "FALSE", stT: "TRUE", stU: "UNDEF", stE: "ERROR"}[st]
}

// TestPlaneLogicMatchesReference checks planeAnd and planeOr against ApplyBinaryOp for all sixteen state
// pairs of both operators -- every entry of the truth table the whole representation rests on.
func TestPlaneLogicMatchesReference(t *testing.T) {
	ev := classad.NewEvaluator(nil)
	box := func(st int) classad.Value {
		switch st {
		case stF:
			return classad.NewBoolValue(false)
		case stT:
			return classad.NewBoolValue(true)
		case stU:
			return classad.NewUndefinedValue()
		default:
			return classad.NewErrorValue()
		}
	}
	for _, op := range []string{"&&", "||"} {
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				xhi, xlo := []uint64{uint64(x >> 1)}, []uint64{uint64(x & 1)}
				yhi, ylo := []uint64{uint64(y >> 1)}, []uint64{uint64(y & 1)}
				ohi, olo := make([]uint64, 1), make([]uint64, 1)
				if op == "&&" {
					planeAnd(xhi, xlo, yhi, ylo, ohi, olo)
				} else {
					planeOr(xhi, xlo, yhi, ylo, ohi, olo)
				}
				got := maskStateAt(ohi, olo, 0)
				want, ok := valueToMaskState(ev.ApplyBinaryOp(op, box(x), box(y)))
				if !ok {
					t.Fatalf("%s %s %s: reference produced a value a mask cannot hold",
						maskName(x), op, maskName(y))
				}
				if got != want {
					t.Errorf("%s %s %s: planes say %s, reference says %s",
						maskName(x), op, maskName(y), maskName(got), maskName(want))
				}
			}
		}
	}
}

// TestPlaneNotMatchesReference checks planeNot against ApplyUnaryOp for all four states.
func TestPlaneNotMatchesReference(t *testing.T) {
	ev := classad.NewEvaluator(nil)
	for _, src := range []struct {
		st  int
		val classad.Value
	}{
		{stF, classad.NewBoolValue(false)},
		{stT, classad.NewBoolValue(true)},
		{stU, classad.NewUndefinedValue()},
		{stE, classad.NewErrorValue()},
	} {
		hi, lo := []uint64{uint64(src.st >> 1)}, []uint64{uint64(src.st & 1)}
		ohi, olo := make([]uint64, 1), make([]uint64, 1)
		planeNot(hi, lo, ohi, olo)
		got := maskStateAt(ohi, olo, 0)
		want, _ := valueToMaskState(ev.ApplyUnaryOp("!", src.val))
		if got != want {
			t.Errorf("!%s: planes say %s, reference says %s", maskName(src.st), maskName(got), maskName(want))
		}
	}
}

// TestAllDefinedFastPathMatchesGeneral checks the per-word shortcut planeAnd and planeOr take when neither
// operand has an UNDEFINED or ERROR in those 64 records -- the common case, since a comparison over an
// escape-free column yields only TRUE or FALSE. It must produce what the general path would.
func TestAllDefinedFastPathMatchesGeneral(t *testing.T) {
	const words = 64
	xhi, xlo := make([]uint64, words), make([]uint64, words)
	yhi, ylo := make([]uint64, words), make([]uint64, words)
	for w := 0; w < words; w++ {
		xlo[w] = uint64(w)*0x9e3779b97f4a7c15 | 1
		ylo[w] = uint64(w)*0xc2b2ae3d27d4eb4f | 3
	}
	for _, tc := range []struct {
		name string
		fast func(ohi, olo []uint64)
		want func(w int) uint64
	}{
		{"and", func(ohi, olo []uint64) { planeAnd(xhi, xlo, yhi, ylo, ohi, olo) }, func(w int) uint64 { return xlo[w] & ylo[w] }},
		{"or", func(ohi, olo []uint64) { planeOr(xhi, xlo, yhi, ylo, ohi, olo) }, func(w int) uint64 { return xlo[w] | ylo[w] }},
	} {
		ohi, olo := make([]uint64, words), make([]uint64, words)
		tc.fast(ohi, olo)
		for w := 0; w < words; w++ {
			if ohi[w] != 0 {
				t.Fatalf("%s word %d: defined inputs produced undefined/error", tc.name, w)
			}
			if olo[w] != tc.want(w) {
				t.Errorf("%s word %d: %#x != %#x", tc.name, w, olo[w], tc.want(w))
			}
		}
	}
}

// TestShortCircuitNoOpExhaustive is the strong form of the claim that lets && and || vectorize at all: the
// vector executor SKIPS the short-circuit jump and always evaluates both operands, so for every one of the
// sixteen state pairs of both operators its answer must equal the scalar path's -- which does
// short-circuit, and therefore never evaluates a right operand the left one settles.
//
// The executor's differential test covers this incidentally, over whatever pairs a twelve-record corpus
// happens to produce. This covers all thirty-two deliberately, which matters because the table is
// left-biased: a combine that quietly symmetrised it would still pass a corpus with no ERROR on the left.
func TestShortCircuitNoOpExhaustive(t *testing.T) {
	src := map[int]string{stF: "false", stT: "true", stU: "undefined", stE: "error"}
	empty := newTestSource([]map[string]string{{}})
	for _, op := range []string{"&&", "||"} {
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				expr := fmt.Sprintf("(%s) %s (%s)", src[x], op, src[y])
				q, err := Parse(expr)
				if err != nil {
					t.Fatal(err)
				}
				got, ok := q.VecEval(empty, 1, nil)
				if !ok {
					t.Fatalf("%s: vector executor declined", expr)
				}
				if !sameValue(got.value(0), q.Eval(classad.New())) {
					t.Errorf("%s %s %s: vector %s != scalar %s", maskName(x), op, maskName(y),
						got.value(0), q.Eval(classad.New()))
				}
			}
		}
	}
}

// TestMaskConversionMatchesReference pins the DATA-to-mask conversion, which is where the truthiness rule
// lives: numbers are truthy in a logical position, strings are an error, and a real zero is false.
//
// The rule that looks right and is wrong is "anything that is not a boolean is an ERROR". It holds for
// strings and fails for every number.
func TestMaskConversionMatchesReference(t *testing.T) {
	operands := []string{"0", "1", "42", "-1", "0.0", "1.5", "-0.0", `""`, `"x"`, "true", "false", "undefined", "error"}
	empty := newTestSource([]map[string]string{{}})
	for _, l := range operands {
		for _, form := range []string{"(%s) && true", "(%s) && false", "(%s) || false", "(%s) || true", "!(%s)"} {
			expr := fmt.Sprintf(form, l)
			q, err := Parse(expr)
			if err != nil {
				t.Fatalf("parse %q: %v", expr, err)
			}
			got, ok := q.VecEval(empty, 1, nil)
			if !ok {
				t.Fatalf("%s: declined", expr)
			}
			if want := q.Eval(classad.New()); !sameValue(got.value(0), want) {
				t.Errorf("%s: vector %s != scalar %s", expr, got.value(0), want)
			}
		}
	}
}
