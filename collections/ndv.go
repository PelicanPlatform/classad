package collections

import (
	"fmt"
	"math"
	"slices"
	"strings"
)

// The number of distinct values an attribute takes -- NDV -- read from index metadata the
// store already holds, for a planner that has to choose an access path before reading
// anything.
//
// WHY IT NEEDS A CLASS, NOT JUST A NUMBER. The textbook join-cardinality estimate is
// |L| * |R| / max(ndv(L), ndv(R)), so NDV sits in a DENOMINATOR: understating it
// overstates the join's output, and it does so without bound. An attribute with a
// distinct value per record -- the best possible join key -- understated as 4096 makes a
// 1e6 x 1e6 join look like 2.4e8 rows instead of 1e6. A planner handed that number alone
// cannot tell it from an attribute that genuinely has 4096 values, so it prices the best
// key it has as the worst one. Every answer here therefore carries how it was obtained and
// which way it can be wrong, and a caller that ignores Accuracy is choosing to be misled.
//
// WHERE THE NUMBER COMES FROM. A segment's index already counts its distinct values
// exactly -- the postings map holds one entry per value, so len(post) IS the segment's NDV
// -- and segStats keeps it (segStats.ndv) alongside a HyperLogLog over the same values.
// The HLL is the part that makes several segments answerable: distinct counts do not add
// (two segments both holding "osg" must not count it twice), while HLL registers merge by
// register-wise max and the merged sketch counts the UNION. Both are computed once at
// index build, persisted in the sidecar, and parsed back eagerly on open, so reading them
// pages no postings and touches no record.
//
// WHAT IT DOES NOT COME FROM. Two other sources look like they should serve and do not:
//
//   - The equi-depth value histogram (segStats.hist) is built for range selectivity and
//     cannot yield NDV. Its buckets hold roughly equal COUNTS, and a bucket records only
//     its top value and a cumulative count; how many distinct values fell inside it is not
//     stored and is not recoverable. All it supports is NDV >= the number of buckets,
//     which saturates at histMaxBuckets -- useless for exactly the high-cardinality keys a
//     join is made on. It also exists only for a VALUE (numeric) index, so a string join
//     key has none at all.
//   - The sampled attribute profile behind SuggestIndexes (attrProfile.distinct) counts
//     distinct values over a bounded sample of recent ads and stops at maxDistinctSample,
//     marking itself Capped. Past the cap its number is a lower bound unrelated to the
//     truth, and obtaining it costs a sample scan -- which is a scan, so it does not
//     belong on a per-query planning path. It stays where it is, deciding which indexes
//     to build.

// NDVAccuracy says how to read NDVEstimate.Distinct. It is derived from Err, so the two
// cannot disagree; it exists because "exact" and "estimate" are what a planner branches on
// while Err is what it reasons with.
type NDVAccuracy string

const (
	// NDVExact: Distinct is the count, over the records Records names.
	NDVExact NDVAccuracy = "exact"
	// NDVBound: Distinct bounds the count, in the one direction Err names.
	NDVBound NDVAccuracy = "bound"
	// NDVEstimated: Distinct may be wrong in either direction. RelErr quantifies it when a
	// sketch produced it, and is zero when nothing quantifies it.
	NDVEstimated NDVAccuracy = "estimate"
)

// NDVErr is the direction Distinct can be wrong in, stated as what it can NEVER do.
type NDVErr string

const (
	NDVErrNone   NDVErr = "none"   // Distinct is the count
	NDVErrLow    NDVErr = "low"    // never over the count: the truth is >= Distinct
	NDVErrHigh   NDVErr = "high"   // never under the count: the truth is <= Distinct
	NDVErrEither NDVErr = "either" // no direction holds
)

// combineErr is the lattice join of two error directions: the result can be wrong any way
// either input can. Opposite one-sided errors compose to no bound at all, which is the
// case worth being explicit about -- a count that is low for one reason and high for
// another bounds nothing, however small each reason is on its own.
func combineErr(a, b NDVErr) NDVErr {
	if a == b {
		return a
	}
	if a == NDVErrNone {
		return b
	}
	if b == NDVErrNone {
		return a
	}
	return NDVErrEither
}

