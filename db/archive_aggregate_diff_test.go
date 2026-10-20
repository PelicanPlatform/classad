package db

import (
	"fmt"
	"math"
	"math/rand"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// Differential test: every aggregate an archive answers -- by whichever fast path it picks -- must equal
// the same aggregate computed by brute force over every record.
//
// The fixture is shaped like a production history table, because that is where the fast paths and the
// data disagreed. Most jobs ran and carry usage attributes; some were removed before running and carry
// none, so those attributes fall out of the base columnar schema into group schemas. Some schema fields
// hold an EXPRESSION in a minority of records (FooExpr, BarStr), the way MemoryUsage or RequestMemory do
// in real ads. The archive is maintained the way the server maintains it -- merged, columnarized, group
// schemas derived -- and reopened, so the columns are read back from disk.
//
// What this caught: a columnarized segment keeps a record's expression-valued attributes in the record,
// and the column readers took "not in the block" to mean "undefined". `FooExpr is undefined` counted
// every expression-valued record; the numeric GROUP BY agreed with that count, and only a string GROUP
// BY -- which scans -- disagreed. On a production history table that read as the string grouping
// undercounting 154k "no usage" jobs as ~3.3k, when the count was the wrong one.
//
// CLASSAD_AGG_DIFF_HEAVY=1 runs a larger archive and many more random constraints.

// exprs=false writes literals only, for the newest batch: the open segment's row walk declines on an
// expression, which would hide a columnar reader that mishandles one in the sealed segments.
func diffHistoryAd(rng *rand.Rand, i int, exprs bool) string {
	u := rng.Intn(40)
	owner := fmt.Sprintf("user%02d", u)
	if u%7 == 0 && rng.Intn(2) == 0 {
		owner = strings.ToUpper(owner) // fold-equal but distinct group keys
	}
	cid := 1000 + i/10
	t0 := 1700000000 + i*7
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f+"\n", a...) }
	w("ClusterId = %d", cid)
	w("ProcId = %d", i%10)
	w("Owner = %q", owner)
	w("QDate = %d", t0-500)
	// A zone-mapped attribute that is an expression in a few records, evaluating past every literal
	// in its segment: a zone map that leaves it out prunes the segment holding it.
	if exprs && rng.Intn(20) == 0 {
		w("EnteredHistoryTime = QDate + 40505")
	} else {
		w("EnteredHistoryTime = %d", t0+5)
	}
	w("JobUniverse = %d", []int{5, 5, 5, 7}[rng.Intn(4)])
	w("CondorVersion = %q", fmt.Sprintf("$CondorVersion: 24.%d.%d BuildID: %d $", rng.Intn(4), rng.Intn(3), 7000+rng.Intn(3)))
	w("Cmd = %q", "/home/"+owner+"/run.sh")
	w("GlobalJobId = %q", fmt.Sprintf("ap#%d.%d#%d", cid, i%10, t0-500))
	w("RequestCpus = %d", 1+rng.Intn(4))
	w("Requirements = (TARGET.Memory >= RequestMemory)")
	// Schema fields that are an expression in a minority of records.
	if rng.Intn(10) == 0 && exprs {
		w("FooExpr = RequestCpus * 2")
	} else {
		w("FooExpr = %d", rng.Intn(5))
	}
	if rng.Intn(10) == 0 && exprs {
		w("BarStr = strcat(\"x\", Owner)")
	} else {
		w("BarStr = %q", fmt.Sprintf("bar%d", rng.Intn(5)))
	}
	if rng.Intn(100) < 15 { // removed before it ran: no usage at all
		w("JobStatus = 3")
		if exprs && rng.Intn(4) == 0 {
			w("CompletionDate = -1") // a negative constant is unary minus over a literal
		} else {
			w("CompletionDate = 0")
		}
		w("RemoteWallClockTime = 0.0")
		w("NumJobStarts = 0")
		if rng.Intn(2) == 0 {
			w("HoldReasonCode = 16")
		}
		return b.String()
	}
	w("JobStatus = 4")
	w("CompletionDate = %d", t0)
	w("RemoteWallClockTime = %d.0", 100+rng.Intn(5000))
	w("NumJobStarts = %d", 1+rng.Intn(3))
	w("ExitCode = %d", rng.Intn(3))
	w("LastRemoteHost = %q", fmt.Sprintf("slot1@host%03d.site%d.org", rng.Intn(100), rng.Intn(8)))
	if rng.Intn(3) == 0 {
		w("MachineAttrGLIDEIN_Site0 = undefined")
	} else {
		w("MachineAttrGLIDEIN_Site0 = %q", fmt.Sprintf("Site_%d", rng.Intn(10)))
	}
	if exprs {
		w("MemoryUsage = ((ResidentSetSize + 1023) / 1024)")
	}
	switch rng.Intn(4) {
	case 0: // ran, used nothing; the starter writes these as integers
		w("RemoteUserCpu = 0")
		w("CpusUsage = 0")
		w("ResidentSetSize = 0")
	case 1:
		w("RemoteUserCpu = %d", rng.Intn(5000))
		w("CpusUsage = %d", rng.Intn(3))
		w("ResidentSetSize = %d", rng.Intn(1<<20))
	default:
		w("RemoteUserCpu = %d.0", rng.Intn(5000))
		w("CpusUsage = %f", rng.Float64()*2)
		w("ResidentSetSize = %d", rng.Intn(1<<20))
	}
	if rng.Intn(4) != 0 {
		w("DiskUsage = %d", rng.Intn(1<<20))
	}
	return b.String()
}

