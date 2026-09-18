package collections

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The delta store must be INDISTINGUISHABLE from a whole-record store through every read API,
// not just the ones that were remembered. Reviewing this feature found roughly ten readers
// serving fragments -- short ads presented as complete, biased toward recently-updated keys,
// with no error anywhere. Two hooked paths agreeing proved nothing about the other eight.
//
// So this drives the SAME writes into a delta store and a control store and compares every
// reader's output. A path that is added later and forgets about deltas fails here.

// confPair builds two stores, identical but for delta mode, and applies the same writes.
func confPair(t *testing.T, deltaMax int) (delta, control *Collection) {
	t.Helper()
	mk := func(dm int) *Collection {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16, DeltaMax: dm})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	delta, control = mk(deltaMax), mk(0)
	const n = 300
	for _, c := range []*Collection{delta, control} {
		for i := 0; i < n; i++ {
			ad := jobAd(i, map[string]int64{"N": 0})
			ad.InsertAttrString("Owner", fmt.Sprintf("user%d", i%5))
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("k%04d.0", i)), ad)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatalf("seed %d conflicted", i)
			}
		}
		// A hot subset left mid-chain, so live deltas exist when the readers run.
		for round := 1; round <= 5; round++ {
			for i := 0; i < 60; i++ {
				patch := classad.New()
				patch.InsertAttr("N", int64(round))
				w := c.Begin()
				w.PatchAttrs([]byte(fmt.Sprintf("k%04d.0", i)), patch, nil)
				if r := w.Commit(); r.Conflicted() {
					t.Fatalf("round %d key %d conflicted", round, i)
				}
			}
		}
	}
	if d, _ := delta.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the harness is not exercising what it claims")
	}
	if d, _ := control.DeltaStats(); d != 0 {
		t.Fatal("control wrote deltas; it is not a control")
	}
	return delta, control
}

// adSig renders an ad as a stable string so two stores' answers compare exactly.
func adSig(ad *classad.ClassAd) string {
	if ad == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(ad.AST().Attributes))
	for _, a := range ad.AST().Attributes {
		parts = append(parts, a.Name+"="+a.Value.String())
	}
	sort.Strings(parts)
	return fmt.Sprint(len(parts), ":", parts)
}

func sigsEqual(t *testing.T, path string, got, want []string) {
	t.Helper()
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) {
		t.Errorf("%s: delta store returned %d rows, control returned %d", path, len(got), len(want))
		return
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("%s: row %d differs\n delta:   %s\n control: %s", path, i, got[i], want[i])
			if i+1 < len(got) {
				t.Errorf("  next delta:   %s\n  next control: %s", got[i+1], want[i+1])
			}
			return
		}
	}
}

func collectQuery(t *testing.T, c *Collection, expr string) []string {
	t.Helper()
	q, err := vm.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for ad := range c.Query(q) {
		out = append(out, adSig(ad))
	}
	return out
}

func TestDeltaConformanceGet(t *testing.T) {
	d, ctl := confPair(t, 16)
	var got, want []string
	for i := 0; i < 300; i++ {
		key := []byte(fmt.Sprintf("k%04d.0", i))
		ad, ok := d.Get(key)
		if !ok {
			t.Fatalf("delta store lost %s", key)
		}
		cad, cok := ctl.Get(key)
		if !cok {
			t.Fatalf("control lost %s", key)
		}
		got = append(got, adSig(ad))
		want = append(want, adSig(cad))
	}
	sigsEqual(t, "Get", got, want)
}

func TestDeltaConformanceQueryUnindexed(t *testing.T) {
	d, ctl := confPair(t, 16)
	sigsEqual(t, "Query(true)", collectQuery(t, d, "true"), collectQuery(t, ctl, "true"))
}

// An indexed constraint routes to a different scan than an unindexed one, and the delta for a
// hot key carries no posting for Owner -- so the job silently drops out of the result.
func TestDeltaConformanceQueryFiltered(t *testing.T) {
	d, ctl := confPair(t, 16)
	for _, expr := range []string{`Owner == "user0"`, `N >= 3`, `ClusterId < 60`, `Owner == "user1" || N == 5`} {
		sigsEqual(t, "Query("+expr+")", collectQuery(t, d, expr), collectQuery(t, ctl, expr))
	}
}

func TestDeltaConformanceScan(t *testing.T) {
	d, ctl := confPair(t, 16)
	var got, want []string
	for ad := range d.Scan() {
		got = append(got, adSig(ad))
	}
	for ad := range ctl.Scan() {
		want = append(want, adSig(ad))
	}
	sigsEqual(t, "Scan", got, want)
}

