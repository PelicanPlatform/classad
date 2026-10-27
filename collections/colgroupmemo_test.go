package collections

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// memoKeys builds an adversarial sequence of group keys for one bucket width: long runs inside a
// bucket (what the memo is for), values sitting exactly on and immediately around a boundary (where a
// memo interval can disagree with the division bucketOf does), negatives, reals with fractional parts,
// the three non-finite values, and the non-numeric kinds that have no bucket at all.
//
// The order matters as much as the values: the memo is STATE, so what it answers for a key depends on
// the key before it. Every interesting value is therefore placed directly after a value that would
// have primed the memo with a neighbouring interval.
func memoKeys(width int64, rng *rand.Rand) []groupComp {
	intKey := func(i int64) groupComp { return groupComp{kind: gkInt, raw: i} }
	realKey := func(f float64) groupComp {
		return groupComp{kind: gkReal, raw: int64(math.Float64bits(f))}
	}
	w := float64(width)
	var keys []groupComp
	for _, base := range []int64{0, width, 17 * width, -3 * width, 1700000000 / width * width} {
		lo := float64(base)
		hi := lo + w
		// A run inside the bucket, then each way out of it.
		for j := 0; j < 8; j++ {
			keys = append(keys, intKey(base+rng.Int63n(max(width, 1))), realKey(lo+rng.Float64()*w))
		}
		keys = append(keys,
			realKey(lo), realKey(math.Nextafter(lo, math.Inf(-1))),
			realKey(math.Nextafter(lo, math.Inf(1))),
			realKey(hi), realKey(math.Nextafter(hi, math.Inf(-1))),
			realKey(math.Nextafter(math.Nextafter(hi, math.Inf(-1)), math.Inf(-1))),
			realKey(hi-w*(1.0/(1<<52))), realKey(hi+w*(1.0/(1<<52))),
			intKey(base), intKey(base+width), intKey(base-1),
			// Straight back into the middle of the bucket just left, so the memo is primed
			// again for whatever comes next.
			realKey(lo+w/2),
			// In no bucket, each immediately after a value that primed the memo.
			realKey(math.NaN()), realKey(lo+w/2),
			realKey(math.Inf(1)), realKey(lo+w/2),
			realKey(math.Inf(-1)), realKey(lo+w/2),
			// No number at all: these must be dropped on their kind, not on their bits, and
			// their raw bits are deliberately those of a value inside the live bucket.
			groupComp{kind: gkString, raw: base}, realKey(lo+w/2),
			groupComp{kind: gkBool, raw: base}, realKey(lo+w/2),
			groupComp{kind: gkUndef, raw: base}, realKey(lo+w/2),
			groupComp{kind: gkError, raw: base}, realKey(lo+w/2),
		)
	}
	return keys
}

// TestBucketIntervalMemoMatchesBucketOf checks the bucketed accumulator against bucketOf itself, key
// by key, over that sequence.
//
// bucketOf is the definition every tier agrees on, and the memo answers WITHOUT running it -- it
// decides from the previous record's interval instead. So the property to hold is not that the counts
// look reasonable but that each key lands in exactly the bucket bucketOf would have given it, and that
// every key bucketOf refuses is still refused.
func TestBucketIntervalMemoMatchesBucketOf(t *testing.T) {
	for _, width := range []int64{1, 7, 3600, 86400, 1 << 40} {
		rng := rand.New(rand.NewSource(width))
		keys := memoKeys(width, rng)
		a := newGroupAccum([]int64{width})
		want := map[int64]int{}
		for i, k := range keys {
			g, ok := accFor1(a, k, 0)
			if !ok {
				t.Fatalf("width %d key %d (%+v): accFor declined", width, i, k)
			}
			f, isNum := k.number()
			b, inBucket := int64(0), false
			if isNum {
				b, inBucket = bucketOf(f, width)
			}
			if !isNum || !inBucket {
				if g != nil {
					t.Errorf("width %d key %d (%+v): grouped, but it is in no bucket", width, i, k)
				}
				continue
			}
			if g == nil {
				t.Errorf("width %d key %d (%+v, value %v): dropped, but bucketOf puts it in %d",
					width, i, k, f, b)
				continue
			}
			// Which group it landed in, read back from the accumulation map: the bucket the
			// memo decided has to be the one keyed by bucketOf's own answer.
			if want := a.g1.acc[key1(groupComp{kind: gkInt, raw: b})]; want != g {
				t.Errorf("width %d key %d (%+v, value %v): did not land in bucket %d",
					width, i, k, f, b)
			}
			want[b]++
		}
		if a.nGroups() != len(want) {
			t.Errorf("width %d: %d groups, bucketOf gives %d", width, a.nGroups(), len(want))
		}
		for key, g := range a.g1.acc {
			if want[key.raw[0]] != g.n {
				t.Errorf("width %d: bucket %d counted %d, bucketOf counts %d",
					width, key.raw[0], g.n, want[key.raw[0]])
			}
		}
	}
}

