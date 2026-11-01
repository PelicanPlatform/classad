package vm

import (
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
)

// The dictionary kernels are the string path's whole point -- a comparison against a literal becomes an
// integer range test over codes -- and they are the part the scalar interpreter has no equivalent of, so
// they are also the part a differential test has to reach. The storage layer produces coded columns, but a
// test there can only compare match COUNTS; this fixture produces them inside the vm package, so the same
// per-record parity check the rest of the executor is held to applies to codes as well.

// foldDict is a DictResolver over an explicit entry list, with the contract the store's dictionary has:
// sorted by the evaluator's fold comparison with an exact-byte tiebreak, so the entries that compare equal
// to a literal are contiguous.
type foldDict struct{ entries []string }

func newFoldDict(vals []string) *foldDict {
	e := append([]string(nil), vals...)
	sort.SliceStable(e, func(i, j int) bool {
		if c := classad.CompareStringsFold(e[i], e[j]); c != 0 {
			return c < 0
		}
		return e[i] < e[j]
	})
	return &foldDict{entries: e}
}

func (d *foldDict) Len() int { return len(d.entries) }

func (d *foldDict) Range(lit string) (int, int, bool) {
	lo := sort.Search(len(d.entries), func(j int) bool {
		return classad.CompareStringsFold(d.entries[j], lit) >= 0
	})
	hi := lo + sort.Search(len(d.entries)-lo, func(j int) bool {
		return classad.CompareStringsFold(d.entries[lo+j], lit) > 0
	})
	return lo, hi, true
}

func (d *foldDict) At(code int) (string, bool) {
	if code < 0 || code >= len(d.entries) {
		return "", false
	}
	return d.entries[code], true
}

func (d *foldDict) code(s string) (int, bool) {
	for i, e := range d.entries {
		if e == s {
			return i, true
		}
	}
	return 0, false
}

// dictSource delivers the named attributes as dictionary CODES, and everything else exactly as
// testSource does. Each coded attribute gets its OWN dictionary, so a comparison between two of them
// is the cross-dictionary case that cannot be answered from the codes.
type dictSource struct {
	*testSource
	dicts map[string]*foldDict
	// Each column is built ONCE, so LoadColumn is three slice copies. A fixture that resolved a code per
	// record would be the thing a benchmark over this source measured -- which it was, at a hundred
	// nanoseconds a record for a kernel that should cost one.
	cols map[string]*dictColumn
}

type dictColumn struct {
	st   []uint8
	i    []int64
	s    []string
	dict *foldDict

	hasStr bool
}

func newDictSource(recs []map[string]string, coded ...string) *dictSource {
	s := &dictSource{testSource: newTestSource(recs), dicts: map[string]*foldDict{},
		cols: map[string]*dictColumn{}}
	names := map[string]bool{}
	for i := range recs {
		for k := range recs[i] {
			names[strings.ToLower(k)] = true
		}
	}
	isCoded := map[string]bool{}
	for _, name := range coded {
		// Folded, like testSource.vals and like a real source's intern table.
		name = strings.ToLower(name)
		isCoded[name] = true
		var vals []string
		seen := map[string]bool{}
		for i := range recs {
			// Through names: this loop iterates FOLDED names, while testSource.vals is keyed by the
			// spelling the record used.
			v, ok := s.vals[i][s.names[name]]
			if !ok || v.Type() != classad.StringValue {
				continue
			}
			str, _ := v.StringValue()
			if !seen[str] {
				seen[str] = true
				vals = append(vals, str)
			}
		}
		s.dicts[name] = newFoldDict(vals)
	}
	for name := range names {
		col := &dictColumn{st: make([]uint8, len(recs)), i: make([]int64, len(recs)),
			s: make([]string, len(recs))}
		d := s.dicts[name]
		col.dict = d
		scratch := &Vec{n: len(recs), I: col.i, S: col.s, St: col.st}
		for i := range recs {
			v, ok := s.vals[i][s.names[name]]
			switch {
			case !ok:
				col.st[i] = VsUndef
			case d != nil && v.Type() == classad.StringValue:
				str, _ := v.StringValue()
				c, found := d.code(str)
				if !found {
					panic("value not in its own dictionary: " + str)
				}
				col.st[i], col.i[i] = VsDictCode, int64(c)
			default:
				if !scratch.setValueData(i, v) {
					panic("unrepresentable value in " + name)
				}
			}
		}
		for i := range col.st {
			if col.st[i] == VsString {
				col.hasStr = true
				break
			}
		}
		s.cols[name] = col
	}
	return s
}