// accuracyFor maps an error direction to the class a caller branches on.
func accuracyFor(e NDVErr) NDVAccuracy {
	switch e {
	case NDVErrNone:
		return NDVExact
	case NDVErrLow, NDVErrHigh:
		return NDVBound
	default:
		return NDVEstimated
	}
}

// hllRelErr is the relative standard error of a HyperLogLog at hllPrecision registers:
// 1.04/sqrt(m). At p=10 (1024 registers) that is ~3.25%. It is the SD of the estimator,
// not a guarantee -- a single reading can be further out -- which is why the accuracy
// class says "either" rather than naming an interval.
var hllRelErr = 1.04 / math.Sqrt(float64(hllRegisters))

// NDVEstimate is what the store can say about one attribute's distinct-value count
// without reading a record.
//
// It is returned with an ok flag and never fabricated: an attribute no segment indexes
// yields ok == false, which a planner must be able to tell from Distinct == 1. The two
// lead to opposite decisions -- one distinct value is the degenerate key whose hash bucket
// IS the cross product, and "we do not know" is the case for measuring instead of
// assuming -- so conflating them picks the wrong plan with confidence.
type NDVEstimate struct {
	// Attr is the attribute as the index spells it.
	Attr string `json:"attr"`
	// Distinct is the count, read through Accuracy and Err.
	Distinct uint64 `json:"distinct"`

	Accuracy NDVAccuracy `json:"accuracy"`
	Err      NDVErr      `json:"err"`
	// RelErr is the relative standard error when a sketch produced Distinct, and 0
	// otherwise. A zero RelErr on an NDVEstimated answer means the error is unquantified, NOT that
	// there is none.
	RelErr float64 `json:"relErr,omitempty"`

	// Records is how many records carried an indexable value of this attribute in what was
	// counted -- the population Distinct describes, which is not the table's record count.
	Records uint64 `json:"records"`
	// Exceptions is how many records carry the attribute as something the index cannot key
	// (a list, a nested ad, an expression, the wrong literal kind). They are outside
	// Distinct entirely: a planner joining on such an attribute is joining on values no
	// statistic here has seen.
	Exceptions uint64 `json:"exceptions,omitempty"`
	// Segments is how many segments contributed a summary; Uncovered is how many hold
	// records this attribute is not indexed in, plus any segment whose index stops short of
	// its own records. Uncovered > 0 is what makes an otherwise exact count a lower bound.
	Segments  int `json:"segments"`
	Uncovered int `json:"uncovered,omitempty"`

	// Source is one sentence naming the metadata the answer came from, for EXPLAIN.
	Source string `json:"source"`
}

// ndvProbe resolves an attribute name to the indexed-attribute probe the per-segment
// summaries are keyed by, or reports that nothing indexes it. A name the collection has
// never seen, and one it has seen but does not index, are the same answer: no statistic.
func (c *Collection) ndvProbe(attr string) (usableProbe, bool) {
	spec := c.spec.Load()
	if spec == nil {
		return usableProbe{}, false
	}
	fold := strings.ToLower(strings.TrimSpace(attr))
	if fold == "" {
		return usableProbe{}, false
	}
	var id uint32
	var ok bool
	if spec.inline {
		id, ok = spec.nameToID[fold]
	} else {
		id, ok = c.intern.LookupID(fold)
	}
	if !ok {
		return usableProbe{}, false
	}
	if _, isCat := spec.cat[id]; isCat {
		return usableProbe{attrID: id, cat: true}, true
	}
	if _, isVal := spec.val[id]; isVal {
		return usableProbe{attrID: id}, true
	}
	return usableProbe{}, false
}

