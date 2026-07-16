package collections

import (
	"sort"
	"strings"

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
	// Rank attributes by ACCESS -- how often the workload's queries/matches read each one
	// (c.demand.reads) -- not by mere presence in ads. Presence is near-uniform (every ad
	// carries all its attributes), so ranking by it made all attributes tie and truncated
	// alphabetically; front-loading an attribute nothing evaluates is wasted. Presence is
	// kept only as a tiebreak, so an unqueried store still gets a stable set.
	//
	// Names come via ForEachNamed (both encodings: interned ids in RAM, inline names when
	// persistent) -- the old id-based ForEach counted nothing on a persistent collection.
	presence := make(map[string]int)
	display := make(map[string]string) // folded -> first-seen spelling
	for _, w := range c.CollectSamples(sampleMax) {
		wire.Ad(w).ForEachNamed(c.intern, func(name string, _ []byte) bool {
			fold := strings.ToLower(name)
			presence[fold]++
			if _, ok := display[fold]; !ok {
				display[fold] = name
			}
			return true
		})
	}
	if len(presence) == 0 {
		return 0
	}
	reads := func(fold string) int64 {
		if v, ok := c.demand.m.Load(fold); ok {
			return v.(*demandCounts).reads.Load()
		}
		return 0
	}
	folded := make([]string, 0, len(presence))
	accessed := 0
	for n := range presence {
		folded = append(folded, n)
		if reads(n) > 0 {
			accessed++
		}
	}
	// Most-accessed first; break ties by presence, then name for determinism.
	sort.Slice(folded, func(i, j int) bool {
		ri, rj := reads(folded[i]), reads(folded[j])
		if ri != rj {
			return ri > rj
		}
		if presence[folded[i]] != presence[folded[j]] {
			return presence[folded[i]] > presence[folded[j]]
		}
		return folded[i] < folded[j]
	})
	// When the workload has actually read attributes, front-load only those (capped at
	// topN) -- don't pad the set with never-read attributes. Only a store with no query
	// signal at all falls back to the presence-ranked top-N.
	limit := topN
	if accessed > 0 && accessed < limit {
		limit = accessed
	}
	if limit > len(folded) {
		limit = len(folded)
	}
	chosen := make([]string, limit)
	for i, f := range folded[:limit] {
		chosen[i] = display[f]
	}
	c.installHotNames(chosen)
	return limit
}

// installHotNames sets the hot attribute set from names, in the form the collection's
// encoder reads: folded name set (inline/persistent) or interned id set (RAM).
func (c *Collection) installHotNames(names []string) {
	if c.inline {
		c.hotNames.Store(newHotNamesHolder(names))
		return
	}
	set := make(map[uint32]struct{}, len(names))
	for _, n := range names {
		set[c.intern.Intern(n)] = struct{}{}
	}
	c.hotSet.Store(&hotSetHolder{set})
}

// AddHotAttrs pins the named attributes into the hot set (front-loaded in future writes'
// hot headers), merging them with the current set, and returns the resulting hot
// attribute names. Unlike RefreshHotSet, which recomputes the set from sampled frequency,
// this forces specific attributes in regardless of how often they appear. Works in both
// RAM (interned) and persistent (inline) modes.
func (c *Collection) AddHotAttrs(names ...string) []string {
	if c.inline {
		merged := append([]string(nil), c.currentHotDisplay()...)
		merged = append(merged, names...)
		c.hotNames.Store(newHotNamesHolder(merged))
		return c.HotAttrNames()
	}
	if c.intern == nil {
		return c.HotAttrNames()
	}
	set := make(map[uint32]struct{}, len(c.currentHotSet())+len(names))
	for id := range c.currentHotSet() {
		set[id] = struct{}{}
	}
	for _, n := range names {
		set[c.intern.Intern(n)] = struct{}{}
	}
	c.hotSet.Store(&hotSetHolder{set})
	return c.HotAttrNames()
}

// HotAttrNames returns the current hot attributes by name (for diagnostics).
func (c *Collection) HotAttrNames() []string {
	if c.inline {
		return c.currentHotDisplay()
	}
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
