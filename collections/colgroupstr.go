package collections

import (
	"encoding/binary"
	"math"
	"strings"
	"unsafe"

	"github.com/PelicanPlatform/classad/collections/wire"
)

// GROUP BY a STRING column, answered from the columns rather than by scanning records.
//
// Against a production history archive, grouping one numeric column took 90 ms and grouping Owner --
// the same query shape, the same records -- took 21.1 s, because a non-numeric group column declined
// every columnar tier and fell to projecting each matching record. Two hundred times apart for a
// difference in the column's TYPE, not in the work.
//
// The lever is the per-block string dictionary (see strdict.go): a dictionary-encoded field stores its
// distinct values once per block and gives each record a fixed-width CODE into them. So the per-record
// read is an indexed one rather than a walk down the positional string region -- but more importantly,
// the CODE is enough to identify the group. Resolving a code to a group is work per DISTINCT VALUE PER
// BLOCK (a few hundred), not per record (millions): groupCol holds a code -> *groupAcc table filled
// lazily as codes are met, and the per-record step becomes an array index and a counter bump.
//
// Codes are per BLOCK -- the same owner has a different code in the next block -- so the table is
// rebuilt at every block boundary. That is the whole cost of the string case, and it is why the
// dictionary makes this cheap instead of merely possible.
//
// Three things bypass the dictionary and take a per-record path: a block where the field has no
// dictionary at all (positional read, the pre-dictionary cost), an ESCAPED record whose value did not
// fit the column and lives in the cold tail, and a segment whose schema does not carry the field
// (the row fallback, which reads the attribute from the record itself).

// groupValKind is the set of scalar shapes a group value can take. It is deliberately NOT adKind:
// adKind describes what a column can STORE, and a group value also has to cover what a column can only
// report -- an absent attribute, an explicit UNDEFINED or ERROR -- because the scan this must agree
// with emits a group for each of those (ValueText renders them "undefined" / "error").
type groupValKind uint8

const (
	gkInt    groupValKind = iota // the zero value, so a bare groupKey is a zero-valued integer group
	gkReal                       // raw holds math.Float64bits
	gkString                     // s holds the value
	gkBool
	gkUndef // the attribute is absent, or holds UNDEFINED: one group either way, as the scan has it
	gkError
)

// groupableKind reports whether a schema field of this kind can be a GROUP column. Numeric and string
// fields can: both are columnar, and both render to the text the scan groups by.
//
// A BOOL field cannot, even though a boolean VALUE can be a group: bools are bit-packed rather than
// slotted, so there is no column reader for one here. Such a group column declines and scans, which is
// what it did before.
func groupableKind(k adKind) bool { return numericKind(k) || k == akString }

// groupCol reads the GROUP column of one block, with everything resolvable hoisted out of the
// per-record loop. Numeric fields keep the numCol reader unchanged -- the fast path that was already
// there costs exactly what it did. String fields get the dictionary machinery above.
type groupCol struct {
	blk      *columnarBlock
	fieldIdx int
	fieldID  uint32
	isStr    bool

	num numCol // numeric fields

	// String fields, dictionary path. codeAcc[c] is the group that code c resolved to in THIS block,
	// nil until the first record carrying it is met.
	hasDict bool
	entries [][]byte
	codes   []byte
	codeW   int
	codeAcc []*groupAcc

	// String fields with no dictionary in this block: the decompressed positional string region,
	// walked per record.
	strRegion []byte
}

// groupColScratch is the per-query scratch a group column reuses from block to block: the dictionary
// parse buffer and the code table. Both are sized by a block's cardinality, so allocating them per
// block would make a segment's worth of garbage out of buffers that are overwritten anyway -- the same
// reason dictEntries takes the caller's buffer rather than allocating one.
type groupColScratch struct {
	dict [][]byte
	code []*groupAcc
}

