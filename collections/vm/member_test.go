package vm

import (
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// memberScope is the ad the member tests evaluate in: one attribute of each kind a first argument or
// list element can read.
const memberScope = `[ A = 5; R = 5.0; B = true; S = "abc"; T = "ABC"; L = {1, 5}; N = [a = 5]; E = error; ` +
	`U = undefined; Big = 9007199254740993; Neg = -5; Z = -0.0 ]`

// memberFirstArgs and memberLists span the semantics member's compiled form has to reproduce:
// numeric coercion across int/real/bool, case folding (ASCII only), integers past 2^53, NaN and
// signed zero, UNDEFINED/ERROR on either side, a list or ad where a scalar belongs, and list
// elements that are not constants (which must stay delegated).
var (
	memberFirstArgs = []string{
		"5", "5.0", "true", "false", "1", "0", "1.0", "0.0", "-0.0", `"abc"`, `"ABC"`, `"aBc"`, `""`,
		`"é"`, `"É"`, "undefined", "error", "{5}", "[a=5]", "Missing", "A", "R", "B", "S", "T", "L",
		"N", "E", "U", "Big", "Neg", "Z", "9007199254740993", "9007199254740992", "9007199254740992.0",
		`real("NaN")`, `real("INF")`, `-real("INF")`, "-5", "A + 0", "S ?: 1", `strcat("a", "bc")`,
	}
	memberLists = []string{
		"{1, 5}", "{5.0}", "{true}", "{false}", "{1}", "{0}", "{1.0}", `{"abc"}`, `{"ABC"}`, `{"é"}`,
		"{}", "{undefined}", "{error}", `{1, "abc"}`, `{"abc", 5}`, "{undefined, 5}", "{error, 5}",
		"{9007199254740992}", "{9007199254740992.0}", "{9007199254740993}", "{-0.0}", "{0.0}",
		"{-5}", "{(-5)}", "{-(-5)}", "{+5}", `{-"x"}`, "{!true}", `{real("NaN")}`, `{real("INF")}`,
		`{""}`, "{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}",
		// Not constants: these stay delegated.
		"{{5}}", "{[a=5]}", "{A}", "{1+4}", "{Missing}", "{E}", "{U}",
		// Not literal lists at all.
		"undefined", "error", "5", `"abc"`, "L", "Missing", "N", "S",
	}
	memberFuncs = []string{"member", "identicalMember", "MEMBER", "IdenticalMember"}
)

// TestMemberCompiledMatchesEvaluator holds the compiled member (OpMember) to the tree-walker over
// every first argument x list x spelling above -- and, so the comparison is not vacuous, checks that
// every call over a constant list actually compiled to OpMember rather than being delegated.
func TestMemberCompiledMatchesEvaluator(t *testing.T) {
	scope, err := classad.Parse(memberScope)
	if err != nil {
		t.Fatal(err)
	}
	native := 0
	for _, fn := range memberFuncs {
		for _, x := range memberFirstArgs {
			for _, l := range memberLists {
				src := fmt.Sprintf("%s(%s, %s)", fn, x, l)
				expr, err := parser.ParseExpr(src)
				if err != nil {
					t.Fatalf("parse %q: %v", src, err)
				}
				p := CompileProgram(expr)
				_, constList := constListOf(expr.(*ast.FunctionCall).Args[1])
				if usesOp(p, OpMember) != constList {
					t.Errorf("%s: OpMember compiled = %v, constant list = %v", src, usesOp(p, OpMember), constList)
				}
				if usesOp(p, OpMember) {
					native++
				}
				want := refEval(scope, expr)
				if got := Run(p, scope); !valuesEqual(want, got) {
					t.Errorf("%s: compiled %s, evaluator %s", src, describe(got), describe(want))
				}
			}
		}
	}
	if native < 1000 {
		t.Fatalf("only %d calls compiled to OpMember; the test is not exercising it", native)
	}
}

// TestMemberCompiledRandom compares the compiled member against the tree-walker on random lists of
// up to 300 constants and random first arguments drawn from the same value pool, so the set lookups
// are tested at the sizes they are built for (the hand-written table above stays small).
func TestMemberCompiledRandom(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	scope := classad.New()
	for i := 0; i < 400; i++ {
		fn := memberFuncs[rng.Intn(2)]
		n := []int{0, 1, 2, 10, 300}[rng.Intn(5)]
		elems := make([]string, n)
		for j := range elems {
			elems[j] = randomMemberConst(rng)
			if strings.HasPrefix(elems[j], "real(") {
				elems[j] = "-0.0" // a call is not a constant; NaN can only be the first argument
			}
		}
		list := "{" + strings.Join(elems, ", ") + "}"
		for k := 0; k < 25; k++ {
			x := randomMemberConst(rng)
			if n > 0 && rng.Intn(3) == 0 {
				x = elems[rng.Intn(n)] // a guaranteed candidate, so TRUE is common
			}
			src := fmt.Sprintf("%s(%s, %s)", fn, x, list)
			expr, err := parser.ParseExpr(src)
			if err != nil {
				t.Fatalf("parse %q: %v", src, err)
			}
			p := CompileProgram(expr)
			if !usesOp(p, OpMember) {
				t.Fatalf("%s: not compiled to OpMember", src)
			}
			want := refEval(scope, expr)
			if got := Run(p, scope); !valuesEqual(want, got) {
				t.Fatalf("%s: compiled %s, evaluator %s", src, describe(got), describe(want))
			}
		}
	}
}

// randomMemberConst draws a constant from a deliberately collision-prone pool: small integers and
// their real and boolean twins, case variants of a few strings, values around 2^53, and UNDEFINED and
// ERROR.
func randomMemberConst(rng *rand.Rand) string {
	switch rng.Intn(9) {
	case 0, 1:
		return strconv.Itoa(rng.Intn(20) - 5)
	case 2:
		return strconv.FormatFloat(float64(rng.Intn(20)-5), 'f', 1, 64)
	case 3:
		return strconv.FormatFloat(float64(rng.Intn(40))/4, 'f', 2, 64)
	case 4:
		return []string{"true", "false"}[rng.Intn(2)]
	case 5:
		return strconv.Quote([]string{"a", "A", "ab", "AB", "aB", "", "x1", "X1", "é", "É"}[rng.Intn(10)])
	case 6:
		return strconv.FormatInt(9007199254740992+int64(rng.Intn(5)-2), 10)
	case 7:
		return []string{"9007199254740992.0", "9007199254740994.0", `real("NaN")`, "-0.0"}[rng.Intn(4)]
	default:
		return []string{"undefined", "error"}[rng.Intn(2)]
	}
}

func usesOp(p *Program, op Opcode) bool {
	return slices.ContainsFunc(p.code, func(in Instr) bool { return in.Op == op })
}

// TestMemberNativeQuery checks the query-level consequence: a member over a constant list no longer
// delegates, so the query is Native and every columnar and resolver-based path can take it.
//
// A member whose list argument is a LIST LITERAL that is not constant still delegates, because the
// literal itself compiles to OpEvalNode. One whose list argument is an attribute REFERENCE no longer
// does -- see the row below.
func TestMemberNativeQuery(t *testing.T) {
	for _, tc := range []struct {
		src    string
		native bool
	}{
		{`member(ClusterId, {1, 2, 3}) && (JobStatus == 1 || JobStatus == 2)`, true},
		{`identicalMember(Owner, {"alice", "bob"})`, true},
		{`member(ClusterId, {1, -2, (3)})`, true},
		{`member(ClusterId, {1, Other})`, false}, // a non-constant LIST LITERAL is OpEvalNode
		{`member(ClusterId)`, false},             // wrong arity: ERROR without evaluating args
		{`member(ClusterId, {1}, 2)`, false},     // likewise
		// A list-valued attribute REFERENCE is native as of OpCallFunc: member is a strict built-in,
		// so both arguments are OpLoadRef and the call is a value operation -- nothing is delegated.
		// Native does not mean servable, and the columnar paths still refuse this one up front rather
		// than part way through: SomeList is not a schema field, so newColPrefilter rejects it on the
		// read plan and LoadColumn declines it. A decline, not a delegation.
		{`member(ClusterId, SomeList)`, true},
	} {
		q, err := Parse(tc.src)
		if err != nil {
			t.Fatal(err)
		}
		if q.Native() != tc.native {
			t.Errorf("%s: Native = %v, want %v", tc.src, q.Native(), tc.native)
		}
	}
}

// memberDict is a DictResolver over a column's distinct strings, sorted the way a block's dictionary
// is: by the fold comparison, ties broken by the exact bytes, so the entries == to a literal are one
// contiguous run.
type memberDict struct{ entries []string }

func newMemberDict(vals []string) *memberDict {
	seen := map[string]bool{}
	var d memberDict
	for _, v := range vals {
		if !seen[v] {
			seen[v] = true
			d.entries = append(d.entries, v)
		}
	}
	sort.Slice(d.entries, func(i, j int) bool {
		if c := classad.CompareStringsFold(d.entries[i], d.entries[j]); c != 0 {
			return c < 0
		}
		return d.entries[i] < d.entries[j]
	})
	return &d
}

func (d *memberDict) Range(lit string) (int, int, bool) {
	lo := sort.Search(len(d.entries), func(j int) bool { return classad.CompareStringsFold(d.entries[j], lit) >= 0 })
	hi := sort.Search(len(d.entries), func(j int) bool { return classad.CompareStringsFold(d.entries[j], lit) > 0 })
	return lo, hi, true
}

func (d *memberDict) At(code int) (string, bool) {
	if code < 0 || code >= len(d.entries) {
		return "", false
	}
	return d.entries[code], true
}

func (d *memberDict) Len() int { return len(d.entries) }

func (d *memberDict) code(s string) int {
	for i, e := range d.entries {
		if e == s {
			return i
		}
	}
	panic("not in dictionary: " + s)
}

// memberVecSource is testSource plus dictionary-encoded columns: an attribute named D* whose records
// all hold strings (or nothing) is served as codes into a memberDict, the way a block serves a
// dictionary-encoded string column. Absent records are UNDEFINED, as an escaped-missing value is.
type memberVecSource struct {
	*testSource
	dicts map[string]*memberDict
}

func newMemberVecSource(recs []map[string]string) *memberVecSource {
	s := &memberVecSource{testSource: newTestSource(recs), dicts: map[string]*memberDict{}}
	for _, r := range recs {
		for k := range r {
			if !strings.HasPrefix(k, "D") || s.dicts[k] != nil {
				continue
			}
			var vals []string
			for i := range recs {
				if v, ok := s.vals[i][k]; ok {
					str, err := v.StringValue()
					if err != nil {
						panic("dictionary column " + k + " holds a non-string")
					}
					vals = append(vals, str)
				}
			}
			s.dicts[k] = newMemberDict(vals)
		}
	}
	return s
}

func (s *memberVecSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	d := s.dicts[name]
	if d == nil {
		dst.Dict = nil
		return s.testSource.LoadColumn(name, scope, dst)
	}
	dst.Dict = d
	for i := range s.recs {
		v, ok := s.vals[i][name]
		if !ok {
			dst.St[i] = VsUndef
			continue
		}
		str, _ := v.StringValue()
		dst.St[i], dst.I[i] = VsDictCode, int64(d.code(str))
	}
	return true
}

// memberVecCorpus puts every element state the vector kernel distinguishes into the columns member
// reads: integers and their real/boolean twins, large integers, NaN-free reals, case-variant strings
// both plain (S) and dictionary-encoded (D), UNDEFINED, ERROR and absent values.
func memberVecCorpus() *memberVecSource {
	ints := []string{"1", "2", "5", "-5", "0", "9007199254740993", "9007199254740992", "300", "299"}
	reals := []string{"1.0", "5.0", "2.5", "-0.0", "9007199254740992.0"}
	strs := []string{`"abc"`, `"ABC"`, `"aBc"`, `"x1"`, `"X1"`, `""`, `"é"`, `"É"`}
	var recs []map[string]string
	for i := 0; i < 70; i++ {
		r := map[string]string{
			"A": ints[i%len(ints)],
			"R": reals[i%len(reals)],
			"S": strs[i%len(strs)],
			"D": strs[(i*3)%len(strs)],
			"M": []string{"5", "5.0", "true", `"abc"`, "undefined", "error", "false", "1"}[i%8],
		}
		switch i % 11 {
		case 3:
			delete(r, "A")
			delete(r, "D")
		case 7:
			r["A"] = "undefined"
		case 9:
			r["A"] = "error"
		}
		recs = append(recs, r)
	}
	return newMemberVecSource(recs)
}

var memberVecLists = []string{
	"{1, 5}", "{5.0}", "{true}", "{false}", "{1.0, 2.5}", "{-5, 0}", "{9007199254740992}",
	"{9007199254740992.0}", "{9007199254740993}", `{"abc"}`, `{"ABC", "x1"}`, `{"é"}`, `{""}`,
	`{1, "abc"}`, "{undefined}", "{error}", "{undefined, error, 5}", "{}", "{-0.0}",
	"{" + intRange(0, 300) + "}",
}

func intRange(lo, hi int) string {
	parts := make([]string, 0, hi-lo)
	for i := lo; i < hi; i++ {
		parts = append(parts, strconv.Itoa(i))
	}
	return strings.Join(parts, ", ")
}

// TestMemberVecMatchesScalar is the vector executor's parity test for OpMember: per record, the
// vectorized member equals the scalar interpreter's -- for plain and dictionary-encoded strings,
// every numeric state, UNDEFINED, ERROR, a constant first argument, and member inside a larger
// expression. It also requires every one of these to vectorize, so a kernel that declined would fail
// rather than pass vacuously.
func TestMemberVecMatchesScalar(t *testing.T) {
	src := memberVecCorpus()
	n := len(src.recs)
	var scratch VecScratch
	for _, fn := range []string{"member", "identicalMember"} {
		for _, attr := range []string{"A", "R", "S", "D", "M", "Missing", "5", `"ABC"`, "undefined", "error"} {
			for _, l := range memberVecLists {
				for _, shape := range []string{"%s(%s, %s)", "%s(%s, %s) && A > 0", "!%s(%s, %s) || R == 5"} {
					expr := fmt.Sprintf(shape, fn, attr, l)
					q, err := Parse(expr)
					if err != nil {
						t.Fatalf("parse %q: %v", expr, err)
					}
					got, ok := q.VecEval(src, n, &scratch)
					if !ok {
						t.Fatalf("%s: declined", expr)
					}
					for i := 0; i < n; i++ {
						want := q.Eval(src.ad(i))
						if !sameValue(got.value(i), want) {
							t.Fatalf("%s record %d: vector %s, scalar %s", expr, i, got.value(i), want)
						}
					}
				}
			}
		}
	}
}

// TestMemberVecDictBothStrategies drives memberCodes down both of its fill strategies -- locating each
// literal by Range, and scanning every dictionary entry -- and requires the same per-record answers,
// since which one runs depends only on the list and dictionary sizes.
func TestMemberVecDictBothStrategies(t *testing.T) {
	var recs []map[string]string
	for i := 0; i < 200; i++ {
		s := fmt.Sprintf("u%03d", i%150)
		if i%4 == 0 {
			s = strings.ToUpper(s)
		}
		recs = append(recs, map[string]string{"D": strconv.Quote(s)})
	}
	src := newMemberVecSource(recs)
	var scratch VecScratch
	short := `{"U008", "u101"}` // 2 literals against ~190 entries: Range per literal
	parts := make([]string, 0, 120)
	for i := 0; i < 240; i += 2 {
		parts = append(parts, strconv.Quote(fmt.Sprintf("U%03d", i)))
	}
	long := "{" + strings.Join(parts, ", ") + "}" // 120 literals: a scan of the entries
	for _, fn := range []string{"member", "identicalMember"} {
		for _, l := range []string{short, long} {
			expr := fmt.Sprintf("%s(D, %s)", fn, l)
			q, err := Parse(expr)
			if err != nil {
				t.Fatal(err)
			}
			got, ok := q.VecEval(src, len(recs), &scratch)
			if !ok {
				t.Fatalf("%s: declined", expr)
			}
			matches := 0
			for i := range recs {
				want := q.Eval(src.ad(i))
				if !sameValue(got.value(i), want) {
					t.Fatalf("%s record %d: vector %s, scalar %s", expr, i, got.value(i), want)
				}
				if got.IsTrue(i) {
					matches++
				}
			}
			if matches == 0 {
				t.Fatalf("%s: no record matched; the fixture does not exercise the dictionary", expr)
			}
		}
	}
}

// TestMemberProbe pins the planner's reading of member: an "in" probe for member and identicalMember
// over a self-scoped attribute and a list of constants, nothing for anything else, and the exactness
// ExactProbes reports -- member is exact, identicalMember only a superset.
func TestMemberProbe(t *testing.T) {
	for _, tc := range []struct {
		src   string
		want  string // rendered probes, "" for none
		exact bool
	}{
		{`member(ClusterId, {1, 2, -3})`, "ClusterId in [1 2 -3]", true},
		{`MEMBER(MY.ClusterId, {(4)})`, "ClusterId in [4]", true},
		{`member(Owner, {"a", "B"})`, `Owner in ["a" "B"]`, true},
		{`member(X, {1, "a", 2.5, true})`, `X in [1 "a" 2.5 true]`, true},
		// UNDEFINED and ERROR elements are never == to anything: dropped.
		{`member(X, {undefined, 1, error})`, "X in [1]", true},
		{`member(X, {undefined})`, "", false},
		{`member(X, {})`, "", false},
		// identicalMember: a superset probe, and none at all when the list holds UNDEFINED/ERROR.
		{`identicalMember(X, {1, 2})`, "X in [1 2]", false},
		{`identicalMember(X, {undefined, 1})`, "", false},
		{`identicalMember(X, {error})`, "", false},
		// Not a constant list, or not a self-scoped attribute.
		{`member(X, {1, Y})`, "", false},
		{`member(X, L)`, "", false},
		{`member(X, {1+1})`, "", false},
		{`member(TARGET.X, {1})`, "", false},
		{`member(X + 1, {1})`, "", false},
		{`!member(X, {1})`, "", false},
		// Conjunctions: exact only when every conjunct is.
		{`member(ClusterId, {1, 2}) && (JobStatus == 1 || JobStatus == 2)`, "ClusterId in [1 2], JobStatus in [1 2]", true},
		{`member(ClusterId, {1, 2}) && identicalMember(Owner, {"a"})`, `ClusterId in [1 2], Owner in ["a"]`, false},
		// An OR-of-equalities mixing strings and numbers: ERROR || TRUE is ERROR, so not exact.
		{`X == 1 || X == "a"`, `X in [1 "a"]`, false},
		{`X == "a" || X == "b"`, `X in ["a" "b"]`, true},
		{`X == 1 || X == 2.5 || X == true`, `X in [1 2.5 true]`, true},
	} {
		q, err := Parse(tc.src)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.src, err)
		}
		if got := renderProbes(q.Probes()); got != tc.want {
			t.Errorf("Probes(%s) = %q, want %q", tc.src, got, tc.want)
		}
		if _, exact := q.ExactProbes(); exact != tc.exact {
			t.Errorf("ExactProbes(%s) exact = %v, want %v", tc.src, exact, tc.exact)
		}
	}
}

