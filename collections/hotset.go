package collections

import (
	"sort"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// RefreshHotSet samples up to sampleMax live ads, tallies how often each
// attribute appears, and installs the topN most common attributes as the hot set
// used to front-load future writes' hot headers. It counts attribute ids directly
// from the wire form (no full decode). Existing ads keep the hot header they were
// written with; because daemons rewrite ads periodically, the population's hot
// headers converge on the refreshed set over time.
//
// Returns the number of attributes chosen. A no-op (returns 0) when there are no
// ads yet.
func (c *Collection) RefreshHotSet(sampleMax, topN int) int {
	if topN <= 0 {
		return 0
	}
	counts := make(map[uint32]int)
	for _, w := range c.CollectSamples(sampleMax) {
		wire.Ad(w).ForEach(func(id uint32, _ []byte) bool {
			counts[id]++
			return true
		})
	}
	if len(counts) == 0 {
		return 0
	}
	ids := make([]uint32, 0, len(counts))
	for id := range counts {
		ids = append(ids, id)
	}
	// Most frequent first; break ties by id for determinism.
	sort.Slice(ids, func(i, j int) bool {
		if counts[ids[i]] != counts[ids[j]] {
			return counts[ids[i]] > counts[ids[j]]
		}
		return ids[i] < ids[j]
	})
	if topN > len(ids) {
		topN = len(ids)
	}
	set := make(map[uint32]struct{}, topN)
	for _, id := range ids[:topN] {
		set[id] = struct{}{}
	}
	c.hotSet.Store(&hotSetHolder{set})
	return topN
}

// HotAttrNames returns the current hot attributes by name (for diagnostics).
func (c *Collection) HotAttrNames() []string {
	set := c.currentHotSet()
	names := make([]string, 0, len(set))
	for id := range set {
		if n, ok := c.intern.Name(id); ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	return names
}
