package collections

// A BATCHED, ZONE-HOISTED LOOKUP: many keys per read instead of one.
//
// The shape that needs it is a nested-loop join, which holds nothing and instead looks each
// streamed row's key up in the other side. Done one key at a time, what that costs is set by
// the SEGMENT COUNT and not by the key: every segment's zone map is consulted, and every
// segment the zone map cannot rule out has its index walked. Over an archive of a few hundred
// segments that per-row floor is the whole price, and it is paid again for every row.
//
// Handing the segments many keys at once removes the segment term from the per-key price:
//
//   - The ZONE CHECK is hoisted out of the per-key loop. A batch of keys spans [lo,hi], and a
//     segment whose zone map for the key lies entirely outside that span can hold none of them,
//     by the same argument zoneMayMatch makes for one constant. So the surviving segment set is
//     computed ONCE per batch, with one comparison per segment, instead of once per key. It is a
//     superset of what testing each key separately would leave -- a segment inside a gap of the
//     batch survives -- and a superset is what pruning needs: the index walk and the matcher
//     still decide every record.
//   - The INDEX WALK is then one pass over the surviving segments for the whole batch, taking
//     the union of the batch's postings per segment, rather than one pass per key.
//
// WHY THIS IS NOT THE ORDINARY CONSTRAINT PATH. A set of keys expressed as a constraint --
// `member(k, {...})` or an OR of equalities -- folds to a single "in" probe, which the planner
// handles. But a read planned that way passes overReadGate, which declines the index when the
// estimated candidate union approaches the whole collection, and that estimate runs over for a
// batch in a way that grows with the batch: on the archive measured in batchlookup_test.go,
// 1024 keys are estimated at 1.00 of the collection where the truth is 0.085, and the index is
// declined.
//
// The reason is not that the estimate is blind to zone pruning. It is not: estimateCandidates
// asks each segment index, and a segment whose min/max rules the probe out contributes nothing
// (indexCanSkip). What it cannot express is that a key of the batch lives in ONE of the
// segments the batch's span leaves, not in all of them -- a value index keeps no per-segment
// membership filter, so every surviving segment is priced as though it held every key.
// indexEstCandidates says as much about itself. The over-count is therefore about the surviving
// segment count, and no amount of extra pruning removes it, because the pruning has already
// happened by the time the estimate is summed.
//
// So hoisting does not make the gate agree; a caller that has established the restriction
// itself declines the gate instead, which is what planning through planIndex rather than
// planRead means here. Skipping it is also worth something directly: the estimate reads every
// segment's index stats, which is negligible beside a whole archive read and is not negligible
// beside a lookup for 64 keys.
//
// NUMERIC KEYS ONLY, and that is not a simplification to lift later: a zone map holds a numeric
// [min,max] and zonePrune declines a non-numeric probe value outright, so a string key has no
// span to hoist and every segment survives. Batching a string key would still save the repeated
// index walks, but the floor this exists to remove would still be there.