// AttrNDV reports how many distinct values attr takes, from the per-segment index
// summaries alone. ok is false when no segment indexes attr, which is a real answer and
// not a zero: the caller knows nothing about this attribute's cardinality and must not
// act as though it knows it is small.
//
// COST. One pass over the segments, taking each shard's read lock briefly, reading only
// heap-resident summaries -- no posting, no record, no page of a sidecar's bitmaps. A
// single contributing segment answers from its exact count and allocates nothing. Several
// merge HyperLogLog registers, which is hllRegisters byte comparisons per segment; see
// BenchmarkAttrNDV for what that costs against the EXPLAIN a join already pays per side.
//
// WHAT IT COUNTS. The values posted in each segment's INDEXED PREFIX. Three things follow,
// and each lands in Err rather than being smoothed away:
//
//   - A segment the attribute is not indexed in, and the un-indexed tail of a segment the
//     index has not caught up with, hold values nothing here has seen. The count can only
//     be low for that reason, so it becomes a lower bound.
//   - On a mutable collection the postings still carry SUPERSEDED record versions until
//     compaction reclaims them, so a value only an overwritten version held is counted.
//     The count can only be high for that reason, so it becomes an upper bound. A
//     collection with both is bounded in neither direction and says so.
//   - Records whose value the index cannot key are in Exceptions and in neither the count
//     nor its bounds.
func (c *Collection) AttrNDV(attr string) (NDVEstimate, bool) {
	up, ok := c.ndvProbe(attr)
	if !ok {
		return NDVEstimate{}, false
	}
	shape, anyDead := c.ndvIndexShape()
	if est, hit := c.ndvCached(up.attrID, shape, anyDead); hit {
		return est, true
	}
	est, ok := c.ndvCompute(attr, up, anyDead)
	if ok {
		c.ndvCache.Store(up.attrID, &ndvCacheEntry{shape: shape, anyDead: anyDead, est: est})
	}
	return est, ok
}

// ndvCacheEntry memoizes one attribute's answer against the exact index objects it was
// computed from. A segment's index is immutable once published and a new one is published
// by pointer swap, so the identity of every segment's index -- together with whether the
// index has caught up with the segment's records -- is a complete description of what the
// answer was derived from. Two walks that see the same pointers must produce the same
// number, and one that sees a different pointer must not reuse it.
type ndvCacheEntry struct {
	shape   []ndvSegShape
	anyDead bool
	est     NDVEstimate
}

// ndvSegShape is one segment's contribution to that identity: which index object it
// carries (nil when it has none) and whether records sit beyond what the index covers.
// The second is what turns an otherwise exact answer into a lower bound, and it changes
// while the index object does not -- an append to the active segment leaves its already
// built index in place -- so it has to be part of the shape rather than derived from it.
type ndvSegShape struct {
	idx  readIndex
	tail bool
}

// ndvIndexShape snapshots every segment's index identity under the shard read locks. It
// loads pointers and nothing else: no summary is looked up, no map is probed, so a
// cache hit costs one atomic load per segment rather than the register merge the answer
// was built with.
func (c *Collection) ndvIndexShape() (shape []ndvSegShape, anyDead bool) {
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue // an empty segment hides nothing
			}
			if seg.dead > 0 {
				anyDead = true
			}
			ss := ndvSegShape{idx: seg.readIdx()}
			if ss.idx != nil && int(ss.idx.coveredUpto()) < seg.used {
				ss.tail = true
			}
			shape = append(shape, ss)
		}
		sh.mu.RUnlock()
	}
	return shape, anyDead
}

// ndvCached returns the memoized answer when it was computed from exactly this shape.
func (c *Collection) ndvCached(attrID uint32, shape []ndvSegShape, anyDead bool) (NDVEstimate, bool) {
	v, ok := c.ndvCache.Load(attrID)
	if !ok {
		return NDVEstimate{}, false
	}
	e, ok := v.(*ndvCacheEntry)
	if !ok || e.anyDead != anyDead || !slices.Equal(e.shape, shape) {
		return NDVEstimate{}, false
	}
	return e.est, true
}

