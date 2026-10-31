package dbrpc

import (
	"context"
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
	"github.com/PelicanPlatform/classad/db"
)

// statsCatPair is statsPair over a catalog, so a test can reach archive tables.
func statsCatPair(t *testing.T, opts ServeOptions) (*Client, *Server, func()) {
	t.Helper()
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat)
	s.EnableQueryStats(QueryStatsOptions{})
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, opts) }()
	c := NewClient(cconn)
	return c, s, func() { c.Close(); s.Close(); cat.Close() }
}

// shapeFor returns the one tracked shape whose grouping matches, or fails.
func shapeFor(t *testing.T, rep QueryStatsReport, groupBy string) QueryShapeStat {
	t.Helper()
	for _, sh := range rep.Shapes {
		if sh.GroupBy == groupBy {
			return sh
		}
	}
	t.Fatalf("no shape grouped by %q; tracked: %s", groupBy, shapeSummary(rep))
	return QueryShapeStat{}
}

func shapeSummary(rep QueryStatsReport) string {
	out := ""
	for _, sh := range rep.Shapes {
		out += fmt.Sprintf("\n  op=%s table=%s where=%s group=[%s] aggs=[%s] count=%d rows=%d",
			sh.Op, sh.Table, sh.Shape, sh.GroupBy, sh.Aggregates, sh.Count, sh.Rows)
	}
	if out == "" {
		return "(none)"
	}
	return out
}

// TestQueryStatsRecordsAggregates is the regression this file exists for: a GROUP BY was
// served by opcodes that never reached the query log at all, so every aggregate a
// dashboard ran -- the status counts, the goodput rollup, the metrics histograms -- was
// absent from the slow-query report rather than merely summarised oddly. An operator
// reading the report saw the reads and concluded the aggregates were cheap.
func TestQueryStatsRecordsAggregates(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 30) // 3 distinct Owners

	rows, err := c.Aggregate(ctx, "true", []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatalf("Aggregate: %v", err)
	}
	if len(rows) != 3 {
		t.Fatalf("GROUP BY Owner produced %d groups, want 3", len(rows))
	}

	rep := waitRecorded(t, s, 1)
	if len(rep.Shapes) != 1 {
		t.Fatalf("got %d shapes, want 1: %s", len(rep.Shapes), shapeSummary(rep))
	}
	sh := rep.Shapes[0]
	if sh.Op != "Aggregate" {
		t.Errorf("Op = %q, want %q", sh.Op, "Aggregate")
	}
	if sh.GroupBy != "owner" {
		t.Errorf("GroupBy = %q, want %q", sh.GroupBy, "owner")
	}
	if sh.Aggregates != "count(*)" {
		t.Errorf("Aggregates = %q, want %q", sh.Aggregates, "count(*)")
	}
	// Rows counts GROUPS for an aggregate -- three owners, not thirty ads. The report
	// prints it next to the duration, so it must not read as "this query returned the
	// whole table".
	if sh.Rows != 3 {
		t.Errorf("Rows = %d, want 3 (one per group)", sh.Rows)
	}
	if sh.Count != 1 {
		t.Errorf("Count = %d, want 1", sh.Count)
	}
	if sh.Total <= 0 {
		t.Errorf("Total = %v, want a measured duration", sh.Total)
	}
}

