package dbrpc

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/PelicanPlatform/classad/db"
)

// Diagnostics is a snapshot of the store's storage, hot set, indexes, and index
// tuning advice -- the payload of the diagnostic ".stats"/".indexes"/".hot"
// commands.
type Diagnostics struct {
	Stats              db.Stats             `json:"stats"`
	Hot                []string             `json:"hot"`
	CategoricalIndexes []string             `json:"categoricalIndexes"`
	ValueIndexes       []string             `json:"valueIndexes"`
	Suggestions        []db.IndexSuggestion `json:"suggestions"`
	DropSuggestions    []db.DropSuggestion  `json:"dropSuggestions"`
}

// diagSampleMax bounds the ad sample the server takes for index suggestions.
const diagSampleMax = 2000

// diagJSON gathers the server's diagnostics into JSON.
func (s *Server) diagJSON() ([]byte, error) {
	cat, val := s.db.IndexedAttrs()
	d := Diagnostics{
		Stats:              s.db.Stats(),
		Hot:                s.db.HotAttrs(),
		CategoricalIndexes: cat,
		ValueIndexes:       val,
		Suggestions:        s.db.SuggestIndexes(diagSampleMax),
		DropSuggestions:    s.db.SuggestDrops(diagSampleMax),
	}
	return json.Marshal(d)
}

// admin runs a management action. Actions:
//
//	index.add.categorical <attr>...   add categorical (string eq/membership) indexes
//	index.add.value <attr>...         add value (numeric + range) indexes
//	index.drop <attr>...              drop indexes on the given attributes
//	index.reindex                     rebuild all indexes from live ads
//	hot.add <attr>...                 pin attributes into the hot set
//	hot.refresh <sampleMax> <topN>    recompute the hot set from sampled frequency
//	compact                           reclaim dead space in warranted shards
//	rewrite                           re-encode all ads with the current hot set
func (s *Server) admin(action string, args []string) (string, error) {
	switch action {
	case "index.add.categorical":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.categorical needs at least one attribute")
		}
		return s.addIndex("categorical index on "+join(args), args, nil), nil
	case "index.add.value":
		if len(args) == 0 {
			return "", fmt.Errorf("index.add.value needs at least one attribute")
		}
		return s.addIndex("value index on "+join(args), nil, args), nil
	case "index.drop":
		if len(args) == 0 {
			return "", fmt.Errorf("index.drop needs at least one attribute")
		}
		changed := s.db.DropIndex(args...)
		if changed {
			s.db.Reindex() // rebuild segment indexes so the dropped postings are reclaimed
		}
		return changedMsg("dropped index on "+join(args), changed), nil
	case "index.reindex":
		s.db.Reindex()
		return "reindexed", nil
	case "compact":
		n := s.db.Compact()
		return fmt.Sprintf("compacted %d shard(s)", n), nil
	case "rewrite":
		n := s.db.Rewrite()
		return fmt.Sprintf("rewrote %d ad(s) with the current hot set and compacted", n), nil
	case "hot.add":
		if len(args) == 0 {
			return "", fmt.Errorf("hot.add needs at least one attribute")
		}
		hot := s.db.AddHotAttrs(args...)
		return "hot attributes: " + join(hot), nil
	case "hot.refresh":
		if len(args) != 2 {
			return "", fmt.Errorf("hot.refresh needs <sampleMax> <topN>")
		}
		sampleMax, e1 := strconv.Atoi(args[0])
		topN, e2 := strconv.Atoi(args[1])
		if e1 != nil || e2 != nil {
			return "", fmt.Errorf("hot.refresh arguments must be integers")
		}
		n := s.db.RefreshHotSet(sampleMax, topN)
		return fmt.Sprintf("refreshed hot set: %d attribute(s)", n), nil
	default:
		return "", fmt.Errorf("unknown admin action %q", action)
	}
}

// addIndex adds an index and, when the spec changed, reindexes so the new index
// is built over the existing ads (AddIndex updates only the spec; existing
// segments' indexes are rebuilt by Reindex). Without this the index would apply
// only to future writes and would not prune the current data.
func (s *Server) addIndex(what string, categorical, value []string) string {
	changed := s.db.AddIndex(categorical, value)
	if !changed {
		return what + " (no change)"
	}
	s.db.Reindex()
	return what + " (changed; reindexed existing ads)"
}

func changedMsg(what string, changed bool) string {
	if changed {
		return what + " (changed)"
	}
	return what + " (no change)"
}

func join(ss []string) string {
	out := ""
	for i, s := range ss {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

// --- client ---

// Diagnostics fetches the store's storage stats, hot set, indexes, and tuning
// suggestions.
func (c *Client) Diagnostics() (*Diagnostics, error) {
	status, body, err := c.call(func(id uint64) []byte { return req(id, opDiag) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var d Diagnostics
	if err := json.Unmarshal([]byte(body.str()), &d); err != nil {
		return nil, err
	}
	return &d, nil
}

// Explain reports how the store would execute a constraint query.
func (c *Client) Explain(constraint string) (*db.QueryExplain, error) {
	status, body, err := c.call(func(id uint64) []byte { return putStr(req(id, opExplain), constraint) })
	if err != nil {
		return nil, err
	}
	if status != stOK {
		return nil, statusErr(status, body)
	}
	var ex db.QueryExplain
	if err := json.Unmarshal([]byte(body.str()), &ex); err != nil {
		return nil, err
	}
	return &ex, nil
}

// Admin runs a management action (index/hot-set); it returns the server's
// human-readable result. Refused on a read-only connection.
func (c *Client) Admin(action string, args ...string) (string, error) {
	status, body, err := c.call(func(id uint64) []byte {
		b := putStr(req(id, opAdmin), action)
		b = putI32(b, int32(len(args)))
		for _, a := range args {
			b = putStr(b, a)
		}
		return b
	})
	if err != nil {
		return "", err
	}
	if status != stOK {
		return "", statusErr(status, body)
	}
	return body.str(), nil
}
