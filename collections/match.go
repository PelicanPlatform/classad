package collections

import (
	"iter"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Matchmaking: given a job ClassAd, find the ads in the collection (typically slot
// ads) that symmetrically match it -- job.Requirements holds with the ad as TARGET
// and the ad's Requirements holds with job as TARGET -- optionally ranked by the
// job's Rank expression. See docs/MATCH.md.
//
// Match fans out across segments when the collection is not chained and query
// parallelism is enabled (the default): each worker holds its own copy of the job,
// because classad.MatchClassAd mutates an ad's match target and a shared job could not
// be evaluated concurrently. A chained collection uses the serial Scan path so it
// inherits Scan's flatten-children / hide-structural read view.
//
// Not yet done (docs/MATCH.md): wire-native evaluation of the job side (avoid decoding
// slots the job rejects) and index candidate pre-filtering. This still full-decodes
// every visible ad; the API is stable across those additions.

// rankedMatch is a matched ad and the job's Rank of it. ad is nil for a
// deferred-materialization record, whose slot bytes are held in wire until
// materialize builds the ClassAd (only for records actually returned).
type rankedMatch struct {
	ad      *classad.ClassAd
	wire    []byte // deferred: a copy of the slot's decompressed wire bytes (nil once ad is built)
	rank    float64
	hasRank bool
}

// matchOne tests ad against the job in m, returning whether it symmetrically matched
// and (for a match) the job's Rank of it. It clears ad's match target afterward so the
// result is not left pointing at the job.
func matchOne(m *classad.MatchClassAd, ad *classad.ClassAd) (ok bool, rank float64, hasRank bool) {
	return matchOneLeftKnown(m, ad, false)
}

// matchOneLeftKnown is matchOne with an escape hatch: when leftKnownTrue, the left
// (job) Requirements has already been proven true wire-native by the candidate
// pre-filter, so the bilateral match only needs the right (slot) Requirements --
// skipping a redundant tree-walk of the job's Requirements per surviving candidate.
// The wire-native pre-filter is authoritative exactly when it returned a definitive
// bool (see matchCandidate), which is the only case that sets leftKnownTrue, so this
// is result-identical to the full Symmetry.
func matchOneLeftKnown(m *classad.MatchClassAd, ad *classad.ClassAd, leftKnownTrue bool) (ok bool, rank float64, hasRank bool) {
	m.ReplaceRightAd(ad)
	var matched bool
	if leftKnownTrue {
		rightReq := m.EvaluateAttrRight("Requirements")
		if b, err := rightReq.BoolValue(); err == nil && b {
			matched = true
		}
	} else {
		matched = m.Match()
	}
	if !matched {
		ad.SetTarget(nil)
		return false, 0, false
	}
	r, hr := m.EvaluateRankLeft()
	ad.SetTarget(nil)
	return true, r, hr
}

// Match returns every ad in the collection that symmetrically matches job, in no
// particular order. Use MatchSorted for a Rank-ordered result. job is not modified.
func (c *Collection) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		if job == nil {
			return
		}
		// Unlimited: every match is returned, so materialize inline (deferMat=false);
		// records already carry their ad.
		for _, rm := range c.collectMatches(job, false) {
			if !yield(rm.ad) {
				return
			}
		}
	}
}

// MatchSorted returns the matching ads ranked by job's Rank expression, best (highest
// Rank) first. limit <= 0 returns all matches; limit > 0 returns at most the top
// limit. Ads whose Rank does not evaluate to a number sort after ranked ones. Ties in
// Rank are broken in an unspecified order (it depends on scan/fan-out order); a caller
// needing a deterministic tiebreak should apply its own. job is not modified.
func (c *Collection) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd {
	ranked := c.MatchSortedRanked(job, limit)
	if ranked == nil {
		return nil
	}
	out := make([]*classad.ClassAd, len(ranked))
	for i := range ranked {
		out[i] = ranked[i].Ad
	}
	return out
}

// RankedMatch is a matched ad and the job's Rank of it (HasRank is false when the
// job's Rank does not evaluate to a number for that ad).
type RankedMatch struct {
	Ad      *classad.ClassAd
	Rank    float64
	HasRank bool
}

