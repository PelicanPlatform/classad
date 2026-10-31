package collections

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// historyArchiveOptions is a history archive as htcondordb configures one: ClusterId
// value-indexed, Owner categorical, zone maps on the completion times.
func historyArchiveOptions(dir string, segSize int) ArchiveOptions {
	return ArchiveOptions{Dir: dir, SegmentSize: segSize,
		ValueAttrs: []string{"ClusterId"}, CategoricalAttrs: []string{"Owner"},
		ZoneAttrs: []string{"CompletionDate", "EnteredHistoryTime"}}
}

// historyAd is the i-th finished job: cluster 500's procs finish throughout the history
// (one in every 25 records), every other cluster's within a few records. A few records
// carry no ClusterId, or a string one, so the index's exception sets are exercised.
func historyAd(t testing.TB, i int) *classad.ClassAd {
	cluster, proc := 1000+i/40, i%40
	if i%25 == 0 {
		cluster, proc = 500, i/25
	}
	clusterAttr := fmt.Sprintf("ClusterId = %d\n", cluster)
	switch {
	case i%97 == 0:
		clusterAttr = ""
	case i%89 == 0:
		clusterAttr = fmt.Sprintf("ClusterId = \"%d\"\n", cluster)
	}
	text := fmt.Sprintf("%sID = %d\nProcId = %d\nOwner = \"u%d\"\nCompletionDate = %d\nEnteredHistoryTime = %d\n"+
		"Cmd = \"/home/u/run.sh\"\nArgs = \"--seed %d --input /data/set%d\"\nJobStatus = %d",
		clusterAttr, i, proc, i%5, 1700000000+i, 1700000000+i, i, i%13, 3+i%2)
	ad, err := classad.ParseOld(text)
	if err != nil {
		t.Fatalf("parsing %q: %v", text, err)
	}
	return ad
}

func fillHistory(t testing.TB, a *Archive, from, to int) {
	for i := from; i < to; i++ {
		if err := a.Append(historyAd(t, i)); err != nil {
			t.Fatal(err)
		}
	}
}

// ids is the ID of each ad, in order.
func ids(seq func(func(*classad.ClassAd) bool)) []int64 {
	var out []int64
	for ad := range seq {
		id, _ := ad.EvaluateAttrInt("ID")
		out = append(out, id)
	}
	return out
}

// reference is what the constraint selects, newest first, decided by evaluating it on every
// retained ad: neither the index nor the vm's compiled matcher has a say.
func reference(t *testing.T, a *Archive, constraint string) []int64 {
	t.Helper()
	expr, err := classad.ParseExpr(constraint)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := vm.Parse("true")
	var out []int64
	for ad := range a.Query(all) {
		if v := ad.EvaluateExprWithTarget(expr, nil); v.IsBool() {
			if b, _ := v.BoolValue(); b {
				id, _ := ad.EvaluateAttrInt("ID")
				out = append(out, id)
			}
		}
	}
	return out
}

var indexFetchConstraints = []string{
	"ClusterId == 500 && ProcId == 0",
	"ClusterId == 500 && ProcId == 3000",
	"ClusterId == 500",
	"ClusterId == 1003 && ProcId == 7",
	"(ClusterId == 1003 || ClusterId == 500) && ProcId == 1",
	"ClusterId >= 1100 && ClusterId < 1103 && Owner == \"u1\"",
	"ClusterId == 500 && Owner =!= \"u2\" && JobStatus == 4",
	"ClusterId == 500 && Missing =?= undefined && ProcId < 3",
	"ClusterId =?= undefined",
	"ClusterId == \"1002\"",
	"ClusterId == 500 && CompletionDate > 1700060000",
	"ClusterId == 99999",
	"ClusterId > 0 && ProcId == 5",
	"ClusterId == 500 && ClusterId == 501",
	"Owner == \"u3\" && ProcId == 39",
}

// checkFetches compares every constraint's full result and its LIMIT 1 and 5 against the
// reference, through both the decoding and the projected read.
func checkFetches(t *testing.T, a *Archive, stage string) {
	t.Helper()
	for _, c := range indexFetchConstraints {
		want := reference(t, a, c)
		q, err := vm.Parse(c)
		if err != nil {
			t.Fatal(err)
		}
		if got := ids(a.QueryLimit(q, 0)); !slices.Equal(got, want) {
			t.Errorf("%s: %s: %d rows %v, want %d %v", stage, c, len(got), head(got), len(want), head(want))
		}
		for _, limit := range []int{1, 5} {
			if got, w := ids(a.QueryLimit(q, limit)), want[:min(limit, len(want))]; !slices.Equal(got, w) {
				t.Errorf("%s: %s LIMIT %d: %v, want %v", stage, c, limit, got, w)
			}
		}
		var proj []int64
		for raw := range a.QueryRawProjected(q, []string{"ID", "ProcId"}, false, false) {
			lines := make([]string, len(raw.Exprs))
			for i, e := range raw.Exprs {
				lines[i] = string(e)
			}
			ad, err := classad.ParseOld(strings.Join(lines, "\n"))
			if err != nil {
				t.Fatal(err)
			}
			id, _ := ad.EvaluateAttrInt("ID")
			proj = append(proj, id)
		}
		if !slices.Equal(proj, want) {
			t.Errorf("%s: %s projected: %d rows, want %d", stage, c, len(proj), len(want))
		}
	}
}