// TestQueryStatsSeparatesGroupings is the other half of recording them: the grouping is
// part of the shape key, so aggregates that differ only in HOW they group stay apart.
// They are the same constraint and wildly different work -- which was the argument for
// bucketing by constraint shape, and is the argument against bucketing by it alone.
func TestQueryStatsSeparatesGroupings(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 30)

	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, err := c.Aggregate(ctx, "true", []string{"Owner"}, count); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Aggregate(ctx, "true", []string{"JobStatus"}, count); err != nil {
		t.Fatal(err)
	}
	// The same attribute bucketed two ways: hourly and daily differ by 24x in group
	// cardinality, and a report that merged them would name one and rank it by the sum.
	for _, width := range []int64{3600, 86400} {
		if _, err := c.AggregateBucketed(ctx, "true",
			[]GroupCol{{Attr: "CompletionDate", BucketWidth: width}}, count); err != nil {
			t.Fatal(err)
		}
	}
	// Same grouping as the first, different aggregate function: SUM reads an attribute
	// from every matched row where COUNT(*) need not.
	if _, err := c.Aggregate(ctx, "true", []string{"Owner"},
		[]AggSpec{{Func: AggSum, Arg: "CompletionDate"}}); err != nil {
		t.Fatal(err)
	}

	rep := waitRecorded(t, s, 5)
	if len(rep.Shapes) != 5 {
		t.Fatalf("got %d shapes, want 5: %s", len(rep.Shapes), shapeSummary(rep))
	}
	for _, want := range []struct{ group, aggs string }{
		{"owner", "count(*)"},
		{"jobstatus", "count(*)"},
		{"completiondate/3600", "count(*)"},
		{"completiondate/86400", "count(*)"},
		{"owner", "sum(completiondate)"},
	} {
		found := false
		for _, sh := range rep.Shapes {
			if sh.GroupBy == want.group && sh.Aggregates == want.aggs {
				found = true
				if sh.Count != 1 {
					t.Errorf("shape %s/%s: Count = %d, want 1", want.group, want.aggs, sh.Count)
				}
			}
		}
		if !found {
			t.Errorf("no shape grouped by %q aggregating %q: %s", want.group, want.aggs, shapeSummary(rep))
		}
	}
}