// diffConstraint builds a random constraint over the fixture's attributes: presence probes, numeric and
// string comparisons, elvis defaults, and their conjunctions, disjunctions and negations -- the shapes
// someone asking for "jobs with no usage" writes.
func diffConstraint(rng *rand.Rand, depth int) string {
	nums := []string{"CpusUsage", "RemoteUserCpu", "RemoteWallClockTime", "NumJobStarts", "JobStatus",
		"FooExpr", "DiskUsage", "HoldReasonCode", "ExitCode", "MemoryUsage", "ResidentSetSize", "EnteredHistoryTime",
		"CompletionDate"}
	strs := []string{"Owner", "BarStr", "LastRemoteHost", "CondorVersion", "MachineAttrGLIDEIN_Site0"}
	all := append(append([]string(nil), nums...), strs...)
	if depth > 0 && rng.Intn(3) == 0 {
		l, r := diffConstraint(rng, depth-1), diffConstraint(rng, depth-1)
		switch rng.Intn(3) {
		case 0:
			return "(" + l + ") && (" + r + ")"
		case 1:
			return "(" + l + ") || (" + r + ")"
		default:
			return "!(" + l + ")"
		}
	}
	switch rng.Intn(5) {
	case 0:
		a := all[rng.Intn(len(all))]
		return a + []string{" is undefined", " isnt undefined", " =?= undefined", " =!= undefined"}[rng.Intn(4)]
	case 1:
		return "isUndefined(" + all[rng.Intn(len(all))] + ")"
	case 2:
		n := nums[rng.Intn(len(nums))]
		v := []string{"0", "1", "2", "0.5", "100", "1700020000"}[rng.Intn(6)]
		if rng.Intn(3) == 0 {
			return "(" + n + " ?: 0) " + []string{"==", "<", ">="}[rng.Intn(3)] + " " + v
		}
		return n + " " + []string{"==", "!=", "<", "<=", ">", ">="}[rng.Intn(6)] + " " + v
	case 3:
		s := strs[rng.Intn(len(strs))]
		lit := []string{"user03", "USER07", "bar1", "Site_2", "xuser05"}[rng.Intn(5)]
		return s + " " + []string{"==", "!=", "=?="}[rng.Intn(3)] + " " + strconv.Quote(lit)
	default:
		return "RemoteUserCpu + FooExpr " + []string{"==", ">"}[rng.Intn(2)] + " 0"
	}
}

