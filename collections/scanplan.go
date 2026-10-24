package collections

import (
	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// scanEmit consumes each visible record's decompressed wire bytes w and its segment dict
// (nil for an inline/global segment; non-nil resolves segment-local interned ids). The
// consumer decodes/renders w -- via dict when set -- so one scan path serves Query, QueryRaw,
// projection, and aggregates over both inline and interned segments. w aliases a reused
// buffer; the consumer must not retain it.
type scanEmit = func(w []byte, dict *segDictHandle) bool

// queryPlan bundles a compiled query with everything a scan needs to evaluate it,
// including reusable per-scan state.
//
// A plan that carries a query is built by newQueryPlan and nothing else. The zero
// value is the unfiltered plan the Scan paths use: no query, no probes, no pruning.
type queryPlan struct {
	q        *vm.Query
	plan     vm.ReadPlan
	m        *vm.Matcher
	wireOK   bool // native + partial-safe: try wire-native evaluation
	ws       *wireScope
	resolver func(name string, scope ast.AttributeScope) classad.Value
	// proj, when non-nil, is the projection the consumer will keep, so the scan can build a narrower
	// ad directly from a columnarized segment's columns instead of reassembling a whole record.
	proj *projPlan
	// probes are the query's top-level AND conjuncts, extracted once per scan. They
	// drive BOTH index planning and sealed-segment zone pruning: zoneWalk reads them
	// and asks the collection whether it has zone maps, rather than each scan path
	// deciding to pass a separate set of zone probes -- which three paths in turn
	// forgot to do, each silently turning a prunable query into a full scan.
	probes []vm.Probe
	// stats, when non-nil, accumulates per-scan work counts for EXPLAIN ANALYZE. nil on the
	// normal path (no overhead beyond nil checks).
	stats *ScanStats
}

// newQueryPlan builds the scan plan for q: the reusable match state, the projection
// the consumer will keep (nil for none), the stats sink (nil for none), and the
// query's probes.
//
// It is the only constructor of a plan that carries a query. Every filtered scan
// path goes through it, so none can be written that evaluates q but skips index
// demand, zone pruning, or the wire-native fast path -- the prologue is not
// something a path assembles for itself.
func (c *Collection) newQueryPlan(q *vm.Query, proj *projPlan, stats *ScanStats) queryPlan {
	plan := q.ReadPlan()
	ws := &wireScope{ctx: c}
	return queryPlan{
		q:        q,
		plan:     plan,
		m:        q.Matcher(), // reused across ads; the iterator runs single-threaded
		wireOK:   q.Native() && plan.PartialSafe,
		ws:       ws,
		resolver: ws.resolve, // bound once to avoid a per-ad closure allocation
		proj:     proj,
		probes:   q.Probes(),
		stats:    stats,
	}
}

// workerPlan clones qp with fresh per-goroutine execution state (a Matcher, a
// wireScope and a bound resolver). The program, read plan and probes it references
// are immutable and shared.
//
// The stats sink is dropped: ScanStats is not safe for concurrent accumulation, and
// a worker that inherited one would corrupt an EXPLAIN ANALYZE rather than fail.
func (c *Collection) workerPlan(qp queryPlan) queryPlan {
	ws := &wireScope{ctx: c}
	qp.m = qp.q.Matcher()
	qp.ws = ws
	qp.resolver = ws.resolve
	qp.stats = nil
	return qp
}

// planQuery records the query's index demand and returns the probes an index can
// prune with (empty: no usable index, so the caller full-scans).
func (c *Collection) planQuery(qp queryPlan) []usableProbe {
	c.demand.record(qp.probes)
	return c.planRead(qp.probes)
}

// runQuery drives the per-shard scan for a planned query: a shard whose index can
// narrow the candidates is visited through it, the rest are walked in full, and both
// re-verify the whole predicate before emit sees a record. Returns false if the
// consumer stopped iteration.
func (c *Collection) runQuery(qp queryPlan, usable []usableProbe, emit scanEmit) bool {
	for _, sh := range c.shards {
		var cont bool
		if len(usable) > 0 {
			cont = c.scanShardIndexed(sh, usable, qp, emit)
		} else {
			cont = c.scanShard(sh, qp, emit)
		}
		if !cont {
			return false
		}
	}
	return true
}

// scanQuery is the whole filtered scan: plan the read, then run it. The scan paths
// that have no special case of their own -- the projected, raw-projected and
// wire-subset scans -- are this call plus their own emitter.
func (c *Collection) scanQuery(qp queryPlan, emit scanEmit) bool {
	return c.runQuery(qp, c.planQuery(qp), emit)
}
