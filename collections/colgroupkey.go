package collections

import (
	"math"
	"strings"

	"github.com/PelicanPlatform/classad/classad"
)

// The identity of a group, for a GROUP BY of one column or of several.
//
// A group is identified by a TUPLE of component values, one per group column. The reference -- the
// record scan -- renders each component to text and joins them, so two records are in the same
// group exactly when every component renders alike. This file holds the storage-side counterpart:
// a fixed-size, POINTER-FREE key that the runtime hashes and compares as plain memory.
//
// Pointer-free is the property, not an accident. A key with a string in it makes the map hash and
// compare it field by field, string hash included, even when the string is empty -- and a group
// lookup happens once per RECORD, so that is the per-record cost of grouping. A numeric GROUP BY
// was measured 55% slower when the key gained a string field, which is why this key carries a
// query-local ID for a string and not the string. TestGroupKeyIsPlainMemory pins it.
//
// THE IDENTITY CONTRACT, which is what makes the fast path safe where the two could disagree:
//
//	packed(a) == packed(b)  =>  text(a) == text(b)
//
// The packed key is a REFINEMENT of the scan's rendered identity, never a coarsening. The converse
// is allowed to fail -- an integer 42 and the string "42" are two keys here and one group in the
// scan -- and where it does, the db layer's render-and-merge step folds the two keys into one row
// by rendering them with the very function the scan uses. A coarsening has no such repair: it would
// report two of the scan's groups as one and there is nothing downstream that could tell.
//
// The sharp direction is strings. The block dictionaries in this package are FOLD-ORDERED and the
// comparison path works in fold ranges, so keying a string component by its folded form, or by the
// fold range it lands in, is the natural mistake -- and it would merge "u07" with "U07", which the
// scan keeps as two groups. Fold is for ORDERING AND PRUNING ONLY, never for identity: the intern
// table below is keyed on the EXACT BYTES. TestGroupKeyStringsAreExactBytes pins it.

// maxGroupCols is how many GROUP BY columns one packed key holds.
//
// Past it the columnar path DECLINES and the query goes back to the scan -- which is what EVERY
// multi-column group did before this, so a cap costs nothing that was not already being paid. It
// never truncates: a truncated key would merge groups that differ only in a component the key
// dropped, and report that merge as an answer.
//
// Two, and the number is measured rather than chosen. Every slot costs EVERY query, because the key
// is hashed and compared once per record whatever it groups by, and a single group column over a
// small group set is a couple of nanoseconds a record -- a path where the key's width is most of
// the work. Against main, on the one-column arms of BenchmarkGroupAccum:
//
//	slots  key size   raw_cycle4 (4 groups)   bucket_scattered (30 buckets, memo always misses)
//	    2   24 bytes                  +3.7%                                              -0.3%
//	    4   40 bytes                 +27.5%                                             +11.3%
//
// Two covers the shape this change exists for -- one dimension beside one time bucket, "per owner
// per day" -- and the shape the index tier already recognizes. A third column is a four-byte-wider
// key for every query in the system to buy a shape nothing asks yet; when something does, raising
// this is one line, and the table above is the measurement to repeat.
const maxGroupCols = 2

// MaxGroupCols is maxGroupCols, for the db layer's eligibility test. A caller that groups by more
// columns than this must not be handed a truncated key, so it has to know where the limit is rather
// than discovering it as a decline it cannot explain.
const MaxGroupCols = maxGroupCols

// groupValKind is the set of scalar shapes a group component can take. It is deliberately NOT
// adKind: adKind describes what a column can STORE, and a group value also has to cover what a
// column can only report -- an absent attribute, an explicit UNDEFINED or ERROR -- because the scan
// this must agree with emits a group for each of those (ValueText renders them "undefined" /
// "error").
type groupValKind uint8

const (
	gkInt    groupValKind = iota // the zero value, so a bare component is a zero-valued integer
	gkReal                       // raw holds math.Float64bits
	gkString                     // raw holds a query-local id into groupAccum.strVal
	gkBool                       // raw is 0 or 1
	gkUndef                      // the attribute is absent, or holds UNDEFINED: one group either way
	gkError
)

