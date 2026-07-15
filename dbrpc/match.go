package dbrpc

import (
	"sort"
	"strconv"

	"github.com/PelicanPlatform/classad/classad"
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
// (ranked by the request's Rank), pre-filtered by targetWhere. keyAttr is the ad
// attribute holding each row's key (used for the returned Request/Resource keys).
func (c *Client) MatchTables(reqTable, resTable, keyAttr, reqWhere, targetWhere string, limit int) ([]MatchRow, error) {
	_, frames, err := c.callStream(func(id uint64) []byte {
		b := putStr(req(id, opMatchTables), reqTable)
		b = putStr(b, resTable)
		b = putStr(b, keyAttr)
		b = putStr(b, reqWhere)
		b = putStr(b, targetWhere)
		b = putI32(b, int32(limit))
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
			row := MatchRow{Request: body.str(), Resource: body.str(), Rank: body.str()}
			out = append(out, row)
		case stErr:
			return out, statusErr(status, body)
		}
	}
	return out, nil
}

// rankedResource is one candidate resource's key and the request's Rank of it.
type rankedResource struct {
	key     string
	rank    float64
	hasRank bool
}

// streamMatchTables matchmakes each request ad against the resource table's
// candidates and streams the ranked results. The resource-side filter is pushed
// down once (an indexed Query on the resource table), then each request is
// bilaterally matched against the resulting candidate set.
func (s *Server) streamMatchTables(reqID uint64, r *reader, write func([]byte)) {
	reqTable := r.str()
	resTable := r.str()
	keyAttr := r.str()
	reqWhere := r.str()
	targetWhere := r.str()
	limit := int(r.i32())
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

	// Push the resource-side filter down: one indexed query selects the candidate
	// resources; every request matchmakes against this same set.
	candSeq, err := resDB.Query(orTrue(targetWhere))
	if err != nil {
		write(respErr(reqID, "resource filter: "+err.Error()))
		return
	}
	var candAds []*classad.ClassAd
	var candKeys []string
	for ad := range candSeq {
		candAds = append(candAds, ad)
		candKeys = append(candKeys, attrKey(ad, keyAttr))
	}

	reqSeq, err := reqDB.Query(orTrue(reqWhere))
	if err != nil {
		write(respErr(reqID, err.Error()))
		return
	}
	for reqAd := range reqSeq {
		reqKey := attrKey(reqAd, keyAttr)
		m := classad.NewMatchClassAd(reqAd, nil) // request is LEFT; resource swapped in as RIGHT
		var matched []rankedResource
		for i, cand := range candAds {
			m.ReplaceRightAd(cand)
			if !m.Match() {
				continue
			}
			rank, hasRank := m.EvaluateRankLeft() // the request's Rank of this resource
			matched = append(matched, rankedResource{key: candKeys[i], rank: rank, hasRank: hasRank})
		}
		// Best rank first; unranked matches after ranked ones (as MatchSorted does).
		sort.SliceStable(matched, func(a, b int) bool {
			x, y := matched[a], matched[b]
			if x.hasRank != y.hasRank {
				return x.hasRank
			}
			if x.hasRank && x.rank != y.rank {
				return x.rank > y.rank
			}
			return false
		})
		if limit > 0 && limit < len(matched) {
			matched = matched[:limit]
		}
		for _, mr := range matched {
			b := putStr(respHead(reqID, stStream), reqKey)
			b = putStr(b, mr.key)
			b = putStr(b, rankText(mr))
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

func rankText(mr rankedResource) string {
	if !mr.hasRank {
		return ""
	}
	return strconv.FormatFloat(mr.rank, 'g', -1, 64)
}
