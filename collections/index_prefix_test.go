package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// prefixAPs are the access points a fixture's GlobalJobId values are spread over. The names
// share prefixes on purpose -- "ap40" is a prefix of "ap40-dev" -- so a probe for the shorter
// one must take the longer one's keys too, which a plain equality lookup would miss.
var prefixAPs = []string{"ap40", "ap40-dev", "ap2001", "ap2002", "login04"}

// prefixFixture builds a store whose GlobalJobId is indexed categorically and whose values
// are "<ap>.<n>.0#<n>", with a scattering of SHOUTED spellings: the index folds those into
// the same keys, so a case-sensitive pattern must reject them after the index admits them.
func prefixFixture(t *testing.T, opts Options, n int) *Collection {
	t.Helper()
	opts.CategoricalAttrs = append(opts.CategoricalAttrs, "GlobalJobId")
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		ad := jobAd(i, nil)
		ap := prefixAPs[i%len(prefixAPs)]
		gjid := fmt.Sprintf("%s.%d.0#%d", ap, i, i)
		if i%17 == 0 {
			gjid = fmt.Sprintf("%s.%d.0#%d", upperFirst(ap), i, i)
		}
		switch {
		case i%101 == 3:
			// An EXPRESSION, not a string literal: the index cannot key it, so it lands in
			// the exception set. It still evaluates to a matching value, so a probe that
			// forgets the exceptions loses it.
			e, perr := classad.ParseExpr(fmt.Sprintf("strcat(%q, %q)", ap, fmt.Sprintf(".%d.0#%d", i, i)))
			if perr != nil {
				t.Fatal(perr)
			}
			ad.InsertExpr("GlobalJobId", e)
		case i%103 == 5:
			ad.InsertAttr("GlobalJobId", int64(i)) // a number: also exceptional
		case i%107 == 7:
			ad.Delete("GlobalJobId") // absent: a pattern over it is UNDEFINED, never true
		case i%109 == 9:
			// A newline inside the value, which is what tells a multiline `^` apart from an
			// anchored one: a value of "zzz<newline>ap40..." is matched by the multiline form.
			ad.InsertAttrString("GlobalJobId", "zzz\n"+gjid)
		default:
			ad.InsertAttrString("GlobalJobId", gjid)
		}
		if putErr := c.Put([]byte(fmt.Sprintf("k%05d.0", i)), ad); putErr != nil {
			t.Fatal(putErr)
		}
	}
	c.Reindex()
	// A fixture whose index was never built answers every query by scanning, which passes a
	// correctness check and makes every pruning check vacuous. Refuse to be that fixture.
	q, err := vm.Parse(`GlobalJobId == "ap40.0.0#0"`)
	if err != nil {
		t.Fatal(err)
	}
	if ex := c.ExplainQuery(q); ex.UnindexedSegments > 0 {
		t.Fatalf("%d segments have no GlobalJobId index: %s", ex.UnindexedSegments, ex.Reason)
	}
	return c
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return string(s[0]-32) + s[1:]
}

// prefixQueries exercise the shapes the probe must get right: a prefix that is itself a prefix
// of other keys, one that matches nothing, a case-sensitive pattern over folded keys, a pattern
// with more to it than the literal, and the shapes that must NOT be probed at all.
var prefixQueries = []struct {
	expr   string
	probed bool // the planner extracts a prefix probe
}{
	{`regexp("^ap40", GlobalJobId)`, true},     // also matches every ap40-dev value
	{`regexp("^ap40-dev", GlobalJobId)`, true}, // the narrower run
	{`regexp("^ap2001", GlobalJobId)`, true},
	{`regexp("^qq", GlobalJobId)`, true},        // no key begins with it: the segments skip
	{`regexp("^Ap40", GlobalJobId)`, true},      // case-sensitive: folded run, fewer answers
	{`regexp("^ap40", GlobalJobId, "i")`, true}, // case-insensitive: the whole folded run
	{`regexp("^ap2001.*#7$", GlobalJobId)`, true},
	{`regexp("^ap40", GlobalJobId) && ClusterId < 50`, true},
	{`regexp("ap40", GlobalJobId)`, false},        // unanchored: matches mid-string
	{`regexp("^ap40", GlobalJobId, "m")`, false},  // multiline ^ is not a value prefix
	{`!regexp("^ap40", GlobalJobId)`, false},      // a negated match prunes nothing
	{`regexp("^ap40|login", GlobalJobId)`, false}, // the alternation escapes the prefix
}

func hasPrefixProbe(t *testing.T, expr string) bool {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range q.Probes() {
		if p.Op == "prefix" {
			return true
		}
	}
	return false
}

