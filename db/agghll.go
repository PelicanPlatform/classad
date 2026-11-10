package db

import (
	"math"
	"math/bits"

	"github.com/PelicanPlatform/classad/classad"
)

// The HyperLogLog behind AggApproxCountDistinct: one sketch per group, built DURING THE SCAN
// from the values the scan is already yielding.
//
// WHY IT IS BUILT HERE AND NOT READ FROM THE INDEX. The storage layer persists a sketch per
// segment per INDEXED attribute (collections keeps one beside each segment's exact distinct
// count), and merging those would answer a distinct count without decoding a single record.
// It would also answer a different question. Those sketches cover a whole segment and only an
// indexed attribute, so they can serve one shape -- an unconstrained, ungrouped count over an
// indexed attribute -- and nothing else: a WHERE clause selects a subset of the segment the
// sketch cannot subtract from itself, a GROUP BY partitions it into groups the sketch has no
// per-group registers for, and a per-aggregate FILTER does both at once. Accumulating per
// group as the rows arrive costs the scan the scan was going to pay anyway and composes with
// all three, which is the whole reason this aggregate is usable in a real query rather than in
// one query.
//
// WHAT THE SKETCH BUYS. The exact AggCountDistinct keeps one map entry per distinct value per
// group, so its state grows with the answer: COUNT(DISTINCT GlobalJobId) over a few hundred
// million archived rows holds every job id in memory to produce one number. This holds a fixed
// register array per group whatever the cardinality, which is what makes the same query a
// bounded amount of memory instead of a proportional one.

const (
	// approxDistinctPrecision is the HyperLogLog precision: the number of leading hash bits
	// that pick a register, so there are 2^p of them. It is a CONSTANT and not a per-query
	// knob, because the bound on memory per group is the point of the aggregate -- a request
	// that could raise it could reintroduce exactly the growth the sketch exists to avoid,
	// and two queries answering the same question with different accuracy is a worse problem
	// than either accuracy.
	//
	// p=14 is the standard choice: 16384 registers at one byte each, so 16 KiB per group per
	// aggregate, with a relative standard error of 1.04/sqrt(16384) ~ 0.81%. Changing it
	// changes every answer this aggregate gives, so it is stated in ApproxDistinctRelErr for
	// callers and derived from here for the code, and never written down twice.
	//
	// THE FOOTPRINT IS PER GROUP, which is what decides when this aggregate is the wrong one.
	// A query grouping by a high-cardinality column asks for 16 KiB times the number of
	// groups whether or not any group holds many values, and below roughly two thousand
	// distinct values per group the exact set is both smaller AND right. The sketch wins when
	// the cardinality INSIDE a group is large, not when the number of groups is.
	approxDistinctPrecision = 14
	approxDistinctRegisters = 1 << approxDistinctPrecision
)

// approxDistinctAlpha is the bias-correction constant for m = approxDistinctRegisters, from
// the HyperLogLog paper's large-m form.
const approxDistinctAlpha = 0.7213 / (1 + 1.079/approxDistinctRegisters)

// ApproxDistinctRelErr is the relative standard error of the estimate AggApproxCountDistinct
// returns: 1.04/sqrt(m), about 0.81% at the fixed precision.
//
// It is EXPORTED because a caller rendering the number owes its reader the fact that it is an
// estimate, and a caller cannot state the error without being told it. It is a standard
// DEVIATION and not a bound: roughly a third of groups land further out than this, and a
// single group can land several multiples of it away. A caller presenting it as a tolerance
// -- "within 0.81%" -- is making a promise the sketch does not make.
//
// It is a CONSTANT and not a variable because it describes the sketch rather than configuring
// it: assigning to it would change what every caller is told about an answer without changing
// a single answer. The square root is written out because a constant expression cannot take
// one; the declaration above it fails to compile if the precision moves and leaves the two
// disagreeing.
const _ = uint(approxDistinctRegisters - 128*128) // sqrt(approxDistinctRegisters) == 128

const ApproxDistinctRelErr = 1.04 / 128.0

// approxDistinctAcc is one group's sketch. It satisfies Accumulator, which is not decoration:
// the laws that interface documents (Merge associative AND commutative, Result repeatable and
// non-mutating) are exactly the laws a parallel reduction needs from this, and expressing it
// as an Accumulator is what lets CheckAggregate test them against the code the engine runs
// rather than against a test-only restatement of it.
type approxDistinctAcc struct {
	// reg holds one register per hash bucket, each the largest leading-zero run (plus one)
	// seen for that bucket. It is nil until the first value arrives, so a group all of whose
	// rows fail this aggregate's FILTER -- a group that still appears in the output, with an
	// answer -- costs nothing but the pointer.
	reg []uint8
}

func newApproxDistinctAcc() *approxDistinctAcc { return &approxDistinctAcc{} }

// Add folds one row's argument value in, skipping undefined and error exactly as the exact
// AggCountDistinct does: those are not values the attribute took, so a row missing the
// attribute must not count as a distinct one.
//
// The value is keyed through GroupValueText, the same rendering the group tuple uses, so the
// two counts agree on what "the same value" means: an approximate COUNT(DISTINCT N) and a
// GROUP BY N count the same partition, 1 and 1.0 included.
func (a *approxDistinctAcc) Add(v classad.Value) {
	if v.IsUndefined() || v.IsError() {
		return
	}
	a.addText(GroupValueText(v))
}

