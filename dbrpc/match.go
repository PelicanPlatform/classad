package dbrpc

import (
	"context"
	"strconv"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/db"
)

// MatchRow is one matchmaking result: a request key, the resource it matched, and
// the request's Rank of that resource (Rank is "" when the request has no numeric
// Rank for it).
type MatchRow struct {
	Request  string
	Resource string
	Rank     string
}

// MatchTables performs cross-table matchmaking as a greedy assignment: it walks
// the requests in reqTable matching reqWhere (in table order) and gives each one
// the single best-ranked resource in resTable — bilaterally matching, passing the
// resource-side filter targetWhere, and not already assigned to an earlier
// request — then removes that resource from the pool. limit bounds the number of
// *requests* assigned (the first `limit` in order), not resources per request. The
// result is one row per assigned request (Resource is "" when it could not be
// placed). keyAttr is the ad attribute holding each row's key.
//
// significantAttrs, if non-empty, enables autoclustering: requests whose
// significant attributes are textually identical share one ranked-candidate
// computation (the matchmaking runs once per distinct signature and is reused;
// assignment still consumes resources per request), mirroring HTCondor's
// significant-attribute autoclusters. It must list every attribute the match
// depends on (the request's Requirements and Rank, and the request attributes the
// resources' Requirements reference).
func (c *Client) MatchTables(reqTable, resTable, keyAttr, reqWhere, targetWhere string, limit int, significantAttrs []string) ([]MatchRow, error) {
	_, frames, err := c.callStream(func(id uint64) []byte {
		b := putStr(req(id, opMatchTables), reqTable)
		b = putStr(b, resTable)
		b = putStr(b, keyAttr)
		b = putStr(b, reqWhere)
		b = putStr(b, targetWhere)
		b = putI32(b, int32(limit))
		b = putI32(b, int32(len(significantAttrs)))
		for _, a := range significantAttrs {
			b = putStr(b, a)
		}
		return b
	})
	if err != nil {
		return nil, err
	}
	var out []MatchRow
	for frame := range frames {
		_, status, body, ok := respHeader(frame)
		if !ok {
			return out, errShort
		}
		switch status {
		case stStream:
			out = append(out, MatchRow{Request: body.str(), Resource: body.str(), Rank: body.str()})
		case stErr:
			return out, statusErr(status, body)
		}
	}
	return out, nil
}

// matchResult is one row of a request's (or autocluster's) ranked candidates.
type matchResult struct {
	resourceKey string
	rank        string
}

// streamMatchTables greedily assigns resources to requests and streams one row per
// request (best-ranked unclaimed resource, or empty when unplaceable), consuming
// each assigned resource so no two requests share one. The request's Requirements
// prunes candidate slots via any covering index (the matchmaking pushdown, in
// MatchSortedRanked); the resource-side filter (targetWhere) is applied to the
// ranked candidates. limit caps the number of requests assigned. With significant
// attributes supplied, identical requests (by signature) reuse a single cached
// ranked-candidate list (assignment still consumes resources per request).
func (s *Server) streamMatchTables(ctx context.Context, reqID uint64, r *reader, write func([]byte)) {
	reqTable := r.str()
	resTable := r.str()
	keyAttr := r.str()
	reqWhere := r.str()
	targetWhere := r.str()
	limit := int(r.i32())
	nSig := int(r.i32())
	if r.err != nil || nSig < 0 || nSig > 1024 {
		write(respBad(reqID))
		return
	}
	sigAttrs := make([]string, nSig)
	for i := range sigAttrs {
		sigAttrs[i] = r.str()
	}
	if r.err != nil {
		write(respBad(reqID))
		return
	}

	reqDB, ok := s.cat.Table(reqTable)
	if !ok {
		write(respErr(reqID, "no such table: "+reqTable))
		return
	}
	resDB, ok := s.cat.Table(resTable)
	if !ok {
		write(respErr(reqID, "no such table: "+resTable))
		return
	}
	var resFilter *db.Constraint
	if targetWhere != "" {
		f, err := db.ParseConstraint(targetWhere)
		if err != nil {
			write(respErr(reqID, "resource filter: "+err.Error()))
			return
		}
		resFilter = f
	}

	// candidatesFor returns a request's full ranked candidate list (best first),
	// with the resource-side filter applied. It is NOT limited: assignment consumes
	// machines, so a later job may need one ranked past every machine an earlier job
	// claimed. Under autoclustering this list is computed once per signature and
	// shared, so the full-list cost is paid once for a whole autocluster.
	candidatesFor := func(reqAd *classad.ClassAd) []matchResult {
		matches := resDB.MatchSortedRanked(reqAd, 0)
		out := make([]matchResult, 0, len(matches))
		for i := range matches {
			m := matches[i]
			if resFilter != nil && !resFilter.Matches(m.Ad) {
				continue
			}
			out = append(out, matchResult{resourceKey: attrKey(m.Ad, keyAttr), rank: rankText(m)})
		}
		return out
	}

	// Autocluster cache: signature -> ranked candidate list (identical requests
	// reuse it). Empty significant-attrs disables it (recompute each request).
	cache := map[uint64][]matchResult{}
	useCache := len(sigAttrs) > 0

	// claimed holds machines already assigned to an earlier job; greedy assignment
	// walks each job's ranked candidates and takes the best one not yet claimed,
	// removing it from the pool. limit bounds the number of *jobs* assigned.
	claimed := map[string]bool{}

	seq, err := reqDB.Query(orTrue(reqWhere))
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	jobs := 0
	for reqAd := range seq {
		if cancelled(ctx) {
			return // client gone: stop assigning the remaining jobs
		}
		if limit > 0 && jobs >= limit {
			break // limit reached: only the first `limit` jobs are assigned
		}
		jobs++
		reqKey := attrKey(reqAd, keyAttr)

		var candidates []matchResult
		if useCache {
			sig := db.MatchSignature(reqAd, sigAttrs)
			cached, hit := cache[sig]
			if !hit {
				cached = candidatesFor(reqAd)
				cache[sig] = cached
			}
			candidates = cached
		} else {
			candidates = candidatesFor(reqAd)
		}

		// Take this job's best-ranked machine that no earlier job has claimed. An
		// empty resource means the job could not be placed (its candidates were all
		// claimed, or it had none). One row per job either way.
		var got matchResult
		for _, cand := range candidates {
			if claimed[cand.resourceKey] {
				continue
			}
			claimed[cand.resourceKey] = true
			got = cand
			break
		}
		b := putStr(respHead(reqID, stStream), reqKey)
		b = putStr(b, got.resourceKey)
		b = putStr(b, got.rank)
		write(b)
	}
	write(respHead(reqID, stStreamEnd))
}

func orTrue(constraint string) string {
	if constraint == "" {
		return "true"
	}
	return constraint
}

// attrKey renders an ad's key attribute value as a string.
func attrKey(ad *classad.ClassAd, keyAttr string) string {
	return valueText(ad.EvaluateAttr(keyAttr))
}

func rankText(m db.RankedMatch) string {
	if !m.HasRank {
		return ""
	}
	return strconv.FormatFloat(m.Rank, 'g', -1, 64)
}
