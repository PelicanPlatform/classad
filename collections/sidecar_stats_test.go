package collections

import (
	"bytes"
	"fmt"
	"path/filepath"
	"testing"
)

// TestSidecarStatsRoundTrip verifies the v7 per-attribute stats block: the summary a
// mmapSegIndex reads back (statsFor) is identical to the in-RAM segIndex's build-time stats.
// This is what lets a sealed segment answer canSkip/estCandidates/selectivityOrder over the
// mmap without paging its postings to recompute finishStats.
func TestSidecarStatsRoundTrip(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1, CategoricalAttrs: []string{"Owner", "State"}, ValueAttrs: []string{"Memory", "Cpus"}})
	owners := []string{"alice", "bob", "carol", "dave"}
	states := []string{"Idle", "Running", "Held"}
	for i := 0; i < 3000; i++ {
		ad := fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; Memory=%d; Cpus=%d ]`,
			i, owners[i%len(owners)], states[i%len(states)], (i%16+1)*512, i%8)
		if i%23 == 0 { // a type exception for Memory
			ad = fmt.Sprintf(`[ Id=%d; Owner=%q; State=%q; Memory="lots"; Cpus=%d ]`, i, owners[i%len(owners)], states[i%len(states)], i%8)
		}
		if err := c.Put([]byte(fmt.Sprintf("m%d", i)), mustAd(t, ad)); err != nil {
			t.Fatal(err)
		}
	}
	c.Reindex()

	checked := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			si := seg.idx.Load()
			if si == nil {
				continue
			}
			path := filepath.Join(t.TempDir(), fmt.Sprintf("seg-%d.idx", seg.id))
			if err := writeSidecarIndex(path, si); err != nil {
				t.Fatalf("write sidecar: %v", err)
			}
			data, closer, err := mapFile(path)
			if err != nil {
				t.Fatalf("map sidecar: %v", err)
			}
			msi, err := parseMmapSidecar(data)
			if err != nil {
				t.Fatalf("parse sidecar: %v", err)
			}
			for id, cp := range si.cat {
				assertStatsEqual(t, "cat", &cp.stats, msi.catStats[id])
				checked++
			}
			for id, vp := range si.val {
				assertStatsEqual(t, "val", &vp.stats, msi.valStats[id])
				checked++
			}
			_ = closer()
		}
	}
	if checked == 0 {
		t.Fatal("no attribute stats were compared")
	}
}

func assertStatsEqual(t *testing.T, kind string, want, got *segStats) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no mmap stats parsed", kind)
	}
	if want.covered != got.covered || want.exc != got.exc || want.ndv != got.ndv {
		t.Errorf("%s: covered/exc/ndv mismatch: want %d/%d/%d got %d/%d/%d",
			kind, want.covered, want.exc, want.ndv, got.covered, got.exc, got.ndv)
	}
	if want.hasRange != got.hasRange || (want.hasRange && (want.min != got.min || want.max != got.max)) {
		t.Errorf("%s: range mismatch: want hasRange=%v [%v,%v] got hasRange=%v [%v,%v]",
			kind, want.hasRange, want.min, want.max, got.hasRange, got.min, got.max)
	}
	if len(want.top) != len(got.top) {
		t.Fatalf("%s: top-N length mismatch: want %d got %d", kind, len(want.top), len(got.top))
	}
	for i := range want.top {
		if want.top[i].skey != got.top[i].skey || want.top[i].fkey != got.top[i].fkey || want.top[i].count != got.top[i].count {
			t.Errorf("%s: top[%d] mismatch: want %+v got %+v", kind, i, want.top[i], got.top[i])
		}
	}
	switch {
	case want.hll == nil && got.hll == nil:
	case want.hll == nil || got.hll == nil:
		t.Errorf("%s: HLL presence mismatch (want nil=%v, got nil=%v)", kind, want.hll == nil, got.hll == nil)
	case !bytes.Equal(want.hll.reg, got.hll.reg):
		t.Errorf("%s: HLL registers differ", kind)
	}
}
