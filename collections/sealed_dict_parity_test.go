package collections

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// A sealed value reads the same whichever segment layout holds it. An interned segment resolves
// attribute ids through its own dictionary, and every read that decodes from one has to open sealed
// values with the collection's key exactly as a read of an inline segment does; one that does not
// sees an undecodable node, takes the attribute as absent, and silently drops the record from a
// filter on it. These tests build the same records in every layout a sealed value is stored in and
// require every read to give the answer the plain evaluation of the source ads gives.

const sealedParityN = 300

// sealedParityOwner and the functions beside it give record i's sealed values, so a test can say what
// a read must return for it. A patched record (see the delta layout) has a different owner.
func sealedParityOwner(i int, patched bool) string {
	if patched && i%3 == 0 {
		return "user9"
	}
	return fmt.Sprintf("user%d", i%7)
}

func sealedParityToken(i int) (string, bool) {
	if i%5 == 0 {
		return "", false
	}
	return fmt.Sprintf("tok%d", i%13), true
}

func sealedParityJobStatus(i int, patched bool) int {
	if patched && i%3 == 0 {
		return 4
	}
	return 1 + i%5
}

// sealedParityAd is record i. Owner, Token and SealedNum are sealed by EncryptedAttrs and ClaimId as
// a private attribute; ProcId, JobStatus and RequestMemory stay in the clear so a filter can combine
// a sealed attribute with a column. Token is absent from every fifth record, for =?= undefined.
func sealedParityAd(i int, patched bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ MyType="Job"; TargetType="Machine"; ClusterId=%d; ProcId=%d; JobStatus=%d;`+
		` RequestMemory=%d; MemDouble = RequestMemory * 2; Owner=%q; SealedNum=%d; ClaimId="<secret-%d>"`,
		i, i%10, sealedParityJobStatus(i, patched), 512*(1+i%4), sealedParityOwner(i, patched), i%11, i)
	if tok, ok := sealedParityToken(i); ok {
		fmt.Fprintf(&b, `; Token=%q`, tok)
	}
	for f := 0; f < 6; f++ {
		fmt.Fprintf(&b, `; Filler%d="filler %d-%d"`, f, i, f)
	}
	b.WriteString(" ]")
	return b.String()
}

var sealedParityAttrs = []string{"Owner", "Token", "SealedNum"}

func sealedParityOpts(dir string) Options {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 3)
	}
	return Options{Dir: dir, Shards: 2, SegmentSize: 1 << 14, DataKey: key, DeltaMax: 8,
		EncryptedAttrs: sealedParityAttrs, QueryParallelism: 4}
}

// sealedLayoutCounts reports how many visible records are in inline segments and how many in
// interned (dictionary) segments, and how many segments are columnarized.
func sealedLayoutCounts(c *Collection) (inline, dict, columnar int) {
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		c.forEachVisible(s0, wins, func(_ []byte, _ Codec, d *segDictHandle) bool {
			if d != nil {
				dict++
			} else {
				inline++
			}
			return true
		})
		releaseWindows(wins)
	}
	return inline, dict, columnarSegmentsForTest(c)
}

// sealedPut stores records [lo, hi) in one transaction: the reads are what is under test, and a
// durable commit per record is most of the fixture's time.
func sealedPut(t testing.TB, c *Collection, lo, hi int) {
	t.Helper()
	tx := c.Begin()
	for i := lo; i < hi; i++ {
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), mustAd(t, sealedParityAd(i, false)))
	}
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("put conflicted")
	}
}

// internAll rewrites every shard into interned segments, the form a compaction leaves.
func internAll(c *Collection) {
	for _, sh := range c.shards {
		c.compactShard(sh, c.currentCodec())
	}
	c.reindexAfterCompaction()
}

type sealedLayout struct {
	name string
	// patched: the layout patches records i%3 == 0 after storing them (sealedParityAd(i, true)).
	patched bool
	// every: read it in parallel as well as serially, and under every match configuration. The
	// layouts that mix inline, interned, columnarized and delta records reach every arm; the
	// others run the default serial reads, to keep the test's time down.
	every bool
	// build stores the records into c, laid out as named, and returns the collection to read, which
	// is a reopened one for a layout that tests recovery.
	build func(t *testing.T, c *Collection, dir string) *Collection
	// layoutOK validates the layout, so a fixture that drifted cannot make the comparison vacuous.
	layoutOK func(c *Collection, inline, dict, columnar int) bool
}

var sealedLayouts = []sealedLayout{
	{name: "inline", build: func(t *testing.T, c *Collection, _ string) *Collection {
		sealedPut(t, c, 0, sealedParityN)
		return c
	}, layoutOK: func(_ *Collection, in, d, _ int) bool { return in == sealedParityN && d == 0 }},
	{name: "dict", build: func(t *testing.T, c *Collection, _ string) *Collection {
		sealedPut(t, c, 0, sealedParityN)
		internAll(c)
		return c
	}, layoutOK: func(_ *Collection, in, d, _ int) bool { return in == 0 && d == sealedParityN }},
	{name: "mixed", build: func(t *testing.T, c *Collection, _ string) *Collection {
		sealedPut(t, c, 0, sealedParityN/2)
		internAll(c)
		sealedPut(t, c, sealedParityN/2, sealedParityN)
		return c
	}, layoutOK: func(_ *Collection, in, d, _ int) bool { return in > 0 && d > 0 }},
	{name: "columnar", every: true, build: func(t *testing.T, c *Collection, _ string) *Collection {
		sealedPut(t, c, 0, sealedParityN*3/4)
		internAll(c)
		if !c.BuildAndEnableSchemaScan(2000, 8) {
			t.Fatal("no columnar schema derived")
		}
		c.ColumnarizeSealed()
		c.Reindex()
		sealedPut(t, c, sealedParityN*3/4, sealedParityN)
		return c
	}, layoutOK: func(_ *Collection, in, d, col int) bool { return in > 0 && d > 0 && col > 0 }},
	{name: "delta", patched: true, every: true, build: func(t *testing.T, c *Collection, _ string) *Collection {
		// Every base in an interned segment, every patch a delta record over it: a patched record's
		// sealed Owner comes from the delta and its other sealed values from the interned base.
		sealedPut(t, c, 0, sealedParityN)
		internAll(c)
		tx := c.Begin()
		for i := 0; i < sealedParityN; i += 3 {
			patch := classad.New()
			patch.InsertAttr("JobStatus", int64(sealedParityJobStatus(i, true)))
			patch.InsertAttrString("Owner", sealedParityOwner(i, true))
			tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), patch, nil)
		}
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("patch conflicted")
		}
		return c
	}, layoutOK: func(c *Collection, _, d, _ int) bool {
		deltas, _ := c.DeltaStats()
		return d > 0 && deltas > 0
	}},
	{name: "reopened", build: func(t *testing.T, c *Collection, dir string) *Collection {
		sealedPut(t, c, 0, sealedParityN)
		if _, err := c.RetrainDict(2000); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		c2, err := Open(sealedParityOpts(dir))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c2.Close() })
		return c2
	}, layoutOK: func(_ *Collection, in, d, _ int) bool { return d > 0 && in+d == sealedParityN }},
}

// openSealedLayout opens a fresh encrypted store and lays the records out as lay says.
func openSealedLayout(t *testing.T, lay sealedLayout) *Collection {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(sealedParityOpts(dir))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c = lay.build(t, c, dir)
	if in, d, col := sealedLayoutCounts(c); !lay.layoutOK(c, in, d, col) {
		t.Fatalf("layout %s: %d inline, %d interned records, %d columnarized segments", lay.name, in, d, col)
	}
	return c
}

// sealedParityFilters each read a sealed attribute: alone, compared, probed for absence, and inside
// a conjunction or disjunction with a column.
var sealedParityFilters = []string{
	`Owner == "user3"`,
	`Owner < "user3"`,
	`SealedNum > 5`,
	`Token =?= undefined`,
	`Token =!= undefined`,
	`Token == "tok4"`,
	`ClaimId == "<secret-17>"`,
	`Owner == "user3" && ProcId < 4`,
	`ProcId < 4 && SealedNum >= 7`,
	`ProcId < 3 && Token =?= undefined`,
	`Owner == "user3" || JobStatus == 2`,
	`SealedNum == 7 || ProcId == 1`,
	`regexp("^user[12]$", Owner)`,
	// eval() names what it reads only when it runs, so the record is matched on its whole decode
	// rather than on the attributes the query names.
	`eval(strcat("Own", "er")) == "user3"`,
}

// sealedSources parses the source ads once per form; the oracle evaluates every filter over them.
func sealedSources(t *testing.T, patched bool) []*classad.ClassAd {
	t.Helper()
	out := make([]*classad.ClassAd, sealedParityN)
	for i := range out {
		out[i] = mustAd(t, sealedParityAd(i, patched))
	}
	return out
}

// sealedHiddenSources is sealedSources as a reader not entitled to sealed values is shown them: every
// sealed attribute removed. Such a reader's evaluation must give the answer these give.
func sealedHiddenSources(t *testing.T, patched bool) []*classad.ClassAd {
	t.Helper()
	out := sealedSources(t, patched)
	for _, ad := range out {
		for _, name := range append(slices.Clone(sealedParityAttrs), "ClaimId") {
			ad.Delete(name)
		}
	}
	return out
}

// sealedOracle evaluates q against every source ad: the answer every read must give.
func sealedOracle(q *vm.Query, src []*classad.ClassAd) map[int]bool {
	out := map[int]bool{}
	for i, ad := range src {
		if q.Matches(ad) {
			out[i] = true
		}
	}
	return out
}

func sameIDs(t *testing.T, label string, ids []int, want map[int]bool) {
	t.Helper()
	got := make(map[int]bool, len(ids))
	for _, id := range ids {
		if got[id] {
			t.Errorf("%s: ClusterId %d returned twice", label, id)
		}
		got[id] = true
	}
	var missing, extra []int
	for k := range want {
		if !got[k] {
			missing = append(missing, k)
		}
	}
	for k := range got {
		if !want[k] {
			extra = append(extra, k)
		}
	}
	if len(missing) == 0 && len(extra) == 0 {
		return
	}
	sort.Ints(missing)
	sort.Ints(extra)
	t.Errorf("%s: %d records, want %d (missing %d %v, extra %d %v)", label, len(got), len(want),
		len(missing), sealedFirst(missing), len(extra), sealedFirst(extra))
}

func sealedFirst(s []int) []int {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// sealedPlaintexts are substrings only a sealed value's plaintext contains.
var sealedPlaintexts = []string{"user", "tok", "<secret-"}

// checkSealedRow checks a rendered row (attribute -> value text): its sealed values must be the
// source's for an entitled read, and absent or undefined for a redacted one. Returns the row's id.
func checkSealedRow(t *testing.T, label string, m map[string]string, redact, patched bool) int {
	t.Helper()
	i, err := strconv.Atoi(m["ClusterId"])
	if err != nil {
		t.Fatalf("%s: ClusterId %q is not an integer", label, m["ClusterId"])
	}
	if redact {
		for _, name := range []string{"Owner", "Token", "SealedNum", "ClaimId"} {
			if v, ok := m[name]; ok && v != "undefined" {
				t.Fatalf("%s: a redacted row carries %s = %s", label, name, v)
			}
		}
		for name, v := range m {
			for _, pt := range sealedPlaintexts {
				if strings.Contains(v, pt) {
					t.Fatalf("%s: a redacted row carries plaintext in %s = %s", label, name, v)
				}
			}
		}
		return i
	}
	want := map[string]string{
		"Owner":     strconv.Quote(sealedParityOwner(i, patched)),
		"SealedNum": strconv.Itoa(i % 11),
		"ClaimId":   strconv.Quote(fmt.Sprintf("<secret-%d>", i)),
	}
	if tok, ok := sealedParityToken(i); ok {
		want["Token"] = strconv.Quote(tok)
	}
	for name, w := range want {
		if m[name] != w {
			t.Fatalf("%s: record %d %s = %q, want %s", label, i, name, m[name], w)
		}
	}
	if v, ok := m["Token"]; ok && want["Token"] == "" {
		t.Fatalf("%s: record %d has no Token, read %s", label, i, v)
	}
	return i
}

// adRow renders the attributes checkSealedRow reads from a decoded ad.
func adRow(ad *classad.ClassAd) map[string]string {
	m := map[string]string{}
	for _, name := range []string{"ClusterId", "Owner", "Token", "SealedNum", "ClaimId"} {
		if e, ok := ad.Lookup(name); ok {
			m[name] = e.String()
		}
	}
	return m
}

var sealedProj = []string{"ClusterId", "Owner", "Token", "SealedNum", "ClaimId"}

var sealedProjectAttrs = append(slices.Clone(sealedProj), "MemDouble")

// TestSealedValueDictSegmentParity: every read of a filter on a sealed attribute returns exactly the
// records the plain evaluation of the source ads selects, with the sealed values an entitled reader
// is owed and none for a redacted one, in every layout -- inline, interned, mixed, columnarized,
// delta records over interned bases, and reopened from disk.
//
// The full set of reads runs serially under the default match configuration. On the layouts that
// mix every record form it also runs in parallel, and Query also runs with the narrowed match and
// the column conjunct rejection each switched off, since those pick which decode a record is
// matched on.
func TestSealedValueDictSegmentParity(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	sources := map[bool][]*classad.ClassAd{false: sealedSources(t, false), true: sealedSources(t, true)}
	hidden := map[bool][]*classad.ClassAd{false: sealedHiddenSources(t, false), true: sealedHiddenSources(t, true)}
	for _, lay := range sealedLayouts {
		t.Run(lay.name, func(t *testing.T) {
			c := openSealedLayout(t, lay)
			for _, parallel := range []bool{false, true} {
				if parallel && !lay.every {
					continue
				}
				if parallel {
					c.parallelMinBytes = 0
				} else {
					c.parallelMinBytes = 1 << 62
				}
				for _, f := range sealedParityFilters {
					q := mustParseQ(t, f)
					want := sealedOracle(q, sources[lay.patched])
					wantHidden := sealedOracle(q, hidden[lay.patched])
					if len(want) == 0 || len(want) == sealedParityN {
						t.Fatalf("%s: the oracle selects %d of %d records; the filter discriminates nothing",
							f, len(want), sealedParityN)
					}
					for _, arm := range narrowBenchArms {
						full := arm.narrow && arm.conjunct // the default configuration
						if (parallel || !lay.every) && !full {
							continue // see sealedLayout.every
						}
						restore := arm.set()
						sealedCheckReads(t, c, f, fmt.Sprintf("%s parallel=%v", arm.name, parallel), q, want,
							wantHidden, lay.patched, full)
						restore()
					}
				}
			}
			if t.Failed() {
				return
			}
			for i := 0; i < sealedParityN; i++ {
				key := []byte(fmt.Sprintf("%d.0", i))
				for _, redact := range []bool{false, true} {
					get := c.Get
					if redact {
						get = c.GetRedacted
					}
					ad, ok := get(key)
					if !ok {
						t.Fatalf("Get(%s) redact=%v: missing", key, redact)
					}
					checkSealedRow(t, fmt.Sprintf("Get redact=%v", redact), adRow(ad), redact, lay.patched)
				}
			}
		})
	}
}

// sealedCheckReads runs the reads of q over c and compares each with want. full runs every read;
// otherwise only Query, which reaches every arm of the matcher the configuration switches between.
//
// A redacted read is checked against wantHidden: it evaluates with no key, so it must select exactly what
// the plain evaluation of the source ads with every sealed attribute removed selects.
func sealedCheckReads(t *testing.T, c *Collection, f, arm string, q *vm.Query, want, wantHidden map[int]bool, patched, full bool) {
	t.Helper()
	lbl := func(read string) string { return fmt.Sprintf("[%s] %s: %s", arm, read, f) }

	var ids []int
	for ad := range c.Query(q) {
		ids = append(ids, checkSealedRow(t, lbl("Query"), adRow(ad), false, patched))
	}
	sameIDs(t, lbl("Query"), ids, want)
	if !full {
		return
	}

	ids = ids[:0]
	for ad := range c.QueryRedacted(q) {
		ids = append(ids, checkSealedRow(t, lbl("QueryRedacted"), adRow(ad), true, patched))
	}
	sameIDs(t, lbl("QueryRedacted"), ids, wantHidden)

	sealedCheckKeyless(t, c, lbl, q, wantHidden, patched)

	for _, count := range []struct {
		name string
		fn   func(*vm.Query) (int, bool)
	}{
		{"CountQuery", c.CountQuery},
		{"PresenceCountQuery", c.PresenceCountQuery},
		{"ColumnarEvalCount", c.ColumnarEvalCount},
		{"VectorEvalCount", c.VectorEvalCount},
	} {
		if n, ok := count.fn(q); ok && n != len(want) {
			t.Errorf("%s = %d, want %d", lbl(count.name), n, len(want))
		}
	}

	for _, redact := range []bool{false, true} {
		wantAs := want
		if redact {
			wantAs = wantHidden
		}
		for _, chase := range []bool{false, true} {
			ids = ids[:0]
			for ra := range c.QueryRawProjected(q, sealedProj, chase, redact) {
				ids = append(ids, checkSealedRow(t, lbl("QueryRawProjected"), rawAdMap(t, ra), redact, patched))
			}
			sameIDs(t, lbl(fmt.Sprintf("QueryRawProjected chase=%v redact=%v", chase, redact)), ids, wantAs)
		}
		ids = ids[:0]
		for row := range c.QueryRawWire(q, sealedProj, redact) {
			ids = append(ids, checkSealedRow(t, lbl("QueryRawWire"), wireRowMap(t, row), redact, patched))
		}
		sameIDs(t, lbl(fmt.Sprintf("QueryRawWire redact=%v", redact)), ids, wantAs)
		ids = ids[:0]
		for row := range c.QueryRawWireRefs(q, sealedProj, redact) {
			ids = append(ids, checkSealedRow(t, lbl("QueryRawWireRefs"), wireRowMap(t, row), redact, patched))
		}
		sameIDs(t, lbl(fmt.Sprintf("QueryRawWireRefs redact=%v", redact)), ids, wantAs)
	}

	ids = ids[:0]
	seq, _ := c.QueryRawFromSeq(q, sealedProj, SeqCursor{}, 0)
	for ra := range seq {
		ids = append(ids, checkSealedRow(t, lbl("QueryRawFromSeq"), rawAdMap(t, ra), false, patched))
	}
	sameIDs(t, lbl("QueryRawFromSeq"), ids, want)

	// QueryProject evaluates what it projects, so a sealed value -- not a literal on the wire -- and
	// an expression are both read from a decode of the record.
	ids = ids[:0]
	for vals := range c.QueryProject(q, sealedProjectAttrs) {
		m := map[string]string{}
		for i, v := range vals {
			switch {
			case v.IsString():
				s, _ := v.StringValue()
				m[sealedProjectAttrs[i]] = strconv.Quote(s)
			case v.IsInteger():
				n, _ := v.IntValue()
				m[sealedProjectAttrs[i]] = strconv.FormatInt(n, 10)
			case !v.IsUndefined():
				m[sealedProjectAttrs[i]] = v.String()
			}
		}
		id := checkSealedRow(t, lbl("QueryProject"), m, false, patched)
		if want := strconv.Itoa(2 * 512 * (1 + id%4)); m["MemDouble"] != want {
			t.Fatalf("%s: record %d MemDouble = %q, want %s", lbl("QueryProject"), id, m["MemDouble"], want)
		}
		ids = append(ids, id)
	}
	sameIDs(t, lbl("QueryProject"), ids, want)

	// Aggregates over the matched records: grouped by a column and by a sealed attribute, and the
	// statistics of a column and of a sealed attribute. A columnar aggregate that cannot serve the
	// shape declines (ok=false), which is not checked; one that answers must answer right.
	byStatus, byOwner := map[string]int{}, map[string]int{}
	var memSum, numSum int64
	for i := range want {
		byStatus[strconv.Itoa(sealedParityJobStatus(i, patched))]++
		byOwner[strconv.Quote(sealedParityOwner(i, patched))]++
		memSum += int64(512 * (1 + i%4))
		numSum += int64(i % 11)
	}
	for _, g := range []struct {
		attr string
		want map[string]int
	}{{"JobStatus", byStatus}, {"Owner", byOwner}} {
		if gs, ok := c.GroupStatsQuery(q, g.attr, nil); ok {
			got := map[string]int{}
			for _, s := range gs {
				got[s.Values[0].String()] = s.Count
			}
			if !sealedMapsEqual(got, g.want) {
				t.Errorf("%s by %s = %v, want %v", lbl("GroupStatsQuery"), g.attr, got, g.want)
			}
		}
	}
	for _, a := range []struct {
		attr string
		sum  int64
	}{{"RequestMemory", memSum}, {"SealedNum", numSum}} {
		if ns, ok := c.NumStatsQuery(q, a.attr); ok && (ns.N != len(want) || ns.IntSum != a.sum) {
			t.Errorf("%s(%s) = n %d sum %d, want n %d sum %d", lbl("NumStatsQuery"), a.attr,
				ns.N, ns.IntSum, len(want), a.sum)
		}
		if _, seen, ok := c.TopKOrderThreshold(q, a.attr, true, 5, nil); ok && seen != len(want) {
			t.Errorf("%s(%s): seen %d, want %d", lbl("TopKOrderThreshold"), a.attr, seen, len(want))
		}
	}
}

func sealedMapsEqual(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// TestSealedParentValueDictSegmentParity is the chained form: a job's Owner lives on its cluster ad
// (the parent) and is sealed there, so a filter on it is answered from the parent's record, which may
// be in an interned segment while the child is not, or the other way round.
func TestSealedParentValueDictSegmentParity(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const clusters, procs = 60, 5
	parentAd := func(cl int) string {
		return fmt.Sprintf(`[ ClusterId=%d; Owner="user%d"; SealedNum=%d; Cmd="/bin/sleep" ]`, cl, cl%7, cl%11)
	}
	childAd := func(cl, p int) string {
		return fmt.Sprintf(`[ ProcId=%d; JobStatus=%d; Filler0="filler %d-%d" ]`, p, 1+(cl+p)%5, cl, p)
	}
	put := func(t *testing.T, c *Collection, parents bool) {
		tx := c.Begin()
		for cl := 1; cl <= clusters; cl++ {
			if parents {
				tx.Put([]byte(fmt.Sprintf("%d.-1", cl)), mustAd(t, parentAd(cl)))
				continue
			}
			for p := 0; p < procs; p++ {
				tx.Put([]byte(fmt.Sprintf("%d.%d", cl, p)), mustAd(t, childAd(cl, p)))
			}
		}
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("put conflicted")
		}
	}
	layouts := []struct {
		name  string
		build func(t *testing.T, c *Collection)
	}{
		{"inline", func(t *testing.T, c *Collection) { put(t, c, true); put(t, c, false) }},
		{"dict", func(t *testing.T, c *Collection) { put(t, c, true); put(t, c, false); internAll(c) }},
		{"parents interned", func(t *testing.T, c *Collection) { put(t, c, true); internAll(c); put(t, c, false) }},
		{"children interned", func(t *testing.T, c *Collection) { put(t, c, false); internAll(c); put(t, c, true) }},
	}
	filters := []string{
		`Owner == "user3"`,
		`Owner == "user3" && ProcId < 2`,
		`SealedNum > 5 || JobStatus == 2`,
		`eval(strcat("Own", "er")) == "user3"`,
	}
	for _, lay := range layouts {
		t.Run(lay.name, func(t *testing.T) {
			opts := sealedParityOpts(t.TempDir())
			opts.ParentKeyFor, opts.IsStructural = jobParentKey, jobStructural
			c, err := Open(opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			lay.build(t, c)
			if _, d, _ := sealedLayoutCounts(c); (d > 0) != (lay.name != "inline") {
				t.Fatalf("layout %s: %d records interned", lay.name, d)
			}
			for _, f := range filters {
				q := mustParseQ(t, f)
				want := map[string]bool{}
				for cl := 1; cl <= clusters; cl++ {
					parent := mustAd(t, parentAd(cl))
					for p := 0; p < procs; p++ {
						child := mustAd(t, childAd(cl, p))
						child.SetParent(parent)
						if q.Matches(child) {
							want[fmt.Sprintf("%d.%d", cl, p)] = true
						}
					}
				}
				if len(want) == 0 {
					t.Fatalf("%s: the oracle selects nothing", f)
				}
				for _, arm := range narrowBenchArms {
					restore := arm.set()
					got := map[string]bool{}
					for ad := range c.Query(q) {
						cl, _ := ad.EvaluateAttrInt("ClusterId")
						p, _ := ad.EvaluateAttrInt("ProcId")
						if owner, _ := ad.EvaluateAttrString("Owner"); owner != fmt.Sprintf("user%d", cl%7) {
							t.Fatalf("[%s] %s: %d.%d read Owner %q", arm.name, f, cl, p, owner)
						}
						got[fmt.Sprintf("%d.%d", cl, p)] = true
					}
					restore()
					if !sealedMapsEqualBool(got, want) {
						t.Errorf("[%s] Query: %s: %d records, want %d", arm.name, f, len(got), len(want))
					}
				}
			}
		})
	}
}

func sealedMapsEqualBool(a, b map[string]bool) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if !b[k] {
			return false
		}
	}
	return true
}

// TestAddIndexSkipsSealedAttrs: a runtime index on an attribute the collection seals is refused the
// way Open and SetEncryptedAttrs refuse one -- the stored value is ciphertext, so the index could
// only make every record a candidate -- while the plaintext attributes beside it are indexed, and a
// collection without encryption indexes a private attribute as before.
func TestAddIndexSkipsSealedAttrs(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	c, err := Open(sealedParityOpts(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	sealedPut(t, c, 0, 100)
	if !c.AddIndex([]string{"Owner", "ClaimId", "Filler0"}, []string{"SealedNum", "RequestMemory"}) {
		t.Fatal("AddIndex changed nothing")
	}
	cat, val := c.IndexedAttrs()
	if !slices.Equal(cat, []string{"Filler0"}) || !slices.Equal(val, []string{"RequestMemory"}) {
		t.Fatalf("indexed categorical %v value %v; want only the plaintext attributes", cat, val)
	}
	if c.AddIndex([]string{"Owner"}, []string{"SealedNum"}) {
		t.Fatal("AddIndex of sealed attributes alone reported a change")
	}
	c.Reindex()
	n := 0
	for range c.Query(mustParseQ(t, `Owner == "user3" && RequestMemory == 1024`)) {
		n++
	}
	if want := len(sealedOracle(mustParseQ(t, `Owner == "user3" && RequestMemory == 1024`), sealedSources(t, false)[:100])); n != want {
		t.Fatalf("Query over the indexed store: %d records, want %d", n, want)
	}

	plain := New(Options{})
	if !plain.AddIndex([]string{"ClaimId"}, nil) {
		t.Fatal("a collection without encryption refused to index a private attribute")
	}
}

// TestQueryProjectInternedSegments: QueryProject decodes a record whose projected attribute is not a
// literal through the record's segment dictionary. It used the collection-wide decode, which cannot
// read an interned record, so on a plain store as much as an encrypted one every attribute of such a
// row came back ERROR -- for a mutable table after compaction and for an archive interned at seal,
// which is where the aggregates that scan through QueryProject read from.
func TestQueryProjectInternedSegments(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 300
	check := func(t *testing.T, c *Collection) {
		t.Helper()
		if _, d, _ := sealedLayoutCounts(c); d == 0 {
			t.Fatal("no record is in an interned segment: the case is not covered")
		}
		rows := 0
		for vals := range c.QueryProject(mustParseQ(t, "ProcId < 4"), []string{"ClusterId", "MemDouble"}) {
			id, err := vals[0].IntValue()
			if err != nil {
				t.Fatalf("ClusterId = %v", vals[0])
			}
			if got, _ := vals[1].IntValue(); got != 2*512*(1+id%4) {
				t.Fatalf("record %d MemDouble = %v, want %d", id, vals[1], 2*512*(1+id%4))
			}
			rows++
		}
		if rows != n*4/10 {
			t.Fatalf("%d rows, want %d", rows, n*4/10)
		}
	}
	t.Run("mutable", func(t *testing.T) {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 14})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		sealedPut(t, c, 0, n)
		internAll(c)
		check(t, c)
	})
	t.Run("archive", func(t *testing.T) {
		a, err := CreateArchive(ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 14, InternAtSeal: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { a.Close() })
		for i := 0; i < n; i++ {
			if err := a.Append(mustAd(t, sealedParityAd(i, false))); err != nil {
				t.Fatal(err)
			}
		}
		a.c.InternSealed() // the eager hook interns as segments seal; this catches up any it has not reached
		check(t, a.c)
	})
}
