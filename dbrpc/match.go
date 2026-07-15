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

// MatchTables performs cross-table matchmaking: for each ad in reqTable matching
// reqWhere, it finds the top-limit bilaterally-matching resources in resTable
// (ranked by the request's Rank), with the resource-side filter targetWhere
// applied. keyAttr is the ad attribute holding each row's key.
//
// significantAttrs, if non-empty, enables autoclustering: requests whose
// significant attributes are textually identical share one candidate computation
// (the matchmaking runs once per distinct signature and is reused), mirroring
// HTCondor's significant-attribute autoclusters. It must list every attribute the
// match depends on (the request's Requirements and Rank, and the request
// attributes the resources' Requirements reference).
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

// streamMatchTables matchmakes each request ad against the resource table and
// streams the ranked results. The request's Requirements prunes candidate slots
// via any covering index (the matchmaking pushdown, in MatchSortedRanked); the
// resource-side filter (targetWhere) is applied to the ranked matches. With
// significant attributes supplied, identical requests (by signature) reuse a
// single cached candidate computation.
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

	// compute matchmakes one request ad against the resource table, applying the
	// resource filter, returning the top-limit ranked results.
	compute := func(reqAd *classad.ClassAd) []matchResult {
		// Without a resource filter, push the limit into the match (deferred
		// materialization). With one, take all matches, filter, then cap.
		matchLimit := limit
		if resFilter != nil {
			matchLimit = 0
		}
		matches := resDB.MatchSortedRanked(reqAd, matchLimit)
		out := make([]matchResult, 0, len(matches))
		for i := range matches {
			m := matches[i]
			if resFilter != nil && !resFilter.Matches(m.Ad) {
				continue
			}
			out = append(out, matchResult{resourceKey: attrKey(m.Ad, keyAttr), rank: rankText(m)})
			if limit > 0 && len(out) >= limit {
				break
			}
		}
		return out
	}

	// Autocluster cache: signature -> ranked candidate list (identical requests
	// reuse it). Empty significant-attrs disables it (match each request).
	cache := map[uint64][]matchResult{}
	useCache := len(sigAttrs) > 0

	seq, err := reqDB.Query(orTrue(reqWhere))
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for reqAd := range seq {
		if cancelled(ctx) {
			return // client gone: stop matchmaking the remaining requests
		}
		reqKey := attrKey(reqAd, keyAttr)
		var rows []matchResult
		if useCache {
			sig := db.MatchSignature(reqAd, sigAttrs)
			cached, hit := cache[sig]
			if !hit {
				cached = compute(reqAd)
				cache[sig] = cached
			}
			rows = cached
		} else {
			rows = compute(reqAd)
		}
		for _, row := range rows {
			b := putStr(respHead(reqID, stStream), reqKey)
			b = putStr(b, row.resourceKey)
			b = putStr(b, row.rank)
			write(b)
		}
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
