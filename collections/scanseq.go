package collections

import (
	"bytes"
	"iter"
	"sort"

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

// advance moves the run to its next record visible at s0 whose sequence is at
// least minSeq, leaving ok false when the run is exhausted.
//
// It deliberately does NOT filter on the cursor's key. Records committed
// together share a sequence and sit in the arena in insertion order, not key
// order, so a record belonging after the cursor can sit before one belonging
// before it. The key comparison happens once a sequence's whole group has been
// collected and sorted.
func (r *seqRun) advance(s0, minSeq uint64) {
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
// limit <= 0 means no limit, in which case More is always false.
func (c *Collection) QueryRawFromSeq(q *vm.Query, projection []string, after SeqCursor, limit int) (iter.Seq[RawAd], *SeqPage) {
	page := &SeqPage{Next: after}

	return func(yield func(RawAd) bool) {
		ws := &wireScope{ctx: c}
		qp := queryPlan{ws: ws, resolver: ws.resolve}
		if q != nil {
			plan := q.ReadPlan()
			qp.q, qp.plan, qp.m = q, plan, q.Matcher()
			qp.wireOK = q.Native() && plan.PartialSafe
		}
		// Same in-walk projection as QueryRawProjected: an empty projection
		// means the whole ad, so a caller asking for four attributes does not
		// pay to render the rest.
		emit := c.yieldRawProjected(yield, c.newRawProjector(projection, false, false))

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
						if gseq == cursor.Seq && cursor.Key != "" && bytes.Compare(g.key, []byte(cursor.Key)) <= 0 {
							continue
						}
						if limit > 0 && emitted == limit {
							page.More = true
							return true
						}
						if isSystemKeyBytes(g.key) {
							continue // internal system record, hidden as in Scan
						}
						// The record's full ad — for a columnarized segment,
						// its own bytes spliced with the columnar payload.
						w, err := c.wire(g.ref, dbuf)
						if err != nil {
							continue // undecodable record: skip, do not abort
						}
						dbuf = w
						ws.dict = g.ref.dict
						if q != nil && !matchWire(w, qp) {
							continue
						}
						page.Next = SeqCursor{
							Shard:    uint32(si),
							Snapshot: s0,
							Seq:      gseq,
							Key:      string(g.key),
						}
						emitted++
						if !emit(w, g.ref.dict) {
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
		run.advance(s0, after.Seq)
		runs = append(runs, run)
	}
	return runs, func() {
		for _, r := range runs {
			releaseWindows([]segWindow{r.w})
		}
	}
}