// TestPrefixProbeAnswersAnchoredRegexp is the correctness half: every query returns exactly what
// a plain evaluator scan returns, on a mutable store and on one whose sealed segments answer from
// their sidecar indexes. The fixture's case-shouted values are the point -- the folded key run
// admits them and the store has to throw them out again.
func TestPrefixProbeAnswersAnchoredRegexp(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{"mutable", Options{Shards: 2}},
		{"sealed", Options{Shards: 2, SegmentSize: 1 << 14}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			opts := tc.opts
			if opts.SegmentSize > 0 {
				if !mmapSupported {
					t.Skip("persistence is unix-only")
				}
				opts.Dir = t.TempDir()
			}
			c := prefixFixture(t, opts, 500)
			for _, pq := range prefixQueries {
				if got := hasPrefixProbe(t, pq.expr); got != pq.probed {
					t.Errorf("%s: prefix probe = %v, want %v", pq.expr, got, pq.probed)
				}
				q, err := vm.Parse(pq.expr)
				if err != nil {
					t.Fatal(err)
				}
				want := scanMatches(t, c, q)
				sigsEqual(t, pq.expr, collectQuery(t, c, pq.expr), want)
			}
		})
	}
}

// prefixVisited runs expr through the indexed read path and reports how many stored records it
// had to look at, plus how many it returned.
func prefixVisited(t *testing.T, c *Collection, expr string) (ScanStats, int) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var st ScanStats
	rows := 0
	for range c.QueryRawWireStats(q, []string{"ClusterId"}, false, &st) {
		rows++
	}
	return st, rows
}

// TestPrefixProbePrunes is the effect half: the index must actually narrow the read. Asserting
// that the plan CONTAINS a prefix probe would pass even if probeOffsets handed back every
// record, so the assertion is on the records visited.
func TestPrefixProbePrunes(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 500
	c := prefixFixture(t, Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14}, n)
	// Most segments here are sealed and answer from a mapped sidecar rather than a heap
	// index, which is the only way the mmap key-blob search gets exercised at all.
	if sidecar, total := dnfSidecarCounts(c); sidecar == 0 {
		t.Fatalf("no sealed segment is served by a sidecar index (%d segments): the mmap prefix search is untested", total)
	}

	// One access point in five, so a correct probe visits about a fifth of the store. The
	// bound allows for the shouted spellings and the uncovered active-segment tail, and
	// rules out unioning every key (or falling back to a scan), which visits all 500.
	st, rows := prefixVisited(t, c, `regexp("^ap2001", GlobalJobId)`)
	if rows == 0 {
		t.Fatal("no rows matched: the fixture does not exercise the probe")
	}
	if st.RecordsVisited > n/4 {
		t.Errorf("visited %d of %d records for one access point in %d; stats %+v",
			st.RecordsVisited, n, len(prefixAPs), st)
	}

	// A prefix that is a prefix of another access point's name must take both runs: ap40 and
	// ap40-dev. Nothing else may come with them -- two runs of five, not the whole store.
	stWide, rowsWide := prefixVisited(t, c, `regexp("^ap40", GlobalJobId)`)
	if rowsWide <= rows {
		t.Errorf("^ap40 returned %d rows, ^ap2001 %d: the ap40-dev run was not taken", rowsWide, rows)
	}
	if stWide.RecordsVisited > 45*n/100 {
		t.Errorf("visited %d of %d records for two access points; stats %+v", stWide.RecordsVisited, n, stWide)
	}

	// A prefix no key carries skips the indexed segments outright rather than walking them.
	stNone, rowsNone := prefixVisited(t, c, `regexp("^qq", GlobalJobId)`)
	if rowsNone != 0 {
		t.Errorf("^qq matched %d rows", rowsNone)
	}
	if stNone.SegmentsIndexSkipped == 0 {
		t.Errorf("no segment was skipped for a prefix no key carries; stats %+v", stNone)
	}
	if stNone.RecordsVisited > n/10 {
		t.Errorf("visited %d records for a prefix that matches nothing; stats %+v", stNone.RecordsVisited, stNone)
	}

	// The planner's estimate feeds the pushdown gate and the join cost model, so it has to
	// be close to what the read actually visits, not merely finite.
	q, err := vm.Parse(`regexp("^ap2001", GlobalJobId)`)
	if err != nil {
		t.Fatal(err)
	}
	ex := c.ExplainQuery(q)
	if ex.EstVisited < int64(st.RecordsVisited)/2 || ex.EstVisited > int64(st.RecordsVisited)*2 {
		t.Errorf("estimated %d records visited, actually %d", ex.EstVisited, st.RecordsVisited)
	}
}

// TestPrefixSpanBoundaries pins the key-run arithmetic directly, including the boundary cases a
// store fixture is unlikely to produce: an empty run, a run at either end of the key order, and
// a prefix whose last byte is the maximum one (whose successor has to drop a byte).
func TestPrefixSpanBoundaries(t *testing.T) {
	t.Parallel()
	keys := []string{"aa", "ab", "ab-dev", "abz", "ac", "b", "ba", "zz"}
	for _, tc := range []struct {
		p    string
		want []string
	}{
		{"ab", []string{"ab", "ab-dev", "abz"}},
		{"a", []string{"aa", "ab", "ab-dev", "abz", "ac"}},
		{"aa", []string{"aa"}},
		{"b", []string{"b", "ba"}},
		{"zz", []string{"zz"}},
		{"zzz", nil},
		{"", []string{"aa", "ab", "ab-dev", "abz", "ac", "b", "ba", "zz"}},
		{"q", nil},
		{"\xff", nil}, // no successor exists: the span runs to the end and finds nothing
	} {
		from, to := strPrefixSpan(keys, tc.p)
		got := keys[from:to]
		if len(got) != len(tc.want) {
			t.Errorf("strPrefixSpan(%q) = %v, want %v", tc.p, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("strPrefixSpan(%q) = %v, want %v", tc.p, got, tc.want)
				break
			}
		}
	}
}

