package collections

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The pushdown gate (maxPushdownFrac) declines an index walk that would hand the scan
// nearly every record. These tests are about its INPUT: a union of many highly selective
// disjuncts that the estimate priced as though it were unselective, so the gate -- working
// exactly as designed -- threw away a walk that was still several times faster than the
// scan. Each one asserts the true matched count beside the estimate, so the gap between
// them is a number rather than a claim.

// pushdownTupleUnion builds a tuple-membership constraint: one disjunct per key tuple, the
// shape a "give me these N jobs" query compiles to. keyOf supplies each disjunct's ClusterId
// so a caller can make the union selective (distinct keys) or not (a handful of keys).
func pushdownTupleUnion(n int, keyOf func(i int) int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteString(" || ")
		}
		fmt.Fprintf(&b, "(ClusterId == %d && ProcId == %d)", keyOf(i), i%4)
	}
	return b.String()
}

// pushdownAd is one record of the fixtures below: two indexed value attributes and enough
// padding that a segment holds tens of records rather than thousands, which is what puts
// many segment indexes under one query.
func pushdownAd(cluster, proc int) *classad.ClassAd {
	ad := classad.New()
	ad.InsertAttr("ClusterId", int64(cluster))
	ad.InsertAttr("ProcId", int64(proc))
	ad.InsertAttrString("Owner", fmt.Sprintf("user%d", cluster%7))
	for j := 0; j < 10; j++ {
		ad.InsertAttr(fmt.Sprintf("Pad%02d", j), int64(j))
	}
	return ad
}

// pushdownFixture fills a fresh collection with n records whose ClusterId comes from
// clusterOf, and returns it with the number of segment indexes the query planner will sum
// over. That count is the precondition every test here needs: the over-estimate was a
// per-segment assumption made once per segment, so a fixture of one segment could not have
// exhibited it and a test built on one proves nothing.
func pushdownFixture(t *testing.T, n int, clusterOf func(i int) int) (*Collection, int) {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13,
		ValueAttrs: []string{"ClusterId", "ProcId"}, CategoricalAttrs: []string{"Owner"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%05d", i)), pushdownAd(clusterOf(i), i%4)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()
	return c, pushdownIndexedSegments(c)
}

func pushdownIndexedSegments(c *Collection) int {
	segs := 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			if w.seg.readIdx() != nil {
				segs++
			}
		}
		releaseWindows(wins)
	}
	return segs
}

// pushdownPlan reports the plan the read takes for expr, the planner's estimated candidate
// fraction, and how many records really match.
func pushdownPlan(t *testing.T, c *Collection, expr string) (plan string, frac float64, truth int, ex QueryExplain) {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	groups, prunable := c.planIndexGroups(q.ProbePlan())
	if !prunable {
		t.Fatalf("the union is not index-prunable at all, so it never reaches the pushdown gate; "+
			"this fixture cannot exercise what the test is about (expr begins %.60s)", expr)
	}
	var est bool
	frac, est = c.planFrac(groups)
	if !est {
		t.Fatal("the union has no candidate estimate, so the gate does not apply; the fixture's indexes are not built")
	}
	ex = c.ExplainQuery(q)
	return ex.Plan, frac, len(scanMatches(t, c, q)), ex
}

// TestPushdownEstimateCountsOnlySegmentsThatCanHoldTheValue is the per-segment half of the
// bug, isolated: one equality on a key that lives in a single segment, priced against a
// collection of many segment indexes.
func TestPushdownEstimateCountsOnlySegmentsThatCanHoldTheValue(t *testing.T) {
	t.Parallel()
	const n = 8192
	c, segs := pushdownFixture(t, n, func(i int) int { return i / 4 }) // 4 procs per cluster, ascending
	if segs < 50 {
		t.Fatalf("the fixture built %d segment indexes; with so few, summing a per-segment "+
			"estimate over all of them could not inflate anything and the test proves nothing", segs)
	}
	q, err := vm.Parse(`ClusterId == 17`)
	if err != nil {
		t.Fatal(err)
	}
	truth := len(scanMatches(t, c, q))
	if truth == 0 {
		t.Fatal("no record has ClusterId == 17; the fixture is not what the test assumes")
	}
	up := c.planIndex(q.Probes())
	if len(up) != 1 {
		t.Fatalf("`ClusterId == 17` planned to %d usable probes, want 1", len(up))
	}
	cand, covered := c.estimateCandidates(up[0])
	if !covered {
		t.Fatal("no segment index covers ClusterId; the estimate is not being exercised")
	}
	// The ceiling that matters: one average posting per segment index, which is what the
	// estimate came to when every segment was assumed to hold the key.
	if cand > 4*float64(truth) {
		t.Errorf("`ClusterId == 17` estimated at %.0f candidates over %d matching records in %d segments; "+
			"the estimate is counting segments whose [min,max] excludes the key", cand, truth, segs)
	}
	t.Logf("ClusterId == 17: estimated %.0f, true %d, over %d segment indexes", cand, truth, segs)
}

