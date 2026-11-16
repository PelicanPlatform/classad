package collections

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
)

// openCarrier opens the carrier fixture, which has segments large enough to hold a real
// trained dictionary.
func openCarrier(t *testing.T, dir string, shards int) *Collection {
	t.Helper()
	c, err := Open(Options{Dir: dir, Shards: shards, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// trailerCarriers lists the segment files under dir that hold a copy of dictionary id,
// by base name.
func trailerCarriers(t *testing.T, dir string, shards int, id uint32) []string {
	t.Helper()
	var out []string
	for s := 0; s < shards; s++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", s))
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".dat") {
				continue
			}
			tr, ok := readSegTrailerFile(filepath.Join(shardDir, e.Name()))
			if !ok {
				continue
			}
			if _, has := tr.dictReplica(id); has {
				out = append(out, e.Name())
			}
		}
	}
	sort.Strings(out)
	return out
}

func stateFor(t *testing.T, states []DictReplicaState, id uint32) DictReplicaState {
	t.Helper()
	for _, s := range states {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("no state reported for dictionary %d (got %+v)", id, states)
	return DictReplicaState{}
}

// TestEnsureDictReplicasReachesTheTarget: the census writes copies until the level is met,
// and the copies on disk are the dictionary rather than something that merely parses.
func TestEnsureDictReplicasReachesTheTarget(t *testing.T) {
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)

	st := stateFor(t, c.EnsureDictReplicas(), 1)
	if st.Err != nil {
		t.Fatalf("census: %v", st.Err)
	}
	if st.Copies != dictReplicaTarget || st.Written != dictReplicaTarget {
		t.Fatalf("copies=%d written=%d; want %d of each", st.Copies, st.Written, dictReplicaTarget)
	}

	want, err := os.ReadFile(filepath.Join(dir, "dicts", "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	carriers := trailerCarriers(t, dir, 1, 1)
	if len(carriers) != dictReplicaTarget {
		t.Fatalf("%d segment(s) carry dictionary 1; want %d", len(carriers), dictReplicaTarget)
	}
	for _, name := range carriers {
		tr, ok := readSegTrailerFile(filepath.Join(dir, "0", name))
		if !ok {
			t.Fatalf("%s: no trailer", name)
		}
		got, _ := tr.dictReplica(1)
		if !bytes.Equal(got, want) {
			t.Errorf("%s carries something that is not dictionary 1", name)
		}
	}
}

// TestEnsureDictReplicasIsIdempotent: the census runs on every maintenance pass, so a pass
// that found the level met must write nothing at all.
func TestEnsureDictReplicasIsIdempotent(t *testing.T) {
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)
	if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Written == 0 {
		t.Fatal("the first pass wrote nothing")
	}
	before := trailerCarriers(t, dir, 1, 1)

	st := stateFor(t, c.EnsureDictReplicas(), 1)
	if st.Written != 0 {
		t.Errorf("a second pass wrote %d more copies", st.Written)
	}
	if st.Copies != dictReplicaTarget {
		t.Errorf("copies = %d; want %d", st.Copies, dictReplicaTarget)
	}
	after := trailerCarriers(t, dir, 1, 1)
	if len(before) != len(after) {
		t.Errorf("carriers changed: %v -> %v", before, after)
	}
}

// TestEnsureDictReplicasPrefersTheNewestSegments is the placement rule, and it is not
// cosmetic: retention reaps the OLDEST segments contiguously, so copies spread by age are
// the first to go on a store at its byte ceiling. The newest segments are where a copy
// survives longest.
func TestEnsureDictReplicasPrefersTheNewestSegments(t *testing.T) {
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)

	type seg struct {
		name string
		rank uint64
	}
	var sealed []seg
	sh := c.shards[0]
	sh.mu.RLock()
	for _, s := range sh.segs {
		if s != nil && s != sh.act && s.path != "" && s.used > 0 {
			sealed = append(sealed, seg{filepath.Base(s.path), s.maxSeq})
		}
	}
	sh.mu.RUnlock()
	if len(sealed) <= dictReplicaTarget {
		t.Skipf("fixture has only %d sealed segments", len(sealed))
	}
	sort.Slice(sealed, func(a, b int) bool { return sealed[a].rank > sealed[b].rank })
	wantNames := []string{}
	for _, s := range sealed[:dictReplicaTarget] {
		wantNames = append(wantNames, s.name)
	}
	sort.Strings(wantNames)

	c.EnsureDictReplicas()
	got := trailerCarriers(t, dir, 1, 1)
	if strings.Join(got, ",") != strings.Join(wantNames, ",") {
		t.Errorf("carriers = %v; want the newest %d: %v", got, dictReplicaTarget, wantNames)
	}
}