// TestQueryStatsRecordsArchiveAggregate covers the history side, where the expensive
// aggregates actually run, and pins two things the mutable test cannot: an archive
// aggregate is named for the engine that answered rather than for the opcode the client
// sent (so reaching the same history table by name or through the archive opcodes is ONE
// shape), and the scan breakdown is collected while aggregation is on, so the report can
// say which tier answered.
func TestQueryStatsRecordsArchiveAggregate(t *testing.T) {
	c, s, cleanup := statsCatPair(t, ServeOptions{Privileged: true})
	defer cleanup()
	ctx := context.Background()

	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{
		ValueAttrs: []string{"ClusterId"},
		ZoneAttrs:  []string{"ClusterId"},
	}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	owners := []string{"alice", "bob", "alice", "carol", "bob", "alice"}
	for i, o := range owners {
		if err := c.ArchiveAppend(ctx, "history",
			fmt.Sprintf("ClusterId = %d\nOwner = %q", i, o)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}

	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	if _, err := c.ArchiveAggregate(ctx, "history", "true", []string{"Owner"}, count); err != nil {
		t.Fatalf("ArchiveAggregate: %v", err)
	}
	// The same aggregate addressed as a table: the mutable handler resolves the name to
	// the archive and reduces it with the same ladder, so it must land in the same bucket.
	if _, err := c.AggregateTable(ctx, "history", "true", []string{"Owner"}, count); err != nil {
		t.Fatalf("AggregateTable on an archive: %v", err)
	}

	rep := waitRecorded(t, s, 2)
	if len(rep.Shapes) != 1 {
		t.Fatalf("got %d shapes, want 1 (both reach the same archive): %s", len(rep.Shapes), shapeSummary(rep))
	}
	sh := rep.Shapes[0]
	if sh.Op != "ArchiveAggregate" {
		t.Errorf("Op = %q, want %q", sh.Op, "ArchiveAggregate")
	}
	if sh.Table != "history" || sh.GroupBy != "owner" || sh.Aggregates != "count(*)" {
		t.Errorf("shape = %s/%s/%s, want history/owner/count(*)", sh.Table, sh.GroupBy, sh.Aggregates)
	}
	if sh.Count != 2 {
		t.Errorf("Count = %d, want 2", sh.Count)
	}
	if sh.Rows != 6 {
		t.Errorf("Rows = %d, want 6 (3 groups x 2 queries)", sh.Rows)
	}
	if len(sh.Exemplars) == 0 {
		t.Fatal("no exemplars kept")
	}
	// The tier is what the stats threading buys: it says the aggregate was answered from
	// metadata or the columns rather than by rebuilding every record, which is the
	// difference this report exists to show.
	if sh.Exemplars[0].Scan.Tier == collections.TierUnreported {
		t.Error("exemplar reports no aggregate tier: the scan stats were not collected")
	}
}

// TestQueryStatsRecordsKeyAndTxnReads covers the other reads that ran unlogged. Neither
// returns ads, which is why both looked cheap enough to leave out; both evaluate the
// constraint over the same rows a Query would.
func TestQueryStatsRecordsKeyAndTxnReads(t *testing.T) {
	c, s, cleanup := statsPair(t, ServeOptions{Privileged: true}, QueryStatsOptions{})
	defer cleanup()
	ctx := context.Background()
	seedJobs(t, ctx, c, 30)

	var keys []string
	if err := c.QueryKeys(ctx, `Owner == "ua"`, func(key string) bool {
		keys = append(keys, key)
		return true
	}); err != nil {
		t.Fatalf("QueryKeys: %v", err)
	}
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Query(ctx, `Owner == "ub"`, 0); err != nil {
		t.Fatalf("Tx.Query: %v", err)
	}
	if _, err := tx.KeysWhere(ctx, `Owner == "uc"`); err != nil {
		t.Fatalf("Tx.KeysWhere: %v", err)
	}
	if err := tx.Abort(ctx); err != nil {
		t.Fatal(err)
	}

	rep := waitRecorded(t, s, 3)
	byOp := map[string]QueryShapeStat{}
	for _, sh := range rep.Shapes {
		byOp[sh.Op] = sh
	}
	for _, op := range []string{"QueryKeys", "TxnQuery", "TxnQueryKeys"} {
		sh, ok := byOp[op]
		if !ok {
			t.Errorf("no shape for op %q: %s", op, shapeSummary(rep))
			continue
		}
		if sh.Count != 1 {
			t.Errorf("op %q: Count = %d, want 1", op, sh.Count)
		}
	}
	if sh := byOp["QueryKeys"]; sh.Rows != int64(len(keys)) {
		t.Errorf("QueryKeys Rows = %d, want %d (the keys it streamed)", sh.Rows, len(keys))
	}
}

// TestGroupAndAggShape fixes the rendering the report prints and keys on.
func TestGroupAndAggShape(t *testing.T) {
	cases := []struct {
		name   string
		groups []GroupCol
		aggs   []AggSpec
		group  string
		agg    string
	}{
		{
			name:  "plain read carries neither",
			group: "", agg: "",
		},
		{
			name:   "names fold to lower case, as the constraint shape does",
			groups: []GroupCol{{Attr: "Owner"}},
			aggs:   []AggSpec{{Func: AggCount, Arg: "*"}},
			group:  "owner", agg: "count(*)",
		},
		{
			name:   "a bucket width is kept: it is a plan parameter, not a tested value",
			groups: []GroupCol{{Attr: "QDate", BucketWidth: 3600}},
			aggs:   []AggSpec{{Func: AggSum, Arg: "RemoteWallClockTime"}},
			group:  "qdate/3600", agg: "sum(remotewallclocktime)",
		},
		{
			name:   "column order is preserved: it is the result's row shape",
			groups: []GroupCol{{Attr: "Owner"}, {Attr: "JobStatus"}},
			aggs:   []AggSpec{{Func: AggMin, Arg: "QDate"}, {Func: AggMax, Arg: "QDate"}},
			group:  "owner, jobstatus", agg: "min(qdate), max(qdate)",
		},
		{
			name:   "a per-aggregate FILTER is fingerprinted like a constraint",
			groups: []GroupCol{{Attr: "Owner"}},
			aggs:   []AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 5"}},
			group:  "owner", agg: "count(*) filter (jobstatus == ?)",
		},
		{
			name:  "an aggregate over the whole match has no grouping",
			aggs:  []AggSpec{{Func: AggCountDistinct, Arg: "Owner"}},
			group: "", agg: "count distinct(owner)",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := groupShape(c.groups); got != c.group {
				t.Errorf("groupShape = %q, want %q", got, c.group)
			}
			if got := aggShape(c.aggs); got != c.agg {
				t.Errorf("aggShape = %q, want %q", got, c.agg)
			}
		})
	}
}

