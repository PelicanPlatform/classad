package collections

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// Measuring the NDV estimate against ground truth on real pool ads.
//
// The corpus is the committed OSPool `condor_status -l` sample, so every attribute
// measured here carries its own empirical distribution rather than a uniform one made up
// to be easy: Name is near-unique (203 distinct in 205 ads), Arch has exactly one value
// across the whole pool, State and Activity have two with a 127/16 split, GLIDEIN_Site has
// 44 with a long tail under one dominant site, and Memory is a skewed numeric column.
//
// Scaling to a pool of thousands tiles the corpus. Tiling alone would hold every
// attribute's distinct count at the corpus's, which for Name would measure the fixture
// rather than the estimator, so ndvPoolAds gives each tiled slot its own Name built from a
// real one -- a pool of N slots has N slot names. That is the ONLY value it changes;
// everything else is the untouched empirical distribution, and every count below is
// measured, never assumed.

// ndvOracle counts by brute force, over every stored record, what the index would key for
// attr: the distinct values, the records that carry a keyable one, and the records that
// carry the attribute as something the index cannot key. It is the ground truth every
// estimate in this file is scored against, and it is deliberately the expensive way --
// a full scan and an unbounded set -- because that is what makes it an oracle.
func ndvOracle(tb testing.TB, c *Collection, attr string) (distinct, records, exceptions int) {
	tb.Helper()
	seen := map[string]struct{}{}
	for w := range c.ScanRawWire(nil, false) {
		wire.Ad(w).ForEachNamed(c.intern, func(name string, node []byte) bool {
			if !strings.EqualFold(name, attr) {
				return true
			}
			lit, ok := scalarValue(node)
			if !ok {
				exceptions++
				return false
			}
			switch lit.Kind {
			case wire.LitString:
				records++
				seen[strings.ToLower(lit.Str)] = struct{}{}
			case wire.LitInt, wire.LitReal, wire.LitBool:
				records++
				seen[numKey(lit)] = struct{}{}
			default:
				exceptions++
			}
			return false
		})
	}
	return len(seen), records, exceptions
}

// ndvFillPool writes n slot ads built by tiling the real corpus, giving each its own Name
// (see above). write takes one ad and stores it; the ad is reused and mutated between
// calls, so write must encode it before returning -- which Update and Append both do.
func ndvFillPool(tb testing.TB, n int, write func(*classad.ClassAd)) {
	tb.Helper()
	sample := loadCorpus(tb) // a fresh parse per call, so mutating these ads is local
	names := make([]string, len(sample))
	for i, ad := range sample {
		nm, _ := ad.EvaluateAttrString("Name")
		if nm == "" {
			nm = "slot1@host"
		}
		names[i] = nm
	}
	for i := 0; i < n; i++ {
		j := i % len(sample)
		ad := sample[j]
		ad.InsertAttrString("Name", names[j]+"--"+strconv.Itoa(i/len(sample)))
		write(ad)
	}
}

// ndvFillCollection is ndvFillPool into a mutable collection, one record per ad.
func ndvFillCollection(tb testing.TB, c *Collection, n int) {
	tb.Helper()
	i := 0
	ndvFillPool(tb, n, func(ad *classad.ClassAd) {
		if err := c.Update([]AdUpdate{{Key: []byte("slot-" + strconv.Itoa(i)), Ad: ad}}); err != nil {
			tb.Fatal(err)
		}
		i++
	})
}

// ndvMeasuredAttrs are the attributes measured, with the index kind each is configured
// under. They span the distribution shapes a join key actually takes.
var ndvMeasuredAttrs = []struct {
	attr  string
	cat   bool
	shape string
}{
	{"Name", true, "near-unique string key"},
	{"Machine", true, "mostly-unique string"},
	{"GLIDEIN_Site", true, "skewed, long tail"},
	{"OpSysAndVer", true, "low cardinality"},
	{"State", true, "two values, 8:1 split"},
	{"Arch", true, "one value across the pool"},
	{"Memory", false, "skewed numeric"},
	{"TotalSlots", false, "numeric, moderate"},
	{"Cpus", false, "numeric, 8 values"},
}

func ndvIndexOpts() (cat, val []string) {
	for _, a := range ndvMeasuredAttrs {
		if a.cat {
			cat = append(cat, a.attr)
		} else {
			val = append(val, a.attr)
		}
	}
	return cat, val
}

