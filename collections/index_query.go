package collections

import (
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Reindex (re)builds the per-segment value/categorical indexes for every live
// segment, covering all records written so far. It reads only immutable segment
// bytes, so it runs off the write path and does not block writers or compaction.
// Call it on whatever schedule you like: queries use whatever coverage exists and
// full-scan the rest, so Reindex only affects query speed, never results.
//
// Reindex also reconciles segments with the current index configuration: a segment
// indexed before an AddIndex is rebuilt so the new attribute is backfilled, and one
// indexed before a DropIndex is rebuilt (or, if nothing is indexed anymore, its
// index is dropped) so the removed attribute's postings are reclaimed. A whole
// span of segments therefore evolves toward the current spec at whatever cadence
// the caller reindexes — no write-path or compaction coupling.
func (c *Collection) Reindex() {
	spec := c.spec.Load()
	for _, sh := range c.shards {
		// Snapshot segments + watermarks under the read lock, then build off-lock.
		sh.mu.RLock()
		type target struct {
			seg  *segment
			used int
		}
		var tgts []target
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			cur := seg.idx.Load()
			if !spec.any() {
				if cur != nil {
					tgts = append(tgts, target{seg, seg.used}) // clear a now-orphaned index
				}
				continue
			}
			if seg.used == 0 {
				continue
			}
			// Rebuild when the index is missing, behind the write watermark, or built
			// under an older spec generation (an add/drop happened since).
			if cur == nil || int(cur.upto) < seg.used || cur.specGen != spec.gen {
				tgts = append(tgts, target{seg, seg.used})
			}
		}
		sh.mu.RUnlock()
		for _, t := range tgts {
			if !spec.any() {
				t.seg.idx.Store(nil)
				continue
			}
			t.seg.idx.Store(buildSegIndex(t.seg.data, t.used, t.seg.codec, spec))
		}
	}
}

// usableProbe is a query Probe matched to a configured index (interned attr id,
// normalized value type). The store builds candidate offset bitmaps from these;
// the full query is re-verified per candidate, so any probe omitted only costs
// selectivity.
type usableProbe struct {
	attrID uint32
	cat    bool
	op     string
	svals  []string
	fvals  []float64
}

// planIndex matches the query's probes against the configured indexes. Empty means
// no index-usable constraint (the store full-scans).
func (c *Collection) planIndex(probes []vm.Probe) []usableProbe {
	spec := c.spec.Load()
	if !spec.any() {
		return nil
	}
	var out []usableProbe
	for _, p := range probes {
		var id uint32
		var ok bool
		if spec.inline {
			id, ok = spec.nameToID[strings.ToLower(p.Attr)]
		} else {
			id, ok = c.intern.LookupID(p.Attr)
		}
		if !ok {
			continue
		}
		if _, isCat := spec.cat[id]; isCat {
			if up, ok := catUsable(id, p); ok {
				out = append(out, up)
			}
			continue
		}
		if _, isVal := spec.val[id]; isVal {
			if up, ok := valUsable(id, p); ok {
				out = append(out, up)
			}
		}
	}
	return out
}

func catUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	switch p.Op {
	case "==", "!=", "in":
	default:
		return usableProbe{}, false // ranges are not indexed for categoricals
	}
	svals := make([]string, 0, len(p.Vals))
	for _, v := range p.Vals {
		s, err := v.StringValue()
		if err != nil {
			return usableProbe{}, false
		}
		svals = append(svals, strings.ToLower(s)) // fold to match the index key
	}
	return usableProbe{attrID: id, cat: true, op: p.Op, svals: svals}, true
}

func valUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	switch p.Op {
	case "==", "!=", "in", "<", "<=", ">", ">=":
	default:
		return usableProbe{}, false
	}
	fvals := make([]float64, 0, len(p.Vals))
	for _, v := range p.Vals {
		f, ok := numericFloat(v)
		if !ok {
			return usableProbe{}, false
		}
		fvals = append(fvals, f)
	}
	return usableProbe{attrID: id, cat: false, op: p.Op, fvals: fvals}, true
}

