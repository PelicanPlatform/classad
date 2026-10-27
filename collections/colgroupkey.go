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
// ONE KEY TYPE PER ARITY, which is why the key below is a generic struct over two array widths
// rather than a plain one.
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
// The types are INSTANTIATIONS of one generic struct rather than three hand-written ones, and so
// are the packing, the memo probe, the bucket entry, the insert and the unpacking. What stayed
// per-arity is the shell: which map, and the branch structure around it. That split is not a matter
// of taste. A generic function called out of line is called with a runtime dictionary in the first
// argument register, and its outgoing arguments and that dictionary are part of the CALLER's frame
// -- so putting one on a per-record path moves cost onto the function that calls it, including
// accFor itself, which every arity goes through. Reached only through a concrete per-arity shell,
// the generic bodies either instantiate INTO it (the packing) or sit behind it on a path taken once
// per bucket (the entry), and every per-record function compiles to the frame it had before any of
// this was shared. accFor is opcode-for-opcode what it was.
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
// Three. Each arity is a separate key instantiation, a separate map and a separate BLOCK OF
// PER-COMPONENT STATE, so a fourth widens nothing the first three hash, compare or read -- the
// measurement that justified the old cap of two does not apply to this one.
//
// It did not used to be free. The accumulator once held the bucket widths, the component index lists
// and the memo's intervals as arrays sized by this constant, sitting among the fields a one-column
// query reads every record, and raising the cap from two to three was measured at about 5% on the
// two-column bucketed arm whose records almost all answer from the interval memo -- a query that
// executes nothing different, paying for a column it does not have. Those arrays now live in the
// per-arity state below, sized by THAT arity, so a one-column query reads one component's worth of
// them and a cap raised past three cannot reach them at all. TestGroupAccumHasNoCapSizedState and
// TestGroupAccumHotFieldOffsets pin it, and both pass unchanged in a build with this constant at 4.
// So does the measurement: that build moves no one- or two-column arm of BenchmarkGroupAccum further
// than equivalent builds of the same code move each other.
//
// Raising it further is still a key width in rawArr and kindArr, a per-arity state, a case in each
// switch, a fuzz shape that reaches it and an eligibility change at the db boundary. What it is no
// longer is a cost to the arities already here, and nothing yet asks for a fourth group column.
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

// The packed key, one instantiation per arity. Each identifies one group: per component, the kind
// and a raw int64 that identifies the value within that kind -- the value itself for an integer,
// Float64bits for a real, 0/1 for a boolean, a query-local id for a string, and 0 for undefined and
// error.
//
// The KIND is part of every key and may never be dropped. Without it an integer 1 and the boolean
// TRUE share a raw of 1 and would collide into one group, while the scan renders them "1" and
// "true" -- a coarsening, and the one the contract above forbids. TestGroupKeyKindSeparatesRawBits
// pins it.
//
// ONE type, parameterised on the two array widths, rather than a hand-written struct per arity.
// Every type in each constraint below is an array with the same element type, which is what lets
// the generic body INDEX k.raw[i] and k.kind[i] and take len(k.raw); and distinct array lengths are
// distinct shapes, so the compiler stencils genuinely separate code and a map keyed on one of these
// is a map on a concrete comparable struct. There is no interface and no per-record dispatch here.
//
// What this buys is that the packing, the memo probe, the bucket entry and the unpacking are each
// written ONCE instead of once per arity, so an added arity is a line in each union, a case in the
// dispatch switch and nothing else. What it does not buy is a free arity: see maxGroupCols.
//
// Go has no const generics, so the set is CLOSED -- an arity must be named in both unions. That is
// deliberate rather than a limitation worked around: an arity with no instantiation has no key, and
// accFor declines it instead of truncating a key to fit.
//
// A one-component key is [1]int64 + [1]groupValKind, which measures the same 16 bytes the pair of
// scalars it replaced did (TestGroupKeyIsPlainMemory pins the width). The arity-1 hot paths index
// it with a CONSTANT 0 against the concrete field, so they compile to the same direct stores and
// loads the scalar form did; only the multi-component paths are generic, and those are the ones
// that were duplicated.
type rawArr interface {
	[1]int64 | [2]int64 | [3]int64
}

type kindArr interface {
	[1]groupValKind | [2]groupValKind | [3]groupValKind
}

