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
	// A hot attribute that reads a target-dependent job attribute (TARGET.x, or an
	// unscoped x the slot may not define) needs that attribute's own slot reads too;
	// collect those references and resolve them once the hot set is known.
	jobRefs := mw.jobRefScratch[:0]
	a.ForEachHot(func(id uint32, node []byte) bool {
		if expr, err := c.decodeNode(node); err == nil {
			if name, ok := c.intern.Name(id); ok {
				partial.Insert(name, expr)
				mw.hotPresent[strings.ToLower(name)] = true
			}
			if len(mw.deps) > 0 {
				walkSlotRefs(expr, func(ref string, toJob bool) {
					if mw.depAttr(ref) != nil {
						jobRefs = append(jobRefs, slotJobRef{name: ref, toJob: toJob})
					}
				})
			}
		}
		return true
	})
	mw.jobRefScratch = jobRefs[:0]
	for _, r := range jobRefs {
		if !r.toJob && mw.hotPresent[strings.ToLower(r.name)] {
			continue // defined by the slot: the hot closure already holds it
		}
		if !c.topUpJobAttr(a, partial, mw, r.name) {
			return nil
		}
	}
	for _, ref := range mw.rankRefs {
		if !c.topUpRef(a, partial, mw, ref) {
			return nil // eval() in a rank ref's closure: cannot trust a partial ad
		}
	}
	return partial
}

// slotJobRef is a reference from a slot expression that may resolve in the job.
type slotJobRef struct {
	name  string
	toJob bool // TARGET.x; otherwise an unscoped x that reaches the job only if the slot lacks it
}

// depAttr returns the target-dependent job attribute named name, or nil.
func (mw *matchWorker) depAttr(name string) *depAttr {
	for i := range mw.deps {
		if strings.EqualFold(mw.deps[i].name, name) {
			return &mw.deps[i]
		}
	}
	return nil
}

// topUpJobAttr adds the slot attributes a target-dependent job attribute reads (when
// name is one) to partial. Returns false if they are not statically known.
func (c *Collection) topUpJobAttr(a wire.Ad, partial *classad.ClassAd, mw *matchWorker, name string) bool {
	d := mw.depAttr(name)
	if d == nil {
		return true
	}
	if d.opaque {
		return false
	}
	for _, r := range d.reads {
		if !c.topUpRef(a, partial, mw, r) {
			return false
		}
	}
	return true
}

// walkSlotRefs calls fn for each attribute reference in a slot expression that can
// resolve to an attribute: toJob is true for TARGET.x (the job's attribute) and false
// for an unscoped or MY reference (the slot's own; an unscoped one the slot does not
// define falls through to the job). It does not report eval(); callers rely on
// vm.SelfRefsSafe or the hot-closure flag for that.
func walkSlotRefs(e ast.Expr, fn func(name string, toJob bool)) {
	switch n := e.(type) {
	case nil:
	case *ast.AttributeReference:
		switch n.Scope {
		case ast.TargetScope:
			fn(n.Name, true)
		case ast.NoScope, ast.MyScope:
			fn(n.Name, false)
		}
	case *ast.ParenExpr:
		walkSlotRefs(n.Inner, fn)
	case *ast.UnaryOp:
		walkSlotRefs(n.Expr, fn)
	case *ast.BinaryOp:
		walkSlotRefs(n.Left, fn)
		walkSlotRefs(n.Right, fn)
	case *ast.ConditionalExpr:
		walkSlotRefs(n.Condition, fn)
		walkSlotRefs(n.TrueExpr, fn)
		walkSlotRefs(n.FalseExpr, fn)
	case *ast.ElvisExpr:
		walkSlotRefs(n.Left, fn)
		walkSlotRefs(n.Right, fn)
	case *ast.ListLiteral:
		for _, el := range n.Elements {
			walkSlotRefs(el, fn)
		}
	case *ast.RecordLiteral:
		if n.ClassAd != nil {
			for _, a := range n.ClassAd.Attributes {
				walkSlotRefs(a.Value, fn)
			}
		}
	case *ast.FunctionCall:
		for _, a := range n.Args {
			walkSlotRefs(a, fn)
		}
	case *ast.SelectExpr:
		walkSlotRefs(n.Record, fn)
	case *ast.SubscriptExpr:
		walkSlotRefs(n.Container, fn)
		walkSlotRefs(n.Index, fn)
	}
}

// topUpRef adds root and its transitive closure to partial from the full ad's wire
// bytes, skipping anything already present. It is for the few slot attributes the job
// side reads (Rank, target-dependent job attributes) that the Requirements closure
// does not cover. A reference from an added attribute into a target-dependent job
// attribute pulls in that attribute's slot reads too. Returns false if an added
// attribute uses eval(), or a job attribute it reaches has statically unknown reads.
func (c *Collection) topUpRef(a wire.Ad, partial *classad.ClassAd, mw *matchWorker, root string) bool {
	present := mw.hotPresent
	work := []string{strings.ToLower(root)}
	ok := true
	for len(work) > 0 && ok {
		name := work[len(work)-1]
		work = work[:len(work)-1]
		if present[name] {
			continue
		}
		present[name] = true
		var node []byte
		id, found := c.intern.LookupID(name)
		if found {
			node, found = a.Lookup(id)
		}
		if !found {
			// Absent from the slot: an unscoped reference to it falls through to the job.
			ok = c.topUpJobAttr(a, partial, mw, name)
			continue
		}
		expr, err := c.decodeNode(node)
		if err != nil {
			continue
		}
		if cname, ok := c.intern.Name(id); ok {
			partial.Insert(cname, expr)
		}
		if _, safe := vm.SelfRefsSafe(expr); !safe {
			return false
		}
		walkSlotRefs(expr, func(ref string, toJob bool) {
			if toJob {
				if ok {
					ok = c.topUpJobAttr(a, partial, mw, ref)
				}
				return
			}
			work = append(work, strings.ToLower(ref))
		})
	}
	return ok
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
		if _, safe := vm.SelfRefsSafe(expr); !safe {
			// eval() in a closure attribute: its refs are not statically known, so the
			// partial ad could be incomplete. Abandon closure decode; the caller full-
			// decodes for an exact result.
			mw.closureWork = work[:0]
			return nil
		}
		reachOK := true
		walkSlotRefs(expr, func(ref string, toJob bool) {
			if !toJob {
				if rid, found := c.intern.LookupID(ref); found {
					if _, present := mw.nodeIdx[rid]; present {
						if !mw.seenIDs[rid] {
							work = append(work, rid)
						}
						return
					}
				}
				// Not defined by the slot: an unscoped reference falls through to the job.
			}
			// A reference into a target-dependent job attribute: the job evaluates it
			// against this slot, so the slot attributes it reads join the closure.
			d := mw.depAttr(ref)
			if d == nil {
				return
			}
			if d.opaque {
				reachOK = false
				return
			}
			for _, r := range d.reads {
				if rid, found := c.intern.LookupID(r); found && !mw.seenIDs[rid] {
					work = append(work, rid)
				}
			}
		})
		if !reachOK {
			mw.closureWork = work[:0]
			return nil
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