func (s *dictSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	if scope != ast.NoScope && scope != ast.MyScope {
		return false
	}
	col, ok := s.cols[strings.ToLower(name)]
	if !ok {
		for i := range s.recs {
			dst.St[i] = VsUndef
		}
		return true
	}
	dst.Dict = col.dict
	copy(dst.St, col.st)
	copy(dst.I, col.i)
	if col.hasStr {
		// Only when some element actually is a plain string: a coded column's string payload is empty,
		// and copying it anyway is 16 bytes per record of fixture work in front of every kernel.
		copy(dst.S, col.s)
	}
	return true
}

// dictCorpus puts two fold-EQUAL entries in the same dictionary ("user3" and "USER3"), which is the case
// the range test exists for: they are one contiguous run, == matches both and =?= matches one.
//
// D is a pure code column, so a comparison against a literal takes the range test. E is MIXED -- a missing
// value, a number where the schema says string -- so the range test declines it and the elementwise path
// has to resolve codes itself. D2 is a second dictionary, so D == D2 can only be answered after resolving.
func dictCorpus() *dictSource {
	return newDictSource([]map[string]string{
		{"D": `"user3"`, "D2": `"USER3"`, "E": `"user3"`, "A": "1"},
		{"D": `"USER3"`, "D2": `"user3"`, "E": `"zz"`, "A": "2"},
		{"D": `"user10"`, "D2": `"user10"`, "A": "3"},     // E missing
		{"D": `"user5"`, "D2": `"a"`, "E": "7", "A": "4"}, // E is a NUMBER in a string column
		{"D": `""`, "D2": `""`, "E": `""`, "A": "5"},
		{"D": `"User3"`, "D2": `"zz"`, "E": `"USER3"`, "A": "6"},
	}, "D", "D2", "E")
}

var dictExprs = []string{
	// the range test: every operator, against a literal whose fold-equal run has two entries
	`D == "user3"`, `D != "user3"`, `D < "user3"`, `D <= "user3"`, `D > "user3"`, `D >= "user3"`,
	`D == "USER3"`, `D == "User3"`, `D == "user5"`, `D == "nobody"`, `D == ""`,
	// the literal on the LEFT, which is the same predicate flipped
	`"user3" == D`, `"user3" != D`, `"user3" < D`, `"user3" <= D`, `"user3" > D`, `"user3" >= D`,
	`"nobody" == D`, `"" < D`,
	// codes against codes, across two different dictionaries
	`D == D2`, `D != D2`, `D < D2`, `D >= D2`, `D =?= D2`, `D =!= D2`,
	// a mixed column: the range test declines and every element is resolved or hooked
	`E == "user3"`, `E != "user3"`, `E < "user3"`, `"user3" == E`, `E == D`, `D == E`,
	// identity, which does NOT fold case
	`D =?= "user3"`, `D =!= "user3"`, `D =?= "USER3"`, `D =?= undefined`,
	// a code against a number, and a code in a logical position: both the hook's problem
	`D == 1`, `D < A`, `D && true`, `!D`,
	// combines, so the mask the range test wrote is consumed by the plane logic
	`D == "user3" && A > 2`, `D == "user3" || D == "user10"`, `D != "user3" && E == "user3"`,
	// string-VALUED results: the payload has to come back out of the dictionary
	`D`, `E`, `D + 1`,
	// BLENDED results. A conditional and an Elvis both copy a chosen element into a fresh vector, and a
	// code copied out of its dictionary is not a value -- see blendDictSource. `D ?: "zz"` covers the
	// Elvis path where the code comes from the SELECTOR rather than from a pushed path, and
	// `ifThenElse(A > 2, D, D2)` covers two dictionaries meeting in one result.
	`D ?: "zz"`, `Missing ?: D`, `ifThenElse(A > 2, D, "x")`, `ifThenElse(A > 2, D, D2)`,
	`ifThenElse(A > 2, D, E)`, `(D ?: "zz") == "user3"`, `(Missing ?: D) == "user10"`,
	`ifThenElse(A > 2, D, D2) =?= "user10"`,
}