func TestDeltaConformanceForEachAd(t *testing.T) {
	d, ctl := confPair(t, 16)
	var got, want []string
	d.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		got = append(got, adSig(ad))
		return true
	})
	ctl.ForEachAd(func(key string, ad *classad.ClassAd) bool {
		want = append(want, adSig(ad))
		return true
	})
	sigsEqual(t, "ForEachAd", got, want)
}

func TestDeltaConformanceAfterReopen(t *testing.T) {
	d, ctl := confPair(t, 16)
	sigsEqual(t, "Query after compaction", func() []string {
		d.Compact()
		return collectQuery(t, d, "true")
	}(), func() []string {
		ctl.Compact()
		return collectQuery(t, ctl, "true")
	}())
}

// TestDeltaConformanceCrashRecovery is the reviewer's highest-severity finding. rebuildDir --
// which runs on every crash recovery, and on a clean reopen of a time-travel or parent/child
// collection -- relinks each hash bucket to only each key's CURRENT record, and the sealed key
// index skips the active segment by construction. So a delta chain living inside the active
// segment, which is the normal shape, lost its base: the key VANISHED from Get while Has still
// reported it present and a scan yielded a fragment. Three different answers, permanently.
//
// Simulated the way recovery actually happens: drop the directory snapshot, which is what a
// crash leaves behind (it is only written on a clean Close), and reopen.
func TestDeltaConformanceCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	open := func() *Collection {
		c, err := Open(Options{Dir: dir, Shards: 4, SegmentSize: 1 << 16, DeltaMax: 16})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	c := open()
	const n = 60
	for i := 0; i < n; i++ {
		tx := c.Begin()
		tx.Put([]byte(fmt.Sprintf("x%03d.0", i)), jobAd(i, map[string]int64{"N": 0}))
		if r := tx.Commit(); r.Conflicted() {
			t.Fatalf("seed %d conflicted", i)
		}
	}
	for round := 1; round <= 3; round++ {
		for i := 0; i < n; i++ {
			patch := classad.New()
			patch.InsertAttr("N", int64(round))
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("x%03d.0", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("round %d key %d conflicted", round, i)
			}
		}
	}
	if d, _ := c.DeltaStats(); d == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// A crash leaves no directory snapshot, so recovery takes the rebuildDir path.
	removeDirSnapshots(t, dir)

	c2 := open()
	defer c2.Close()
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("x%03d.0", i)
		ad, ok := c2.Get([]byte(key))
		if !ok {
			t.Fatalf("%s vanished after recovery: its chain lost the whole record it merges from", key)
		}
		if got := len(ad.AST().Attributes); got != 45 {
			t.Fatalf("%s has %d attributes after recovery, want 45", key, got)
		}
		if v, _ := ad.EvaluateAttrInt("N"); v != 3 {
			t.Fatalf("%s N = %d after recovery, want 3", key, v)
		}
	}
	// And the readers must agree with each other, not just be non-empty.
	var scanned int
	for ad := range c2.Scan() {
		if got := len(ad.AST().Attributes); got != 45 {
			t.Fatalf("scan after recovery yielded %d attributes, want 45", got)
		}
		scanned++
	}
	if scanned != n {
		t.Fatalf("scan after recovery returned %d rows, want %d", scanned, n)
	}
}

// removeDirSnapshots deletes each shard's directory snapshot, which is what a crash leaves
// behind: the snapshot is written only on a clean Close, so its absence is what sends Open
// down the rebuildDir recovery path.
func removeDirSnapshots(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name(), dirSnapName)
		if err := os.Remove(p); err == nil {
			removed++
		}
	}
	if removed == 0 {
		t.Fatal("no directory snapshots removed; this test is not exercising recovery")
	}
}

