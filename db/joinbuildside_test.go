package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// buildSideFixture: a small table of keys against a large one, in both mutable and archive
// form, so the same join can be asked of a table that promises an order and one that does
// not. archiveUnordered says whether the archive pair gives up newest-first.
func buildSideFixture(tb testing.TB, small, big int, archiveUnordered bool) *Catalog {
	tb.Helper()
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: tb.TempDir()})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	ms, err := cat.CreateTable("small")
	if err != nil {
		tb.Fatal(err)
	}
	mb, err := cat.CreateTable("big")
	if err != nil {
		tb.Fatal(err)
	}
	as, err := cat.CreateArchiveTable("asmall", ArchiveConfig{SegmentSize: 1 << 14, Unordered: archiveUnordered})
	if err != nil {
		tb.Fatal(err)
	}
	ab, err := cat.CreateArchiveTable("abig", ArchiveConfig{SegmentSize: 1 << 14, Unordered: archiveUnordered})
	if err != nil {
		tb.Fatal(err)
	}
	stride := big / small
	for i := 0; i < big; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nExitCode = %d", i, i%7))
		if perr != nil {
			tb.Fatal(perr)
		}
		if perr = mb.Put(fmt.Sprintf("%d", i), ad); perr != nil {
			tb.Fatal(perr)
		}
		if perr = ab.Append(ad); perr != nil {
			tb.Fatal(perr)
		}
		if i%stride == 0 {
			k, kerr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nOwner = \"user%d\"", i, i%4))
			if kerr != nil {
				tb.Fatal(kerr)
			}
			if perr = ms.Put(fmt.Sprintf("%d", i), k); perr != nil {
				tb.Fatal(perr)
			}
			if perr = as.Append(k); perr != nil {
				tb.Fatal(perr)
			}
		}
	}
	return cat
}

func bsSpec(left, right string) JoinSpec {
	return JoinSpec{
		Kind:  JoinInner,
		Left:  JoinSide{Table: left, Alias: "l"},
		Right: JoinSide{Table: right, Alias: "r"},
		Equi:  []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
	}
}

func drainJoin(tb testing.TB, cat *Catalog, spec JoinSpec) ([]string, JoinStats) {
	tb.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		tb.Fatal(err)
	}
	var out []string
	for ad := range seq {
		out = append(out, ad.String())
	}
	return out, st
}

// When the sides are exchanged the join must answer with the same ROWS. Their order is what
// the swap is allowed to change and the only thing it may change, so the sets are compared
// sorted and the count is compared as it stands.
func TestBuildSideSwapKeepsTheRows(t *testing.T) {
	cat := buildSideFixture(t, 50, 5000, false)
	spec := bsSpec("small", "big") // mutable both sides: swappable

	swapped, sst := drainJoin(t, cat, spec)
	forceNoBuildSideSwap = true
	asWritten, ast := drainJoin(t, cat, spec)
	forceNoBuildSideSwap = false

	if !sst.BuildSideSwapped {
		t.Fatalf("the sides were not exchanged, so nothing was compared: %s", sst.BuildSideWhy)
	}
	if ast.BuildSideSwapped {
		t.Fatal("the disabled arm exchanged the sides")
	}
	if len(swapped) == 0 {
		t.Fatal("the fixture produced no rows")
	}
	if len(swapped) != len(asWritten) {
		t.Fatalf("swapped returned %d rows, as written %d", len(swapped), len(asWritten))
	}
	sort.Strings(swapped)
	sort.Strings(asWritten)
	for i := range swapped {
		if swapped[i] != asWritten[i] {
			t.Fatalf("row %d differs:\n swapped: %s\n written: %s", i, swapped[i], asWritten[i])
		}
	}
	// The point of the exchange, with no index to separate the reads: hold the small side.
	if sst.HashRows >= ast.HashRows {
		t.Errorf("swapped holds %d rows, as written %d: the exchange held no less",
			sst.HashRows, ast.HashRows)
	}
}

