package collections

// Keeping a dictionary in more than one place.
//
// A compression dictionary exists once, in dicts/<id>.zst, and the segments that need it
// record only its NUMBER, in their file names. Lose the file and every record in every
// segment naming it is unreadable, which is a single point of failure for most of a store.
//
// Holding the file open (dictpin.go) covers the loss while this process is alive. It does
// not cover a loss that outlives the process, which is the shape the failure actually
// took: the directory went, the daemon ran for eleven more days on the codecs it already
// held, and the records were gone at the next restart.
//
// So a copy also rides inside segments, in their trailers (segtrailer.go). Segment files
// are the right place and not merely a convenient one: if what removes a dictionary is an
// age-based tmp cleaner -- the leading theory, since a dictionary is read once at Open and
// then never touched, leaving its atime stale while the daemon is busiest -- then any
// extra copy kept as a loose file has the same cold profile as the original and dies in
// the same sweep. Segment files are mmapped and stay hot.
//
// The rule is a LEVEL, not an invariant:
//
//	Maintain dictReplicaTarget copies of every dictionary a live segment names. Place each
//	in a different shard where the store has more than one, and within a shard in the
//	newest sealed segment not already carrying it. Re-seed when a carrier is dropped.
//
// Nothing guards the reap sites, and that is deliberate. There are at least six of them,
// and a precondition on each is the surface area that produced the bug where pruning
// consulted the mapped segments rather than the segment FILES. Worse, there is no reliable
// place to hook: a segment's unlink can happen from a reader's unpin, in arbitrary context
// (see segment.retire). So reapers stay dumb, the level is restored afterwards, and a reap
// that takes the last copy is self-corrected by the next pass rather than being a
// correctness bug. The exposure is the joint probability of a reap taking the last copy
// AND dicts/ vanishing before the next pass -- and these are backups; dicts/ is still the
// primary and is present in every ordinary case.
//
// Newest-first is not an aesthetic choice. Retention reaps the OLDEST segments
// contiguously, so copies spread evenly by age are the first to go on an archive at its
// byte ceiling -- which is the case that matters. Putting them in the newest segments puts
// them where they are reclaimed last. Different-shard-first buys the one extra failure
// domain available without leaving the store.

import (
	"fmt"
	"sort"
	"time"
)

// dictReplicaTarget is how many copies of each dictionary to keep in segment trailers.
// Three is enough that losing the level takes three reaps between two maintenance passes,
// and cheap: the cost is dictReplicaTarget * ~112 KB per dictionary for the whole store,
// not per segment and not per byte written.
const dictReplicaTarget = 3

// dictReplicaInterval bounds how often maintenance re-runs the census. The census itself
// is a loop over segment metadata already in memory and costs nothing; this paces the
// WRITES that follow it, which matter on a store sealing segments quickly.
const dictReplicaInterval = time.Minute

// DictReplicaState is where one dictionary's copies stand after a census.
type DictReplicaState struct {
	ID     uint32
	Copies int // how many segment trailers hold it now
	Target int
	// Written is how many copies this pass added.
	Written int
	// Err is why the target could not be reached: no segment with room, or a write that
	// failed. A store whose segments are all too small for a dictionary reports it here
	// every pass, which is the honest answer -- see replicaSizeRatio.
	Err error
}

// EnsureDictReplicas brings every dictionary's copy count up to the target and reports
// where each one stands.
//
// Maintenance calls this on its own, rate-limited; this entry point runs it immediately.
// It is safe against concurrent readers: a trailer lives past the segment's mapping, so
// writing one touches no mapped byte, and it is safe against a concurrent reap because the
// write opens the segment file without O_CREATE -- a segment already unlinked simply fails
// and is skipped.
func (c *Collection) EnsureDictReplicas() []DictReplicaState {
	c.lastDictReplica.Store(time.Now().UnixNano())
	return c.ensureDictReplicas()
}