// newGroupCol resolves the group column for one block. ok=false when the field cannot be read here,
// which gives the whole query back to the scan rather than silently dropping the block's records.
func newGroupCol(blk *columnarBlock, fieldIdx int, fieldID uint32, bc *blockCache,
	sc *groupColScratch) (groupCol, bool) {
	c := groupCol{blk: blk, fieldIdx: fieldIdx, fieldID: fieldID}
	if blk.schema.fields[fieldIdx].kind != akString {
		num, ok := newNumCol(blk, fieldIdx, fieldID, bc)
		if !ok {
			return groupCol{}, false
		}
		c.num = num
		return c, true
	}
	c.isStr = true
	if blk.dictOwns(fieldIdx) {
		// The dictionary is AUTHORITATIVE for this field: its values are not in the positional
		// region at all, so if either half of it fails to read there is no other place to look.
		entries, ok := blk.dictEntries(fieldIdx, bc, &sc.dict)
		if !ok {
			return groupCol{}, false
		}
		codes, w, ok := blk.dictCodes(fieldIdx, bc)
		if !ok {
			return groupCol{}, false
		}
		if cap(sc.code) < len(entries) {
			sc.code = make([]*groupAcc, len(entries))
		}
		sc.code = sc.code[:len(entries)]
		clear(sc.code)
		c.hasDict, c.entries, c.codes, c.codeW, c.codeAcc = true, entries, codes, w, sc.code
		return c, true
	}
	region, err := bc.stream(blk, kindStr)
	if err != nil {
		return groupCol{}, false
	}
	c.strRegion = region
	return c, true
}

// accFor returns record k's group accumulator, creating the group when it is new.
//
// ok=false means the value is one only evaluation can produce -- an expression, a list, a nested ad.
// Such a record still forms a group in the scan, so the caller gives the query back rather than
// returning a result with a group missing.
func (c *groupCol) accFor(k int, bc *blockCache, acc *groupAccum, nAgg int) (*groupAcc, bool) {
	// The dictionary fast path: one indexed read of the code column, then an array index. The group
	// is resolved once per distinct value per block, so this loop does no hashing at all after the
	// block's first few records.
	if c.hasDict && !testBit(c.blk.escapeAt(k), c.fieldIdx) {
		// Under a bucket this record's value comes from the string dictionary, and a string has
		// no bucket to be floored into, so it leaves the series exactly as the scan drops it.
		//
		// Only this RECORD, not the column. A column is typed by what dominates it, and a string
		// column still carries the records whose value escaped that type -- a number among the
		// strings buckets normally, and those records reach keyAt below. Skipping the whole column
		// here instead lost them silently: every count stayed plausible and most of the groups
		// simply were not there.
		if acc.bucket > 0 {
			return nil, true
		}
		code := dictCodeAt(c.codes, c.codeW, k)
		if code >= len(c.entries) {
			return nil, false
		}
		if g := c.codeAcc[code]; g != nil {
			g.n++
			return g, true
		}
		// Unreachable with a bucket (the guard above returns first), so accFor cannot decline.
		g, _ := acc.accFor(groupKey{kind: gkString, s: bytesToStr(c.entries[code])}, nAgg)
		c.codeAcc[code] = g
		return g, true
	}
	key, ok := c.keyAt(k, bc)
	if !ok {
		return nil, false
	}
	return acc.accFor(key, nAgg)
}