// What may and may not be exchanged. Each case states the rule it stands for, because the
// rules are about what a caller is entitled to rely on and not about speed.
func TestBuildSideSwapEligibility(t *testing.T) {
	for _, tc := range []struct {
		name      string
		unordered bool
		spec      func() JoinSpec
		want      bool
		why       string
	}{{
		name: "mutable tables promise no order",
		spec: func() JoinSpec { return bsSpec("small", "big") },
		want: true,
	}, {
		name: "an archive reads newest-first and that is its contract",
		spec: func() JoinSpec { return bsSpec("asmall", "abig") },
		want: false,
		why:  "fixed order",
	}, {
		name:      "unless it gives that up",
		unordered: true,
		spec:      func() JoinSpec { return bsSpec("asmall", "abig") },
		want:      true,
	}, {
		name: "a LEFT join preserves the side written first",
		spec: func() JoinSpec { s := bsSpec("small", "big"); s.Kind = JoinLeft; return s },
		want: false,
		why:  "LEFT join",
	}, {
		name: "a chain is read as written",
		spec: func() JoinSpec {
			return JoinSpec{
				Left: JoinSide{Table: "small", Alias: "l"},
				Steps: []JoinStep{{
					Kind: JoinInner, Side: JoinSide{Table: "big", Alias: "r"},
					Equi: []JoinEqui{{Left: "ClusterId", Right: "ClusterId"}},
				}},
			}
		},
		want: false,
		why:  "chain",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			cat := buildSideFixture(t, 50, 2000, tc.unordered)
			_, st := drainJoin(t, cat, tc.spec())
			if st.BuildSideSwapped != tc.want {
				t.Fatalf("swapped = %v, want %v (%s)", st.BuildSideSwapped, tc.want, st.BuildSideWhy)
			}
			if tc.why != "" && !strings.Contains(st.BuildSideWhy, tc.why) {
				t.Errorf("reason %q does not mention %q", st.BuildSideWhy, tc.why)
			}
		})
	}
}

// A grouped aggregate's answer is one row per group, so the order the join's rows arrive in
// is not in it. That holds even over an archive that does promise newest-first, which is the
// whole point: the promise is about what a reader SEES, and this reader sees groups.
func TestBuildSideSwapsUnderAnAggregateOverAnOrderedArchive(t *testing.T) {
	cat := buildSideFixture(t, 50, 2000, false) // archives keep newest-first

	var st JoinStats
	rows, err := cat.JoinAggregate(JoinAggSpec{
		Join:      bsSpec("asmall", "abig"),
		GroupCols: []GroupCol{{Attr: "l.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
	}, JoinLimits{}, &st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !st.BuildSideSwapped {
		t.Fatalf("an aggregate over an ordered archive did not exchange the sides: %s", st.BuildSideWhy)
	}
	if len(rows) == 0 {
		t.Fatal("the aggregate returned no groups, so nothing was joined")
	}
	// The same aggregate with the choice disabled must give the same groups.
	forceNoBuildSideSwap = true
	plain, err := cat.JoinAggregate(JoinAggSpec{
		Join:      bsSpec("asmall", "abig"),
		GroupCols: []GroupCol{{Attr: "l.Owner"}},
		Aggs:      []AggSpec{{Func: AggCount, Arg: "*"}},
	}, JoinLimits{}, &JoinStats{}, nil)
	forceNoBuildSideSwap = false
	if err != nil {
		t.Fatal(err)
	}
	got, want := aggRowKeys(rows), aggRowKeys(plain)
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("swapped groups %v, as written %v", got, want)
	}
}

func aggRowKeys(rows []AggRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, strings.Join(r.Group, ",")+"="+strings.Join(r.Values, ","))
	}
	sort.Strings(out)
	return out
}