func head(xs []int64) []int64 { return xs[:min(len(xs), 6)] }

// A row fetch through the index returns exactly what a scan returns, in the same newest-first
// order and under the same LIMIT, at every stage an archive goes through: plain sealed
// segments with sidecars, an unsealed active segment with none, columnarized segments,
// reopened (mmapped sidecars), merged segments, and with the oldest segments rotated away.
func TestArchiveIndexedFetchMatchesScan(t *testing.T) {
	dir := t.TempDir()
	a, err := CreateArchive(historyArchiveOptions(dir, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	fillHistory(t, a, 0, 12000)
	checkFetches(t, a, "sealed+active")

	if !a.BuildAndEnableSchemaScan(2000, 16) {
		t.Fatal("no columnar schema built")
	}
	fillHistory(t, a, 12000, 12300) // a fresh tail: unsealed, unindexed, not columnar
	checkFetches(t, a, "columnar")

	a.Close()
	if a, err = OpenArchive(historyArchiveOptions(dir, 64<<10)); err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	checkFetches(t, a, "reopened")

	a.SetRetention(Retention{MaxSegments: 20})
	if n, err := a.Rotate(0); err != nil || n == 0 {
		t.Fatalf("rotate: %d, %v", n, err)
	}
	checkFetches(t, a, "rotated")

	if n := a.MergePass(MergeOptions{TargetSegments: 8, TriggerSegments: 9}); n == 0 {
		t.Fatal("nothing merged")
	}
	checkFetches(t, a, "merged")
}

// Segments sealed before the attribute was indexed have no postings for it: the fetch walks
// them in full and uses the index everywhere else, with the same result as a scan.
func TestArchiveIndexedFetchUnindexedSegments(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	opts := historyArchiveOptions(dir, 64<<10)
	opts.ValueAttrs = nil
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	fillHistory(t, a, 0, 6000)
	a.Close()
	// Reopened with the index configured, carried back over only the newest segment: the
	// older sealed segments' sidecars predate it.
	opts = historyArchiveOptions(dir, 64<<10)
	opts.IndexBackfillBytes = 64 << 10
	if a, err = OpenArchive(opts); err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	fillHistory(t, a, 6000, 12000)
	checkFetches(t, a, "index added later")

	q, _ := vm.Parse("ClusterId == 500 && ProcId == 1")
	ex := a.ExplainQuery(q)
	if ex.Plan != "indexed" || ex.UnindexedSegments == 0 || ex.IndexSegments == 0 ||
		!strings.Contains(ex.Reason, "segments have no index on it and are scanned in full") {
		t.Errorf("explain: %+v", ex)
	}
	var st ScanStats
	for range a.QueryRawProjectedStats(q, nil, false, false, &st) {
	}
	if st.SegmentsUnindexed != ex.UnindexedSegments || st.SegmentsIndexed == 0 {
		t.Errorf("stats %+v against explain %+v", st, ex)
	}
}

// An old row of a big cluster costs its candidates' column tests, not their reassembly: the
// filter's ProcId is answered from the columns for every candidate the index hands over,
// and only the match is rebuilt. The scan breakdown says which path ran and over what.
func TestArchiveIndexedFetchStats(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(historyArchiveOptions(dir, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	fillHistory(t, a, 0, 12000)
	if !a.BuildAndEnableSchemaScan(2000, 16) {
		t.Fatal("no columnar schema built")
	}
	// Cluster 500's oldest proc with a ClusterId (record 0 has none).
	q, _ := vm.Parse("ClusterId == 500 && ProcId == 1")
	var st ScanStats
	n := 0
	for range a.QueryRawProjectedStats(q, nil, false, false, &st) {
		n++
	}
	if n != 1 {
		t.Fatalf("%d rows", n)
	}
	// The candidates are cluster 500's records and the value index's exception set (the
	// records whose ClusterId is a string, which a numeric posting cannot rule out).
	cluster500 := len(reference(t, a, "ClusterId == 500"))
	exceptions := len(reference(t, a, "isString(ClusterId)"))
	if st.SegmentsIndexed == 0 || st.IndexCandidates < cluster500 || st.IndexCandidates > cluster500+exceptions {
		t.Errorf("index: %d segments, %d candidates; cluster 500 has %d records, %d exceptions: %+v",
			st.SegmentsIndexed, st.IndexCandidates, cluster500, exceptions, st)
	}
	if st.SegmentsUnindexed != 0 {
		t.Errorf("unindexed segments = %d, want 0 (the active segment indexes as it goes): %+v", st.SegmentsUnindexed, st)
	}
	// A candidate is decided from the columns and never rebuilt, unless it is the match or one
	// the columns cannot judge (an exception-set record, or one in the row-form active segment).
	if st.RecordsVisited != st.IndexCandidates || st.RecordsReassembled > exceptions+1 ||
		st.RecordsColumnDecided+st.RecordsReassembled < st.IndexCandidates {
		t.Errorf("column-decided %d, reassembled %d of %d candidates: %+v",
			st.RecordsColumnDecided, st.RecordsReassembled, st.IndexCandidates, st)
	}
	if st.RecordsVisited >= a.Count()/4 {
		t.Errorf("visited %d of %d records", st.RecordsVisited, a.Count())
	}

}

// A cluster lives in one or two segments; with no long-lived cluster widening every
// segment's ClusterId range and every ClusterId a number (no exception set to search), every
// other segment is ruled out before a posting is read -- by its zone map, which the indexed plan
// consults first, or by its index. (With a long-lived cluster in the mix, as in the other tests,
// no segment can be ruled out: each one's range spans it.)
func TestArchiveIndexedFetchSkipsSegments(t *testing.T) {
	t.Parallel()
	a, err := CreateArchive(historyArchiveOptions(t.TempDir(), 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for i := range 12000 {
		ad, err := classad.ParseOld(fmt.Sprintf("ClusterId = %d\nProcId = %d\nID = %d\nOwner = \"u%d\"\n"+
			"CompletionDate = %d\nArgs = \"--seed %d --input /data/set%d\"", 1000+i/40, i%40, i, i%5, 1700000000+i, i, i%13))
		if err != nil {
			t.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	q, _ := vm.Parse("ClusterId == 1003")
	var st ScanStats
	for range a.QueryRawProjectedStats(q, nil, false, false, &st) {
	}
	if want := len(reference(t, a, "ClusterId == 1003")); st.RowsMatched != want || want == 0 {
		t.Fatalf("matched %d, want %d", st.RowsMatched, want)
	}
	if st.SegmentsPruned+st.SegmentsIndexSkipped < st.SegmentsTotal-3 || st.IndexCandidates != st.RowsMatched {
		t.Errorf("small cluster: %+v", st)
	}
}

// EXPLAIN names the path and why: the index probe and its candidates, the scan when the
// index would hand over nearly everything, or the scan when nothing is indexed.
func TestArchiveIndexedFetchExplain(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	a, err := CreateArchive(historyArchiveOptions(dir, 64<<10))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { a.Close() }()
	fillHistory(t, a, 0, 12000)
	explain := func(c string) QueryExplain {
		q, err := vm.Parse(c)
		if err != nil {
			t.Fatal(err)
		}
		return a.ExplainQuery(q)
	}
	ex := explain("ClusterId == 500 && ProcId == 0")
	want := fmt.Sprintf("index probe on ClusterId: %d of %d segments answer from postings, ~%d candidate records",
		ex.IndexSegments, ex.IndexSegments+ex.IndexSkippedSegments, ex.EstCandidates)
	if ex.Plan != "indexed" || !strings.HasPrefix(ex.Reason, want) || ex.IndexSegments == 0 ||
		ex.UnindexedSegments != 0 || ex.EstCandidates < 400 || ex.EstCandidates > 600 {
		t.Errorf("point fetch: %+v", ex)
	}
	// The other clusters are confined to a few segments; the rest are ruled out.
	if ex := explain("ClusterId == 1003"); ex.Plan != "indexed" || ex.IndexSkippedSegments == 0 {
		t.Errorf("narrow cluster: %+v", ex)
	}
	if ex := explain("ClusterId > 0"); ex.Plan != "serial-scan" || !strings.Contains(ex.Reason, "the index on ClusterId would hand the scan about") {
		t.Errorf("unselective: %+v", ex)
	}
	// And the read does what EXPLAIN says: the scan, not the index.
	q, _ := vm.Parse("ClusterId > 0")
	var st ScanStats
	for range a.QueryRawProjectedStats(q, []string{"ID"}, false, false, &st) {
	}
	if st.SegmentsIndexed != 0 || st.IndexCandidates != 0 || st.RecordsVisited != a.Count() {
		t.Errorf("unselective read: %+v", st)
	}
	if ex := explain("ProcId == 0"); ex.Plan != "serial-scan" || !strings.Contains(ex.Reason, "no conjunct of the filter is on an indexed attribute") {
		t.Errorf("unindexed: %+v", ex)
	}
	if ex := explain("ClusterId < 10 && ClusterId > 20"); ex.Plan != "empty" {
		t.Errorf("contradiction: %+v", ex)
	}
}
