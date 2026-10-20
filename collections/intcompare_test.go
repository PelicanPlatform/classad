package collections

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Every path that answers a numeric comparison must agree with the comparison operators on integers
// past 2^53, where neighboring integers share a float64. The operators compare two integers as int64
// and an integer against a real as float64 (the reference's doComparison). The value index, segment
// stats, segment zones and block zones store float64; the columnar count, aggregate and WHERE-count
// paths compare float64. Those are exact only for integer probes inside (-2^53, 2^53), so a larger
// integer probe has to widen what a pruning structure keeps and make an exact path decline.

const p53 = int64(1) << 53

// bigNum is one stored or probed value: an integer or a real.
type bigNum struct {
	isInt bool
	i     int64
	f     float64
}

func (v bigNum) text() string {
	if v.isInt {
		return strconv.FormatInt(v.i, 10)
	}
	return strconv.FormatFloat(v.f, 'f', 1, 64)
}

func (v bigNum) float() float64 {
	if v.isInt {
		return float64(v.i)
	}
	return v.f
}

func bigInt(i int64) bigNum    { return bigNum{isInt: true, i: i} }
func bigReal(f float64) bigNum { return bigNum{f: f} }
func parseBig(s string) bigNum {
	if strings.Contains(s, ".") {
		f, _ := strconv.ParseFloat(s, 64)
		return bigReal(f)
	}
	i, _ := strconv.ParseInt(s, 10, 64)
	return bigInt(i)
}

// bigHolds is the comparison operators' rule for `x op c`, restated independently of the engine.
func bigHolds(op string, x, c bigNum) bool {
	if op == "=?=" {
		return x.isInt == c.isInt && (x.isInt && x.i == c.i || !x.isInt && x.f == c.f)
	}
	var cmp int
	if x.isInt && c.isInt {
		switch {
		case x.i < c.i:
			cmp = -1
		case x.i > c.i:
			cmp = 1
		}
	} else {
		a, b := x.float(), c.float()
		switch {
		case a < b:
			cmp = -1
		case a > b:
			cmp = 1
		case a != b:
			return op == "!=" // NaN; not in this data
		}
	}
	switch op {
	case "<":
		return cmp < 0
	case "<=":
		return cmp <= 0
	case ">":
		return cmp > 0
	case ">=":
		return cmp >= 0
	case "==":
		return cmp == 0
	}
	return cmp != 0
}

// bigValues are the stored values: integers on both sides of +-2^53 and near the int64 limits, and
// the reals those integers round to.
var bigValues = []bigNum{
	bigInt(p53 - 2), bigInt(p53 - 1), bigInt(p53), bigInt(p53 + 1), bigInt(p53 + 2), bigInt(p53 + 3),
	bigInt(1<<60 + 1), bigInt(math.MaxInt64 - 1), bigInt(math.MaxInt64),
	bigInt(-p53 + 1), bigInt(-p53), bigInt(-p53 - 1),
	bigInt(math.MinInt64 + 1), bigInt(math.MinInt64), bigInt(0), bigInt(1000),
	bigReal(float64(p53)), bigReal(float64(p53 + 2)), bigReal(0.5),
}

// bigConstraints pairs every operator with literals on and around the stored values, plus
// conjunctions the planner folds into one band or equality.
func bigConstraints() []string {
	lits := []string{
		strconv.FormatInt(p53, 10), strconv.FormatInt(p53+1, 10), strconv.FormatInt(p53+2, 10),
		strconv.FormatInt(p53-1, 10), strconv.FormatInt(math.MaxInt64-1, 10),
		strconv.FormatInt(1<<60, 10), "1000", "9007199254740992.0", "9007199254740994.0",
	}
	var out []string
	for _, op := range []string{"<", "<=", ">", ">=", "==", "!=", "=?="} {
		for _, l := range lits {
			out = append(out, "N "+op+" "+l)
		}
	}
	return append(out,
		"N > 9007199254740992 && N < 9007199254740994",
		"N >= 9007199254740993 && N <= 9007199254740993",
		"N == 9007199254740993 && N > 9007199254740992",
		"N == 9007199254740993 && N == 9007199254740992",
		"N > 9007199254740991 && N < 9007199254740993",
		"N > 1000 && N < 9223372036854775807",
	)
}

