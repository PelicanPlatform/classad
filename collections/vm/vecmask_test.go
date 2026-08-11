package vm

import (
	"fmt"
	"math/bits"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// SPIKE: three-valued logic as BITPLANES, which is the representation both the combine and the
// comparison want.
//
// The phase spike found a logical combine costs about as much as a comparison that loads and compares
// int64s, which makes no sense for an operation that only merges booleans -- until you look at the
// representation. A boolean element is nine bytes here (a state byte plus an eight-byte payload) and the
// combine walks it with a four-way branch per record.
//
// Two bitplanes hold the same four states in two BITS per record:
//
//	hi lo
//	 0  0  FALSE      0  1  TRUE      1  0  UNDEFINED      1  1  ERROR
//
// which makes a combine a fixed sequence of word-wise ANDs and ORs over 64 records at a time, branch
// free, in ordinary Go. It is also exactly what SIMD wants: Int64x8.Greater RETURNS a Mask64x8 rather
// than a vector, Mask64x8.And is the combine, ToBits feeds a popcount, and Int64x8.Compress is the
// selection-pushdown primitive. So bitplanes are not a detour to take before SIMD -- they are the layout
// SIMD plugs into, and they carry most of the win to every architecture without the experiment flag.

const (
	stF = 0 // hi=0 lo=0
	stT = 1 // hi=0 lo=1
	stU = 2 // hi=1 lo=0
	stE = 3 // hi=1 lo=1
)

// planeAnd is ClassAd &&, and the table is LEFT-BIASED rather than symmetric:
//
//	ERROR && anything  = ERROR        FALSE && ERROR = FALSE
//	TRUE  && y         = y            UNDEF && FALSE = FALSE
//	FALSE && anything  = FALSE        UNDEF && ERROR = ERROR, else UNDEF
//
// So && does not commute: `ERROR && FALSE` is ERROR while `FALSE && ERROR` is FALSE. That is
// short-circuit semantics made total -- the left operand decides where it can -- and it is exactly what
// this spike's first version got wrong by assuming FALSE dominates everything, which is what the
// agreement test against the executor caught. Asymmetry costs a few more word operations and nothing
// else; it is still branch-free over 64 records at a time.
func planeAnd(xhi, xlo, yhi, ylo, ohi, olo []uint64) {
	for w := range ohi {
		xh, xl, yh, yl := xhi[w], xlo[w], yhi[w], ylo[w]
		isTx, isUx, isEx := ^xh&xl, xh&^xl, xh&xl
		isTy, isUy, isEy := ^yh&yl, yh&^yl, yh&yl
		// FALSE is 00, so it needs no term: whatever is neither ERROR, TRUE nor UNDEFINED falls out of
		// both planes as zero.
		resE := isEx | ((isTx | isUx) & isEy)
		resT := isTx & isTy
		resU := (isTx & isUy) | (isUx & (isTy | isUy))
		ohi[w], olo[w] = resU|resE, resT|resE
	}
}

// planeOr is ClassAd ||, left-biased in the same way:
//
//	ERROR || anything = ERROR        TRUE  || ERROR = TRUE
//	FALSE || y        = y            UNDEF || TRUE  = TRUE
//	TRUE  || anything = TRUE         UNDEF || ERROR = ERROR, else UNDEF
func planeOr(xhi, xlo, yhi, ylo, ohi, olo []uint64) {
	for w := range ohi {
		xh, xl, yh, yl := xhi[w], xlo[w], yhi[w], ylo[w]
		isFx, isTx, isUx, isEx := ^xh&^xl, ^xh&xl, xh&^xl, xh&xl
		isFy, isTy, isUy, isEy := ^yh&^yl, ^yh&yl, yh&^yl, yh&yl
		hands := isFx | isUx // the left operand hands the decision to the right
		resE := isEx | (hands & isEy)
		resT := isTx | (hands & isTy)
		resU := (isFx & isUy) | (isUx & (isFy | isUy))
		ohi[w], olo[w] = resU|resE, resT|resE
	}
}

// planeAndDefined is the same operator when neither side can be UNDEFINED or ERROR: one AND per 64
// records. The block already knows when that holds -- escapeFree(field) says a column has no missing or
// unstorable value, so a comparison over it yields only TRUE or FALSE -- which is the common case on
// real data and the reason to carry the flag at all.
func planeAndDefined(xlo, ylo, olo []uint64) {
	for w := range olo {
		olo[w] = xlo[w] & ylo[w]
	}
}

// planeCountTrue counts records whose result is TRUE and which the snapshot can see: one popcount per 64
// records, against a per-record branch today.
func planeCountTrue(hi, lo, live []uint64) int {
	n := 0
	for w := range hi {
		n += bits.OnesCount64(^hi[w] & lo[w] & live[w])
	}
	return n
}

func packPlanes(states []uint8, hi, lo []uint64) {
	for i, s := range states {
		w, b := i/64, uint(i%64)
		if s == stU || s == stE {
			hi[w] |= 1 << b
		}
		if s == stT || s == stE {
			lo[w] |= 1 << b
		}
	}
}

// TestPlaneLogicMatchesExecutor is the precondition for any timing: the bitplane operators must agree
// with the executor's combine -- which is itself verified per record against the tree-walking evaluator
// -- for every one of the sixteen state pairs.
func TestPlaneLogicMatchesExecutor(t *testing.T) {
	names := map[int]string{stF: "FALSE", stT: "TRUE", stU: "UNDEF", stE: "ERROR"}
	ev := classad.NewEvaluator(nil)
	for _, op := range []string{"&&", "||"} {
		for x := 0; x < 4; x++ {
			for y := 0; y < 4; y++ {
				// The executor's answer, in its own representation.
				l, r := newVec(1), newVec(1)
				setPlaneState(l, 0, x)
				setPlaneState(r, 0, y)
				if !logicalVec(ev, op, l, r) {
					t.Fatalf("%s %s %s: executor declined", names[x], op, names[y])
				}
				want := stateOf(l, 0)

				// The bitplane answer.
				xhi, xlo := make([]uint64, 1), make([]uint64, 1)
				yhi, ylo := make([]uint64, 1), make([]uint64, 1)
				ohi, olo := make([]uint64, 1), make([]uint64, 1)
				packPlanes([]uint8{uint8(x)}, xhi, xlo)
				packPlanes([]uint8{uint8(y)}, yhi, ylo)
				if op == "&&" {
					planeAnd(xhi, xlo, yhi, ylo, ohi, olo)
				} else {
					planeOr(xhi, xlo, yhi, ylo, ohi, olo)
				}
				got := int(ohi[0]&1)<<1 | int(olo[0]&1)
				if got != want {
					t.Errorf("%s %s %s: bitplanes say %s, executor says %s",
						names[x], op, names[y], names[got], names[want])
				}
			}
		}
	}
}

// TestShortCircuitNoOpExhaustive is the strong form of the claim that lets && and || vectorize at all:
// the vector executor SKIPS the short-circuit jump and always evaluates both operands, so for every one
// of the sixteen state pairs of both operators its answer must equal the scalar path's -- which does
// short-circuit, and therefore never even evaluates a right operand the left one settles.
//
// The executor's own differential test covers this incidentally, over whatever state pairs a
// twelve-record corpus happens to produce. This covers all thirty-two deliberately, which matters more
// than it looks: the table is left-biased, so `ERROR && FALSE` and `FALSE && ERROR` differ, and a
// combine that quietly symmetrised them would still pass a corpus that never put an ERROR on the left.
func TestShortCircuitNoOpExhaustive(t *testing.T) {
	src := map[int]string{stF: "false", stT: "true", stU: "undefined", stE: "error"}
	names := map[int]string{stF: "FALSE", stT: "TRUE", stU: "UNDEF", stE: "ERROR"}
	empty := &testSource{recs: []map[string]string{{}}, vals: []map[string]classad.Value{{}}}
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
					t.Errorf("%s %s %s: vector %s != scalar %s",
						names[x], op, names[y], got.value(0), q.Eval(classad.New()))
				}
			}
		}
	}
}

