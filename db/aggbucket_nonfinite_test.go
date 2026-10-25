package db

import (
	"fmt"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// A bucketed GROUP BY -- time_bucket(X, width) -- over a value that is a number but not a finite
// one.
//
// The bucket key is floor(v/width)*width rendered as an int64, and int64() of a non-finite float
// is implementation-defined in Go. Measured on darwin/arm64 with width 86400: NaN -> 0, +Inf ->
// -86400, -Inf -> 0. So every NaN row landed in bucket 0 beside the genuine epoch-0 rows, and
// every +Inf row in a bucket that starts before the epoch. The guard was numberOf/
// EvaluateAttrNumber, which accepts both because they ARE reals.
//
// No differential test could find this: the record scan derives its key with the same arithmetic,
// so the columnar path and the scan agreed with each other on nonsense. These tests assert the
// group set against what the values MEAN -- a bucket is the half-open interval [b, b+width), and
// a NaN or an infinity is in none of them, so such a row is in no group at all, exactly as a
// string or an absent attribute already is.
//
// Fixture note: old-ClassAd text cannot write a NaN (`nan` lexes as a bare attribute reference and
// evaluates to UNDEFINED), so stored NaNs are built with InsertAttrFloat. NaN by arithmetic
// (0.0/0.0) and +Inf as a literal (1.0e999) are writable, and take a different storage path -- an
// expression node rather than a literal -- so both forms are covered.

// TestBucketKeyTextDropsNonFinite pins the key derivation itself, the one place the mutable
// aggregate, the archive aggregate and the view engine all floor through. A refactor that puts the
// bare cast back fails here rather than in whichever aggregate is next asked for a NaN.
func TestBucketKeyTextDropsNonFinite(t *testing.T) {
	const day = 86400
	for _, tc := range []struct {
		v    classad.Value
		want string
	}{
		{classad.NewIntValue(0), "0"},
		{classad.NewIntValue(day - 1), "0"},
		{classad.NewIntValue(day), "86400"},
		{classad.NewIntValue(1700000000), "1699920000"},
		{classad.NewRealValue(1700000000.75), "1699920000"}, // a real and an int floor alike
		{classad.NewIntValue(-1), "-86400"},                 // floor, not truncation
	} {
		got, ok := bucketKeyText(tc.v, day)
		if !ok || got != tc.want {
			t.Errorf("bucketKeyText(%v) = %q, %v; want %q, true", ValueText(tc.v), got, ok, tc.want)
		}
	}
	for _, f := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if got, ok := bucketKeyText(classad.NewRealValue(f), day); ok {
			t.Errorf("bucketKeyText(%v) = %q, true; want no bucket", f, got)
		}
		if got, ok := bucketFloorInt(f, day); ok {
			t.Errorf("bucketFloorInt(%v) = %d, true; want no bucket", f, got)
		}
	}
	// The finite neighbours of those values are untouched: same bucket, same floor.
	for _, tc := range []struct {
		v    float64
		want int64
	}{{0, 0}, {day, day}, {-1, -day}, {1700000000, 1699920000}} {
		if got, ok := bucketFloorInt(tc.v, day); !ok || got != tc.want {
			t.Errorf("bucketFloorInt(%v) = %d, %v; want %d, true", tc.v, got, ok, tc.want)
		}
	}
}

const bucketDay = 86400

