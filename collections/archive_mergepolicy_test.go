package collections

import "testing"

// TestMergePassReachesTarget drives the policy end to end: an archive well over its segment
// target is merged down to it, and every record survives in order -- checked again after a
// reopen, since that is where a merge landing in the wrong position would surface.
func TestMergePassReachesTarget(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := buildMergeArchive(t, dir, 4000)
	before := allClusterIDs(t, a)
	segsBefore := a.c.Stats().Segments
	if segsBefore < 40 {
		t.Fatalf("need a lot of segments to exercise the policy, got %d", segsBefore)
	}

	opts := MergeOptions{
		TargetSegments: 8,
		MinMergeBytes:  1, // the corpus is tiny; let run size come from the target
		KeepRecent:     2,
	}
	n := a.MergePass(opts)
	if n == 0 {
		t.Fatal("MergePass did no merges though the archive is far over target")
	}
	after := a.c.Stats().Segments
	if after >= segsBefore {
		t.Fatalf("segments %d -> %d: no reduction", segsBefore, after)
	}
	// KeepRecent segments are exempt, so the floor is the target plus those.
	if after > opts.TargetSegments+opts.KeepRecent+1 {
		t.Errorf("segments = %d after %d merges, want <= ~%d", after, n, opts.TargetSegments+opts.KeepRecent+1)
	}
	if got := allClusterIDs(t, a); !equalIDs(got, before) {
		t.Fatalf("records changed: %d, want %d", len(got), len(before))
	}
	a.Close()

	a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if got := allClusterIDs(t, a2); !equalIDs(got, before) {
		t.Errorf("after reopen: %d records, want %d", len(got), len(before))
	}
	t.Logf("segments %d -> %d in %d merges, %d records preserved", segsBefore, after, n, len(before))
}

// TestMergePassRespectsLimits pins the guards that keep a pass bounded and the hot tail
// intact: nothing happens under target, the newest segments are never consumed, and no
// merged segment exceeds the size cap.
func TestMergePassRespectsLimits(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	t.Run("no-op under target", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 500)
		defer a.Close()
		segs := a.c.Stats().Segments
		if n := a.MergePass(MergeOptions{TargetSegments: segs + 10}); n != 0 {
			t.Errorf("merged %d runs though already under target", n)
		}
	})

	t.Run("keeps the newest segments", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 4000)
		defer a.Close()
		newest := newestSealedSegments(t, a, 3)
		a.MergePass(MergeOptions{TargetSegments: 4, MinMergeBytes: 1, KeepRecent: 3})
		live := map[*segment]bool{}
		for _, s := range policySegments(t, a) {
			live[s] = true
		}
		for i, s := range newest {
			if !live[s] {
				t.Errorf("newest sealed segment %d was merged away despite KeepRecent", i)
			}
		}
	})

	t.Run("honors the size cap", func(t *testing.T) {
		dir := t.TempDir()
		a := buildMergeArchive(t, dir, 4000)
		defer a.Close()
		const cap = 24 << 10
		a.MergePass(MergeOptions{TargetSegments: 1, MinMergeBytes: 1, MaxSegmentBytes: cap, KeepRecent: 0})
		for _, s := range policySegments(t, a) {
			if int64(s.used) > cap*2 {
				t.Errorf("segment holds %d bytes, well past the %d cap", s.used, cap)
			}
		}
	})
}

func policySegments(t *testing.T, a *Archive) []*segment {
	t.Helper()
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	var out []*segment
	for _, s := range sh.segs {
		if s != nil && s.used > 0 {
			out = append(out, s)
		}
	}
	return out
}

func newestSealedSegments(t *testing.T, a *Archive, k int) []*segment {
	t.Helper()
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	var sealed []*segment
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.used > 0 {
			sealed = append(sealed, s)
		}
	}
	if len(sealed) <= k {
		t.Fatalf("need >%d sealed segments, got %d", k, len(sealed))
	}
	return sealed[len(sealed)-k:]
}
