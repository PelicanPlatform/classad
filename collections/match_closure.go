package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Closure-decode: evaluate a wide slot ad's match without building the whole ClassAd.
// OSPool-style slot ads carry hundreds of attributes but the match (Requirements,
// and the job's Rank) references only a small transitive closure. Rather than
// FromAST the entire ad per candidate, closureDecode builds a partial ClassAd holding
// just that closure, evaluates the bilateral match + rank on it, and lets the caller
// defer full materialization to the returned top-N.
//
// It is cache-free and correct per ad (the closure is computed from this ad's own
// expressions): pass 1 indexes id -> node bytes in one ForEach (no AST build); a BFS
// from the seed roots then AST-builds only the closure via O(1) map lookups -- not
// the scattered O(N) Ad.Lookup scans that make a naive partial decode slower than a
// full one. See the spike (ospool_spike_test.go) for the measurements.

// closureDecodeMinAttrs gates the strategy: closure decode only pays when the ad is
// much wider than its match closure. Narrow ads (the common case) stay on the plain
// full decode, which is cache-friendlier than building the id->node index. A var so a
// benchmark can A/B it; not a tuned knob.
var closureDecodeMinAttrs = 64

// rankSlotRefs returns the slot attribute names the job's Rank reads (its TARGET
// references), by rewriting Rank into the slot's frame (TARGET.x -> self-scoped x,
// job refs baked to constants) and collecting the self-scoped names. These must join
// the closure seed so Rank evaluates the same on the partial ad as on the full ad.
func rankSlotRefs(job *classad.ClassAd, jobVals map[string]classad.Value) []string {
	rankExpr := jobRankExpr(job)
	if rankExpr == nil {
		return nil
	}
	return vm.SelfRefs(rewriteForSlot(rankExpr, jobVals))
}

// closureDecode builds a partial ClassAd containing the transitive closure of the
// worker's match seeds (Requirements + the job Rank's slot refs) for the slot in w,
// or nil if the collection is not interned (the closure walk needs interned ids) or
// the seeds resolve to nothing. Reuses the worker's node index and visited set.
func (c *Collection) closureDecode(w []byte, mw *matchWorker) *classad.ClassAd {
	if c.inline || c.intern == nil {
		return nil
	}
	a := wire.Ad(w)
	if mw.nodeIdx == nil {
		mw.nodeIdx = make(map[uint32][]byte, 600)
		mw.seenIDs = make(map[uint32]bool, 64)
	}
	clear(mw.nodeIdx)
	a.ForEach(func(id uint32, node []byte) bool {
		mw.nodeIdx[id] = node
		return true
	})
	clear(mw.seenIDs)
	work := mw.closureWork[:0]
	for _, name := range mw.closureSeeds {
		if id, ok := c.intern.LookupID(name); ok {
			work = append(work, id)
		}
	}
	out := classad.New()
	for len(work) > 0 {
		id := work[len(work)-1]
		work = work[:len(work)-1]
		if mw.seenIDs[id] {
			continue
		}
		mw.seenIDs[id] = true
		node, ok := mw.nodeIdx[id]
		if !ok {
			continue
		}
		expr, err := c.decodeNode(node)
		if err != nil {
			continue
		}
		if name, ok := c.intern.Name(id); ok {
			out.Insert(name, expr)
		}
		refs, safe := vm.SelfRefsSafe(expr)
		if !safe {
			// eval() in a closure attribute: its refs are not statically known, so the
			// partial ad could be incomplete. Abandon closure decode; the caller full-
			// decodes for an exact result.
			mw.closureWork = work[:0]
			return nil
		}
		for _, ref := range refs {
			if rid, ok := c.intern.LookupID(ref); ok && !mw.seenIDs[rid] {
				work = append(work, rid)
			}
		}
	}
	mw.closureWork = work[:0]
	return out
}

// wantsClosureDecode reports whether the slot is wide enough that closure decode
// beats a full decode. The seeds must be non-empty (a job with no wire-native
// Requirements never reaches the deferred survivor path anyway).
func (mw *matchWorker) wantsClosureDecode(w []byte) bool {
	return len(mw.closureSeeds) > 0 && wire.Ad(w).AttrCount() > closureDecodeMinAttrs
}

// buildClosureSeeds is the seed set for a match's closure: the Requirements root plus
// the job Rank's slot references.
func buildClosureSeeds(rankRefs []string) []string {
	seeds := make([]string, 0, 1+len(rankRefs))
	seeds = append(seeds, "Requirements")
	for _, r := range rankRefs {
		if !strings.EqualFold(r, "Requirements") {
			seeds = append(seeds, r)
		}
	}
	return seeds
}