// TestBucketMemoDoesNotPrimeOnNonNumbers pins the ordering inside the bucketed branch: a key with no
// number must be refused on its KIND, before the interval is consulted, and must leave the interval
// as it found it.
//
// Both halves have teeth. A string, boolean, undefined or error key still carries raw bits -- a
// boolean's are 0 or 1, and an undefined key's are whatever the key was built from -- so a memo
// consulted before the type test would hand one of them the live bucket's accumulator. And a memo
// INVALIDATED by one of them would drop the run it is in the middle of, turning every interleaved
// non-numeric record into a full recomputation for the next real one.
func TestBucketMemoDoesNotPrimeOnNonNumbers(t *testing.T) {
	const day = 86400
	a := newGroupAccum([]int64{day})
	live, ok := accFor1(a, groupComp{kind: gkInt, raw: 3*day + 10}, 0)
	if !ok || live == nil {
		t.Fatal("a plain timestamp formed no group")
	}
	for _, k := range []groupComp{
		{kind: gkString, raw: 7},
		{kind: gkBool, raw: 1},
		{kind: gkBool, raw: 3*day + 11}, // bits inside the live bucket
		{kind: gkUndef, raw: 3*day + 12},
		{kind: gkError, raw: 3*day + 13},
	} {
		g, ok := accFor1(a, k, 0)
		if !ok || g != nil {
			t.Errorf("accFor(%+v) under a bucket = %v, %v; want nil, true (no number, no bucket)", k, g, ok)
		}
	}
	if live.n != 1 {
		t.Errorf("the live bucket absorbed %d records, want 1: a non-numeric key reached the memo", live.n)
	}
	if a.mAcc != live {
		t.Error("a non-numeric key cleared the interval memo, so a run of them restarts the bucket each time")
	}
	if g, ok := accFor1(a, groupComp{kind: gkInt, raw: 3*day + 20}, 0); !ok || g != live {
		t.Errorf("the next timestamp in the same day got %v, want the group the first one made", g)
	}
	if a.nGroups() != 1 {
		t.Errorf("formed %d groups, want 1", a.nGroups())
	}
}

