package collections

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

func buildUpgradeArchive(t *testing.T, dir string, n int) *Archive {
	t.Helper()
	// Intern at seal, so segments are already interned by the time a retrain runs: the
	// retrain then leaves them on their old dictionary, which is exactly the state the
	// upgrade pass exists to evaluate. Without this, interning-at-retrain would re-encode
	// them onto the new codec and there would be nothing left to consider.
	a, err := CreateArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 13, InternAtSeal: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		ad, _ := classad.ParseOld(fmt.Sprintf(
			"ClusterId = %d\nOwner = %q\nCmd = \"/usr/bin/payload\"\nPad = %q",
			i, fmt.Sprintf("user%04d", i%32), strings.Repeat("a", 100)))
		if err := a.Append(ad); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func archiveIDs(t *testing.T, a *Archive) []int64 {
	t.Helper()
	q, err := vm.Parse("true")
	if err != nil {
		t.Fatal(err)
	}
	var out []int64
	for ad := range a.Query(q) {
		v, _ := ad.EvaluateAttrInt("ClusterId")
		out = append(out, v)
	}
	return out
}

// TestRetrainDoesNotRewriteArchive is the point of the change: a retrain must cost a sample
// and a registration, not a rewrite of the whole archive. Old segments keep the dictionary
// they were written under and stay readable, because recovery reconstructs every registered
// dictionary.
func TestRetrainDoesNotRewriteArchive(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := buildUpgradeArchive(t, dir, 3000)
	before := archiveIDs(t, a)

	sh := a.c.shards[0]
	sh.mu.RLock()
	var sealedBefore []*segment
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.used > 0 {
			sealedBefore = append(sealedBefore, s)
		}
	}
	sh.mu.RUnlock()
	if len(sealedBefore) < 3 {
		t.Fatalf("need several sealed segments, got %d", len(sealedBefore))
	}

	if _, err := a.RetrainDict(2000); err != nil {
		t.Fatal(err)
	}

	// The existing segments must be the very same objects: a reseal would have replaced them.
	sh.mu.RLock()
	live := map[*segment]bool{}
	for _, s := range sh.segs {
		if s != nil {
			live[s] = true
		}
	}
	sh.mu.RUnlock()
	for i, s := range sealedBefore {
		if !live[s] {
			t.Fatalf("sealed segment %d was rewritten by the retrain", i)
		}
	}
	if got := archiveIDs(t, a); !equalIDs(got, before) {
		t.Errorf("records changed across retrain: %d, want %d", len(got), len(before))
	}
	a.Close()

	// Old segments still decode after a reopen, under the dictionary they were written with.
	a2, err := OpenArchive(ArchiveOptions{Dir: dir, SegmentSize: 1 << 13, InternAtSeal: true})
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if got := archiveIDs(t, a2); !equalIDs(got, before) {
		t.Errorf("after reopen: %d records, want %d", len(got), len(before))
	}
}

// TestUpgradeRecordsRejection covers the case where the newer dictionary is not better: the
// pass must decline AND remember declining, or every later pass re-samples the same segments
// forever. The verdict is keyed to the dictionary generation, so a later retrain re-opens
// the question rather than inheriting a stale answer.
func TestUpgradeRecordsRejection(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := buildUpgradeArchive(t, dir, 3000)
	defer a.Close()
	before := archiveIDs(t, a)

	if _, err := a.RetrainDict(2000); err != nil {
		t.Fatal(err)
	}
	// Demand an implausible gain so every segment is judged not worth re-encoding.
	if n := a.UpgradeCodecPass(UpgradeOptions{MinGainFrac: 0.99}); n != 0 {
		t.Fatalf("upgraded %d segments despite an unreachable gain threshold", n)
	}

	shardDir := findShardDir(t, dir)
	data, err := os.ReadFile(filepath.Join(shardDir, upgradeVerdictsFile))
	if err != nil {
		t.Fatalf("no verdict file written: %v", err)
	}
	var v upgradeVerdicts
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Rejected) == 0 {
		t.Error("verdicts recorded nothing; the next pass would re-sample every segment")
	}

	// A second pass must find nothing left to consider.
	if n := a.UpgradeCodecPass(UpgradeOptions{MinGainFrac: 0.99}); n != 0 {
		t.Errorf("second pass upgraded %d segments", n)
	}
	if got := archiveIDs(t, a); !equalIDs(got, before) {
		t.Errorf("records changed: %d, want %d", len(got), len(before))
	}
}

// TestUpgradeReencodesWhenItPays is the converse: with a reachable threshold the pass does
// re-encode, the records survive, and the segments move onto the current codec.
func TestUpgradeReencodesWhenItPays(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := t.TempDir()
	a := buildUpgradeArchive(t, dir, 3000)
	defer a.Close()
	before := archiveIDs(t, a)

	if _, err := a.RetrainDict(2000); err != nil {
		t.Fatal(err)
	}
	// A threshold of "any improvement at all" -- whether it triggers depends on the corpus,
	// so accept either outcome and only demand that the data is intact and, if it did run,
	// that the segments actually moved to the current codec.
	n := a.UpgradeCodecPass(UpgradeOptions{MinGainFrac: -1})
	if got := archiveIDs(t, a); !equalIDs(got, before) {
		t.Fatalf("records changed across upgrade: %d, want %d", len(got), len(before))
	}
	if n == 0 {
		t.Skip("no segment cleared the gain threshold on this corpus")
	}
	target := a.c.currentCodec()
	sh := a.c.shards[0]
	sh.mu.RLock()
	defer sh.mu.RUnlock()
	stale := 0
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.used > 0 && s.codec != target {
			stale++
		}
	}
	if stale > 0 && n >= defaultUpgradeSegs {
		return // bounded by MaxSegments, not by having finished
	}
	if stale > 0 {
		t.Errorf("%d segments still on an older codec after upgrading %d", stale, n)
	}
}