// TestPushdownGateTakesIndexForSelectiveUnion is the reported case: a tuple-membership
// constraint of 126 disjuncts over 8192 records, matching 126 of them. The index walk is
// the right plan by a wide margin and the gate must not decline it.
func TestPushdownGateTakesIndexForSelectiveUnion(t *testing.T) {
	t.Parallel()
	const n = 8192
	c, segs := pushdownFixture(t, n, func(i int) int { return i / 4 })
	if segs < 50 {
		t.Fatalf("only %d segment indexes; the fixture cannot exhibit a per-segment over-count", segs)
	}
	for _, nd := range []int{8, 32, 64, 112, 126, 128, 256} {
		expr := pushdownTupleUnion(nd, func(i int) int { return i })
		plan, frac, truth, _ := pushdownPlan(t, c, expr)
		trueFrac := float64(truth) / float64(n)
		if trueFrac > 0.1 {
			t.Fatalf("%d disjuncts match %d of %d records (%.1f%%): this arm is not a SELECTIVE "+
				"union and cannot show the gate declining one", nd, truth, n, 100*trueFrac)
		}
		if plan != "indexed-union" {
			t.Errorf("%d disjuncts matching %d of %d records (%.2f%%) planned as %q at an estimated %.4f "+
				"of the collection; the index union is the cheaper plan here",
				nd, truth, n, 100*trueFrac, plan, frac)
		}
		if frac > 10*trueFrac+0.01 {
			t.Errorf("%d disjuncts: estimated %.4f of the collection against a true %.4f, over 10x out",
				nd, frac, trueFrac)
		}
		t.Logf("%3d disjuncts: plan=%-13s estimated %.4f, true %.4f (%d records)", nd, plan, frac, trueFrac, truth)
	}
}

// TestPushdownGateStillScansUnselectiveUnion is the other direction, and the one a fix that
// merely pushes everything down would fail: the same 126-disjunct SHAPE over only eight
// distinct keys, so the union really does cover the whole collection. The gate exists for
// this and must still fire.
func TestPushdownGateStillScansUnselectiveUnion(t *testing.T) {
	t.Parallel()
	const n = 8192
	c, segs := pushdownFixture(t, n, func(i int) int { return i % 8 })
	if segs < 50 {
		t.Fatalf("only %d segment indexes; this fixture is not comparable to the selective one", segs)
	}
	expr := pushdownTupleUnion(126, func(i int) int { return i % 8 })
	plan, frac, truth, ex := pushdownPlan(t, c, expr)
	if truth != n {
		t.Fatalf("the union matches %d of %d records; it is not the unselective union this test needs, "+
			"so a scan verdict would not be evidence of anything", truth, n)
	}
	if !strings.HasSuffix(plan, "-scan") {
		t.Errorf("a union matching every one of %d records planned as %q at an estimated %.4f; "+
			"the index walk would visit the whole collection and the gate should have declined it",
			n, plan, frac)
	}
	if !strings.Contains(ex.Reason, "stops paying") {
		t.Errorf("the scan verdict is not attributed to the pushdown gate: %q", ex.Reason)
	}
	t.Logf("126 unselective disjuncts: plan=%s estimated %.4f, true %.4f", plan, frac, float64(truth)/float64(n))
}

