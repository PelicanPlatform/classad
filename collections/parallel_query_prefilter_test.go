package collections

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// parQueryFillers is how many padding attributes each probe ad carries, so reassembling a record
// is visibly more work than deciding it from a column -- which is the whole point of the
// prefilter the fan-out had to keep.
const parQueryFillers = 10

// parQueryStore builds the store the full-decode probes run against: enough ads to fill several
// segments per shard, then MAINTAINED (dictionary, columnar schema, columnarize, reindex) because
// the columnar prefilter does not exist on a freshly written store and a probe over one would
// prove nothing about the path it is here to cover.
//
// It returns the literal values each ad was written with, keyed by ClusterId.ProcId. That map is
// the independent oracle: the parallel and serial paths share their record machinery, so
// comparing them only to each other would be satisfied by both being wrong the same way.
func parQueryStore(t *testing.T, encrypted bool) (*Collection, map[string]map[string]string) {
	t.Helper()
	opts := Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16, QueryParallelism: 4}
	if encrypted {
		key := make([]byte, 32)
		for i := range key {
			key[i] = byte(i + 11)
		}
		opts.DataKey = key
		opts.EncryptedAttrs = []string{"Note"}
	}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })

	const n = 2000
	want := make(map[string]map[string]string, n)
	for i := 0; i < n; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ MyType="Job"; ClusterId=%d; ProcId=%d; JobStatus=%d; Owner="user%d"; RequestMemory=%d; Note="SECRET-%d"`,
			i, i%10, 1+i%5, i%7, (1+i%8)*1024, i)
		exp := map[string]string{
			"MyType":        `"Job"`,
			"ClusterId":     fmt.Sprint(i),
			"ProcId":        fmt.Sprint(i % 10),
			"JobStatus":     fmt.Sprint(1 + i%5),
			"Owner":         fmt.Sprintf("%q", fmt.Sprintf("user%d", i%7)),
			"RequestMemory": fmt.Sprint((1 + i%8) * 1024),
			"Note":          fmt.Sprintf("%q", fmt.Sprintf("SECRET-%d", i)),
		}
		for f := 0; f < parQueryFillers; f++ {
			fmt.Fprintf(&b, `; Filler%02d="filler value %d-%d"`, f, i, f)
			exp[fmt.Sprintf("Filler%02d", f)] = fmt.Sprintf("%q", fmt.Sprintf("filler value %d-%d", i, f))
		}
		b.WriteString(" ]")
		k := fmt.Sprintf("%d.%d", i, i%10)
		if err := c.Put([]byte(k), mustAd(t, b.String())); err != nil {
			t.Fatal(err)
		}
		want[k] = exp
	}
	if _, err := c.RetrainDict(4000); err != nil {
		t.Fatal(err)
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no columnar schema derived: the prefilter path is not covered")
	}
	c.ColumnarizeSealed()
	c.Reindex()
	if columnarSegmentsForTest(c) == 0 {
		t.Fatal("no segment columnarized: the prefilter path is not covered")
	}
	return c, want
}

// parQueryAdAttrs renders one result ad as attribute -> unparsed value, so a row can be compared
// against the literal text it was written with.
func parQueryAdAttrs(t *testing.T, a *classad.ClassAd) map[string]string {
	t.Helper()
	out := make(map[string]string, a.Size())
	for _, name := range a.GetAttributes() {
		e, ok := a.Lookup(name)
		if !ok {
			t.Fatalf("ad lists attribute %q but does not resolve it", name)
		}
		out[name] = e.String()
	}
	return out
}

// parQueryPath selects which implementation a probe runs.
type parQueryPath int

const (
	// parPathFanout forces the cross-segment fan-out (the path under change).
	parPathFanout parQueryPath = iota
	// parPathFallback forces runParallelQuery's own serial fallback: the same gathered tasks
	// walked on the calling goroutine. Covered separately because it is a distinct loop that
	// has been the one path without the prefilter before.
	parPathFallback
	// parPathSerial is the canonical serial full scan (scanShard -> scanWindows -> refVisitor),
	// the reference the other two must reproduce exactly.
	parPathSerial
)

// runParQuery runs one full-decode query down a CHOSEN path and returns the ads keyed by
// ClusterId.ProcId, the row count, and the scan counters.
//
// Picking the path rather than hoping for it is the point: the fan-out declines SILENTLY (too
// little work, no budget) and runs the same tasks serially, so a probe that quietly ran the
// serial scan twice would prove nothing. queryFanouts is what makes the choice checkable.
func runParQuery(t *testing.T, c *Collection, qtext string, redact bool, limit int, path parQueryPath) (map[string]map[string]string, int, ScanStats) {
	t.Helper()
	if path == parPathFanout {
		c.parallelMinBytes = 0
	} else {
		c.parallelMinBytes = 1 << 62
	}
	var stats ScanStats
	qp := c.newQueryPlan(mustParseQ(t, qtext), nil, &stats)
	rows := make(map[string]map[string]string)
	count := 0
	before := queryFanouts.Load()
	yield := func(a *classad.ClassAd) bool {
		attrs := parQueryAdAttrs(t, a)
		count++
		rows[attrs["ClusterId"]+"."+attrs["ProcId"]] = attrs
		return limit <= 0 || count < limit
	}
	if path == parPathSerial {
		// readPlan{} is the full walk: an index path would visit candidates instead of every
		// record and so would not produce the counters the other two must match.
		c.runQuery(qp, readPlan{}, c.yieldAdAs(yield, redact))
	} else {
		c.runParallelQuery(qp, yield, redact)
	}
	switch fanned := queryFanouts.Load() != before; {
	case path == parPathFanout && !fanned && runtime.GOMAXPROCS(0) < 2:
		t.Skip("this machine resolves to serial query parallelism; nothing to fan out")
	case path == parPathFanout && !fanned:
		// Not a skip. The budget is free (nothing else is querying this store) and the gate is
		// wide open, so a read that did not fan out means the dispatch is gone -- and skipping
		// would hide exactly that, since the fallback returns the same ads.
		t.Fatal("the parallel arm did not fan out")
	case path != parPathFanout && fanned:
		t.Fatal("a serial arm fanned out; the comparison would be the parallel path against itself")
	}
	return rows, count, stats
}

// TestQueryParallelMatchesSerial pins the full-decode query's three implementations -- the
// fan-out, its serial fallback and the plain serial scan -- to each other AND to the values the
// ads were written with.
//
// The fan-out used to walk the records itself, which skipped the columnar prefilter: every
// rejected record was rebuilt from the arena before being thrown away. Routing it through
// refVisitorState means each worker now owns a prefilter, a projection scope, a decompression
// buffer, a matcher and a counter set, and every one of those is a chance for a worker to read
// another's state. The symptom is not a crash but ads that are subtly wrong -- an attribute from
// the wrong record, a row dropped, a count doubled -- so the assertions are on the attribute
// VALUES and on the scan counters, not on "did it return rows".
func TestQueryParallelMatchesSerial(t *testing.T) {
	c, want := parQueryStore(t, false)
	cases := []struct {
		name  string
		q     string
		match func(i int) bool
	}{
		// Reject-heavy: the shape the prefilter exists for -- nearly every record is decided
		// from the columns and never reassembled.
		{"reject-heavy", "ClusterId < 50", func(i int) bool { return i < 50 }},
		{"match-heavy", "ProcId < 8", func(i int) bool { return i%10 < 8 }},
		// Two conjuncts over two columnar attributes: the shape that decided 100% from the
		// columns on the production mirror.
		{"two-conjuncts", "JobStatus == 3 && ProcId > 6", func(i int) bool { return 1+i%5 == 3 && i%10 > 6 }},
		// A string equality, so the comparison is not only over integers.
		{"string-eq", `Owner == "user3"`, func(i int) bool { return i%7 == 3 }},
		// Matches nothing a column can decide against: the prefilter cannot help, and the
		// fan-out must not get slower or wronger for it.
		{"no-prefilter-help", `strcat(Owner, "") == "user3"`, func(i int) bool { return i%7 == 3 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			par, parN, parStats := runParQuery(t, c, tc.q, false, 0, parPathFanout)
			fb, fbN, fbStats := runParQuery(t, c, tc.q, false, 0, parPathFallback)
			ser, serN, serStats := runParQuery(t, c, tc.q, false, 0, parPathSerial)

			// The oracle: exactly the keys whose written values satisfy the constraint.
			expect := map[string]map[string]string{}
			for k, exp := range want {
				i := 0
				if _, err := fmt.Sscanf(k, "%d.", &i); err != nil {
					t.Fatal(err)
				}
				if tc.match(i) {
					expect[k] = exp
				}
			}
			if len(expect) == 0 {
				t.Fatal("the constraint matches nothing: the probe proves nothing")
			}
			if parN != len(expect) || fbN != len(expect) || serN != len(expect) {
				t.Fatalf("row counts: fan-out %d, fallback %d, serial %d, want %d", parN, fbN, serN, len(expect))
			}
			for k, exp := range expect {
				for _, got := range []struct {
					path string
					row  map[string]string
				}{{"fan-out", par[k]}, {"fallback", fb[k]}, {"serial", ser[k]}} {
					if got.row == nil {
						t.Fatalf("%s read dropped key %s", got.path, k)
					}
					if len(got.row) != len(exp) {
						t.Fatalf("%s read ad %s has %d attributes %v, want %d", got.path, k, len(got.row), sortedKeys(got.row), len(exp))
					}
					for a, v := range exp {
						if got.row[a] != v {
							t.Fatalf("%s read ad %s: %s = %q, want %q", got.path, k, a, got.row[a], v)
						}
					}
				}
			}

			// The counters must agree too, which is what catches a worker accumulating into a
			// shared sink, a worker whose counters are never merged back, and the segment counts
			// being recorded twice (once while gathering the tasks, once by the walk).
			if parStats != serStats {
				t.Fatalf("scan stats differ:\n fan-out %+v\n  serial %+v", parStats, serStats)
			}
			if fbStats != serStats {
				t.Fatalf("scan stats differ:\n fallback %+v\n   serial %+v", fbStats, serStats)
			}
			// And the prefilter must have SURVIVED the fan-out: without a per-worker colScope the
			// obvious way to make this path compile is to drop the prefilter, which costs a full
			// reassembly per rejected record and would leave this at zero.
			if tc.name != "no-prefilter-help" && parStats.RecordsColumnDecided == 0 {
				t.Fatalf("no record was decided from the columns; the parallel path lost the prefilter (%+v)", parStats)
			}
		})
	}

	// Reject-heavy must actually be reject-heavy: the counters, not the wall clock, are what
	// says the prefilter is carrying the scan rather than merely being present.
	t.Run("reject-heavy-is-column-decided", func(t *testing.T) {
		_, n, st := runParQuery(t, c, "ClusterId < 50", false, 0, parPathFanout)
		if st.RecordsColumnDecided != st.RecordsVisited {
			t.Fatalf("only %d of %d visited records were decided from the columns (%+v)", st.RecordsColumnDecided, st.RecordsVisited, st)
		}
		if st.RecordsReassembled != n {
			t.Fatalf("reassembled %d records for %d matches; a rejected record is being rebuilt (%+v)", st.RecordsReassembled, n, st)
		}
	})

	// A consumer that stops early. Ads arrive in arbitrary order under fan-out, so the assertion
	// is on the SET they are drawn from, not on which ones they are.
	t.Run("limit", func(t *testing.T) {
		const limit = 37
		par, parN, _ := runParQuery(t, c, "ProcId < 8", false, limit, parPathFanout)
		if parN != limit || len(par) != limit {
			t.Fatalf("limited read returned %d ads (%d distinct), want %d", parN, len(par), limit)
		}
		for k, row := range par {
			exp, ok := want[k]
			if !ok {
				t.Fatalf("limited read invented key %s", k)
			}
			for a, v := range exp {
				if row[a] != v {
					t.Fatalf("limited read ad %s: %s = %q, want %q", k, a, row[a], v)
				}
			}
		}
	})
}

// TestQueryParallelProjection covers refVisitorState's columnar projection on the fan-out: with
// a projection plan a matched record can be answered straight from the columns, never
// reassembled. That is a SECOND piece of per-worker state -- the colScope, its scratch, and the
// projection plan's own per-segment id cache -- and sharing any of them narrows an ad with the
// ids another segment's dictionary resolved, which keeps the WRONG attributes rather than
// failing.
//
// The plan is built here rather than reached through a public method because Query passes no
// projection today; the worker code path is the same either way, and leaving it uncovered means
// the per-worker ownership of the projection state is only ever exercised by the wire read.
func TestQueryParallelProjection(t *testing.T) {
	c, want := parQueryStore(t, false)
	proj := []string{"ClusterId", "ProcId", "Owner", "RequestMemory"}
	const q = "JobStatus == 3"

	expect := map[string]map[string]string{}
	for k, exp := range want {
		i := 0
		if _, err := fmt.Sscanf(k, "%d.", &i); err != nil {
			t.Fatal(err)
		}
		if 1+i%5 == 3 {
			expect[k] = exp
		}
	}
	if len(expect) == 0 {
		t.Fatal("the constraint matches nothing")
	}

	run := func(path parQueryPath) (map[string]map[string]string, ScanStats) {
		t.Helper()
		if path == parPathFanout {
			c.parallelMinBytes = 0
		} else {
			c.parallelMinBytes = 1 << 62
		}
		var stats ScanStats
		qp := c.newQueryPlan(mustParseQ(t, q), c.newProjPlan(proj), &stats)
		rows := map[string]map[string]string{}
		before := queryFanouts.Load()
		c.runParallelQuery(qp, func(a *classad.ClassAd) bool {
			attrs := parQueryAdAttrs(t, a)
			rows[attrs["ClusterId"]+"."+attrs["ProcId"]] = attrs
			return true
		}, false)
		switch fanned := queryFanouts.Load() != before; {
		case path == parPathFanout && !fanned && runtime.GOMAXPROCS(0) < 2:
			t.Skip("this machine resolves to serial query parallelism; nothing to fan out")
		case path == parPathFanout && !fanned:
			t.Fatal("the projected parallel arm did not fan out")
		case path != parPathFanout && fanned:
			t.Fatal("the projected serial arm fanned out")
		}
		return rows, stats
	}
	par, parStats := run(parPathFanout)
	ser, serStats := run(parPathFallback)
	if len(par) != len(expect) || len(ser) != len(expect) {
		t.Fatalf("projected row counts: fan-out %d, serial %d, want %d", len(par), len(ser), len(expect))
	}
	for k, exp := range expect {
		for _, got := range []struct {
			path string
			row  map[string]string
		}{{"fan-out", par[k]}, {"serial", ser[k]}} {
			if got.row == nil {
				t.Fatalf("%s projected read dropped key %s", got.path, k)
			}
			if len(got.row) != len(proj) {
				t.Fatalf("%s projected ad %s has %v, want exactly %v", got.path, k, sortedKeys(got.row), proj)
			}
			for _, a := range proj {
				if got.row[a] != exp[a] {
					t.Fatalf("%s projected ad %s: %s = %q, want %q", got.path, k, a, got.row[a], exp[a])
				}
			}
		}
	}
	if parStats != serStats {
		t.Fatalf("projected scan stats differ:\n fan-out %+v\n  serial %+v", parStats, serStats)
	}
	// Projected straight from the columns: a matched record counts as matched without ever being
	// reassembled. If the workers lost the projection scope this falls back to reassembly and the
	// count climbs to the match count.
	if parStats.RecordsReassembled != 0 {
		t.Fatalf("projected fan-out reassembled %d records; the columnar projection is not engaged (%+v)", parStats.RecordsReassembled, parStats)
	}
}

// TestQueryParallelEncrypted covers the entitlement dispatch on the fan-out: redact is threaded
// into the worker's own decode, because this path decodes ads ITSELF rather than through the
// caller's emit. Getting it wrong does not fail loudly -- an unentitled read quietly serves the
// plaintext, or an entitled one fails to decode and drops the ad from the result entirely.
func TestQueryParallelEncrypted(t *testing.T) {
	c, want := parQueryStore(t, true)
	const q = "ProcId < 8"
	nWant := 0
	for k := range want {
		var i int
		if _, err := fmt.Sscanf(k, "%d.", &i); err != nil {
			t.Fatal(err)
		}
		if i%10 < 8 {
			nWant++
		}
	}

	// Entitled: the literal value the ad was written with, on both paths.
	par, parN, _ := runParQuery(t, c, q, false, 0, parPathFanout)
	ser, serN, _ := runParQuery(t, c, q, false, 0, parPathSerial)
	if parN != nWant || serN != nWant {
		t.Fatalf("entitled row counts: fan-out %d, serial %d, want %d", parN, serN, nWant)
	}
	for k, row := range par {
		if row["Note"] != want[k]["Note"] {
			t.Fatalf("entitled fan-out ad %s: Note = %q, want %q", k, row["Note"], want[k]["Note"])
		}
		if ser[k]["Note"] != want[k]["Note"] {
			t.Fatalf("entitled serial ad %s: Note = %q, want %q", k, ser[k]["Note"], want[k]["Note"])
		}
	}

	// Redacted: every ad is still served (a secret the caller may not read becomes undefined, it
	// does not make the ad disappear) and no plaintext reaches the consumer.
	c.parallelMinBytes = 0
	before := queryFanouts.Load()
	rows, leaked := 0, 0
	for a := range c.QueryRedacted(mustParseQ(t, q)) {
		rows++
		if strings.Contains(a.StringWithPrivate(), "SECRET-") {
			leaked++
		}
	}
	if queryFanouts.Load() == before && runtime.GOMAXPROCS(0) >= 2 {
		t.Fatal("the redacted read did not fan out; it would be probing the serial path")
	}
	if rows != nWant {
		t.Fatalf("redacted parallel read returned %d ads, want %d", rows, nWant)
	}
	if leaked != 0 {
		t.Fatalf("redacted parallel read leaked %d plaintext secrets", leaked)
	}
	// Not leaking is not the same as never holding: assert the key is never USED, so the
	// protection is a withheld key rather than a filter on the way out.
	ctr := &countingSealer{inner: c.sealer}
	c.sealer = ctr
	for range c.QueryRedacted(mustParseQ(t, q)) {
	}
	if n := ctr.opens.Load(); n != 0 {
		t.Fatalf("redacted parallel read opened %d sealed values; it should hold no key", n)
	}
	ctr.opens.Store(0)
	for range c.Query(mustParseQ(t, q)) {
	}
	if ctr.opens.Load() == 0 {
		t.Fatal("entitled parallel read opened nothing; the probe is not reaching the key")
	}

	// Redacted and entitled must agree on everything but the sealed attribute, and the redacted
	// fan-out must agree with the redacted serial scan.
	red, _, _ := runParQuery(t, c, q, true, 0, parPathFanout)
	redSer, _, _ := runParQuery(t, c, q, true, 0, parPathSerial)
	if len(red) != len(redSer) {
		t.Fatalf("redacted row counts: fan-out %d, serial %d", len(red), len(redSer))
	}
	for k, row := range red {
		for _, a := range []string{"ClusterId", "ProcId", "Owner", "RequestMemory"} {
			if row[a] != want[k][a] {
				t.Fatalf("redacted fan-out ad %s: %s = %q, want %q", k, a, row[a], want[k][a])
			}
		}
		if row["Note"] == want[k]["Note"] {
			t.Fatalf("redacted fan-out ad %s still carries the plaintext Note", k)
		}
		if row["Note"] != redSer[k]["Note"] {
			t.Fatalf("redacted ad %s: fan-out Note %q, serial %q", k, row["Note"], redSer[k]["Note"])
		}
	}
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