// TestAggShapeMergesFilterLiterals is the filter's half of the merging contract: two
// aggregates whose FILTER differs only in its literals are one shape, exactly as two
// constraints differing only in theirs are.
func TestAggShapeMergesFilterLiterals(t *testing.T) {
	a := aggShape([]AggSpec{{Func: AggCount, Arg: "*", Filter: "EnteredHistoryTime >= 1700000000"}})
	b := aggShape([]AggSpec{{Func: AggCount, Arg: "*", Filter: "EnteredHistoryTime >= 1799999999"}})
	if a != b {
		t.Errorf("filters differing only in a literal rendered apart:\n  %q\n  %q", a, b)
	}
	if c := aggShape([]AggSpec{{Func: AggCount, Arg: "*", Filter: "JobStatus == 5"}}); c == a {
		t.Errorf("different filters rendered alike: %q", c)
	}
}

// TestAggFuncNameUnknown pins the unknown-selector rendering: a function added to the
// wire after this was written must not be reported as one of the known names.
func TestAggFuncNameUnknown(t *testing.T) {
	if got := aggFuncName(AggFunc(200)); got != "agg200" {
		t.Errorf("aggFuncName(200) = %q, want %q", got, "agg200")
	}
}

// TestArchiveAggregateWithStatsOff runs the same aggregates with NO query aggregation
// enabled, which is the configuration every other test here hides: the handler then
// passes a nil ScanStats down the archive ladder, where before it passed none at all.
// Every assertion in this file would still pass if that nil panicked in production.
func TestArchiveAggregateWithStatsOff(t *testing.T) {
	cat, err := db.OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := NewServerCatalog(cat) // deliberately NOT EnableQueryStats
	cconn, sconn := netPipe()
	go func() { _ = s.ServeConnOpts(sconn, ServeOptions{Privileged: true}) }()
	c := NewClient(cconn)
	defer func() { c.Close(); s.Close(); cat.Close() }()

	ctx := context.Background()
	if err := c.CreateArchiveTable(ctx, "history", db.ArchiveConfig{ZoneAttrs: []string{"ClusterId"}}); err != nil {
		t.Fatalf("CreateArchiveTable: %v", err)
	}
	for i, o := range []string{"alice", "bob", "alice"} {
		if err := c.ArchiveAppend(ctx, "history", fmt.Sprintf("ClusterId = %d\nOwner = %q", i, o)); err != nil {
			t.Fatalf("ArchiveAppend: %v", err)
		}
	}
	count := []AggSpec{{Func: AggCount, Arg: "*"}}
	// Both doors to the archive ladder: the archive opcode, and the table-name one that
	// now hands it the stats block.
	rows, err := c.AggregateTable(ctx, "history", "true", []string{"Owner"}, count)
	if err != nil {
		t.Fatalf("AggregateTable on an archive with stats off: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("GROUP BY Owner produced %d groups, want 2", len(rows))
	}
	if rows, err = c.ArchiveAggregate(ctx, "history", "true", []string{"Owner"}, count); err != nil {
		t.Fatalf("ArchiveAggregate with stats off: %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("GROUP BY Owner produced %d groups, want 2", len(rows))
	}
}

// TestQueryLogGroupingText covers the renderers a front end uses for its own per-query
// log line, so one aggregate reads the same there as in the shape report.
func TestQueryLogGroupingText(t *testing.T) {
	q := QueryLog{
		Op: "ArchiveAggregate", Table: "history",
		Groups: []GroupCol{{Attr: "Owner"}, {Attr: "EnteredHistoryTime", BucketWidth: 3600}},
		Aggs:   []AggSpec{{Func: AggCount, Arg: "*"}},
	}
	if got, want := q.GroupingText(), "owner, enteredhistorytime/3600"; got != want {
		t.Errorf("GroupingText = %q, want %q", got, want)
	}
	if got, want := q.AggregatesText(), "count(*)"; got != want {
		t.Errorf("AggregatesText = %q, want %q", got, want)
	}
	var plain QueryLog
	if plain.GroupingText() != "" || plain.AggregatesText() != "" {
		t.Errorf("a plain read rendered a grouping: %q / %q", plain.GroupingText(), plain.AggregatesText())
	}
}