// addText folds an already-rendered group key in, for the reduction, which has the text in
// hand from the grouping it just did.
func (a *approxDistinctAcc) addText(s string) {
	x := hashApproxDistinctKey(s)
	if a.reg == nil {
		a.reg = make([]uint8, approxDistinctRegisters)
	}
	// The top p bits pick the register; the rest's leading-zero run sizes the estimate. The
	// guard bit bounds that run, so a hash whose remaining bits are all zero yields the
	// largest rank the word can express rather than running off the end of it.
	idx := x >> (64 - approxDistinctPrecision)
	w := (x << approxDistinctPrecision) | (1 << (approxDistinctPrecision - 1))
	if rank := uint8(bits.LeadingZeros64(w)) + 1; rank > a.reg[idx] {
		a.reg[idx] = rank
	}
}

// Merge folds another group's partial sketch into this one.
//
// IT IS A REGISTER-WISE MAXIMUM, which is the property the whole arrangement rests on. Max is
// associative, commutative and idempotent, so the merged sketch describes the UNION of what
// the two saw however the rows were divided and in whatever order the partials are combined
// -- and a value both sides saw raises one register once rather than being counted twice.
// That is what makes the aggregate safe under a fan-out whose segment-to-worker assignment is
// a race, and it is pinned by a test rather than argued here.
func (a *approxDistinctAcc) Merge(other Accumulator) {
	o, ok := other.(*approxDistinctAcc)
	if !ok || o.reg == nil {
		return // nothing was added on the other side: the identity
	}
	if a.reg == nil {
		// Copied rather than adopted. The contract permits consuming the other side, but a
		// caller that merges one partial into several targets would then have them sharing
		// registers, and the symptom -- counts that are right on the first merge and too
		// high afterwards -- is far more expensive to find than this one allocation.
		a.reg = append([]uint8(nil), o.reg...)
		return
	}
	for i, r := range o.reg {
		if r > a.reg[i] {
			a.reg[i] = r
		}
	}
}

// Result is the estimate as an integer value. It reads the registers without touching them,
// so it may be called before a further merge and again afterwards.
func (a *approxDistinctAcc) Result() classad.Value {
	return classad.NewIntValue(int64(a.estimate()))
}

// estimate is the HyperLogLog estimator: the harmonic mean of the registers, bias-corrected,
// with linear counting substituted in the small-cardinality range where the raw estimator is
// badly biased and the register array is mostly empty.
//
// There is no large-range correction. That one exists to undo hash collisions as the count
// approaches the hash space, and a 64-bit hash puts that point far past any table this will
// run over.
//
// THE REMAINING BIAS IS WORTH KNOWING ABOUT RATHER THAN HIDING. Just above the linear-counting
// handover -- cardinalities of a few times m, so roughly 40k to 100k here -- the raw estimator
// reads about one to two percent HIGH, which is a bias and not the spread: it is in the same
// direction every time, so averaging several groups does not wash it out. Correcting it takes
// a table of empirically fitted offsets per precision, which buys a percent in one band at the
// cost of a block of constants nobody can check by reading. The error is stated instead, here
// and in ApproxDistinctRelErr, and the tests record the observed error at each decade so a
// reader can see the shape of it.
func (a *approxDistinctAcc) estimate() uint64 {
	if a.reg == nil {
		return 0 // a group that saw no defined value has no distinct values, exactly
	}
	m := float64(approxDistinctRegisters)
	var sum float64
	zeros := 0
	for _, r := range a.reg {
		sum += 1 / float64(uint64(1)<<r)
		if r == 0 {
			zeros++
		}
	}
	est := approxDistinctAlpha * m * m / sum
	if est <= 2.5*m && zeros > 0 {
		// Small range: linear counting off the empty-register count is more accurate here,
		// and it is EXACT for a cardinality small against m, which is why a handful of
		// distinct values comes back as that handful rather than as an estimate near it.
		est = m * math.Log(m/float64(zeros))
	}
	return uint64(math.Round(est))
}

// hashApproxDistinctKey hashes a rendered group key to 64 bits: FNV-1a followed by an
// avalanche finalizer.
//
// THE FINALIZER IS LOAD-BEARING. The register index comes from the TOP bits of the hash, and
// raw FNV-1a mixes its high bits poorly for short, similar keys -- "v0" through "v9", or a
// run of job ids differing in their last characters -- which collapses them onto one register
// and makes the estimate collapse with them. The finalizer spreads a one-byte difference
// across the whole word before anything reads the top of it.
//
// IT IS A FIXED HASH, not a seeded or process-random one. Two processes asking the same
// question of the same data get the same number, a sketch is comparable across runs, and the
// error of a given query is a property of that query rather than a lottery held at startup --
// which is also what lets a test over known data assert the estimate rather than a range
// around it.
func hashApproxDistinctKey(s string) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime64
	}
	// splitmix64's finalizer: three xor-shift-multiply rounds, enough that every input bit
	// affects every output bit.
	h ^= h >> 33
	h *= 0xff51afd7ed558ccd
	h ^= h >> 33
	h *= 0xc4ceb9fe1a85ec53
	h ^= h >> 33
	return h
}