// keyAt derives record k's group key the slow way: for a numeric column, a block where the string
// field has no dictionary, or an escaped record whose value lives in the cold tail.
func (c *groupCol) keyAt(k int, bc *blockCache) (groupKey, bool) {
	if !c.isStr {
		nv, cell := c.num.cell(k, bc)
		switch cell {
		case cellNumber:
			return groupKeyOf(nv), true
		case cellExpr:
			return groupKey{}, false
		}
		// cellAbsent or cellOther: the value escaped the numeric slot and is not a number. It is
		// still a group -- an absent attribute groups as `undefined`, a string as its text -- so
		// read the literal itself rather than declining the way the numbers-only path had to.
		return escapedGroupKey(c.blk, k, c.fieldID, bc)
	}
	if c.hasDict || testBit(c.blk.escapeAt(k), c.fieldIdx) {
		// With a dictionary this is reached only for an ESCAPED record -- accFor serves every other
		// one -- and an escaped value is never in the dictionary.
		return escapedGroupKey(c.blk, k, c.fieldID, bc)
	}
	s, ok := c.positionalStrAt(k)
	if !ok {
		return groupKey{}, false
	}
	return groupKey{kind: gkString, s: s}, true
}

// positionalStrAt reads record k's string out of the positional region, for a block where this field
// has no dictionary: uvarint(len)+bytes per non-escaped, non-dictionary string field in schema order, so
// reaching this field means walking the ones before it. That walk is the cost the dictionary exists to
// remove, and a block without one still pays it.
//
// It ALIASES the decompressed region rather than copying. One Go string allocation per record is what
// made string column reads slower than the row path they replaced, and nothing here retains the alias:
// groupAccFor copies on the one path that does.
func (c *groupCol) positionalStrAt(k int) (string, bool) {
	b := c.blk
	region := c.strRegion[b.strOffAt(k):b.strOffAt(k+1)]
	esc := b.escapeAt(k)
	for i := range b.schema.fields {
		// A field the dictionary owns is NOT written here, so walking past it would misalign every
		// field after it.
		if b.schema.fields[i].kind != akString || testBit(esc, i) || b.dictOwns(i) {
			continue
		}
		l, n := binary.Uvarint(region)
		if n <= 0 || int(l)+n > len(region) {
			return "", false
		}
		if i == c.fieldIdx {
			if l == 0 {
				return "", true
			}
			return unsafe.String(&region[n], int(l)), true
		}
		region = region[n+int(l):]
	}
	return "", false
}

// escapedGroupKey reads the group value of a record whose column slot does not hold it: the value is
// in the block's cold tail, or the record has no such attribute at all.
func escapedGroupKey(b *columnarBlock, k int, fieldID uint32, bc *blockCache) (groupKey, bool) {
	node, found, err := b.escapedNode(k, fieldID, bc)
	if err != nil {
		return groupKey{}, false
	}
	if !found {
		if b.heldAt(k, fieldID) {
			return groupKey{}, false // the value stayed in the record: only evaluating it answers
		}
		return groupKey{kind: gkUndef}, true
	}
	return litGroupKey(node)
}

// litGroupKey turns a wire node into a group key. ok=false for anything that is not a scalar literal
// -- an expression, a list, a nested ad -- because rendering one needs the evaluator the scan has and
// this does not.
func litGroupKey(node []byte) (groupKey, bool) {
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return groupKey{}, false
	}
	switch lit.Kind {
	case wire.LitInt:
		return groupKey{kind: gkInt, raw: lit.Int}, true
	case wire.LitReal:
		return groupKey{kind: gkReal, raw: int64(math.Float64bits(lit.Real))}, true
	case wire.LitBool:
		var i int64
		if lit.Bool {
			i = 1
		}
		return groupKey{kind: gkBool, raw: i}, true
	case wire.LitString:
		return groupKey{kind: gkString, s: lit.Str}, true
	case wire.LitUndef:
		return groupKey{kind: gkUndef}, true
	case wire.LitError:
		return groupKey{kind: gkError}, true
	}
	return groupKey{}, false
}

// adGroupKey derives a group key from a record's own wire node, for the row fallbacks. An attribute
// the record does not have groups as `undefined`, which is what the scan's projection yields for it.
func adGroupKey(node []byte, found bool) (groupKey, bool) {
	if !found {
		return groupKey{kind: gkUndef}, true
	}
	return litGroupKey(node)
}