// MatchSortedRanked is MatchSorted that also returns each match's Rank, best
// (highest Rank) first, up to limit (<= 0 = all). Ads whose Rank is not numeric
// sort after ranked ones. Like MatchSorted, the job's Requirements is used to
// visit only index-candidate slots when an index covers it (the matchmaking
// pushdown), rather than bilaterally evaluating every slot.
func (c *Collection) MatchSortedRanked(job *classad.ClassAd, limit int) []RankedMatch {
	if job == nil {
		return nil
	}
	// Defer ClassAd materialization when a limit will discard most survivors: rank
	// records wire-native, sort, truncate, then build only the returned ads.
	matches := c.collectMatches(job, limit > 0)
	sort.SliceStable(matches, func(i, j int) bool {
		a, b := matches[i], matches[j]
		if a.hasRank != b.hasRank {
			return a.hasRank
		}
		if a.hasRank && a.rank != b.rank {
			return a.rank > b.rank
		}
		return false
	})
	if limit > 0 && limit < len(matches) {
		matches = matches[:limit]
	}
	out := make([]RankedMatch, 0, len(matches))
	for i := range matches {
		if ad := c.materialize(&matches[i]); ad != nil {
			out = append(out, RankedMatch{Ad: ad, Rank: matches[i].rank, HasRank: matches[i].hasRank})
		}
	}
	return out
}

// collectMatches gathers all symmetric matches, in parallel when possible. When the
// job's Requirements yield an index-usable constraint on the slots, it visits only
// candidate slots (A2); otherwise it scans every slot with the wire-native reject.
func (c *Collection) collectMatches(job *classad.ClassAd, deferMat bool) []rankedMatch {
	// Reorder the job's Requirements && / || operands so the evaluator short-circuits
	// on the cheapest, most-decisive operand first (a copy; the caller's job is not
	// modified). Purely an evaluation-order optimization -- the match set is unchanged.
	job = c.reorderJobRequirements(job)
	if c.parentKeyFor != nil {
		return c.serialScanMatches(job) // chained: need Scan's flatten/hide-structural view
	}
	jp := c.compileJobSide(job)
	// Record the slot-side probes as resource-index demand -- even when nothing is
	// indexed yet -- so SuggestIndexes/.suggest can recommend indexing the attributes
	// the match filters slots on. When an index already covers them, use it to visit
	// only candidate slots (the pushdown); otherwise scan.
	probes := c.slotProbes(job, jp.vals)
	c.demand.record(probes)
	c.recordMatchReads(job, jp)
	if c.spec.Load().any() {
		// A DNF plan: one group for a conjunctive predicate, or several when the slot
		// predicate carries undefined-guard exceptions (`… || catalogs isnt undefined`).
		// Visit the union of the groups' candidates; if any disjunct is unconstrained,
		// slotMatchPlan reports not-prunable and we fall back to the full scan.
		if groups, prunable := c.slotMatchPlan(job, jp.vals); prunable {
			return c.indexedMatches(job, groups, jp, deferMat)
		}
	}
	return c.taskMatches(job, jp, deferMat)
}

// recordMatchReads records the slot attributes a match evaluates -- the Requirements'
// slot references plus the job Rank's -- as hot-set access demand, so the attributes
// matching actually reads (and re-checks) are front-loaded, not every attribute an ad
// happens to carry.
func (c *Collection) recordMatchReads(job *classad.ClassAd, jp *jobPlan) {
	// The bilateral match evaluates the slot's own Requirements (and Rank), so those
	// attributes are read on every match -- along with the slot attributes the job's
	// Requirements and Rank reference.
	reads := []string{"Requirements", "Rank"}
	if reqExpr := jobRequirementsExpr(job); reqExpr != nil {
		reads = append(reads, vm.SelfRefs(rewriteForSlot(reqExpr, jp.vals, nil, false))...)
	}
	reads = append(reads, jp.rankRefs...)
	c.demand.recordReads(reads)
}

