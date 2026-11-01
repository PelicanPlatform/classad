package collections

import (
	"github.com/PelicanPlatform/classad/classad"
)

// Matchmaking for a reader NOT entitled to sealed values.
//
// Which slots match a job, and how the job ranks them, are answers about every attribute the two
// Requirements and the Rank read -- a sealed one included. These run the ordinary matcher in the
// reader's entitlement (jobPlan.redact): every slot is read with no key, so a sealed value is undefined
// to the wire-native reject, the partial closure ads, the full bilateral match and the returned ads
// alike. The index pushdown needs nothing: a sealed attribute is never indexed.

// MatchSortedRedacted is MatchSorted for a reader NOT entitled to sealed values (see above).
func (c *Collection) MatchSortedRedacted(job *classad.ClassAd, limit int) []*classad.ClassAd {
	ranked := c.MatchSortedRankedRedacted(job, limit)
	if ranked == nil {
		return nil
	}
	out := make([]*classad.ClassAd, len(ranked))
	for i := range ranked {
		out[i] = ranked[i].Ad
	}
	return out
}

// MatchSortedRankedRedacted is MatchSortedRanked for a reader NOT entitled to sealed values (see above).
func (c *Collection) MatchSortedRankedRedacted(job *classad.ClassAd, limit int) []RankedMatch {
	return c.matchSortedRankedAs(job, limit, true)
}

// MatchSortedRankedFilteredRedacted is MatchSortedRankedFiltered for a reader NOT entitled to sealed
// values (see above): the resource-side constraint is evaluated on the keyless slot too.
func (c *Collection) MatchSortedRankedFilteredRedacted(job *classad.ClassAd, targetConstraint string, limit int) ([]RankedMatch, error) {
	return c.matchSortedRankedFilteredAs(job, targetConstraint, limit, true)
}
