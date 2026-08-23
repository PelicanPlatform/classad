package collections

import (
	"bytes"
	"iter"

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
// The cost of a page is one forward skip inside the segment straddling the
// cursor, plus a merge across the segments above it; segments that end before
// the cursor are skipped whole via maxSeq.

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

// after reports whether a record at (seq, key) sorts strictly after the cursor.
func (c SeqCursor) after(seq uint64, key []byte) bool {
	if seq != c.Seq {
		return seq > c.Seq
	}
	return bytes.Compare(key, []byte(c.Key)) > 0
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
type seqRun struct {
	w    segWindow
	off  uint32
	dict *segDictHandle
	// cur is the record the run is positioned at, valid while ok.
	cur recRef
	seq uint64
	key []byte
	ok  bool
}

// advance moves the run to its next record visible at s0 and strictly after the
// cursor, leaving ok false when the run is exhausted.
func (r *seqRun) advance(s0 uint64, after SeqCursor) {
	for int(r.off) < r.w.used {
		o := r.off
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
		key := recKey(r.w.data, o)
		if !after.after(seq, key) {
			continue // at or before the cursor
		}
		r.cur, r.seq, r.key, r.ok = recRef{w: r.w, off: o, dict: r.dict}, seq, key, true
		return
	}
	r.ok = false
}

// QueryRawFromSeq yields the records matching q that sort after the cursor, in
// (shard, seq, key) order, at most limit of them. A nil query matches
// everything. It is the paginated form of QueryRaw: successive calls, each
// handed the previous page's Next cursor, visit every record exactly once.
//
// The zero cursor starts at the beginning. Each shard is frozen at the sequence
// it had when its first page was taken, and is finished before the next shard
// is opened — so a record written during pagination is invisible if its shard
// had already started, and visible if it had not. That is the guarantee Scan
// already makes, which is likewise per shard.
//
// limit <= 0 means no limit, in which case More is always false.
func (c *Collection) QueryRawFromSeq(q *vm.Query, after SeqCursor, limit int) (iter.Seq[RawAd], *SeqPage) {
	page := &SeqPage{Next: after}

	return func(yield func(RawAd) bool) {
		ws := &wireScope{ctx: c}
		qp := queryPlan{ws: ws, resolver: ws.resolve}
		if q != nil {
			plan := q.ReadPlan()
			qp.q, qp.plan, qp.m = q, plan, q.Matcher()
			qp.wireOK = q.Native() && plan.PartialSafe
		}
		emit := c.yieldRaw(yield, false)

		var dbuf []byte
		emitted := 0

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

			runs, release := shardRuns(sh, s0, cursor)
			done := func() bool {
				defer release()
				for {
					pick := pickRun(runs)
					if pick == nil {
						return false // shard exhausted; fall through to the next
					}
					if limit > 0 && emitted == limit {
						page.More = true
						return true
					}

					r := pick.cur
					seq, key := pick.seq, append([]byte(nil), pick.key...)
					pick.advance(s0, cursor)

					if isSystemKeyBytes(r.key()) {
						continue // internal system record, hidden as in Scan
					}
					// The record's full ad — for a columnarized segment, its
					// own bytes spliced with the segment's columnar payload.
					w, err := c.wire(r, dbuf)
					if err != nil {
						continue // undecodable record: skip rather than abort
					}
					dbuf = w
					ws.dict = r.dict
					if q != nil && !matchWire(w, qp) {
						continue
					}
					page.Next = SeqCursor{
						Shard:    uint32(si),
						Snapshot: s0,
						Seq:      seq,
						Key:      string(key),
					}
					emitted++
					if !emit(w, r.dict) {
						return true // the consumer stopped
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

// shardRuns opens one run per segment of sh holding records visible at s0 above
// the cursor, and returns a release for their pins.
func shardRuns(sh *shard, s0 uint64, after SeqCursor) ([]*seqRun, func()) {
	var runs []*seqRun
	for _, w := range sh.snapshotAt(s0) {
		// A segment whose newest record predates the cursor holds nothing this
		// page can want. maxSeq is 0 on a segment whose records were not walked
		// (fast reopen), which is never skipped, as with minSeq.
		if w.seg != nil && w.seg.maxSeq != 0 && w.seg.maxSeq < after.Seq {
			releaseWindows([]segWindow{w})
			continue
		}
		run := &seqRun{w: w, dict: w.dict()}
		run.advance(s0, after)
		runs = append(runs, run)
	}
	return runs, func() {
		for _, r := range runs {
			releaseWindows([]segWindow{r.w})
		}
	}
}

// pickRun returns the run holding the smallest (seq, key) still to be emitted,
// or nil when every run is exhausted.
func pickRun(runs []*seqRun) *seqRun {
	var pick *seqRun
	for _, r := range runs {
		if !r.ok {
			continue
		}
		if pick == nil || r.seq < pick.seq ||
			(r.seq == pick.seq && bytes.Compare(r.key, pick.key) < 0) {
			pick = r
		}
	}
	return pick
}
