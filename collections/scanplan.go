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
	// fold is this plan for the query's CurrentTime-folded form (vm.Query.Folded), evaluated in
	// windows proven free of CurrentTime (segWindow.ctFree); nil when the query has none. It shares
	// the plan's wireScope, projection and stats. See at.
	fold *queryPlan
}

// newQueryPlan builds the scan plan for q: the reusable match state, the projection
// the consumer will keep (nil for none), the stats sink (nil for none), and the
// query's probes.
//
// It is the only constructor of a plan that carries a query. Every filtered scan
// path goes through it, so none can be written that evaluates q but skips index
// demand, zone pruning, read demand, or the wire-native fast path -- the prologue is
// not something a path assembles for itself.
//
// Recording the constraint's read demand here, rather than in planQuery, is
// deliberate: planQuery is skipped by the paths that do not plan an index (the
// chained scan, the disjunctive index path, the point-in-time scan), and those scans
// read the constraint's attributes out of every record just the same.
func (c *Collection) newQueryPlan(q *vm.Query, proj *projPlan, stats *ScanStats) queryPlan {
	plan := q.ReadPlan()
	ws := &wireScope{ctx: c}
	// Read demand for the attributes the constraint evaluates -- once per scan, never
	// per record. Only the FILTER half: a path with a projection records that itself,
	// because not every path has one and "no projection" means the whole ad, which is
	// not evidence that any particular attribute is worth front-loading.
	c.demand.recordReads(q.ReadAttrs())
	qp := queryPlan{
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
	if qf := c.foldFor(q); qf != nil {
		f := qp
		fplan := qf.ReadPlan()
		f.q, f.plan, f.m, f.wireOK = qf, fplan, qf.Matcher(), qf.Native() && fplan.PartialSafe
		qp.fold = &f
	}
	return qp
}

// at returns the plan to evaluate in window w: the folded form's where w is proven free of
// CurrentTime, qp's own everywhere else.
func (qp *queryPlan) at(w segWindow) *queryPlan {
	if qp.fold != nil && w.ctFree {
		return qp.fold
	}
	return qp
}

// prunesCT reports whether qp's probes were computed with CurrentTime folded to the pinned time
// (vm.Query.ReadsCurrentTime), so they may prune only a window proven free of CurrentTime.
func (qp *queryPlan) prunesCT() bool { return qp.q != nil && qp.q.ReadsCurrentTime() }

// constTrue reports whether qp's query reads no attribute and is TRUE, so no record can change its
// answer.
func constTrue(qp *queryPlan) bool {
	return qp.plan.PartialSafe && len(qp.plan.Seeds) == 0 && qp.q.Matches(classad.New())
}

// workerPlan clones qp with fresh per-goroutine execution state (a Matcher, a
// wireScope, a bound resolver and the projection plan's segment cache). The program,
// read plan and probes it references are immutable and shared.
//
// The stats sink is dropped: ScanStats is not safe for concurrent accumulation, and
// a worker that inherited one would corrupt an EXPLAIN ANALYZE rather than fail.
func (c *Collection) workerPlan(qp queryPlan) queryPlan {
	ws := &wireScope{ctx: c}
	qp.m = qp.q.Matcher()
	qp.ws = ws
	qp.resolver = ws.resolve
	qp.stats = nil
	// The projection plan caches one segment's resolved id space IN ITSELF (projPlan.localWanted),
	// so despite its name it is execution state and not program. Workers steal different segments,
	// so a shared one is both a data race on the cache fields and a chance to narrow a record with
	// the ids another segment's dictionary resolved -- which keeps the wrong attributes rather than
	// failing. Only the cache is reset; the resolved names and global ids are immutable and shared.
	if qp.proj != nil {
		p := *qp.proj
		p.localDict, p.local = nil, nil
		qp.proj = &p
	}
	if qp.fold != nil {
		f := *qp.fold
		f.m, f.ws, f.resolver, f.stats, f.proj = f.q.Matcher(), ws, ws.resolve, nil, qp.proj
		qp.fold = &f
	}
	return qp
}

// planQuery records the query's index demand and returns the probes an index can
// prune with (empty: no usable index, so the caller full-scans).
func (c *Collection) planQuery(qp queryPlan) []usableProbe {
	c.demand.record(qp.probes)
	return c.planRead(qp.probes)
}

// readPlan is how a filtered read narrows its walk with the segment indexes: through the
// candidates of one conjunction of probes (usable), through the union of a disjunctive plan's
// groups (groups, planned from the query's DNF plan), or not at all (both empty: a full walk).
type readPlan struct {
	usable []usableProbe
	plan   []vm.ProbeGroup // the DNF groups was planned from; nil for a conjunctive read
	groups [][]usableProbe
}

// disjunctive reports a read through the union of a DNF plan's groups.
func (rp readPlan) disjunctive() bool { return rp.groups != nil }

// planReadQuery records the query's index demand and plans its read. A query whose top-level OR
// spine (after distributing nested ORs, vm.Query.ProbePlan) yields more than one group, every one
// of them index-usable, reads the union of the groups' candidates, unless the estimated union is
// nearly the whole collection (overSelectivityGate); anything else is planned as a conjunction
// (planQuery). Every filtered read plans here -- Query, the raw and projected reads, the paged
// read -- so they all take the same access path, and ExplainQuery reports it (dnfReadPlan).
//
// A read that will walk index candidates first brings the segment indexes up to date
// (catchUpIndexes), so it does not walk every record written since the last Reindex.
func (c *Collection) planReadQuery(qp queryPlan) readPlan {
	rp := c.planReadShape(qp)
	if rp.disjunctive() || (len(rp.usable) > 0 && !unsatisfiable(rp.usable)) {
		c.catchUpIndexes()
	}
	return rp
}

// planReadShape is planReadQuery's choice of access path.
func (c *Collection) planReadShape(qp queryPlan) readPlan {
	if qp.q != nil {
		if plan := qp.q.ProbePlan(); len(plan) > 1 {
			for _, g := range plan {
				c.demand.record(g.Probes)
			}
			if groups, ok := c.dnfReadPlan(plan); ok {
				return readPlan{plan: plan, groups: groups}
			}
		}
	}
	return readPlan{usable: c.planQuery(qp)}
}

// dnfReadPlan returns the index groups a read of the multi-group DNF plan uses, or false when
// some disjunct has no index-usable probe (the union covers everything) or the estimated union is
// nearly every record.
func (c *Collection) dnfReadPlan(plan []vm.ProbeGroup) ([][]usableProbe, bool) {
	groups, prunable := c.planIndexGroups(plan)
	if !prunable || overSelectivityGate(c, groups) {
		return nil, false
	}
	if live, none := liveGroups(groups); !none {
		groups = live // a disjunct proved impossible contributes nothing to the union
	}
	return groups, true
}

// runQuery drives the per-shard scan for a planned query: a shard whose index can
// narrow the candidates is visited through it, the rest are walked in full, and both
// re-verify the whole predicate before emit sees a record. Returns false if the
// consumer stopped iteration.
func (c *Collection) runQuery(qp queryPlan, rp readPlan, emit scanEmit) bool {
	for _, sh := range c.shards {
		var cont bool
		switch {
		case rp.disjunctive():
			cont = c.scanShardIndexedGroups(sh, rp.groups, rp.plan, qp, emit)
		case len(rp.usable) > 0:
			cont = c.scanShardIndexed(sh, rp.usable, qp, emit)
		default:
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
	return c.runQuery(qp, c.planReadQuery(qp), emit)
}