// ndvCompute reads the per-segment summaries and combines them. It is the miss path; see
// AttrNDV for what the combination means and why each case classes itself as it does.
func (c *Collection) ndvCompute(attr string, up usableProbe, anyDead bool) (NDVEstimate, bool) {
	name := attr
	if canon, ok := c.attrDisplayName(up.attrID); ok {
		name = canon
	}

	var (
		merged     *hyperLogLog
		maxNDV     uint64
		sumNDV     uint64
		records    uint64
		exceptions uint64
		contrib    int // segments whose summary held at least one distinct value
		covering   int // segments that index the attribute at all
		uncovered  int
		noSketch   bool
	)
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil || seg.used == 0 {
				continue
			}
			ri := seg.readIdx()
			ip, _ := ri.(indexPrimitives) //nolint:errcheck // a nil ip is the "this segment has no readable summary" case, handled below
			var st *segStats
			if ip != nil {
				st = ip.statsFor(up)
			}
			if st == nil {
				uncovered++
				continue
			}
			covering++
			// The index covers [0, coveredUpto); anything appended since is unseen. This
			// is the active segment's normal state, so it is the common reason an answer
			// is a lower bound rather than exact.
			if int(ri.coveredUpto()) < seg.used {
				uncovered++
			}
			exceptions += st.exc
			if st.covered >= st.exc {
				records += st.covered - st.exc
			}
			if st.ndv == 0 {
				continue
			}
			contrib++
			sumNDV += st.ndv
			maxNDV = max(maxNDV, st.ndv)
			if st.hll == nil {
				noSketch = true
				continue
			}
			if merged == nil {
				merged = newHLL()
			}
			merged.merge(st.hll)
		}
		sh.mu.RUnlock()
	}
	if covering == 0 {
		return NDVEstimate{}, false
	}

	est := NDVEstimate{
		Attr:       name,
		Records:    records,
		Exceptions: exceptions,
		Segments:   covering,
		Uncovered:  uncovered,
	}
	var err NDVErr
	switch {
	case contrib == 0:
		// Indexed, but no record posted a value the index could key. That is an exact
		// zero over what it saw, and it is a real finding: every record that carries the
		// attribute carries it as something unkeyable (Exceptions), or none carries it.
		est.Distinct, err, est.Source = 0, NDVErrNone,
			"no segment's index posted a keyable value for it"
	case contrib == 1:
		// One segment holds every distinct value, so its own exact count is the answer and
		// no sketch is consulted: merging one HLL would replace a number that is right
		// with one that is 3% wrong.
		est.Distinct, err, est.Source = maxNDV, NDVErrNone,
			"the exact distinct-value count of the one segment's index postings"
	case merged != nil && !noSketch:
		// Several segments, every one sketched: the merged registers count the union, so a
		// value several segments share is counted once. Clamped into [max, sum], which the
		// exact per-segment counts bound it by regardless of what the sketch says.
		e := uint64(math.Round(merged.estimate()))
		est.Distinct = min(max(e, maxNDV), sumNDV)
		err = NDVErrEither
		est.RelErr = hllRelErr
		est.Source = fmt.Sprintf("a HyperLogLog merged over %d segments' index sketches, clamped to [%d, %d]",
			contrib, maxNDV, sumNDV)
	default:
		// At least one segment carries no sketch (an index summary written before they were
		// kept), so the union cannot be estimated. The largest single segment's exact count
		// is a floor on the union, and saying so is better than adding counts that overlap.
		est.Distinct, err = maxNDV, NDVErrLow
		est.Source = fmt.Sprintf("the largest of %d segments' exact counts; a segment carries no sketch, so their union cannot be estimated",
			contrib)
	}
	if uncovered > 0 {
		err = combineErr(err, NDVErrLow)
		est.Source += fmt.Sprintf("; %d segments hold records it has not seen", uncovered)
	}
	if !c.appendOnly() && anyDead {
		err = combineErr(err, NDVErrHigh)
		est.Source += "; superseded record versions are still posted, so a value only an overwritten version held is counted"
	}
	est.Err = err
	est.Accuracy = accuracyFor(err)
	return est, true
}

// attrDisplayName is the attribute's canonical spelling for an id, from whichever naming
// the collection uses (an inline spec's own table, or the global intern table).
func (c *Collection) attrDisplayName(id uint32) (string, bool) {
	spec := c.spec.Load()
	if spec != nil && spec.inline {
		n, ok := spec.names[id]
		return n, ok
	}
	return c.intern.Name(id)
}