type groupKeyN[R rawArr, K kindArr] struct {
	raw  R
	kind K
}

// One alias per arity the cap admits. They name the instantiations the dispatch switch selects, and
// pairing the two widths here rather than at each use is what stops a key being built with its raw
// and kind arrays at different lengths.
type (
	groupKey1 = groupKeyN[[1]int64, [1]groupValKind]
	groupKey2 = groupKeyN[[2]int64, [2]groupValKind]
	groupKey3 = groupKeyN[[3]int64, [3]groupValKind]
)

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

// groupMap is the map half of one arity's accumulation state: the map itself plus the key scratch
// every probe of it is built in. Everything here is parameterised on the key type and nothing in it
// knows how a key is PACKED -- that differs per arity and is written out below, once each.
//
// key is the per-record scratch, held here rather than declared per call. A fresh `var key K` is a
// key's worth of zeroing on every record, and the paths that pay it are the ones where the
// accumulation map is small enough to sit in L1 -- a status histogram, a daily series -- so it is
// pure overhead against a probe that costs almost nothing. Reusing one value works because the
// slots past the query's component count are never written (they start zero) and the rest are
// overwritten every record.
//
// The memo's probe is NOT here. It is key-shaped, but it is read beside the memo's intervals and
// nothing else, so it sits with them in the arity's state below -- and at arity 1 there is no probe
// at all, since a lone bucketed column's memo is decided by the interval alone.
type groupMap[K comparable] struct {
	acc map[K]*groupAcc
	key K
}

// groupState1 / groupState2 / groupState3 is everything ONE arity accumulates with: the map above,
// beside the per-component state and the memo probe the bucket paths read.
//
// EVERY ARRAY HERE IS SIZED BY THE ARITY, never by maxGroupCols, and that is the property this
// split exists for. These arrays used to be fields of groupAccum sized by the cap, which put a
// three-component interval table and a three-component bucket-width table among the fields a
// one-column query reads once per record -- so raising the cap moved a query that executes nothing
// different. Sized by the arity, a one-column query carries one component's worth and a fourth
// arity is incapable of reaching it: a groupState4 would be a new field after groupState3, and
// nothing ahead of it moves. TestGroupAccumHasNoCapSizedState and TestGroupAccumHotFieldOffsets
// pin both halves of that.
//
// FIELD ORDER IS NOT INCIDENTAL. A multi-column memo HIT reads the intervals, the component index
// lists and the probe and nothing else, so those three are declared together, in that order, at the
// front, and everything a hit does not read is behind them. Ordered the way the accumulator's old
// cap-sized arrays were -- intervals at the front, probe behind the map -- the two-column hit spans
// four cache lines where before the split it spanned three. TestGroupAccumHotFieldOffsets pins the
// arrangement. It is held on that reasoning rather than on a measurement: on the machine this was
// written on the difference is below what that arm resolves. (What IS resolvable on that arm, and
// large, is whether the containment test inlines -- see groupState2.inMemoIntervals.)
//
//   - mIv first. Low and high sit beside each other, one pair per bucket, rather than in two
//     arrays: the test reads both, so a pair is two loads from one line, where two arrays put them
//     a table apart and the pair straddled a line as soon as the tables grew.
//   - the component index lists beside it, since the bucket loops walk them together, and absent
//     from arity 1 entirely -- accFor serves one column straight-line and never looks at a list.
//   - mKey, the probe, last of the three. It is absent from arity 1 for the same reason: a lone
//     bucketed column has no unbucketed component to match, so its memo is the interval alone.
//   - bucket last of all at arities 2 and 3, behind everything a hit reads, because it is read once
//     per bucket ENTERED rather than once per record. At arity 1 there is no probe for it to
//     displace, so it stays ahead of the map, which puts the map header and the key scratch -- the
//     whole of the UNBUCKETED single-column path -- on one line together.
//
// The intervals: [mIv[r][0], mIv[r][1]) is the half-open interval of VALUES that fell in the bucket
// of the r-th BUCKETED component, and bucket[r] is that component's width. bktIdx[r] says which
// group column r is.
//
// INDEXED BY BUCKET RANK, not by component index, and that is not cosmetic. A query that groups by
// one dimension and one time bucket -- the common one -- has a single bucketed component, so one
// entry of each table is live and it is the first. It also keeps the compiler's bounds checks where
// they already were: the loops walk bktIdx, whose length bounds the rank, so an interval read needs
// no check of its own, while indexing a table of ARITY entries by a COMPONENT index does -- the
// component array handed in is still cap-wide, so proving i < 3 does not prove i < 2. That check,
// once per bucketed component per record, measured about 5% on the two-column bucketed arm.
//
// The accumulator the intervals belong to is groupAccum.mAcc, which is one pointer for every arity
// and so is not per-component state. mKey holds the unbucketed components a hit also has to match:
// the previous record's, with the bucketed slots left zero so that one struct compare answers for
// all of them at once. All of it is only meaningful when some component is bucketed.
//
// Records arrive in insertion order and a bucketed group column advances slowly against its width --
// an hour bucket over a per-second timestamp holds thousands of consecutive records -- so the next
// record is overwhelmingly in the bucket the last one was, under the same owner. Remembering the
// INTERVAL rather than the resulting key is what makes that worth having: the per-record cost of a
// bucketed group is the float divide and floor in bucketOf, which runs before any map is touched, so
// a memo on the finished key would still pay for the divide. A compare or two replaces the divide,
// the floor and the map probe together -- and a HIT never touches a map at all, which is why the
// key's width is a cost on misses only.
//
// The top of each interval is pulled a few ulps BELOW its true edge, because bucketOf reaches its
// bucket through a float division whose rounding can place a value within about an ulp of that edge
// in the next bucket. Admitting slightly less than the real interval only sends those few values
// down the slow path, where bucketOf itself decides; admitting slightly more would group one of them
// differently from every other tier.
type groupState1 struct {
	mIv    [1][2]float64
	bucket [1]int64
	groupMap[groupKey1]
}