// relErrPct is the signed relative error of an estimate against the truth, in percent.
func relErrPct(est uint64, truth int) float64 {
	if truth == 0 {
		if est == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return 100 * (float64(est) - float64(truth)) / float64(truth)
}

// TestNDVAccuracyAgainstGroundTruth is the measurement: for each attribute and each store
// shape, the estimate against a brute-force count, with the stated accuracy class checked
// to hold. A class that does not hold is a failure -- an estimate a caller cannot trust the
// label of is worse than no estimate.
func TestNDVAccuracyAgainstGroundTruth(t *testing.T) {
	t.Parallel()
	cat, val := ndvIndexOpts()
	const n = 4000

	type shape struct {
		name  string
		build func(tb testing.TB) *Collection
	}
	shapes := []shape{
		{"one segment", func(tb testing.TB) *Collection {
			c := New(Options{Shards: 1, SegmentSize: 64 << 20, CategoricalAttrs: cat, ValueAttrs: val})
			ndvFillCollection(tb, c, n)
			c.Reindex()
			return c
		}},
		{"many segments", func(tb testing.TB) *Collection {
			c := New(Options{Shards: 1, SegmentSize: 256 << 10, CategoricalAttrs: cat, ValueAttrs: val})
			ndvFillCollection(tb, c, n)
			c.Reindex()
			return c
		}},
		{"index behind the writes", func(tb testing.TB) *Collection {
			// Half the records written after the last index build: the index has not seen
			// them, so whatever it says is a floor.
			c := New(Options{Shards: 1, SegmentSize: 256 << 10, CategoricalAttrs: cat, ValueAttrs: val})
			ndvFillCollection(tb, c, n/2)
			c.Reindex()
			ndvFillPool(tb, n, func(ad *classad.ClassAd) {
				nm, _ := ad.EvaluateAttrString("Name")
				if err := c.Update([]AdUpdate{{Key: []byte("late-" + nm), Ad: ad}}); err != nil {
					tb.Fatal(err)
				}
			})
			return c
		}},
		{"overwritten in place", func(tb testing.TB) *Collection {
			// Every key rewritten with a different slot name, so the arena holds a
			// superseded version per record and the postings still carry its value.
			c := New(Options{Shards: 1, SegmentSize: 256 << 10, CategoricalAttrs: cat, ValueAttrs: val})
			ndvFillCollection(tb, c, n)
			i := 0
			ndvFillPool(tb, n, func(ad *classad.ClassAd) {
				nm, _ := ad.EvaluateAttrString("Name")
				ad.InsertAttrString("Name", "rewritten-"+nm)
				if err := c.Update([]AdUpdate{{Key: []byte("slot-" + strconv.Itoa(i)), Ad: ad}}); err != nil {
					tb.Fatal(err)
				}
				i++
			})
			c.Reindex()
			return c
		}},
		{"sealed archive", func(tb testing.TB) *Collection {
			dir := tb.TempDir()
			a, err := CreateArchive(ArchiveOptions{
				Dir: dir, SegmentSize: 256 << 10,
				CategoricalAttrs: cat, ValueAttrs: val,
			})
			if err != nil {
				tb.Fatal(err)
			}
			tb.Cleanup(func() { _ = a.Close() })
			ndvFillPool(tb, n, func(ad *classad.ClassAd) {
				if err := a.Append(ad); err != nil {
					tb.Fatal(err)
				}
			})
			if err := a.Flush(); err != nil {
				tb.Fatal(err)
			}
			a.Reindex()
			return a.c
		}},
	}

	var worst float64
	var worstWhere string
	for _, sh := range shapes {
		c := sh.build(t)
		t.Logf("--- %s: %d records, %d segments ---", sh.name, c.StoredLen(), c.Stats().Segments)
		for _, a := range ndvMeasuredAttrs {
			truth, records, exc := ndvOracle(t, c, a.attr)
			est, ok := c.AttrNDV(a.attr)
			if !ok {
				t.Errorf("%s/%s: no estimate for a configured index", sh.name, a.attr)
				continue
			}
			e := relErrPct(est.Distinct, truth)
			t.Logf("%-14s truth=%-6d est=%-6d err=%+7.2f%%  %-8s %-6s segs=%d/%d recs=%d exc=%d  (%s)",
				a.attr, truth, est.Distinct, e, est.Accuracy, est.Err,
				est.Segments, est.Segments+est.Uncovered, records, exc, a.shape)

			// The class must hold. This is the assertion the whole design rests on.
			switch est.Err {
			case NDVErrNone:
				if int(est.Distinct) != truth {
					t.Errorf("%s/%s: claims exact but %d != %d", sh.name, a.attr, est.Distinct, truth)
				}
			case NDVErrLow:
				if int(est.Distinct) > truth {
					t.Errorf("%s/%s: claims a lower bound but %d > %d", sh.name, a.attr, est.Distinct, truth)
				}
			case NDVErrHigh:
				if int(est.Distinct) < truth {
					t.Errorf("%s/%s: claims an upper bound but %d < %d", sh.name, a.attr, est.Distinct, truth)
				}
			}
			if est.Accuracy != accuracyFor(est.Err) {
				t.Errorf("%s/%s: accuracy %q does not follow from err %q", sh.name, a.attr, est.Accuracy, est.Err)
			}
			if math.Abs(e) > worst {
				worst, worstWhere = math.Abs(e), fmt.Sprintf("%s/%s (truth %d, est %d)", sh.name, a.attr, truth, est.Distinct)
			}
		}
	}
	t.Logf("worst relative error over all shapes and attributes: %.2f%% at %s", worst, worstWhere)
}

// TestNDVSampledProfileError measures what the sampled attribute profile behind
// SuggestIndexes would give if it were used as NDV, against the same ground truth. The
// point is the DIRECTION and the SIZE of its error on a high-cardinality key, since NDV
// divides a join-cardinality estimate: a count that is low by a factor multiplies the
// estimated output by that factor.
func TestNDVSampledProfileError(t *testing.T) {
	t.Parallel()
	cat, val := ndvIndexOpts()
	const n = 20000 // past maxDistinctSample, which is where the cap bites
	c := New(Options{Shards: 1, SegmentSize: 1 << 20, CategoricalAttrs: cat, ValueAttrs: val})
	ndvFillCollection(t, c, n)
	c.Reindex()

	profiles := c.profileAttrs(maxDistinctSample)
	for _, a := range ndvMeasuredAttrs {
		truth, _, _ := ndvOracle(t, c, a.attr)
		p := profiles[strings.ToLower(a.attr)]
		sampled, capped, present := 0, false, 0
		if p != nil {
			sampled, capped, present = len(p.distinct), p.capped, p.present
		}
		est, _ := c.AttrNDV(a.attr)
		t.Logf("%-14s truth=%-6d sampled=%-6d capped=%-5v sample_present=%-5d err=%+8.2f%%   index=%-6d err=%+7.2f%%",
			a.attr, truth, sampled, capped, present, relErrPct(uint64(sampled), truth),
			est.Distinct, relErrPct(est.Distinct, truth))
		if capped && sampled > truth {
			t.Errorf("%s: a capped sample is a lower bound but %d > %d", a.attr, sampled, truth)
		}
	}

	// What the Capped flag can and cannot catch. It is raised by the DISTINCT-SET
	// ceiling, which is reached only by taking more samples than the ceiling allows
	// distinct values -- so at the sample size the auto-tuner uses it cannot be raised at
	// all, and the badly short count above goes out unflagged. Raising the sample size is
	// what makes it fire. The error it misses is the error on exactly the high-cardinality
	// keys a join is made on, which is why the flag is necessary and not sufficient.
	truth, _, _ := ndvOracle(t, c, "Name")
	for _, sampleMax := range []int{maxDistinctSample, 4 * maxDistinctSample, 16 * maxDistinctSample} {
		p := c.profileAttrs(sampleMax)[strings.ToLower("Name")]
		if p == nil {
			t.Fatal("Name is in every sampled ad")
		}
		t.Logf("Name sampleMax=%-6d sampled_ads=%-6d distinct=%-6d capped=%-5v err=%+8.2f%%",
			sampleMax, p.present, len(p.distinct), p.capped, relErrPct(uint64(len(p.distinct)), truth))
	}
}

// TestNDVHistogramCannotYieldNDV measures the best an equi-depth histogram can do. Its
// buckets hold roughly equal counts and record only a top value and a cumulative count, so
// the only distinct-value fact recoverable from one is that there are at least as many
// distinct values as buckets -- which saturates at histMaxBuckets however large the true
// count is. The numbers below are what that bound is worth on real data.
func TestNDVHistogramCannotYieldNDV(t *testing.T) {
	t.Parallel()
	_, val := ndvIndexOpts()
	const n = 20000
	c := New(Options{Shards: 1, SegmentSize: 64 << 20, ValueAttrs: append(val, "DaemonStartTime")})
	ndvFillCollection(t, c, n)
	c.Reindex()

	spec := c.spec.Load()
	for _, attr := range append(val, "DaemonStartTime") {
		id, ok := c.intern.LookupID(strings.ToLower(attr))
		if !ok {
			continue
		}
		if _, isVal := spec.val[id]; !isVal {
			continue
		}
		truth, _, _ := ndvOracle(t, c, attr)
		buckets := 0
		for _, sh := range c.shards {
			sh.mu.RLock()
			for _, seg := range sh.segs {
				if seg == nil {
					continue
				}
				si := seg.idx.Load()
				if si == nil {
					continue
				}
				if vp := si.val[id]; vp != nil && vp.stats.hist != nil {
					buckets = max(buckets, len(vp.stats.hist.bound))
				}
			}
			sh.mu.RUnlock()
		}
		est, _ := c.AttrNDV(attr)
		t.Logf("%-18s truth=%-6d histogram buckets=%-4d (a lower bound, capped at %d; err %+.2f%%)   index=%-6d err=%+.2f%%",
			attr, truth, buckets, histMaxBuckets, relErrPct(uint64(buckets), truth),
			est.Distinct, relErrPct(est.Distinct, truth))
		if buckets > truth {
			t.Errorf("%s: bucket count %d exceeds the distinct count %d, so it is not even a lower bound", attr, buckets, truth)
		}
	}
	// A categorical attribute -- the common join key -- has no histogram at all.
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if si := seg.idx.Load(); si != nil {
				for id, cp := range si.cat {
					if cp.stats.hist != nil {
						t.Errorf("categorical attr id %d has a histogram; the measurement above assumes none", id)
					}
				}
			}
		}
		sh.mu.RUnlock()
	}
}

