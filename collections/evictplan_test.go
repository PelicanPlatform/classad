package collections

import "testing"

// segWithValues builds a sealed segment whose profile says it holds `each` bytes at every
// listed value. bucket is the profile's quantile resolution.
func segWithValues(bucket int64, each int64, values ...float64) *segment {
	p := &profileAcc{attr: "T"}
	for _, v := range values {
		p.add(v, each, true)
	}
	return &segment{profile: p.finish(bucket, 0, 0)}
}

// THE case this exists for. Segment 0 is the oldest by position but holds the NEWEST data;
// segment 1 is a spoke's backfill, appended later and far older. The positional rule drops
// segment 0 and keeps the backfill -- exactly backwards.
func TestPlanEvictionIgnoresAppendOrder(t *testing.T) {
	segs := []*segment{
		segWithValues(100, 50, 2000, 2001, 2002, 2003), // newest data, oldest position
		segWithValues(100, 50, 1000, 1001, 1002, 1003), // a backfill: older data, newer position
	}
	plan := planEviction(segs, nil, 100, "T")
	if !plan.enough {
		t.Fatal("could not reach the requested bytes")
	}
	if plan.cutoff > 2000 {
		t.Errorf("cutoff %v reaches into the newest segment's data", plan.cutoff)
	}
	for _, i := range append(append([]int{}, plan.whole...), plan.partial...) {
		if i == 0 {
			t.Errorf("selected segment 0, which holds the newest data (append order drove the choice)")
		}
	}
	if len(plan.whole)+len(plan.partial) == 0 {
		t.Error("selected nothing")
	}
}

func TestPlanEvictionFreesAtLeastWhatWasAsked(t *testing.T) {
	segs := []*segment{
		segWithValues(100, 50, 10, 11, 12, 13, 14, 15),
		segWithValues(100, 50, 20, 21, 22, 23, 24, 25),
	}
	for _, need := range []int64{100, 200, 300} {
		plan := planEviction(segs, nil, need, "T")
		if !plan.enough {
			t.Errorf("need=%d: not enough", need)
			continue
		}
		if plan.freed < need {
			t.Errorf("need=%d: freed bound %d is below it", need, plan.freed)
		}
		// The bound must also be true -- recount from the cutoff.
		var recount int64
		for _, s := range segs {
			b, _ := s.profile.bytesBelow(plan.cutoff)
			recount += b
		}
		if recount < need {
			t.Errorf("need=%d: cutoff %v really frees only %d", need, plan.cutoff, recount)
		}
	}
}

func TestPlanEvictionTakesWholeSegmentsWhole(t *testing.T) {
	segs := []*segment{
		segWithValues(100, 50, 1, 2, 3, 4), // entirely below any cutoff above 4
		segWithValues(100, 50, 100, 101, 102, 103),
	}
	plan := planEviction(segs, nil, 200, "T")
	if !plan.enough {
		t.Fatal("not enough")
	}
	if len(plan.whole) != 1 || plan.whole[0] != 0 {
		t.Errorf("whole = %v, want [0]", plan.whole)
	}
}

// An unprofiled segment is UNKNOWN, not empty. Treating it as holding nothing below the
// cutoff would be harmless; treating it as holding everything would evict it first. It must
// simply not be selected -- the positional fallback is what reclaims it.
func TestPlanEvictionSkipsUnprofiledSegments(t *testing.T) {
	segs := []*segment{
		{}, // no profile at all
		segWithValues(100, 50, 1, 2, 3, 4),
	}
	plan := planEviction(segs, nil, 100, "T")
	if !plan.enough {
		t.Fatal("not enough")
	}
	for _, i := range append(append([]int{}, plan.whole...), plan.partial...) {
		if i == 0 {
			t.Error("selected the unprofiled segment")
		}
	}
}

// A segment holding bytes whose value could not be bounded is never taken WHOLE, however
// low its known values are: the unknown bytes might be anything.
func TestPlanEvictionNeverTakesUnknownBytesWhole(t *testing.T) {
	p := &profileAcc{attr: "T"}
	p.add(1, 100, true)
	p.add(0, 100, false) // an expression
	segs := []*segment{{profile: p.finish(100, 0, 0)}}

	plan := planEviction(segs, nil, 100, "T")
	for _, i := range plan.whole {
		if i == 0 {
			t.Error("took a segment holding unbounded bytes whole")
		}
	}
}