type groupState2 struct {
	mIv    [2][2]float64
	rawIdx [2]uint8
	bktIdx [2]uint8
	mKey   groupKey2
	groupMap[groupKey2]
	bucket [2]int64
}

type groupState3 struct {
	mIv    [3][2]float64
	rawIdx [3]uint8
	bktIdx [3]uint8
	mKey   groupKey3
	groupMap[groupKey3]
	bucket [3]int64
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
// Exactly ONE of g1/g2/g3 is live, decided by n at construction: that is the per-arity choice, made
// once per query, and it now carries the whole of that arity's state rather than just its map. What
// is left here is what has ONE of whatever the arity -- the component counts, the memo's
// accumulator pointer, the string table -- so NO FIELD OF THIS STRUCT IS SIZED BY maxGroupCols, and
// a fourth arity is a fourth state field behind the third, moving nothing ahead of it.
type groupAccum struct {
	// The fields the per-record paths read, first and together. The bucketed single-column hit --
	// the shape a time series asks, and the one the memo exists for -- reads n, bktN, mAcc and one
	// interval and touches nothing else, and in this order those are one cache line.
	n    int // how many components a key of this query carries; 0 = no key type fits, decline
	bktN int
	rawN int
	mCmp bool // whether there are unbucketed components for mKey to match at all

	// Bit i is set when component i is floored into buckets. It answers "is this column bucketed"
	// for the column readers, which have to know before they can decide what a record that has no
	// number to floor does -- and it answers it from one byte rather than from the live arity's
	// bucket-width table, which is where a cap-sized array used to be read once per record.
	bktMask uint8

	// The accumulator the bucket interval memo is armed with: the group the previous record landed
	// in, handed straight back to the next record whose components all fall in the remembered
	// intervals. One pointer whatever the arity, so it stays here, directly ahead of the live
	// state's intervals -- which is what keeps the single-column bucketed hit to one cache line.
	// nil means the memo is unarmed and every record takes the slow path.
	mAcc *groupAcc

	// Exactly ONE of these is live, decided by n at construction. They sit directly behind the memo
	// pointer, ahead of everything a memo hit does not read, because the per-record paths reach an
	// interval table, a key scratch or a memo probe and little else.
	g1 groupState1
	g2 groupState2
	g3 groupState3

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
		if b > 0 {
			a.bktMask |= 1 << uint(i)
			a.bktN++
			continue
		}
		a.rawN++
	}
	// With nothing unbucketed the memo's key probe is the zero value on both sides, so comparing
	// it would be work that can never decide anything. That is the lone bucketed column -- the
	// shape the memo was built for -- so it is worth the flag.
	a.mCmp = a.rawN > 0
	// The per-component state goes into the ONE arity that will use it, which is why this is a
	// switch rather than a loop over arrays held here: the arrays are sized by the arity, so there
	// is no cap-sized place left to put them. Arity 1 takes only its bucket width -- accFor serves
	// one column straight-line and never walks a component list.
	switch a.n {
	case 1:
		a.g1.acc = map[groupKey1]*groupAcc{}
		a.g1.bucket[0] = buckets[0]
	case 2:
		a.g2.acc = map[groupKey2]*groupAcc{}
		splitComps(buckets, a.g2.bucket[:], a.g2.rawIdx[:], a.g2.bktIdx[:])
	case 3:
		a.g3.acc = map[groupKey3]*groupAcc{}
		splitComps(buckets, a.g3.bucket[:], a.g3.rawIdx[:], a.g3.bktIdx[:])
	}
	return a
}

