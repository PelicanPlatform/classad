package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// autoPromoteFixture builds a persistent collection whose ads carry two attribute bundles that
// travel together (a Ran* bundle and a container bundle), the shape a group schema exists for. It
// deliberately does NOT enable the accelerator: the test drives the maintenance passes itself so it
// can watch the gate-qualified group set mature. Modelled on bundledGroupFixture's clean-sample
// portion, with the real stability gate (GroupStabilityRuns default 3) left in force.
func autoPromoteFixture(t *testing.T, budget int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 16,
		GroupSchemaCount: 4, ColumnarSegmentBudget: budget})
	if err != nil {
		t.Fatal(err)
	}
	put := func(key, text string) {
		ad, err := classad.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(key), ad); err != nil {
			t.Fatal(err)
		}
	}
	base := func(i int) string {
		return fmt.Sprintf(`ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; RequestMemory=%d`,
			i, i%10, i%7, i%6, (i%16)*1024)
	}
	for i := range 3000 {
		switch i % 3 {
		case 0:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; RanStart=%d; RanEnd=%d; RanHost=%q; RanExit=%d ]`,
				base(i), i, i+10, fmt.Sprintf("h%d", i%50), i%3))
		case 1:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s; CtrImage="img%d"; CtrTag="t%d"; CtrDigest="d%d" ]`,
				base(i), i%20, i%5, i%100))
		default:
			put(fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ %s ]`, base(i)))
		}
	}
	c.RetrainDict(0)
	return c
}

// sealedColumnarized returns the sealed, columnarized segments of shard 0 and their current colblk.
func sealedColumnarized(c *Collection) map[*segment]*colSegment {
	out := map[*segment]*colSegment{}
	sh := c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	for _, seg := range sh.segs {
		if seg == nil || seg == sh.act || seg.used == 0 || !seg.columnarized() {
			continue
		}
		out[seg] = seg.colblk.Load()
	}
	return out
}

// TestGroupSchemasAutoPromoteOnRefresh is the core mutation test: with the accelerator already
// enabled, a routine BuildAndEnableSchemaScan refresh (NOT a ReschemaScan, NOT a restart, NOT a
// schema-pointer change) must adopt the gate-qualified group set as the stability history matures.
// Without the fix st.groups is frozen at its (empty) first-enable value forever, so the loop below
// never sees a nonempty set and the test fails.
func TestGroupSchemasAutoPromoteOnRefresh(t *testing.T) {
	c := autoPromoteFixture(t, 0) // default columnar budget
	defer c.Close()

	// Pass 1: checkpoint one derivation, then first-enable. History is length 1 (< the stability
	// gate of 3), so no group qualifies yet and the segments columnarize under an EMPTY group set --
	// which is exactly the frozen state the fix has to thaw.
	c.GroupSchemas(4096, 0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("accelerator not enabled after first enable")
	}
	if len(st.groups) != 0 {
		t.Skipf("groups qualified at first enable (%d) -- fixture too eager for this test", len(st.groups))
	}
	if len(sealedColumnarized(c)) == 0 {
		t.Skip("no sealed segment was columnarized at first enable")
	}

	// Routine refreshes with the accelerator ALREADY enabled, checkpointing one derivation before
	// each as db.Maintain does. Once the history reaches the stability gate the qualified set should
	// be adopted on a refresh -- no restart, no .schema rebuild.
	adopted := 0
	for pass := 0; pass < 6; pass++ {
		c.GroupSchemas(4096, 0)
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("refresh returned false while enabled")
		}
		if got := len(c.schemaScan.Load().groups); got > 0 {
			adopted = got
			break
		}
	}
	if adopted == 0 {
		t.Fatal("group set was never auto-promoted on a routine refresh: it stayed empty as the history matured")
	}
	t.Logf("auto-promoted %d group schemas on a routine refresh", adopted)

	// The base schema pointer must be unchanged: adoption is a group-set change, not a re-schema.
	if c.schemaScan.Load().schema != st.schema {
		t.Fatal("adoption changed the base schema pointer; it must reuse the pinned schema")
	}
}

// TestGroupSchemasAppliedToColumnarizedSegments checks item 2: the adopted groups are actually
// applied to segments that were columnarized BEFORE the group set qualified. Their columnar payload
// must gain the group columns, within a bounded number of passes.
func TestGroupSchemasAppliedToColumnarizedSegments(t *testing.T) {
	c := autoPromoteFixture(t, 0)
	defer c.Close()

	c.GroupSchemas(4096, 0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	if len(c.schemaScan.Load().groups) != 0 {
		t.Skip("groups qualified at first enable -- cannot test the pre-columnarized case")
	}
	before := sealedColumnarized(c)
	if len(before) == 0 {
		t.Skip("no sealed segment was columnarized at first enable")
	}
	for seg, cs := range before {
		if cs != nil && len(cs.groups) != 0 {
			t.Fatalf("segment %d already carries group columns before adoption", seg.id)
		}
	}

	// Drive refreshes until the group set is adopted AND every columnarized segment carries it.
	converged := false
	for pass := 0; pass < 12 && !converged; pass++ {
		c.GroupSchemas(4096, 0)
		c.BuildAndEnableSchemaScan(4096, 8)
		if len(c.schemaScan.Load().groups) == 0 {
			continue
		}
		converged = true
		for _, cs := range sealedColumnarized(c) {
			if cs == nil || len(cs.groups) == 0 {
				converged = false
				break
			}
		}
	}
	if !converged {
		t.Fatal("adopted groups were not applied to previously-columnarized segments within the pass budget")
	}

	// The reassembled ads must still be correct after the re-columnarize -- a group's members are
	// removed from the records, so a broken rewrite loses them silently.
	n := 0
	for i := 0; i < 3000; i += 111 {
		ad, ok := c.Get([]byte(fmt.Sprintf("%d.0", i)))
		if !ok || ad == nil {
			t.Fatalf("get %d.0: ad=%v ok=%v", i, ad, ok)
		}
		if i%3 == 0 {
			if v, ok := ad.EvaluateAttrInt("RanStart"); !ok || v != int64(i) {
				t.Fatalf("%d.0 RanStart=%d ok=%v, want %d", i, v, ok, i)
			}
			if v, ok := ad.EvaluateAttrInt("RanExit"); !ok || v != int64(i%3) {
				t.Fatalf("%d.0 RanExit=%d ok=%v, want %d", i, v, ok, i%3)
			}
		}
		if v, ok := ad.EvaluateAttrInt("ClusterId"); !ok || v != int64(i) {
			t.Fatalf("%d.0 ClusterId=%d ok=%v, want %d", i, v, ok, i)
		}
		n++
	}
	t.Logf("verified %d reassembled ads after group re-columnarize", n)
}

// TestGroupSchemasRefreshNoChurn checks item 3's guardrail: once the group set is stable and
// committed, a refresh does NOT re-columnarize or republish. Convergence first, then one more
// refresh must leave every columnarized segment's payload (its colblk pointer) and the global
// columnarize count untouched.
func TestGroupSchemasRefreshNoChurn(t *testing.T) {
	c := autoPromoteFixture(t, 0)
	defer c.Close()

	c.GroupSchemas(4096, 0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable")
	}
	// Converge: adopt the groups and re-columnarize every stale segment.
	converged := false
	for pass := 0; pass < 12 && !converged; pass++ {
		c.GroupSchemas(4096, 0)
		c.BuildAndEnableSchemaScan(4096, 8)
		if len(c.schemaScan.Load().groups) == 0 {
			continue
		}
		converged = true
		for _, cs := range sealedColumnarized(c) {
			if cs == nil || len(cs.groups) == 0 {
				converged = false
				break
			}
		}
	}
	if !converged {
		t.Skip("group set never converged; nothing to test for churn")
	}

	stBefore := c.schemaScan.Load()
	blkBefore := sealedColumnarized(c)
	colsBefore, _ := ColumnarizedSegments()
	buildsBefore := colSegmentBuilds.Load()

	// One more steady-state refresh. The group set is unchanged, so this must do no rewrite work.
	c.GroupSchemas(4096, 0)
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("steady refresh returned false")
	}

	if stBefore != c.schemaScan.Load() {
		t.Error("steady refresh republished the schema-scan state though the group set was unchanged")
	}
	colsAfter, _ := ColumnarizedSegments()
	if colsAfter != colsBefore {
		t.Errorf("steady refresh re-columnarized %d segment(s); want 0", colsAfter-colsBefore)
	}
	if got := colSegmentBuilds.Load(); got != buildsBefore {
		t.Errorf("steady refresh built %d sidecar block(s); want 0", got-buildsBefore)
	}
	blkAfter := sealedColumnarized(c)
	if len(blkAfter) != len(blkBefore) {
		t.Fatalf("segment count changed across a steady refresh: %d -> %d", len(blkBefore), len(blkAfter))
	}
	for seg, cs := range blkBefore {
		if blkAfter[seg] != cs {
			t.Errorf("segment %d colblk pointer changed across a steady refresh (re-columnarized needlessly)", seg.id)
		}
	}
}

// TestGroupSchemasAutoPromoteInMemory runs the adoption path on an in-memory store. In memory there
// is no directory and so no derivation history to mature, so groupSchemasFor returns nil and the set
// stays empty -- the refresh must simply behave: no panic, no churn, no spurious adoption. This is
// the "always test both stores" companion to the persistent tests.
func TestGroupSchemasAutoPromoteInMemory(t *testing.T) {
	c, err := Open(Options{Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for i := range 3000 {
		ad, err := classad.Parse(fmt.Sprintf(`[ ClusterId=%d; ProcId=%d; RanStart=%d; RanEnd=%d ]`, i, i%10, i, i+10))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(4096, 8) {
		t.Skip("schema scan did not enable in memory")
	}
	if got := len(c.schemaScan.Load().groups); got != 0 {
		t.Fatalf("in-memory store adopted %d groups with no durable history", got)
	}
	// Several routine refreshes must remain no-ops for the group set.
	for pass := 0; pass < 4; pass++ {
		c.GroupSchemas(4096, 0) // no-op checkpoint (no dir), exercises the same call shape
		if !c.BuildAndEnableSchemaScan(4096, 8) {
			t.Fatal("in-memory refresh returned false")
		}
		if got := len(c.schemaScan.Load().groups); got != 0 {
			t.Fatalf("in-memory refresh pass %d adopted %d groups; want 0", pass, got)
		}
	}
}