// serialScanMatches matches over Scan (the chained/structural-aware read view).
func (c *Collection) serialScanMatches(job *classad.ClassAd) []rankedMatch {
	orig := job.GetTarget()
	defer job.SetTarget(orig)
	m := classad.NewMatchClassAd(job, nil)
	var out []rankedMatch
	for ad := range c.Scan() {
		if ok, r, hr := matchOne(m, ad); ok {
			out = append(out, rankedMatch{ad: ad, rank: r, hasRank: hr})
		}
	}
	return out
}

// matchScope resolves references while evaluating the JOB's Requirements over a slot:
// TARGET.attr comes from the slot's wire bytes (wire-native, no decode), and unscoped
// / MY.attr come from the job's precomputed attribute values. If a TARGET attribute is
// a non-literal expression, it sets fellBack and the caller decodes the slot fully.
//
// The job's Requirements references a small, fixed set of attribute names, resolved
// once per candidate slot across the whole match. Rather than re-fold and re-look-up
// those names every candidate (a locked, allocating intern LookupID on the target
// side; a ToLower on the job side), matchScope caches each name's resolution: the
// interned id for a target ref (stable for the collection) and the constant value for
// a job ref (stable for the job). One matchScope lives per worker for one match, so
// the caches populate on the first candidate and are pure hits thereafter.
type matchScope struct {
	slot     wire.Ad
	ctx      wireCtx
	jobVals  map[string]classad.Value
	fellBack bool

	// resolution caches, keyed by the reference name as it appears in the expression.
	inline   bool                // collection uses inline names (no intern table)
	intern   *wire.InternTable   // interned-mode attribute id table (nil when inline)
	idCache  map[string]cachedID // target ref name -> interned id (interned mode only)
	jobCache map[string]classad.Value
}

// cachedID memoizes an interned-mode target-attribute name resolution.
type cachedID struct {
	id    uint32
	found bool
}

func (ms *matchScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	if scope == ast.TargetScope {
		node, ok := ms.lookupTarget(name)
		if !ok {
			return classad.NewUndefinedValue()
		}
		lit, ok := wire.LiteralValue(node)
		if !ok {
			ms.fellBack = true // non-literal slot attr: cannot decide wire-native
			return classad.NewUndefinedValue()
		}
		return litToValue(lit)
	}
	// Job side: the value is a constant for the whole match, so cache it by the
	// reference's own casing and fold only on the first miss.
	if v, ok := ms.jobCache[name]; ok {
		return v
	}
	v, ok := ms.jobVals[strings.ToLower(name)]
	if !ok {
		v = classad.NewUndefinedValue()
	}
	if ms.jobCache == nil {
		ms.jobCache = make(map[string]classad.Value, 4)
	}
	ms.jobCache[name] = v
	return v
}

// lookupTarget returns the slot's raw node bytes for a TARGET reference, caching the
// name's interned id so repeated candidates skip the locked, folding intern lookup.
// It mirrors Collection.wireLookup exactly (inline path unchanged: LookupByName
// already compares case-insensitively without allocating).
func (ms *matchScope) lookupTarget(name string) ([]byte, bool) {
	if ms.inline {
		return ms.slot.LookupByName(name)
	}
	e, ok := ms.idCache[name]
	if !ok {
		id, found := ms.intern.LookupID(name)
		e = cachedID{id: id, found: found}
		if ms.idCache == nil {
			ms.idCache = make(map[string]cachedID, 4)
		}
		ms.idCache[name] = e
	}
	if !e.found {
		return nil, false
	}
	return ms.slot.Lookup(e.id)
}

// jobRequirementsExpr returns the job's Requirements expression, or nil if absent.
func jobRequirementsExpr(job *classad.ClassAd) ast.Expr {
	for _, a := range job.AST().Attributes { // AST() sorts once; benign + idempotent
		if strings.EqualFold(a.Name, "Requirements") {
			return a.Value
		}
	}
	return nil
}

// jobValues precomputes the job's own attribute values with no target set, so
// unscoped/MY references resolve to constants (a TARGET-dependent job attribute
// evaluates to undefined) without touching the shared job concurrently.
func jobValues(job *classad.ClassAd) map[string]classad.Value {
	orig := job.GetTarget()
	job.SetTarget(nil)
	vals := make(map[string]classad.Value)
	for _, name := range job.GetAttributes() {
		vals[strings.ToLower(name)] = job.EvaluateAttr(name)
	}
	job.SetTarget(orig)
	return vals
}