func renderProbes(ps []Probe) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		vals := make([]string, 0, len(p.Vals))
		for _, v := range p.Vals {
			vals = append(vals, v.String())
		}
		parts = append(parts, fmt.Sprintf("%s %s [%s]", p.Attr, p.Op, strings.Join(vals, " ")))
	}
	return strings.Join(parts, ", ")
}

// TestMemberProbeIsExactOnRecords checks the claim ExactProbes makes for member, record by record:
// where it says exact, the conjunct is TRUE for a value exactly when the value is == to one of the
// probe's values. The values include every type member treats specially.
func TestMemberProbeIsExactOnRecords(t *testing.T) {
	values := []string{"1", "1.0", "true", "false", "0", "2.5", `"a"`, `"A"`, `"b"`, "undefined", "error",
		"{1}", "[q = 1]", "9007199254740993", "9007199254740992.0", "-0.0", `real("NaN")`}
	for _, src := range []string{
		`member(X, {1, "a", 2.5})`, `member(X, {true, -0.0})`, `member(X, {9007199254740992.0, undefined})`,
		`X == "a" || X == "b"`, `X == 1 || X == 2.5`,
	} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		probes, exact := q.ExactProbes()
		if !exact || len(probes) != 1 {
			t.Fatalf("%s: want one exact probe, got %v exact=%v", src, probes, exact)
		}
		for _, v := range values {
			ad, err := classad.Parse("[ X = " + v + " ]")
			if err != nil {
				t.Fatal(err)
			}
			xv := ad.EvaluateAttr("X")
			inSet := false
			for _, pv := range probes[0].Vals {
				eq := classad.NewEvaluator(nil).ApplyBinaryOp("==", xv, pv)
				if b, err := eq.BoolValue(); err == nil && b {
					inSet = true
				}
			}
			res := refEval(ad, q.Expr())
			matched := res.IsBool()
			if matched {
				matched, _ = res.BoolValue()
			}
			if matched != inSet {
				t.Errorf("%s with X = %s: query is %s, probe says %v", src, v, describe(res), inSet)
			}
		}
	}
}

// TestMemberPreferredOverGenericCall pins the compiler's preference order. member over a constant
// list is ALSO a strict built-in, so it would compile to a generic OpCallFunc if emitMember were not
// tried first -- and that would be a silent performance regression, not a test failure: memberVec
// tests a per-block bitset over dictionary codes, where the generic path boxes every element and
// rescans the list per record.
func TestMemberPreferredOverGenericCall(t *testing.T) {
	for _, src := range []string{
		`member(ClusterId, {1, 2, 3})`,
		`identicalMember(Owner, {"alice", "bob"})`,
	} {
		q, err := Parse(src)
		if err != nil {
			t.Fatal(err)
		}
		p := q.Program()
		if !usesOp(p, OpMember) {
			t.Errorf("%s: want the specialized OpMember", src)
		}
		if usesOp(p, OpCallFunc) {
			t.Errorf("%s: compiled to the generic OpCallFunc; emitMember must be tried first", src)
		}
	}
	// And the generic call is what a strict built-in with no specialization gets.
	q, err := Parse(`toUpper(Owner)`)
	if err != nil {
		t.Fatal(err)
	}
	if !usesOp(q.Program(), OpCallFunc) {
		t.Error(`toUpper(Owner): want OpCallFunc`)
	}
}
