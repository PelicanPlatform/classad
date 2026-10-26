package collections

import "github.com/RoaringBitmap/roaring/v2"

// INDEX CANDIDATES AND DELTA RECORDS.
//
// A delta record holds only the attributes one write changed; the rest of its ad is inherited
// from the key's earlier versions. The segment index posts each record from its own bytes, so a
// delta that did not touch an indexed attribute posts nothing for it -- and an indexed read, which
// visits only posted offsets, never reaches the key's current version. `ClusterId >= 0` over a
// store whose keys had been patched since their last whole record returned only the keys that had
// not been.
//
// The index cannot post what a delta inherits: those values live in other records, often in other
// segments, and the version chain is what joins them. So the delta records are added to every
// candidate set instead, like the exception set: a superset the caller re-verifies against the
// merged ad. They are read from the record headers when a query first asks rather than recorded in
// the index, so an index written before this -- a sealed segment's sidecar -- is corrected without
// being rebuilt.

// segDeltas is the set of delta records in a segment's first upto bytes.
type segDeltas struct {
	upto uint32
	bm   *roaring.Bitmap // nil: no delta record there
}

// deltaCandidates returns the offsets of the delta records in w's first upto bytes -- the index's
// covered prefix -- or nil when there are none. It is nil without a walk for a collection that has
// never stored a delta.
//
// The returned bitmap is shared; callers Or it into their own and never modify it.
//
// Cached on the segment. A sealed segment's prefix never changes, and an active one's covered
// prefix only grows when its index is rebuilt, which changes upto and so misses the cache.
func (c *Collection) deltaCandidates(w segWindow, upto uint32) *roaring.Bitmap {
	if !c.deltaRead || w.seg == nil || upto == 0 {
		return nil
	}
	if d := w.seg.deltas.Load(); d != nil && d.upto == upto {
		return d.bm
	}
	var bm *roaring.Bitmap
	for off := uint32(0); off < upto; {
		if end, ok := w.dmg.skip(off); ok {
			off = end
			continue
		}
		total := recTotalLen(w.data, off)
		if total == 0 {
			break
		}
		if !recIsMarker(w.data, off) && recIsDelta(w.data, off) {
			if bm == nil {
				bm = roaring.New()
			}
			bm.Add(off)
		}
		off += total
	}
	w.seg.deltas.Store(&segDeltas{upto: upto, bm: bm})
	return bm
}

// deltaScope decides, for one shard at one snapshot and one conjunction of index probes, which
// delta records of each window's covered prefix the index cannot rule out.
//
// Admitting every delta is sound but can be most of the work: a key patched since the last seal has
// its current version as a delta in the active segment, and a queue mirror patches most of its
// keys, so `ClusterId == X` would rebuild every one of them to find one job. The scope narrows that
// with what the index CAN say about a delta:
//
//   - a delta visible at the snapshot is the only kind a read can return, so the rest are dropped;
//   - for a probe on an attribute the delta carries, the delta's own posting decides -- it is the
//     value the delta's ad has, so the ordinary candidate set already includes it or rightly not;
//   - for one it does not carry, the value is inherited from the newest earlier version of the
//     same key that does, and that version is a record the index posted too. So the delta stays a
//     candidate only if some record of its key, anywhere in the shard, is a candidate for the
//     probe: a superset, since any record of the key will do.
//
// The last step is sound only when every record of the shard is under an index covering the
// probes, so that "no posting" means "no such value". A shard with an unindexed segment, a sealed
// segment indexed short of its end, or -- for probes that fold CurrentTime -- a record not proven
// free of it, admits every visible delta instead. The active segment's uncovered tail does not
// count: it is newer than every covered record, so nothing covered inherits from it, and it is
// walked in full anyway.
type deltaScope struct {
	// per maps a window's segment to its delta candidates; a window with none is absent.
	per map[*segment]*roaring.Bitmap
}