// groupKey identifies one group: per component, the kind and a raw int64 that identifies the value
// within that kind -- the value itself for an integer, Float64bits for a real, 0/1 for a boolean, a
// query-local id for a string, and 0 for undefined and error.
//
// The KIND is part of the key and may never be dropped. Without it an integer 1 and the boolean
// TRUE share a raw of 1 and would collide into one group, while the scan renders them "1" and
// "true" -- a coarsening, and the one the contract above forbids. TestGroupKeyKindSeparatesRawBits
// pins it.
//
// Components past the query's own count stay zero. They are constant for the whole query, so they
// cost a few bytes of hash and never distinguish two groups.
type groupKey struct {
	raw  [maxGroupCols]int64
	kind [maxGroupCols]groupValKind
}

// groupComp is one component of a group key as read from a column, already resolved: a string has
// become its query-local id, so this is pointer-free too and packing it is a pair of stores.
type groupComp struct {
	raw  int64
	kind groupValKind
}

// number returns the component's numeric value, which is what a bucket floors.
//
// A boolean is deliberately NOT a number here, matching db.bucketKeyText: it runs number(v), which
// rejects a ClassAd boolean, so the scan drops a boolean from a bucketed series rather than
// bucketing it as 0 or 1.
func (c groupComp) number() (float64, bool) {
	switch c.kind {
	case gkInt:
		return float64(c.raw), true
	case gkReal:
		return math.Float64frombits(uint64(c.raw)), true
	}
	return 0, false
}

// compOf turns a numeric column value into a key component. A boolean reaches this only from a cold
// tail (a numeric column's slot holds numbers), and keeps its kind so it renders "true"/"false" as
// the scan does rather than as the 1/0 the aggregates coerce it to.
func compOf(nv colVal) groupComp {
	switch nv.kind {
	case akReal:
		return groupComp{raw: int64(math.Float64bits(nv.f)), kind: gkReal}
	case akBool:
		return groupComp{raw: nv.i, kind: gkBool}
	}
	return groupComp{raw: nv.i, kind: gkInt}
}

// groupAccum is the accumulation map plus the one thing every tier has to agree on: how a tuple of
// group values becomes a group key. Bundling them is what stops one tier forming a raw key while
// its sibling forms a bucketed one, which would split a group in two and read as a plain wrong
// count.
//
// A bucket width > 0 on a component floors that component into fixed-width buckets -- the shape a
// time series asks for, where the group is the day rather than the second. Components are bucketed
// independently, so "per owner per day" buckets the time column and leaves the owner alone.
type groupAccum struct {
	acc map[groupKey]*groupAcc
	n   int // how many components a key of this query carries

	bucket [maxGroupCols]int64 // per component; 0 = group by the raw value

	// The component indices, split by whether they are bucketed, so the per-record loops iterate
	// only the components they apply to rather than testing every slot. Both are fixed at
	// construction and never change.
	rawIdx [maxGroupCols]uint8
	rawN   int
	bktIdx [maxGroupCols]uint8
	bktN   int

	// The query-local string table. A string component's raw is an index into strVal; strID maps
	// the EXACT BYTES back to that index (see the fold warning at the top of this file).
	//
	// One entry per distinct string value per QUERY, not per group and not per record: with several
	// group columns the same owner name appears in many group tuples and interns once. A LOOKUP
	// never retains the caller's string -- which is what lets the hot path hand it a view aliasing
	// a block's decompressed region -- and an INSERT clones, once per distinct value.
	strID  map[string]int64
	strVal []string

	// The bucket interval memo: the previous record's UNBUCKETED components, the half-open
	// interval of VALUES that fell in each bucketed component's bucket, and the resulting
	// accumulator. Only meaningful when some component is bucketed.
	//
	// Records arrive in insertion order and a bucketed group column advances slowly against its
	// width -- an hour bucket over a per-second timestamp holds thousands of consecutive records --
	// so the next record is overwhelmingly in the bucket the last one was, under the same owner.
	// Remembering the INTERVAL rather than the resulting key is what makes that worth having: the
	// per-record cost of a bucketed group is the float divide and floor in bucketOf, which runs
	// before any map is touched, so a memo on the finished key would still pay for the divide. A
	// compare or two replaces the divide, the floor and the map probe together.
	//
	// mHi is pulled a few ulps BELOW the interval's true top edge, because bucketOf reaches its
	// bucket through a float division whose rounding can place a value within about an ulp of that
	// edge in the next bucket. Admitting slightly less than the real interval only sends those few
	// values down the slow path, where bucketOf itself decides; admitting slightly more would group
	// one of them differently from every other tier.
	mKey     groupKey // the unbucketed components only; bucketed slots stay zero
	mCmp     bool     // ... and whether there are any, so a lone bucketed column skips the compare
	mLo, mHi [maxGroupCols]float64
	mAcc     *groupAcc

	// The per-record key scratch, held here rather than declared per call.
	//
	// A fresh `var key groupKey` is a key's worth of zeroing on every record, and the paths that
	// pay it are the ones where the accumulation map is small enough to sit in L1 -- a status
	// histogram, a daily series -- so it is pure overhead against a probe that costs almost
	// nothing. Reusing one value works because the slots past the query's component count are
	// never written (they start zero) and the rest are overwritten every record.
	key groupKey
}