// buildDiffArchive builds and maintains the fixture: merged, columnarized, group schemas derived, and
// reopened between batches, with an unsealed tail at the end.
func buildDiffArchive(t *testing.T, batches []int) *ArchiveTable {
	t.Helper()
	dir := t.TempDir()
	cfg := ArchiveConfig{
		SegmentSize:        64 << 10,
		CategoricalAttrs:   []string{"Owner"},
		ValueAttrs:         []string{"ClusterId"},
		ZoneAttrs:          []string{"CompletionDate", "EnteredHistoryTime"},
		GroupStabilityRuns: 1,
	}
	var cat *Catalog
	var h *ArchiveTable
	open := func() {
		var err error
		if cat, err = OpenCatalog(dir); err != nil {
			t.Fatal(err)
		}
		if h, err = cat.CreateArchiveTable("history", cfg); err != nil {
			t.Fatal(err)
		}
	}
	maintain := func() {
		h.MergePass(MergeOptions{})
		if stale, _ := h.StaleIndexSegments(); stale > 0 {
			h.Reindex()
		}
		h.BuildAndEnableSchemaScan(2000, 32)
		h.GroupSchemas(2000, 0)
	}
	rng := rand.New(rand.NewSource(1))
	n := 0
	open()
	for bi, k := range batches {
		for j := 0; j < k; j++ {
			if err := h.AppendOld(diffHistoryAd(rng, n, bi < len(batches)-1)); err != nil {
				t.Fatal(err)
			}
			n++
		}
		if bi == len(batches)-1 {
			break // leave the newest records where the writer put them
		}
		maintain()
		maintain() // the second pass is the one that adopts the group schemas the first derived
		if bi == 0 {
			cat.Close()
			open()
		}
	}
	t.Cleanup(func() { cat.Close() })
	if info := h.SchemaScanInfo(); info.CoveredSegments == 0 || info.GroupSchemas == 0 {
		t.Fatalf("fixture lacks the shape under test: covered=%d groups=%d", info.CoveredSegments, info.GroupSchemas)
	}
	return h
}

