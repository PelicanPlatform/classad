package collections

import "sync/atomic"

// Folding one worker's groups into another's.
//
// A fanned-out grouped pass gives every worker its own groupAccum, and the answer is their union:
// a group both workers saw has to become one group holding both their contributions.
//
// THE ONLY THING THAT IS NOT A PLAIN MAP UNION IS THE STRING TABLE. A string component's key holds
// a QUERY-LOCAL id into groupAccum.strVal, and each worker builds its own table in the order it
// happened to meet values in -- so the same owner name is id 3 to one worker and id 17 to another,
// and two different names can share an id. Unioning the maps by their keys as stored would both
// SPLIT a group across two ids and FUSE two groups that collided on one, and the result reads as an
// ordinary count with no sign of either. Every string component is therefore resolved back through
// the source's table and re-interned into the destination's.
//
// Nothing else is remapped, and deliberately. A bucketed component's stored raw is already its
// bucket's edge, so the key is canonical: re-entering it through the group-forming path would floor
// an already-floored value, which is idempotent in exact arithmetic and a thing to be avoided with
// a float quotient. Merging on the stored key avoids the question entirely.

// MergeGroupSink folds one group's caller-supplied accumulators into another's.
//
// The collections layer cannot do it: a GroupSink only accepts values, and what it does with them
// is the caller's. So a caller that supplies newSink supplies this too, and it is required exactly
// when newSink is -- a fanned-out pass gives each worker its own sinks per group, and a group two
// workers both saw has two sets to fold into one. It is nil for a query using the built-in
// accumulators, which the storage layer folds itself.
//
// It is called on the goroutine that started the pass, after every worker has returned, so an
// implementation needs no synchronization. It must be associative and commutative: which worker
// draws which segment is a race, so the fold order is not fixed.
type MergeGroupSink func(dst, src GroupSink)

// mergeGroupAccum folds src's groups into dst. Both must have been built from the same request, so
// their arity, bucket mask and aggregate count agree.
func mergeGroupAccum(dst, src *groupAccum, nAgg int, mergeSink MergeGroupSink) {
	switch dst.n {
	case 1:
		mergeGroupMap(dst, &dst.g1.groupMap, src, &src.g1.groupMap, nAgg, mergeSink)
	case 2:
		mergeGroupMap(dst, &dst.g2.groupMap, src, &src.g2.groupMap, nAgg, mergeSink)
	case 3:
		mergeGroupMap(dst, &dst.g3.groupMap, src, &src.g3.groupMap, nAgg, mergeSink)
	}
}

// mergeGroupMap folds one arity's map into the matching one, re-interning string components as it
// goes (see the note above).
func mergeGroupMap[R rawArr, K kindArr](dst *groupAccum, dm *groupMap[groupKeyN[R, K]],
	src *groupAccum, sm *groupMap[groupKeyN[R, K]], nAgg int, mergeSink MergeGroupSink) {
	for k, g := range sm.acc {
		key := k
		for i := 0; i < len(k.raw); i++ {
			if k.kind[i] == gkString {
				key.raw[i] = dst.internStr(src.strVal[k.raw[i]])
			}
		}
		into, ok := dm.acc[key]
		if !ok {
			into = newGroupAcc(nAgg, dst.nFilt, dst.newSink)
			if dm.acc == nil {
				dm.acc = make(map[groupKeyN[R, K]]*groupAcc, len(sm.acc))
			}
			dm.acc[key] = into
		}
		into.mergeFrom(g, mergeSink)
	}
}

// mergeFrom folds one group's accumulation into another's.
func (g *groupAcc) mergeFrom(src *groupAcc, mergeSink MergeGroupSink) {
	g.n += src.n
	for i := range src.rows {
		if i < len(g.rows) {
			g.rows[i] += src.rows[i]
		}
	}
	if g.sink != nil {
		if mergeSink != nil && src.sink != nil {
			mergeSink(g.sink, src.sink)
		}
		return
	}
	for i := range src.stats {
		if i < len(g.stats) {
			g.stats[i].merge(&src.stats[i])
		}
	}
}

// groupsPar runs the grouped columnar pass over every segment, fanning it across workers when the
// pass is large enough and the worker budget has capacity, and folding the workers' group maps into
// one.
//
// It is the grouped counterpart of valuesPar and gates on the same threshold, for the same reason.
// What differs is the per-worker state: an ungrouped worker needs an accumulator, where a grouped
// one needs an accumulator, a group map, its own string table, and -- because gf.pass is rewritten
// per record -- its own copy of the filter planes.
//
// mergeSink folds one group's caller-supplied accumulators into another's, and must be non-nil
// exactly when newSink is: the collections layer cannot fold a GroupSink, which only accepts values.
func (c *Collection) groupsPar(groupIDs, aggIDs []uint32, buckets []int64, preds []fieldPred,
	newSink func() GroupSink, mergeSink MergeGroupSink, gf *groupFilters, bc *blockCache) (*groupAccum, bool) {
	tasks, totalBytes, release := c.gatherTasks()
	defer release()

	W := 0
	if c.qsem != nil && len(tasks) >= 2 && totalBytes >= c.aggMinBytes {
		want := c.queryPar
		if want > len(tasks) {
			want = len(tasks)
		}
		W = tryAcquire(c.qsem, want)
	}
	newAccum := func() *groupAccum {
		a := newGroupAccum(buckets, newSink)
		a.nFilt = gf.n()
		return a
	}
	p := c.planGroupPass(groupIDs, aggIDs, preds)
	if W < 2 {
		for i := 0; i < W; i++ {
			<-c.qsem // one token is not enough to parallelize; give it back
		}
		acc := newAccum()
		st := newGroupPassState(len(aggIDs), gf)
		for _, t := range tasks {
			if !c.groupPassWindow(&p, &st, t.win, t.s0, bc, acc) {
				return nil, false
			}
		}
		return acc, true
	}
	defer func() {
		for i := 0; i < W; i++ {
			<-c.qsem
		}
	}()
	aggFanouts.Add(1)

	accs := make([]*groupAccum, W)
	states := make([]groupPassState, W)
	for i := range accs {
		accs[i] = newAccum()
		// Each worker's own filter planes: cand is rebuilt per block, idxs per segment, and pass
		// is rewritten per record. Sharing them would attribute one record's filter verdict to
		// another record in another group, and the answer would be a plausible number.
		states[i] = newGroupPassState(len(aggIDs), gf.clone())
	}
	var declined atomic.Bool
	var next int64
	wg := workerGroup{stop: &declined}
	for i := 0; i < W; i++ {
		wg.Go(func() {
			wi := i
			for {
				if declined.Load() {
					return
				}
				idx := int(atomic.AddInt64(&next, 1)) - 1
				if idx >= len(tasks) {
					return
				}
				if !c.groupPassWindow(&p, &states[wi], tasks[idx].win, tasks[idx].s0, bc, accs[wi]) {
					declined.Store(true)
					return
				}
			}
		})
	}
	wg.Wait()
	// Only after Wait: a worker's panic is re-raised here, on the caller's goroutine, once nothing
	// is still reading what release() is about to unpin.
	wg.rethrow()
	if declined.Load() {
		return nil, false
	}
	for _, b := range accs[1:] {
		mergeGroupAccum(accs[0], b, len(aggIDs), mergeSink)
	}
	return accs[0], true
}
