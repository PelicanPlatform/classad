package db

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// groupCountByScan computes GROUP BY <attr> COUNT(*) the slow, obviously-correct way: it
// reads every ad back and counts in the test. The fast path is only trustworthy if it
// agrees with this on every shape, so every case below is checked against it rather than
// against hand-written expectations.
func groupCountByScan(t *testing.T, hist *ArchiveTable, attr string) map[string]int64 {
	t.Helper()
	seq, err := hist.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for ad := range seq {
		v, ok := ad.EvaluateAttrString(attr)
		if !ok {
			// The aggregate engine renders an absent or non-string value as the literal
			// "undefined" and groups those together, so the reference must do the same --
			// these records are exactly the ones the index-resident path cannot attribute,
			// and it must decline rather than drop them.
			v = "undefined"
		}
		got[v]++
	}
	return got
}

func aggGroupCounts(t *testing.T, hist *ArchiveTable, attr string) map[string]int64 {
	t.Helper()
	rows, err := hist.Aggregate("true", []string{attr}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{}
	for _, r := range rows {
		var n int64
		if _, err := fmt.Sscan(r.Values[0], &n); err != nil {
			t.Fatalf("non-numeric count %q", r.Values[0])
		}
		got[r.Group[0]] = n
	}
	return got
}

func mkHist(t *testing.T, cfg ArchiveConfig, ads []string) *ArchiveTable {
	t.Helper()
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cat.Close() })
	hist, err := cat.CreateArchiveTable("history", cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range ads {
		if err := hist.AppendOld(a); err != nil {
			t.Fatal(err)
		}
	}
	return hist
}

// TestArchiveGroupCountMatchesScan is the differential test: for each shape, the aggregate
// (which may or may not take the index-resident path) must produce exactly what a full
// scan produces. Shapes are chosen to cover the ways the index can be an incomplete
// description of the data -- a missing attribute, a non-literal expression, a mixture of
// spellings -- since those are precisely the cases where reading posting cardinalities
// would silently undercount if the fast path did not decline.
func TestArchiveGroupCountMatchesScan(t *testing.T) {
	indexed := ArchiveConfig{CategoricalAttrs: []string{"Owner"}, ZoneAttrs: []string{"CompletionDate"}}
	cases := []struct {
		name string
		cfg  ArchiveConfig
		ads  []string
	}{
		{
			name: "every record carries the attribute",
			cfg:  indexed,
			ads: []string{
				`Owner = "alice"`, `Owner = "alice"`, `Owner = "bob"`, `Owner = "carol"`,
			},
		},
		{
			name: "one record is missing the attribute",
			cfg:  indexed,
			ads:  []string{`Owner = "alice"`, `Owner = "bob"`, `JobStatus = 4`},
		},
		{
			name: "mixed case spellings are distinct groups",
			cfg:  indexed,
			ads:  []string{`Owner = "Alice"`, `Owner = "alice"`, `Owner = "ALICE"`, `Owner = "bob"`},
		},
		{
			name: "case-uniform single spelling",
			cfg:  indexed,
			ads:  []string{`Owner = "Alice"`, `Owner = "Alice"`, `Owner = "bob"`},
		},
		{
			name: "attribute holds a non-literal expression",
			cfg:  indexed,
			ads:  []string{`Owner = "alice"`, `Owner = strcat("bo", "b")`, `Owner = "carol"`},
		},
		{
			name: "attribute is not indexed at all",
			cfg:  ArchiveConfig{ValueAttrs: []string{"ClusterId"}},
			ads:  []string{`ClusterId = 1` + "\n" + `Owner = "alice"`, `ClusterId = 2` + "\n" + `Owner = "bob"`},
		},
		{
			name: "empty archive",
			cfg:  indexed,
			ads:  nil,
		},
		{
			name: "single value repeated",
			cfg:  indexed,
			ads:  []string{`Owner = "alice"`, `Owner = "alice"`, `Owner = "alice"`},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hist := mkHist(t, c.cfg, c.ads)
			want := groupCountByScan(t, hist, "Owner")
			got := aggGroupCounts(t, hist, "Owner")
			if len(got) != len(want) {
				t.Fatalf("groups = %v, want %v", got, want)
			}
			for k, w := range want {
				if got[k] != w {
					t.Errorf("group %q = %d, want %d (full result %v)", k, got[k], w, got)
				}
			}
		})
	}
}