// nonFiniteStampDB is a mutable table whose Stamp column is a real holding ordinary unix seconds
// across three days plus NaN, +Inf and -Inf. want is the bucket -> count the VALUES mean: one
// group per day present in the data, with the non-finite rows in none of them.
func nonFiniteStampDB(t *testing.T, n int) (*DB, map[string]int) {
	t.Helper()
	d, err := OpenConfig(Config{SegmentSize: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	base := int64(1700000000) / bucketDay * bucketDay
	want := map[string]int{}
	tx := d.Begin()
	for i := 0; i < n; i++ {
		ad := classad.New()
		ad.InsertAttr("ProcId", int64(i%7))
		var stamp float64
		switch i % 50 {
		case 0:
			stamp = math.NaN()
		case 1:
			stamp = math.Inf(1)
		case 2:
			stamp = math.Inf(-1)
		default:
			stamp = float64(base + int64(i%3)*bucketDay + int64(i%bucketDay))
			want[strconv.FormatInt(int64(stamp)/bucketDay*bucketDay, 10)]++
		}
		ad.InsertAttrFloat("Stamp", stamp)
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !d.EnableSchemaScan(4000, 8) {
		t.Skip("no sealed segments to accelerate")
	}
	return d, want
}

// checkBucketCounts asserts a bucketed result against the counts the values mean, and names the two
// bucket keys the unguarded cast produces so a failure says which non-finite value leaked.
func checkBucketCounts(t *testing.T, what string, rows []AggRow, want map[string]int) {
	t.Helper()
	got := bucketRows(t, rows, 1)
	for k, v := range got {
		w, ok := want[k]
		if !ok {
			extra := ""
			switch k {
			case "0":
				extra = " (int64(NaN/width)*width == 0 on this architecture)"
			case "-86400":
				extra = " (int64(+Inf/width)*width == -86400 on this architecture)"
			}
			t.Errorf("%s: bucket %q = %v, which no value belongs in%s", what, k, v, extra)
			continue
		}
		if v[0] != strconv.Itoa(w) {
			t.Errorf("%s: bucket %q count = %s, want %d", what, k, v[0], w)
		}
	}
	for k, w := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("%s: bucket %q missing, want count %d", what, k, w)
		}
	}
}

// TestBucketedAggregateExcludesNonFinite is the semantic test over a stored literal NaN and literal
// infinities: both the columnar group path and the record scan must report exactly the finite rows'
// buckets, and therefore must also agree with each other.
func TestBucketedAggregateExcludesNonFinite(t *testing.T) {
	d, want := nonFiniteStampDB(t, 4000)
	groupCols := []GroupCol{{Attr: "Stamp", BucketWidth: bucketDay}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}

	got, err := d.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBucketCounts(t, "columnar", got, want)
	checkBucketCounts(t, "scan", scanGroupedMutable(t, d, "true", groupCols, aggs), want)
}

// TestBucketedAggregateExcludesNonFiniteExpressions covers the other two ways a non-finite value
// reaches the aggregate: NaN produced by arithmetic (0.0/0.0, stored as an expression, so the
// columnar path hands the query back and the scan answers) and +Inf written as a literal
// (1.0e999). The expected group set is the three ads that name a real instant.
func TestBucketedAggregateExcludesNonFiniteExpressions(t *testing.T) {
	d, err := OpenConfig(Config{})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	base := int64(1700000000) / bucketDay * bucketDay
	tx := d.Begin()
	for i, src := range []string{
		fmt.Sprintf("Stamp = %d", base),
		fmt.Sprintf("Stamp = %d", base+10),
		fmt.Sprintf("Stamp = %d", base+bucketDay),
		"Stamp = 0.0/0.0",       // NaN by arithmetic
		"Stamp = 0.0/0.0 + 0.0", // still NaN after further arithmetic
		"Stamp = 1.0e999",       // +Inf
		"Stamp = -1.0e999",      // -Inf
		"Stamp = \"yesterday\"", // the control: a non-number already dropped
	} {
		ad, err := classad.ParseOld(src)
		if err != nil {
			t.Fatal(err)
		}
		tx.NewClassAd(fmt.Sprintf("k%d", i), ad)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{
		strconv.FormatInt(base, 10):           2,
		strconv.FormatInt(base+bucketDay, 10): 1,
	}
	groupCols := []GroupCol{{Attr: "Stamp", BucketWidth: bucketDay}}
	aggs := []AggSpec{{Func: AggCount, Arg: "*"}}
	rows, err := d.AggregateColsStats("true", groupCols, aggs, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	checkBucketCounts(t, "aggregate", rows, want)
	checkBucketCounts(t, "scan", scanGroupedMutable(t, d, "true", groupCols, aggs), want)
}

// TestViewTimeBucketDropsNonFinite is the same rule in the continuous-aggregate engine, which
// floors a base ad's bucket attribute into a series label. A NaN there would have opened a series
// labelled 0 -- a legitimate-looking epoch-0 series -- that no further data can ever join.
func TestViewTimeBucketDropsNonFinite(t *testing.T) {
	cat, err := OpenCatalog(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer cat.Close()
	base, err := cat.CreateTable("jobs")
	if err != nil {
		t.Fatal(err)
	}
	stamped := func(owner string, qdate float64) *classad.ClassAd {
		ad := classad.New()
		ad.InsertAttrString("Owner", owner)
		ad.InsertAttr("RequestMemory", int64(100))
		ad.InsertAttrFloat("QDate", qdate)
		return ad
	}
	base.Put("1", viewJobTS(t, "alice", 100, 3600))
	base.Put("2", viewJobTS(t, "alice", 300, 3700)) // same series as "1"
	base.Put("3", stamped("bob", math.NaN()))
	base.Put("4", stamped("bob", math.Inf(1)))
	base.Put("5", stamped("bob", math.Inf(-1)))
	if err := cat.CreateView("jobs_ts", timeBucketSpec()); err != nil {
		t.Fatalf("CreateView: %v", err)
	}
	v, ok := cat.View("jobs_ts")
	if !ok {
		t.Fatal("view jobs_ts missing")
	}
	if got := v.SeriesCount(); got != 1 {
		t.Fatalf("series = %d, want 1 (only the finite bucket)", got)
	}
	for _, key := range []string{"0\x00bob", "-3600\x00bob"} {
		if _, found := viewGroup(t, cat, "jobs_ts", key); found {
			t.Errorf("series %q exists: a non-finite QDate was filed into a bucket", key)
		}
	}
	// Live maintenance takes the same path as the initial build, so a non-finite ad arriving
	// afterwards must not open a series either.
	base.Put("6", stamped("carol", math.NaN()))
	time.Sleep(100 * time.Millisecond)
	if got := v.SeriesCount(); got != 1 {
		t.Errorf("series = %d after a live NaN, want 1", got)
	}
}

// TestCategoricalGroupCountsBucketedNonFinite covers the archive's index-resident bucketed counts,
// which place whole segments from zone maps without reading a record. A NaN clears the zone's Full
// flag so that path is already refused for it, but an infinity is an ordinary zone edge: a segment
// whose range ended at +Inf satisfied "min and max are in the same bucket" and attributed every
// record in it to bucket -86400.
func TestCategoricalGroupCountsBucketedNonFinite(t *testing.T) {
	base := int64(1700000000) / bucketDay * bucketDay
	var ads []string
	want := map[int64]map[string]int64{}
	for i := 0; i < 600; i++ {
		owner := fmt.Sprintf("user%d", i%3)
		if i%100 == 7 {
			ads = append(ads, fmt.Sprintf("CompletionDate = 1.0e999\nOwner = %q", owner))
			continue
		}
		ts := base + int64(i)*600
		ads = append(ads, fmt.Sprintf("CompletionDate = %d\nOwner = %q", ts, owner))
		b := ts / bucketDay * bucketDay
		if want[b] == nil {
			want[b] = map[string]int64{}
		}
		want[b][owner]++
	}
	hist := mkHist(t, ArchiveConfig{
		SegmentSize:      1 << 14,
		CategoricalAttrs: []string{"Owner"},
		ZoneAttrs:        []string{"CompletionDate"},
	}, ads)
	got, ok := hist.CategoricalGroupCountsBucketed("Owner", "CompletionDate", bucketDay)
	if !ok {
		t.Skip("index-resident bucketed path declined; nothing to compare")
	}
	if len(got) != len(want) {
		t.Fatalf("buckets = %v, want %v", bucketSetOf(got), bucketSetOf(want))
	}
	for b, wm := range want {
		for o, w := range wm {
			if got[b][o] != w {
				t.Errorf("bucket %d owner %q = %d, want %d", b, o, got[b][o], w)
			}
		}
	}
	if _, bogus := got[-bucketDay]; bogus {
		t.Errorf("bucket %d exists: int64(+Inf/width)*width, not a timestamp", -bucketDay)
	}
}

func bucketSetOf(m map[int64]map[string]int64) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