// bucketMemoMargin is how far inside its top edge a bucket interval has to be for the memo to decide
// a value, as a fraction of the edge. bucketOf's quotient is off by at most half an ulp, which is
// worth at most the edge times 2^-53 once it is scaled back up, so eight times that is margin to
// spare. At the magnitudes a timestamp or a counter has, the excluded band is a fraction of a second
// wide and no ordinary value is ever in it; TestBucketMemoDeclinesTheCoarseTopEdge covers one that
// is.
const bucketMemoMargin = 1.0 / (1 << 50)

// newGroupAccum builds the accumulator for a query grouping by len(buckets) columns, where
// buckets[i] is component i's bucket width (0 = the raw value).
func newGroupAccum(buckets []int64) *groupAccum {
	a := &groupAccum{acc: map[groupKey]*groupAcc{}, n: len(buckets)}
	for i, b := range buckets {
		a.bucket[i] = b
		if b > 0 {
			a.bktIdx[a.bktN] = uint8(i)
			a.bktN++
			continue
		}
		a.rawIdx[a.rawN] = uint8(i)
		a.rawN++
	}
	// With nothing unbucketed the memo's key probe is the zero value on both sides, so comparing
	// it would be work that can never decide anything. That is the lone bucketed column -- the
	// shape the memo was built for -- so it is worth the flag.
	a.mCmp = a.rawN > 0
	return a
}

// internStr returns s's query-local id, assigning one if this is the first time the query has seen
// these EXACT BYTES. Two strings that differ only in case are two ids, which is what the scan does
// with them; see the fold warning at the top of this file.
func (a *groupAccum) internStr(s string) int64 {
	if id, ok := a.strID[s]; ok {
		return id
	}
	// The caller's string may alias a block's decompressed region, and the table outlives the
	// block -- it is read back when the result is shaped, after the block cache may have evicted
	// and re-decompressed that region into a different array. Copy on INSERT only: once per
	// distinct value for the whole query, never once per record.
	cs := strings.Clone(s)
	id := int64(len(a.strVal))
	a.strVal = append(a.strVal, cs)
	if a.strID == nil {
		a.strID = make(map[string]int64, 16)
	}
	a.strID[cs] = id
	return id
}

// strComp returns the key component for a string value.
func (a *groupAccum) strComp(s string) groupComp {
	return groupComp{raw: a.internStr(s), kind: gkString}
}

// accFor returns the accumulator for the group comps[:n] names, counting the record against it and
// creating the group when it is new.
//
//	g != nil, ok     accumulate this record's aggregates into g
//	g == nil, ok     this record is in no group; skip it
//
// The second only happens under a bucket. Only a FINITE number can be floored, so a string,
// boolean, undefined, error, NaN or infinity has no bucket to land in -- and db.bucketKeyText drops
// such a record from the series rather than bucketing it as zero, so this drops it too. With
// several group columns ONE unbucketable component drops the whole record, because the reference
// drops the row as soon as any of its bucket keys fails to render.
func (a *groupAccum) accFor(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	// ONE group column gets a straight-line path, bucketed and not. It is the shape almost every
	// query asks, it costs a couple of nanoseconds per record, and at that size the structure of
	// the code is most of the cost: routing it through the N-component form -- a second call, a
	// loop over the bucketed component indices, indexed loads of the interval instead of direct
	// ones -- was measured at +55% on a bucketed column and +32% on a raw one against main, on a
	// path whose whole purpose is to be a few instructions. Both branches below do exactly what the
	// general code does for n == 1, with the loops unrolled; enterBucket1 is the same split one
	// level down, for the records the memo cannot decide.
	if a.n == 1 {
		if a.bktN == 0 {
			k := &a.key
			k.raw[0], k.kind[0] = comps[0].raw, comps[0].kind
			g := a.acc[*k]
			if g == nil {
				g = newGroupAcc(nAgg)
				a.acc[*k] = g
			}
			g.n++
			return g, true
		}
		// The type test stays ahead of the memo: a string, boolean, undefined or error has no
		// number at all, so it is in no bucket however near the last one its raw bits sit.
		f, isNum := comps[0].number()
		if !isNum {
			return nil, true
		}
		// Positive containment, never `f < lo || f >= hi` -- see inMemoIntervals for why a NaN
		// makes the negated form hand out the previous record's bucket.
		if g := a.mAcc; g != nil && a.mLo[0] <= f && f < a.mHi[0] {
			g.n++
			return g, true
		}
		return a.enterBucket1(f, nAgg)
	}
	if a.bktN > 0 {
		return a.accForBucketed(comps, nAgg)
	}
	key := &a.key
	for i := 0; i < a.n; i++ {
		key.raw[i], key.kind[i] = comps[i].raw, comps[i].kind
	}
	// No memo without a bucket. The same one-entry memo on the raw KEY was measured here and did
	// not pay: an unbucketed group column repeats its value only when the records themselves do,
	// which is a far shorter run than a bucket's, and the key compare it saves is a map probe
	// rather than the divide a bucket costs. It was a few percent either way on a column whose
	// values run in short blocks and a steady loss on every column that cycles.
	g := a.acc[*key]
	if g == nil {
		g = newGroupAcc(nAgg)
		a.acc[*key] = g
	}
	g.n++
	return g, true
}