// TestArchiveGroupCountAcrossSegments exercises the multi-segment path: counts must be
// summed across every sealed segment plus the still-open active one, which is the case a
// per-segment index walk gets wrong if it skips or double-counts a segment.
func TestArchiveGroupCountAcrossSegments(t *testing.T) {
	const n = 5000
	owners := []string{"alice", "bob", "carol", "dave"}
	ads := make([]string, n)
	for i := range ads {
		ads[i] = fmt.Sprintf("CompletionDate = %d\nOwner = %q\nPad = %q",
			1700000000+i, owners[i%len(owners)], strings.Repeat("x", 200))
	}
	hist := mkHist(t, ArchiveConfig{
		SegmentSize:      1 << 16, // force many segments
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate"},
	}, ads)

	if segs := hist.Stats().Segments; segs < 2 {
		t.Fatalf("expected multiple segments, got %d", segs)
	}
	want := groupCountByScan(t, hist, "Owner")
	got := aggGroupCounts(t, hist, "Owner")
	if len(got) != len(owners) {
		t.Fatalf("groups = %v, want %d", got, len(owners))
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("group %q = %d, want %d", k, got[k], w)
		}
	}
	var total int64
	for _, v := range got {
		total += v
	}
	if total != n {
		t.Errorf("counts sum to %d, want %d", total, n)
	}
}

// TestCategoricalGroupCountsDeclines pins the conditions under which the collections-level
// helper must refuse rather than answer, since every one of them would otherwise produce a
// silently short count.
func TestCategoricalGroupCountsDeclines(t *testing.T) {
	cases := []struct {
		name string
		cfg  ArchiveConfig
		ads  []string
	}{
		{"attribute not indexed", ArchiveConfig{ValueAttrs: []string{"ClusterId"}},
			[]string{`ClusterId = 1` + "\n" + `Owner = "alice"`}},
		{"a record lacks the attribute", ArchiveConfig{CategoricalAttrs: []string{"Owner"}},
			[]string{`Owner = "alice"`, `JobStatus = 4`}},
		{"a record holds a non-literal", ArchiveConfig{CategoricalAttrs: []string{"Owner"}},
			[]string{`Owner = "alice"`, `Owner = strcat("b", "ob")`}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hist := mkHist(t, c.cfg, c.ads)
			if _, ok := hist.CategoricalGroupCounts("Owner"); ok {
				t.Error("expected the index-resident path to decline")
			}
		})
	}
}

// TestCategoricalGroupCountsUsed is the converse: on the clean shape it must actually
// engage, otherwise the fast path is dead code and the benchmark measures nothing.
func TestCategoricalGroupCountsUsed(t *testing.T) {
	hist := mkHist(t, ArchiveConfig{CategoricalAttrs: []string{"Owner"}},
		[]string{`Owner = "alice"`, `Owner = "alice"`, `Owner = "bob"`})
	counts, ok := hist.CategoricalGroupCounts("Owner")
	if !ok {
		t.Fatal("index-resident path declined on a fully indexed archive")
	}
	if counts["alice"] != 2 || counts["bob"] != 1 {
		t.Errorf("counts = %v, want alice=2 bob=1", counts)
	}
	// Attribute names are case-insensitive, as everywhere else in ClassAds.
	if lower, ok := hist.CategoricalGroupCounts("owner"); !ok || lower["alice"] != 2 {
		t.Errorf("lower-cased attribute name = %v ok=%v, want the same counts", lower, ok)
	}
}

// TestArchiveGroupCountSurvivesReopen checks the fast path works off the persisted mmap
// sidecars, not just the in-RAM index a freshly written archive happens to hold.
func TestArchiveGroupCountSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	cat, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	hist, err := cat.CreateArchiveTable("history", ArchiveConfig{
		SegmentSize:      1 << 16,
		CategoricalAttrs: []string{"Owner"},
	})
	if err != nil {
		t.Fatal(err)
	}
	owners := []string{"alice", "bob", "carol"}
	for i := 0; i < 3000; i++ {
		if err := hist.AppendOld(fmt.Sprintf("Owner = %q\nPad = %q",
			owners[i%3], strings.Repeat("y", 200))); err != nil {
			t.Fatal(err)
		}
	}
	cat.Close()

	cat2, err := OpenCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer cat2.Close()
	h2, ok := cat2.ArchiveTable("history")
	if !ok {
		t.Fatal("archive not recovered")
	}
	counts, ok := h2.CategoricalGroupCounts("Owner")
	if !ok {
		t.Fatal("index-resident path declined after reopen (sidecar path broken)")
	}
	want := groupCountByScan(t, h2, "Owner")
	if len(counts) != len(want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
	for k, w := range want {
		if counts[k] != w {
			t.Errorf("group %q = %d, want %d", k, counts[k], w)
		}
	}
}

