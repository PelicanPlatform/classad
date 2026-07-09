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
	m.ReplaceRightAd(ad)
	if !m.Match() {
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

// collectMatches gathers all symmetric matches, in parallel when possible.
func (c *Collection) collectMatches(job *classad.ClassAd) []rankedMatch {
	if c.parentKeyFor != nil {
		return c.serialScanMatches(job) // chained: need Scan's flatten/hide-structural view
	}
	return c.taskMatches(job)
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
type matchScope struct {
	slot     wire.Ad
	ctx      wireCtx
	jobVals  map[string]classad.Value
	fellBack bool
}

func (ms *matchScope) resolve(name string, scope ast.AttributeScope) classad.Value {
	if scope == ast.TargetScope {
		node, ok := ms.ctx.wireLookup(ms.slot, name)
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
	if v, ok := ms.jobVals[strings.ToLower(name)]; ok {
		return v
	}
	return classad.NewUndefinedValue()
}

// compileJobSide prepares the wire-native job-side reject: it compiles job.Requirements
// and precomputes the job's own attribute values (so unscoped/MY references resolve
// without touching the shared job concurrently). Returns (nil, nil) when the job has no
// Requirements or it is not wire-native evaluable (then every slot is fully decoded).
func (c *Collection) compileJobSide(job *classad.ClassAd) (*vm.Query, map[string]classad.Value) {
	var reqExpr ast.Expr
	for _, a := range job.AST().Attributes { // AST() sorts once; benign + idempotent
		if strings.EqualFold(a.Name, "Requirements") {
			reqExpr = a.Value
			break
		}
	}
	if reqExpr == nil {
		return nil, nil
	}
	q := vm.Compile(reqExpr)
	if !q.Native() {
		return nil, nil // ternary/list/func etc.: no wire-native path
	}
	orig := job.GetTarget()
	job.SetTarget(nil) // evaluate the job's own attrs with no target (TARGET-dependent → undefined)
	vals := make(map[string]classad.Value)
	for _, name := range job.GetAttributes() {
		vals[strings.ToLower(name)] = job.EvaluateAttr(name)
	}
	job.SetTarget(orig)
	return q, vals
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
		mw.ms = &matchScope{ctx: c, jobVals: jobVals}
	}
	return mw
}

// taskMatches matches over the collection's segments directly (non-chained), fanning
// out across workers when the scan is large enough and the worker budget allows.
func (c *Collection) taskMatches(job *classad.ClassAd) []rankedMatch {
	tasks, totalBytes, release := c.gatherTasks()
	defer release()

	jobReq, jobVals := c.compileJobSide(job)

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

// matchWindow visits each visible record in one window: it first tries the wire-native
// job-side reject (skipping the full decode of slots the job definitely rejects), then
// decodes and bilaterally matches the survivors.
func (c *Collection) matchWindow(t scanTask, mw *matchWorker, dbuf *[]byte, out *[]rankedMatch) {
	forEachVisibleWindow(t.s0, t.win, func(adBytes []byte, codec Codec) bool {
		w, err := codec.Decompress((*dbuf)[:0], adBytes)
		if err != nil {
			return true
		}
		*dbuf = w
		if mw.jm != nil {
			mw.ms.slot = wire.Ad(w)
			mw.ms.fellBack = false
			v := mw.jm.EvalResolved(mw.ms.resolve)
			if !mw.ms.fellBack && v.IsBool() {
				if b, _ := v.BoolValue(); !b {
					return true // job definitely rejects this slot: skip the decode
				}
			}
		}
		node, err := c.decodeWire(w)
		if err != nil {
			return true
		}
		ad := classad.FromAST(node)
		if ok, r, hr := matchOne(mw.mc, ad); ok {
			*out = append(*out, rankedMatch{ad, r, hr})
		}
		return true
	})
}