// TestVecDictMatchesScalarPerRecord holds the dictionary kernels to the same per-record parity as the
// rest of the executor: same Program, same records, value by value.
func TestVecDictMatchesScalarPerRecord(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	var scratch VecScratch
	declined := 0
	for _, expr := range dictExprs {
		q, err := Parse(expr)
		if err != nil {
			t.Fatalf("parse %q: %v", expr, err)
		}
		got, ok := q.VecEval(src, n, &scratch)
		if !ok {
			declined++
			t.Logf("declined: %s", expr)
			continue
		}
		for i := 0; i < n; i++ {
			want := q.Eval(src.ad(i))
			if !sameValue(got.value(i), want) {
				t.Errorf("%q record %d: vector %s (state %d) != scalar %s",
					expr, i, got.value(i), got.St[i], want)
			}
		}
	}
	if declined > 0 {
		t.Errorf("%d/%d dictionary expressions declined", declined, len(dictExprs))
	}
	t.Logf("%d expressions x %d records verified against the scalar interpreter", len(dictExprs), n)
}

// TestVecDictCaseInsensitive pins the rule the range test depends on and the identity operators do not
// share: == folds case, =?= does not. Stated as exact match sets rather than as counts, so a record
// moving between them fails here.
func TestVecDictCaseInsensitive(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	for _, tc := range []struct {
		expr string
		want []int // the records whose result is TRUE
	}{
		{`D == "user3"`, []int{0, 1, 5}}, // user3, USER3, User3
		{`D == "USER3"`, []int{0, 1, 5}}, // the literal's own case must not matter
		{`"uSeR3" == D`, []int{0, 1, 5}}, // nor on the left
		{`D =?= "user3"`, []int{0}},      // identity is byte-exact
		{`D =?= "USER3"`, []int{1}},      //
		{`D != "user3"`, []int{2, 3, 4}}, //
		{`E == "user3"`, []int{0, 5}},    // the mixed column takes the elementwise path
	} {
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, n, nil)
		if !ok {
			t.Fatalf("%q declined", tc.expr)
		}
		var have []int
		for i := 0; i < n; i++ {
			if got.IsTrue(i) {
				have = append(have, i)
			}
		}
		if !sameInts(have, tc.want) {
			t.Errorf("%q matched %v, want %v", tc.expr, have, tc.want)
		}
	}
}

// TestVecDictResolvesExactEntry checks the value that comes BACK out of a code: within a run of
// fold-equal entries a code identifies one exactly, so reading a column must give the record's own
// spelling and not whichever entry the range happened to start at.
func TestVecDictResolvesExactEntry(t *testing.T) {
	src := dictCorpus()
	q, err := Parse("D")
	if err != nil {
		t.Fatal(err)
	}
	got, ok := q.VecEval(src, len(src.recs), nil)
	if !ok {
		t.Fatal("declined")
	}
	want := []string{"user3", "USER3", "user10", "user5", "", "User3"}
	for i, w := range want {
		s, err := got.value(i).StringValue()
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if s != w {
			t.Errorf("record %d resolved to %q, want %q", i, s, w)
		}
	}
}

