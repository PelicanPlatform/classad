package collections

import (
	"sort"
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Filtered matchmaking: MatchSortedRanked with an extra resource-side constraint (a
// MATCH's WHERE TARGET / NOPREEMPT). The constraint is pushed down -- its index probes
// are intersected into the job's candidate plan, so the scan visits only slots that can
// satisfy BOTH the job and the constraint instead of matching every slot and filtering
// afterwards -- and then applied exactly on the matched slot, so results are correct even
// where a probe is an over-broad superset.

// MatchSortedRankedFiltered is MatchSortedRanked restricted to slots that also satisfy
// targetConstraint (a constraint over the resource ad, e.g. `State =!= "Claimed"`). The
// constraint's index-satisfiable probes narrow the candidate scan (pushdown); the full
// constraint is then re-checked on each matched slot. An empty constraint is exactly
// MatchSortedRanked. Returns an error only if targetConstraint fails to parse.
func (c *Collection) MatchSortedRankedFiltered(job *classad.ClassAd, targetConstraint string, limit int) ([]RankedMatch, error) {
	return c.matchSortedRankedFilteredAs(job, targetConstraint, limit, false)
}

func (c *Collection) matchSortedRankedFilteredAs(job *classad.ClassAd, targetConstraint string, limit int, redact bool) ([]RankedMatch, error) {
	if job == nil {
		return nil, nil
	}
	if strings.TrimSpace(targetConstraint) == "" {
		return c.matchSortedRankedAs(job, limit, redact), nil
	}
	tq, err := vm.Parse(targetConstraint)
	if err != nil {
		return nil, err
	}
	// Materialize survivors (deferMat=false) so the constraint can be re-checked on the
	// slot ad before ranking/truncation.
	// The constraint is re-checked on the slot as the match decoded it, so a redacted match checks it on
	// the keyless slot too.
	matches := c.collectMatchesFiltered(job, tq.Expr(), redact)
	kept := matches[:0]
	for i := range matches {
		ad := matches[i].ad
		if ad == nil {
			ad = c.materialize(&matches[i])
		}
		if ad != nil && tq.Matches(ad) {
			matches[i].ad = ad
			kept = append(kept, matches[i])
		}
	}
	sort.SliceStable(kept, func(i, j int) bool {
		a, b := kept[i], kept[j]
		if a.hasRank != b.hasRank {
			return a.hasRank
		}
		if a.hasRank && a.rank != b.rank {
			return a.rank > b.rank
		}
		return false
	})
	if limit > 0 && limit < len(kept) {
		kept = kept[:limit]
	}
	out := make([]RankedMatch, 0, len(kept))
	for i := range kept {
		if ad := c.materialize(&kept[i]); ad != nil {
			out = append(out, RankedMatch{Ad: ad, Rank: kept[i].rank, HasRank: kept[i].hasRank})
		}
	}
	return out, nil
}

// collectMatchesFiltered gathers matches with the resource-side constraint's probes
// pushed into the candidate plan. It mirrors collectMatches but combines the job's DNF
// candidate groups with the constraint's, so the indexed scan visits the intersection.
// Materialization is never deferred here (the caller re-checks the constraint on the ad).
func (c *Collection) collectMatchesFiltered(job *classad.ClassAd, targetExpr ast.Expr, redact bool) []rankedMatch {
	if c.parentKeyFor != nil {
		return c.serialScanMatches(job, redact) // chained: the caller re-checks the constraint
	}
	jp := c.compileJobSide(job)
	jp.redact = redact
	c.demand.record(c.slotProbes(job, jp.vals))
	c.recordMatchReads(job, jp)

	var tGroups [][]usableProbe
	tPrunable := false
	if targetExpr != nil {
		tq := vm.Compile(targetExpr)
		tplan := tq.ProbePlan()
		for _, g := range tplan {
			c.demand.record(g.Probes) // the WHERE TARGET filter is resource-index demand too
		}
		// Probes over a CurrentTime the slot may define would assume it does not.
		if !tq.ReadsCurrentTime() {
			tGroups, tPrunable = c.planIndexGroups(tplan)
		}
	}

	if c.spec.Load().any() {
		jGroups, jPrunable := c.slotMatchPlan(job, jp.vals)
		if groups, prunable := combineGroups(jGroups, jPrunable, tGroups, tPrunable); prunable {
			return c.indexedMatches(job, groups, jp, false)
		}
	}
	return c.taskMatches(job, jp, false)
}

// combineGroups intersects two DNF candidate plans (job and resource-side constraint):
// the candidate set of each is the union of its groups, so their intersection is the
// union over every (job group, constraint group) pair of the concatenated probes
// (candidate = the intersection of that pair's probes). When only one side has a usable
// plan, that side's groups are used alone (the other is enforced by the re-check); when
// neither does, the caller full-scans.
func combineGroups(jg [][]usableProbe, jp bool, tg [][]usableProbe, tp bool) ([][]usableProbe, bool) {
	switch {
	case jp && tp:
		out := make([][]usableProbe, 0, len(jg)*len(tg))
		for _, a := range jg {
			for _, b := range tg {
				g := make([]usableProbe, 0, len(a)+len(b))
				g = append(g, a...)
				g = append(g, b...)
				out = append(out, g)
			}
		}
		return out, true
	case jp:
		return jg, true
	case tp:
		return tg, true
	default:
		return nil, false
	}
}