// compileJobSide prepares the wire-native job side: it compiles job.Requirements and
// job.Rank (each nil when absent or not wire-native evaluable) and precomputes the
// job's own attribute values. jobPlan.vals is always populated (the index pre-filter
// uses it too); rankPresent records a Rank attribute regardless of compilability, so
// a non-wire-native Rank still forces a decode rather than being read as "unranked".
func (c *Collection) compileJobSide(job *classad.ClassAd) *jobPlan {
	jp := &jobPlan{vals: jobValues(job)}
	if reqExpr := jobRequirementsExpr(job); reqExpr != nil {
		if q := vm.Compile(reqExpr); q.Native() {
			jp.req = q // ternary/list/func etc. leave req nil (no wire-native path)
		}
	}
	if rankExpr := jobRankExpr(job); rankExpr != nil {
		jp.rankPresent = true
		if q := vm.Compile(rankExpr); q.Native() {
			jp.rank = q
		}
		jp.rankRefs = rankSlotRefs(job, jp.vals)
	}
	return jp
}

// matchWorker is one goroutine's per-match state: a full bilateral matcher (its own
// job copy, since MatchClassAd mutates targets) and, when available, wire-native
// job-side matchers to skip decoding slots the job rejects and to rank/decide
// survivors without a ClassAd.
type matchWorker struct {
	mc          *classad.MatchClassAd
	jm          *vm.Matcher // job.Requirements matcher, or nil (no wire-native reject)
	rankM       *vm.Matcher // job.Rank matcher, or nil
	rankPresent bool        // job has a Rank attribute
	deferMat    bool        // defer ClassAd materialization (MatchSorted with a limit)
	ms          *matchScope

	// Closure-decode scratch (deferred path, wide interned ads): a partial ClassAd of
	// just the match closure instead of a full FromAST. Buffers are reused per worker.
	closureSeeds []string          // Requirements + job Rank's slot refs
	rankRefs     []string          // job Rank's slot refs (for hot-path top-up)
	nodeIdx      map[uint32][]byte // id -> node bytes, refilled per candidate
	seenIDs      map[uint32]bool   // BFS visited set, cleared per candidate
	closureWork  []uint32          // BFS work stack, reused
	hotPresent   map[string]bool   // hot-closure names present, cleared per candidate
}

func newMatchWorker(job *classad.ClassAd, c *Collection, jp *jobPlan, deferMat bool) *matchWorker {
	mw := &matchWorker{mc: classad.NewMatchClassAd(job, nil), rankPresent: jp.rankPresent, deferMat: deferMat}
	if jp.rank != nil {
		mw.rankM = jp.rank.Matcher()
	}
	if jp.req != nil {
		mw.jm = jp.req.Matcher()
		mw.ms = &matchScope{ctx: c, jobVals: jp.vals, inline: c.inline, intern: c.intern}
		mw.closureSeeds = buildClosureSeeds(jp.rankRefs)
		mw.rankRefs = jp.rankRefs
	}
	return mw
}