// splitComps splits the group columns into the raw list and the bucketed list the per-record loops
// walk, and copies each bucketed column's width into the bucket table BESIDE ITS RANK IN THAT LIST,
// which is how every per-record reader indexes it. An unbucketed column's width is zero and is never
// read, so it is not stored at all.
//
// It takes slices of the caller's arrays rather than being written per arity because it runs once
// per QUERY: the arity is what has to be in the array types, not in this.
func splitComps(buckets, bucket []int64, rawIdx, bktIdx []uint8) {
	nRaw, nBkt := 0, 0
	for i, b := range buckets {
		if b > 0 {
			bktIdx[nBkt] = uint8(i)
			bucket[nBkt] = b
			nBkt++
			continue
		}
		rawIdx[nRaw] = uint8(i)
		nRaw++
	}
}

// bktMask is a byte, so the cap cannot pass the number of bits in one without this failing to
// compile. A larger cap is a wider mask, not a silently truncated one.
const _ = uint(8 - maxGroupCols)

// bucketed reports whether group component i is floored into buckets.
//
// It reads the mask rather than the live arity's bucket-width table because the callers are column
// readers running once per record, and the table is now behind an arity dispatch they would
// otherwise have to repeat. The mask is set once, in newGroupAccum, from the same widths.
func (a *groupAccum) bucketed(i int) bool {
	return a.bktMask&(1<<uint(i)) != 0
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
			m.key.raw[0], m.key.kind[0] = comps[0].raw, comps[0].kind
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
		// Positive containment, never `f < lo || f >= hi` -- see groupState2.inMemoIntervals for
		// why the negated form hands a NaN the previous record's bucket.
		if g := a.mAcc; g != nil && a.g1.mIv[0][0] <= f && f < a.g1.mIv[0][1] {
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

// inMemoIntervals reports whether every bucketed component falls in the interval the memo remembers
// for it. bktN is the accumulator's count of bucketed components, which is the same at every arity.
//
// The containment test is written in its POSITIVE form and negated, never as `f < lo || f >= hi`.
// Every comparison against a NaN is false, so the negated form reads a NaN as "neither below nor
// above, therefore inside" and hands it the previous record's bucket -- a NaN counted as an ordinary
// day, which is exactly what bucketOf refuses to let happen. The positive form is false for a NaN,
// so it falls through to bucketOf, which is the only thing that decides a value is in no bucket.
//
// There is one of these per arity, rather than one reading a cap-sized interval table, because the
// table is now the live arity's and sized by it. Written out, not shared, and NOT generic: this is
// the one piece of the memo that runs once per RECORD, and the rest of this file already records
// what a generic body the compiler declines to instantiate costs on such a path -- a call with a
// runtime dictionary in the first argument register.
//
// IT HAS TO INLINE, and that is what decides the shape of this signature. The bktN it needs is the
// accumulator's, not the state's, so the obvious form -- a method on *groupAccum that opens with
// `m := &a.g2` -- was written first, and that one local put the body at cost 81 against an inline
// budget of 80. Out of line it cost 19% on the two-column bucketed arm, which answers about 99.6%
// of its records here and nowhere else. Taking the count as an argument keeps the body at 75 -- five
// to spare, so this is a tight fit and not a comfortable one. TestGroupMemoPathsInline fails if it
// goes back over, and checks the CALL SITES rather than the budget, because a body under the budget
// can still be left out of line at a caller that has grown too big to take it.
func (m *groupState2) inMemoIntervals(comps *[maxGroupCols]groupComp, bktN int) bool {
	for r, i := range m.bktIdx[:bktN] {
		f, isNum := comps[i].number()
		if !isNum || !(m.mIv[r][0] <= f && f < m.mIv[r][1]) {
			return false
		}
	}
	return true
}

// inMemoIntervals is groupState2's at three group columns. See there for why it is written out
// rather than shared, and for why its signature is this shape.
func (m *groupState3) inMemoIntervals(comps *[maxGroupCols]groupComp, bktN int) bool {
	for r, i := range m.bktIdx[:bktN] {
		f, isNum := comps[i].number()
		if !isNum || !(m.mIv[r][0] <= f && f < m.mIv[r][1]) {
			return false
		}
	}
	return true
}

// armMemo points the interval memo at the buckets key landed in.
//
// WRITTEN OUT PER ARITY, and it did not used to be: the memo's tables are the live arity's now, and
// one shared generic form has to be handed them as slices. Three slice headers is nothing once per
// bucket -- but "once per bucket" is once per RECORD on a multi-column group whose categorical
// components cycle, because then the probe misses every time and every record enters a bucket. That
// shape measured about 5% slower with the shared form than with these. The arity-1 and arity-2 arms
// did not move either way; this is the three-column all-miss shape and nothing else.
//
// What the sharing bought was one place to get the indexing below right, and losing it is only
// affordable because two tests now cover what it protected -- behaviour rather than spelling, which
// is the stronger guard. See the paragraph below for which test catches which way of getting it
// wrong.
//
// Read back from the FINISHED key rather than from intervals accumulated as the floor loop went:
// key.raw[i] is the bucket start it stored, so the interval is recoverable from it and there is no
// pair of per-call arrays to zero. A record that turns out to be in NO bucket never reaches here,
// and it has to leave the memo exactly as it found it.
//
// TWO INDEXINGS MEET HERE. r is the BUCKET RANK, which is how the memo's own tables are indexed; i
// is the COMPONENT index, which is how the key's slots are. Both ways of confusing them are quiet:
//
//   - mIv[i] for mIv[r] stays CORRECT. The reader compares against a slot that was never armed, so
//     it never hits, and every record takes the slow path and gets the same answer -- a silent loss
//     of the whole acceleration. TestBucketMemoDecidesARepeatedRecord catches it.
//   - bucket[0] for bucket[r] arms a component's interval from ANOTHER component's width. Too
//     narrow is merely slow; too wide hands the previous record's group to a record that belongs in
//     a different bucket, which is a wrong count. It is invisible unless the widths DIFFER.
//     TestBucketMemoDeclinesARecordThatMovedBucket catches it, and nothing did before.
func (m *groupState2) armMemo(key *groupKey2, bktN int) {
	for r, i := range m.bktIdx[:bktN] {
		lo := float64(key.raw[i])
		hi := lo + float64(m.bucket[r])
		m.mIv[r][0], m.mIv[r][1] = lo, hi-math.Abs(hi)*bucketMemoMargin
	}
}

// armMemo is groupState2's at three group columns.
func (m *groupState3) armMemo(key *groupKey3, bktN int) {
	for r, i := range m.bktIdx[:bktN] {
		lo := float64(key.raw[i])
		hi := lo + float64(m.bucket[r])
		m.mIv[r][0], m.mIv[r][1] = lo, hi-math.Abs(hi)*bucketMemoMargin
	}
}

// packProbeN writes only the components idx names, leaving the rest of the key zero. It is the
// memo's probe: the bucketed slots stay zero on both sides so one struct compare answers for every
// unbucketed component at once.
func packProbeN[R rawArr, K kindArr](k *groupKeyN[R, K], comps *[maxGroupCols]groupComp, idx []uint8) {
	for _, i := range idx {
		k.raw[i], k.kind[i] = comps[i].raw, comps[i].kind
	}
}

// newGroupN creates the group k names and files it in m. Once per GROUP rather than once per
// record, and shared between the raw and the bucketed paths so the two cannot create a group
// differently.
//
// k is a POINTER for the caller's sake, not this function's. A key is an array pair and Go's ABI
// never puts an array in a register, so by value it is copied into the caller's outgoing argument
// area -- and the caller is the per-record lookup, whose frame then carries a whole key it never
// otherwise needs.
func newGroupN[R rawArr, K kindArr](m *groupMap[groupKeyN[R, K]], k *groupKeyN[R, K],
	nAgg int) *groupAcc {
	g := newGroupAcc(nAgg)
	m.acc[*k] = g
	return g
}

// accForRaw2 / accForRaw3 are accFor for two or three unbucketed group columns.
//
// What is left here per arity is which map to use, the stores that fill its key, and a probe of it.
// None of those is shared, and the reason is the same for all three: a generic function the
// compiler declines to instantiate INTO its caller becomes a call with a runtime dictionary in the
// first argument register, and every line of this function runs once per RECORD. The packing in
// particular was shared first and measured: a loop over len(key.raw) -- a constant the compiler
// knows and does not unroll -- cost about 7% on a two-column raw group whose map is small enough to
// sit in L1, where the stores are most of the work. The widths are spelled out instead.
//
// What IS shared is everything that is not per-record: the key TYPE, the insert (newGroupN), the
// memo probe's packing, the bucket arithmetic and the unpacking. Those are where an arity could be
// got wrong, and they are written once.
//
// The arity-1 shape never reaches here: accFor serves it straight-line, because at a couple of
// nanoseconds a record the structure of the code is most of the cost there.
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
		g = newGroupN(&m.groupMap, &m.key, nAgg)
	}
	g.n++
	return g, true
}