// maybeEnsureDictReplicas runs the census unless one ran within dictReplicaInterval.
func (c *Collection) maybeEnsureDictReplicas() {
	now := time.Now().UnixNano()
	last := c.lastDictReplica.Load()
	if now-last < int64(dictReplicaInterval) {
		return
	}
	if !c.lastDictReplica.CompareAndSwap(last, now) {
		return
	}
	c.ensureDictReplicas()
}

// carrier is one sealed segment considered as a place to put a copy.
type carrier struct {
	shard int
	seg   *segment
	path  string
	used  int
	// rank orders carriers within a shard, newest first.
	rank uint64
	// dictID is the compression dictionary this segment's records were written under,
	// and fingerprinted whether its trailer already records which one that was.
	dictID        uint32
	fingerprinted bool
	// carries is what this segment's trailer already holds, snapshotted under the shard
	// lock with everything else. The pass reads THIS rather than the segment, because the
	// segment's own list is appended to under that lock and a concurrent census -- a
	// caller's EnsureDictReplicas against a maintenance pass -- would otherwise read it
	// while the other writes.
	carries map[uint32]bool
}

// dictCensus is one pass's view: which dictionaries live segments name, where the copies
// are, and which segments could hold another.
type dictCensus struct {
	needed   map[uint32]bool
	copies   map[uint32]int
	byShard  map[uint32]map[int]bool // dict id -> shards already holding a copy
	carriers []carrier
}

// takeDictCensus reads the shards. Everything it needs is already in memory -- the
// dictionary a segment was written under, and the copies its trailer carries, both read at
// recovery -- so this costs no I/O at all.
func (c *Collection) takeDictCensus() dictCensus {
	cen := dictCensus{
		needed:  map[uint32]bool{},
		copies:  map[uint32]int{},
		byShard: map[uint32]map[int]bool{},
	}
	for i, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			// Which dictionary this segment's records need. The ACTIVE segment counts
			// here -- its dictionary is as load-bearing as any -- even though it can
			// never be a carrier.
			if id, ok := c.dicts.idFor(seg.codec); ok && id != 0 {
				cen.needed[id] = true
			}
			for _, id := range seg.carriedDicts {
				cen.copies[id]++
				if cen.byShard[id] == nil {
					cen.byShard[id] = map[int]bool{}
				}
				cen.byShard[id][i] = true
			}
			if seg == sh.act || !seg.persistent || seg.path == "" || seg.used == 0 {
				continue
			}
			carries := make(map[uint32]bool, len(seg.carriedDicts))
			for _, id := range seg.carriedDicts {
				carries[id] = true
			}
			segDict, _ := c.dicts.idFor(seg.codec)
			cen.carriers = append(cen.carriers, carrier{
				shard: i, seg: seg, path: seg.path, used: seg.used, rank: seg.maxSeq,
				carries: carries, dictID: segDict, fingerprinted: seg.fingerprinted,
			})
		}
		sh.mu.RUnlock()
	}
	// Newest first: retention reaps the oldest contiguously, so a copy in the newest
	// segment is the last one it can take.
	sort.SliceStable(cen.carriers, func(a, b int) bool {
		return cen.carriers[a].rank > cen.carriers[b].rank
	})
	return cen
}

