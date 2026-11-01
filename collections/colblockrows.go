package collections

import "github.com/PelicanPlatform/classad/collections/wire"

// The per-block row fallback of the columnar aggregate scans.
//
// A block whose schema carries every field a scan needs can still fail to read one of them. The cold
// numeric stream holds every cold numeric column of the block, so when it will not decode none of
// those columns exist for any of the block's records. Leaving the block out drops its records from
// the answer with nothing to show for it, and treating an unread predicate column as passing keeps
// records nobody tested; both are wrong answers rather than slow right ones. The block's records are
// read instead the way a segment without a block is: one at a time from the record arena, reading
// only the attributes the scan needs.

// numColsReadable reports whether every numeric field among idxs reads from b. A hot field always
// does; a cold one needs the block's cold numeric stream to decode. One stream holds every cold
// field, so this is at most one decode -- the one the scan is about to pay anyway, and the cache
// keeps it for that read.
func (b *columnarBlock) numColsReadable(idxs []int, bc *blockCache) bool {
	for _, i := range idxs {
		if _, hot := b.hotColStart(i); hot {
			continue
		}
		if _, cold := b.coldFieldStart(i); !cold {
			return false
		}
		_, err := bc.stream(b, kindColdNum)
		return err == nil
	}
	return true
}

// eachBlockRecord calls fn with the wire form of each record of a block that is visible at s0 -- the
// block's records are cs's offsets [base, base+n) -- read from the record arena as the row walks read
// theirs. A record that will not read is skipped exactly as they skip it, and c.wire counts it as it
// counts theirs, so the two paths cannot disagree about it. fn returns false to give the query up.
//
// Each record handed over counts in ColumnarFallbacks: the columns could not answer for it.
func (c *Collection) eachBlockRecord(w segWindow, s0 uint64, cs *colSegment, base, n int, fn func(wire.Ad) bool) bool {
	var buf []byte
	d := w.dict()
	for k := 0; k < n; k++ {
		gk := base + k
		if gk >= cs.offsLen() {
			break
		}
		o := cs.offAt(gk)
		if !w.visible(o, s0) {
			continue
		}
		columnarFallbacks.Add(1)
		ww, err := c.wire(recRef{w: w, off: o, dict: d}, buf, sealedUnopened)
		if err != nil {
			continue
		}
		buf = ww
		if !fn(wire.Ad(ww)) {
			return false
		}
	}
	return true
}

// wirePredsMatch tests a record's predicated attributes from their wire nodes: the row fallbacks'
// form of the column narrowing. An absent or non-numeric value leaves the comparison UNDEFINED, so the
// record does not match. ok=false when a value is an expression, which only evaluation can compare.
func wirePredsMatch(d *segDictHandle, ad wire.Ad, preds []fieldPred, lookups []fieldLookup) (match, ok bool) {
	for i := range preds {
		node, found := lookups[i](d, ad)
		if !found {
			return false, true
		}
		if isExprNode(node) {
			return false, false
		}
		nv, isNum := nodeColVal(node)
		if !isNum || !preds[i].eval(nv.f) {
			return false, true
		}
	}
	return true, true
}
