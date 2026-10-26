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
//
// ONE KEY TYPE PER ARITY, which is why there are three of them below.
//
// The key is hashed and compared once per RECORD whatever the query groups by, so a key wide enough
// for the widest GROUP BY would charge its width to the narrowest. It used to: one fixed-width key
// held every arity, and widening it from two slots to four cost +27.5% on a one-column raw group
// and +11.3% on a one-column bucketed one. That is what capped the number of group columns at the
// width every query could afford to pay.
//
// Narrowing the key to the arity in hand is worth much of what widening it cost. A 16-byte
// one-component key against the two-slot one, with nothing else changed, measured -1% to -16% on
// the one-column arms, and the shipped form holds most of that. Against the two-slot key on
// BenchmarkGroupAccum, min-of-15 and median:
//
//	arm                               min      median
//	raw_str_unique                 -11.4%      -11.1%
//	bucket_run1                    -10.8%      -11.4%
//	raw_run4                        -7.9%       -8.4%
//	raw_cycle4                      -3.4%       -7.1%
//	raw_str_cycle256                +0.3%       -3.1%
//	bucket_scattered                +1.2%       +0.4%
//	bucket_run3600                  +3.2%       +3.7%
//	bucket_76groups                 +5.4%       +5.0%
//
// So each arity gets its own comparable key type and its OWN accumulation map, chosen once per
// query from the number of group columns rather than per record, and a one-column GROUP BY pays for
// one component.
//
// The two arms that LOSE are the ones where the key is never touched. A bucketed column whose runs
// are long answers almost every record from the interval memo below, which decides before any map
// is reached -- so there is no key to narrow there, and what is left is the layout the bigger
// accumulator falls into. The instruction sequence on that path is the same one the fixed-width key
// compiled to, instruction for instruction; the 3-6% is where those instructions sit. Two builds of
// equivalent code measured +-2.3% apart on these arms on the machine the table was taken on, so it
// is a real effect rather than noise, and it is the price of the rest of the table.

// maxGroupCols is the highest GROUP BY arity that has a key type here.
//
// Past it the columnar path DECLINES and the query goes back to the scan. It never truncates: a
// truncated key would merge groups that differ only in a component the key dropped, and report that
// merge as an answer.
//
// Three. Each arity is a separate type and a separate map, so a fourth does not widen anything the
// first three hash or compare -- the measurement that justified the old cap of two does not apply
// to this one. It is not quite free either: the accumulator holds one key scratch and one memo
// probe per arity and the per-component arrays are sized by the cap, and raising the cap from two
// to three was measured at about 5% on the two-column bucketed arm whose records almost all answer
// from the interval memo, with the same cap held at two showing no change on it at all. That is
// layout, not key width, and it buys a column that previously fell to the record scan -- two orders
// of magnitude -- so it is worth paying once. It is not worth paying repeatedly for shapes nothing
// asks: one dimension beside one time bucket is what a dashboard draws, and a third column covers
// "per owner per site per day". Raising it further is a key type, a case in each switch and a fuzz
// shape that reaches it, plus that measurement repeated.
const maxGroupCols = 3

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

// The packed key types, one per arity. Each identifies one group: per component, the kind and a raw
// int64 that identifies the value within that kind -- the value itself for an integer, Float64bits
// for a real, 0/1 for a boolean, a query-local id for a string, and 0 for undefined and error.
//
// The KIND is part of every key and may never be dropped. Without it an integer 1 and the boolean
// TRUE share a raw of 1 and would collide into one group, while the scan renders them "1" and
// "true" -- a coarsening, and the one the contract above forbids. TestGroupKeyKindSeparatesRawBits
// pins it.
//
// A one-component key is a pair of scalars rather than a pair of length-1 arrays. It is the same 16
// bytes either way, but the arrays make the packing an indexed store and the unpacking an indexed
// load on the path where there is exactly one of each.
type groupKey1 struct {
	raw  int64
	kind groupValKind
}

type groupKey2 struct {
	raw  [2]int64
	kind [2]groupValKind
}