// TestPushdownEstimateSkewedKey guards the direction a better estimate is most likely to
// get wrong: uniform synthetic keys make every estimator look good, and an estimate that
// has learned to discount a key per segment must not discount a key that really is in every
// segment. Here one cluster holds 90% of the collection and the rest are a long tail.
func TestPushdownEstimateSkewedKey(t *testing.T) {
	t.Parallel()
	const n = 8192
	c, segs := pushdownFixture(t, n, func(i int) int {
		if i%10 != 0 {
			return 0 // the dominant key: present in every segment
		}
		return 1 + i/10 // the tail: one record each, ascending
	})
	if segs < 50 {
		t.Fatalf("only %d segment indexes", segs)
	}
	q, err := vm.Parse(`ClusterId == 0`)
	if err != nil {
		t.Fatal(err)
	}
	truth := len(scanMatches(t, c, q))
	if truth < n/2 {
		t.Fatalf("the dominant key covers %d of %d records; the fixture is not skewed", truth, n)
	}
	up := c.planIndex(q.Probes())
	cand, covered := c.estimateCandidates(up[0])
	if !covered {
		t.Fatal("no segment index covers ClusterId")
	}
	// Both directions: it must not be deflated (the key IS everywhere) and must not run
	// away above the records that carry it.
	if cand < 0.5*float64(truth) || cand > 1.5*float64(truth) {
		t.Errorf("the dominant key `ClusterId == 0` estimated at %.0f against %d matching records", cand, truth)
	}
	t.Logf("dominant key: estimated %.0f, true %d", cand, truth)

	// A union over the long tail is selective and must take the index, on the same skewed
	// collection that holds the dominant key.
	plan, frac, tailTruth, _ := pushdownPlan(t, c, pushdownTupleUnion(126, func(i int) int { return 1 + i*5 }))
	if tailTruth == 0 {
		t.Fatal("the tail union matches nothing; it cannot show a pushdown being taken")
	}
	if float64(tailTruth)/float64(n) > 0.1 {
		t.Fatalf("the tail union matches %d of %d records and is not selective", tailTruth, n)
	}
	if plan != "indexed-union" {
		t.Errorf("a tail union matching %d of %d records planned as %q at an estimated %.4f", tailTruth, n, plan, frac)
	}
	t.Logf("skewed tail union: plan=%s estimated %.4f, true %.4f", plan, frac, float64(tailTruth)/float64(n))
}

// pushdownRows answers q through the real read executor with the plan forced, bypassing the
// gate's choice: forceUnion runs the index union the gate may have declined, otherwise the
// plain scan. Both go through runQuery, so neither is a reimplementation of the read.
func pushdownRows(t *testing.T, c *Collection, q *vm.Query, forceUnion bool) []string {
	t.Helper()
	c.catchUpIndexes()
	qp := c.newQueryPlan(q, nil, nil, false)
	var rp readPlan
	if forceUnion {
		plan := q.ProbePlan()
		groups, prunable := c.planIndexGroups(plan)
		if !prunable {
			t.Fatal("the constraint has no index union to force")
		}
		rp = readPlan{plan: plan, groups: groups}
		if !rp.disjunctive() {
			t.Fatal("the forced plan is not disjunctive; the differential is not comparing the two paths")
		}
	}
	var out []string
	c.runQuery(qp, rp, c.yieldAdAs(func(ad *classad.ClassAd) bool {
		out = append(out, adSig(ad))
		return true
	}, false))
	sort.Strings(out)
	return out
}

