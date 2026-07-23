package collections

import (
	"iter"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// ScanRawProjected is ScanRaw restricted to the projected attribute names,
// applied INSIDE the wire walk: the projection is resolved to interned ids once
// per scan, and each ad is walked as raw (id, node) pairs -- a non-projected
// attribute costs one id comparison and a TLV length hop, and is never name-
// resolved or rendered. For a typical monitoring projection (10-20 attributes of
// a several-hundred-attribute ad) this skips the overwhelming majority of the
// per-ad decode work that a decode-everything-then-filter projection pays.
//
// chaseRefs additionally resolves each emitted expression's attribute
// references against the same ad (transitively, per ad -- an "elevator" of
// linear passes that repeats only while new references surface), so a projected
// ad evaluates self-contained. HTCondor's query protocol sends exactly the
// requested attributes, so a protocol-compatible server passes false.
//
// redact strips private attributes exactly as ScanRawRedacted does. An empty
// projection means no attribute filter (the whole ad, matching QueryRawProject
// semantics upstream). Inline-name collections yield nothing, as with ScanRaw.
func (c *Collection) ScanRawProjected(projection []string, chaseRefs, redact bool) iter.Seq[RawAd] {
	return func(yield func(RawAd) bool) {
		p := c.newRawProjector(projection, chaseRefs, redact)
		emit := c.yieldRawProjected(yield, p)
		q := queryPlan{}
		for _, sh := range c.shards {
			if !c.scanShard(sh, q, emit) {
				return
			}
		}
	}
}

// QueryRawProjected is QueryRaw with the same in-walk projection as
// ScanRawProjected (see there for chaseRefs/redact).
func (c *Collection) QueryRawProjected(q *vm.Query, projection []string, chaseRefs, redact bool) iter.Seq[RawAd] {
	return func(yield func(RawAd) bool) {
		p := c.newRawProjector(projection, chaseRefs, redact)
		emit := c.yieldRawProjected(yield, p)
		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}
		probes := q.Probes()
		c.demand.record(probes)
		usable := c.planIndex(probes)
		for _, sh := range c.shards {
			var cont bool
			if len(usable) > 0 {
				cont = c.scanShardIndexed(sh, usable, qp, emit)
			} else {
				cont = c.scanShard(sh, qp, emit)
			}
			if !cont {
				return
			}
		}
	}
}

// rawTypeFieldNames are the type fields every raw response lifts into RawAd;
// recorded as read demand so the hot set learns to front-load them.
var rawTypeFieldNames = []string{"MyType", "TargetType"}

// rawProjector is one projected scan's state: the base wanted-id set (resolved
// from the projection once) plus per-ad scratch recycled across every ad of the
// scan. Per-ad membership uses a generation counter (gen bumps each ad; slot ==
// gen means "this ad") so nothing is cleared between ads.
type rawProjector struct {
	c         *Collection
	chaseRefs bool
	redact    bool

	wantAll   bool   // empty projection: no attribute filter (redaction still applies)
	want      []bool // base wanted set, indexed by intern id; immutable during the scan
	wantCount int    // number of distinct resolved wanted ids (the hot fast path's target)

	// Inline (persistent) collections store attribute names in the ad body, not
	// interned ids, so their wanted set is name-based: the projected names (deduped
	// case-insensitively, private ones pre-dropped under redact) with precomputed
	// fold hashes -- matched hash-first against entries and hot pairs, then
	// verified with a case-insensitive byte compare (hashes can collide).
	inline   bool
	inNames  []string
	inHashes []uint32
	inDone   []uint32 // gen-marked per projected name (see gen)

	haveMyType, haveTargetType bool
	myTypeID, targetTypeID     uint32

	gen        uint32
	wantGen    []uint32 // ref-closure additions for the current ad (slot == gen)
	doneGen    []uint32 // ids already emitted for the current ad (slot == gen)
	refs       []uint32 // per-node reference-id scratch
	hotScratch []uint32 // hot-header pair scratch (ForEachHotBuf), reused across ads

	buf   []byte
	offs  []int
	exprs [][]byte
}

