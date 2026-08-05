package db

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// whereRefCount is the obviously-correct reference: fetch every matching ad and count by
// hand. The constrained index path is only trustworthy if it agrees with this.
func whereRefCount(t *testing.T, hist *ArchiveTable, attr, constraint string) map[string]int64 {
	t.Helper()
	seq, err := hist.Query(constraint)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for ad := range seq {
		v, ok := ad.EvaluateAttrString(attr)
		if !ok {
			continue
		}
		got[v]++
	}
	return got
}

func mkWhereHist(t *testing.T, n int) *ArchiveTable {
	t.Helper()
	owners := []string{"alice", "bob", "carol", "dave"}
	ads := make([]string, n)
	for i := range ads {
		ads[i] = fmt.Sprintf("CompletionDate = %d\nRequestMemory = %d\nOwner = %q\nPad = %q",
			1700000000+i*60, 1024+(i%8)*512, owners[i%len(owners)], strings.Repeat("q", 120))
	}
	return mkHist(t, ArchiveConfig{
		SegmentSize:      1 << 16,
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate", "RequestMemory"},
	}, ads)
}

// TestGroupCountsWhereMatchesScan is the differential test over many constraint shapes,
// including randomized ranges. Whenever the index path answers, it must equal the scan; the
// shapes it refuses simply fall through (and are covered separately below).
func TestGroupCountsWhereMatchesScan(t *testing.T) {
	const n = 3000
	hist := mkWhereHist(t, n)
	base := int64(1700000000)

	var constraints []string
	for _, c := range []string{
		"CompletionDate >= %d",
		"CompletionDate > %d",
		"CompletionDate <= %d",
		"CompletionDate < %d",
		"CompletionDate == %d",
	} {
		constraints = append(constraints, fmt.Sprintf(c, base+1000*60))
	}
	constraints = append(constraints,
		fmt.Sprintf("CompletionDate >= %d && CompletionDate < %d", base+500*60, base+1500*60),
		fmt.Sprintf("%d <= CompletionDate && CompletionDate < %d", base+100*60, base+200*60),
		fmt.Sprintf("(CompletionDate >= %d) && (CompletionDate < %d)", base, base+60),
		"RequestMemory >= 2048",
		fmt.Sprintf("RequestMemory >= 1536 && CompletionDate < %d", base+900*60),
		fmt.Sprintf("CompletionDate >= %d", base+n*60), // matches nothing
		fmt.Sprintf("CompletionDate >= %d", base-1),    // matches everything
	)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 25; i++ {
		lo := rng.Intn(n)
		hi := lo + rng.Intn(n-lo+1)
		constraints = append(constraints, fmt.Sprintf("CompletionDate >= %d && CompletionDate <= %d",
			base+int64(lo)*60, base+int64(hi)*60))
	}

	answered := 0
	for _, c := range constraints {
		got, ok := hist.CategoricalGroupCountsWhere("Owner", c)
		if !ok {
			continue
		}
		answered++
		want := whereRefCount(t, hist, "Owner", c)
		if len(got) != len(want) {
			t.Errorf("constraint %q: groups = %v, want %v", c, got, want)
			continue
		}
		for k, w := range want {
			if got[k] != w {
				t.Errorf("constraint %q: group %q = %d, want %d", c, k, got[k], w)
			}
		}
	}
	if answered < len(constraints)/2 {
		t.Errorf("index path answered only %d of %d constraints; it should handle plain conjunctive ranges",
			answered, len(constraints))
	}
}