// accForBucketed is accFor where at least one component is floored into buckets: it tries the
// interval memo first and falls to enterBuckets, which does the arithmetic and re-arms it.
func (a *groupAccum) accForBucketed(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	// Every component bucketed -- two time buckets, say -- means no unbucketed component, so the
	// memo key is the zero key on both sides and there is nothing to compare. Split out rather than
	// folded into the general case, because the general case has to BUILD a key to compare, and
	// building one is a key's worth of writes on a path whose whole job is to replace a float
	// divide with two compares. (A LONE bucketed column never reaches here: accFor serves n == 1
	// straight-line.)
	if !a.mCmp {
		if g := a.mAcc; g != nil && a.inMemoIntervals(comps) {
			g.n++
			return g, true
		}
		return a.enterBuckets(comps, nAgg, groupKey{})
	}
	// The unbucketed components, which the memo has to match exactly. Bucketed slots stay zero in
	// the probe so one struct compare answers for all of them at once.
	var probe groupKey
	for _, i := range a.rawIdx[:a.rawN] {
		probe.raw[i], probe.kind[i] = comps[i].raw, comps[i].kind
	}
	if g := a.mAcc; g != nil && probe == a.mKey && a.inMemoIntervals(comps) {
		g.n++
		return g, true
	}
	return a.enterBuckets(comps, nAgg, probe)
}

// inMemoIntervals reports whether every bucketed component falls in the interval the memo remembers
// for it.
//
// The containment test is written in its POSITIVE form and negated, never as `f < lo || f >= hi`.
// Every comparison against a NaN is false, so the negated form reads a NaN as "neither below nor
// above, therefore inside" and hands it the previous record's bucket -- a NaN counted as an ordinary
// day, which is exactly what bucketOf refuses to let happen. The positive form is false for a NaN,
// so it falls through to bucketOf, which is the only thing that decides a value is in no bucket.
func (a *groupAccum) inMemoIntervals(comps *[maxGroupCols]groupComp) bool {
	for _, i := range a.bktIdx[:a.bktN] {
		f, isNum := comps[i].number()
		if !isNum || !(a.mLo[i] <= f && f < a.mHi[i]) {
			return false
		}
	}
	return true
}