func (c *Collection) newRawProjector(projection []string, chaseRefs, redact bool) *rawProjector {
	p := &rawProjector{c: c, chaseRefs: chaseRefs, redact: redact, inline: c.inline}
	if p.inline {
		p.wantAll = len(projection) == 0
		seen := make(map[string]struct{}, len(projection))
		for _, name := range projection {
			fold := strings.ToLower(name)
			if _, dup := seen[fold]; dup {
				continue
			}
			seen[fold] = struct{}{}
			// Private names never leave a redacted response; dropping them from the
			// wanted set here makes redaction free per ad.
			if redact && classad.IsPrivateAttribute(name) {
				continue
			}
			p.inNames = append(p.inNames, name)
			p.inHashes = append(p.inHashes, wire.NameHash32(name))
		}
		p.inDone = make([]uint32, len(p.inNames))
		if len(projection) > 0 {
			c.demand.recordReads(projection)
			c.demand.recordReads(rawTypeFieldNames)
		}
		return p
	}
	n := c.intern.Len()
	p.want = make([]bool, n)
	// No filter: want everything (QueryRawProject treats an empty projection as
	// "whole ad"; a flag rather than a filled slice, so attributes interned after
	// this point are still emitted). Redaction still applies below.
	p.wantAll = len(projection) == 0
	for _, name := range projection {
		// A name that was never interned exists in no stored ad; there is nothing
		// to project for it.
		if id, ok := c.intern.LookupID(name); ok && int(id) < n && !p.want[id] {
			p.want[id] = true
			p.wantCount++
		}
	}
	if len(projection) > 0 {
		// Register projection demand: RefreshHotSet ranks attributes by how often
		// queries read them, so the attributes monitoring keeps projecting (plus the
		// type fields every raw response lifts) migrate into the hot set and future
		// writes front-load them -- which is what lets the hot fast path in render
		// satisfy the whole projection without walking the ad.
		c.demand.recordReads(projection)
		c.demand.recordReads(rawTypeFieldNames)
	}
	// MyType/TargetType travel as RawAd fields (trailing type info on the wire),
	// not as projected expressions; they are lifted whenever present.
	if id, ok := c.intern.LookupID("MyType"); ok {
		p.myTypeID, p.haveMyType = id, true
	}
	if id, ok := c.intern.LookupID("TargetType"); ok {
		p.targetTypeID, p.haveTargetType = id, true
	}
	p.wantGen = make([]uint32, n)
	p.doneGen = make([]uint32, n)
	return p
}

func (c *Collection) yieldRawProjected(yield func(RawAd) bool, p *rawProjector) func(w []byte) bool {
	return func(w []byte) bool {
		ra, ok := p.render(w)
		if !ok {
			return true // inline-name or undecodable record: skip, keep scanning
		}
		return yield(ra)
	}
}