func (a *groupAccum) accForRaw3(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g3
	m.key.raw[0], m.key.kind[0] = comps[0].raw, comps[0].kind
	m.key.raw[1], m.key.kind[1] = comps[1].raw, comps[1].kind
	m.key.raw[2], m.key.kind[2] = comps[2].raw, comps[2].kind
	g := m.acc[m.key]
	if g == nil {
		g = newGroupN(&m.groupMap, &m.key, nAgg)
	}
	g.n++
	return g, true
}

// accForBucketed2 / accForBucketed3 are accForRaw2 / accForRaw3 where at least one component is
// floored into buckets: they try the interval memo first and fall to enterBucketsN, which does the
// arithmetic and re-arms it.
//
// The `!a.mCmp` case is an early return rather than a condition around the probe below, which reads
// the same and is not the same. Guarding only the fill leaves the compiler laying the common case
// -- there IS an unbucketed component -- out of line, so a memo hit runs three disjoint stretches
// of code joined by two long branches instead of falling through; it measured +36% on the
// three-column arm whose records almost all answer from the memo, with the instructions themselves
// unchanged.
//
// Every component bucketed -- two time buckets, say -- means no unbucketed component, so the memo
// key is the zero key on both sides and there is nothing to compare. (A LONE bucketed column never
// reaches here: accFor serves n == 1 straight-line.)
func (a *groupAccum) accForBucketed2(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g2
	if !a.mCmp {
		if g := a.mAcc; g != nil && m.inMemoIntervals(comps, a.bktN) {
			g.n++
			return g, true
		}
		return a.enterBuckets2(comps, nAgg, groupKey2{})
	}
	var probe groupKey2
	packProbeN(&probe, comps, m.rawIdx[:a.rawN])
	if g := a.mAcc; g != nil && probe == m.mKey && m.inMemoIntervals(comps, a.bktN) {
		g.n++
		return g, true
	}
	return a.enterBuckets2(comps, nAgg, probe)
}