// TestNDVPlanTimeCost measures what an estimate costs to read, against the EXPLAIN a join
// already pays per side when planning one. The comparison is the point: the budget for a
// per-query statistic is what the planner already spends, not zero. Cold and warm are
// reported apart because they are different costs -- cold merges a sketch per segment,
// warm compares one pointer per segment -- and a planner pays warm on all but the first
// query after a segment's index changes.
func TestNDVPlanTimeCost(t *testing.T) {
	t.Parallel()
	cat, val := ndvIndexOpts()
	for _, n := range []int{4000, 40000} {
		c := New(Options{Shards: 1, SegmentSize: 256 << 10, CategoricalAttrs: cat, ValueAttrs: val})
		ndvFillCollection(t, c, n)
		c.Reindex()
		segs := c.Stats().Segments
		q, err := vm.Parse(`Memory > 1024`)
		if err != nil {
			t.Fatal(err)
		}
		// Cold: one call per attribute, each the first for its own id, so none is served
		// from the memo.
		var coldTotal time.Duration
		coldN := 0
		for _, a := range ndvMeasuredAttrs {
			t0 := time.Now()
			if _, ok := c.AttrNDV(a.attr); !ok {
				t.Fatalf("no estimate for %s", a.attr)
			}
			coldTotal += time.Since(t0)
			coldN++
		}
		const iters = 200
		t0 := time.Now()
		for i := 0; i < iters; i++ {
			if _, ok := c.AttrNDV("Name"); !ok {
				t.Fatal("no estimate")
			}
		}
		warm := time.Since(t0).Nanoseconds() / iters
		t0 = time.Now()
		for i := 0; i < iters; i++ {
			c.ExplainQuery(q)
		}
		explain := time.Since(t0).Nanoseconds() / iters
		cold := coldTotal.Nanoseconds() / int64(coldN)
		t.Logf("%6d records in %4d segments: AttrNDV cold %8dns (%5.0fns/segment), warm %7dns (%4.1fns/segment); ExplainQuery %8dns",
			n, segs, cold, float64(cold)/float64(segs), warm, float64(warm)/float64(segs), explain)
	}
}

// BenchmarkAttrNDV prices the estimate per segment count, so the O(segments) term is
// visible rather than inferred.
func BenchmarkAttrNDV(b *testing.B) {
	cat, val := ndvIndexOpts()
	for _, segSize := range []int{64 << 20, 1 << 20, 256 << 10} {
		c := New(Options{Shards: 1, SegmentSize: segSize, CategoricalAttrs: cat, ValueAttrs: val})
		ndvFillCollection(b, c, 20000)
		c.Reindex()
		b.Run(fmt.Sprintf("segments=%d", c.Stats().Segments), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, ok := c.AttrNDV("Name"); !ok {
					b.Fatal("no estimate")
				}
			}
		})
	}
}