// render emits one ad's projected (and, with chaseRefs, transitively
// referenced) attributes as "Name = Value" expression slices.
//
// Fast path: the whole projection is satisfied from the HOT HEADER alone --
// O(hotCount) with direct node offsets, never touching the ad's other
// attributes. It applies when every resolved projected id (and both type
// fields) was found hot; the projection demand recorded at construction steers
// RefreshHotSet toward exactly that state. Anything short of full satisfaction
// (an attribute that is cold, or absent from the ad) falls back to the linear
// walk, which skips already-emitted ids via doneGen.
//
// The walk repeats only when chaseRefs surfaced references to ids not yet
// wanted for this ad (an earlier entry may hold a newly wanted attribute).
// Passes are bounded: each repeat strictly grows the per-ad wanted set, which
// is capped by the ad's own attribute count.
func (p *rawProjector) render(w []byte) (RawAd, bool) {
	if p.inline {
		return p.renderInline(w)
	}
	p.gen++
	gen := p.gen
	p.buf = p.buf[:0]
	p.offs = append(p.offs[:0], 0)
	var myType, targetType string
	var mtDone, ttDone bool
	ad := wire.Ad(w)
	intern := p.c.intern

	good := true
	again := false
	// handle processes one (id, node) entry -- shared by the hot fast path and
	// the linear walk. Returns false only on a render error (good is cleared).
	handle := func(id uint32, node []byte) bool {
		if !mtDone && p.haveMyType && id == p.myTypeID {
			if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
				myType = lit.Str
				mtDone = true
				return true
			}
		}
		if !ttDone && p.haveTargetType && id == p.targetTypeID {
			if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
				targetType = lit.Str
				ttDone = true
				return true
			}
		}
		wanted := p.wantAll || (int(id) < len(p.want) && p.want[id]) ||
			(int(id) < len(p.wantGen) && p.wantGen[id] == gen)
		if !wanted || (int(id) < len(p.doneGen) && p.doneGen[id] == gen) {
			return true
		}
		if p.redact && intern.IsPrivate(id) {
			return true
		}
		name, ok := intern.Name(id)
		if !ok {
			return true // unresolved id: skip (matches ForEachNamed)
		}
		p.buf = append(p.buf, name...)
		p.buf = append(p.buf, ' ', '=', ' ')
		var aerr error
		p.buf, aerr = appendWireValue(p.buf, node, intern)
		if aerr != nil {
			good = false
			return false
		}
		p.offs = append(p.offs, len(p.buf))
		if int(id) < len(p.doneGen) {
			p.doneGen[id] = gen
		}
		if p.chaseRefs {
			p.refs = wire.AppendNodeRefIDs(node, p.refs[:0])
			for _, rid := range p.refs {
				if int(rid) < len(p.wantGen) && !p.want[rid] && p.wantGen[rid] != gen {
					p.wantGen[rid] = gen
					again = true
				}
			}
		}
		return true
	}

	// Hot fast path (real projections only -- wantAll must walk everything).
	if !p.wantAll {
		needed := p.wantCount
		if p.haveMyType {
			needed++
		}
		if p.haveTargetType {
			needed++
		}
		before := len(p.offs)
		var hotOK bool
		p.hotScratch, hotOK = ad.ForEachHotBuf(p.hotScratch, handle)
		if !good {
			return RawAd{}, false
		}
		satisfied := (len(p.offs) - before) + boolInt(mtDone) + boolInt(ttDone)
		if hotOK && satisfied == needed && !again {
			p.exprs = p.exprs[:0]
			for i := 0; i+1 < len(p.offs); i++ {
				p.exprs = append(p.exprs, p.buf[p.offs[i]:p.offs[i+1]])
			}
			return RawAd{Exprs: p.exprs, MyType: myType, TargetType: targetType}, true
		}
		// Fall through: something projected was cold or absent (or references are
		// pending); the walk fills in the rest, doneGen skipping what hot emitted.
	}

	for {
		again = false
		walked := ad.ForEachIDNode(handle)
		if !walked || !good {
			return RawAd{}, false
		}
		if !again {
			break
		}
		// A new reference id may name an attribute stored EARLIER in the ad than
		// where it was discovered; restart the linear walk. doneGen keeps already-
		// emitted ids from duplicating.
	}

	p.exprs = p.exprs[:0]
	for i := 0; i+1 < len(p.offs); i++ {
		p.exprs = append(p.exprs, p.buf[p.offs[i]:p.offs[i+1]])
	}
	return RawAd{Exprs: p.exprs, MyType: myType, TargetType: targetType}, true
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// renderInline is render for a persistent (inline-names) collection: entries
// carry their names in the ad body, so the wanted test is hash-first (the same
// folded hash the inline hot header uses) verified by a case-insensitive byte
// compare. The hot fast path resolves (hash, entry-offset) pairs and early-exits
// when the whole projection plus both type fields were found hot. chaseRefs is
// not supported for inline ads (their expressions reference attributes by
// inline name, not id); the projection is served exactly.
func (p *rawProjector) renderInline(w []byte) (RawAd, bool) {
	p.gen++
	gen := p.gen
	p.buf = p.buf[:0]
	p.offs = append(p.offs[:0], 0)
	var myType, targetType string
	var mtDone, ttDone bool
	ad := wire.Ad(w)

	good := true
	handle := func(name, node []byte) bool {
		if !mtDone && wire.FoldEqualBytes(name, "MyType") {
			if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
				myType = lit.Str
				mtDone = true
				return true
			}
		}
		if !ttDone && wire.FoldEqualBytes(name, "TargetType") {
			if lit, ok := wire.LiteralValue(node); ok && lit.Kind == wire.LitString {
				targetType = lit.Str
				ttDone = true
				return true
			}
		}
		if p.wantAll {
			if p.redact && isPrivateNameBytes(name) {
				return true
			}
		} else {
			h := wire.NameHash32Bytes(name)
			hit := false
			for i, wh := range p.inHashes {
				if wh == h && p.inDone[i] != gen && wire.FoldEqualBytes(name, p.inNames[i]) {
					p.inDone[i] = gen
					hit = true
					break
				}
			}
			if !hit {
				return true
			}
		}
		p.buf = append(p.buf, name...)
		p.buf = append(p.buf, ' ', '=', ' ')
		var aerr error
		p.buf, aerr = wire.AppendNodeTextInline(p.buf, node)
		if aerr != nil {
			good = false
			return false
		}
		p.offs = append(p.offs, len(p.buf))
		return true
	}

	// Hot fast path (real projections only): the projection was pre-pruned of
	// private names, so redaction costs nothing here either.
	if !p.wantAll {
		needed := len(p.inNames) + 2 // + MyType and TargetType, lifted by name
		var hotOK bool
		p.hotScratch, hotOK = ad.ForEachHotInlineBuf(p.hotScratch, func(_ uint32, name, node []byte) bool {
			return handle(name, node)
		})
		if !good {
			return RawAd{}, false
		}
		satisfied := (len(p.offs) - 1) + boolInt(mtDone) + boolInt(ttDone)
		if hotOK && satisfied == needed {
			p.exprs = p.exprs[:0]
			for i := 0; i+1 < len(p.offs); i++ {
				p.exprs = append(p.exprs, p.buf[p.offs[i]:p.offs[i+1]])
			}
			return RawAd{Exprs: p.exprs, MyType: myType, TargetType: targetType}, true
		}
	}

	if !ad.ForEachNameNode(handle) || !good {
		return RawAd{}, false
	}
	p.exprs = p.exprs[:0]
	for i := 0; i+1 < len(p.offs); i++ {
		p.exprs = append(p.exprs, p.buf[p.offs[i]:p.offs[i+1]])
	}
	return RawAd{Exprs: p.exprs, MyType: myType, TargetType: targetType}, true
}

// isPrivateNameBytes is classad.IsPrivateAttribute for raw name bytes, gated so
// the string conversion (an allocation) happens only for the few names that
// could possibly be private (V1 names start with c/t; V2 with '_').
func isPrivateNameBytes(nm []byte) bool {
	if len(nm) == 0 {
		return false
	}
	switch nm[0] {
	case 'c', 'C', 't', 'T', '_':
		return classad.IsPrivateAttribute(string(nm))
	}
	return false
}
