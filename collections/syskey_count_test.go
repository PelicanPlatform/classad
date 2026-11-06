package collections

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// mustParseAd parses an ad literal or fails the test.
func mustParseAd(t *testing.T, s string) *classad.ClassAd {
	t.Helper()
	ad, err := classad.Parse(s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return ad
}

// scanLen counts the ads a client-facing scan yields -- the number Len claims to report.
func scanLen(c *Collection) int {
	n := 0
	for range c.Scan() {
		n++
	}
	return n
}

// sysLen counts the live system-keyed records, which no client-facing path yields.
func sysLen(c *Collection) int {
	n := 0
	c.ForEachSystemAd(func(string, *classad.ClassAd) bool { n++; return true })
	return n
}

// checkLen asserts Len against what a scan yields and StoredLen against the two keyspaces
// together, so a counter that drifts in either direction is caught where it drifted.
func checkLen(t *testing.T, c *Collection, stage string) {
	t.Helper()
	visible, sys := scanLen(c), sysLen(c)
	if got := c.Len(); got != visible {
		t.Errorf("%s: Len = %d, scan yields %d ads (%d system records live)", stage, got, visible, sys)
	}
	if got := c.StoredLen(); got != visible+sys {
		t.Errorf("%s: StoredLen = %d, want %d (%d visible + %d system)", stage, got, visible+sys, visible, sys)
	}
}

// Len counts the rows a reader can see, not the keys the store holds.
//
// The reserved system keyspace (syskey.go) is stored, superseded, compacted and recovered
// exactly like data, and only the client-facing enumeration paths hide it -- so the live count
// is maintained in two parts and every path that maintains one must maintain the other. This
// drives a randomized history through all of them: inserts and overwrites of both keyspaces,
// deletes, compaction, a clean close and reopen from the directory snapshot, and a reopen that
// walks the records instead. Len is checked against a scan at each stage, because a count that
// disagrees with the listing is wrong whichever path produced it.
func TestLenExcludesSystemRecordsAcrossRestarts(t *testing.T) {
	configs := map[string]func(dir string) Options{
		"mutable": func(dir string) Options {
			return Options{Dir: dir, Shards: 2, SegmentSize: 1 << 14, GroupSchemaCount: -1}
		},
		"append-only": func(dir string) Options {
			return Options{Dir: dir, Shards: 2, SegmentSize: 1 << 13, AppendOnly: true, GroupSchemaCount: -1}
		},
	}
	for name, mk := range configs {
		for seed := int64(1); seed <= 4; seed++ {
			t.Run(fmt.Sprintf("%s/seed%d", name, seed), func(t *testing.T) {
				dir := t.TempDir()
				opts := mk(dir)
				c, err := Open(opts)
				if err != nil {
					t.Fatal(err)
				}
				rng := rand.New(rand.NewSource(seed))
				ad := func(i int) *classad.ClassAd {
					return mustParseAd(t, fmt.Sprintf(`[ Seq=%d; State="Idle" ]`, i))
				}
				for i := range 400 {
					// Roughly one record in six is system-keyed, which is a far heavier
					// mix than a batch-per-commit marker makes -- the point is to move the
					// two counters independently, not to be representative.
					var key string
					if rng.Intn(6) == 0 {
						key = SystemKey(fmt.Sprintf("idem:batch-%d", rng.Intn(40)))
					} else {
						key = fmt.Sprintf("%d.0", rng.Intn(300))
					}
					if err := c.Put([]byte(key), ad(i)); err != nil {
						t.Fatal(err)
					}
					switch rng.Intn(25) {
					case 0:
						c.Delete([]byte(key)) // may be either keyspace, may already be gone
					case 1:
						c.Compact()
					}
				}
				checkLen(t, c, "live")
				if sysLen(c) == 0 {
					t.Fatal("fixture wrote no live system records; the test proves nothing")
				}

				c.Compact()
				checkLen(t, c, "after compaction")
				wantVisible, wantSys := scanLen(c), sysLen(c)
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}

				// Restored from the directory snapshot, which carries the system count.
				c, restored, rebuilt := openCountingSnapshots(t, opts)
				if restored != len(c.shards) || rebuilt != 0 {
					c.Close()
					t.Fatalf("clean reopen: %d shards restored, %d rebuilt; want all restored", restored, rebuilt)
				}
				checkLen(t, c, "reopened from the snapshot")
				if got := c.Len(); got != wantVisible {
					t.Errorf("reopened from the snapshot: Len = %d, want %d as before the close", got, wantVisible)
				}
				if got := sysLen(c); got != wantSys {
					t.Errorf("reopened from the snapshot: %d system records, want %d as before the close", got, wantSys)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}

				// Rebuilt by walking the records: rebuildDir, or rebuildAppendLog.
				removeDirSnapshots(t, dir)
				c, restored, rebuilt = openCountingSnapshots(t, opts)
				if rebuilt != len(c.shards) || restored != 0 {
					c.Close()
					t.Fatalf("reopen without snapshots: %d restored, %d rebuilt; want all rebuilt", restored, rebuilt)
				}
				checkLen(t, c, "rebuilt from the records")
				if got := c.Len(); got != wantVisible {
					t.Errorf("rebuilt from the records: Len = %d, want %d as before the close", got, wantVisible)
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

// Compaction rebuilds the live count from the records it copies forward, so it rebuilds the
// system count with it. compactShard is driven directly rather than through Compact, which
// reports a pass as having done work when all it did was unlink a wholly dead segment -- a
// return value of 1 would then be read here as "the shard was rewritten" when nothing was
// recounted. The dead bytes it reclaims are checked instead, so the rewrite is what the test
// is standing on.
func TestCompactionKeepsTheSystemCount(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 14, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// A fat attribute, so overwriting the same keys leaves plenty of dead bytes to reclaim.
	pad := strings.Repeat("x", 512)
	for round := range 12 {
		for i := range 20 {
			ad := mustParseAd(t, fmt.Sprintf("[ Seq=%d; Pad=%q ]", round, pad))
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
		for i := range 7 {
			ad := mustParseAd(t, fmt.Sprintf("[ IdemAt=%d; Pad=%q ]", round, pad))
			if err := c.Put([]byte(SystemKey(fmt.Sprintf("idem:batch-%d", i))), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	deadBefore := c.Stats().DeadBytes
	if deadBefore == 0 {
		t.Fatal("the fixture left nothing dead to reclaim")
	}
	c.maintMu.Lock()
	c.compactShard(c.shards[0], c.currentCodec())
	c.maintMu.Unlock()
	if after := c.Stats().DeadBytes; after >= deadBefore {
		t.Fatalf("dead bytes %d -> %d: the shard was not rewritten", deadBefore, after)
	}

	if got := sysLen(c); got != 7 {
		t.Fatalf("after compaction: %d live system records, want 7", got)
	}
	checkLen(t, c, "after a compaction rewrite")
	if got := c.Len(); got != 20 {
		t.Errorf("after a compaction rewrite: Len = %d, want 20", got)
	}
}

// Compaction's copy runs off the shard lock, so writes that arrive during it land in fresh
// segments past the barrier and are folded into the recount by a second loop -- which must
// classify them as the first one does. A writer runs against the copy here to produce them.
// The system records it writes are new keys, so the shard's system count can only be right if
// that second loop counted them: the test asserts it against what a scan sees, and separately
// that the concurrent writes survived at all, since a fixture whose writer finished before the
// copy began would check nothing.
func TestCompactionKeepsTheSystemCountUnderConcurrentWrites(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 1, SegmentSize: 1 << 13, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	pad := strings.Repeat("x", 512)
	for round := range 30 {
		for i := range 60 {
			ad := mustParseAd(t, fmt.Sprintf("[ Seq=%d; Pad=%q ]", round, pad))
			if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}

	// Written only while the copy is in flight, so every one of these is post-barrier.
	stop := make(chan struct{})
	done := make(chan int)
	go func() {
		n := 0
		for {
			select {
			case <-stop:
				done <- n
				return
			default:
			}
			key := SystemKey(fmt.Sprintf("idem:live-%d", n))
			if err := c.Put([]byte(key), mustParseAd(t, fmt.Sprintf(`[ IdemAt=%d ]`, n))); err != nil {
				done <- n
				return
			}
			n++
			if err := c.Put([]byte(fmt.Sprintf("live-%d.0", n)), mustParseAd(t, `[ Seq=1 ]`)); err != nil {
				done <- n
				return
			}
		}
	}()

	c.maintMu.Lock()
	c.compactShard(c.shards[0], c.currentCodec())
	c.maintMu.Unlock()
	close(stop)
	wrote := <-done
	if wrote == 0 {
		t.Fatal("the concurrent writer wrote nothing during the copy")
	}

	checkLen(t, c, "after a compaction rewrite under concurrent writes")
	if got := sysLen(c); got == 0 {
		t.Fatal("no live system records after the rewrite; the check above proves nothing")
	}
}

// Rotation drops whole segments and subtracts what they held from the live count, so it must
// subtract the system records among them from the system count. Both keyspaces are written
// interleaved so the dropped segments hold a mix of the two, and the rotation is required to
// have dropped something.
func TestRotationKeepsTheSystemCount(t *testing.T) {
	c, err := Open(Options{
		Dir: t.TempDir(), AppendOnly: true, SegmentSize: 1 << 12,
		Retention: Retention{MaxSegments: 3}, GroupSchemaCount: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := range 300 {
		key, ad := fmt.Sprintf("%d.0", i), fmt.Sprintf(`[ Seq=%d ]`, i)
		if i%5 == 0 {
			key, ad = SystemKey(fmt.Sprintf("idem:batch-%d", i)), fmt.Sprintf(`[ IdemAt=%d ]`, i)
		}
		if putErr := c.Put([]byte(key), mustParseAd(t, ad)); putErr != nil {
			t.Fatal(putErr)
		}
	}
	checkLen(t, c, "before rotation")

	before := liveSegs(c.shards[0])
	dropped, err := c.Rotate(0)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 {
		t.Fatalf("nothing rotated out of %d segments; the fixture never exceeded MaxSegments", before)
	}
	checkLen(t, c, "after rotation")
	if sysLen(c) == 0 {
		t.Fatal("rotation left no live system records; the check above proves nothing")
	}
}

// Truncate empties the store, so both counts go with it. A system count left behind would make
// Len negative -- the store would report fewer rows than the none it holds.
func TestTruncateClearsTheSystemCount(t *testing.T) {
	c, err := Open(Options{Dir: t.TempDir(), Shards: 2, GroupSchemaCount: -1})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	for i := range 30 {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustParseAd(t, fmt.Sprintf(`[ Seq=%d ]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 9 {
		key := []byte(SystemKey(fmt.Sprintf("idem:batch-%d", i)))
		if err := c.Put(key, mustParseAd(t, `[ IdemAt=1 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	checkLen(t, c, "before truncate")

	c.Truncate()
	if got := c.Len(); got != 0 {
		t.Errorf("after truncate: Len = %d, want 0", got)
	}
	if got := c.StoredLen(); got != 0 {
		t.Errorf("after truncate: StoredLen = %d, want 0", got)
	}
	checkLen(t, c, "after truncate")

	// And the counts restart from empty rather than from whatever was left behind.
	for i := range 4 {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustParseAd(t, `[ Seq=1 ]`)); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Put([]byte(SystemKey("idem:after")), mustParseAd(t, `[ IdemAt=1 ]`)); err != nil {
		t.Fatal(err)
	}
	checkLen(t, c, "after writing again")
	if got := c.Len(); got != 4 {
		t.Errorf("after writing again: Len = %d, want 4", got)
	}
}

// A snapshot written before the system count was carried must not be read as "no system
// records": that is the value an unwritten field reads as, and it would make Len too large by
// exactly the count it failed to subtract -- the bug this all exists to prevent, reintroduced
// by an upgrade. The older snapshot is rewritten here from the real one so the segments it
// describes are genuinely its own, and the restore must derive the count by walking them.
func TestOlderDirSnapshotDerivesTheSystemCount(t *testing.T) {
	dir := t.TempDir()
	opts := Options{Dir: dir, Shards: 2, SegmentSize: 1 << 14, GroupSchemaCount: -1}
	c, err := Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 120 {
		if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), mustParseAd(t, fmt.Sprintf(`[ Seq=%d ]`, i))); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().Unix()
	for i := range 17 {
		key := []byte(SystemKey(fmt.Sprintf("idem:batch-%d", i)))
		if err := c.Put(key, mustParseAd(t, fmt.Sprintf(`[ IdemAt=%d ]`, now))); err != nil {
			t.Fatal(err)
		}
	}
	wantVisible, wantSys := scanLen(c), sysLen(c)
	if wantVisible != 120 || wantSys != 17 {
		t.Fatalf("fixture: %d visible, %d system; want 120 and 17", wantVisible, wantSys)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	downgradeDirSnapshots(t, dir)

	c, restored, rebuilt := openCountingSnapshots(t, opts)
	defer c.Close()
	if restored != len(c.shards) || rebuilt != 0 {
		t.Fatalf("reopen on the older snapshot: %d restored, %d rebuilt; want all restored", restored, rebuilt)
	}
	checkLen(t, c, "restored from an older snapshot")
	if got := c.Len(); got != wantVisible {
		t.Errorf("restored from an older snapshot: Len = %d, want %d", got, wantVisible)
	}
}

// downgradeDirSnapshots rewrites every shard's snapshot as the previous version: the system
// count is dropped from the header and the CRC recomputed, leaving a file this build still
// reads but which no longer carries the count.
func downgradeDirSnapshots(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), dirSnapName)
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		body := data[:len(data)-4]
		if binary.LittleEndian.Uint32(body[4:]) != dirSnapVersion {
			t.Fatalf("%s: snapshot version %d, want %d", p, binary.LittleEndian.Uint32(body[4:]), dirSnapVersion)
		}
		// Header: magic u32, version u32, commitSeq u64, count u64, sysCount u64, ...
		out := make([]byte, 0, len(body)-8)
		out = append(out, body[:20]...)
		out = append(out, body[28:]...)
		binary.LittleEndian.PutUint32(out[4:], dirSnapVersionV2)
		out = binary.LittleEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
		if err := os.WriteFile(p, out, 0o600); err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no snapshots to downgrade")
	}
}