// TestPrefixUpperKey pins the successor string the span's upper bound is built from.
func TestPrefixUpperKey(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"ab", "ac", true},
		{"a", "b", true},
		{"a\xff", "b", true}, // the trailing max byte is dropped, not wrapped
		{"\xff\xff", "", false},
		{"", "", false},
	} {
		got, ok := prefixUpperKey(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("prefixUpperKey(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

// TestPrefixCoveringEveryKey exercises the degenerate run -- a prefix every key carries, so
// the probe prunes nothing. It must still answer exactly, and must still spare the records
// that carry no value at all: the short-circuit stands in for the union of every key's
// postings, which is the records that POSTED one, not every record in the segment.
func TestPrefixCoveringEveryKey(t *testing.T) {
	t.Parallel()
	const n = 300
	c, err := Open(Options{Shards: 1, CategoricalAttrs: []string{"GlobalJobId"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	posted, absent := 0, 0
	for i := 0; i < n; i++ {
		ad := jobAd(i, nil)
		switch {
		case i%20 == 0:
			ad.Delete("GlobalJobId") // no value: outside every key's postings AND outside exc
			absent++
		case i%21 == 0:
			e, perr := classad.ParseExpr(fmt.Sprintf("strcat(%q, %q)", "ap40", fmt.Sprintf(".%d", i)))
			if perr != nil {
				t.Fatal(perr)
			}
			ad.InsertExpr("GlobalJobId", e) // exceptional, and it does match
			posted++
		default:
			ad.InsertAttrString("GlobalJobId", fmt.Sprintf("ap%02d.%d.0#%d", i%7, i, i))
			posted++
		}
		if putErr := c.Put([]byte(fmt.Sprintf("k%05d.0", i)), ad); putErr != nil {
			t.Fatal(putErr)
		}
	}
	c.Reindex()
	for _, expr := range []string{`regexp("^ap", GlobalJobId)`, `regexp("^a", GlobalJobId)`} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		sigsEqual(t, expr, collectQuery(t, c, expr), scanMatches(t, c, q))
		st, _ := prefixVisited(t, c, expr)
		if st.RecordsVisited > posted {
			t.Errorf("%s: visited %d records, but only %d carry a value -- the %d that do not should not be candidates; stats %+v",
				expr, st.RecordsVisited, posted, absent, st)
		}
		if st.RecordsVisited < posted {
			t.Errorf("%s: visited %d of the %d records that carry a value: the run did not cover every key", expr, st.RecordsVisited, posted)
		}
	}
}

// TestPrefixProbeOnANestedPath: a prefix probe names its attribute through the same target
// resolution every other probe uses, so a categorical index configured on a nested path
// answers one. The assertion is on records visited, not on the probe's existence: a probe
// that resolves the path and then prunes nothing would pass the latter.
func TestPrefixProbeOnANestedPath(t *testing.T) {
	t.Parallel()
	const n = 400
	c, err := Open(Options{Shards: 1, CategoricalAttrs: []string{"Res.Host"}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := 0; i < n; i++ {
		ad := jobAd(i, nil)
		rec, perr := classad.ParseExpr(fmt.Sprintf("[ Host = %q; N = %d ]", fmt.Sprintf("%s.%d", prefixAPs[i%len(prefixAPs)], i), i))
		if perr != nil {
			t.Fatal(perr)
		}
		ad.InsertExpr("Res", rec)
		if putErr := c.Put([]byte(fmt.Sprintf("k%05d.0", i)), ad); putErr != nil {
			t.Fatal(putErr)
		}
	}
	c.Reindex()

	const expr = `regexp("^ap2001", Res.Host)`
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var p vm.Probe
	for _, pr := range q.Probes() {
		if pr.Op == "prefix" {
			p = pr
		}
	}
	if len(p.Path) != 2 {
		t.Fatalf("probe %+v did not resolve Res.Host as a path", p)
	}
	sigsEqual(t, expr, collectQuery(t, c, expr), scanMatches(t, c, q))
	st, rows := prefixVisited(t, c, expr)
	if rows == 0 {
		t.Fatal("no rows matched: the fixture does not exercise the probe")
	}
	if st.RecordsVisited > n/3 {
		t.Errorf("visited %d of %d records for one of %d hosts; stats %+v",
			st.RecordsVisited, n, len(prefixAPs), st)
	}
}
