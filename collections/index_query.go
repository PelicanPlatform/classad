package collections

import (
	"math"
	"sort"
	"strings"

	"github.com/RoaringBitmap/roaring/v2"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// Reindex (re)builds the per-segment value/categorical indexes for every live
// segment, covering all records written so far. It reads only immutable segment
// bytes, so it runs off the write path and does not block writers or compaction.
// Call it on whatever schedule you like: queries use whatever coverage exists and
// full-scan the rest, so Reindex only affects query speed, never results.
//
// Reindex also reconciles segments with the current index configuration: a segment
// indexed before an AddIndex is rebuilt so the new attribute is backfilled, and one
// indexed before a DropIndex is rebuilt (or, if nothing is indexed anymore, its
// index is dropped) so the removed attribute's postings are reclaimed. A whole
// span of segments therefore evolves toward the current spec at whatever cadence
// the caller reindexes — no write-path or compaction coupling.
func (c *Collection) Reindex() {
	spec := c.spec.Load()
	for _, sh := range c.shards {
		// Snapshot segments + watermarks under the read lock, then build off-lock.
		sh.mu.RLock()
		type target struct {
			seg  *segment
			used int
		}
		var tgts []target
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			cur := seg.idx.Load()
			if !spec.any() {
				if cur != nil {
					tgts = append(tgts, target{seg, seg.used}) // clear a now-orphaned index
				}
				continue
			}
			if seg.used == 0 {
				continue
			}
			// Rebuild when the index is missing, behind the write watermark, or built
			// under an older spec generation (an add/drop happened since).
			if cur == nil || int(cur.upto) < seg.used || cur.specGen != spec.gen {
				tgts = append(tgts, target{seg, seg.used})
			}
		}
		sh.mu.RUnlock()
		for _, t := range tgts {
			if !spec.any() {
				t.seg.idx.Store(nil)
				continue
			}
			t.seg.idx.Store(buildSegIndex(t.seg.data, t.used, t.seg.codec, spec))
		}
	}
}

// usableProbe is a query Probe matched to a configured index (interned attr id,
// normalized value type). The store builds candidate offset bitmaps from these;
// the full query is re-verified per candidate, so any probe omitted only costs
// selectivity.
type usableProbe struct {
	attrID uint32
	cat    bool
	op     string
	svals  []string
	fvals  []float64
}

// planIndex matches the query's probes against the configured indexes. Empty means
// no index-usable constraint (the store full-scans).
// ProbeExplain describes how one index-satisfiable conjunct of a query relates
// to the configured indexes.
type ProbeExplain struct {
	Attr    string `json:"attr"`
	Op      string `json:"op"`
	Indexed bool   `json:"indexed"`        // this probe can use an index to prune
	Kind    string `json:"kind,omitempty"` // "categorical" | "value" | "" (attr not indexed)

	// Selectivity and EstCandidates are the planner's estimate of how much this
	// probe prunes, present only when Indexed: EstCandidates is the estimated
	// number of ads the index would visit for this probe (summed over the segment
	// indexes' selectivity stats), and Selectivity is that as a fraction of the
	// total (lower is more selective). HasSelectivity distinguishes a real 0 from
	// "not estimated".
	HasSelectivity bool    `json:"hasSelectivity"`
	Selectivity    float64 `json:"selectivity,omitempty"`
	EstCandidates  int64   `json:"estCandidates,omitempty"`
}

// QueryExplain is a description of how the store would execute a query, for the
// diagnostic ".explain" command -- what the planner sees and the access path it
// would choose.
type QueryExplain struct {
	// Native reports wire-native evaluation: the query reads scalar-literal
	// attributes directly from the encoded ads, building no ClassAd per ad.
	Native bool `json:"native"`
	// Probes are the query's index-satisfiable conjuncts and their index status.
	Probes []ProbeExplain `json:"probes"`
	// IndexUsable is how many probes can prune via an index.
	IndexUsable int `json:"indexUsable"`
	// Plan is the chosen access path: "indexed" (visit index candidates),
	// "parallel-scan", or "serial-scan" (full scan).
	Plan string `json:"plan"`
	// Parallelism is the configured per-query worker cap; Shards is the shard count.
	Parallelism int `json:"parallelism"`
	Shards      int `json:"shards"`
	// TotalAds is the live ad count, the denominator for probe selectivity.
	TotalAds int `json:"totalAds"`
}