// taskMatches matches over the collection's segments directly (non-chained), fanning
// out across workers when the scan is large enough and the worker budget allows.
func (c *Collection) taskMatches(job *classad.ClassAd, jp *jobPlan, deferMat bool) []rankedMatch {
	tasks, totalBytes, release := c.gatherTasks()
	defer release()

	W := 0
	// A worker needs its own job copy; if the job cannot round-trip, stay single-
	// threaded (uses the original job under the lock-free single goroutine).
	jobText := job.StringWithPrivate()
	if _, err := classad.Parse(jobText); err == nil &&
		c.qsem != nil && len(tasks) >= 2 && totalBytes >= c.parallelMinBytes {
		want := c.queryPar
		if want > len(tasks) {
			want = len(tasks)
		}
		W = tryAcquire(c.qsem, want)
	}
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
		orig := job.GetTarget()
		defer job.SetTarget(orig)
		mw := newMatchWorker(job, c, jp, deferMat)
		var dbuf []byte
		var out []rankedMatch
		for _, t := range tasks {
			c.matchWindow(t, mw, &dbuf, &out)
		}
		return out
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()

	perWorker := make([][]rankedMatch, W)
	var next int64
	var wg sync.WaitGroup
	for i := 0; i < W; i++ {
		wg.Add(1)
		go func(wi int) {
			defer wg.Done()
			jobCopy, _ := classad.Parse(jobText) // validated above
			mw := newMatchWorker(jobCopy, c, jp, deferMat)
			var dbuf []byte
			var local []rankedMatch
			for {
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					break
				}
				c.matchWindow(tasks[idx], mw, &dbuf, &local)
			}
			perWorker[wi] = local
		}(i)
	}
	wg.Wait()
	var out []rankedMatch
	for _, lw := range perWorker {
		out = append(out, lw...)
	}
	return out
}

// matchWindow visits each visible record in one window, matching each via
// matchCandidate.
func (c *Collection) matchWindow(t scanTask, mw *matchWorker, dbuf *[]byte, out *[]rankedMatch) {
	forEachVisibleWindow(t.s0, t.win, func(adBytes []byte, codec Codec) bool {
		w, err := codec.Decompress((*dbuf)[:0], adBytes)
		if err != nil {
			return true
		}
		*dbuf = w
		c.matchCandidate(w, mw, out)
		return true
	})
}

// matchCandidate matches one slot (given its decompressed wire bytes) against the
// worker's job: it first tries the wire-native job-side reject (skipping the full
// decode of a slot the job definitely rejects), then decodes and bilaterally matches
// the survivor, appending a hit to out.
func (c *Collection) matchCandidate(w []byte, mw *matchWorker, out *[]rankedMatch) {
	leftKnownTrue := false
	if mw.jm != nil {
		mw.ms.slot = wire.Ad(w)
		mw.ms.fellBack = false
		v := mw.jm.EvalResolved(mw.ms.resolve)
		if !mw.ms.fellBack && v.IsBool() {
			b, _ := v.BoolValue()
			if !b {
				return // job definitely rejects this slot: skip the decode
			}
			leftKnownTrue = true // job definitely accepts: skip the left re-eval below
		}
	}
	// Fully wire-native survivor: when materialization is deferred (MatchSorted with a
	// limit), the job accepted this slot, and the slot's own Requirements + the job's
	// Rank can be decided from wire alone, decide and rank it without building a
	// ClassAd. Anything ambiguous returns ok=false and drops to the full decode below,
	// so results are identical. Only worthwhile when deferring: an unlimited Match
	// materializes every survivor regardless, so it stays on the plain decode path.
	if leftKnownTrue && mw.deferMat {
		if matched, rank, hasRank, ok := c.wireNativeSurvivor(mw); ok {
			if matched {
				c.appendSurvivor(mw, w, rank, hasRank, out)
			}
			return
		}
		// Non-literal slot Requirements on a wide ad: decide + rank on a partial
		// ClassAd of just the match closure, deferring full materialization to the
		// returned top-N. matchOneLeftKnown on the closure ad is result-identical to
		// the full ad (the closure holds every attribute Requirements/Rank reads).
		// Prefer the hot-header read (O(closure), no body scan) when the ad is flagged
		// closure-complete; else fall back to the cache-free full-ad two-pass.
		if mw.wantsClosureDecode(w) {
			partial := c.hotClosureMatch(w, mw)
			if partial == nil {
				partial = c.closureDecode(w, mw)
			}
			if partial != nil {
				if ok, r, hr := matchOneLeftKnown(mw.mc, partial, true); ok {
					c.appendSurvivor(mw, w, r, hr, out)
				}
				return
			}
		}
	}
	node, err := c.decodeWire(w)
	if err != nil {
		return
	}
	ad := classad.FromAST(node)
	if ok, r, hr := matchOneLeftKnown(mw.mc, ad, leftKnownTrue); ok {
		*out = append(*out, rankedMatch{ad: ad, rank: r, hasRank: hr})
	}
}
