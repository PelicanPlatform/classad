package collections

import (
	"bytes"
	"iter"
	"slices"
	"sort"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// Resume-from-sequence scanning: a paginated read whose cursor is a commit
// sequence rather than a physical position.
//
// The store has no key order, and adding one would mean a whole ordered index
// over the mutable arena. It does have an order already, though, one it
// maintains for MVCC: every record carries the sequence it was born at, and
// records are appended in commit order, so each segment is a sorted run by
// sequence. At a fixed snapshot S0 each key has exactly one visible version, so
// (seq, key) is a total order over what a scan at S0 can see — enough to say
// "continue after here" without naming a physical location.
//
// Naming a physical location is what a cursor must not do. A segment's id is its
// index in the shard's slice: compaction renumbers ids and the reopen path
// assigns them by sort position, so ids are reused, and a stale (segment,
// offset) pair would silently address a different record rather than fail. A
// sequence survives all of that — compaction carries the stamps to the
// destination records because MVCC correctness depends on it.
//
// The cost of a page is a seek into each segment that may hold records after
// the cursor, plus a merge across them; segments that end before the cursor are
// skipped whole via maxSeq. The seek (seqSeek) is what keeps a deep page from
// walking every record header below the cursor again on every page.

// SeqCursor marks a position in a sequence-ordered scan: the last record the
// caller consumed. The zero value starts at the beginning.
//
// It is per shard, because the commit sequence is: each shard stamps records
// from its own counter (`seq := sh.commitSeq + 1`), so sequences from different
// shards are unrelated numbers and only order records within one shard. A scan
// therefore finishes a shard before moving to the next, and the cursor names
// which shard it is in — the same shape as Scan's guarantee, which is likewise
// per shard ("each key present at the moment A SHARD's scan begins").
//
// Snapshot is that shard's sequence when its first page was taken; carrying it
// forward is what keeps later pages from seeing writes that landed meanwhile.
// It is only meaningful together with Shard.
//
// Key breaks ties within a commit, where several records share a sequence. It
// is compared bytewise, and only against records with the same sequence, so it
// imposes no ordering of its own beyond making the position unambiguous.
type SeqCursor struct {
	Shard    uint32
	Snapshot uint64
	Seq      uint64
	Key      string
}

// SeqPage is one page of a sequence-ordered scan.
type SeqPage struct {
	// Next is where a following page resumes: hand it back unchanged. It
	// carries the shard and that shard's snapshot as well as the position.
	// Meaningful only when More is true.
	Next SeqCursor
	// More reports whether the scan stopped at the page limit with records
	// still to come, as opposed to having reached the end of the collection.
	More bool
}

// seqRun is one segment's records, positioned at the first one after the
// cursor. Records within a segment are appended in commit order, so a run is
// already sorted by sequence and is consumed front to back.
//
// A run whose segment index can serve the query visits only that index's
// candidates in the covered prefix -- offsets ascending, so still in commit
// order -- and then every record of the uncovered tail. Either way it yields a
// superset of the segment's matches, in the same order the whole walk would.
type seqRun struct {
	w    segWindow
	dict *segDictHandle
	// cand iterates the index's candidates in the covered prefix, nil once
	// exhausted or when the run walks its whole segment.
	cand roaring.IntPeekable
	// off is the next offset of the walk that follows the candidates: the
	// uncovered tail, or the whole segment for a run without an index.
	off uint32
	// cur is the record the run is positioned at, valid while ok.
	cur recRef
	seq uint64
	key []byte
	ok  bool
}

// advance moves the run to its next record visible at s0 whose sequence is at
// least minSeq, leaving ok false when the run is exhausted.
//
// It deliberately does NOT filter on the cursor's key. Records committed
// together share a sequence and sit in the arena in insertion order, not key
// order, so a record belonging after the cursor can sit before one belonging
// before it. The key comparison happens once a sequence's whole group has been
// collected and sorted.
func (r *seqRun) advance(s0, minSeq uint64) {
	for r.cand != nil {
		if !r.cand.HasNext() {
			r.cand = nil
			break
		}
		o := r.cand.Next()
		if int(o) >= r.w.used {
			// Beyond the window: an index built after this snapshot was taken can
			// cover records written since, none of which the snapshot can see.
			r.cand = nil
			break
		}
		if recIsMarker(r.w.data, o) || !r.w.visible(o, s0) {
			continue
		}
		if seq := recSeq(r.w.data, o); seq >= minSeq {
			r.cur, r.seq, r.key, r.ok = recRef{w: r.w, off: o, dict: r.dict}, seq, recKey(r.w.data, o), true
			return
		}
	}
	for int(r.off) < r.w.used {
		o := r.off
		if end, ok := r.w.dmg.skip(o); ok {
			r.off = end
			continue
		}
		total := recTotalLen(r.w.data, o)
		if total == 0 {
			break
		}
		r.off += total
		if recIsMarker(r.w.data, o) {
			continue
		}
		seq := recSeq(r.w.data, o)
		if seq > s0 || recSuperseded(r.w.data, o) <= s0 {
			continue // not visible at this snapshot
		}
		if seq < minSeq {
			continue // an earlier commit than the cursor's
		}
		r.cur, r.seq, r.key, r.ok = recRef{w: r.w, off: o, dict: r.dict}, seq, recKey(r.w.data, o), true
		return
	}
	r.ok = false
}

// QueryRawFromSeq yields the records matching q that sort after the cursor, in
// (shard, seq, key) order, at most limit of them, projected to the named
// attributes. A nil query matches everything; an empty projection keeps the
// whole ad. It is the paginated form of QueryRaw: successive calls, each
// handed the previous page's Next cursor, visit every record exactly once.
//
// The zero cursor starts at the beginning. Each shard is frozen at the sequence
// it had when its first page was taken, and is finished before the next shard
// is opened — so a record written during pagination is invisible if its shard
// had already started, and visible if it had not. That is the guarantee Scan
// already makes, which is likewise per shard.
//
// It reads the way a query does, only in a different order: a segment index
// that can serve the constraint narrows each segment to its candidates, the
// columns are asked before a record is rebuilt, and a projection the columns
// can serve is built from them (refVisitor). None of that changes which records
// match or the order they come in -- the candidates of a segment are a subset of
// its records in their arena order -- so the pages are the ones the whole walk
// gives. More may differ: it is set when the page is full and another CANDIDATE
// follows, which is a record the walk would have reached too, so it still never
// reports false while a match remains.
//
// limit <= 0 means no limit, in which case More is always false.
func (c *Collection) QueryRawFromSeq(q *vm.Query, projection []string, after SeqCursor, limit int) (iter.Seq[RawAd], *SeqPage) {
	page := &SeqPage{Next: after}

	return func(yield func(RawAd) bool) {
		// Built by the one constructor, so this read takes the same match state,
		// read demand, CurrentTime fold and index plan as any other query.
		//
		// The projection plan lets a record the columns decided be built from the
		// named columns rather than reassembled. Safe here because this emit only
		// SHIPS the projected entries (chaseRefs is off): see colproject.go for the
		// consumers that evaluate and so must not narrow.
		//
		// It also names MyType and TargetType, which the raw projector lifts into
		// the RawAd from whatever ad it is handed, projected or not: an ad built
		// from columns without them would come back typeless.
		var proj *projPlan
		if len(projection) > 0 {
			proj = c.newProjPlan(append(slices.Clip(projection), rawTypeFieldNames...))
		}
		var (
			qp     queryPlan
			usable []usableProbe
		)
		if q != nil {
			qp = c.newQueryPlan(q, proj, nil)
			usable = c.planQuery(qp)
		} else {
			s := &wireScope{ctx: c}
			qp.ws, qp.resolver, qp.proj = s, s.resolve, proj
		}
		// Same in-walk projection as QueryRawProjected: an empty projection
		// means the whole ad, so a caller asking for four attributes does not
		// pay to render the rest.
		render := c.yieldRawProjected(yield, c.newRawProjector(projection, false, false))

		if unsatisfiable(usable) {
			// The constraint's indexed conjuncts contradict each other: nothing
			// anywhere can match, so no shard is worth opening.
			page.Next = SeqCursor{Shard: uint32(len(c.shards))}
			return
		}

		emitted := 0
		// The record being visited: the emit below stamps it into the cursor when
		// the visitor finds that it matches, so only a match pays for the key copy.
		var (
			atShard     uint32
			atS0, atSeq uint64
			atKey       []byte
		)
		emit := func(w []byte, dict *segDictHandle) bool {
			page.Next = SeqCursor{Shard: atShard, Snapshot: atS0, Seq: atSeq, Key: string(atKey)}
			emitted++
			return render(w, dict)
		}

		for si := int(after.Shard); si < len(c.shards); si++ {
			sh := c.shards[si]
			cursor := SeqCursor{Shard: uint32(si)}
			if si == int(after.Shard) {
				cursor = after
			}
			if cursor.Snapshot == 0 {
				// First page for this shard: freeze it here. Later pages carry
				// the same value back, so a write that lands meanwhile is
				// invisible to them — the per-shard form of what Scan
				// guarantees.
				sh.mu.RLock()
				cursor.Snapshot = sh.commitSeq
				sh.mu.RUnlock()
			}
			s0 := cursor.Snapshot

			runs, release := c.shardRuns(sh, s0, cursor, qp, usable)
			visit, stopped := c.refVisitor(s0, qp, emit)
			ckey := []byte(cursor.Key)
			done := func() bool {
				defer release()
				var group []groupRec
				for {
					group = nextGroup(runs, s0, cursor.Seq, group[:0])
					if len(group) == 0 {
						return false // shard exhausted; fall through to the next
					}
					gseq := group[0].seq
					for _, g := range group {
						// Within the cursor's own commit, skip what the last
						// page already emitted. Elsewhere the whole group is
						// new.
						if gseq == cursor.Seq && cursor.Key != "" && bytes.Compare(g.key, ckey) <= 0 {
							continue
						}
						if limit > 0 && emitted == limit {
							page.More = true
							return true
						}
						// System records are hidden by the visitor, as in Scan.
						atShard, atS0, atSeq, atKey = uint32(si), s0, gseq, g.key
						if !visit(g.ref) || stopped() {
							return true // the consumer stopped
						}
					}
				}
			}()
			if done {
				return
			}
			// Next shard starts fresh: a new snapshot, no position.
			page.Next = SeqCursor{Shard: uint32(si) + 1}
		}
	}, page
}

// groupRec is one record of a commit group, held while the group is sorted.
type groupRec struct {
	ref recRef
	seq uint64
	key []byte
}

// nextGroup collects every record carrying the lowest sequence still pending
// across the runs, sorted by key, and advances those runs past it. Appends into
// buf so the backing array is reused between groups.
//
// A group is one commit's records within one shard: they share a sequence, and
// the arena holds them in insertion order. Sorting them by key is what makes
// (seq, key) an order a cursor can resume from — without it, a page boundary
// inside a commit would skip whatever sorted below the last record emitted.
func nextGroup(runs []*seqRun, s0, minSeq uint64, buf []groupRec) []groupRec {
	gseq := uint64(0)
	found := false
	for _, r := range runs {
		if r.ok && (!found || r.seq < gseq) {
			gseq, found = r.seq, true
		}
	}
	if !found {
		return buf[:0]
	}
	for _, r := range runs {
		for r.ok && r.seq == gseq {
			buf = append(buf, groupRec{ref: r.cur, seq: r.seq, key: r.key})
			r.advance(s0, minSeq)
		}
	}
	sort.Slice(buf, func(i, j int) bool { return bytes.Compare(buf[i].key, buf[j].key) < 0 })
	return buf
}

// shardRuns opens one run per segment of sh holding records visible at s0 above
// the cursor, and returns a release for their pins.
//
// Sealed segments the query's zone maps rule out are dropped whole
// (zoneExcludes), and with usable index probes a segment whose index covers
// them is narrowed to its candidates (windowIndex, prefixCandidates) -- the
// decisions the query paths make (scanShardIndexed, scanWindows). Neither
// touches the cursor: a dropped segment or record holds no match, so the pages
// are the same, and its order within a segment is the arena's.
//
// A zone map summarizes each record's own bytes, which for a delta record lack
// what it inherits -- but only an append-only collection keeps zone maps, and
// it writes no delta records, so the two never meet.
func (c *Collection) shardRuns(sh *shard, s0 uint64, after SeqCursor, qp queryPlan, usable []usableProbe) ([]*seqRun, func()) {
	// Each segment's newest sequence, read under the lock that guards it: the
	// active segment's moves with every write.
	sh.mu.RLock()
	wins := sh.buildWindowsLocked(s0)
	maxSeqs := make([]uint64, len(wins))
	for i, w := range wins {
		maxSeqs[i] = w.seg.maxSeq
	}
	sh.mu.RUnlock()
	release := func() { releaseWindows(wins) }

	// A segment whose newest record predates the cursor holds nothing this page
	// can want. maxSeq is 0 on a segment whose records were not walked (fast
	// reopen), which is never skipped, as with minSeq; its seek (seekSeq) then
	// passes over it instead.
	walk := make([]segWindow, 0, len(wins))
	for i, w := range wins {
		if ms := maxSeqs[i]; ms == 0 || ms >= after.Seq {
			walk = append(walk, w)
		}
	}
	ct := qp.prunesCT()
	if len(qp.probes) > 0 && c.hasZones.Load() {
		kept := walk[:0]
		for _, w := range walk {
			if !c.zoneExcludes(w, qp.probes, ct) {
				kept = append(kept, w)
			}
		}
		walk = kept
	}
	ds := c.newDeltaScope(sh, s0, walk, usable, ct)
	runs := make([]*seqRun, 0, len(walk))
	for _, w := range walk {
		run := &seqRun{w: w, dict: w.dict()}
		// Every record before from has a sequence below the cursor's, so the walk would pass over
		// it: start there, and drop the candidates before it too.
		from := seekSeq(w, after.Seq)
		run.off = from
		if len(usable) > 0 {
			if ix := windowIndex(w, usable, ct); ix != nil {
				if cand := prefixCandidates(w, ix, usable, ds, nil); cand != nil {
					run.cand = cand.Iterator()
					run.cand.AdvanceIfNeeded(from)
				}
				run.off = max(ix.coveredUpto(), from)
			}
		}
		run.advance(s0, after.Seq)
		runs = append(runs, run)
	}
	return runs, release
}

// seqSeekBlock is how many records one seek checkpoint spans, which bounds how many records below
// the cursor a seek can leave the walk to pass over.
const seqSeekBlock = 32

// seqSeek lets a paged read start walking a segment near its cursor instead of at offset 0.
//
// The walk passes over every record whose sequence is below the cursor's. A page deep into a shard
// therefore re-reads, in every segment it opens, the headers of every record the earlier pages
// consumed -- and where a segment's maxSeq is unknown (zero after a reopen from the directory
// snapshot, which does not walk the records) it cannot skip even a segment that ends below the
// cursor. A page's cost then grows with its depth and the size of the shard rather than with the
// page.
//
// seqSeek records, for each block of seqSeekBlock records, where the block starts and the highest
// sequence of any record from the segment's start through the end of the block. That running
// maximum never decreases, so a binary search finds the first block that can hold a record at or
// past the cursor, and every record before it is one the walk would have skipped. Nothing here
// assumes a segment is in sequence order: a running maximum is sound over any order, it only skips
// less over a segment that is not.
//
// It is built from bytes that never change once written: a record's sequence and length are set at
// append, and a segment only grows. Each version is immutable once published; a reader that finds
// the segment has grown past it extends a copy.
type seqSeek struct {
	offs []uint32 // the offset of each block's first record
	maxs []uint64 // the highest sequence of any record from offset 0 through the end of each block
	upto uint32   // the offset just past the last record covered
	last int      // records in the last block, which an extension fills before opening another
}

// seekSeq returns the offset at which a walk of w for records with sequence >= minSeq may start:
// every record before it has a lower sequence. It is an offset the walk from 0 itself reaches, or
// one at or past w.used when no record of w can qualify. It is 0 when there is nothing to skip.
//
// A window with damage walks from the start: the walk steps over damaged spans the framing does
// not describe, so the checkpoints, which follow the framing, would not be offsets it reaches.
func seekSeq(w segWindow, minSeq uint64) uint32 {
	if minSeq == 0 || w.seg == nil || w.dmg != nil {
		return 0
	}
	ss := w.seg.seqSeek.Load()
	if ss == nil || int(ss.upto) < w.used {
		ss = ss.extend(w)
		for {
			cur := w.seg.seqSeek.Load()
			if (cur != nil && cur.upto >= ss.upto) || w.seg.seqSeek.CompareAndSwap(cur, ss) {
				break
			}
		}
	}
	// A version extended by a reader whose window reached further than w's still holds for w: its
	// first len(offs) blocks are w's records plus later ones, so the block it picks starts at or
	// before the first record of w that qualifies, or at or past w.used when none does.
	i := sort.Search(len(ss.maxs), func(i int) bool { return ss.maxs[i] >= minSeq })
	if i == len(ss.maxs) {
		return ss.upto
	}
	return ss.offs[i]
}

// extend returns a copy of ss (nil: an empty one) covering w's records up to w.used, following the
// same framing as the walk. It stops short at a record the walk would stop at or end on -- a zero
// length, or one running past w.used -- leaving that record to the walk.
func (ss *seqSeek) extend(w segWindow) *seqSeek {
	n := &seqSeek{}
	if ss != nil {
		n.offs, n.maxs, n.upto, n.last = slices.Clone(ss.offs), slices.Clone(ss.maxs), ss.upto, ss.last
	}
	o := n.upto
	for int(o) < w.used {
		total := recTotalLen(w.data, o)
		if total == 0 || int(o)+int(total) > w.used {
			break
		}
		seq := recSeq(w.data, o)
		if k := len(n.maxs); k == 0 || n.last == seqSeekBlock {
			if k > 0 {
				seq = max(seq, n.maxs[k-1])
			}
			n.offs, n.maxs, n.last = append(n.offs, o), append(n.maxs, seq), 1
		} else {
			n.maxs[k-1] = max(n.maxs[k-1], seq)
			n.last++
		}
		o += total
	}
	n.upto = o
	return n
}
