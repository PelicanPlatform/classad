package collections

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// openDelta opens a persistent collection with delta records on. Persistent because deltas
// require inline-name records: a fragment has to decode without a shared intern table.
func openDelta(t *testing.T, max int) (*Collection, string) {
	t.Helper()
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16, DeltaMax: max})
	if err != nil {
		t.Fatal(err)
	}
	return c, dir
}

// jobAd builds a wide ad in the shape this optimization exists for: a job with many
// attributes, only a couple of which any one update touches.
func jobAd(cluster int, extra map[string]int64) *classad.ClassAd {
	a := classad.New()
	a.InsertAttr("ClusterId", int64(cluster))
	a.InsertAttr("ProcId", 0)
	a.InsertAttrString("Owner", "alice")
	a.InsertAttr("JobStatus", 2)
	for i := 0; i < 40; i++ {
		a.InsertAttr(fmt.Sprintf("Pad%02d", i), int64(i))
	}
	for k, v := range extra {
		a.InsertAttr(k, v)
	}
	return a
}

func getInt(t *testing.T, c *Collection, key, attr string) int64 {
	t.Helper()
	ad, ok := c.Get([]byte(key))
	if !ok {
		t.Fatalf("%s missing", key)
	}
	v, ok2 := ad.EvaluateAttrInt(attr)
	if !ok2 {
		t.Fatalf("%s[%s] not an integer / missing", key, attr)
	}
	return v
}

