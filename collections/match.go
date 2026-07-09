package collections

import (
	"iter"
	"sort"

	"github.com/PelicanPlatform/classad/classad"
)

// Matchmaking: given a job ClassAd, find the ads in the collection (typically slot
// ads) that symmetrically match it -- job.Requirements holds with the ad as TARGET
// and the ad's Requirements holds with job as TARGET -- optionally ranked by the
// job's Rank expression. See docs/MATCH.md.
//
// This first cut reuses Scan (so it inherits the exact read view: MVCC-snapshot,
// exactly-once, structural ads hidden, chained children flattened) and the
// classad.MatchClassAd primitive. It is serial; the parallel wire-native +
// index-pre-filtered path (docs/MATCH.md steps 1-2) is a follow-up. The API here is
// stable across that change.

// Match returns every ad in the collection that symmetrically matches job. Matches
// are yielded in scan order; use MatchSorted for a Rank-ordered result. job is not
// modified (its match target is restored on return).
func (c *Collection) Match(job *classad.ClassAd) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		if job == nil {
			return
		}
		orig := job.GetTarget()
		defer job.SetTarget(orig)
		m := classad.NewMatchClassAd(job, nil)
		for ad := range c.Scan() {
			m.ReplaceRightAd(ad)
			if m.Match() {
				ad.SetTarget(nil) // don't leak the match target to the caller
				if !yield(ad) {
					return
				}
			}
		}
	}
}

// MatchSorted returns the matching ads ranked by job's Rank expression, best (highest
// Rank) first. limit <= 0 returns all matches; limit > 0 returns at most the top
// limit. Ads whose Rank does not evaluate to a number sort after ranked ones, in scan
// order. job is not modified.
func (c *Collection) MatchSorted(job *classad.ClassAd, limit int) []*classad.ClassAd {
	if job == nil {
		return nil
	}
	orig := job.GetTarget()
	defer job.SetTarget(orig)
	m := classad.NewMatchClassAd(job, nil)

	type ranked struct {
		ad      *classad.ClassAd
		rank    float64
		hasRank bool
	}
	var matches []ranked
	for ad := range c.Scan() {
		m.ReplaceRightAd(ad)
		if m.Match() {
			r, ok := m.EvaluateRankLeft() // job.Rank with this ad as TARGET
			ad.SetTarget(nil)
			matches = append(matches, ranked{ad, r, ok})
		}
	}
	// Highest rank first; ranked before unranked; stable on ties (scan order).
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