// Asking for more than the profiles can account for must say so, not evict everything
// visible and call it done.
func TestPlanEvictionReportsWhenItCannotReach(t *testing.T) {
	segs := []*segment{segWithValues(100, 50, 1, 2, 3, 4)}
	plan := planEviction(segs, nil, 1<<30, "T")
	if plan.enough {
		t.Error("claimed to reach a target far beyond what the segments hold")
	}
	if len(plan.whole)+len(plan.partial) != 0 {
		t.Errorf("selected segments anyway: whole=%v partial=%v", plan.whole, plan.partial)
	}
}

// The active segment is never evicted: Rotate has the same rule, so a size pass cannot race
// the writer.
func TestPlanEvictionNeverTakesTheActiveSegment(t *testing.T) {
	act := segWithValues(100, 50, 1, 2, 3, 4)
	segs := []*segment{act, segWithValues(100, 50, 5, 6, 7, 8)}
	plan := planEviction(segs, act, 100, "T")
	for _, i := range append(append([]int{}, plan.whole...), plan.partial...) {
		if segs[i] == act {
			t.Error("selected the active segment")
		}
	}
}

// Rewriting a straddler costs the bytes it KEEPS, so the list has to start with the one
// that keeps least.
func TestPlanEvictionOrdersPartialsMostFreeableFirst(t *testing.T) {
	segs := []*segment{
		segWithValues(100, 50, 1, 2, 90, 91, 92, 93), // ~1 bucket below a cutoff near 50
		segWithValues(100, 50, 1, 2, 3, 4, 5, 95),    // ~2 buckets below it
	}
	plan := planEviction(segs, nil, 250, "T")
	if len(plan.partial) < 2 {
		t.Skipf("fixture produced %d straddlers, not 2", len(plan.partial))
	}
	first, _ := segs[plan.partial[0]].profile.bytesBelow(plan.cutoff)
	second, _ := segs[plan.partial[1]].profile.bytesBelow(plan.cutoff)
	if first < second {
		t.Errorf("partials out of order: first frees %d, second %d", first, second)
	}
}

// segsByValueAsc is the ordering the size pass actually evicts by, and it is a different
// code path from planEviction. A segment holding bytes whose value could not be bounded must
// not appear in it at all: its valueMax covers only the values that WERE bounded, so
// evicting on it means evicting on a number that does not describe the segment.
func TestSegsByValueAscExcludesUnbounded(t *testing.T) {
	clean := &profileAcc{attr: "T"}
	clean.add(500, 100, true)

	dirty := &profileAcc{attr: "T"}
	dirty.add(1, 100, true)  // a low known value...
	dirty.add(0, 100, false) // ...beside bytes nothing can bound

	sh := &shard{segs: []*segment{
		{profile: clean.finish(100, 0, 0)},
		{profile: dirty.finish(100, 0, 0)},
	}}

	got := segsByValueAsc(sh, "T")
	for _, i := range got {
		if i == 1 {
			t.Error("listed a segment holding unbounded bytes: its valueMax does not describe it")
		}
	}
	if len(got) != 1 || got[0] != 0 {
		t.Errorf("segsByValueAsc = %v, want [0]", got)
	}
}

// Ordering is by the newest value a segment holds, ascending -- not by position.
func TestSegsByValueAscOrdersByNewestValue(t *testing.T) {
	mk := func(vals ...float64) *segment {
		p := &profileAcc{attr: "T"}
		for _, v := range vals {
			p.add(v, 100, true)
		}
		return &segment{profile: p.finish(100, 0, 0)}
	}
	sh := &shard{segs: []*segment{
		mk(900, 901), // oldest POSITION, newest data
		mk(100, 101), // newest position, oldest data
		mk(500, 501),
	}}
	got := segsByValueAsc(sh, "T")
	want := []int{1, 2, 0}
	if len(got) != len(want) {
		t.Fatalf("segsByValueAsc = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("segsByValueAsc = %v, want %v (position drove the order)", got, want)
		}
	}
}

// A profile built over a DIFFERENT attribute must not be ordered on: its values are in
// another space entirely.
func TestSegsByValueAscIgnoresOtherAttributes(t *testing.T) {
	p := &profileAcc{attr: "Other"}
	p.add(1, 100, true)
	sh := &shard{segs: []*segment{{profile: p.finish(100, 0, 0)}}}
	if got := segsByValueAsc(sh, "T"); len(got) != 0 {
		t.Errorf("segsByValueAsc = %v, want none (profile is over %q)", got, "Other")
	}
}