// TestDeltaChainReadsCorrectly is the core correctness claim: a key written once in full and
// then updated many times through PutPatch must read back exactly as if every write had
// stored the whole ad. It walks past DeltaMax so the re-materialization boundary is crossed
// several times, and checks the untouched attributes survive every merge -- dropping those is
// the failure a delta store makes, and it is invisible if you only assert the attribute you
// just wrote.
func TestDeltaChainReadsCorrectly(t *testing.T) {
	c, _ := openDelta(t, 4)
	defer c.Close()
	key := []byte("1.0")

	tx := c.Begin()
	tx.Put(key, jobAd(1, map[string]int64{"LastJobLeaseRenewal": 100}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	for i := 1; i <= 20; i++ {
		ad, ok := c.Get(key)
		if !ok {
			t.Fatalf("round %d: key missing", i)
		}
		ad.InsertAttr("LastJobLeaseRenewal", int64(100+i))
		tx := c.Begin()
		tx.PutPatch(key, ad, []string{"LastJobLeaseRenewal"}, false)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
		if got := getInt(t, c, "1.0", "LastJobLeaseRenewal"); got != int64(100+i) {
			t.Fatalf("round %d: LastJobLeaseRenewal = %d, want %d", i, got, 100+i)
		}
		// Attributes no write has touched must still be there and unchanged.
		if got := getInt(t, c, "1.0", "ClusterId"); got != 1 {
			t.Fatalf("round %d: ClusterId = %d, want 1 (untouched attribute lost)", i, got)
		}
		if got := getInt(t, c, "1.0", "Pad37"); got != 37 {
			t.Fatalf("round %d: Pad37 = %d, want 37 (untouched attribute lost)", i, got)
		}
		final, _ := c.Get(key)
		if n := len(final.AST().Attributes); n != 45 {
			t.Fatalf("round %d: ad has %d attributes, want 45", i, n)
		}
	}
	// Without this the test proves only that reads are correct -- which they also are if every
	// write silently stored the whole ad and the optimization did nothing.
	deltas, fulls := c.DeltaStats()
	if deltas == 0 {
		t.Fatalf("no deltas were written (%d full records): the chain is not being used", fulls)
	}
	// 20 updates at DeltaMax 4 should be roughly 4 deltas per re-materialization.
	if deltas < 12 {
		t.Errorf("only %d of 20 updates stored as deltas (%d full): bound looks wrong", deltas, fulls)
	}
	t.Logf("stored %d deltas, %d full records", deltas, fulls)
}

// TestDeltaSurvivesReopen covers the durability of a chain: the deltas and their base are
// separate records, and a reopen rebuilds the directory from disk, so this is where a design
// that only works in RAM falls over.
func TestDeltaSurvivesReopen(t *testing.T) {
	c, dir := openDelta(t, 8)
	key := []byte("2.0")
	tx := c.Begin()
	tx.Put(key, jobAd(2, map[string]int64{"N": 0}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	for i := 1; i <= 5; i++ {
		ad, _ := c.Get(key)
		ad.InsertAttr("N", int64(i))
		tx := c.Begin()
		tx.PutPatch(key, ad, []string{"N"}, false)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	c2, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16, DeltaMax: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	if got := getInt(t, c2, "2.0", "N"); got != 5 {
		t.Fatalf("after reopen N = %d, want 5", got)
	}
	if got := getInt(t, c2, "2.0", "Pad12"); got != 12 {
		t.Fatalf("after reopen Pad12 = %d, want 12", got)
	}
	// And a further delta written after the reopen still resolves.
	ad, _ := c2.Get(key)
	ad.InsertAttr("N", 99)
	tx = c2.Begin()
	tx.PutPatch(key, ad, []string{"N"}, false)
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("post-reopen write conflicted")
	}
	if got := getInt(t, c2, "2.0", "N"); got != 99 {
		t.Fatalf("post-reopen N = %d, want 99", got)
	}
}

// TestDeltaRemovedAttributeForcesFullRecord pins the one thing a delta of present attributes
// cannot express. If a removal were stored as a delta the attribute would reappear on merge.
func TestDeltaRemovedAttributeForcesFullRecord(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("3.0")
	tx := c.Begin()
	tx.Put(key, jobAd(3, map[string]int64{"Doomed": 7}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	ad, _ := c.Get(key)
	if !ad.Delete("Doomed") {
		t.Fatal("precondition: Doomed should exist")
	}
	tx = c.Begin()
	tx.PutPatch(key, ad, []string{"Doomed"}, true) // removed = true
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("delete conflicted")
	}
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("key missing")
	}
	if _, present := got.Lookup("Doomed"); present {
		t.Fatal("removed attribute came back: a removal was stored as a delta")
	}
	if got2 := getInt(t, c, "3.0", "ClusterId"); got2 != 3 {
		t.Fatalf("ClusterId = %d, want 3", got2)
	}
}

// TestDeltaSnapshotIsolation checks that replay respects the reading transaction's snapshot
// rather than merging every delta it can find. A transaction that read the key before later
// updates must still see the state it opened at.
func TestDeltaSnapshotIsolation(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("4.0")
	tx := c.Begin()
	tx.Put(key, jobAd(4, map[string]int64{"N": 0}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	for i := 1; i <= 3; i++ {
		ad, _ := c.Get(key)
		ad.InsertAttr("N", int64(i))
		w := c.Begin()
		w.PutPatch(key, ad, []string{"N"}, false)
		if r := w.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	// Pin a snapshot at N == 3, then advance the key past it.
	reader := c.Begin()
	if ad, ok := reader.Get(key); !ok {
		t.Fatal("reader saw no key")
	} else if v, _ := ad.EvaluateAttrInt("N"); v != 3 {
		t.Fatalf("reader initial N = %d, want 3", v)
	}
	for i := 4; i <= 8; i++ {
		ad, _ := c.Get(key)
		ad.InsertAttr("N", int64(i))
		w := c.Begin()
		w.PutPatch(key, ad, []string{"N"}, false)
		if r := w.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	ad, ok := reader.Get(key)
	if !ok {
		t.Fatal("reader lost the key")
	}
	if v, _ := ad.EvaluateAttrInt("N"); v != 3 {
		t.Fatalf("reader N after later commits = %d, want 3 (replay ignored the snapshot)", v)
	}
	if got := getInt(t, c, "4.0", "N"); got != 8 {
		t.Fatalf("fresh read N = %d, want 8", got)
	}
}

// TestDeltaRemoveAndSetTogether is the case the removed flag actually exists for. When the
// deleted attribute is also the named one, deltaAd cannot find it and falls back to a full
// record on its own -- so a test that only deletes proves nothing about the flag. Deleting X
// while setting Y produces a perfectly encodable delta of Y, and merging it over the base
// brings X back from the dead unless the write is forced to store the whole ad.
func TestDeltaRemoveAndSetTogether(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("5.0")
	tx := c.Begin()
	tx.Put(key, jobAd(5, map[string]int64{"Doomed": 7, "Keeper": 1}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	// Establish a delta chain first, so the write under test is a delta candidate.
	for i := 1; i <= 2; i++ {
		ad, _ := c.Get(key)
		ad.InsertAttr("Keeper", int64(i+1))
		w := c.Begin()
		w.PutPatch(key, ad, []string{"Keeper"}, false)
		if r := w.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	ad, _ := c.Get(key)
	if !ad.Delete("Doomed") {
		t.Fatal("precondition: Doomed should exist")
	}
	ad.InsertAttr("Keeper", 42) // a perfectly encodable delta, alongside a removal
	w := c.Begin()
	w.PutPatch(key, ad, []string{"Keeper"}, true)
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("write conflicted")
	}
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("key missing")
	}
	if _, present := got.Lookup("Doomed"); present {
		t.Fatal("Doomed came back: a write that removed an attribute was stored as a delta")
	}
	if v, _ := got.EvaluateAttrInt("Keeper"); v != 42 {
		t.Fatalf("Keeper = %d, want 42", v)
	}
	if v, _ := got.EvaluateAttrInt("ClusterId"); v != 5 {
		t.Fatalf("ClusterId = %d, want 5", v)
	}
}

// TestDeltaSurvivesCompaction is the hard invariant: compaction reclaims superseded versions,
// and a delta whose base was reclaimed is unreadable. Chains are collapsed before compaction
// runs, so this must read back intact -- and it must do so for MANY keys, because a single-key
// test would pass on a store where compaction simply had nothing to reclaim.
func TestDeltaSurvivesCompaction(t *testing.T) {
	c, _ := openDelta(t, 8)
	defer c.Close()
	const n = 1200
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("c%d.0", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	// Leave every key mid-chain, so compaction runs with chains open.
	for round := 1; round <= 12; round++ {
		for i := 0; i < n; i++ {
			key := []byte(fmt.Sprintf("c%d.0", i))
			ad, ok := c.Get(key)
			if !ok {
				t.Fatalf("round %d: key %d missing", round, i)
			}
			ad.InsertAttr("N", int64(round))
			tx := c.Begin()
			tx.PutPatch(key, ad, []string{"N"}, false)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
	}
	deltas, _ := c.DeltaStats()
	if deltas == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	// Assert compaction actually did something. shouldCompact needs a shard to be at least
	// compactMinBytes with over half of it dead, so a smaller test silently compacts nothing
	// and would "pass" no matter what happens to delta chains.
	if n := c.Compact(); n == 0 {
		t.Fatal("Compact() reclaimed nothing: this test is not exercising compaction at all")
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("c%d.0", i)
		if got := getInt(t, c, key, "N"); got != 12 {
			t.Fatalf("%s N = %d, want 12 after compaction", key, got)
		}
		if got := getInt(t, c, key, "ClusterId"); got != int64(i) {
			t.Fatalf("%s ClusterId = %d, want %d after compaction", key, got, i)
		}
		if got := getInt(t, c, key, "Pad20"); got != 20 {
			t.Fatalf("%s Pad20 = %d, want 20 after compaction", key, got)
		}
	}
}

// TestPatchAttrsNeedsNoRead is the point of the whole exercise: buffering a change must not
// read the stored ad. It is asserted by counting reads at the shard, because "we removed a
// read" is otherwise invisible -- the results are identical either way, and a regression that
// puts the read back would be caught by nothing.
func TestPatchAttrsNeedsNoRead(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("6.0")
	tx := c.Begin()
	tx.Put(key, jobAd(6, map[string]int64{"N": 0}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}

	before := storeReads.Load()
	for i := 1; i <= 10; i++ {
		patch := classad.New()
		patch.InsertAttr("N", int64(i))
		w := c.Begin()
		w.PatchAttrs(key, patch, nil)
		if r := w.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	reads := storeReads.Load() - before
	// One re-materialization is allowed within 10 updates at DeltaMax 16 only if the chain
	// bound were hit, which it is not -- so a correct implementation reads zero times.
	if reads != 0 {
		t.Errorf("10 patch writes performed %d stored reads, want 0", reads)
	}
	if got := getInt(t, c, "6.0", "N"); got != 10 {
		t.Fatalf("N = %d, want 10", got)
	}
	if got := getInt(t, c, "6.0", "Pad31"); got != 31 {
		t.Fatalf("Pad31 = %d, want 31 (untouched attribute lost)", got)
	}
	deltas, fulls := c.DeltaStats()
	t.Logf("%d deltas, %d full records, %d stored reads", deltas, fulls, reads)
}

// TestPatchAttrsReadYourWrites: a transaction that buffers a patch and then reads the key must
// see its own change merged over the stored ad, even though the write itself read nothing.
func TestPatchAttrsReadYourWrites(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("7.0")
	tx := c.Begin()
	tx.Put(key, jobAd(7, map[string]int64{"N": 1}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	w := c.Begin()
	patch := classad.New()
	patch.InsertAttr("N", 55)
	patch.InsertAttrString("Fresh", "yes")
	w.PatchAttrs(key, patch, nil)
	ad, ok := w.Get(key)
	if !ok {
		t.Fatal("read-your-writes: key missing")
	}
	if v, _ := ad.EvaluateAttrInt("N"); v != 55 {
		t.Fatalf("read-your-writes N = %d, want 55", v)
	}
	if v, _ := ad.EvaluateAttrString("Fresh"); v != "yes" {
		t.Fatalf("read-your-writes Fresh = %q, want yes", v)
	}
	if v, _ := ad.EvaluateAttrInt("ClusterId"); v != 7 {
		t.Fatalf("read-your-writes lost the stored ad: ClusterId = %d, want 7", v)
	}
	if !w.Has(key) {
		t.Fatal("Has said no for a key this transaction patched")
	}
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("commit conflicted")
	}
}

// TestDeltaVsColumnarization probes the interaction this design does NOT obviously survive.
//
// Delta chains are collapsed before COMPACTION, but nothing collapses them before a segment
// SEALS -- so deltas do reach sealed segments. Columnarization then rewrites sealed segments on
// the assumption that each record holds a whole ad, moving schema'd attributes out of records
// into a per-segment columnar payload. A fragment passed through that comes out claiming to be
// a whole ad that is missing everything the fragment never held.
//
// It matters because htcondordb turns the schema scan on by DEFAULT for mutable tables
// (SchemaScanHotTopN 32), so production would hit this even though a plain benchmark never does.
// The test asserts the contents are unchanged across columnarization; if that is not yet true it
// is a blocker, and this is the reproduction.
func TestDeltaVsColumnarization(t *testing.T) {
	c, _ := openDelta(t, 8)
	defer c.Close()
	const n = 2000
	want := map[string]int64{}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k%d.0", i)
		tx := c.Begin()
		tx.Put([]byte(key), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
		want[key] = 0
	}
	// Enough delta traffic to fill and seal several segments while chains are open.
	for round := 1; round <= 10; round++ {
		for i := 0; i < n; i++ {
			key := fmt.Sprintf("k%d.0", i)
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(key), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
			want[key] = int64(round)
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	check := func(stage string) {
		t.Helper()
		for key, wantN := range want {
			ad, ok := c.Get([]byte(key))
			if !ok {
				t.Fatalf("%s: %s missing", stage, key)
			}
			if v, _ := ad.EvaluateAttrInt("N"); v != wantN {
				t.Fatalf("%s: %s N = %d, want %d", stage, key, v, wantN)
			}
			if v, _ := ad.EvaluateAttrInt("Pad07"); v != 7 {
				t.Fatalf("%s: %s Pad07 = %d, want 7 (attribute lost)", stage, key, v)
			}
			if got := len(ad.AST().Attributes); got != 45 {
				t.Fatalf("%s: %s has %d attributes, want 45", stage, key, got)
			}
		}
	}
	check("before columnarization")
	if !c.BuildAndEnableSchemaScan(2000, 32) {
		t.Skip("schema scan did not enable; nothing to test")
	}
	// Count the delta records sitting in sealed segments BEFORE the rewrite, so the log says
	// what was actually put through it rather than leaving that to be assumed.
	sealedDeltas := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 || seg == sh.act {
				continue
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if recKeyLen(seg.data, off)&markerFlag == 0 {
					if raw, err := seg.codec.Decompress(nil, recAd(seg.data, off)); err == nil && isDeltaRecord(raw) {
						sealedDeltas++
					}
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	nseg := 0
	for pass := 0; pass < 3; pass++ { // converges over passes (bounded per pass)
		nseg += c.ColumnarizeSealed()
	}
	t.Logf("columnarized %d sealed segments holding %d delta records", nseg, sealedDeltas)
	if nseg == 0 {
		t.Fatal("no sealed segment was columnarized; this test is not exercising the interaction")
	}
	if sealedDeltas == 0 {
		t.Fatal("no delta records were in the columnarized segments; nothing was put through the rewrite")
	}
	check("after columnarization")
}

// TestDeltaScanMatchesPointRead is the blocker this addresses. A scan walks records directly
// rather than resolving a key, so before it learned about deltas it happily returned fragments
// -- rows that look like whole ads and are missing everything the last write did not touch.
// That is worse than an error: reconcile reads the table this way, so a corrupt mirror would
// have been written back as if it were the truth.
//
// The assertion is that both read paths agree, attribute for attribute, which is exactly the
// check that caught the problem on the real log (the scan reported 6,882,447 attributes where
// point reads reported 7,873,144).
func TestDeltaScanMatchesPointRead(t *testing.T) {
	c, _ := openDelta(t, 8)
	defer c.Close()
	const n = 500
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("s%d.0", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	// Leave keys at assorted chain depths, so the scan meets bases and deltas alike.
	for round := 1; round <= 7; round++ {
		for i := round % 3; i < n; i += 2 {
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("s%d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}

	point := map[string]int{}
	pointAttrs := 0
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("s%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s missing from a point read", key)
		}
		point[key] = len(ad.AST().Attributes)
		pointAttrs += point[key]
	}
	seen := map[string]int{}
	scanAttrs, rows := 0, 0
	q, qerr := vm.Parse("true")
	if qerr != nil {
		t.Fatal(qerr)
	}
	for ad := range c.Query(q) {
		k, _ := ad.EvaluateAttrString("Key")
		if k == "" { // no Key attribute in these ads; identify by ClusterId
			v, _ := ad.EvaluateAttrInt("ClusterId")
			k = fmt.Sprintf("s%d.0", v)
		}
		seen[k] = len(ad.AST().Attributes)
		scanAttrs += seen[k]
		rows++
	}
	if rows != n {
		t.Fatalf("scan returned %d rows, want %d", rows, n)
	}
	if scanAttrs != pointAttrs {
		t.Fatalf("scan saw %d attributes, point reads saw %d: the scan is returning fragments",
			scanAttrs, pointAttrs)
	}
	for key, want := range point {
		if got := seen[key]; got != want {
			t.Fatalf("%s: scan gave %d attributes, point read gave %d", key, got, want)
		}
	}
}

// TestDeltaAfterDeleteAndRecreate covers the tracker's other hard requirement. Deleting a key
// removes the full record a chain would be merged from, so if the tracker still claimed one
// existed, the re-created key's next update would be stored as a delta chained to nothing.
func TestDeltaAfterDeleteAndRecreate(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	key := []byte("8.0")
	tx := c.Begin()
	tx.Put(key, jobAd(8, map[string]int64{"N": 1}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	patch := classad.New()
	patch.InsertAttr("N", 2)
	w := c.Begin()
	w.PatchAttrs(key, patch, nil)
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("patch conflicted")
	}
	d := c.Begin()
	d.Delete(key)
	if r := d.Commit(); r.Conflicted() {
		t.Fatal("delete conflicted")
	}
	if _, ok := c.Get(key); ok {
		t.Fatal("key still present after delete")
	}
	// Re-create by patching a key with no stored record at all: the store must notice there is
	// no base and write a whole record.
	p2 := classad.New()
	p2.InsertAttr("N", 3)
	p2.InsertAttr("ClusterId", 8)
	w2 := c.Begin()
	w2.PatchAttrs(key, p2, nil)
	if r := w2.Commit(); r.Conflicted() {
		t.Fatal("recreate conflicted")
	}
	got, ok := c.Get(key)
	if !ok {
		t.Fatal("re-created key unreadable: a delta was chained to a deleted base")
	}
	if v, _ := got.EvaluateAttrInt("N"); v != 3 {
		t.Fatalf("N = %d, want 3", v)
	}
}

// TestDeltaStoreReopenedWithoutDeltaMax is the hazard that the option is not the record.
//
// Delta records are a property of what is ON DISK, but replay was gated on the DeltaMax option
// passed at open. Reopen the same store without it -- a config change, a different tool, a
// rollback -- and every delta reads back as a whole ad consisting of whatever the last write
// touched. Not an error: a short ad, silently. This is the same trap the basecodec file exists
// to prevent for compression, and it needs the same answer.
func TestDeltaStoreReopenedWithoutDeltaMax(t *testing.T) {
	c, dir := openDelta(t, 16)
	key := []byte("9.0")
	tx := c.Begin()
	tx.Put(key, jobAd(9, map[string]int64{"N": 0}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	for i := 1; i <= 4; i++ {
		patch := classad.New()
		patch.InsertAttr("N", int64(i))
		w := c.Begin()
		w.PatchAttrs(key, patch, nil)
		if r := w.Commit(); r.Conflicted() {
			t.Fatalf("round %d conflicted", i)
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// Reopened with delta mode OFF. The records on disk have not changed.
	c2, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	ad, ok := c2.Get(key)
	if !ok {
		t.Fatal("key unreadable after reopening without DeltaMax")
	}
	if n := len(ad.AST().Attributes); n != 45 {
		t.Fatalf("ad has %d attributes, want 45: reopening without DeltaMax served a fragment", n)
	}
	if v, _ := ad.EvaluateAttrInt("N"); v != 4 {
		t.Fatalf("N = %d, want 4", v)
	}
}

// TestNoLiveDeltaOutsideActiveSegment is the invariant the seal-collapse design exists to
// provide, and the one that makes the sealed format safe.
//
// Fragments still physically sit in sealed segments -- collapsing supersedes them, it does not
// erase them, and compaction reclaims them later. What must be true is that none of them is
// LIVE: every key's current record outside the active segment is a whole ad. That is what lets
// columnarization, and anything else that rewrites a sealed segment, keep assuming a live
// record is a complete ad.
//
// Counting all records instead of live ones would pass trivially and prove nothing, which is
// what the first version of this test did.
func TestNoLiveDeltaOutsideActiveSegment(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	const n = 400
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("z%d.0", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	// Give a SUBSET of keys open chains and then leave them alone, while unrelated traffic
	// seals segments around them. Updating every key in a loop instead would leave each key's
	// newest delta in the active segment by construction, so live fragments would never reach
	// a sealed segment and the test would pass without the collapse doing anything -- which is
	// exactly how the first version of this test fooled itself.
	const hot = 40
	for round := 1; round <= 6; round++ {
		for i := 0; i < hot; i++ {
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("z%d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
	}
	// Now bury them: enough unrelated whole-ad traffic to seal many segments.
	for i := n; i < n+600; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("z%d.0", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("filler %d conflicted", i)
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	liveSealedDeltas, deadSealedDeltas, liveActiveDeltas, sealedSegs := 0, 0, 0, 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			active := seg == sh.act
			if !active {
				sealedSegs++
			}
			for off := uint32(0); off < uint32(seg.used); {
				tl := recTotalLen(seg.data, off)
				if tl == 0 || off+tl > uint32(seg.used) {
					break
				}
				if recKeyLen(seg.data, off)&markerFlag == 0 {
					if raw, err := seg.codec.Decompress(nil, recAd(seg.data, off)); err == nil && isDeltaRecord(raw) {
						live := recSuperseded(seg.data, off) == seqMax
						switch {
						case live && active:
							liveActiveDeltas++
						case live:
							liveSealedDeltas++
						default:
							deadSealedDeltas++
						}
					}
				}
				off += tl
			}
		}
		sh.mu.RUnlock()
	}
	t.Logf("%d sealed segments: %d LIVE delta records, %d superseded; active: %d live deltas",
		sealedSegs, liveSealedDeltas, deadSealedDeltas, liveActiveDeltas)
	if sealedSegs == 0 {
		t.Fatal("nothing sealed; the test is not exercising what it claims")
	}
	if liveSealedDeltas != 0 {
		t.Fatalf("%d LIVE delta records in sealed segments: a sealed segment rewrite would "+
			"treat a fragment as a whole ad", liveSealedDeltas)
	}
	// Deliberately NOT asserting live deltas in the active segment: this workload ends with
	// unrelated filler traffic, so by the end every chain has been collapsed and the active
	// segment legitimately holds none. That deltas are used at all is asserted by DeltaStats
	// above; what this test is for is the sealed-segment invariant.
	_ = liveActiveDeltas
	if deadSealedDeltas == 0 {
		t.Log("note: no superseded fragments found; collapse may be materializing more than needed")
	}
}

// TestDeltaSurvivesRetrainDict covers the route into compactShard that had no collapse at all.
//
// Compact() guarded itself; RetrainDict did not, and compactShard's interning re-encode
// rebuilds a record from its AST without carrying the wire flags. A delta that went through it
// came out as a record CLAIMING to be a whole ad, with only the attributes the fragment held --
// corruption written back to disk, which no amount of replay can undo. Reproduced at 40 of 40
// keys truncated from 45 attributes to 5.
func TestDeltaSurvivesRetrainDict(t *testing.T) {
	c, _ := openDelta(t, 16)
	defer c.Close()
	// Enough records, with string content, for the dictionary trainer to accept the corpus --
	// otherwise RetrainDict errors out before it ever reaches the rewrite this test is about.
	const n = 1500
	for i := 0; i < n; i++ {
		ad := jobAd(i, map[string]int64{"N": 0})
		ad.InsertAttrString("Cmd", fmt.Sprintf("/home/user%d/analysis/run_%d.sh", i%50, i))
		ad.InsertAttrString("Args", fmt.Sprintf("--input dataset_%d --seed %d --verbose", i%97, i))
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("t%d.0", i)), ad)
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	// Leave chains open across the whole key set.
	for round := 1; round <= 4; round++ {
		for i := 0; i < n; i++ {
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("t%d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	if _, err := c.RetrainDict(3000); err != nil {
		t.Logf("RetrainDict: %v (may decline a small corpus; the rewrite is what matters)", err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("t%d.0", i)
		ad, ok := c.Get([]byte(key))
		if !ok {
			t.Fatalf("%s vanished after RetrainDict", key)
		}
		if got := len(ad.AST().Attributes); got != 47 {
			t.Fatalf("%s has %d attributes after RetrainDict, want 47: a delta was re-encoded "+
				"as a whole ad", key, got)
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != 4 {
			t.Fatalf("%s N = %d, want 4", key, v)
		}
		if v, _ := ad.EvaluateAttrInt("Pad33"); v != 33 {
			t.Fatalf("%s Pad33 = %d, want 33", key, v)
		}
	}
}

// TestDeltaWatchEmitsWholeAd pins what a watcher receives. A delta write's stored bytes are
// only the attributes that changed; publishing them hands every consumer a fragment that looks
// like a complete ad. A changefeed exporter that replaces its destination document on each
// event would then delete the rest of the record downstream -- data loss outside this process,
// caused by a store that reads back perfectly.
func TestDeltaWatchEmitsWholeAd(t *testing.T) {
	dir := t.TempDir()
	c, err := Open(Options{Dir: dir, Shards: 2, SegmentSize: 1 << 16, DeltaMax: 16, WatchHistory: 256})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	key := []byte("w1.0")
	tx := c.Begin()
	tx.Put(key, jobAd(1, map[string]int64{"N": 0}))
	if r := tx.Commit(); r.Conflicted() {
		t.Fatal("seed conflicted")
	}
	cur, err := c.WatchCursor()
	if err != nil {
		t.Skipf("watch unavailable: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	events, err := c.Watch(ctx, cur)
	if err != nil {
		t.Fatal(err)
	}
	patch := classad.New()
	patch.InsertAttr("N", 7)
	w := c.Begin()
	w.PatchAttrs(key, patch, nil)
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("patch conflicted")
	}
	for ev := range events {
		if ev.Ad != nil {
			t.Logf("event key=%s kind=%v attrs=%d", ev.Key, ev.Kind, len(ev.Ad.AST().Attributes))
		} else {
			t.Logf("event key=%s kind=%v ad=nil", ev.Key, ev.Kind)
		}
		if string(ev.Key) != "w1.0" || ev.Ad == nil {
			continue
		}
		if got := len(ev.Ad.AST().Attributes); got != 45 {
			t.Fatalf("watch event carried %d attributes, want 45: a fragment was published "+
				"as the new value", got)
		}
		if v, _ := ev.Ad.EvaluateAttrInt("N"); v != 7 {
			t.Fatalf("watch event N = %d, want 7", v)
		}
		if v, _ := ev.Ad.EvaluateAttrInt("Pad11"); v != 11 {
			t.Fatalf("watch event Pad11 = %d, want 11 (untouched attribute absent)", v)
		}
		return
	}
	t.Fatal("no watch event for the delta write")
}

// TestDeltaModeMarkerIsLazyAndReversible pins that enabling the option is not the same as
// storing a delta.
//
// The marker records that the store CONTAINS deltas, and once present it forces replay for the
// life of the store. Writing it at open meant setting DeltaMax once -- even on a table that
// then never wrote a single delta -- turned replay on permanently, with no supported way back.
// Written on the first actual delta instead, the option is genuinely reversible.
func TestDeltaModeMarkerIsLazyAndReversible(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, deltaModeFile)

	c, err := Open(Options{Dir: dir, Shards: 2, SegmentSize: 1 << 16, DeltaMax: 16})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker written at open, before any delta was stored")
	}
	// Whole-ad writes only: still no deltas, so still no marker.
	for i := 0; i < 20; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("m%d", i)), jobAd(i, nil))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("put %d conflicted", i)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("marker written for a store that has only whole records")
	}
	// The first delta must put it down.
	patch := classad.New()
	patch.InsertAttr("N", 1)
	w := c.Begin()
	w.PatchAttrs([]byte("m0"), patch, nil)
	if r := w.Commit(); r.Conflicted() {
		t.Fatal("patch conflicted")
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no delta stored; the test is not exercising what it claims")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("marker absent after a delta was stored: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// And the marker keeps replay on when reopened without the option.
	c2, err := Open(Options{Dir: dir, Shards: 2, SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	ad, ok := c2.Get([]byte("m0"))
	if !ok {
		t.Fatal("m0 unreadable after reopening without DeltaMax")
	}
	if n := len(ad.AST().Attributes); n != 45 {
		t.Fatalf("m0 has %d attributes, want 45", n)
	}
}