// TestDeltaConformanceEncryptedRedacted covers a reader NOT entitled to sealed values.
//
// Replay decodes each version with the collection's data key and re-encodes the merged result,
// so the sealing has to survive that round trip: an attribute that was encrypted at rest must
// still come back undefined for a redacted reader. If it did not, delta mode would quietly
// hand plaintext to a caller the entitlement exists to keep it from -- and the ad would look
// perfectly well-formed, so nothing else would notice.
func TestDeltaConformanceEncryptedRedacted(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	mk := func(dm int) *Collection {
		c, err := Open(Options{
			Dir: t.TempDir(), Shards: 2, SegmentSize: 1 << 16, DeltaMax: dm,
			DataKey: key, EncryptedAttrs: []string{"Secret"},
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	d, ctl := mk(16), mk(0)
	const n = 120
	for _, c := range []*Collection{d, ctl} {
		for i := 0; i < n; i++ {
			ad := jobAd(i, map[string]int64{"N": 0})
			ad.InsertAttrString("Secret", fmt.Sprintf("token-%d", i))
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("e%03d.0", i)), ad)
			if r := tx.Commit(); r.Conflicted() {
				t.Fatalf("seed %d conflicted", i)
			}
		}
		for round := 1; round <= 4; round++ {
			for i := 0; i < n; i++ {
				patch := classad.New()
				patch.InsertAttr("N", int64(round))
				w := c.Begin()
				w.PatchAttrs([]byte(fmt.Sprintf("e%03d.0", i)), patch, nil)
				if r := w.Commit(); r.Conflicted() {
					t.Fatalf("round %d key %d conflicted", round, i)
				}
			}
		}
	}
	if dd, _ := d.DeltaStats(); dd == 0 {
		t.Fatal("no deltas written; the test is not exercising what it claims")
	}

	// An ENTITLED reader sees the secret, in both stores, and they agree.
	for i := 0; i < n; i++ {
		k := []byte(fmt.Sprintf("e%03d.0", i))
		ad, ok := d.Get(k)
		if !ok {
			t.Fatalf("delta store lost e%03d.0", i)
		}
		cad, _ := ctl.Get(k)
		if adSig(ad) != adSig(cad) {
			t.Fatalf("e%03d.0 entitled read differs\n delta:   %s\n control: %s", i, adSig(ad), adSig(cad))
		}
		if v, _ := ad.EvaluateAttrString("Secret"); v != fmt.Sprintf("token-%d", i) {
			t.Fatalf("e%03d.0 entitled reader got Secret=%q", i, v)
		}
	}

	// A REDACTED reader must see the secret as undefined -- and must still see everything else.
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	rows, leaked := 0, 0
	for ad := range d.QueryRedacted(q) {
		rows++
		if e, present := ad.Lookup("Secret"); present {
			if s := e.String(); s != "undefined" {
				leaked++
				if leaked == 1 {
					t.Errorf("redacted reader saw Secret = %s through a delta chain", s)
				}
			}
		}
		if v, _ := ad.EvaluateAttrInt("Pad22"); v != 22 {
			t.Fatalf("redacted read lost an unencrypted attribute: Pad22 = %d", v)
		}
	}
	if rows != n {
		t.Fatalf("redacted query returned %d rows, want %d", rows, n)
	}
	if leaked > 0 {
		t.Fatalf("%d of %d redacted reads exposed the sealed value", leaked, rows)
	}
}

// TestDeltaLowReuseDoesNotRegress guards the workload where delta records STOP paying.
//
// The saving comes from a key being updated several times between seals. Spread the same number
// of updates across many more distinct keys and each one's first write is a whole record
// anyway, so the machinery -- the presence probe, the tracker insert, the collapse scan -- is
// pure overhead. That is a plausible shape for a bigger AP (hundreds of thousands of jobs, each
// touched rarely), and the failure is silent: correct answers, quietly slower.
//
// So this pins that delta mode is not WORSE than the control on that shape. It deliberately
// asserts a loose bound -- the point is to catch a regression into pathology, not to police a
// few percent on a shared machine.
func TestDeltaLowReuseDoesNotRegress(t *testing.T) {
	run := func(deltaMax int) (allocBytes uint64, keys int) {
		c, err := Open(Options{Dir: t.TempDir(), Shards: 4, SegmentSize: 1 << 16, DeltaMax: deltaMax})
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		const n = 4000 // many keys, each touched about twice: almost no reuse
		var m0, m1 runtime.MemStats
		runtime.ReadMemStats(&m0)
		for i := 0; i < n; i++ {
			tx := c.Begin()
			tx.Put([]byte(fmt.Sprintf("lo%05d", i)), jobAd(i, map[string]int64{"N": 0}))
			if r := tx.Commit(); r.Conflicted() {
				t.Fatalf("seed %d conflicted", i)
			}
		}
		for i := 0; i < n; i++ {
			patch := classad.New()
			patch.InsertAttr("N", 1)
			w := c.Begin()
			w.PatchAttrs([]byte(fmt.Sprintf("lo%05d", i)), patch, nil)
			if r := w.Commit(); r.Conflicted() {
				t.Fatalf("update %d conflicted", i)
			}
		}
		runtime.ReadMemStats(&m1)
		return m1.TotalAlloc - m0.TotalAlloc, n
	}
	deltaAlloc, n := run(16)
	ctlAlloc, _ := run(0)
	t.Logf("low-reuse (%d keys, ~1 update each): delta %.2f GiB, control %.2f GiB (%.2fx)",
		n, float64(deltaAlloc)/(1<<30), float64(ctlAlloc)/(1<<30),
		float64(deltaAlloc)/float64(ctlAlloc))
	// Allocation, not time: it is the deterministic axis on a shared machine.
	if deltaAlloc > ctlAlloc*3/2 {
		t.Errorf("delta mode allocated %.2fx the control on a low-reuse workload; the feature "+
			"has become a pessimization for keyspaces where reuse is rare",
			float64(deltaAlloc)/float64(ctlAlloc))
	}
}