// TestBucketMemoDeclinesTheCoarseTopEdge is why the memo's interval stops short of its top edge.
//
// bucketOf reaches a bucket through a float DIVISION, and the quotient is rounded. At magnitudes
// where a float64 can no longer count by ones -- around 4e18, where consecutive values are hundreds
// apart -- that rounding is worth hundreds on the way back, which is enough to put a value a few
// steps below a bucket's top edge in the NEXT bucket. The interval the memo would otherwise use says
// the previous bucket, and the two answers would be visible as a record counted in the wrong day.
//
// The cases below are such values. Each asserts its own premise first: if bucketOf ever stops
// disagreeing with the plain interval here, the test says so rather than passing on a case that no
// longer tests anything.
func TestBucketMemoDeclinesTheCoarseTopEdge(t *testing.T) {
	realKey := func(f float64) groupComp {
		return groupComp{kind: gkReal, raw: int64(math.Float64bits(f))}
	}
	for _, tc := range []struct{ width, start int64 }{
		{86400, 4394786696646854400},
		{604800, 6260866483188038400},
	} {
		lo := float64(tc.start)
		mid := lo + float64(tc.width)/2
		top := math.Nextafter(lo+float64(tc.width), math.Inf(-1))
		if b, ok := bucketOf(mid, tc.width); !ok || b != tc.start {
			t.Fatalf("width %d: the interval's middle buckets to %d, not %d: the case has moved",
				tc.width, b, tc.start)
		}
		want, ok := bucketOf(top, tc.width)
		if !ok {
			t.Fatalf("width %d: %v is in no bucket", tc.width, top)
		}
		if want == tc.start {
			t.Fatalf("width %d: %v now buckets to %d like the rest of the interval, so this case no "+
				"longer exercises the top edge", tc.width, top, tc.start)
		}
		a := newGroupAccum([]int64{tc.width})
		primed, ok := accFor1(a, realKey(mid), 0)
		if !ok || primed == nil {
			t.Fatalf("width %d: the priming value formed no group", tc.width)
		}
		g, ok := accFor1(a, realKey(top), 0)
		if !ok || g == nil {
			t.Fatalf("width %d: %v formed no group, but bucketOf puts it in %d", tc.width, top, want)
		}
		if g == primed {
			t.Errorf("width %d: %v went in bucket %d off the memo's interval, bucketOf says %d",
				tc.width, top, tc.start, want)
		}
		if g != a.g1.acc[key1(groupComp{kind: gkInt, raw: want})] {
			t.Errorf("width %d: %v did not land in bucket %d", tc.width, top, want)
		}
	}
}

// TestBucketMemoDecidesARepeatedRecord fails if the bucket interval memo stops ANSWERING, at any
// arity.
//
// Nothing else here can fail that way. A memo that never hits is still CORRECT: every record it
// declines goes to the slow path, which floors the value itself and finds the same group, so every
// count a correctness test checks comes out right while the acceleration the memo exists for is
// quietly gone. The way that happens in practice is an index convention drifting -- the intervals
// are written at the BUCKET RANK and read at the COMPONENT INDEX, or the reverse -- which at any
// arity above one leaves the reader comparing against a slot that was never armed, so the test is
// `0 <= f && f < 0`, false, every record, forever. Swapping the two in either direction leaves the
// whole package green without this.
//
// The hit is made observable by removing the fallback: feed a record, then EMPTY the accumulation
// map and feed the same record again. A memo hit never touches a map, so it returns the same
// accumulator and leaves the map empty. A miss re-creates the group, which the map then shows.
func TestBucketMemoDecidesARepeatedRecord(t *testing.T) {
	for _, shape := range memoShapes() {
		a := newGroupAccum(shape.buckets)
		first, ok := accForN(a, shape.comps, 0)
		if !ok || first == nil {
			t.Fatalf("%s: the first record formed no group (ok %v)", shape.name, ok)
		}
		if got := a.nGroups(); got != 1 {
			t.Fatalf("%s: %d groups after one record, want 1", shape.name, got)
		}
		clearGroupMaps(t, a)
		second, ok := accForN(a, shape.comps, 0)
		if !ok || second == nil {
			t.Fatalf("%s: the repeated record formed no group (ok %v)", shape.name, ok)
		}
		if second != first || a.nGroups() != 0 {
			t.Fatalf("%s: the repeated record went to the map (%d groups formed, same "+
				"accumulator %v); the interval memo is meant to decide it without one, and it "+
				"cannot if the intervals are armed at one index, one rank or one WIDTH and read "+
				"at another", shape.name, a.nGroups(), second == first)
		}
	}
}

// clearGroupMaps empties whichever arity's accumulation map is live, leaving the interval memo armed.
//
// It names the arities one at a time, so raising the cap without adding a line here would leave the
// live map FULL and the test above would then read a perfectly good memo hit as a miss. t is taken
// so that it says which of the two went wrong rather than letting the caller guess.
func clearGroupMaps(t *testing.T, a *groupAccum) {
	t.Helper()
	clear(a.g1.acc)
	clear(a.g2.acc)
	clear(a.g3.acc)
	if n := a.nGroups(); n != 0 {
		t.Fatalf("arity %d: clearing left %d groups; clearGroupMaps does not know this arity, so "+
			"the memo check below would read a hit as a miss", a.n, n)
	}
}