// newDeltaScope builds the scope for the windows wins of sh, visible at s0. It returns nil when no
// window the index serves holds a visible delta in its covered prefix, which is the whole of an
// ordinary store.
func (c *Collection) newDeltaScope(sh *shard, s0 uint64, wins []segWindow, usable []usableProbe, ct bool) *deltaScope {
	if !c.deltaRead || len(usable) == 0 {
		return nil
	}
	// The visible deltas of each served window's covered prefix.
	type served struct {
		w      segWindow
		si     readIndex
		deltas *roaring.Bitmap
	}
	var todo []served
	for _, w := range wins {
		si := windowIndex(w, usable, ct)
		if si == nil {
			continue // walked in full: its deltas are visited like every other record
		}
		all := c.deltaCandidates(w, si.coveredUpto())
		if all == nil {
			continue
		}
		vis := roaring.New()
		it := all.Iterator()
		for it.HasNext() {
			if o := it.Next(); w.visible(o, s0) {
				vis.Add(o)
			}
		}
		if !vis.IsEmpty() {
			todo = append(todo, served{w: w, si: si, deltas: vis})
		}
	}
	if len(todo) == 0 {
		return nil
	}
	ds := &deltaScope{per: make(map[*segment]*roaring.Bitmap, len(todo))}
	admitAll := func() *deltaScope {
		for _, t := range todo {
			ds.per[t.w.seg] = t.deltas
		}
		return ds
	}

	// Every segment of the shard, visible at s0 or not: a superseded base lives in a segment a
	// scan at s0 may have no window on, and its posting is what a delta inherits.
	sh.mu.RLock()
	act := sh.act
	if ct && !sh.ctShardProvenLocked() {
		sh.mu.RUnlock()
		return admitAll()
	}
	// Captured under the lock as windows: the active segment's used moves with every write.
	var segs []segWindow
	for _, seg := range sh.segs {
		if seg == nil || seg.used == 0 {
			continue
		}
		si := seg.readIdx()
		if si == nil || !si.covers(usable) || (seg != act && int(si.coveredUpto()) < seg.used) {
			sh.mu.RUnlock()
			releaseWindows(segs)
			return admitAll()
		}
		seg.pin()
		segs = append(segs, segWindow{data: seg.data, used: seg.used, seg: seg, dmg: seg.damaged()})
	}
	sh.mu.RUnlock()
	defer releaseWindows(segs)

	// keep[i] is the subset of todo[i].deltas still admitted; each probe narrows it.
	keep := make([]*roaring.Bitmap, len(todo))
	for i, t := range todo {
		keep[i] = t.deltas.Clone()
	}
	for _, up := range usable {
		single := []usableProbe{up}
		present := []usableProbe{{attrID: up.attrID, cat: up.cat, op: "present"}}
		// Deltas that do not carry the probe's attribute and are not candidates on their own:
		// they inherit it, and are decided by their key below.
		inherit := make([]*roaring.Bitmap, len(todo))
		need := map[string]struct{}{}
		for i, t := range todo {
			own := t.si.candidateOffsets(single)
			carry := t.si.candidateOffsets(present)
			inh := keep[i].Clone()
			if own != nil {
				inh.AndNot(own)
			}
			if carry != nil {
				// A delta carrying the attribute and not posted under a matching value does
				// not match: its own value is its ad's.
				fails := keep[i].Clone()
				fails.And(carry)
				if own != nil {
					fails.AndNot(own)
				}
				keep[i].AndNot(fails)
				inh.AndNot(carry)
			}
			inherit[i] = inh
			it := inh.Iterator()
			for it.HasNext() {
				need[string(recKey(t.w.data, it.Next()))] = struct{}{}
			}
		}
		if len(need) == 0 {
			continue
		}
		// The keys among them with some record, in any segment, that the probe admits.
		found := make(map[string]struct{}, len(need))
		for _, sw := range segs {
			if len(found) == len(need) {
				break
			}
			si := sw.seg.readIdx()
			if si == nil || si.skipsPrefix(single) {
				continue
			}
			cand := si.candidateOffsets(single)
			if cand == nil {
				continue
			}
			it := cand.Iterator()
			for it.HasNext() {
				o := it.Next()
				if int(o) >= sw.used {
					break
				}
				if (sw.dmg != nil && sw.dmg.holds(o)) || recIsMarker(sw.data, o) {
					continue
				}
				k := recKey(sw.data, o)
				if _, ok := need[string(k)]; ok {
					found[string(k)] = struct{}{}
				}
			}
		}
		for i, t := range todo {
			drop := roaring.New()
			it := inherit[i].Iterator()
			for it.HasNext() {
				o := it.Next()
				if _, ok := found[string(recKey(t.w.data, o))]; !ok {
					drop.Add(o)
				}
			}
			keep[i].AndNot(drop)
		}
	}
	for i, t := range todo {
		if !keep[i].IsEmpty() {
			ds.per[t.w.seg] = keep[i]
		}
	}
	return ds
}

// withDeltas returns cand with the delta records of w's covered prefix that ds admits added. cand
// may be nil (no posted candidate); the result is nil only when both are empty. A nil ds admits
// none.
func (ds *deltaScope) withDeltas(w segWindow, cand *roaring.Bitmap) *roaring.Bitmap {
	if ds == nil {
		return cand
	}
	d := ds.per[w.seg]
	if d == nil {
		return cand
	}
	if cand == nil {
		return d.Clone()
	}
	cand.Or(d)
	return cand
}