type groupKey3 struct {
	raw  [3]int64
	kind [3]groupValKind
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

// groupMap is one arity's accumulation state: the map itself plus the two key-shaped values the
// per-record paths need. Everything here is parameterised on the key type and nothing in it knows
// how a key is PACKED -- that differs per arity and is written out below, once each.
//
// key is the per-record scratch, held here rather than declared per call. A fresh `var key K` is a
// key's worth of zeroing on every record, and the paths that pay it are the ones where the
// accumulation map is small enough to sit in L1 -- a status histogram, a daily series -- so it is
// pure overhead against a probe that costs almost nothing. Reusing one value works because the
// slots past the query's component count are never written (they start zero) and the rest are
// overwritten every record.
//
// mKey is the bucket memo's probe: the previous record's UNBUCKETED components, with the bucketed
// slots left zero so one struct compare answers for all of them at once.
type groupMap[K comparable] struct {
	acc  map[K]*groupAcc
	key  K
	mKey K
}

// groupAccum is the accumulation map plus the one thing every tier has to agree on: how a tuple of
// group values becomes a group key. Bundling them is what stops one tier forming a raw key while
// its sibling forms a bucketed one, which would split a group in two and read as a plain wrong
// count.
//
// A bucket width > 0 on a component floors that component into fixed-width buckets -- the shape a
// time series asks for, where the group is the day rather than the second. Components are bucketed
// independently, so "per owner per day" buckets the time column and leaves the owner alone.
//
// Exactly ONE of g1/g2/g3 has a map, decided by n at construction: that is the per-arity choice,
// made once per query. Everything outside them -- the bucket widths, the component index lists, the
// memo's intervals and the string table -- is the same shape at every arity and is held once, and
// the fields the per-record paths touch are ordered so that the hottest of them share a line.
type groupAccum struct {
	// The fields the per-record paths read, first and together. The bucketed single-column hit --
	// the shape a time series asks, and the one the memo exists for -- reads n, bktN, mAcc and one
	// interval and touches nothing else, and in this order those are one cache line.
	n      int // how many components a key of this query carries; 0 = no key type fits, decline
	bktN   int
	rawN   int
	mCmp   bool // whether there are unbucketed components for mKey to match at all
	rawIdx [maxGroupCols]uint8
	bktIdx [maxGroupCols]uint8

	// The bucket interval memo: the half-open interval [mIv[i][0], mIv[i][1]) of VALUES that fell
	// in bucketed component i's bucket, and the accumulator they landed in. The unbucketed
	// components a hit also has to match are in the live groupMap's mKey, since those are
	// key-shaped. Only meaningful when some component is bucketed.
	//
	// Records arrive in insertion order and a bucketed group column advances slowly against its
	// width -- an hour bucket over a per-second timestamp holds thousands of consecutive records --
	// so the next record is overwhelmingly in the bucket the last one was, under the same owner.
	// Remembering the INTERVAL rather than the resulting key is what makes that worth having: the
	// per-record cost of a bucketed group is the float divide and floor in bucketOf, which runs
	// before any map is touched, so a memo on the finished key would still pay for the divide. A
	// compare or two replaces the divide, the floor and the map probe together -- and a HIT never
	// touches a map at all, which is why the key's width is a cost on misses only.
	//
	// Low and high sit beside each other, one pair per component, rather than in two arrays. The
	// test reads both, so a pair is two loads from one line however many components there are;
	// two arrays put them maxGroupCols*8 bytes apart and the pair straddled a line as soon as the
	// arrays grew.
	//
	// The top of each interval is pulled a few ulps BELOW its true edge, because bucketOf reaches
	// its bucket through a float division whose rounding can place a value within about an ulp of
	// that edge in the next bucket. Admitting slightly less than the real interval only sends those
	// few values down the slow path, where bucketOf itself decides; admitting slightly more would
	// group one of them differently from every other tier.
	mAcc *groupAcc
	mIv  [maxGroupCols][2]float64

	// Exactly ONE of these has a map, decided by n at construction: that is the per-arity choice,
	// made once per query. They sit directly behind the memo, ahead of everything a memo hit does
	// not read, because the per-record paths reach a key scratch or a memo probe and little else:
	// one unbucketed column reads n, the map header and the key, and two columns add the memo
	// probe. Moving the bucket widths out from among them was worth several percent on both.
	g1 groupMap[groupKey1]
	g2 groupMap[groupKey2]
	g3 groupMap[groupKey3]

	// Per component; 0 = group by the raw value. Read only when a bucket has to be ENTERED, which
	// is once per bucket rather than once per record, so it sits behind every field a memo hit
	// reads -- including the keys, where it was splitting the two-component memo probe across a
	// cache line.
	bucket [maxGroupCols]int64

	// The query-local string table. A string component's raw is an index into strVal; strID maps
	// the EXACT BYTES back to that index (see the fold warning at the top of this file).
	//
	// One entry per distinct string value per QUERY, not per group and not per record: with several
	// group columns the same owner name appears in many group tuples and interns once. A LOOKUP
	// never retains the caller's string -- which is what lets the hot path hand it a view aliasing
	// a block's decompressed region -- and an INSERT clones, once per distinct value.
	strID  map[string]int64
	strVal []string
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
//
// An arity no key type covers leaves n at zero and allocates nothing. Every accFor then returns
// ok=false, which is the caller's signal to give the query back to the record scan; no key is
// formed, so nothing can be truncated into one. The eligibility test callers run before reading
// anything (resolveGroupAttrs) refuses the same arities, so this is the backstop rather than the
// gate.
func newGroupAccum(buckets []int64) *groupAccum {
	a := &groupAccum{}
	if len(buckets) < 1 || len(buckets) > maxGroupCols {
		return a
	}
	a.n = len(buckets)
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
	switch a.n {
	case 1:
		a.g1.acc = map[groupKey1]*groupAcc{}
	case 2:
		a.g2.acc = map[groupKey2]*groupAcc{}
	case 3:
		a.g3.acc = map[groupKey3]*groupAcc{}
	}
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
//	g == nil, !ok    this cannot form the group: give the query back to the scan
//
// The second only happens under a bucket. Only a FINITE number can be floored, so a string,
// boolean, undefined, error, NaN or infinity has no bucket to land in -- and db.bucketKeyText drops
// such a record from the series rather than bucketing it as zero, so this drops it too. With
// several group columns ONE unbucketable component drops the whole record, because the reference
// drops the row as soon as any of its bucket keys fails to render.
//
// The third is the arity no key type covers. It is a DECLINE and never a truncation: an answer
// built from the components that did fit would merge groups that differ only in the ones that did
// not, and read as an ordinary count.
//
// a.n is fixed when the accumulator is built, so the dispatch below is a compare against a field
// that holds the same value for every record of the query -- perfectly predicted, and what the
// single-column special case already cost. Selecting a function value per query instead would turn
// it into an indirect call on a path whose whole body is a handful of instructions.
func (a *groupAccum) accFor(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	// ONE group column gets a straight-line path, bucketed and not. It is the shape almost every
	// query asks, it costs a couple of nanoseconds per record, and at that size the structure of
	// the code is most of the cost: routing it through the N-component form -- a second call, a
	// loop over the bucketed component indices, indexed loads of the interval instead of direct
	// ones -- was measured at +55% on a bucketed column and +32% on a raw one, on a path whose
	// whole purpose is to be a few instructions. Both branches below do exactly what the general
	// code does for n == 1, with the loops unrolled; enterBucket1 is the same split one level down,
	// for the records the memo cannot decide.
	if a.n == 1 {
		if a.bktN == 0 {
			m := &a.g1
			m.key.raw, m.key.kind = comps[0].raw, comps[0].kind
			g := m.acc[m.key]
			if g == nil {
				g = newGroupAcc(nAgg)
				m.acc[m.key] = g
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
		if g := a.mAcc; g != nil && a.mIv[0][0] <= f && f < a.mIv[0][1] {
			g.n++
			return g, true
		}
		return a.enterBucket1(f, nAgg)
	}
	// A bucketed component is the one shape with a slow path worth leaving the function for, and
	// the arity picks which. Dispatching straight to it, rather than through a per-arity accFor
	// that would then branch again, is a call frame per record: routing the two-column bucketed
	// shape through one extra frame was measured at +21% on the memo's best case.
	if a.bktN > 0 {
		switch a.n {
		case 2:
			return a.accForBucketed2(comps, nAgg)
		case 3:
			return a.accForBucketed3(comps, nAgg)
		}
		return nil, false // no key type for this arity: decline, never truncate
	}
	// The unbucketed multi-column forms, one function per arity.
	//
	// They are kept OUT of this function, even though each is half a dozen instructions, because
	// what they cost here is not their own size but accFor's: inlining both put two more map
	// probes in the body, grew the stack frame from 80 bytes to 96, and cost 4-7% on the
	// single-column arms whose own instructions had not changed at all. The frame and the layout
	// are paid by every record of every query; the extra call is paid only by the shape that
	// takes it.
	switch a.n {
	case 2:
		return a.accForRaw2(comps, nAgg)
	case 3:
		return a.accForRaw3(comps, nAgg)
	}
	return nil, false // no key type for this arity: decline, never truncate
}

// accForRaw2 is accFor for two unbucketed group columns.
//
// The packing is written out rather than looped, here and in accForRaw3, because it is the one part
// of the key that cannot be shared across arities: a two-slot key and a three-slot key have no
// common element type to index through, so a loop over them would have to go through an interface
// or a generic dictionary and give up the direct stores.
//
// No memo without a bucket. The same one-entry memo on the raw KEY was measured and did not pay: an
// unbucketed group column repeats its value only when the records themselves do, which is a far
// shorter run than a bucket's, and the key compare it saves is a map probe rather than the divide a
// bucket costs. It was a few percent either way on a column whose values run in short blocks and a
// steady loss on every column that cycles.
func (a *groupAccum) accForRaw2(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g2
	m.key.raw[0], m.key.kind[0] = comps[0].raw, comps[0].kind
	m.key.raw[1], m.key.kind[1] = comps[1].raw, comps[1].kind
	g := m.acc[m.key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[m.key] = g
	}
	g.n++
	return g, true
}

// accForRaw3 is accForRaw2 for three unbucketed group columns.
func (a *groupAccum) accForRaw3(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g3
	m.key.raw[0], m.key.kind[0] = comps[0].raw, comps[0].kind
	m.key.raw[1], m.key.kind[1] = comps[1].raw, comps[1].kind
	m.key.raw[2], m.key.kind[2] = comps[2].raw, comps[2].kind
	g := m.acc[m.key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[m.key] = g
	}
	g.n++
	return g, true
}

// accForBucketed2 is accFor2 where at least one component is floored into buckets: it tries the
// interval memo first and falls to enterBuckets2, which does the arithmetic and re-arms it.
func (a *groupAccum) accForBucketed2(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g2
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
		return a.enterBuckets2(comps, nAgg, groupKey2{})
	}
	// The unbucketed components, which the memo has to match exactly. Bucketed slots stay zero in
	// the probe so one struct compare answers for all of them at once.
	var probe groupKey2
	for _, i := range a.rawIdx[:a.rawN] {
		probe.raw[i], probe.kind[i] = comps[i].raw, comps[i].kind
	}
	if g := a.mAcc; g != nil && probe == m.mKey && a.inMemoIntervals(comps) {
		g.n++
		return g, true
	}
	return a.enterBuckets2(comps, nAgg, probe)
}

// accForBucketed3 is accForBucketed2 for three group columns.
func (a *groupAccum) accForBucketed3(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g3
	if !a.mCmp {
		if g := a.mAcc; g != nil && a.inMemoIntervals(comps) {
			g.n++
			return g, true
		}
		return a.enterBuckets3(comps, nAgg, groupKey3{})
	}
	var probe groupKey3
	for _, i := range a.rawIdx[:a.rawN] {
		probe.raw[i], probe.kind[i] = comps[i].raw, comps[i].kind
	}
	if g := a.mAcc; g != nil && probe == m.mKey && a.inMemoIntervals(comps) {
		g.n++
		return g, true
	}
	return a.enterBuckets3(comps, nAgg, probe)
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
		if !isNum || !(a.mIv[i][0] <= f && f < a.mIv[i][1]) {
			return false
		}
	}
	return true
}

// enterBuckets2 floors every bucketed component, finds the resulting group's accumulator and arms
// the interval memo with it. It is accForBucketed2's slow path: reached on the first record of each
// bucket, and on any record the memo declines to decide.
//
// Kept out of accForBucketed2 rather than written inline, because the hot path there is a handful of
// instructions and the code around it is what decides how they are laid out: folding this in cost
// the UNBUCKETED path measurably while doing nothing for the bucketed one.
//
// The floor loop is written out again in enterBuckets3 rather than shared. It cannot be shared as
// written -- it stores into the key, and a two-slot key and a three-slot key have no common element
// type -- and hoisting just the arithmetic into a helper that returns the bucket starts costs a call
// per record on the shape where every record enters a new bucket. What the two DO share is bucketOf
// itself, which is where agreeing with the scan on where a bucket edge lies is decided.
func (a *groupAccum) enterBuckets2(comps *[maxGroupCols]groupComp, nAgg int, probe groupKey2) (*groupAcc, bool) {
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
	m := &a.g2
	g := m.acc[key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[key] = g
	}
	g.n++
	for _, i := range a.bktIdx[:a.bktN] {
		a.mIv[i][0], a.mIv[i][1] = lo[i], hi[i]-math.Abs(hi[i])*bucketMemoMargin
	}
	m.mKey, a.mAcc = probe, g
	return g, true
}

// enterBuckets3 is enterBuckets2 for three group columns. See there for why the loop is written out
// rather than shared.
func (a *groupAccum) enterBuckets3(comps *[maxGroupCols]groupComp, nAgg int, probe groupKey3) (*groupAcc, bool) {
	key := probe
	var lo, hi [maxGroupCols]float64
	for _, i := range a.bktIdx[:a.bktN] {
		f, isNum := comps[i].number()
		if !isNum {
			return nil, true
		}
		b, inBucket := bucketOf(f, a.bucket[i])
		if !inBucket {
			return nil, true
		}
		key.raw[i], key.kind[i] = b, gkInt
		lo[i] = float64(b)
		hi[i] = lo[i] + float64(a.bucket[i])
	}
	m := &a.g3
	g := m.acc[key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[key] = g
	}
	g.n++
	for _, i := range a.bktIdx[:a.bktN] {
		a.mIv[i][0], a.mIv[i][1] = lo[i], hi[i]-math.Abs(hi[i])*bucketMemoMargin
	}
	m.mKey, a.mAcc = probe, g
	return g, true
}

// enterBucket1 is enterBuckets2 for a single bucketed group column: floor f, find the bucket's
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
	m := &a.g1
	m.key.raw, m.key.kind = b, gkInt
	g := m.acc[m.key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[m.key] = g
	}
	g.n++
	lo := float64(b)
	hi := lo + float64(a.bucket[0])
	a.mIv[0][0], a.mIv[0][1] = lo, hi-math.Abs(hi)*bucketMemoMargin
	a.mAcc = g
	return g, true
}

// compValue renders one packed component back to the value it stands for. The string table is the
// accumulator's, so this is the only place a string id becomes a string again -- and the id is an
// index, so it cannot name a value the query did not intern.
func (a *groupAccum) compValue(raw int64, kind groupValKind) classad.Value {
	switch kind {
	case gkReal:
		return classad.NewRealValue(math.Float64frombits(uint64(raw)))
	case gkString:
		return classad.NewStringValue(a.strVal[raw])
	case gkBool:
		return classad.NewBoolValue(raw != 0)
	case gkUndef:
		return classad.NewUndefinedValue()
	case gkError:
		return classad.NewErrorValue()
	}
	return classad.NewIntValue(raw)
}

// values1 / values2 / values3 render a key back to the group's component values, in request order.
func (a *groupAccum) values1(k groupKey1) []classad.Value {
	return []classad.Value{a.compValue(k.raw, k.kind)}
}

func (a *groupAccum) values2(k groupKey2) []classad.Value {
	return []classad.Value{a.compValue(k.raw[0], k.kind[0]), a.compValue(k.raw[1], k.kind[1])}
}

func (a *groupAccum) values3(k groupKey3) []classad.Value {
	return []classad.Value{
		a.compValue(k.raw[0], k.kind[0]),
		a.compValue(k.raw[1], k.kind[1]),
		a.compValue(k.raw[2], k.kind[2]),
	}
}

// nGroups is how many groups were formed, for a caller sizing its result.
func (a *groupAccum) nGroups() int {
	switch a.n {
	case 1:
		return len(a.g1.acc)
	case 2:
		return len(a.g2.acc)
	case 3:
		return len(a.g3.acc)
	}
	return 0
}

// each calls f once per group, with the group's component values in request order. It is how a
// caller reads the result without knowing which arity answered.
func (a *groupAccum) each(f func([]classad.Value, *groupAcc)) {
	switch a.n {
	case 1:
		for k, g := range a.g1.acc {
			f(a.values1(k), g)
		}
	case 2:
		for k, g := range a.g2.acc {
			f(a.values2(k), g)
		}
	case 3:
		for k, g := range a.g3.acc {
			f(a.values3(k), g)
		}
	}
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