// TestBucketMemoDeclinesARecordThatMovedBucket is the other half of the memo's contract, and the
// half that is a WRONG ANSWER rather than a slow one.
//
// The test above says a repeated record must be decided by the memo. This one says a record that
// moved to a different bucket in ANY ONE component must not be: the interval armed for a component
// has to be that component's own bucket, so a record outside it falls through to bucketOf and forms
// its own group.
//
// What it catches that nothing else does is an interval armed TOO WIDE -- a component's interval
// built from another component's bucket WIDTH. Too narrow is merely slow, and the test above finds
// it. Too wide hands the previous record's group to a record that belongs in a different one, and
// the count that comes back is simply wrong. It takes components with DIFFERENT widths to see at
// all: with equal widths every component's interval is the same size and the bug is invisible.
func TestBucketMemoDeclinesARecordThatMovedBucket(t *testing.T) {
	for _, shape := range memoShapes() {
		for moved := 0; moved < shape.n; moved++ {
			if shape.buckets[moved] == 0 {
				continue // an unbucketed component is the probe's job, not the interval's
			}
			a := newGroupAccum(shape.buckets)
			if _, ok := accForN(a, shape.comps, 0); !ok {
				t.Fatalf("%s: the first record formed no group", shape.name)
			}
			// Move this component a whole bucket on, leaving every other component alone.
			next := append([]groupComp(nil), shape.comps...)
			next[moved] = groupComp{kind: gkInt, raw: next[moved].raw + shape.buckets[moved]}
			g, ok := accForN(a, next, 0)
			if !ok || g == nil {
				t.Fatalf("%s: moving component %d formed no group", shape.name, moved)
			}
			if n := a.nGroups(); n != 2 {
				t.Fatalf("%s: component %d moved a full bucket (width %d) and the accumulator "+
					"holds %d group(s), want 2; the memo decided a record that belongs in a "+
					"different bucket, which means that component's interval was armed from "+
					"something other than its own width",
					shape.name, moved, shape.buckets[moved], n)
			}
		}
	}
}

// memoShapes enumerates the bucketing shapes the memo has to decide, at every arity the cap admits.
//
// TWO FAMILIES, because they reach different code and fail differently:
//
//   - "one bucket" is what production asks: categorical columns beside ONE time bucket. The
//     bucketed component is LAST, so its component index is n-1 while its rank is 0 -- at n == 3
//     those differ by two, which is what makes confusing the two visible at all.
//   - "all bucketed" is every component floored, which is the branch accForBucketed takes when
//     there is no unbucketed component to probe (!mCmp) -- a branch the first family never enters.
//     The widths DIFFER between components on purpose: with equal widths, arming every interval
//     from the first component's width is indistinguishable from arming each from its own, and the
//     bug that does exactly that survives. The timestamps are placed well past the narrowest width
//     into their own bucket, so an interval armed too narrow fails to contain the record it was
//     armed for and the memo misses.
func memoShapes() []struct {
	name    string
	n       int
	buckets []int64
	comps   []groupComp
} {
	const hour, day = 3600, 86400
	const stamp = 1700000000 + 12345 // 12345s into its day: past an hour, so a too-narrow interval shows
	var out []struct {
		name    string
		n       int
		buckets []int64
		comps   []groupComp
	}
	for n := 1; n <= maxGroupCols; n++ {
		one := make([]int64, n)
		one[n-1] = day
		oc := make([]groupComp, n)
		for i := 0; i < n-1; i++ {
			oc[i] = groupComp{kind: gkInt, raw: int64(100 + i)}
		}
		oc[n-1] = groupComp{kind: gkInt, raw: stamp}
		out = append(out, struct {
			name    string
			n       int
			buckets []int64
			comps   []groupComp
		}{fmt.Sprintf("arity %d, one bucket", n), n, one, oc})

		// Every component bucketed, each with its own width.
		all := make([]int64, n)
		ac := make([]groupComp, n)
		for i := 0; i < n; i++ {
			all[i] = []int64{day, hour, day * 7}[i%3]
			ac[i] = groupComp{kind: gkInt, raw: stamp}
		}
		out = append(out, struct {
			name    string
			n       int
			buckets []int64
			comps   []groupComp
		}{fmt.Sprintf("arity %d, all bucketed", n), n, all, ac})
	}
	return out
}