// TestArchiveGroupCountRowOrder documents the fast path's output ordering: sorted by group
// value. GROUP BY without ORDER BY has no defined order, but deterministic beats arbitrary.
func TestArchiveGroupCountRowOrder(t *testing.T) {
	hist := mkHist(t, ArchiveConfig{CategoricalAttrs: []string{"Owner"}},
		[]string{`Owner = "carol"`, `Owner = "alice"`, `Owner = "bob"`})
	rows, err := hist.Aggregate("true", []string{"Owner"}, []AggSpec{{Func: AggCount, Arg: "*"}})
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, len(rows))
	for i, r := range rows {
		got[i] = r.Group[0]
	}
	if !sort.StringsAreSorted(got) {
		t.Errorf("rows = %v, want sorted by group value", got)
	}
}

var _ = collections.Retention{} // keep the collections import meaningful if cases change

// TestArchiveGroupCountBucketedMatchesScan is the differential test for the bucketed form:
// the per-(day, owner) counts derived from zone maps plus per-segment indexes must equal
// what reading every record produces. Segments are small relative to a day so most are
// attributed wholesale and a few straddle a day boundary -- exercising both paths.
func TestArchiveGroupCountBucketedMatchesScan(t *testing.T) {
	const day = 86400
	owners := []string{"alice", "bob", "carol"}
	var ads []string
	base := int64(1700000000) / day * day // align so bucket boundaries are predictable
	for i := 0; i < 4000; i++ {
		// ~1000 records per day across 4 days.
		ts := base + int64(i)*(day/1000)
		ads = append(ads, fmt.Sprintf("CompletionDate = %d\nOwner = %q\nPad = %q",
			ts, owners[i%len(owners)], strings.Repeat("z", 150)))
	}
	hist := mkHist(t, ArchiveConfig{
		SegmentSize:      1 << 16,
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate"},
	}, ads)
	if segs := hist.Stats().Segments; segs < 4 {
		t.Fatalf("expected several segments, got %d", segs)
	}

	got, ok := hist.CategoricalGroupCountsBucketed("Owner", "CompletionDate", day)
	if !ok {
		t.Fatal("bucketed index-resident path declined on a fully indexed archive")
	}

	// Reference: read every ad and bucket it by hand.
	want := map[int64]map[string]int64{}
	seq, err := hist.Query("true")
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	for ad := range seq {
		o, ok := ad.EvaluateAttrString("Owner")
		if !ok {
			t.Fatal("missing Owner")
		}
		ts, okTS := ad.EvaluateAttrInt("CompletionDate")
		if !okTS {
			t.Fatal("missing CompletionDate")
		}
		b := ts / day * day
		if want[b] == nil {
			want[b] = map[string]int64{}
		}
		want[b][o]++
		n++
	}
	if n != int64(len(ads)) {
		t.Fatalf("scan saw %d ads, want %d", n, len(ads))
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %d, want %d (got %v)", len(got), len(want), got)
	}
	for b, wm := range want {
		gm := got[b]
		if len(gm) != len(wm) {
			t.Errorf("bucket %d groups = %v, want %v", b, gm, wm)
			continue
		}
		for o, w := range wm {
			if gm[o] != w {
				t.Errorf("bucket %d owner %q = %d, want %d", b, o, gm[o], w)
			}
		}
	}
}

// TestArchiveGroupCountBucketedDeclines pins the cases where bucketing cannot be done from
// metadata and must not be guessed at.
func TestArchiveGroupCountBucketedDeclines(t *testing.T) {
	ads := []string{`CompletionDate = 1700000000` + "\n" + `Owner = "alice"`}
	t.Run("bucket attribute has no zone map", func(t *testing.T) {
		hist := mkHist(t, ArchiveConfig{CategoricalAttrs: []string{"Owner"}}, ads)
		if _, ok := hist.CategoricalGroupCountsBucketed("Owner", "CompletionDate", 86400); ok {
			t.Error("expected decline without a zone map on the bucket attribute")
		}
	})
	t.Run("non-positive width", func(t *testing.T) {
		hist := mkHist(t, ArchiveConfig{
			CategoricalAttrs: []string{"Owner"}, ZoneAttrs: []string{"CompletionDate"},
		}, ads)
		if _, ok := hist.CategoricalGroupCountsBucketed("Owner", "CompletionDate", 0); ok {
			t.Error("expected decline for a zero bucket width")
		}
	})
	t.Run("a record lacks the bucket attribute", func(t *testing.T) {
		hist := mkHist(t, ArchiveConfig{
			CategoricalAttrs: []string{"Owner"}, ZoneAttrs: []string{"CompletionDate"},
		}, append(append([]string{}, ads...), `Owner = "bob"`))
		if _, ok := hist.CategoricalGroupCountsBucketed("Owner", "CompletionDate", 86400); ok {
			t.Error("expected decline when a record cannot be placed in a bucket")
		}
	})
}
