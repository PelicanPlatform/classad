package collections

import (
	"testing"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// BenchmarkGroupMemoShapes runs the tier-1 grouped aggregate over ONE corpus with group columns of
// different RUN LENGTHS -- how many consecutive records share a group.
//
// Run length is the whole question for a memo in front of the accumulation maps. Records arrive in
// insertion order, so a column that advances slowly (a completion time floored to the hour, a cluster
// id shared by its procs) repeats the previous group over and over, while one that cycles (a proc id,
// an owner drawn round-robin) never does. The arms below span both, so a win on the first is reported
// beside what it costs the second rather than on its own.
//
// Every arm reads the same shared fixture and the same constraint: only the group column differs, so a
// difference between two arms is the column's shape and not a different corpus.
func BenchmarkGroupMemoShapes(b *testing.B) {
	c := narrowFixture(b)
	q, err := vm.Parse(narrowQuery)
	if err != nil {
		b.Fatal(err)
	}
	const hour = 3600
	const day = 86400
	// 120k records of a one-second-per-record timestamp cut into roughly the 76 buckets a 90-day
	// daily series has.
	const prodBucket = 1580
	for _, arm := range []struct {
		name   string
		attr   string
		bucket int64
		why    string
	}{
		// Time-ordered, and the shape production asks for: an archive read in insertion order,
		// grouped into time buckets. QDate advances one second per record here, so the bucket
		// WIDTH sets how many consecutive records share a group.
		//
		// A dashboard's default is a 90-day window bucketed by day, which is a few dozen buckets
		// over whatever the window holds -- one measured access point answered exactly that in
		// 550ms for 76 buckets. What decides the memo's hit rate is that ratio, not the width in
		// seconds, so prodBucket is chosen to cut this corpus into the same ~76 groups; the hour
		// arm is the same shape half as coarse.
		{"ordered/qdate_76buckets", "QDate", prodBucket, "run length ~1580, ~76 groups"},
		{"ordered/qdate_hour", "QDate", hour, "run length ~3600, ~34 groups"},
		// Short runs, high cardinality: a cluster id is shared by its four procs, and there are
		// tens of thousands of them. Three records in four repeat the previous group while the map
		// itself grows past any cache.
		{"ordered/clusterid", "ClusterId", 0, "run length 4, ~30k groups"},
		// The adversarial shape for a bucket interval: the width is as small as the step between
		// consecutive values, so every record crosses a boundary and the interval remembered for
		// the last one is never the right one. It also makes a group per record.
		{"adversarial/qdate_sec", "QDate", 1, "crosses a bucket boundary every record"},
		// Scattered: the day bucket of this fixture's EnteredHistoryTime cycles every 30 records,
		// so consecutive records are in DIFFERENT buckets despite the column being a timestamp.
		{"scattered/entered_day", "EnteredHistoryTime", day, "run length 1, ~30 groups"},
		// Scattered and tiny: proc id cycles 0..3, so the group changes on every record and the
		// map it replaces is small enough to sit in L1. The least a memo can be worth.
		{"scattered/procid", "ProcId", 0, "run length 1, 4 groups"},
		// Scattered strings: owner cycles through 256 values, read through the block dictionary.
		{"scattered/owner", "Owner", 0, "run length 1, 256 string groups"},
		// Every record its own group: a globally unique id, so the memo can never hit and every
		// record pays an insert. The worst case there is.
		{"scattered/globaljobid", "GlobalJobId", 0, "run length 1, one group per record"},
	} {
		if _, ok := c.GroupStatsQueryBucketed(q, arm.attr, arm.bucket, nil); !ok {
			b.Fatalf("%s (%s): declined", arm.name, arm.why)
		}
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				gs, ok := c.GroupStatsQueryBucketed(q, arm.attr, arm.bucket, nil)
				if !ok || len(gs) == 0 {
					b.Fatal("declined or empty")
				}
			}
		})
	}
}

// groupCompSeq builds one shape of group-component sequence for BenchmarkGroupAccum.
//
// n is deliberately the same six figures the end-to-end fixture holds, so the accumulation maps reach
// the same size and the measurement is not one of a map that fits in L1.
//
// A string shape carries the query-local ID a string component holds in the packed key, not the
// string: that is what the accumulator sees, because the column reader resolves a dictionary code to
// an id once per distinct value per block and the per-record step is the id. So the string arms here
// measure the accumulation of a string-KINDED component, which is what they always measured; the
// resolution itself is in the end-to-end arms.
func groupCompSeq(shape string, n int) []groupComp {
	const base = 1700000000
	keys := make([]groupComp, n)
	switch shape {
	case "bucket_76groups", "bucket_run3600", "bucket_run1":
		// A per-second timestamp read in order: which bucket WIDTH is applied decides the run.
		for i := range keys {
			keys[i] = groupComp{kind: gkInt, raw: base + int64(i)}
		}
	case "bucket_scattered":
		// The group advances by a whole day every record and wraps after a month: a timestamp
		// column whose buckets still change on every single record.
		for i := range keys {
			keys[i] = groupComp{kind: gkInt, raw: base + int64(i%30)*86400 + int64(i)}
		}
	case "raw_run4":
		for i := range keys {
			keys[i] = groupComp{kind: gkInt, raw: int64(i / 4)}
		}
	case "raw_cycle4":
		for i := range keys {
			keys[i] = groupComp{kind: gkInt, raw: int64(i % 4)}
		}
	case "raw_str_cycle256":
		for i := range keys {
			keys[i] = groupComp{kind: gkString, raw: int64(i % 256)}
		}
	case "raw_str_unique":
		for i := range keys {
			keys[i] = groupComp{kind: gkString, raw: int64(i)}
		}
	default:
		panic("unknown shape " + shape)
	}
	return keys
}

