package collections

import (
	"iter"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// QueryProject is Query specialized for aggregation and projection: for each ad
// matching q it yields just the named attributes' values, read wire-native
// (straight from the encoded ad, no *classad.ClassAd built) whenever they are
// scalar literals -- the common case for the numeric/string attributes an
// aggregate groups or sums by. Only an ad that stores one of the requested
// attributes as a non-literal expression falls back to a full decode, and only
// that ad.
//
// This avoids the dominant cost of a GROUP BY / aggregate over Query, which
// fully decodes every matching ad (tens of allocations each) just to read one or
// two attributes.
//
// The yielded slice is reused across iterations and aligned with attrs; the
// consumer must copy any value it needs to retain past the next step (the
// aggregate reads each value into its group state immediately).
func (c *Collection) QueryProject(q *vm.Query, attrs []string) iter.Seq[[]classad.Value] {
	return func(yield func([]classad.Value) bool) {
		// Chained collections need parent attributes merged in; fall back to the
		// full-decode path (correctness over speed for the rarer chained case).
		if c.parentKeyFor != nil {
			for ad := range c.Query(q) {
				vals := make([]classad.Value, len(attrs))
				for i, n := range attrs {
					vals[i] = ad.EvaluateAttr(n)
				}
				if !yield(vals) {
					return
				}
			}
			return
		}

		plan := q.ReadPlan()
		ws := &wireScope{ctx: c}
		qp := queryPlan{
			q:        q,
			plan:     plan,
			m:        q.Matcher(),
			wireOK:   q.Native() && plan.PartialSafe,
			ws:       ws,
			resolver: ws.resolve,
		}
		probes := q.Probes()
		c.demand.record(probes)
		usable := c.planIndex(probes)

		scratch := make([]classad.Value, len(attrs))
		rs := &wireScope{ctx: c} // projection resolver, distinct from the match ws
		stopped := false
		emit := func(w []byte) bool {
			c.projectInto(rs, w, attrs, scratch)
			if !yield(scratch) {
				stopped = true
				return false
			}
			return true
		}
		for _, sh := range c.shards {
			var cont bool
			if len(usable) > 0 {
				cont = c.scanShardIndexed(sh, usable, qp, emit)
			} else {
				cont = c.scanShard(sh, qp, emit)
			}
			if !cont || stopped {
				return
			}
		}
	}
}

// projectInto fills out (len == len(attrs)) with each attribute's value read from
// the matching ad's wire bytes w, reading wire-native and falling back to a full
// decode of this one ad only if any requested attribute is a non-literal
// expression.
func (c *Collection) projectInto(rs *wireScope, w []byte, attrs []string, out []classad.Value) {
	ad := wire.Ad(w)
	rs.ad = ad
	rs.fellBack = false
	needDecode := false
	for i, name := range attrs {
		v, found := rs.tryResolve(ad, name)
		if rs.fellBack {
			needDecode = true
			break
		}
		if !found {
			out[i] = classad.NewUndefinedValue()
		} else {
			out[i] = v
		}
	}
	if needDecode {
		if a, err := c.decodeWire(w); err == nil {
			full := classad.FromAST(a)
			for i, name := range attrs {
				out[i] = full.EvaluateAttr(name)
			}
		} else {
			for i := range out {
				out[i] = classad.NewErrorValue()
			}
		}
	}
}