// TestPushdownGateNeverChangesRows: the gate is a cost decision. Whichever way it goes, the
// same constraint must answer with the same records -- so the estimate behind it can be
// changed freely without anyone having to re-argue correctness.
func TestPushdownGateNeverChangesRows(t *testing.T) {
	t.Parallel()
	const n = 8192
	cases := []struct {
		name      string
		clusterOf func(i int) int
		expr      string
	}{
		{"selective", func(i int) int { return i / 4 }, pushdownTupleUnion(126, func(i int) int { return i })},
		{"unselective", func(i int) int { return i % 8 }, pushdownTupleUnion(126, func(i int) int { return i % 8 })},
		{"skewed", func(i int) int {
			if i%10 != 0 {
				return 0
			}
			return 1 + i/10
		}, pushdownTupleUnion(126, func(i int) int { return 1 + i*5 })},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := pushdownFixture(t, n, tc.clusterOf)
			q, err := vm.Parse(tc.expr)
			if err != nil {
				t.Fatal(err)
			}
			union := pushdownRows(t, c, q, true)
			scan := pushdownRows(t, c, q, false)
			if len(scan) == 0 {
				t.Fatal("the constraint matches nothing; two empty answers agree for the wrong reason")
			}
			if len(union) != len(scan) {
				t.Fatalf("index union returned %d records, the scan %d", len(union), len(scan))
			}
			for i := range scan {
				if union[i] != scan[i] {
					t.Fatalf("record %d differs between the two plans:\n union %s\n  scan %s", i, union[i], scan[i])
				}
			}
			t.Logf("%s: %d records, identical either way", tc.name, len(scan))
		})
	}
}

// TestPushdownEstimateSealedTier runs the same question against the sealed mmap sidecar
// tier. The estimate is shared code, but the two tiers have diverged before, so the
// assertion is made where the sidecars actually answer it: an archive, whose sealed
// segments carry mmapSegIndex rather than the in-RAM segIndex.
func TestPushdownEstimateSealedTier(t *testing.T) {
	t.Parallel()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 13,
		ValueAttrs: []string{"ClusterId", "ProcId"}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	const n = 8192
	for i := 0; i < n; i++ {
		if aerr := a.Append(pushdownAd(i/4, i%4)); aerr != nil {
			t.Fatal(aerr)
		}
	}
	sealed, indexed := sealedIndexed(a)
	if sealed < 50 || indexed != sealed {
		t.Fatalf("the archive has %d sealed segments of which %d carry a sidecar; the sealed tier "+
			"is not what is being measured here", sealed, indexed)
	}
	c := a.c
	// The sidecars really are the mmap tier, not in-RAM indexes standing in for them.
	mmapped := 0
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			if _, ok := w.seg.readIdx().(*mmapSegIndex); ok {
				mmapped++
			}
		}
		releaseWindows(wins)
	}
	if mmapped < 50 {
		t.Fatalf("only %d segments answer from an mmap sidecar; this test is measuring the in-RAM tier", mmapped)
	}
	q, err := vm.Parse(`ClusterId == 17`)
	if err != nil {
		t.Fatal(err)
	}
	up := c.planIndex(q.Probes())
	cand, covered := c.estimateCandidates(up[0])
	if !covered {
		t.Fatal("no sidecar covers ClusterId")
	}
	truth := 0
	for range a.Query(q) {
		truth++
	}
	if truth == 0 {
		t.Fatal("no record has ClusterId == 17")
	}
	if cand > 4*float64(truth) {
		t.Errorf("sealed tier: `ClusterId == 17` estimated at %.0f over %d matching records in %d sidecars",
			cand, truth, mmapped)
	}
	t.Logf("sealed tier: estimated %.0f, true %d, over %d mmap sidecars", cand, truth, mmapped)

	// And the union decision the estimate feeds, on the sealed tier.
	expr := pushdownTupleUnion(126, func(i int) int { return i })
	uq, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	groups, prunable := c.planIndexGroups(uq.ProbePlan())
	if !prunable {
		t.Fatal("the union is not index-prunable on the archive")
	}
	frac, est := c.planFrac(groups)
	if !est {
		t.Fatal("the union has no estimate on the archive")
	}
	ex := c.ExplainQuery(uq)
	utruth := 0
	for range a.Query(uq) {
		utruth++
	}
	if float64(utruth)/float64(n) > 0.1 {
		t.Fatalf("the union matches %d of %d archived records and is not selective", utruth, n)
	}
	if ex.Plan != "indexed-union" {
		t.Errorf("sealed tier: 126 disjuncts matching %d of %d records planned as %q at an estimated %.4f",
			utruth, n, ex.Plan, frac)
	}
	t.Logf("sealed tier union: plan=%s estimated %.4f, true %.4f", ex.Plan, frac, float64(utruth)/float64(n))
}