// ExplainQuery reports how the store would execute q: which of its conjuncts are
// index-usable, and the resulting access path (indexed / parallel scan / serial
// scan). It performs no I/O beyond reading the current index spec.
func (c *Collection) ExplainQuery(q *vm.Query) QueryExplain {
	probes := q.Probes()
	usable := c.planIndex(probes)
	total := c.Len()
	ex := QueryExplain{
		Native:      q.Native(),
		IndexUsable: len(usable),
		Parallelism: c.queryPar,
		Shards:      len(c.shards),
		TotalAds:    total,
	}
	for _, p := range probes {
		pe := ProbeExplain{Attr: p.Attr, Op: p.Op}
		var up usableProbe
		var isUsable bool
		pe.Indexed, pe.Kind, up, isUsable = c.probeIndexKind(p)
		if isUsable {
			if cand, covered := c.estimateCandidates(up); covered {
				pe.HasSelectivity = true
				pe.EstCandidates = int64(cand + 0.5)
				if total > 0 {
					pe.Selectivity = math.Min(1, cand/float64(total))
				}
			}
		}
		ex.Probes = append(ex.Probes, pe)
	}
	switch {
	case len(usable) > 0:
		ex.Plan = "indexed"
	case c.queryPar > 1:
		ex.Plan = "parallel-scan"
	default:
		ex.Plan = "serial-scan"
	}
	return ex
}

// probeIndexKind reports whether a probe's attribute is indexed and, if so, its
// index kind, whether this probe's operator can use it (mirrors planIndex's
// per-probe decision), and the resolved usableProbe when usable.
func (c *Collection) probeIndexKind(p vm.Probe) (indexed bool, kind string, up usableProbe, usable bool) {
	spec := c.spec.Load()
	if !spec.any() {
		return false, "", usableProbe{}, false
	}
	var id uint32
	var ok bool
	if spec.inline {
		id, ok = spec.nameToID[strings.ToLower(p.Attr)]
	} else {
		id, ok = c.intern.LookupID(p.Attr)
	}
	if !ok {
		return false, "", usableProbe{}, false
	}
	if _, isCat := spec.cat[id]; isCat {
		up, usable = catUsable(id, p)
		return usable, "categorical", up, usable
	}
	if _, isVal := spec.val[id]; isVal {
		up, usable = valUsable(id, p)
		return usable, "value", up, usable
	}
	return false, "", usableProbe{}, false
}

// estimateCandidates sums, over every segment index that covers up, the segment's
// estimated candidate count for the probe -- the same selectivity estimate the
// planner uses to order intersections. covered reports whether any built segment
// index contributed (false when the matching records are only in an unsealed
// segment with no stats yet, so no estimate is available). Approximate: it counts
// the indexed prefix (not the un-indexed tail) and may include superseded records.
func (c *Collection) estimateCandidates(up usableProbe) (cand float64, covered bool) {
	single := []usableProbe{up}
	for _, sh := range c.shards {
		s0, wins := sh.snapshot()
		for _, w := range wins {
			if si := w.seg.idx.Load(); si != nil && si.covers(single) {
				cand += si.estCandidates(up)
				covered = true
			}
		}
		releaseWindows(wins)
		_ = s0
	}
	return cand, covered
}