// bigTruth evaluates a constraint of the shapes above directly on the stored values.
func bigTruth(t *testing.T, constraint string, vals []bigNum) (n int, matched []bigNum) {
	t.Helper()
	type cond struct {
		op string
		c  bigNum
	}
	var conds []cond
	for _, part := range strings.Split(constraint, " && ") {
		f := strings.Fields(part)
		if len(f) != 3 || f[0] != "N" {
			t.Fatalf("bigTruth cannot read %q", part)
		}
		conds = append(conds, cond{f[1], parseBig(f[2])})
	}
	for _, v := range vals {
		ok := true
		for _, c := range conds {
			ok = ok && bigHolds(c.op, v, c.c)
		}
		if ok {
			n++
			matched = append(matched, v)
		}
	}
	return n, matched
}

// bigFixtureVals is the stored value of every record, in order: a run of each value, so each sealed
// segment of a small-segment archive holds few distinct values and its zone is narrow. The reals get
// short runs, so the columnar schema still carries N as an integer field and they escape it.
func bigFixtureVals(per int) []bigNum {
	var out []bigNum
	for _, v := range bigValues {
		n := per
		if !v.isInt {
			n = per / 12
		}
		for range n {
			out = append(out, v)
		}
	}
	return out
}

func bigAd(t *testing.T, i int, v bigNum) *classad.ClassAd {
	t.Helper()
	ad, err := classad.ParseOld(fmt.Sprintf("N = %s\nG = \"g%d\"\nM = %d\nPad = \"%s%d\"",
		v.text(), i%3, i%3, strings.Repeat("x", 48), i))
	if err != nil {
		t.Fatal(err)
	}
	if v.isInt && v.i < 0 {
		// The old syntax writes a negative integer as unary minus over a literal; store the value
		// itself so the columns and zones see a number rather than an expression.
		ad.InsertAttr("N", v.i)
	}
	return ad
}

func countSeq[T any](seq func(func(T) bool)) int {
	n := 0
	for range seq {
		n++
	}
	return n
}

// checkBigPaths compares every answer the store offers for a constraint with the truth. A path that
// declines is fine; a path that answers must be exact.
func checkBigPaths(t *testing.T, label string, c *Collection, vals []bigNum, served map[string]int) {
	t.Helper()
	for _, constraint := range bigConstraints() {
		want, matched := bigTruth(t, constraint, vals)
		q, err := vm.Parse(constraint)
		if err != nil {
			t.Fatal(err)
		}
		expr, err := classad.ParseExpr(constraint)
		if err != nil {
			t.Fatal(err)
		}
		check := func(path string, got int, ok bool) {
			if !ok {
				return
			}
			served[path]++
			if got != want {
				t.Errorf("%s %s: %s = %d, want %d", label, constraint, path, got, want)
			}
		}
		// The evaluator and the full scan, as the baseline every other path is held to.
		all, _ := vm.Parse("true")
		evalN := 0
		for ad := range c.Query(all) {
			if b, err := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); err == nil && b {
				evalN++
			}
		}
		check("evaluator", evalN, true)
		check("Query", countSeq(c.Query(q)), true)
		n, ok := c.CountQuery(q)
		check("CountQuery", n, ok)
		n, ok = c.VectorEvalCount(q)
		check("VectorEvalCount", n, ok)
		n, ok = c.ColumnarEvalCount(q)
		check("ColumnarEvalCount", n, ok)
		if gc, ok := c.GroupCountConstraint(constraint, "M"); ok {
			sum := 0
			for _, g := range gc {
				sum += g.Count
			}
			check("GroupCountConstraint", sum, true)
		}
		if ns, ok := c.NumStatsQuery(q, "N"); ok {
			check("NumStatsQuery.N", ns.N, true)
			checkBigExtremes(t, label+" "+constraint, ns, matched)
		}
		if c.appendOnly() {
			if gc, ok := c.categoricalGroupCountsWhere("G", constraint); ok {
				sum := 0
				for _, v := range gc {
					sum += int(v)
				}
				check("CategoricalGroupCountsWhere", sum, true)
			}
		}
	}
}