// TestVecDictFlipIsTheSamePredicate is the specific claim behind flipping a literal on the left into the
// range test: for every operator and every literal in the dictionary, `lit OP col` and `col FLIP(OP) lit`
// must agree record for record. Exhaustive over the pair, in the style of the short-circuit test, because
// an operator table that symmetrised < and > would pass any corpus that only ever uses ==.
func TestVecDictFlipIsTheSamePredicate(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	lits := []string{"user3", "USER3", "user5", "user10", "", "zzz", "aaa"}
	flips := map[string]string{"<": ">", "<=": ">=", ">": "<", ">=": "<=", "==": "==", "!=": "!="}
	for _, col := range []string{"D", "E"} {
		for op, flipped := range flips {
			for _, lit := range lits {
				left := `"` + lit + `" ` + op + ` ` + col
				right := col + ` ` + flipped + ` "` + lit + `"`
				lq, err := Parse(left)
				if err != nil {
					t.Fatal(err)
				}
				rq, err := Parse(right)
				if err != nil {
					t.Fatal(err)
				}
				lv, ok := lq.VecEval(src, n, nil)
				if !ok {
					t.Fatalf("%q declined", left)
				}
				rv, ok := rq.VecEval(src, n, nil)
				if !ok {
					t.Fatalf("%q declined", right)
				}
				for i := 0; i < n; i++ {
					if !sameValue(lv.value(i), rv.value(i)) {
						t.Errorf("record %d: %q = %s but %q = %s",
							i, left, lv.value(i), right, rv.value(i))
					}
					// and both against the reference, so agreeing on the wrong answer fails too
					if want := lq.Eval(src.ad(i)); !sameValue(lv.value(i), want) {
						t.Errorf("record %d: %q = %s, scalar %s", i, left, lv.value(i), want)
					}
				}
			}
		}
	}
}

// TestVecDictBatchInvariant splits the same records into sub-batches. A dictionary is per BLOCK in the
// store, so a kernel that cached anything across batches -- a range, a resolved entry -- would answer a
// later batch from an earlier batch's dictionary.
func TestVecDictBatchInvariant(t *testing.T) {
	recs := dictCorpus().recs
	for _, expr := range []string{`D == "user3"`, `"user3" == D`, `D == D2`, `E == "user3"`} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		whole, ok := q.VecEval(newDictSource(recs, "D", "D2", "E"), len(recs), nil)
		if !ok {
			t.Fatalf("%q declined", expr)
		}
		var scratch VecScratch
		for _, batch := range []int{1, 2, 4} {
			for start := 0; start < len(recs); start += batch {
				end := start + batch
				if end > len(recs) {
					end = len(recs)
				}
				sub := newDictSource(recs[start:end], "D", "D2", "E")
				v, ok := q.VecEval(sub, end-start, &scratch)
				if !ok {
					t.Fatalf("%q batch %d declined", expr, batch)
				}
				for i := 0; i < end-start; i++ {
					if !sameValue(v.value(i), whole.value(start+i)) {
						t.Errorf("%q batch=%d record %d: %s != %s",
							expr, batch, start+i, v.value(i), whole.value(start+i))
					}
				}
			}
		}
	}
}

