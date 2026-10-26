package collections

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// MigrateSealedAttrs retires the active segment so it gets migrated too. It did that with no
// collapse anywhere in the pass, which is worse than the same omission elsewhere: resealSegmentsAs
// REFUSES a segment holding a delta, so the segment is not migrated either -- the pass reports
// success having both stranded the fragment and left behind the legacy bytes it exists to remove.
//
// A shard whose active segment still holds a live delta must be skipped, not retired.
func TestMigrateWillNotRetireASegmentHoldingALiveDelta(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	const n = 200
	// The real upgrade shape: written WITHOUT a key, so there is legacy plaintext to migrate.
	// Opened with one from the start there is nothing to reseal, the pass migrates zero segments,
	// and the test exercises none of the code under it -- which is what the first version did.
	pre, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		ad := mustAd(t, fmt.Sprintf(`[ClusterId=%d; JobStatus=1; ClaimId=%q]`, i, secret))
		tx := pre.Begin()
		tx.Put([]byte(fmt.Sprintf("%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatal("seed conflicted")
		}
	}
	if err := pre.Close(); err != nil {
		t.Fatal(err)
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) == 0 {
		t.Fatal("no plaintext on disk: the migration would have nothing to do")
	}

	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Live deltas in the active segments, with the post-commit collapse suppressed -- the state a
	// write landing mid-pass leaves behind.
	if !c.collapsing.CompareAndSwap(false, true) {
		t.Fatal("a collapse is already in flight")
	}
	for i := range n {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(9))
		tx := c.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i)), p, nil)
		tx.Commit()
	}
	c.collapsing.Store(false)

	dirty := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		if segHasLiveDelta(sh.act) {
			dirty++
		}
		sh.mu.RUnlock()
	}
	if dirty == 0 {
		t.Fatal("no shard has a live delta in its active segment: the state under test was not created")
	}

	// Not asserting a non-zero migration count: resealSegmentsAs refuses any segment holding a
	// delta, so a delta-mode store migrates nothing. What matters here is that the pass does not
	// STRAND anything while retiring active segments on its way to that conclusion.
	c.MigrateSealedAttrs(1)

	if got := c.checkSealCollapseInvariant(50); len(got) != 0 {
		t.Errorf("the migration stranded %d fragment(s), e.g. %q", len(got), got[0])
	}
	for i := range n {
		key := fmt.Sprintf("%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s is unreadable after the migration", key)
		}
		if v, ok := ad.EvaluateAttrInt("JobStatus"); !ok || v != 9 {
			t.Fatalf("%s JobStatus = %d (present %v), want 9: its patch was lost", key, v, ok)
		}
		if v, ok := ad.EvaluateAttrString("ClaimId"); !ok || v != secret {
			t.Fatalf("%s lost ClaimId (%q, present %v)", key, v, ok)
		}
	}
}