// TestPlaneAndDefinedMatchesGeneral checks the fast path agrees with the general one wherever it is
// allowed to run: both operands defined.
func TestPlaneAndDefinedMatchesGeneral(t *testing.T) {
	const n = 4096
	words := n / 64
	xhi, xlo := make([]uint64, words), make([]uint64, words)
	yhi, ylo := make([]uint64, words), make([]uint64, words)
	for w := 0; w < words; w++ {
		xlo[w] = uint64(w)*0x9e3779b97f4a7c15 | 1
		ylo[w] = uint64(w)*0xc2b2ae3d27d4eb4f | 3
	}
	gh, gl := make([]uint64, words), make([]uint64, words)
	planeAnd(xhi, xlo, yhi, ylo, gh, gl)
	fl := make([]uint64, words)
	planeAndDefined(xlo, ylo, fl)
	for w := 0; w < words; w++ {
		if gh[w] != 0 {
			t.Fatalf("word %d: general path produced undefined/error from defined inputs", w)
		}
		if gl[w] != fl[w] {
			t.Errorf("word %d: fast %#x != general %#x", w, fl[w], gl[w])
		}
	}
}

func setPlaneState(v Vec, i, st int) {
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

func stateOf(v Vec, i int) int {
	switch v.St[i] {
	case VsBool:
		if v.I[i] != 0 {
			return stT
		}
		return stF
	case VsUndef:
		return stU
	default:
		return stE
	}
}

// BenchmarkCombineRepresentation compares the executor's nine-bytes-per-element combine against the
// bitplane combine over a full-sized row group, across three input distributions.
//
// The distributions are the point. The executor's combine has a fast path for both-operands-boolean and
// falls to the per-element parity hook otherwise, so its cost depends entirely on how many UNDEFINED and
// ERROR values a block contains -- which is a property of the DATA, not the query. The bitplane combine
// is word-wise arithmetic that cannot branch, so its cost is identical in all three. A representation
// whose speed does not depend on how many records were missing an attribute is worth something beyond
// the raw multiple.
//
// The executor's combine writes into its left operand, so it needs its input restored each iteration.
// That restore is reported separately rather than hidden: needing it is a real property of a mutating
// byte-per-element representation, but the reader should be able to subtract it.
func BenchmarkCombineRepresentation(b *testing.B) {
	const n = 8192 // colGroupMaxRows: the largest row group
	words := n / 64
	ev := classad.NewEvaluator(nil)

	for _, dist := range []struct {
		name string
		// state returns the left and right operand states for record i.
		state func(i int) (int, int)
	}{
		// An escape-free column compared to a literal yields only TRUE or FALSE, which is what the block
		// already knows via escapeFree. The common case on real data.
		{"allboolean", func(i int) (int, int) {
			l, r := stF, stF
			if i%3 == 0 {
				l = stT
			}
			if i%5 == 0 {
				r = stT
			}
			return l, r
		}},
		// A few records missing the attribute.
		{"1pct_undefined", func(i int) (int, int) {
			l, r := stF, stF
			if i%3 == 0 {
				l = stT
			}
			if i%5 == 0 {
				r = stT
			}
			if i%97 == 0 {
				l = stU
			}
			if i%101 == 0 {
				r = stU
			}
			return l, r
		}},
		// A third of records not boolean: the shape that makes the executor's fast path miss constantly.
		{"33pct_mixed", func(i int) (int, int) {
			l := stF
			if i%3 == 0 {
				l = stT
			}
			if i%97 == 0 {
				l = stU
			}
			if i%991 == 0 {
				l = stE
			}
			return l, (l + 1) % 4
		}},
	} {
		srcL, srcR := newVec(n), newVec(n)
		lStates, rStates := make([]uint8, n), make([]uint8, n)
		for i := 0; i < n; i++ {
			ls, rs := dist.state(i)
			lStates[i], rStates[i] = uint8(ls), uint8(rs)
			setPlaneState(srcL, i, ls)
			setPlaneState(srcR, i, rs)
		}
		l, r := newVec(n), newVec(n)
		copy(r.St, srcR.St)
		copy(r.I, srcR.I)

		b.Run("executor+restore/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				copy(l.St, srcL.St)
				copy(l.I, srcL.I)
				logicalVec(ev, "&&", l, r)
			}
		})
		b.Run("restoreonly/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				copy(l.St, srcL.St)
				copy(l.I, srcL.I)
			}
		})

		xhi, xlo := make([]uint64, words), make([]uint64, words)
		yhi, ylo := make([]uint64, words), make([]uint64, words)
		ohi, olo := make([]uint64, words), make([]uint64, words)
		packPlanes(lStates, xhi, xlo)
		packPlanes(rStates, yhi, ylo)
		b.Run("bitplane/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				planeAnd(xhi, xlo, yhi, ylo, ohi, olo)
			}
		})
		b.Run("bitplanefast/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				planeAndDefined(xlo, ylo, olo)
			}
		})

		live := make([]uint64, words)
		for w := range live {
			live[w] = ^uint64(0)
		}
		b.Run("countbranch/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				c := 0
				for k := 0; k < n; k++ {
					if l.IsTrue(k) {
						c++
					}
				}
				_ = c
			}
		})
		b.Run("countpopcount/"+dist.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				_ = planeCountTrue(ohi, olo, live)
			}
		})
	}
}
