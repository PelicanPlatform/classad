package collections

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
)

// parWireFillers is how many padding attributes each probe ad carries, so a projection is
// narrowing to a few names out of many rather than keeping almost everything.
const parWireFillers = 10

// parWireStore builds the store the constrained wire-query probes run against: enough ads to fill
// several segments per shard. It returns the literal values each ad was written with, which is the
// independent oracle: the parallel and serial paths share their row assembly, so comparing them to
// each other would be satisfied by both being wrong in the same way.
//
// interned also COLUMNARIZES, which is the state a long-running store settles into and the only
// state in which the columnar prefilter -- the thing the fan-out had to keep per worker -- runs at
// all. The inline (not interned) state is not redundant with it for an ENCRYPTED store: an interned
// record is converted to inline names before the row is assembled, and that conversion redacts, so
// only on an inline store is the assembler's own key the thing standing between a redacted read and
// a sealed value.
func parWireStore(t *testing.T, encrypted, interned bool) (*Collection, map[string]map[string]string) {
	t.Helper()
	opts := Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16}
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
		fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d; Owner="user%d"; RequestMemory=%d; Note="SECRET-%d"`,
			i, i%10, 1+i%5, i%7, (1+i%8)*1024, i)
		exp := map[string]string{
			"ClusterId":     fmt.Sprint(i),
			"ProcId":        fmt.Sprint(i % 10),
			"JobStatus":     fmt.Sprint(1 + i%5),
			"Owner":         fmt.Sprintf("%q", fmt.Sprintf("user%d", i%7)),
			"RequestMemory": fmt.Sprint((1 + i%8) * 1024),
			"Note":          fmt.Sprintf("%q", fmt.Sprintf("SECRET-%d", i)),
		}
		for f := 0; f < parWireFillers; f++ {
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
	if interned {
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
	}
	return c, want
}

// runWireQuery runs one constrained wire read down a CHOSEN path and returns the rows keyed by
// ClusterId.ProcId. Picking the path by the byte gate rather than hoping for it is the point: a
// probe that silently ran the serial scan twice would prove nothing about the fan-out.
//
// And it VERIFIES the path it asked for, through wireQueryFanouts, because the fallback is silent:
// the read declines to fan out, runs serially and returns the same rows, so without the counter a
// regression in the dispatch would leave every comparison below passing.
func runWireQuery(t *testing.T, c *Collection, qtext string, proj []string, redact bool, limit int, parallel bool) (map[string]map[string]string, int, ScanStats) {
	t.Helper()
	if parallel {
		c.parallelMinBytes = 0
	} else {
		c.parallelMinBytes = 1 << 62
	}
	before := wireQueryFanouts.Load()
	var stats ScanStats
	rows := make(map[string]map[string]string)
	count := 0
	for row := range c.QueryRawWireStats(mustParseQ(t, qtext), proj, redact, &stats) {
		attrs, myType := wireRowAttrs(t, row)
		if myType != "Job" {
			t.Errorf("row lost MyType: got %q", myType)
		}
		count++
		rows[attrs["ClusterId"]+"."+attrs["ProcId"]] = attrs
		if limit > 0 && count == limit {
			break
		}
	}
	switch fanned := wireQueryFanouts.Load() != before; {
	case parallel && !fanned && c.queryPar < 2:
		t.Skip("this machine resolves to serial query parallelism; nothing to fan out")
	case parallel && !fanned:
		// Not a skip. The budget is free (nothing else is querying this store) and the gate is
		// open, so a read that did not fan out means the dispatch is gone -- and skipping would
		// hide exactly that, since the fallback returns the same rows.
		t.Fatal("the parallel arm did not fan out")
	case !parallel && fanned:
		t.Fatal("the serial arm fanned out; the comparison would be the parallel path against itself")
	}
	return rows, count, stats
}

// TestQueryRawWireParallelMatchesSerial pins the constrained wire read's two implementations to
// each other AND to the values the ads were written with.
//
// The constrained read used to be serial always, and making it fan out meant giving every worker
// its own columnar prefilter, narrower, decompression buffer, subset scratch and stats. Each of
// those is a chance for a worker to read another's state: the symptom is not a crash but rows that
// are subtly wrong -- an attribute from the wrong record, a row dropped, a count doubled -- so the
// assertions below are on the row VALUES and on the scan counters, not on "did it return rows".
func TestQueryRawWireParallelMatchesSerial(t *testing.T) {
	c, want := parWireStore(t, false, true)
	proj := []string{"ClusterId", "ProcId", "JobStatus", "Owner", "RequestMemory"}
	cases := []struct {
		name  string
		q     string
		proj  []string
		match func(i int) bool
	}{
		// Reject-heavy: the shape the columnar prefilter exists for -- nearly every record is
		// decided from the columns and never reassembled.
		{"reject-heavy", "ClusterId < 50", proj, func(i int) bool { return i < 50 }},
		{"match-heavy", "ProcId < 8", proj, func(i int) bool { return i%10 < 8 }},
		// Whole ad: no projection plan, so the row carries every attribute and the narrowing is
		// bypassed -- a different assembly path from the projected one above.
		{"whole-ad", "JobStatus == 3", nil, func(i int) bool { return 1+i%5 == 3 }},
		// Two conjuncts over two columnar attributes, the EXPLAIN shape that decided 100% from
		// the columns on the production mirror.
		{"two-conjuncts", "JobStatus == 3 && ProcId > 6", nil, func(i int) bool { return 1+i%5 == 3 && i%10 > 6 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			par, parN, parStats := runWireQuery(t, c, tc.q, tc.proj, false, 0, true)
			ser, serN, serStats := runWireQuery(t, c, tc.q, tc.proj, false, 0, false)

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
			if parN != len(expect) || serN != len(expect) {
				t.Fatalf("row counts: parallel %d, serial %d, want %d", parN, serN, len(expect))
			}
			names := tc.proj
			if names == nil {
				names = make([]string, 0, len(want))
				for a := range want["0.0"] {
					names = append(names, a)
				}
			}
			for k, exp := range expect {
				for _, got := range []struct {
					path string
					row  map[string]string
				}{{"parallel", par[k]}, {"serial", ser[k]}} {
					if got.row == nil {
						t.Fatalf("%s read dropped key %s", got.path, k)
					}
					if len(got.row) != len(names) {
						t.Fatalf("%s read row %s has %v, want exactly %v", got.path, k, got.row, names)
					}
					for _, a := range names {
						if got.row[a] != exp[a] {
							t.Fatalf("%s read row %s: %s = %q, want %q", got.path, k, a, got.row[a], exp[a])
						}
					}
				}
			}

			// The counters must agree too, which is what catches both a worker accumulating into
			// a shared sink and the segment counts being recorded twice (once while gathering the
			// tasks, once by the walk).
			if parStats != serStats {
				t.Fatalf("scan stats differ:\n parallel %+v\n   serial %+v", parStats, serStats)
			}
			// And the prefilter must have SURVIVED the fan-out: without a per-worker colScope the
			// obvious way to make this path compile is to drop the prefilter, which costs a full
			// reassembly per rejected record and would leave this at zero.
			if parStats.RecordsColumnDecided == 0 {
				t.Fatalf("no record was decided from the columns; the parallel path lost the prefilter (%+v)", parStats)
			}
		})
	}

	// A limit stops the read early. Rows arrive in arbitrary order under fan-out, so the
	// assertion is on the SET they are drawn from, not on which ones they are.
	t.Run("limit", func(t *testing.T) {
		const limit = 37
		par, parN, _ := runWireQuery(t, c, "ProcId < 8", proj, false, limit, true)
		if parN != limit || len(par) != limit {
			t.Fatalf("limited read returned %d rows (%d distinct), want %d", parN, len(par), limit)
		}
		for k, row := range par {
			exp, ok := want[k]
			if !ok {
				t.Fatalf("limited read invented key %s", k)
			}
			for _, a := range proj {
				if row[a] != exp[a] {
					t.Fatalf("limited read row %s: %s = %q, want %q", k, a, row[a], exp[a])
				}
			}
		}
	})
}

// TestQueryRawWireParallelEncrypted covers the entitlement dispatch on the fan-out: the worker
// reads the render key once, from sel.redact, and a redacted read must hold none. Getting this
// wrong does not fail loudly -- an unentitled read quietly serves the plaintext, or an entitled one
// fails to assemble the row and drops the ad from the result entirely.
//
// Both storage states, because they decide it in different places. On an INTERNED store the record
// is converted to inline names first and that conversion redacts, so the row assembler's key does
// not decide the outcome; on an INLINE store nothing is converted and the assembler's key is the
// only thing standing between a redacted read and a sealed value. Pinning only the interned state
// left both "always entitled" and "never entitled" passing.
func TestQueryRawWireParallelEncrypted(t *testing.T) {
	for _, interned := range []bool{true, false} {
		name := "inline"
		if interned {
			name = "interned"
		}
		t.Run(name, func(t *testing.T) { parWireEncryptedProbe(t, interned) })
	}
}

func parWireEncryptedProbe(t *testing.T, interned bool) {
	c, want := parWireStore(t, true, interned)
	proj := []string{"ClusterId", "ProcId", "Owner", "Note"}
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
	par, parN, _ := runWireQuery(t, c, q, proj, false, 0, true)
	ser, serN, _ := runWireQuery(t, c, q, proj, false, 0, false)
	if parN != nWant || serN != nWant {
		t.Fatalf("entitled row counts: parallel %d, serial %d, want %d", parN, serN, nWant)
	}
	for k, row := range par {
		if row["Note"] != want[k]["Note"] {
			t.Fatalf("entitled parallel row %s: Note = %q, want %q", k, row["Note"], want[k]["Note"])
		}
		if ser[k]["Note"] != want[k]["Note"] {
			t.Fatalf("entitled serial row %s: Note = %q, want %q", k, ser[k]["Note"], want[k]["Note"])
		}
	}

	// Redacted: every row is still served (a secret the caller may not read becomes undefined,
	// it does not make the ad disappear) and no plaintext reaches the consumer.
	c.parallelMinBytes = 0
	before := wireQueryFanouts.Load()
	rows, leaked := 0, 0
	for row := range c.QueryRawWire(mustParseQ(t, q), proj, true) {
		rows++
		if bytes.Contains(row, []byte("SECRET-")) {
			leaked++
		}
	}
	if wireQueryFanouts.Load() == before && c.queryPar >= 2 {
		t.Fatal("the redacted read did not fan out; it would be probing the serial path")
	}
	if rows != nWant {
		t.Fatalf("redacted parallel read returned %d rows, want %d", rows, nWant)
	}
	if leaked != 0 {
		t.Fatalf("redacted parallel read leaked %d plaintext secrets", leaked)
	}
	// Not leaking is not the same as never holding: assert the key is never USED, so the
	// protection is a withheld key rather than a filter on the way out.
	ctr := &countingSealer{inner: c.sealer}
	c.sealer = ctr
	for range c.QueryRawWire(mustParseQ(t, q), proj, true) {
	}
	if n := ctr.opens.Load(); n != 0 {
		t.Fatalf("redacted parallel read opened %d sealed values; it should hold no key", n)
	}
	ctr.opens.Store(0)
	for range c.QueryRawWire(mustParseQ(t, q), proj, false) {
	}
	if ctr.opens.Load() == 0 {
		t.Fatal("entitled parallel read opened nothing; the probe is not reaching the key")
	}
	// Redacted and entitled must agree on everything but the sealed attribute.
	red, _, _ := runWireQuery(t, c, q, proj, true, 0, true)
	redSer, _, _ := runWireQuery(t, c, q, proj, true, 0, false)
	if len(red) != len(redSer) {
		t.Fatalf("redacted row counts: parallel %d, serial %d", len(red), len(redSer))
	}
	for k, row := range red {
		for _, a := range []string{"ClusterId", "ProcId", "Owner"} {
			if row[a] != want[k][a] {
				t.Fatalf("redacted parallel row %s: %s = %q, want %q", k, a, row[a], want[k][a])
			}
		}
		if row["Note"] != redSer[k]["Note"] {
			t.Fatalf("redacted row %s: parallel Note %q, serial %q", k, row["Note"], redSer[k]["Note"])
		}
	}
}