// TestVecDictStaleSlot checks that a stack slot which held a dictionary column does not resolve a LATER
// query's codes through it. The scratch is pooled across queries, so the slot genuinely is reused.
func TestVecDictStaleSlot(t *testing.T) {
	var scratch VecScratch
	dq, err := Parse(`D == "user3"`)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := dq.VecEval(dictCorpus(), len(dictCorpus().recs), &scratch); !ok {
		t.Fatal("declined")
	}
	// Now a query over plain values through the same scratch: no element is a code, so nothing may be
	// resolved through the dictionary the slot still points at.
	plain := vecCorpus()
	q, err := Parse(`S == "user3"`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := q.VecEval(plain, len(plain.recs), &scratch)
	if !ok {
		t.Fatal("declined")
	}
	for i := range plain.recs {
		if want := q.Eval(plain.ad(i)); !sameValue(got.value(i), want) {
			t.Errorf("record %d: %s != %s", i, got.value(i), want)
		}
	}
}

func sameInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestVecDictIdentityExhaustive is TestVecIdentityExhaustive for the coded representation: every ordered
// pair of string values, with the left side a dictionary column and the right side a SECOND dictionary.
//
// Identity is where the representation is most dangerous. It is case SENSITIVE, so it is the one operator
// that can tell two entries of a fold-equal run apart, and a code only means something next to the
// dictionary it came from -- comparing codes across two columns without resolving them made every record
// identical once already.
func TestVecDictIdentityExhaustive(t *testing.T) {
	vals := []string{`"a"`, `"A"`, `"ab"`, `"AB"`, `""`, `"qqq"`, `"zzz"`, "1", "undefined"}
	var recs []map[string]string
	for _, l := range vals {
		for _, r := range vals {
			recs = append(recs, map[string]string{"D": l, "D2": r})
		}
	}
	// One value only D2 carries, which SHIFTS its codes relative to D's. Without it the cross product
	// gives both columns the same dictionary, every equal pair has equal codes, and comparing codes
	// directly -- which is wrong, since a code only means something next to its own dictionary -- would
	// agree with comparing strings for every record in the test.
	recs = append(recs, map[string]string{"D": `"a"`, "D2": `"mmm"`})
	src := newDictSource(recs, "D", "D2")
	for _, expr := range []string{"D is D2", "D isnt D2", `D is "a"`, `"A" is D`, `D isnt ""`} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, len(recs), nil)
		if !ok {
			t.Fatalf("%q declined", expr)
		}
		for i := range recs {
			if want := q.Eval(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%q with D=%s D2=%s: vector %s != scalar %s",
					expr, recs[i]["D"], recs[i]["D2"], got.value(i), want)
			}
		}
	}
}

// TestVecDictBlendKeepsCodesCoded is the guard against a vacuous fix. Resolving every code to its
// string during a blend is CORRECT -- it is what the result does for every source whose dictionary it
// did not adopt -- so a blendDictSource that always returned nil would pass every parity test above
// while materializing a string per record, which is the cost the dictionary exists to avoid.
//
// So this pins the optimization rather than the semantics: with one dictionary in play the blend's
// result must still be CODES, carrying the resolver that assigned them.
func TestVecDictBlendKeepsCodesCoded(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	for _, expr := range []string{`D ?: "zz"`, `ifThenElse(A > 2, D, "x")`} {
		q, err := Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, n, nil)
		if !ok {
			t.Fatalf("%q declined", expr)
		}
		if got.Dict == nil {
			t.Errorf("%q: the blend dropped the dictionary; every code is being materialized", expr)
			continue
		}
		coded := 0
		for i := 0; i < n; i++ {
			if got.St[i] == VsDictCode {
				coded++
			}
		}
		if coded == 0 {
			t.Errorf("%q: no element came through as a code", expr)
		}
	}
}

// TestVecDictBlendAcrossDictionaries pins the case the per-element resolve exists for: two columns with
// SEPARATE dictionaries meeting in one result. Their codes number independently, so a result can hold at
// most one of them coded and the other has to arrive as a string -- and the two columns here are built so
// that a code read against the wrong dictionary gives a real string rather than an out-of-range error,
// which is what makes the failure a silently wrong value instead of a visible one.
func TestVecDictBlendAcrossDictionaries(t *testing.T) {
	src := dictCorpus()
	n := len(src.recs)
	q, err := Parse(`ifThenElse(A > 2, D, D2)`)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := q.VecEval(src, n, nil)
	if !ok {
		t.Fatal("declined")
	}
	// Taken from the scalar interpreter's answer rather than restated, so this cannot drift from it.
	for i := 0; i < n; i++ {
		want := q.Eval(src.ad(i))
		if !sameValue(got.value(i), want) {
			t.Errorf("record %d: vector %s != scalar %s", i, got.value(i), want)
		}
	}
	// And the mixed result really is mixed: if every element were resolved to a string this would pass
	// on values alone and say nothing about the fast path.
	var codes, strs int
	for i := 0; i < n; i++ {
		switch got.St[i] {
		case VsDictCode:
			codes++
		case VsString:
			strs++
		}
	}
	if codes == 0 || strs == 0 {
		t.Errorf("result has %d codes and %d strings; the two-dictionary blend should produce both", codes, strs)
	}
}