import (
	"iter"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// LookupKeys yields every ad of the archive whose attr equals one of keys, newest first, as
// Query does. Duplicate keys are harmless and an empty batch yields nothing.
//
// It is the inner side of a nested-loop join expressed as one call: the caller hands over the
// keys of a batch of streamed rows and gets back the records that could join with any of them.
// Each ad is yielded once however many keys it matches, so the caller re-associates ads with
// its own rows by the key -- this reports what the archive holds, not a join.
func (a *Archive) LookupKeys(attr string, keys []int64) iter.Seq[*classad.ClassAd] {
	return a.c.lookupKeys(attr, keys, false, nil)
}

// LookupKeysRedacted is LookupKeys for a reader not entitled to sealed values (see
// Collection.QueryRedacted).
func (a *Archive) LookupKeysRedacted(attr string, keys []int64) iter.Seq[*classad.ClassAd] {
	return a.c.lookupKeys(attr, keys, true, nil)
}

// LookupKeysStats is LookupKeys that also fills stats (may be nil) with the per-scan work
// breakdown, as the explained reads do. It is how a caller sees what the batch's span actually
// ruled out, rather than what LookupKeysSegments predicts it would.
func (a *Archive) LookupKeysStats(attr string, keys []int64, stats *ScanStats) iter.Seq[*classad.ClassAd] {
	return a.c.lookupKeys(attr, keys, false, stats)
}

// BatchKeyQuery builds the constraint a key batch stands for, `member(attr, {keys...})`, as a
// compiled query. It is exported because a caller measuring or explaining a batch needs the
// same query the lookup plans from, and because building it from an AST rather than from
// formatted text is what makes rebinding a batch cheap: the keys become literals directly, with
// no lexing.
func BatchKeyQuery(attr string, keys []int64) *vm.Query {
	elems := make([]ast.Expr, 0, len(keys))
	for _, k := range keys {
		elems = append(elems, &ast.IntegerLiteral{Value: k})
	}
	return vm.Compile(&ast.FunctionCall{
		Name: "member",
		Args: []ast.Expr{
			ast.NewAttributeReference(attr, ast.NoScope),
			&ast.ListLiteral{Elements: elems},
		},
	})
}

// keyBounds is the span of a batch: the one summary the hoisted zone check needs.
func keyBounds(keys []int64) (lo, hi int64) {
	lo, hi = keys[0], keys[0]
	for _, k := range keys[1:] {
		if k < lo {
			lo = k
		}
		if k > hi {
			hi = k
		}
	}
	return lo, hi
}

func (c *Collection) lookupKeys(attr string, keys []int64, redact bool, stats *ScanStats) iter.Seq[*classad.ClassAd] {
	return func(yield func(*classad.ClassAd) bool) {
		if len(keys) == 0 {
			return
		}
		lo, hi := keyBounds(keys)
		qp := c.newQueryPlan(BatchKeyQuery(attr, keys), nil, stats, redact)
		c.demand.record(qp.probes)
		// planIndex, not planRead: the gate planRead applies is the collection-wide one this
		// lookup exists to get past (see the file comment). A batch with no usable index still
		// reads correctly -- the surviving segments are walked in full and the matcher decides
		// every record -- it simply gets no index acceleration.
		usable := c.planIndex(qp.probes)
		if len(usable) > 0 {
			c.catchUpIndexes()
		}
		zoneID, zoned := uint32(0), false
		if id, ok := c.intern.LookupID(attr); ok && c.hasZones.Load() {
			zoneID, zoned = id, true
		}
		emit := c.yieldAdAs(yield, redact)
		for _, sh := range c.shards {
			if !c.scanShardKeyBatch(sh, usable, qp, zoneID, zoned, float64(lo), float64(hi), emit) {
				return
			}
		}
	}
}

// scanShardKeyBatch is scanShardIndexed for a key batch: the same walk over the same candidate
// source, with the batch's span deciding the segment set instead of a per-probe zone test.
func (c *Collection) scanShardKeyBatch(sh *shard, usable []usableProbe, qp queryPlan,
	zoneID uint32, zoned bool, lo, hi float64, emit scanEmit) bool {
	if unsatisfiable(usable) {
		return true
	}
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	walk := batchZoneWalk(wins, zoneID, zoned, lo, hi, qp.stats)
	if len(usable) == 0 {
		// No index serves the key, so there are no candidates to narrow to -- the surviving
		// windows are walked whole and the matcher decides every record. This is not the same
		// as narrowing to an empty candidate set, which is what going through the candidate
		// source with no probes would do: an index that covers nothing still reports itself as
		// covering the segment, and the walk would then yield only each segment's un-indexed
		// tail. runQuery makes the same split before scanShardIndexed, for the same reason.
		return c.scanWindows(s0, walk, qp, emit)
	}
	visit, stopped := c.refVisitor(s0, qp, emit)
	ct := qp.prunesCT()
	src := conjCandidates{usable: usable, ds: c.newDeltaScope(sh, s0, walk, usable, ct), ct: ct}
	c.forEachCandidateRef(s0, walk, src, c.reverseScan, qp.stats, visit)
	return !stopped()
}

// batchZoneWalk is the hoist: the windows whose zone map for the key does not lie wholly
// outside the batch's span, decided with ONE comparison pair per segment whatever the batch
// size. A window with no zone map for the key -- the active segment, or a segment sealed before
// the attribute was zoned -- is kept, because nothing is known about it.
//
// Soundness is zoneMayMatch's, narrowed to a span. A key k in the batch satisfies lo <= k <= hi,
// so a segment with Max < lo holds nothing >= k for any k, and one with Min > hi holds nothing
// <= k. Testing each key instead would prune at least as much and never more.
func batchZoneWalk(wins []segWindow, zoneID uint32, zoned bool, lo, hi float64, st *ScanStats) []segWindow {
	if !zoned {
		st.segments(len(wins), 0)
		return wins
	}
	walk := make([]segWindow, 0, len(wins))
	for _, w := range wins {
		if !batchKeepsWindow(w, zoneID, lo, hi) {
			continue
		}
		walk = append(walk, w)
	}
	st.segments(len(wins), len(wins)-len(walk))
	return walk
}

// batchKeepsWindow is the span test itself, and it is one function because the lookup and the
// report of what the lookup would read must not be able to disagree: a report derived from a
// second copy of the rule describes the copy.
func batchKeepsWindow(w segWindow, zoneID uint32, lo, hi float64) bool {
	z, ok := w.zones[zoneID]
	if !ok {
		return true // no zone map for the key: nothing is known, so nothing is ruled out
	}
	return z.Max >= lo && z.Min <= hi
}

// LookupKeysSegments reports how many of the archive's segments a batch of keys can match in,
// and how many its span rules out, without reading any of them. It is the hoisted zone check on
// its own, for a caller sizing a batch or explaining a plan.
func (a *Archive) LookupKeysSegments(attr string, keys []int64) (surviving, pruned int) {
	c := a.c
	if len(keys) == 0 {
		return 0, 0
	}
	lo, hi := keyBounds(keys)
	id, ok := c.intern.LookupID(attr)
	zoned := ok && c.hasZones.Load()
	for _, sh := range c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			if zoned && !batchKeepsWindow(w, id, float64(lo), float64(hi)) {
				pruned++
				continue
			}
			surviving++
		}
		releaseWindows(wins)
	}
	return surviving, pruned
}