// BenchmarkGroupAccum measures the accumulation step ALONE: a prepared key sequence folded into a
// fresh groupAccum, with no column reads, no decompression and no visibility walk.
//
// The end-to-end benchmarks are the number that matters, but on this machine their run-to-run spread
// is wider than the effect being measured, so they cannot settle a few percent either way. This one
// has no I/O and no shared cache in it, so the same change shows up against a much smaller noise
// floor; read it as the size of the step that was changed, and the end-to-end numbers as how much of
// a whole query that step is.
func BenchmarkGroupAccum(b *testing.B) {
	const n = 120_000
	for _, arm := range []struct {
		shape  string
		bucket int64
	}{
		{"bucket_76groups", 1580},
		{"bucket_run3600", 3600},
		{"bucket_scattered", 86400},
		{"bucket_run1", 1},
		{"raw_run4", 0},
		{"raw_cycle4", 0},
		{"raw_str_cycle256", 0},
		{"raw_str_unique", 0},
	} {
		keys := groupCompSeq(arm.shape, n)
		b.Run(arm.shape, func(b *testing.B) {
			b.ReportAllocs()
			var comps [maxGroupCols]groupComp
			for i := 0; i < b.N; i++ {
				a := newGroupAccum([]int64{arm.bucket})
				for j := range keys {
					comps[0] = keys[j]
					a.accFor(&comps, 0)
				}
				if a.nGroups() == 0 {
					b.Fatal("no groups")
				}
			}
		})
	}
}

// BenchmarkGroupAccumTwoCols is BenchmarkGroupAccum for the two-column shape: a raw categorical
// component beside a bucketed time component, which is "per owner per day". The arms differ in how
// the two advance against each other, because that is what the interval memo can and cannot help
// with: a memo hit needs the categorical component to be UNCHANGED as well as the timestamp to be in
// the same bucket, so a column that cycles every record defeats it however slow the bucket is.
func BenchmarkGroupAccumTwoCols(b *testing.B) {
	const n = 120_000
	const day = 86400
	for _, arm := range []struct {
		name   string
		owner  func(i int) int64
		bucket int64
		why    string
	}{
		// Submit bursts: one owner for a long run, timestamps advancing a second at a time. Both
		// components hold still, which is the memo's best case and the common production shape.
		{"runs/owner256_day", func(i int) int64 { return int64(i / 256) }, day, "owner runs of 256, ~1.4 days"},
		// The same timestamps with an owner drawn round-robin: the bucket holds, the owner does
		// not, so the memo misses on the key compare every record.
		{"cycle/owner16_day", func(i int) int64 { return int64(i % 16) }, day, "owner cycles every record"},
		// Both components change every record: a group per record and no memo hit at all.
		{"cycle/owner16_sec", func(i int) int64 { return int64(i % 16) }, 1, "a group per record"},
	} {
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			const base = 1700000000
			var comps [maxGroupCols]groupComp
			for i := 0; i < b.N; i++ {
				a := newGroupAccum([]int64{0, arm.bucket})
				for j := 0; j < n; j++ {
					comps[0] = groupComp{kind: gkString, raw: arm.owner(j)}
					comps[1] = groupComp{kind: gkInt, raw: base + int64(j)}
					a.accFor(&comps, 0)
				}
				if a.nGroups() == 0 {
					b.Fatalf("no groups (%s)", arm.why)
				}
			}
		})
	}
}

// BenchmarkGroupThreeCols is BenchmarkGroupAccumTwoCols at the top arity: two raw categorical
// components beside one bucketed time component, which is "per owner per site per day".
//
// There is nothing to compare it against on the two-slot key -- three group columns declined there
// and the query scanned -- so what it measures is the arity's own cost, beside the two-column arms
// above. The same three shapes, because the memo's hit rate is what separates them: a hit needs
// EVERY unbucketed component unchanged as well as the timestamp still in its bucket, so a third
// categorical column is a third way to miss.
func BenchmarkGroupThreeCols(b *testing.B) {
	const n = 120_000
	const day = 86400
	for _, arm := range []struct {
		name   string
		owner  func(i int) int64
		site   func(i int) int64
		bucket int64
		why    string
	}{
		{"runs/owner256_site8_day", func(i int) int64 { return int64(i / 256) },
			func(i int) int64 { return int64(i / 1024) }, day, "both categoricals run"},
		{"cycle/owner16_site4_day", func(i int) int64 { return int64(i % 16) },
			func(i int) int64 { return int64(i % 4) }, day, "the categoricals cycle every record"},
		{"cycle/owner16_site4_sec", func(i int) int64 { return int64(i % 16) },
			func(i int) int64 { return int64(i % 4) }, 1, "a group per record"},
	} {
		b.Run(arm.name, func(b *testing.B) {
			b.ReportAllocs()
			const base = 1700000000
			var comps [maxGroupCols]groupComp
			for i := 0; i < b.N; i++ {
				a := newGroupAccum([]int64{0, 0, arm.bucket})
				for j := 0; j < n; j++ {
					comps[0] = groupComp{kind: gkString, raw: arm.owner(j)}
					comps[1] = groupComp{kind: gkString, raw: arm.site(j)}
					comps[2] = groupComp{kind: gkInt, raw: base + int64(j)}
					a.accFor(&comps, 0)
				}
				if a.nGroups() == 0 {
					b.Fatalf("no groups (%s)", arm.why)
				}
			}
		})
	}
}