// groupAccum is the accumulation map plus the one thing every tier has to agree on: how a group
// value becomes a group key. Bundling them is what stops one tier forming a raw key while its
// sibling forms a bucketed one, which would split a group in two and read as a plain wrong count.
//
// A bucket width > 0 floors the value into fixed-width buckets -- the shape a time series asks for,
// where the group is the day rather than the second.
type groupAccum struct {
	acc    map[groupKey]*groupAcc
	bucket int64 // 0 = group by the raw value

	// num indexes acc's non-string groups by a key with no string in it. groupKey's string field
	// makes the runtime hash and compare it field by field, a string hash included, even when the
	// string is empty; this key has no pointers, so it hashes and compares as 16 bytes of memory.
	// A numeric group column looks up a group once per record, so that is the per-record cost.
	// It points at the same accumulators as acc, which stays the one place results are read from.
	num map[numGroupKey]*groupAcc

	// The bucket memo: the half-open interval of VALUES that fell in the previous record's bucket,
	// and that bucket's accumulator. Only meaningful when bucket > 0.
	//
	// Records arrive in insertion order and a bucketed group column advances slowly against its
	// width -- an hour bucket over a per-second timestamp holds thousands of consecutive records --
	// so the next record is overwhelmingly in the bucket the last one was. Remembering the INTERVAL
	// rather than the resulting key is what makes that worth having: the per-record cost of a
	// bucketed group is the float divide and floor in bucketOf, which runs before any map is
	// touched, so a memo on the finished key would still pay for the divide. Two compares replace
	// the divide, the floor and the map probe together.
	//
	// bHi is pulled a few ulps BELOW the interval's true top edge, because bucketOf reaches its
	// bucket through a float division whose rounding can place a value within about an ulp of that
	// edge in the next bucket. Admitting slightly less than the real interval only sends those few
	// values down the slow path, where bucketOf itself decides; admitting slightly more would group
	// one of them differently from every other tier.
	bLo, bHi float64
	bAcc     *groupAcc
}

// bucketMemoMargin is how far inside its top edge a bucket interval has to be for the memo to decide
// a value, as a fraction of the edge. bucketOf's quotient is off by at most half an ulp, which is
// worth at most the edge times 2^-53 once it is scaled back up, so eight times that is margin to
// spare. At the magnitudes a timestamp or a counter has, the excluded band is a fraction of a second
// wide and no ordinary value is ever in it; TestBucketMemoDeclinesTheCoarseTopEdge covers one that
// is.
const bucketMemoMargin = 1.0 / (1 << 50)

// numGroupKey is groupKey without the string, for a group whose value is not one.
type numGroupKey struct {
	raw  int64
	kind groupValKind
}

func newGroupAccum(bucket int64) *groupAccum {
	return &groupAccum{acc: map[groupKey]*groupAcc{}, bucket: bucket, num: map[numGroupKey]*groupAcc{}}
}

