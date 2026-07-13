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

// rankedMatch is a matched ad and the job's Rank of it.
type rankedMatch struct {
	ad      *classad.ClassAd
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
		for _, rm := range c.collectMatches(job) {
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
	if job == nil {
		return nil
	}
	matches := c.collectMatches(job)
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
	out := make([]*classad.ClassAd, len(matches))
	for i := range matches {
		out[i] = matches[i].ad
	}
	return out
}

// collectMatches gathers all symmetric matches, in parallel when possible. When the
// job's Requirements yield an index-usable constraint on the slots, it visits only
// candidate slots (A2); otherwise it scans every slot with the wire-native reject.
func (c *Collection) collectMatches(job *classad.ClassAd) []rankedMatch {
	if c.parentKeyFor != nil {
		return c.serialScanMatches(job) // chained: need Scan's flatten/hide-structural view
	}
	jobReq, jobVals := c.compileJobSide(job)
	if usable := c.matchIndexPlan(job, jobVals); len(usable) > 0 {
		return c.indexedMatches(job, usable, jobReq, jobVals)
	}
	return c.taskMatches(job, jobReq, jobVals)
}

// serialScanMatches matches over Scan (the chained/structural-aware read view).
func (c *Collection) serialScanMatches(job *classad.ClassAd) []rankedMatch {
	orig := job.GetTarget()
	defer job.SetTarget(orig)
	m := classad.NewMatchClassAd(job, nil)
	var out []rankedMatch
	for ad := range c.Scan() {
		if ok, r, hr := matchOne(m, ad); ok {
			out = append(out, rankedMatch{ad, r, hr})
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

// compileJobSide prepares the wire-native job-side reject: it compiles job.Requirements
// and precomputes the job's own attribute values. jobReq is nil when the job has no
// Requirements or it is not wire-native evaluable (then every candidate is fully
// decoded); jobVals is always returned (the index pre-filter uses it too).
func (c *Collection) compileJobSide(job *classad.ClassAd) (jobReq *vm.Query, jobVals map[string]classad.Value) {
	jobVals = jobValues(job)
	if reqExpr := jobRequirementsExpr(job); reqExpr != nil {
		if q := vm.Compile(reqExpr); q.Native() {
			jobReq = q // ternary/list/func etc. leave jobReq nil (no wire-native path)
		}
	}
	return jobReq, jobVals
}

// matchWorker is one goroutine's per-match state: a full bilateral matcher (its own
// job copy, since MatchClassAd mutates targets) and, when available, a wire-native
// job-side rejecter to skip decoding slots the job cannot accept.
type matchWorker struct {
	mc *classad.MatchClassAd
	jm *vm.Matcher // job.Requirements matcher, or nil (no wire-native reject)
	ms *matchScope
}

func newMatchWorker(job *classad.ClassAd, c *Collection, jobReq *vm.Query, jobVals map[string]classad.Value) *matchWorker {
	mw := &matchWorker{mc: classad.NewMatchClassAd(job, nil)}
	if jobReq != nil {
		mw.jm = jobReq.Matcher()
		mw.ms = &matchScope{ctx: c, jobVals: jobVals, inline: c.inline, intern: c.intern}
	}
	return mw
}

// taskMatches matches over the collection's segments directly (non-chained), fanning
// out across workers when the scan is large enough and the worker budget allows.
func (c *Collection) taskMatches(job *classad.ClassAd, jobReq *vm.Query, jobVals map[string]classad.Value) []rankedMatch {
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
		mw := newMatchWorker(job, c, jobReq, jobVals)
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
			mw := newMatchWorker(jobCopy, c, jobReq, jobVals)
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
	node, err := c.decodeWire(w)
	if err != nil {
		return
	}
	ad := classad.FromAST(node)
	if ok, r, hr := matchOneLeftKnown(mw.mc, ad, leftKnownTrue); ok {
		*out = append(*out, rankedMatch{ad, r, hr})
	}
}