// planIndexGroups plans a DNF probe plan (from vm.ProbePlan): each group's probes are
// matched against the configured indexes. prunable is false if any group has no
// index-usable probe -- an unconstrained disjunct means the union covers everything,
// so the caller must full-scan instead. A single-group plan reduces to planIndex.
func (c *Collection) planIndexGroups(plan []vm.ProbeGroup) (groups [][]usableProbe, prunable bool) {
	if len(plan) == 0 {
		return nil, false
	}
	groups = make([][]usableProbe, 0, len(plan))
	for _, g := range plan {
		u := c.planIndex(g.Probes)
		if len(u) == 0 {
			return nil, false // this disjunct is unconstrained: the union can't prune
		}
		groups = append(groups, u)
	}
	return groups, true
}

func (c *Collection) planIndex(probes []vm.Probe) []usableProbe {
	spec := c.spec.Load()
	if !spec.any() {
		return nil
	}
	var out []usableProbe
	for _, p := range probes {
		var id uint32
		var ok bool
		if spec.inline {
			id, ok = spec.nameToID[strings.ToLower(p.Attr)]
		} else {
			id, ok = c.intern.LookupID(p.Attr)
		}
		if !ok {
			continue
		}
		if _, isCat := spec.cat[id]; isCat {
			if up, ok := catUsable(id, p); ok {
				out = append(out, up)
			}
			continue
		}
		if _, isVal := spec.val[id]; isVal {
			if up, ok := valUsable(id, p); ok {
				out = append(out, up)
			}
		}
	}
	return out
}

func catUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	switch p.Op {
	case "present", "absent":
		return usableProbe{attrID: id, cat: true, op: p.Op}, true // presence needs no value
	case "is", "isnt":
		// =?= / =!= are case-sensitive: carry the exact (unfolded) spelling.
		s, err := p.Vals[0].StringValue()
		if err != nil {
			return usableProbe{}, false
		}
		return usableProbe{attrID: id, cat: true, op: p.Op, svals: []string{s}}, true
	case "==", "!=", "in":
	default:
		return usableProbe{}, false // ranges are not indexed for categoricals
	}
	svals := make([]string, 0, len(p.Vals))
	for _, v := range p.Vals {
		s, err := v.StringValue()
		if err != nil {
			return usableProbe{}, false
		}
		svals = append(svals, strings.ToLower(s)) // fold to match the index key
	}
	return usableProbe{attrID: id, cat: true, op: p.Op, svals: svals}, true
}

func valUsable(id uint32, p vm.Probe) (usableProbe, bool) {
	op := p.Op
	switch p.Op {
	case "present", "absent":
		return usableProbe{attrID: id, cat: false, op: p.Op}, true // presence needs no value
	case "is":
		// numeric =?= plans as == (a superset: int/real fold into one float key; the
		// store re-verifies). =!= ("isnt") is NOT indexed for values -- int/real
		// type-strictness (5 =?= 5.0 is false) means the != posting path would drop the
		// other-typed records =!= must keep -- so it falls through to a scan.
		op = "=="
	case "isnt":
		return usableProbe{}, false
	case "==", "!=", "in", "<", "<=", ">", ">=":
	default:
		return usableProbe{}, false
	}
	fvals := make([]float64, 0, len(p.Vals))
	for _, v := range p.Vals {
		f, ok := numericFloat(v)
		if !ok {
			return usableProbe{}, false
		}
		fvals = append(fvals, f)
	}
	return usableProbe{attrID: id, cat: false, op: op, fvals: fvals}, true
}