// A pass that skips a shard has not migrated it, so it must not write the marker: the pass is one-shot
// per store, and a marker written over a skipped shard left that shard's plaintext in place for good.
func TestMigrateSkippingAShardLeavesTheMarkerUnwritten(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-skipped-shard"
	pre, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 200 {
		if err = pre.Put([]byte(fmt.Sprintf("%d.0", i)),
			mustAd(t, fmt.Sprintf(`[ClusterId=%d; JobStatus=1; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	if err = pre.Close(); err != nil {
		t.Fatal(err)
	}

	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// A commit landing after the pass's collapse leaves a live delta, with the post-commit collapse
	// held off so it stays live through the shard loop.
	sealMigrateHook = func(stage string) {
		if stage != "collapsed" {
			return
		}
		c.collapsing.Store(true)
		p := classad.New()
		p.InsertAttr("JobStatus", int64(2))
		tx := c.Begin()
		tx.PatchAttrs([]byte("0.0"), p, nil)
		tx.Commit()
	}
	skippedBefore := MigrateSkippedLiveDelta()
	c.MigrateSealedAttrs(1)
	sealMigrateHook = nil
	c.collapsing.Store(false)
	if MigrateSkippedLiveDelta() == skippedBefore {
		t.Fatal("the shard was not skipped, so this proves nothing about a skipped one")
	}
	if c.sealMigrationDone() {
		t.Error("the marker was written by a pass that skipped the store's only shard")
	}

	// The next pass finishes the job and only then marks it.
	if m := c.MigrateSealedAttrs(1); m == 0 {
		t.Error("the follow-up pass migrated nothing")
	}
	if !c.sealMigrationDone() {
		t.Error("no marker after a pass that completed")
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("plaintext left after the follow-up pass, in: %v", hits)
	}
	if ad, ok := c.Get([]byte("0.0")); !ok {
		t.Error("0.0 unreadable after the migration")
	} else if v, _ := ad.EvaluateAttrInt("JobStatus"); v != 2 {
		t.Errorf("0.0 JobStatus = %d, want the patched 2", v)
	}
}

// legacyDeltaStore writes n plaintext keys with delta records on and returns the store reopened with a
// key, as an upgrade finds it.
func legacyDeltaStore(t *testing.T, dir, secret string, n int) *Collection {
	t.Helper()
	pre, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err = pre.Put([]byte(fmt.Sprintf("%d.0", i)),
			mustAd(t, fmt.Sprintf(`[ClusterId=%d; JobStatus=1; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	if err = pre.Close(); err != nil {
		t.Fatal(err)
	}
	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// patchUntilSealed applies patch to key as a delta, then writes filler until the segment holding that
// delta is no longer the active one. The caller holds the post-commit collapse off, so the delta stays
// live in a SEALED segment -- the state the seal-collapse normally removes.
func patchUntilSealed(t *testing.T, c *Collection, key, attr string, v int64) {
	t.Helper()
	p := classad.New()
	p.InsertAttr(attr, v)
	tx := c.Begin()
	tx.PatchAttrs([]byte(key), p, nil)
	tx.Commit()
	sh := c.shards[0]
	sh.mu.RLock()
	act := sh.act
	sh.mu.RUnlock()
	for i := 0; ; i++ {
		if err := c.Put([]byte(fmt.Sprintf("filler-%s-%s-%d", key, attr, i)),
			mustAd(t, `[Filler = "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"]`)); err != nil {
			t.Fatal(err)
		}
		sh.mu.RLock()
		rolled := sh.act != act
		sh.mu.RUnlock()
		if rolled {
			return
		}
	}
}

// A reseal drops superseded delta records, which is safe only when nothing still reads them -- when
// every key's current version is a whole record. A live delta in a SEALED segment still needs the
// superseded fragments before it, wherever they sit, so finding one anywhere in a shard has to keep the
// pass off the whole shard. Checking only the active segment let the pass reseal the segment holding the
// chain's earlier fragment, drop it, and lose that fragment's change.
func TestMigrateKeepsDeltaChainsWithALiveDeltaInASealedSegment(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	c := legacyDeltaStore(t, dir, "ClaimId-legacy-plaintext-chain", 200)
	defer c.Close()
	sealMigrateHook = func(stage string) {
		if stage != "collapsed" {
			return
		}
		c.collapsing.Store(true)
		// 7.0's chain: its whole record, then a delta in one sealed segment, superseded by a live
		// delta in the next.
		patchUntilSealed(t, c, "7.0", "JobStatus", 2)
		patchUntilSealed(t, c, "7.0", "RemoteHost", 3)
	}
	c.MigrateSealedAttrs(1)
	sealMigrateHook = nil
	c.collapsing.Store(false)

	ad, ok := c.Get([]byte("7.0"))
	if !ok {
		t.Fatal("7.0 unreadable after the migration")
	}
	if v, _ := ad.EvaluateAttrInt("JobStatus"); v != 2 {
		t.Errorf("7.0 JobStatus = %d, want 2: the chain's first delta was dropped", v)
	}
	if v, _ := ad.EvaluateAttrInt("RemoteHost"); v != 3 {
		t.Errorf("7.0 RemoteHost = %d, want 3", v)
	}
	if c.sealMigrationDone() {
		t.Error("marked done over a shard it could not migrate")
	}
}

// The rewrite runs off the shard lock, so a commit can supersede a record after it was copied. The copy
// has to be stamped too at the swap, or the old version comes back as current.
func TestMigrateCarriesASupersedeThatLandsDuringTheRewrite(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const n = 200
	c := legacyDeltaStore(t, dir, "ClaimId-legacy-plaintext-race", n)
	defer c.Close()
	sealMigrateHook = func(stage string) {
		if stage != "resealed" {
			return
		}
		if err := c.Put([]byte("3.0"), mustAd(t, `[ClusterId=3; JobStatus=4; ClaimId="new"]`)); err != nil {
			t.Error(err)
		}
	}
	if m := c.MigrateSealedAttrs(1); m == 0 {
		t.Fatal("migrated nothing, so no copy existed to be superseded")
	}
	sealMigrateHook = nil

	ad, ok := c.Get([]byte("3.0"))
	if !ok {
		t.Fatal("3.0 unreadable after the migration")
	}
	if v, _ := ad.EvaluateAttrInt("JobStatus"); v != 4 {
		t.Errorf("3.0 JobStatus = %d, want 4 from the write during the rewrite", v)
	}
	q, err := vm.Parse("ClusterId == 3")
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	for range c.Query(q) {
		rows++
	}
	if rows != 1 {
		t.Errorf("3.0 matched %d rows; the version it replaced came back as current", rows)
	}
}

// A collapse leaves a segment that held only deltas holding only SUPERSEDED deltas, which the reseal
// drops -- so its rewrite has no record to write. That has to rebuild the segment empty rather than fail:
// failing left it unmigrated, so the pass never completed and never wrote its marker.
func TestMigrateRewritesASegmentOfOnlySupersededDeltas(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-all-deltas"
	const n = 200
	opts := Options{Shards: 1, Dir: dir, SegmentSize: 1 << 16, DeltaMax: 1 << 20}
	pre, err := Open(opts) // no key: the deltas below carry their ClaimId in the clear
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		if err = pre.Put([]byte(fmt.Sprintf("%d.0", i)),
			mustAd(t, fmt.Sprintf(`[ClusterId=%d; JobStatus=1; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	sh := pre.shards[0]
	activeSeg := func() *segment {
		sh.mu.RLock()
		defer sh.mu.RUnlock()
		return sh.act
	}
	// Patch until the active segment rolls twice. The record that rolls a segment is the first in the
	// next one, so the segment between the two rolls holds nothing but deltas -- with the collapse that
	// follows a seal held off, since it writes whole records into the segment it runs beside.
	pre.collapsing.Store(true)
	patch := func(i int) {
		p := classad.New()
		p.InsertAttr("JobStatus", int64(10+i))
		p.InsertAttrString("ClaimId", fmt.Sprintf("%s-%d", secret, i))
		tx := pre.Begin()
		tx.PatchAttrs([]byte(fmt.Sprintf("%d.0", i%n)), p, nil)
		tx.Commit()
	}
	i := 0
	for start := activeSeg(); activeSeg() == start; i++ {
		patch(i)
	}
	for deltaSeg := activeSeg(); activeSeg() == deltaSeg; i++ {
		patch(i)
	}
	if err = pre.Close(); err != nil {
		t.Fatal(err)
	}

	_, dataKey := deriveDataKey(t)
	opts.DataKey = dataKey
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	before := resealDroppedDeltas.Load()
	c.MigrateSealedAttrs(1)
	if resealDroppedDeltas.Load() == before {
		t.Fatal("the pass dropped no superseded delta, so the state under test was not created")
	}
	if !c.sealMigrationDone() {
		t.Error("the pass did not complete: a segment of superseded deltas was left unmigrated")
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("plaintext left after the pass, in: %v", hits)
	}
	for i := range n {
		if _, ok := c.Get([]byte(fmt.Sprintf("%d.0", i))); !ok {
			t.Fatalf("%d.0 unreadable after the migration", i)
		}
	}
}