// refAggregate is the brute-force answer: filter every record by evaluating the constraint on its full
// ad, then reduce with the same engine the scan path uses.
func refAggregate(t *testing.T, ads []*classad.ClassAd, constraint string, groupBy []string, aggs []AggSpec) []AggRow {
	t.Helper()
	expr, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatalf("%s: %v", constraint, err)
	}
	groupCols := make([]GroupCol, len(groupBy))
	for i, g := range groupBy {
		groupCols[i] = GroupCol{Attr: g}
	}
	attrs, groupCol, aggCol := AggProjection(groupCols, aggs)
	seq := func(yield func([]classad.Value) bool) {
		for _, ad := range ads {
			if b, berr := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); berr != nil || !b {
				continue
			}
			vals := make([]classad.Value, len(attrs))
			for i, a := range attrs {
				vals[i] = ad.EvaluateAttr(a)
			}
			if !yield(vals) {
				return
			}
		}
	}
	rows, err := AggregateValues(seq, attrs, groupCols, aggs, groupCol, aggCol, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// sameValue compares two rendered aggregate values. Reals may differ in the last place, since a column
// pass adds in a different order than a scan (see archive_groupcols.go).
func sameValue(a, b string) bool {
	if a == b {
		return true
	}
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea != nil || eb != nil {
		return false
	}
	return math.Abs(fa-fb) <= 1e-9*math.Max(1, math.Max(math.Abs(fa), math.Abs(fb)))
}

// diffRows returns a description of how got differs from want, or "" when they agree as sets of groups.
func diffRows(got, want []AggRow) string {
	index := func(rows []AggRow) map[string][]string {
		m := make(map[string][]string, len(rows))
		for _, r := range rows {
			m[strings.Join(r.Group, "\x00")] = r.Values
		}
		return m
	}
	g, w := index(got), index(want)
	var out []string
	for k, wv := range w {
		gv, ok := g[k]
		if !ok {
			out = append(out, fmt.Sprintf("group %q missing (want %v)", k, wv))
			continue
		}
		for i := range wv {
			if i >= len(gv) || !sameValue(gv[i], wv[i]) {
				out = append(out, fmt.Sprintf("group %q = %v, want %v", k, gv, wv))
				break
			}
		}
	}
	for k, gv := range g {
		if _, ok := w[k]; !ok {
			out = append(out, fmt.Sprintf("group %q = %v not in reference", k, gv))
		}
	}
	sort.Strings(out)
	if len(out) > 4 {
		out = append(out[:4], fmt.Sprintf("... %d more", len(out)-4))
	}
	return strings.Join(out, "; ")
}

func TestArchiveAggregateMatchesBruteForce(t *testing.T) {
	batches, nRandom := []int{450, 200, 300}, 12
	if os.Getenv("CLASSAD_AGG_DIFF_HEAVY") != "" {
		batches, nRandom = []int{8000, 4000, 2000, 1500}, 400
	}
	h := buildDiffArchive(t, batches)

	all, err := h.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	var ads []*classad.ClassAd
	for ad := range all {
		ads = append(ads, ad)
	}
	if len(ads) != h.Count() {
		t.Fatalf("full scan returned %d of %d records", len(ads), h.Count())
	}

	constraints := []string{
		"true",
		"FooExpr is undefined",
		"FooExpr =!= undefined",
		"FooExpr >= 0",
		"(FooExpr ?: -1) == -1",
		"BarStr is undefined",
		"BarStr == \"bar1\"",
		"MemoryUsage is undefined",
		"(MemoryUsage ?: 0) == 0",
		"CpusUsage is undefined",
		"CpusUsage =?= undefined || CpusUsage == 0",
		"(CpusUsage ?: 0) == 0",
		"RemoteUserCpu == 0",
		"RemoteUserCpu == 0 && RemoteWallClockTime > 0",
		"DiskUsage is undefined && EnteredHistoryTime > 1700010000",
		"LastRemoteHost is undefined",
		"MachineAttrGLIDEIN_Site0 is undefined",
		"HoldReasonCode == 16",
		"ExitCode =?= undefined",
		"Owner == \"user03\" && CpusUsage is undefined",
		"Owner == \"user03\" && FooExpr is undefined",
		"EnteredHistoryTime > 1700010000 && RemoteWallClockTime == 0",
		"EnteredHistoryTime > 1700010000",
		"Owner == \"user03\" && EnteredHistoryTime >= 1700010000",
		"CompletionDate < 0",
		"CompletionDate == -1 && JobStatus == 3",
	}
	fixed := len(constraints)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < nRandom; i++ {
		constraints = append(constraints, diffConstraint(rng, 2))
	}

	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	numAggs := []AggSpec{
		{Func: AggMin, Arg: "FooExpr"}, {Func: AggSum, Arg: "RemoteUserCpu"}, {Func: AggMax, Arg: "CpusUsage"},
	}
	type shape struct {
		groupBy []string
		aggs    []AggSpec
		scan    bool // answered by a record scan: slow, so checked for every constraint only when heavy
	}
	shapes := []shape{
		{nil, count, false},
		{[]string{"Owner"}, count, false},
		{[]string{"JobStatus"}, count, false},
		{[]string{"NumJobStarts"}, count, false},
		{[]string{"MachineAttrGLIDEIN_Site0"}, count, true},
		{[]string{"FooExpr"}, count, false},
		{nil, numAggs[:1], false}, {nil, numAggs[1:2], false}, {nil, numAggs[2:3], false},
		{[]string{"JobStatus"}, numAggs, false},
	}
	// The scan-backed shapes (a string GROUP BY, a listing) are the slow ones, so the random
	// constraints check them only under CLASSAD_AGG_DIFF_HEAVY. The columnar paths -- where every
	// wrong answer so far has come from -- run for every constraint.
	heavy := os.Getenv("CLASSAD_AGG_DIFF_HEAVY") != ""
	for ci, c := range constraints {
		full := ci < fixed || heavy
		t.Run(strconv.Itoa(ci), func(t *testing.T) {
			t.Parallel() // reads only: the archive and the reference ads are shared
			for _, sh := range shapes {
				if sh.scan && !full {
					continue
				}
				want := refAggregate(t, ads, c, sh.groupBy, sh.aggs)
				got, err := h.Aggregate(c, sh.groupBy, sh.aggs)
				if err != nil {
					t.Fatalf("WHERE %s GROUP BY %v: %v", c, sh.groupBy, err)
				}
				if d := diffRows(got, want); d != "" {
					t.Errorf("WHERE %s GROUP BY %v %v: %s", c, sh.groupBy, sh.aggs, d)
				}
			}
			if !full {
				return
			}
			seq, err := h.Query(c)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for range seq {
				n++
			}
			if want := refAggregate(t, ads, c, nil, count)[0].Values[0]; strconv.Itoa(n) != want {
				t.Errorf("Query(%s) returned %d records, want %s", c, n, want)
			}
		})
	}
}
