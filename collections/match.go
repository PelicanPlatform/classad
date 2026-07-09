package collections

import (
	"iter"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/PelicanPlatform/classad/classad"
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

// taskMatches matches over the collection's segments directly (non-chained), fanning
// out across workers when the scan is large enough and the worker budget allows.
func (c *Collection) taskMatches(job *classad.ClassAd) []rankedMatch {
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
		return c.matchTasksSerial(job, tasks)
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
			m := classad.NewMatchClassAd(jobCopy, nil)
			var dbuf []byte
			var local []rankedMatch
			for {
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					break
				}
				c.matchWindow(tasks[idx], m, &dbuf, &local)
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

// matchTasksSerial matches the gathered tasks on the calling goroutine (small scans
// or an exhausted worker budget). Uses the original job, restoring its target.
func (c *Collection) matchTasksSerial(job *classad.ClassAd, tasks []scanTask) []rankedMatch {
	orig := job.GetTarget()
	defer job.SetTarget(orig)
	m := classad.NewMatchClassAd(job, nil)
	var dbuf []byte
	var out []rankedMatch
	for _, t := range tasks {
		c.matchWindow(t, m, &dbuf, &out)
	}
	return out
}

// matchWindow decodes each visible record in one window and appends its matches.
func (c *Collection) matchWindow(t scanTask, m *classad.MatchClassAd, dbuf *[]byte, out *[]rankedMatch) {
	forEachVisibleWindow(t.s0, t.win, func(adBytes []byte, codec Codec) bool {
		w, err := codec.Decompress((*dbuf)[:0], adBytes)
		if err != nil {
			return true
		}
		*dbuf = w
		node, err := c.decodeWire(w)
		if err != nil {
			return true
		}
		ad := classad.FromAST(node)
		if ok, r, hr := matchOne(m, ad); ok {
			*out = append(*out, rankedMatch{ad, r, hr})
		}
		return true
	})
}