// accFor returns key's accumulator and counts the record against it, applying the bucket.
//
//	g != nil, ok     accumulate this record's aggregates into g
//	g == nil, ok     this record is in no group; skip it
//
// The second only happens under a bucket. Only a FINITE number can be floored, so a string,
// boolean, undefined, error, NaN or infinity has no bucket to land in -- and db.bucketKeyText drops
// such a record from the series rather than bucketing it as zero, so this drops it too.
func (a *groupAccum) accFor(key groupKey, nAgg int) (*groupAcc, bool) {
	if a.bucket > 0 {
		// The type test stays ahead of the memo. A string, boolean, undefined or error has no
		// number at all, so it is in no bucket however near the last one its raw bits sit --
		// number() is a switch on the kind, and it is the divide below that this avoids, not this.
		f, isNum := key.number()
		if !isNum {
			return nil, true
		}
		// The interval memo. False for a NaN -- every comparison against one is -- so a non-finite
		// value falls through to bucketOf, which is the only thing that decides it is in no bucket.
		if g := a.bAcc; g != nil && a.bLo <= f && f < a.bHi {
			g.n++
			return g, true
		}
		return a.enterBucket(f, nAgg)
	}
	// No memo without a bucket. The same one-entry memo on the raw KEY was measured here and did
	// not pay: an unbucketed group column repeats its value only when the records themselves do,
	// which is a far shorter run than a bucket's, and the key compare it saves is a map probe
	// rather than the divide a bucket costs. It was a few percent either way on a column whose
	// values run in short blocks and a steady loss on every column that cycles.
	if key.kind == gkString {
		g := groupAccFor(a.acc, key, nAgg)
		g.n++
		return g, true
	}
	// Open-coded rather than through numAcc, which the compiler does not inline. This runs once per
	// RECORD on an unbucketed numeric column, where a call is worth around a tenth of the step; the
	// bucketed path above calls it only when the memo misses, which is once per bucket.
	nk := numGroupKey{raw: key.raw, kind: key.kind}
	g := a.num[nk]
	if g == nil {
		g = groupAccFor(a.acc, key, nAgg)
		a.num[nk] = g
	}
	g.n++
	return g, true
}

// enterBucket floors f into its bucket, finds that bucket's accumulator and arms the interval memo
// with it. It is accFor's bucketed slow path: reached on the first record of each bucket, and on any
// record the memo declines to decide.
//
// Kept out of accFor rather than written inline, because accFor's hot path is a handful of
// instructions and the code around it is what decides how they are laid out: folding this in cost the
// UNBUCKETED path measurably while doing nothing for the bucketed one, which runs this once per
// bucket.
func (a *groupAccum) enterBucket(f float64, nAgg int) (*groupAcc, bool) {
	// The floor goes through float64 and renders as an integer because that is what
	// db.bucketKeyText does: a bucket start is a whole number whatever the column's own type, so an
	// int column and a real column land in the same group. Sharing the arithmetic rather than
	// writing it twice is what keeps the two paths from disagreeing.
	//
	// A number can still be in no bucket: a NaN or an infinity lies in no half-open interval
	// [b, b+width), so bucketOf declines it and the record leaves the series the same way a string
	// does. See bucketOf for why that beats letting the int64 cast invent a bucket.
	b, inBucket := bucketOf(f, a.bucket)
	if !inBucket {
		return nil, true
	}
	g := a.numAcc(groupKey{raw: b, kind: gkInt}, nAgg)
	g.n++
	lo := float64(b)
	hi := lo + float64(a.bucket)
	a.bLo, a.bHi, a.bAcc = lo, hi-math.Abs(hi)*bucketMemoMargin, g
	return g, true
}

// numAcc returns a non-string key's accumulator through the pointer-free index, creating the group
// when it is new. The index points at the same accumulators acc holds, which stays the one place
// results are read from.
func (a *groupAccum) numAcc(key groupKey, nAgg int) *groupAcc {
	nk := numGroupKey{raw: key.raw, kind: key.kind}
	g := a.num[nk]
	if g == nil {
		g = groupAccFor(a.acc, key, nAgg)
		a.num[nk] = g
	}
	return g
}

// groupAccFor returns key's accumulator, creating it when the group is new.
func groupAccFor(acc map[groupKey]*groupAcc, key groupKey, nAgg int) *groupAcc {
	if g := acc[key]; g != nil {
		return g
	}
	// A string key may alias a block's decompressed region. A LOOKUP never retains it, which is why
	// the hit above is free, but the map outlives the block and its keys are handed to the caller as
	// values -- so retaining the alias would pin a whole decompressed region per distinct value, and
	// would survive the block cache evicting and re-decompressing that region into a different
	// array. Copy on insert only: once per group, not once per record.
	if key.kind == gkString {
		key.s = strings.Clone(key.s)
	}
	g := newGroupAcc(nAgg)
	acc[key] = g
	return g
}