func numericFloat(v classad.Value) (float64, bool) {
	if f, err := v.NumberValue(); err == nil {
		return f, true
	}
	if b, err := v.BoolValue(); err == nil {
		if b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// covers reports whether a segment index has postings for every usable probe's
// attribute (a segment indexed before an attribute was added would not).
func (si *segIndex) covers(usable []usableProbe) bool {
	for _, up := range usable {
		if up.cat {
			if si.cat[up.attrID] == nil {
				return false
			}
		} else if si.val[up.attrID] == nil {
			return false
		}
	}
	return true
}

// candidateOffsets returns the segment-record offsets satisfying every usable
// probe (a superset; the store re-verifies). Categorical probes are combined first
// (a map lookup each), then value/range. nil means "no candidates".
func (si *segIndex) candidateOffsets(usable []usableProbe) *roaring.Bitmap {
	var acc *roaring.Bitmap
	for pass := 0; pass < 2; pass++ {
		wantCat := pass == 0
		for _, up := range usable {
			if up.cat != wantCat {
				continue
			}
			bm := si.probeOffsets(up)
			if acc == nil {
				acc = bm
			} else {
				acc.And(bm)
			}
			if acc.IsEmpty() {
				return acc
			}
		}
	}
	return acc
}

// probeOffsets returns a fresh, mutable offset bitmap for one probe.
func (si *segIndex) probeOffsets(up usableProbe) *roaring.Bitmap {
	if up.cat {
		cp := si.cat[up.attrID]
		switch up.op {
		case "==", "in":
			bm := roaring.New()
			for _, s := range up.svals {
				if p := cp.post[s]; p != nil {
					bm.Or(p)
				}
			}
			bm.Or(cp.exc)
			return bm
		case "!=":
			bm := si.all.Clone()
			if p := cp.post[up.svals[0]]; p != nil {
				bm.AndNot(p)
			}
			return bm
		}
		return roaring.New()
	}
	vp := si.val[up.attrID]
	switch up.op {
	case "==", "in":
		bm := roaring.New()
		for _, f := range up.fvals {
			if p := vp.post[f]; p != nil {
				bm.Or(p)
			}
		}
		bm.Or(vp.exc)
		return bm
	case "!=":
		bm := si.all.Clone()
		if p := vp.post[up.fvals[0]]; p != nil {
			bm.AndNot(p)
		}
		return bm
	case "<", "<=", ">", ">=":
		bm := roaring.New()
		t := up.fvals[0]
		for k, p := range vp.post {
			if cmpFloat(up.op, k, t) {
				bm.Or(p)
			}
		}
		bm.Or(vp.exc)
		return bm
	}
	return roaring.New()
}

func cmpFloat(op string, k, t float64) bool {
	switch op {
	case "<":
		return k < t
	case "<=":
		return k <= t
	case ">":
		return k > t
	case ">=":
		return k >= t
	}
	return false
}

// scanShardIndexed yields the shard's matching ads using each segment's index to
// visit only candidate records, and full-scanning any records the index does not
// cover (a segment with no index, or the tail beyond its build watermark). Every
// visited record is MVCC-visibility filtered and full-query re-verified, so the
// result is identical to a full scan. Returns false if the consumer stopped.
func (c *Collection) scanShardIndexed(sh *shard, usable []usableProbe, qp queryPlan, emit func(w []byte) bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	// visit tests one record's visibility, re-verifies, and hands its decompressed
	// wire bytes to emit; returns stop = true when the consumer asked to stop.
	visit := func(w segWindow, o uint32) (stop bool) {
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			return false
		}
		ww, err := w.codec.Decompress(dbuf[:0], recAd(w.data, o))
		if err != nil {
			return false
		}
		dbuf = ww
		if !matchWire(ww, qp) {
			return false
		}
		return !emit(ww)
	}
	// scanRange full-scans records in [from, to).
	scanRange := func(w segWindow, from, to int) bool {
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if visit(w, o) {
				return false
			}
			off += int(total)
		}
		return true
	}

	for _, w := range wins {
		si := w.seg.idx.Load()
		if si == nil || !si.covers(usable) {
			if !scanRange(w, 0, w.used) { // no usable index: full-scan the window
				return false
			}
			continue
		}
		// Indexed prefix [0, upto): visit only candidate offsets.
		if cand := si.candidateOffsets(usable); cand != nil {
			it := cand.Iterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
		// Tail [upto, used): written after the index was built — full-scan it.
		if int(si.upto) < w.used {
			if !scanRange(w, int(si.upto), w.used) {
				return false
			}
		}
	}
	return true
}