func (a *groupAccum) accForBucketed3(comps *[maxGroupCols]groupComp, nAgg int) (*groupAcc, bool) {
	m := &a.g3
	if !a.mCmp {
		if g := a.mAcc; g != nil && m.inMemoIntervals(comps, a.bktN) {
			g.n++
			return g, true
		}
		return a.enterBuckets3(comps, nAgg, groupKey3{})
	}
	var probe groupKey3
	packProbeN(&probe, comps, m.rawIdx[:a.rawN])
	if g := a.mAcc; g != nil && probe == m.mKey && m.inMemoIntervals(comps, a.bktN) {
		g.n++
		return g, true
	}
	return a.enterBuckets3(comps, nAgg, probe)
}

// enterBuckets2 / enterBuckets3 are accForBucketed2 / accForBucketed3's slow path: reached on the
// first record of each bucket, and on any record the memo declines to decide. They floor the
// bucketed components into the key beside the unbucketed ones probe already carries, find the
// resulting group and re-arm the memo with it.
//
// What is shared is the arming (armMemoN) and the insert (newGroupN); what is written out here is
// the floor loop and the map probe. That split is where the compiler put it, not where taste would:
// a generic function it declines to instantiate into its caller becomes a CALL with a runtime
// dictionary, and on a column whose buckets are as narrow as the step between its values this
// function runs once per RECORD rather than once per bucket, so a frame added here is a frame per
// record. The flooring is just over the size the compiler will inline, and it measured.
//
// The arithmetic the floor loop performs is bucketOf, which IS shared -- that is where agreeing
// with the scan on where a bucket edge lies is decided, and it is the part that could disagree.
// What is repeated is the loop around it.
//
// Kept out of accForBucketed2 rather than written inline, because the hot path there is a handful
// of instructions and the code around it is what decides how they are laid out: folding this in
// cost the UNBUCKETED path measurably while doing nothing for the bucketed one.
//
//go:noinline
func (a *groupAccum) enterBuckets2(comps *[maxGroupCols]groupComp, nAgg int,
	probe groupKey2) (*groupAcc, bool) {
	// probe already carries the unbucketed components (and is the zero key when there are none,
	// which is the only case the caller leaves it unfilled), so the key is built by filling in the
	// bucketed slots beside them.
	m := &a.g2
	key := probe
	for r, i := range m.bktIdx[:a.bktN] {
		f, isNum := comps[i].number()
		if !isNum {
			return nil, true // no number to floor: the record leaves the series
		}
		b, inBucket := bucketOf(f, m.bucket[r])
		if !inBucket {
			return nil, true
		}
		key.raw[i], key.kind[i] = b, gkInt
	}
	g := m.acc[key]
	if g == nil {
		g = newGroupN(&m.groupMap, &key, nAgg)
	}
	g.n++
	m.armMemo(&key, a.bktN)
	m.mKey, a.mAcc = probe, g
	return g, true
}

