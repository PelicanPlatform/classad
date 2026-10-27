package collections

import (
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
