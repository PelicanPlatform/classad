package collections

import (
	"strings"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Deferred materialization for MatchSorted(limit). Building a full ClassAd
// (FromAST + attribute index) for every surviving candidate is the dominant match
// cost, yet MatchSorted with a limit -- the negotiator's "pick the best slot(s)"
// shape -- discards all but the top few. When a candidate can be decided AND ranked
// without a ClassAd, its match record instead keeps a copy of the slot's wire bytes
// and is only materialized if it survives the sort/truncate.
//
// A candidate is decidable wire-native when: the job side already proved true
// (the existing wire-native pre-filter, matchCandidate); the slot's OWN Requirements
// is a literal boolean (the ubiquitous START=true case -- a literal has no
// references, so there is no scoping/fallthrough subtlety to get wrong); and the
// job's Rank is either absent or wire-native evaluable against the slot. Anything
// else falls back to the full decode + bilateral MatchClassAd, so results are
// identical -- the fast path only ever avoids work it can prove redundant.

// jobPlan bundles the once-per-job compiled state shared by all match workers: the
// wire-native job Requirements and Rank matchers (nil when absent or not wire-native
// evaluable), whether a Rank attribute exists at all (to tell "no rank" from "rank
// we could not compile wire-native", which forces a decode), and the job's own
// attribute values.
type jobPlan struct {
	req         *vm.Query // job.Requirements, or nil (no wire-native job-side reject)
	rank        *vm.Query // job.Rank wire-native, or nil
	rankPresent bool      // job has a Rank attribute (whether or not wire-native)
	rankRefs    []string  // slot attributes the job Rank reads (for the closure seed)
	vals        map[string]classad.Value
}

// jobRankExpr returns the job's Rank expression, or nil if absent.
func jobRankExpr(job *classad.ClassAd) ast.Expr {
	for _, a := range job.AST().Attributes {
		if strings.EqualFold(a.Name, "Rank") {
			return a.Value
		}
	}
	return nil
}

// rankValue extracts a numeric Rank, matching MatchClassAd.EvaluateRankLeft: an
// integer or real ranks; anything else does not (sorted after ranked matches).
func rankValue(v classad.Value) (float64, bool) {
	if v.IsInteger() {
		i, _ := v.IntValue()
		return float64(i), true
	}
	if v.IsReal() {
		f, _ := v.RealValue()
		return f, true
	}
	return 0, false
}

// wireNativeSurvivor decides and ranks a candidate whose job side already passed,
// without building a ClassAd. ok reports whether the decision is authoritative
// wire-native; when ok is false the caller must fall back to a full decode. When ok
// is true, matched reports the bilateral result and rank/hasRank the job's Rank.
// mw.ms must already have its slot set (matchCandidate sets it for the job-side
// pre-filter) so the cached id lookup and Rank evaluation read this candidate.
func (c *Collection) wireNativeSurvivor(mw *matchWorker) (matched bool, rank float64, hasRank, ok bool) {
	// Right side: the slot's own Requirements, read straight from wire via the
	// scope's cached-id lookup. Only a literal boolean is decided here.
	node, found := mw.ms.lookupTarget("Requirements")
	if !found {
		return false, 0, false, false // absent: let the full path apply the reference semantics
	}
	lit, isLit := wire.LiteralValue(node)
	if !isLit {
		return false, 0, false, false // a real START expression: fall back
	}
	b, err := litToValue(lit).BoolValue()
	if err != nil {
		return false, 0, false, false // non-boolean Requirements: fall back
	}
	if !b {
		return false, 0, false, true // slot rejects the job: authoritative, no ClassAd
	}
	// Rank: absent -> unranked; present and wire-native -> evaluate against the slot
	// (job's Rank sees the slot as TARGET, so ms's resolver already fits); present but
	// not wire-native, or falling back on a non-literal slot attribute -> full decode.
	if !mw.rankPresent {
		return true, 0, false, true
	}
	if mw.rankM == nil {
		return false, 0, false, false
	}
	mw.ms.fellBack = false
	rv := mw.rankM.EvalResolved(mw.ms.resolve)
	if mw.ms.fellBack {
		return false, 0, false, false
	}
	f, hr := rankValue(rv)
	return true, f, hr, true
}

// appendSurvivor records a wire-native match. When materialization is deferred
// (MatchSorted with a limit), it keeps a copy of the slot's decompressed wire bytes
// and defers FromAST to materialize (only the returned top-N pay it); otherwise it
// builds the ClassAd now, as the full path would.
func (c *Collection) appendSurvivor(mw *matchWorker, w []byte, rank float64, hasRank bool, out *[]rankedMatch) {
	if mw.deferMat {
		wc := make([]byte, len(w))
		copy(wc, w)
		*out = append(*out, rankedMatch{wire: wc, rank: rank, hasRank: hasRank})
		return
	}
	node, err := c.decodeWire(w)
	if err != nil {
		return
	}
	*out = append(*out, rankedMatch{ad: classad.FromAST(node), rank: rank, hasRank: hasRank})
}

// materialize returns the match's ClassAd, decoding the retained wire bytes on first
// use. A record produced by the full (fallback) path already has its ad; a deferred
// record builds it here. Returns nil only on a decode error (dropped by the caller).
func (c *Collection) materialize(rm *rankedMatch) *classad.ClassAd {
	if rm.ad != nil {
		return rm.ad
	}
	node, err := c.decodeWire(rm.wire)
	if err != nil {
		return nil
	}
	rm.ad = classad.FromAST(node)
	return rm.ad
}