// TestGroupCountsWhereRefusesUnsafeShapes is the important one. Each of these constraints
// is NOT equivalent to a conjunction of range conditions, so attributing whole segments
// from zone bounds would be wrong. The path must refuse them rather than answer.
func TestGroupCountsWhereRefusesUnsafeShapes(t *testing.T) {
	hist := mkWhereHist(t, 500)
	base := 1700000000
	unsafe := []string{
		fmt.Sprintf("CompletionDate < %d || CompletionDate > %d", base+60, base+120), // disjunction
		fmt.Sprintf("!(CompletionDate < %d)", base+60),                               // negation
		fmt.Sprintf("CompletionDate != %d", base+60),                                 // inequality
		fmt.Sprintf("CompletionDate =?= %d", base+60),                                // meta-equality
		"CompletionDate >= RequestMemory",                                            // attr vs attr
		fmt.Sprintf("TARGET.CompletionDate >= %d", base),                             // scoped
		fmt.Sprintf("CompletionDate + 1 >= %d", base),                                // arithmetic
		`Owner == "alice"`, // string comparison
		fmt.Sprintf("size(Owner) >= 1 && CompletionDate >= %d", base), // function call
		fmt.Sprintf("CompletionDate >= %d ? true : false", base),      // conditional
		"JobStatus >= 1", // not zone-mapped
		"true",           // no conditions
	}
	for _, c := range unsafe {
		t.Run(c, func(t *testing.T) {
			if _, ok := hist.CategoricalGroupCountsWhere("Owner", c); ok {
				t.Errorf("constraint %q was answered from the index; it is not a conjunction of "+
					"zone-decidable range conditions and must be refused", c)
			}
		})
	}
}

// TestGroupCountsWhereEmptyAndFull pins the two degenerate outcomes, where an off-by-one in
// the zone-bound reasoning would be easy to miss.
func TestGroupCountsWhereEmptyAndFull(t *testing.T) {
	const n = 1200
	hist := mkWhereHist(t, n)
	base := int64(1700000000)

	got, ok := hist.CategoricalGroupCountsWhere("Owner", fmt.Sprintf("CompletionDate > %d", base+int64(n)*60))
	if !ok {
		t.Fatal("expected the index path to answer a matches-nothing range")
	}
	if len(got) != 0 {
		t.Errorf("matches-nothing range returned %v, want empty", got)
	}

	got, ok = hist.CategoricalGroupCountsWhere("Owner", fmt.Sprintf("CompletionDate >= %d", base))
	if !ok {
		t.Fatal("expected the index path to answer a matches-everything range")
	}
	var total int64
	for _, v := range got {
		total += v
	}
	if total != n {
		t.Errorf("matches-everything range counted %d, want %d", total, n)
	}
}

// TestConstrainedAggregateMatchesScan drives the constrained path through the public
// Aggregate entry point, so the wiring (not just the helper) is covered -- including the
// shapes that must fall through to the scan and still produce the right answer.
func TestConstrainedAggregateMatchesScan(t *testing.T) {
	const n = 2000
	hist := mkWhereHist(t, n)
	base := int64(1700000000)
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	constraints := []string{
		fmt.Sprintf("CompletionDate >= %d && CompletionDate < %d", base+300*60, base+900*60),
		fmt.Sprintf("CompletionDate < %d || CompletionDate > %d", base+60, base+120), // falls through
		`Owner == "alice"`, // falls through
		"RequestMemory >= 2048",
		"true",
	}
	for _, c := range constraints {
		t.Run(c, func(t *testing.T) {
			rows, err := hist.Aggregate(c, []string{"Owner"}, aggs)
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]int64{}
			for _, r := range rows {
				var v int64
				fmt.Sscan(r.Values[0], &v)
				got[r.Group[0]] = v
			}
			want := whereRefCount(t, hist, "Owner", c)
			if len(got) != len(want) {
				t.Fatalf("groups = %v, want %v", got, want)
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("group %q = %d, want %d", k, got[k], w)
				}
			}
		})
	}
}

// TestBucketedAggregateIgnoresNoConstraint guards the case-2 fast path: the bucketed helper
// takes no constraint, so a WHERE-bearing bucketed aggregate must not be answered from it.
func TestBucketedAggregateIgnoresNoConstraint(t *testing.T) {
	const n = 1500
	const day = 86400
	hist := mkWhereHist(t, n)
	base := int64(1700000000)
	groups := []GroupCol{{Attr: "Owner"}, {Attr: "CompletionDate", BucketWidth: day}}
	constraint := fmt.Sprintf("CompletionDate >= %d", base+600*60)

	rows, err := hist.AggregateCols(constraint, groups, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range rows {
		var v int64
		fmt.Sscan(r.Values[0], &v)
		total += v
	}
	want := int64(0)
	for _, c := range whereRefCount(t, hist, "Owner", constraint) {
		want += c
	}
	if total != want {
		t.Errorf("constrained bucketed aggregate counted %d, want %d (the WHERE was dropped)", total, want)
	}
}