// TestEnsureDictReplicasSpreadsAcrossShards: where the store has more than one shard the
// copies go in different ones, which is the only extra failure domain available without
// leaving the store.
//
// The fixture is deliberately SKEWED -- its last and newest segments all belong to one
// shard -- because an evenly loaded store spreads the copies by accident, through the
// newest-first rule alone, and would pass whether or not the shard rule existed.
func TestEnsureDictReplicasSpreadsAcrossShards(t *testing.T) {
	const shards = 3
	dir := buildCarrierStoreShards(t, shards)
	c := openCarrier(t, dir, shards)
	// Pile the newest records into one shard, so every globally-newest segment is its.
	sample := loadCorpus(t)
	n := 0
	for i := 0; n < 1600; i++ {
		key := []byte(fmt.Sprintf("skew%07d", i))
		if c.shardOf(key, c.h.Hash(key)) != 0 {
			continue
		}
		if err := c.Put(key, sample[n%len(sample)]); err != nil {
			t.Fatal(err)
		}
		n++
	}

	if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Copies != dictReplicaTarget {
		t.Fatalf("copies = %d (%v); want %d", st.Copies, st.Err, dictReplicaTarget)
	}
	// The store rounds its shard count up to a power of two, so ask it rather than the
	// option: scanning the shards that were REQUESTED would miss the ones it made.
	perShard := map[int]int{}
	for s := 0; s < len(c.shards); s++ {
		shardDir := filepath.Join(dir, fmt.Sprintf("%d", s))
		entries, err := os.ReadDir(shardDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".dat") {
				continue
			}
			if tr, ok := readSegTrailerFile(filepath.Join(shardDir, e.Name())); ok {
				if _, has := tr.dictReplica(1); has {
					perShard[s]++
				}
			}
		}
	}
	if len(perShard) != dictReplicaTarget {
		t.Errorf("copies landed in %d shards of %d; want one each in %d: %v",
			len(perShard), len(c.shards), dictReplicaTarget, perShard)
	}
	for s, n := range perShard {
		if n != 1 {
			t.Errorf("shard %d holds %d copies; one per shard is the rule while shards remain", s, n)
		}
	}
}

// TestEnsureDictReplicasReseedsAfterLosingACarrier is why this is a level and not an
// invariant: nothing guards the reap sites, so a pass that takes a carrier is put right by
// the next census rather than being a bug.
func TestEnsureDictReplicasReseedsAfterLosingACarrier(t *testing.T) {
	dir := buildCarrierStore(t)
	func() {
		c := openCarrier(t, dir, 1)
		c.EnsureDictReplicas()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}()
	carriers := trailerCarriers(t, dir, 1, 1)
	if len(carriers) != dictReplicaTarget {
		t.Fatalf("setup: %d carriers", len(carriers))
	}

	// Take one carrier away, the way a reap would.
	victim := filepath.Join(dir, "0", carriers[0])
	if err := os.Remove(victim); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(victim + ".idx")
	_ = os.Remove(filepath.Join(dir, "0", dirSnapName))

	c := openCarrier(t, dir, 1)
	st := stateFor(t, c.EnsureDictReplicas(), 1)
	if st.Copies != dictReplicaTarget {
		t.Errorf("copies = %d after losing a carrier (%v); want the level restored", st.Copies, st.Err)
	}
	if st.Written != 1 {
		t.Errorf("wrote %d copies; want exactly the one that was lost", st.Written)
	}
}

// TestEnsureDictReplicasReportsWhenNothingCanCarry: a store whose segments are all too
// small for its dictionary cannot meet the level, and must say so every pass rather than
// filling the trailers with something that dwarfs the data beside it.
func TestEnsureDictReplicasReportsWhenNothingCanCarry(t *testing.T) {
	dir, _ := buildTwoDictStore(t) // 16 KiB segments; a trained dictionary is ~112 KB
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	states := c.EnsureDictReplicas()
	if len(states) == 0 {
		t.Fatal("no dictionary reported at all")
	}
	for _, st := range states {
		if st.Copies != 0 || st.Written != 0 {
			t.Errorf("dictionary %d: wrote %d copies into segments that cannot hold one", st.ID, st.Written)
		}
		if st.Err == nil {
			t.Errorf("dictionary %d: unmet target reported as success", st.ID)
		}
	}
	if n := len(trailerCarriers(t, dir, 1, 1)); n != 0 {
		t.Errorf("%d segment(s) got a trailer anyway", n)
	}
}

