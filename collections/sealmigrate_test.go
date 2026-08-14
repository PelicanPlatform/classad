package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// A store written before sealing was turned on keeps its private attributes in the clear: sealing
// happens at encode time, so it protects new writes and silently leaves the existing ones. Nothing
// reports that -- reads work, queries work -- which is why the migration exists and why its test starts
// by proving the plaintext is really there.

// legacyPlaintextStore writes a store with NO data key, so its ClaimId values land as plain literals,
// then returns the directory. That is exactly what an upgrade finds on disk.
func legacyPlaintextStore(t *testing.T, dir, secret string, n int) {
	t.Helper()
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16}) // no DataKey: nothing seals
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)),
			mustAd(t, fmt.Sprintf(`[Owner="alice"; Cpus=%d; ClaimId=%q]`, i, secret))); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) == 0 {
		t.Fatal("the legacy fixture has no plaintext on disk, so the migration would have nothing to do " +
			"and this test would prove nothing")
	}
}

func TestMigrateSealedAttrsRewritesLegacyPlaintext(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	const n = 2000
	legacyPlaintextStore(t, dir, secret, n)

	// Reopen WITH a key, as an upgraded binary does.
	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey})
	if err != nil {
		t.Fatal(err)
	}
	migrated := c.MigrateSealedAttrs(0)
	if migrated == 0 {
		t.Fatal("migrated no segments, but the fixture has plaintext private attributes on disk")
	}
	// Every ad must still read back with its value: a migration that loses data is worse than one that
	// never ran.
	q, err := vm.Parse("Cpus >= 0")
	if err != nil {
		t.Fatal(err)
	}
	rows, withValue := 0, 0
	for ad := range c.Query(q) {
		rows++
		if v, err := ad.EvaluateAttr("ClaimId").StringValue(); err == nil && v == secret {
			withValue++
		}
	}
	if rows != n || withValue != n {
		t.Errorf("after migration: %d ads, %d carrying ClaimId; want %d of each", rows, withValue, n)
	}
	c.Close()

	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("plaintext survived the migration, in: %v", hits)
	}
	t.Logf("migrated %d segments; %d ads intact, no plaintext left", migrated, rows)
}

func TestMigrateSealedAttrsIsIdempotentAndCheapOnACleanStore(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	legacyPlaintextStore(t, dir, secret, 500)
	_, dataKey := deriveDataKey(t)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if first := c.MigrateSealedAttrs(0); first == 0 {
		t.Fatal("first pass migrated nothing")
	}
	// A second pass must find nothing: the candidate test is the data itself, so anything else would
	// mean it cannot tell a sealed store from an unsealed one -- and it would rewrite forever.
	if second := c.MigrateSealedAttrs(0); second != 0 {
		t.Errorf("second pass migrated %d segments; the pass is not idempotent", second)
	}
	if _, err := os.Stat(filepath.Join(dir, sealMigrationMarker)); err != nil {
		t.Errorf("no %s after a clean pass: every open would rescan every segment: %v",
			sealMigrationMarker, err)
	}
}

// TestMigrateSealedAttrsResumesAfterInterruption covers the crash case: the marker is written only after
// a clean pass, so an interrupted run leaves it absent and the next open finishes the job. Deleting the
// marker simulates that, and must not depend on which segments were already done.
func TestMigrateSealedAttrsResumesAfterInterruption(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	legacyPlaintextStore(t, dir, secret, 1000)
	_, dataKey := deriveDataKey(t)

	// First open migrates and marks.
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey})
	if err != nil {
		t.Fatal(err)
	}
	c.MigrateSealedAttrs(1)
	c.Close()

	// Simulate a crash before the marker landed: remove it and run again. Nothing is left to do, so the
	// pass must be a no-op rather than a rewrite of everything.
	if err := os.Remove(filepath.Join(dir, sealMigrationMarker)); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if again := c2.MigrateSealedAttrs(1); again != 0 {
		t.Errorf("resumed pass rewrote %d segments that were already migrated; the candidate test must "+
			"be the data, not the marker", again)
	}
	if hits := diskBytesContaining(t, dir, secret); len(hits) != 0 {
		t.Errorf("plaintext present after resume, in: %v", hits)
	}
}