// The eligibility rules check a FLAG. This checks the thing the flag is there to protect:
// the rows of a join whose FROM side is an ordered archive come out in that archive's
// newest-first order, which is condor_history's order.
//
// The two sides are appended in OPPOSITE key order on purpose. With both ascending, each
// side's newest-first is the same descending sequence of joined rows and the test cannot
// tell a preserved order from an exchanged one -- which is exactly what an earlier version
// of it did, passing happily against a planner that exchanged every archive.
func TestBuildSideKeepsAnArchiveNewestFirst(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	keys, err := cat.CreateArchiveTable("keys", ArchiveConfig{SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	body, err := cat.CreateArchiveTable("body", ArchiveConfig{SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	// keys is far smaller than body, so the memory rule WANTS to hash keys and stream
	// body. Only the order promise stops it, which is what makes this a test of the
	// promise rather than of the arithmetic.
	const n, stride = 4000, 100
	for i := 0; i < n; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nExitCode = %d", i, i%7))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr = body.Append(ad); perr != nil { // ascending: newest-first is DESCENDING
			t.Fatal(perr)
		}
	}
	for i := n - 1; i >= 0; i -= stride { // descending: newest-first is ASCENDING
		k, kerr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nOwner = \"u\"", i))
		if kerr != nil {
			t.Fatal(kerr)
		}
		if perr := keys.Append(k); perr != nil {
			t.Fatal(perr)
		}
	}

	rows, st := drainJoin(t, cat, bsSpec("keys", "body"))
	if st.BuildSideSwapped {
		t.Fatalf("an ordered archive was exchanged: %s", st.BuildSideWhy)
	}
	if len(rows) < 2 {
		t.Fatalf("got %d rows, too few to have an order", len(rows))
	}
	// keys is the FROM side and its newest-first is ascending ClusterId. Exchanging the
	// sides would stream body instead and hand back the reverse.
	prev := -1
	for i, r := range rows {
		id := clusterIDOf(t, r)
		if i > 0 && id <= prev {
			t.Fatalf("row %d has ClusterId %d after %d: the FROM archive's newest-first "+
				"order was lost", i, id, prev)
		}
		prev = id
	}
}

func clusterIDOf(tb testing.TB, row string) int {
	tb.Helper()
	i := strings.Index(row, "ClusterId = ")
	if i < 0 {
		tb.Fatalf("no ClusterId in %q", row)
	}
	rest := row[i+len("ClusterId = "):]
	n := 0
	for j := 0; j < len(rest) && rest[j] >= '0' && rest[j] <= '9'; j++ {
		n = n*10 + int(rest[j]-'0')
	}
	return n
}

// The two pulls, in opposition. With an index on the big side's join key, streaming the
// SMALL side lets the probe narrow the big one, so the orientation that holds MORE rows
// reads far fewer records -- and records win. A planner that only knew "hash the smaller
// side" would exchange these and give up the probe.
func TestBuildSideRecordsBeatMemoryWhenTheKeyIsIndexed(t *testing.T) {
	cat, err := OpenCatalogConfig(CatalogConfig{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	small, err := cat.CreateArchiveTable("small", ArchiveConfig{SegmentSize: 1 << 14, Unordered: true})
	if err != nil {
		t.Fatal(err)
	}
	// The big side is indexed on the join key: this is what makes the probe possible.
	big, err := cat.CreateArchiveTable("big", ArchiveConfig{
		SegmentSize: 1 << 14, Unordered: true, ValueAttrs: []string{"ClusterId"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20000; i++ {
		ad, perr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nExitCode = %d\nPad = %q",
			i, i%7, strings.Repeat("x", 48)))
		if perr != nil {
			t.Fatal(perr)
		}
		if perr = big.Append(ad); perr != nil {
			t.Fatal(perr)
		}
		if i%1000 == 0 {
			k, kerr := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nOwner = \"u\"", i))
			if kerr != nil {
				t.Fatal(kerr)
			}
			if perr = small.Append(k); perr != nil {
				t.Fatal(perr)
			}
		}
	}
	big.Reindex()

	// Small written first: streaming it is already right, because the probe will narrow
	// the big side. The memory rule alone would exchange them.
	_, st := drainJoin(t, cat, bsSpec("small", "big"))
	if st.BuildSideSwapped {
		t.Fatalf("exchanged the sides and gave up the probe: %s", st.BuildSideWhy)
	}
	if st.BuildSideCost >= st.BuildSideSwapCost {
		t.Errorf("as written priced at %d, swapped at %d: the probe did not make the "+
			"difference the decision rests on", st.BuildSideCost, st.BuildSideSwapCost)
	}
	if st.ProbeKeys == 0 {
		t.Errorf("the probe did not fire, so this measured something else: %s", st.ProbeWhy)
	}
}