// TestMaintenanceRunsTheReplicaCensus proves the hook: nothing else runs the census on a
// store that is simply being used.
func TestMaintenanceRunsTheReplicaCensus(t *testing.T) {
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)
	c.lastDictReplica.Store(0)
	c.lastDictVerify.Store(0)

	c.Compact()

	if n := len(trailerCarriers(t, dir, 1, 1)); n == 0 {
		t.Error("a maintenance pass left the dictionary with no copies")
	}
}

// TestDictionaryIsRecoverableFromASegment is the point of the whole mechanism: with
// dicts/ gone, the dictionary is still inside the segments that need it.
func TestDictionaryIsRecoverableFromASegment(t *testing.T) {
	dir := buildCarrierStore(t)
	func() {
		c := openCarrier(t, dir, 1)
		c.EnsureDictReplicas()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}()

	dicts := filepath.Join(dir, "dicts")
	want, err := os.ReadFile(filepath.Join(dicts, "1.zst"))
	if err != nil {
		t.Fatal(err)
	}
	if rerr := os.RemoveAll(dicts); rerr != nil { // the whole directory, as it went in production
		t.Fatal(rerr)
	}
	if _, oerr := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20}); oerr == nil {
		t.Fatal("control: the store opened with no dictionaries at all")
	}

	// Find a copy the way a recovery pass would: ask the segments.
	var found []byte
	for _, name := range trailerCarriers(t, dir, 1, 1) {
		tr, ok := readSegTrailerFile(filepath.Join(dir, "0", name))
		if !ok {
			continue
		}
		if b, has := tr.dictReplica(1); has {
			found = append([]byte(nil), b...)
			break
		}
	}
	if found == nil {
		t.Fatal("no segment carried a copy of the dictionary")
	}
	if !bytes.Equal(found, want) {
		t.Fatal("the copy is not the dictionary")
	}

	if mkErr := os.MkdirAll(dicts, 0o755); mkErr != nil {
		t.Fatal(mkErr)
	}
	if werr := os.WriteFile(filepath.Join(dicts, "1.zst"), found, 0o644); werr != nil {
		t.Fatal(werr)
	}
	c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 20})
	if err != nil {
		t.Fatalf("the store did not open on the recovered dictionary: %v", err)
	}
	defer c.Close()
	if n := c.Len(); n == 0 {
		t.Error("no records came back")
	}
}

// TestEnsureDictReplicasReportsAStoreWithNowhereToPutACopy: a young archive whose only
// segment is the one still being appended to has no carrier at all. The census must say
// the level is unmet rather than report a clean pass over nothing -- this is the state a
// store is in for exactly as long as it takes to seal a first segment, and reporting it
// as healthy would make the first pass after that silence look like a regression.
func TestEnsureDictReplicasReportsAStoreWithNowhereToPutACopy(t *testing.T) {
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)

	// Take every sealed segment out of consideration, leaving only the active one.
	sh := c.shards[0]
	sh.mu.Lock()
	kept := 0
	for i, seg := range sh.segs {
		if seg != nil && seg != sh.act {
			sh.segs[i] = nil
			kept++
		}
	}
	sh.mu.Unlock()
	if kept == 0 {
		t.Skip("fixture had no sealed segments to remove")
	}

	st := stateFor(t, c.EnsureDictReplicas(), 1)
	if st.Written != 0 {
		t.Errorf("wrote %d copies with no sealed segment to put one in", st.Written)
	}
	if st.Copies != 0 {
		t.Errorf("copies = %d; want 0", st.Copies)
	}
	if st.Err == nil {
		t.Fatal("a target that cannot be met was reported as met")
	}
	if !strings.Contains(st.Err.Error(), "carry dictionary 1") {
		t.Errorf("the report does not say what is unmet: %v", st.Err)
	}
}

// TestEnsureDictReplicasConcurrent: the census runs from maintenance on its own schedule
// AND from any caller that asks, so two can overlap. Each carrier's trailer contents are
// snapshotted under the shard lock for exactly that reason -- reading a segment's list
// while another pass appends to it is a data race, which only shows up under -race and
// only when two passes actually overlap.
func TestEnsureDictReplicasConcurrent(t *testing.T) {
	dir := buildCarrierStore(t)
	c := openCarrier(t, dir, 1)

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 3; j++ {
				c.EnsureDictReplicas()
			}
		}()
	}
	wg.Wait()

	// Whatever the interleaving, the level is met and no segment ends up with the same
	// dictionary twice.
	if st := stateFor(t, c.EnsureDictReplicas(), 1); st.Copies != dictReplicaTarget {
		t.Errorf("copies = %d (%v); want %d", st.Copies, st.Err, dictReplicaTarget)
	}
	if n := len(trailerCarriers(t, dir, 1, 1)); n != dictReplicaTarget {
		t.Errorf("%d segments carry dictionary 1; want %d", n, dictReplicaTarget)
	}
}