// TestMigrateSealedAttrsNoopWithoutAKey pins that this cannot run where it would destroy data: with no
// sealer there is nothing to seal WITH, and rewriting would just churn the store.
func TestMigrateSealedAttrsNoopWithoutAKey(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	legacyPlaintextStore(t, dir, secret, 200)
	c, err := Open(Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16}) // still no key
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if n := c.MigrateSealedAttrs(0); n != 0 {
		t.Errorf("migrated %d segments with no sealer configured", n)
	}
}

// TestMigrateSealedAttrsKeepsLookupsWorking is the test the first version of this pass needed and did not
// have. It verified reads with a QUERY, which walks segments directly and cannot see the defect at all;
// every LOOKUP path -- Get, Txn.Get -- goes through the directory and the bucket chain, and those name
// (segment, offset). A reseal re-encodes each record and sizes its segment exactly, so those offsets move,
// and the old ones may be past the end of the new file.
//
// Untreated, that read as two different faults, neither of them "the migration broke lookups":
//
//	Get missed 1998 of 2000 keys                                      (a mismatch ends the chain)
//	panic: slice bounds out of range [:67269717] with capacity 148816  (an offset past the mapping)
//
// The second is what took down a daemon in production. Both are covered here, plus the state a restart
// inherits, because the on-disk data was never wrong -- only the structures pointing into it.
func TestMigrateSealedAttrsKeepsLookupsWorking(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	const secret = "ClaimId-legacy-plaintext-capability"
	const n = 2000
	legacyPlaintextStore(t, dir, secret, n)
	_, dataKey := deriveDataKey(t)

	// A DELTA, because CorruptChainLinks is process-wide: another test that deliberately corrupts a link
	// would otherwise be read as this store's fault.
	badLinksBefore := CorruptChainLinks()
	opts := Options{Shards: 2, Dir: dir, SegmentSize: 1 << 16, DataKey: dataKey}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	if m := c.MigrateSealedAttrs(0); m == 0 {
		t.Fatal("migrated no segments, so this proves nothing about a migrated store")
	}
	assertEveryKeyReadable(t, c, n, "in the process that migrated")

	// A write after the migration has to land and be readable: the pass retires each shard's active
	// segment, so the next write allocates a fresh one.
	if err := c.Put([]byte("k-new"), mustAd(t, `[Owner="bob"; Cpus=1; ClaimId="new-capability"]`)); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Get([]byte("k-new")); !ok {
		t.Error("a key written after the migration is not readable")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// And after a restart: a key sidecar left describing the pre-migration layout would reintroduce this
	// on a later start, which is the version an operator would report as intermittent.
	c2, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	assertEveryKeyReadable(t, c2, n, "after a restart")
	if _, ok := c2.Get([]byte("k-new")); !ok {
		t.Error("after a restart: the post-migration write is unreachable")
	}
	if got := CorruptChainLinks() - badLinksBefore; got != 0 {
		t.Errorf("chain walks hit %d unusable links on a healthy migrated store; the offsets should be "+
			"reconciled, not merely survived", got)
	}
}

// assertEveryKeyReadable checks both lookup paths for every key. Get and Txn.Get share findVisible but
// reach it differently, and the production panic came through the Txn path.
func assertEveryKeyReadable(t *testing.T, c *Collection, n int, when string) {
	t.Helper()
	missing := 0
	for i := 0; i < n; i++ {
		if _, ok := c.Get([]byte(fmt.Sprintf("k%d", i))); !ok {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%s: Get missed %d of %d keys", when, missing, n)
	}
	tx := c.Begin()
	missing = 0
	for i := 0; i < n; i++ {
		ad, ok := tx.Get([]byte(fmt.Sprintf("k%d", i)))
		if !ok || ad == nil {
			missing++
		}
	}
	if missing > 0 {
		t.Errorf("%s: Txn.Get missed %d of %d keys", when, missing, n)
	}
}