func numericFloat(v classad.Value) (float64, bool) {
	if f, err := v.NumberValue(); err == nil {
		return f, true
	}
	if b, err := v.BoolValue(); err == nil {
		if b {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

// coversGroups reports whether a segment index covers every probe of every group. If
// it does not cover some group, that disjunct is unconstrained for this segment, so
// the caller must full-scan the window rather than use the (incomplete) union.
func (si *segIndex) coversGroups(groups [][]usableProbe) bool {
	for _, g := range groups {
		if !si.covers(g) {
			return false
		}
	}
	return true
}

// candidateOffsetsGroups returns the union over groups of each group's candidate
// intersection -- the DNF `OR of (AND of probes)`. Callers pass only prunable plans
// (every group has at least one usable probe), so no group widens to everything.
func (si *segIndex) candidateOffsetsGroups(groups [][]usableProbe) *roaring.Bitmap {
	var acc *roaring.Bitmap
	for _, g := range groups {
		gb := si.candidateOffsets(g)
		if gb == nil {
			gb = roaring.New()
		}
		if acc == nil {
			acc = gb
		} else {
			acc.Or(gb)
		}
	}
	return acc
}

// covers reports whether a segment index has postings for every usable probe's
// attribute (a segment indexed before an attribute was added would not).
func (si *segIndex) covers(usable []usableProbe) bool {
	for _, up := range usable {
		if up.cat {
			if si.cat[up.attrID] == nil {
				return false
			}
		} else if si.val[up.attrID] == nil {
			return false
		}
	}
	return true
}

// candidateOffsets returns the segment-record offsets satisfying every usable
// probe (a superset; the store re-verifies). Probes are applied most-selective
// first (by the per-attribute stats estimate) so the roaring intersection shrinks
// fastest and the widest probes touch the smallest accumulator. nil means "no
// candidates".
func (si *segIndex) candidateOffsets(usable []usableProbe) *roaring.Bitmap {
	// Fast path: 0/1 probe needs no ordering (and allocates no order/est slices),
	// which is the common single-constraint query.
	switch len(usable) {
	case 0:
		return nil
	case 1:
		return si.probeOffsets(usable[0])
	}
	order := si.selectivityOrder(usable)
	var acc *roaring.Bitmap
	for _, i := range order {
		bm := si.probeOffsets(usable[i])
		if acc == nil {
			acc = bm
		} else {
			acc.And(bm)
		}
		if acc.IsEmpty() {
			return acc
		}
	}
	return acc
}

// selectivityOrder returns indices into usable ordered by ascending estimated
// candidate count (most selective first). It is a pure ordering heuristic: the
// AND is commutative, so this never changes the result, only the work. Ties and
// missing stats fall back to input order for a deterministic plan.
func (si *segIndex) selectivityOrder(usable []usableProbe) []int {
	order := make([]int, len(usable))
	est := make([]float64, len(usable))
	for i, up := range usable {
		order[i] = i
		est[i] = si.estCandidates(up)
	}
	sort.SliceStable(order, func(a, b int) bool {
		return est[order[a]] < est[order[b]]
	})
	return order
}

// statsFor returns the segStats for a probe's attribute, or nil if that segment
// does not index it (covers() has already been checked in the query path, so this
// is non-nil there; the guard keeps the estimators safe in isolation).
func (si *segIndex) statsFor(up usableProbe) *segStats {
	if up.cat {
		if cp := si.cat[up.attrID]; cp != nil {
			return &cp.stats
		}
		return nil
	}
	if vp := si.val[up.attrID]; vp != nil {
		return &vp.stats
	}
	return nil
}

// canSkip reports whether the segment's indexed prefix provably holds no record
// satisfying this probe — so a query whose conjunction includes it can skip the
// whole prefix and only full-scan the un-indexed tail. It is correctness-critical:
// it must return true only when certain. Exceptional records (value present but
// not the indexed literal type) are re-verified candidates, so any exception
// forbids a skip. Equality/range/membership can skip; `!=` never does.
func (si *segIndex) canSkip(up usableProbe) bool {
	s := si.statsFor(up)
	if s == nil || s.exc > 0 {
		return false
	}
	if up.op == "present" || up.op == "absent" {
		return false // presence spans posted + exc + absent; never provably empty here
	}
	if up.cat {
		if up.op != "==" && up.op != "in" {
			return false
		}
		if s.bloom == nil {
			return false
		}
		for _, v := range up.svals {
			if s.bloom.mayContain(hashString(v)) {
				return false // a value might be present: cannot skip
			}
		}
		return true
	}
	if !s.hasRange {
		// No numeric record in the prefix (and exc==0): nothing an equality/range
		// probe could match. A `!=` still matches every non-exc record, so keep it.
		return up.op != "!="
	}
	switch up.op {
	case "==", "in":
		for _, t := range up.fvals {
			if t >= s.min && t <= s.max {
				return false
			}
		}
		return true
	case "<":
		return s.min >= up.fvals[0]
	case "<=":
		return s.min > up.fvals[0]
	case ">":
		return s.max <= up.fvals[0]
	case ">=":
		return s.max < up.fvals[0]
	}
	return false
}

// estCandidates estimates how many records in the indexed prefix this probe would
// admit (its candidate-bitmap cardinality). Used only to order probes, so a rough
// estimate is fine; it never affects correctness.
func (si *segIndex) estCandidates(up usableProbe) float64 {
	s := si.statsFor(up)
	if s == nil {
		return math.MaxFloat64 // unknown: apply last
	}
	indexable := float64(s.covered - s.exc)
	switch up.op {
	case "==", "in":
		var sum float64
		if up.cat {
			for _, v := range up.svals {
				sum += s.estEqualStr(v)
			}
		} else {
			for _, v := range up.fvals {
				sum += s.estEqualFloat(v)
			}
		}
		return sum + float64(s.exc)
	case "!=":
		// Everything except the single excluded value (plus absent/exc rows that the
		// re-verify handles): close to the full prefix, so it sorts late.
		if up.cat {
			return indexable - s.estEqualStr(up.svals[0]) + float64(s.exc)
		}
		return indexable - s.estEqualFloat(up.fvals[0]) + float64(s.exc)
	case "<", "<=", ">", ">=":
		return s.estRange(up.op, up.fvals[0])*indexable + float64(s.exc)
	}
	return indexable
}

// estEqualStr / estEqualFloat estimate the record count for one equality value:
// its exact top-N count if it is a heavy hitter, else the average tail count (0 if
// the bloom, for a categorical, proves the value absent). Kept as two typed
// helpers so the hot ordering path boxes nothing.
func (s *segStats) estEqualStr(v string) float64 {
	for _, e := range s.top {
		if e.skey == v {
			return float64(e.count)
		}
	}
	if s.bloom != nil && !s.bloom.mayContain(hashString(v)) {
		return 0
	}
	return s.avgTailCount()
}

func (s *segStats) estEqualFloat(v float64) float64 {
	for _, e := range s.top {
		if e.fkey == v {
			return float64(e.count)
		}
	}
	return s.avgTailCount()
}

// estRange returns the estimated fraction of indexable records passing a range
// comparison against threshold t, by linear interpolation over [min,max].
func (s *segStats) estRange(op string, t float64) float64 {
	if !s.hasRange || s.max <= s.min {
		if cmpFloat(op, s.min, t) {
			return 1
		}
		return 0
	}
	frac := (t - s.min) / (s.max - s.min)
	if frac < 0 {
		frac = 0
	} else if frac > 1 {
		frac = 1
	}
	switch op {
	case "<", "<=":
		return frac
	case ">", ">=":
		return 1 - frac
	}
	return 1
}

// skipsPrefix reports whether the whole indexed prefix can be skipped for this
// query: the candidate set is the intersection of the per-probe candidate sets, so
// if any single probe provably has no candidate (canSkip), the intersection is
// empty. Cheaper than candidateOffsets for range probes (no key iteration) and the
// only skip path once postings are dropped from an immutable segment.
func (si *segIndex) skipsPrefix(usable []usableProbe) bool {
	for _, up := range usable {
		if si.canSkip(up) {
			return true
		}
	}
	return false
}

// probeOffsets returns a fresh, mutable offset bitmap for one probe.
func (si *segIndex) probeOffsets(up usableProbe) *roaring.Bitmap {
	if up.cat {
		cp := si.cat[up.attrID]
		switch up.op {
		case "==", "in":
			bm := roaring.New()
			for _, s := range up.svals {
				if p := cp.post[s]; p != nil {
					bm.Or(p)
				}
			}
			bm.Or(cp.exc)
			return bm
		case "!=":
			bm := si.all.Clone()
			if p := cp.post[up.svals[0]]; p != nil {
				bm.AndNot(p)
			}
			return bm
		case "present": // attr isnt undefined: posted a value, or present-but-exceptional
			bm := cp.posted.Clone()
			bm.Or(cp.exc)
			return bm
		case "absent": // attr is undefined: everything but the definitely-posted records
			bm := si.all.Clone()
			bm.AndNot(cp.posted)
			return bm
		case "is": // =?= exact (case-sensitive)
			if e := cp.exactBitmap(up.svals[0]); e != nil {
				return e.Clone()
			}
			return roaring.New()
		case "isnt": // =!= exact: everything but the exact-case matches
			bm := si.all.Clone()
			if e := cp.exactBitmap(up.svals[0]); e != nil {
				bm.AndNot(e)
			}
			return bm
		}
		return roaring.New()
	}
	vp := si.val[up.attrID]
	switch up.op {
	case "==", "in":
		bm := roaring.New()
		for _, f := range up.fvals {
			if p := vp.post[f]; p != nil {
				bm.Or(p)
			}
		}
		bm.Or(vp.exc)
		return bm
	case "!=":
		bm := si.all.Clone()
		if p := vp.post[up.fvals[0]]; p != nil {
			bm.AndNot(p)
		}
		return bm
	case "present":
		bm := vp.posted.Clone()
		bm.Or(vp.exc)
		return bm
	case "absent":
		bm := si.all.Clone()
		bm.AndNot(vp.posted)
		return bm
	case "<", "<=", ">", ">=":
		bm := roaring.New()
		t := up.fvals[0]
		for k, p := range vp.post {
			if cmpFloat(up.op, k, t) {
				bm.Or(p)
			}
		}
		bm.Or(vp.exc)
		return bm
	}
	return roaring.New()
}

func cmpFloat(op string, k, t float64) bool {
	switch op {
	case "<":
		return k < t
	case "<=":
		return k <= t
	case ">":
		return k > t
	case ">=":
		return k >= t
	}
	return false
}

// scanShardIndexed yields the shard's matching ads using each segment's index to
// visit only candidate records, and full-scanning any records the index does not
// cover (a segment with no index, or the tail beyond its build watermark). Every
// visited record is MVCC-visibility filtered and full-query re-verified, so the
// result is identical to a full scan. Returns false if the consumer stopped.
func (c *Collection) scanShardIndexed(sh *shard, usable []usableProbe, qp queryPlan, emit func(w []byte) bool) bool {
	return c.scanShardCandidates(sh, usable, func(w []byte) bool {
		if !matchWire(w, qp) {
			return true // not a match: keep scanning
		}
		return emit(w)
	})
}

// scanShardCandidates visits the candidate records for `usable` in one shard,
// handing each candidate's decompressed wire bytes to onCand (which returns false to
// stop the whole scan). Windows whose per-segment index does not cover the probes,
// and the un-indexed tail of those that do, are full-scanned -- so onCand sees a
// superset of the true candidates and the caller must re-verify. Returns false if
// onCand asked to stop. This is the shared candidate-enumeration used by both the
// indexed Query path (scanShardIndexed) and index-pre-filtered Match.
func (c *Collection) scanShardCandidates(sh *shard, usable []usableProbe, onCand func(w []byte) bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	// visit tests one record's visibility and hands its decompressed wire bytes to
	// onCand; returns stop = true when the consumer asked to stop.
	visit := func(w segWindow, o uint32) (stop bool) {
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			return false
		}
		ww, err := w.codec.Decompress(dbuf[:0], recAd(w.data, o))
		if err != nil {
			return false
		}
		dbuf = ww
		return !onCand(ww)
	}
	// scanRange full-scans records in [from, to).
	scanRange := func(w segWindow, from, to int) bool {
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if visit(w, o) {
				return false
			}
			off += int(total)
		}
		return true
	}

	for _, w := range wins {
		si := w.seg.idx.Load()
		if si == nil || !si.covers(usable) {
			if !scanRange(w, 0, w.used) { // no usable index: full-scan the window
				return false
			}
			continue
		}
		// Segment skip: if any probe provably has no candidate in the indexed prefix
		// (min/max out of range, bloom miss), the conjunction is empty there — skip
		// the prefix and only full-scan the tail written after the index was built.
		if si.skipsPrefix(usable) {
			if int(si.upto) < w.used {
				if !scanRange(w, int(si.upto), w.used) {
					return false
				}
			}
			continue
		}
		// Indexed prefix [0, upto): visit only candidate offsets.
		if cand := si.candidateOffsets(usable); cand != nil {
			it := cand.Iterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
		// Tail [upto, used): written after the index was built — full-scan it.
		if int(si.upto) < w.used {
			if !scanRange(w, int(si.upto), w.used) {
				return false
			}
		}
	}
	return true
}

// scanShardIndexedGroups is scanShardIndexed for a DNF plan: it visits the union of
// the groups' candidates and re-verifies each against the full query.
func (c *Collection) scanShardIndexedGroups(sh *shard, groups [][]usableProbe, qp queryPlan, emit func(w []byte) bool) bool {
	return c.scanShardCandidatesGroups(sh, groups, func(w []byte) bool {
		if !matchWire(w, qp) {
			return true
		}
		return emit(w)
	})
}

// scanShardCandidatesGroups is scanShardCandidates for a DNF plan (vm.ProbePlan with
// more than one group): per window it visits the union over groups of each group's
// candidate intersection, so onCand sees a superset of the union's true matches (the
// caller re-verifies). Segment skipping is not applied here -- a segment is skippable
// only when EVERY disjunct is empty, and visiting a few extra candidates is cheaper
// than proving that per group -- but a window whose index does not cover all groups,
// and every covered window's un-indexed tail, are still full-scanned.
func (c *Collection) scanShardCandidatesGroups(sh *shard, groups [][]usableProbe, onCand func(w []byte) bool) bool {
	s0, wins := sh.snapshot()
	defer releaseWindows(wins)
	var dbuf []byte
	visit := func(w segWindow, o uint32) (stop bool) {
		if !(recSeq(w.data, o) <= s0 && recSuperseded(w.data, o) > s0) {
			return false
		}
		ww, err := w.codec.Decompress(dbuf[:0], recAd(w.data, o))
		if err != nil {
			return false
		}
		dbuf = ww
		return !onCand(ww)
	}
	scanRange := func(w segWindow, from, to int) bool {
		for off := from; off < to; {
			o := uint32(off)
			total := recTotalLen(w.data, o)
			if total == 0 {
				break
			}
			if visit(w, o) {
				return false
			}
			off += int(total)
		}
		return true
	}

	for _, w := range wins {
		si := w.seg.idx.Load()
		if si == nil || !si.coversGroups(groups) {
			if !scanRange(w, 0, w.used) { // index can't serve every disjunct: full-scan
				return false
			}
			continue
		}
		// Single group (a conjunctive plan): reuse the segment-skip fast path -- if a
		// probe provably has no candidate in the indexed prefix, only the tail needs a
		// scan. A union of groups cannot skip this cheaply, so it visits candidates.
		if len(groups) == 1 && si.skipsPrefix(groups[0]) {
			if int(si.upto) < w.used {
				if !scanRange(w, int(si.upto), w.used) {
					return false
				}
			}
			continue
		}
		if cand := si.candidateOffsetsGroups(groups); cand != nil {
			it := cand.Iterator()
			for it.HasNext() {
				if visit(w, it.Next()) {
					return false
				}
			}
		}
		if int(si.upto) < w.used {
			if !scanRange(w, int(si.upto), w.used) {
				return false
			}
		}
	}
	return true
}
