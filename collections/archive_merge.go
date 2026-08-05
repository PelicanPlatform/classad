package collections

import (
	"os"
	"path/filepath"
	"strings"
)

// Segment merging for append-only (archive) collections.
//
// An archive's segment count grows without bound, and every sealed segment costs a mapping
// at open (loadShard mmaps them all, and each queried sidecar adds a second). Against a
// default vm.max_map_count of 65530 that is a ceiling of roughly 30k segments -- ~240 GiB at
// 8 MiB segments -- and it is a fail-to-START, not a slow query. Merging cold segments is
// what keeps a large archive under it while leaving the recent tail finely divided, which is
// what keeps queries fast (the open segment is rescanned per query, so its size is a
// per-query cost).
//
// Durability. A merge replaces N files with one, which is not an atomic operation, so it is
// staged behind an intent marker:
//
//  1. build the merged segment under a "tmp-merge" name -- recovery parses only
//     "seg-N.dX.dat", so the staged file is invisible until it is renamed
//  2. fsync it, then write the marker naming the sources and the target, and fsync that
//  3. rename the target into place
//  4. unlink the sources
//  5. remove the marker
//
// A crash before (2) leaves an ignored temp file and untouched sources: the merge simply did
// not happen. A crash anywhere after it leaves the marker, and recovery finishes the same
// steps -- each of which is idempotent -- so the outcome never depends on where it stopped.
// The ordering matters: the target is made durable and named in the marker BEFORE any source
// is removed, so no window can lose records. The cost of that choice is that a crash can
// leave the target and its sources briefly coexisting, which recovery resolves by completing
// the merge rather than by trying to detect duplicates after the fact.

const (
	mergeTmpPrefix    = "tmp-merge"
	mergeMarkerSuffix = ".merging"
)

// mergeSegments replaces an adjacent run of sealed segments with a single segment holding
// all of their records, in order. run must be in append order and must not include the
// active segment. Reports whether the merge happened; a failure leaves the sources in place.
//
// The caller holds maintMu (as compaction and reseal do), so merges never overlap each other
// or a reseal.
func (c *Collection) mergeSegments(sh *shard, run []*segment) bool {
	if len(run) < 2 || sh.allocNamed == nil || sh.segDir == "" {
		return false
	}
	for _, s := range run {
		if s == nil || s.used == 0 {
			return false
		}
	}

	// Stage the merged segment under a name recovery ignores.
	merged := c.resealSegmentsAs(sh, run, c.currentCodec(), mergeTmpPrefix)
	if merged == nil {
		return false
	}
	abort := func() bool {
		merged.retire()
		merged.reapAndHook()
		return false
	}
	if err := merged.flush(); err != nil {
		return abort()
	}

	// Record the intent before removing anything, so a crash from here on is finishable.
	marker := filepath.Join(sh.segDir, filepath.Base(merged.path)+mergeMarkerSuffix)
	srcNames := make([]string, len(run))
	for i, s := range run {
		srcNames[i] = filepath.Base(s.path)
	}
	if err := writeFileSync(marker, []byte(strings.Join(srcNames, "\n"))); err != nil {
		return abort()
	}

	final := filepath.Join(sh.segDir, mergedFinalName(filepath.Base(merged.path)))
	if err := os.Rename(merged.path, final); err != nil {
		_ = os.Remove(marker)
		return abort()
	}
	merged.path = final

	// Publish before unlinking: a reader that already holds a source keeps its mapping alive
	// through the pin/reap path, so retiring the sources here is safe even mid-scan.
	sh.mu.Lock()
	if int(run[0].id) >= len(sh.segs) || sh.segs[run[0].id] != run[0] {
		sh.mu.Unlock() // slot moved under us; leave the marker for recovery to finish
		return false
	}
	merged.id = run[0].id
	sh.segs[run[0].id] = merged
	var toReap []*segment
	for i, s := range run {
		if i > 0 {
			sh.segs[s.id] = nil
		}
		if s.retire() {
			toReap = append(toReap, s)
		}
	}
	sh.mu.Unlock()
	for _, s := range toReap {
		s.reapAndHook() // munmap + unlink, off-lock
	}
	// Any source still pinned by an in-flight scan is unlinked when its pins drain; the
	// marker is removed regardless, because the merged file is in place and named, and a
	// replayed unlink of an already-gone file is a no-op.
	_ = os.Remove(marker)
	return true
}

// finishPendingMerges completes any merge interrupted by a crash. Called during recovery,
// before segments are loaded, so the shard is never assembled from a half-merged directory.
//
// Every step is idempotent: the rename is skipped if the target is already in place, and
// unlinking an absent source succeeds. A marker whose target is missing entirely (crashed
// between fsync and rename, with the temp file since removed) is dropped without touching
// the sources -- losing the merge is fine, losing records is not.
func finishPendingMerges(shardDir string) {
	entries, err := os.ReadDir(shardDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), mergeMarkerSuffix) {
			continue
		}
		marker := filepath.Join(shardDir, e.Name())
		body, err := os.ReadFile(marker)
		if err != nil {
			continue
		}
		staged := strings.TrimSuffix(e.Name(), mergeMarkerSuffix)
		final := mergedFinalName(staged)
		stagedPath := filepath.Join(shardDir, staged)
		finalPath := filepath.Join(shardDir, final)

		if _, err := os.Stat(finalPath); err != nil {
			if _, serr := os.Stat(stagedPath); serr != nil {
				// Neither target nor staged file survives: the merge never became durable.
				// Leave every source alone.
				_ = os.Remove(marker)
				continue
			}
			if err := os.Rename(stagedPath, finalPath); err != nil {
				continue // leave the marker; try again next open
			}
		}
		for _, name := range strings.Split(string(body), "\n") {
			name = strings.TrimSpace(name)
			if name == "" || name == final {
				continue
			}
			_ = os.Remove(filepath.Join(shardDir, name))
			_ = os.Remove(filepath.Join(shardDir, name+".idx"))
		}
		_ = os.Remove(marker)
	}
	// Drop any staged output with no marker: its merge never reached the durable point.
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), mergeTmpPrefix+"-") {
			if _, err := os.Stat(filepath.Join(shardDir, e.Name()+mergeMarkerSuffix)); err != nil {
				_ = os.Remove(filepath.Join(shardDir, e.Name()))
			}
		}
	}
}

// mergedFinalName maps a staged merge output ("tmp-merge-7.d0.dat") to the live segment name
// recovery will load ("seg-7.d0.dat"). Both the writer and the recovery path derive the
// target the same way, so a marker never has to record it.
func mergedFinalName(staged string) string {
	return "seg-" + strings.TrimPrefix(staged, mergeTmpPrefix+"-")
}
