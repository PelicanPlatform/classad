package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// astClosure returns the folded attribute names in the transitive self-reference
// closure of roots within ad (from the AST, no wire access), and whether that closure
// is statically complete: safe is false if any closure attribute calls eval(), whose
// referenced attributes cannot be determined statically. Used at encode time to front-
// load the match closure into the hot header and decide flagHotClosure.
func astClosure(ad *ast.ClassAd, roots []string) (names []string, safe bool) {
	byName := make(map[string]ast.Expr, len(ad.Attributes))
	for _, a := range ad.Attributes {
		byName[strings.ToLower(a.Name)] = a.Value
	}
	safe = true
	seen := make(map[string]bool, len(roots)+8)
	work := make([]string, 0, len(roots))
	for _, r := range roots {
		work = append(work, strings.ToLower(r))
	}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		if seen[name] {
			continue
		}
		seen[name] = true
		expr, ok := byName[name]
		if !ok {
			continue // referenced but absent from the ad: legitimately not in the closure
		}
		names = append(names, name)
		refs, s := vm.SelfRefsSafe(expr)
		if !s {
			safe = false
		}
		for _, ref := range refs {
			work = append(work, strings.ToLower(ref))
		}
	}
	return names, safe
}

// hotSetForEncode returns the hot id set to front-load for ad and whether the ad
// should be flagged as carrying a complete match closure. With no match roots
// configured it returns the shared frequency set unchanged (no per-ad allocation);
// otherwise it unions the ad's match closure into a fresh set.
func (c *Collection) hotSetForEncode(ad *ast.ClassAd) (hot map[uint32]struct{}, closureComplete bool) {
	base := c.currentHotSet()
	if len(c.matchRoots) == 0 {
		return base, false
	}
	names, safe := astClosure(ad, c.matchRoots)
	if len(names) == 0 {
		return base, false
	}
	union := make(map[uint32]struct{}, len(base)+len(names))
	for id := range base {
		union[id] = struct{}{}
	}
	for _, name := range names {
		union[c.intern.Intern(name)] = struct{}{}
	}
	return union, safe
}

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

// hotClosureMatch reads a wide slot ad's match closure straight from its hot header
// (when the ad is flagged closure-complete), building a partial ClassAd for the
// bilateral eval with no body scan -- the fast path. Returns nil to fall back (ad not
// flagged, not interned, or a rank ref uses eval()). Job Rank slot refs outside the
// Requirements closure are topped up by direct lookup (rank refs are few), so rank
// evaluates the same as on the full ad.
func (c *Collection) hotClosureMatch(w []byte, mw *matchWorker) *classad.ClassAd {
	if c.inline || c.intern == nil {
		return nil
	}
	a := wire.Ad(w)
	if !a.HotClosureComplete() {
		return nil
	}
	if mw.hotPresent == nil {
		mw.hotPresent = make(map[string]bool, 64)
	}
	clear(mw.hotPresent)
	partial := classad.New()
	a.ForEachHot(func(id uint32, node []byte) bool {
		if expr, err := c.decodeNode(node); err == nil {
			if name, ok := c.intern.Name(id); ok {
				partial.Insert(name, expr)
				mw.hotPresent[strings.ToLower(name)] = true
			}
		}
		return true
	})
	for _, ref := range mw.rankRefs {
		if !c.topUpRef(a, partial, mw.hotPresent, ref) {
			return nil // eval() in a rank ref's closure: cannot trust a partial ad
		}
	}
	return partial
}

// topUpRef adds root and its transitive self-closure to partial from the full ad's
// wire bytes, skipping anything already present. It is for the few job Rank slot refs
// not covered by the Requirements closure. Returns false if a ref uses eval().
func (c *Collection) topUpRef(a wire.Ad, partial *classad.ClassAd, present map[string]bool, root string) bool {
	work := []string{strings.ToLower(root)}
	for len(work) > 0 {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		if present[name] {
			continue
		}
		present[name] = true
		id, ok := c.intern.LookupID(name)
		if !ok {
			continue
		}
		node, ok := a.Lookup(id)
		if !ok {
			continue // absent from the ad: legitimately undefined
		}
		expr, err := c.decodeNode(node)
		if err != nil {
			continue
		}
		if cname, ok := c.intern.Name(id); ok {
			partial.Insert(cname, expr)
		}
		refs, safe := vm.SelfRefsSafe(expr)
		if !safe {
			return false
		}
		for _, r := range refs {
			work = append(work, strings.ToLower(r))
		}
	}
	return true
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