// enterBuckets floors every bucketed component, finds the resulting group's accumulator and arms the
// interval memo with it. It is accFor's bucketed slow path: reached on the first record of each
// bucket, and on any record the memo declines to decide.
//
// Kept out of accFor rather than written inline, because accFor's hot path is a handful of
// instructions and the code around it is what decides how they are laid out: folding this in cost
// the UNBUCKETED path measurably while doing nothing for the bucketed one, which runs this once per
// bucket.
func (a *groupAccum) enterBuckets(comps *[maxGroupCols]groupComp, nAgg int, probe groupKey) (*groupAcc, bool) {
	// probe already carries the unbucketed components (and is the zero key when there are none,
	// which is the only case the caller leaves it unfilled), so the key is built by filling in the
	// bucketed slots beside them.
	key := probe
	// The intervals go into locals and are committed only once every component has bucketed. A
	// record that turns out to be in NO bucket returns partway through, and it has to leave the memo
	// exactly as it found it: writing the intervals in place would strand fresh ones beside the
	// PREVIOUS record's accumulator, which a later record would then hit on. Leaving it armed is
	// also what keeps a run of unbucketable records interleaved with ordinary ones from restarting
	// the bucket on every one of them.
	var lo, hi [maxGroupCols]float64
	for _, i := range a.bktIdx[:a.bktN] {
		f, isNum := comps[i].number()
		if !isNum {
			return nil, true // no number to floor: the record leaves the series
		}
		// The floor goes through float64 and renders as an integer because that is what
		// db.bucketKeyText does: a bucket start is a whole number whatever the column's own type,
		// so an int column and a real column land in the same group. Sharing the arithmetic rather
		// than writing it twice is what keeps the two paths from disagreeing.
		//
		// A number can still be in no bucket: a NaN or an infinity lies in no half-open interval
		// [b, b+width), so bucketOf declines it and the record leaves the series the same way a
		// string does. See bucketOf for why that beats letting the int64 cast invent a bucket.
		b, inBucket := bucketOf(f, a.bucket[i])
		if !inBucket {
			return nil, true
		}
		key.raw[i], key.kind[i] = b, gkInt
		lo[i] = float64(b)
		hi[i] = lo[i] + float64(a.bucket[i])
	}
	g := a.acc[key]
	if g == nil {
		g = newGroupAcc(nAgg)
		a.acc[key] = g
	}
	g.n++
	for _, i := range a.bktIdx[:a.bktN] {
		a.mLo[i], a.mHi[i] = lo[i], hi[i]-math.Abs(hi[i])*bucketMemoMargin
	}
	a.mKey, a.mAcc = probe, g
	return g, true
}

// enterBucket1 is enterBuckets for a single bucketed group column: floor f, find the bucket's
// accumulator, arm the memo. Written out rather than routed through the N-component form for the
// reason accFor is: this runs once per record on a column whose buckets change often (a timestamp
// scattered across days rather than advancing through them), and at a couple of nanoseconds a
// record the loop over component indices and the key copy the general form needs are most of the
// cost.
func (a *groupAccum) enterBucket1(f float64, nAgg int) (*groupAcc, bool) {
	// The floor goes through float64 and renders as an integer because that is what
	// db.bucketKeyText does: a bucket start is a whole number whatever the column's own type, so an
	// int column and a real column land in the same group. A number can still be in NO bucket -- a
	// NaN or an infinity lies in no half-open interval [b, b+width) -- and then the record leaves
	// the series the same way a string does.
	b, inBucket := bucketOf(f, a.bucket[0])
	if !inBucket {
		return nil, true
	}
	k := &a.key
	k.raw[0], k.kind[0] = b, gkInt
	g := a.acc[*k]
	if g == nil {
		g = newGroupAcc(nAgg)
		a.acc[*k] = g
	}
	g.n++
	lo := float64(b)
	hi := lo + float64(a.bucket[0])
	a.mLo[0], a.mHi[0] = lo, hi-math.Abs(hi)*bucketMemoMargin
	a.mAcc = g
	return g, true
}

// values renders a key back to the group's component values, in request order. The string table is
// the accumulator's, so this is the only place a string id becomes a string again -- and the id is
// an index, so it cannot name a value the query did not intern.
func (a *groupAccum) values(k groupKey) []classad.Value {
	out := make([]classad.Value, a.n)
	for i := 0; i < a.n; i++ {
		switch k.kind[i] {
		case gkReal:
			out[i] = classad.NewRealValue(math.Float64frombits(uint64(k.raw[i])))
		case gkString:
			out[i] = classad.NewStringValue(a.strVal[k.raw[i]])
		case gkBool:
			out[i] = classad.NewBoolValue(k.raw[i] != 0)
		case gkUndef:
			out[i] = classad.NewUndefinedValue()
		case gkError:
			out[i] = classad.NewErrorValue()
		default:
			out[i] = classad.NewIntValue(k.raw[i])
		}
	}
	return out
}

// groupAcc is one group's accumulation: the record count plus one aggregate accumulator per
// requested aggregate column.
type groupAcc struct {
	n     int
	stats []statsAccum
}

func newGroupAcc(nAgg int) *groupAcc {
	g := &groupAcc{}
	if nAgg > 0 {
		g.stats = make([]statsAccum, nAgg)
		for i := range g.stats {
			g.stats[i] = newStatsAccum()
		}
	}
	return g
}