func (c *Collection) ensureDictReplicas() []DictReplicaState {
	// One census at a time. Overlapping passes would each find the level unmet and each
	// top it up, leaving the store over the target by the number that raced.
	c.dictReplicaMu.Lock()
	defer c.dictReplicaMu.Unlock()
	cen := c.takeDictCensus()
	ids := make([]uint32, 0, len(cen.needed))
	for id := range cen.needed {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	out := make([]DictReplicaState, 0, len(ids))
	for _, id := range ids {
		st := DictReplicaState{ID: id, Copies: cen.copies[id], Target: dictReplicaTarget}
		if st.Copies < st.Target {
			c.topUpDictReplica(id, &cen, &st)
		}
		out = append(out, st)
	}
	c.recordDictFingerprints(&cen)
	return out
}

// dictFingerprintBudget bounds how many fingerprints one pass writes. Every sealed segment
// wants one and an archive has thousands, so without a budget the first census after an
// upgrade would write to every segment in the store in a single maintenance pass. They are
// twelve bytes each and in no hurry.
const dictFingerprintBudget = 64

// recordDictFingerprints writes, into each sealed segment that lacks one, which dictionary
// its records were written under and what that dictionary was.
//
// This is the half of the check that outlives the process. The id is already on disk in
// the file name, so what the fingerprint adds is the ability to notice that the FILE under
// that id is no longer the dictionary those records were compressed with -- which nothing
// else sees until every one of them fails to decode.
func (c *Collection) recordDictFingerprints(cen *dictCensus) {
	spent := 0
	for i := range cen.carriers {
		if spent >= dictFingerprintBudget {
			return
		}
		cand := &cen.carriers[i]
		if cand.fingerprinted || cand.dictID == 0 {
			continue // already recorded, or written under the base codec, which has no file
		}
		c.dicts.mu.Lock()
		pin := c.dicts.pins[cand.dictID]
		c.dicts.mu.Unlock()
		if pin == nil {
			continue
		}
		if err := addDictFingerprint(cand.path, cand.used, cand.dictID, pin.loaded()); err != nil {
			continue // no room, or a segment reaped out from under this pass
		}
		noteFingerprinted(c.shards[cand.shard], cand.seg)
		cand.fingerprinted = true
		spent++
	}
}

// noteFingerprinted records a fingerprint just written, under the shard lock that guards
// the segment's mutable state.
func noteFingerprinted(sh *shard, seg *segment) {
	sh.mu.Lock()
	seg.fingerprinted = true
	sh.mu.Unlock()
}

// topUpDictReplica writes copies of one dictionary until the target is met or the
// candidates run out.
func (c *Collection) topUpDictReplica(id uint32, cen *dictCensus, st *DictReplicaState) {
	c.dicts.mu.Lock()
	pin := c.dicts.pins[id]
	c.dicts.mu.Unlock()
	if pin == nil {
		st.Err = fmt.Errorf("collections: dictionary %d has no file to copy", id)
		return
	}
	dict := pin.loaded()

	// Two rounds: shards with no copy yet, then anywhere. The first spreads the copies
	// across the failure domains the store has; the second is what a single-shard store
	// takes, and what a multi-shard store falls back to once every shard has one.
	placed := cen.byShard[id]
	for round := 0; round < 2 && st.Copies < st.Target; round++ {
		for i := range cen.carriers {
			if st.Copies >= st.Target {
				break
			}
			cand := &cen.carriers[i]
			if round == 0 && placed != nil && placed[cand.shard] {
				continue
			}
			if cand.carries[id] {
				continue
			}
			if werr := addDictReplica(cand.path, cand.used, id, dict); werr != nil {
				// A segment too small, or one reaped out from under this pass. Neither
				// is a reason to stop: another segment may well take the copy.
				st.Err = werr
				continue
			}
			noteCarried(c.shards[cand.shard], cand.seg, id)
			cand.carries[id] = true
			if placed == nil {
				placed = map[int]bool{}
				cen.byShard[id] = placed
			}
			placed[cand.shard] = true
			cen.copies[id]++
			st.Copies++
			st.Written++
			st.Err = nil
		}
	}
	if st.Copies < st.Target && st.Err == nil {
		st.Err = fmt.Errorf("collections: only %d of %d segments can carry dictionary %d",
			st.Copies, st.Target, id)
	}
}

// segCarriesLocked reports whether a segment's trailer already holds a copy of id. The
// caller must hold the shard lock: the list is appended to under it.
func segCarriesLocked(seg *segment, id uint32) bool {
	for _, got := range seg.carriedDicts {
		if got == id {
			return true
		}
	}
	return false
}

// noteCarried records a copy just written, under the shard lock that guards the segment's
// mutable state.
func noteCarried(sh *shard, seg *segment, id uint32) {
	sh.mu.Lock()
	if !segCarriesLocked(seg, id) {
		seg.carriedDicts = append(seg.carriedDicts, id)
		sort.Slice(seg.carriedDicts, func(a, b int) bool {
			return seg.carriedDicts[a] < seg.carriedDicts[b]
		})
	}
	sh.mu.Unlock()
}