//go:noinline
func (a *groupAccum) enterBuckets3(comps *[maxGroupCols]groupComp, nAgg int,
	probe groupKey3) (*groupAcc, bool) {
	m := &a.g3
	key := probe
	for r, i := range m.bktIdx[:a.bktN] {
		f, isNum := comps[i].number()
		if !isNum {
			return nil, true // no number to floor: the record leaves the series
		}
		b, inBucket := bucketOf(f, m.bucket[r])
		if !inBucket {
			return nil, true
		}
		key.raw[i], key.kind[i] = b, gkInt
	}
	g := m.acc[key]
	if g == nil {
		g = newGroupN(&m.groupMap, &key, nAgg)
	}
	g.n++
	m.armMemo(&key, a.bktN)
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
	m := &a.g1
	b, inBucket := bucketOf(f, m.bucket[0])
	if !inBucket {
		return nil, true
	}
	m.key.raw[0], m.key.kind[0] = b, gkInt
	g := m.acc[m.key]
	if g == nil {
		g = newGroupAcc(nAgg)
		m.acc[m.key] = g
	}
	g.n++
	lo := float64(b)
	hi := lo + float64(m.bucket[0])
	m.mIv[0][0], m.mIv[0][1] = lo, hi-math.Abs(hi)*bucketMemoMargin
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

// keyValues renders a key back to the group's component values, in request order. It runs once per
// GROUP rather than once per record, so the loop the generic form costs is charged to the result
// shaping and not to the scan.
func keyValues[R rawArr, K kindArr](a *groupAccum, k groupKeyN[R, K]) []classad.Value {
	vs := make([]classad.Value, len(k.raw))
	for i := 0; i < len(k.raw); i++ {
		vs[i] = a.compValue(k.raw[i], k.kind[i])
	}
	return vs
}

// eachGroup calls f once per group of one arity's map, which is what each dispatches to.
func eachGroup[R rawArr, K kindArr](a *groupAccum, m *groupMap[groupKeyN[R, K]],
	f func([]classad.Value, *groupAcc)) {
	for k, g := range m.acc {
		f(keyValues(a, k), g)
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
		eachGroup(a, &a.g1.groupMap, f)
	case 2:
		eachGroup(a, &a.g2.groupMap, f)
	case 3:
		eachGroup(a, &a.g3.groupMap, f)
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