// checkBigExtremes holds a columnar MIN/MAX to the reference's: exact int64 extremes for an
// all-integer set, the float64 extremes once a real is among them.
func checkBigExtremes(t *testing.T, label string, ns NumStats, matched []bigNum) {
	t.Helper()
	if len(matched) == 0 {
		return
	}
	anyReal := false
	minI, maxI := int64(math.MaxInt64), int64(math.MinInt64)
	minF, maxF := math.Inf(1), math.Inf(-1)
	for _, v := range matched {
		if !v.isInt {
			anyReal = true
		} else {
			if v.i < minI {
				minI = v.i
			}
			if v.i > maxI {
				maxI = v.i
			}
		}
		minF, maxF = math.Min(minF, v.float()), math.Max(maxF, v.float())
	}
	if ns.AnyReal != anyReal {
		t.Errorf("%s: AnyReal = %v, want %v", label, ns.AnyReal, anyReal)
		return
	}
	if anyReal {
		if ns.Min != minF || ns.Max != maxF {
			t.Errorf("%s: Min/Max = %v/%v, want %v/%v", label, ns.Min, ns.Max, minF, maxF)
		}
		return
	}
	if ns.IntMin != minI || ns.IntMax != maxI {
		t.Errorf("%s: IntMin/IntMax = %d/%d, want %d/%d", label, ns.IntMin, ns.IntMax, minI, maxI)
	}
}

// TestBigIntPathsAgreeArchive runs the comparison on archives with per-segment zones on N, one with a
// value index on N and one without (the index takes over from zone pruning where it applies), each
// with a categorical index on G for the WHERE group counts, then with the columnar accelerator.
func TestBigIntPathsAgreeArchive(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts ArchiveOptions
	}{
		{"indexed", ArchiveOptions{ValueAttrs: []string{"N"}, CategoricalAttrs: []string{"G"}}},
		{"zoned", ArchiveOptions{ZoneAttrs: []string{"N"}, CategoricalAttrs: []string{"G"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			opts.Dir, opts.SegmentSize = t.TempDir(), 16<<10
			a, err := OpenArchive(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			vals := bigFixtureVals(120)
			for i, v := range vals {
				if err := a.Append(bigAd(t, i, v)); err != nil {
					t.Fatal(err)
				}
			}
			a.Reindex()
			if with, _, _ := sealedZoneStates(a, "N"); with < 10 {
				t.Fatalf("%d sealed segments with an N zone; the fixture needs many", with)
			}
			served := map[string]int{}
			checkBigPaths(t, tc.name, a.c, vals, served)
			if !a.BuildAndEnableSchemaScan(4000, 4) {
				t.Fatal("the archive did not build its columnar accelerator")
			}
			checkBigPaths(t, tc.name+"+columnar", a.c, vals, served)
			requireServed(t, served, "Query", "CountQuery", "VectorEvalCount", "ColumnarEvalCount",
				"GroupCountConstraint", "NumStatsQuery.N", "CategoricalGroupCountsWhere")
		})
	}
}

// TestBigIntPathsAgreeCollection runs it on a mutable collection with a value index on N, before and
// after the columnar accelerator is built.
func TestBigIntPathsAgreeCollection(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 16 << 10, ValueAttrs: []string{"N"}})
	defer c.Close()
	vals := bigFixtureVals(60)
	for i, v := range vals {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), bigAd(t, i, v)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	served := map[string]int{}
	checkBigPaths(t, "collection", c, vals, served)
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Fatal("the collection did not build its columnar accelerator")
	}
	checkBigPaths(t, "collection+columnar", c, vals, served)
	requireServed(t, served, "Query", "CountQuery", "VectorEvalCount", "ColumnarEvalCount",
		"GroupCountConstraint", "NumStatsQuery.N")
}

// requireServed fails when a path never answered: its agreement would then be vacuous.
func requireServed(t *testing.T, served map[string]int, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if served[p] == 0 {
			t.Errorf("%s never answered a constraint, so its agreement proves nothing", p)
		}
	}
	t.Logf("constraints answered per path: %v", served)
}
