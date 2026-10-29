package collections

import (
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/collections/vm"
)

// TestBaseSchemaConvergenceMeasure measures what the maintenance re-derive costs and buys on an
// epoch-history-shaped archive: ~150 attributes per ad, one in ten carrying VacateReasonCode, a
// couple hundred sealed segments, all columnarized under a schema that leaves VacateReasonCode out.
//
// Reported: a steady maintenance pass (nothing material changed), each converging pass after the
// attribute is required, and the production query before and after convergence -- two copies of
// the same archive, one left on the old schema, timed interleaved with the order alternated each
// round, medians reported.
//
// Measurement, not a check: run with CLASSAD_BENCH_SCHEMA=1.
func TestBaseSchemaConvergenceMeasure(t *testing.T) {
	if os.Getenv("CLASSAD_BENCH_SCHEMA") == "" {
		t.Skip("set CLASSAD_BENCH_SCHEMA=1 to measure")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 60000
	dir := t.TempDir()
	opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 19, ZoneAttrs: []string{"EpochWriteDate"}, GroupSchemaCount: -1}
	a, err := CreateArchive(opts)
	if err != nil {
		t.Fatal(err)
	}
	for i := range n {
		var b strings.Builder
		fmt.Fprintf(&b, `[ EpochWriteDate=%d; ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; `,
			reviewEpoch0+i*10, i/10, i%10, i%97, i%7)
		for j := range 100 {
			fmt.Fprintf(&b, "IntAttr%03d=%d; ", j, (i*(j+3))%(1000*(j+1)))
		}
		for j := range 45 {
			fmt.Fprintf(&b, `StrAttr%02d="v%d"; `, j, (i+j)%(5+j))
		}
		if i%10 == 3 {
			fmt.Fprintf(&b, `VacateReasonCode=%d; VacateReason="reason %d"; `, i%23, i%5)
		}
		b.WriteString("Last=1 ]")
		if aerr := a.Append(mustAd(t, b.String())); aerr != nil {
			t.Fatal(aerr)
		}
	}
	a.c.colBudget = 1 << 20
	if !a.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no enable")
	}
	sealed := len(payloads(a.c))
	t.Logf("%d ads, %d sealed segments, schema %d fields, VacateReasonCode a field: %v",
		n, sealed, len(a.c.schemaScan.Load().schema.fields), schemaHas(a.c, "VacateReasonCode"))

	// Steady passes: a re-derivation that finds nothing material.
	a.c.colBudget = 64
	var steady []time.Duration
	for range 5 {
		before := payloads(a.c)
		t0 := time.Now()
		a.BuildAndEnableSchemaScan(4096, 8)
		steady = append(steady, time.Since(t0))
		if changedPayloads(before, payloads(a.c)) != 0 {
			t.Fatal("a steady pass rewrote segments")
		}
	}
	t.Logf("steady pass, review not due: median %v (%v)", median(steady), steady)
	// The same with a re-derivation on every pass, which is what a pass costs once an hour (or when
	// the required set changed).
	saved := schemaReviewInterval
	schemaReviewInterval = 0
	steady = steady[:0]
	for range 5 {
		t0 := time.Now()
		a.BuildAndEnableSchemaScan(4096, 8)
		steady = append(steady, time.Since(t0))
	}
	schemaReviewInterval = saved
	t.Logf("steady pass, re-derivation due: median %v (%v)", median(steady), steady)

	// Freeze a copy on the old schema for the before/after comparison.
	if cerr := a.Close(); cerr != nil {
		t.Fatal(cerr)
	}
	oldDir := copyTree(t, dir)
	a, err = OpenArchive(ArchiveOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	old, err := OpenArchive(ArchiveOptions{Dir: oldDir})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	a.c.colBudget = 64
	a.AddSchemaRequireAttrs("VacateReasonCode")
	var passes []string
	for pass := 1; ; pass++ {
		before := payloads(a.c)
		t0 := time.Now()
		a.BuildAndEnableSchemaScan(4096, 8)
		d := time.Since(t0)
		rw := changedPayloads(before, payloads(a.c))
		passes = append(passes, fmt.Sprintf("%v/%d segs", d.Round(time.Millisecond), rw))
		if a.c.staleSegmentCount(a.c.schemaScan.Load()) == 0 {
			break
		}
		if pass > sealed {
			t.Fatal("did not converge")
		}
	}
	t.Logf("converging passes (budget 64): %v", passes)

	q, err := vm.Parse(fmt.Sprintf("EpochWriteDate >= %d && VacateReasonCode isnt undefined", reviewEpoch0+(n*9/10)*10))
	if err != nil {
		t.Fatal(err)
	}
	type arm struct {
		name string
		a    *Archive
	}
	arms := []arm{{"old schema", old}, {"converged", a}}
	rows := func(x *Archive) int {
		k := 0
		for range x.Query(q) {
			k++
		}
		return k
	}
	count := func(x *Archive) int {
		k, ok := x.c.CountQuery(q)
		if !ok {
			return rows(x)
		}
		return k
	}
	for _, m := range []struct {
		name string
		fn   func(*Archive) int
	}{{"Query rows", rows}, {"COUNT", count}} {
		times := map[string][]time.Duration{}
		var want = -1
		for round := range 9 {
			order := arms
			if round%2 == 1 {
				order = []arm{arms[1], arms[0]}
			}
			for _, x := range order {
				t0 := time.Now()
				got := m.fn(x.a)
				times[x.name] = append(times[x.name], time.Since(t0))
				if want < 0 {
					want = got
				} else if got != want {
					t.Fatalf("%s: %s answered %d, other arm %d", m.name, x.name, got, want)
				}
			}
		}
		mo, mc := median(times["old schema"]), median(times["converged"])
		t.Logf("%s (%d matches): old schema %v, converged %v, %.1fx", m.name, want, mo, mc, float64(mo)/float64(mc))
	}
}

func median(ds []time.Duration) time.Duration {
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[len(s)/2]
}

// TestRemnantListMeasure measures what the remnant-name list buys and costs: COUNT over an attribute
// no record holds, and over a non-schema attribute most records lack, after a restart -- on a copy
// columnarized without the list (every store before it) and one with it, timed interleaved with the
// order alternated each round, medians reported -- and the list's bytes per segment.
//
// Measurement, not a check: run with CLASSAD_BENCH_SCHEMA=1.
func TestRemnantListMeasure(t *testing.T) {
	if os.Getenv("CLASSAD_BENCH_SCHEMA") == "" {
		t.Skip("set CLASSAD_BENCH_SCHEMA=1 to measure")
	}
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	const n = 60000
	build := func(list bool) *Archive {
		opts := ArchiveOptions{Dir: t.TempDir(), SegmentSize: 1 << 19, GroupSchemaCount: -1}
		a, err := CreateArchive(opts)
		if err != nil {
			t.Fatal(err)
		}
		for i := range n {
			var b strings.Builder
			fmt.Fprintf(&b, `[ EpochWriteDate=%d; ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; `,
				reviewEpoch0+i*10, i/10, i%10, i%97, i%7)
			for j := range 100 {
				fmt.Fprintf(&b, "IntAttr%03d=%d; ", j, (i*(j+3))%(1000*(j+1)))
			}
			for j := range 45 {
				fmt.Fprintf(&b, `StrAttr%02d="v%d"; `, j, (i+j)%(5+j))
			}
			if i%10 == 3 {
				fmt.Fprintf(&b, `VacateReasonCode=%d; VacateReason="reason %d"; `, i%23, i%5)
			}
			b.WriteString("MemoryUsage=((IntAttr001 + 1023) / 1024); Last=1 ]")
			if aerr := a.Append(mustAd(t, b.String())); aerr != nil {
				t.Fatal(aerr)
			}
		}
		a.c.colBudget = 1 << 20
		colWriteRemnantList = list
		ok := a.BuildAndEnableSchemaScan(4096, 8)
		colWriteRemnantList = true
		if !ok {
			t.Fatal("no enable")
		}
		if cerr := a.Close(); cerr != nil {
			t.Fatal(cerr)
		}
		if a, err = OpenArchive(opts); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = a.Close() })
		return a
	}
	legacy, listed := build(false), build(true)

	segs, trailer, payload := 0, 0, 0
	for _, seg := range listed.c.shards[0].segs {
		if cn := seg.colNative.Load(); seg != nil && cn != nil {
			segs++
			payload += len(recAd(seg.data, cn.rec))
			if r := cn.seg.remnants; r != nil {
				trailer += len(appendRemnantTrailer(nil, r.names))
			}
		}
	}
	t.Logf("%d columnarized segments: remnant list %d B/segment, %.3f%% of payload bytes",
		segs, trailer/max(segs, 1), 100*float64(trailer)/float64(max(payload, 1)))

	for _, expr := range []string{"NoSuchAttribute > 1", "VacateReasonCode == 5", "MemoryUsage > 1"} {
		q, err := vm.Parse(expr)
		if err != nil {
			t.Fatal(err)
		}
		arms := []struct {
			name string
			a    *Archive
		}{{"without list", legacy}, {"with list", listed}}
		times := map[string][]time.Duration{}
		fbs := map[string]int64{}
		want := -1
		for round := range 9 {
			order := []int{0, 1}
			if round%2 == 1 {
				order = []int{1, 0}
			}
			for _, i := range order {
				x := arms[i]
				fb := columnarFallbacks.Load()
				t0 := time.Now()
				got, ok := x.a.c.CountQuery(q)
				times[x.name] = append(times[x.name], time.Since(t0))
				fbs[x.name] = columnarFallbacks.Load() - fb
				if !ok {
					t.Fatalf("%s: %s declined", expr, x.name)
				}
				if want < 0 {
					want = got
				} else if got != want {
					t.Fatalf("%s: %s answered %d, other arm %d", expr, x.name, got, want)
				}
			}
		}
		mo, mn := median(times["without list"]), median(times["with list"])
		t.Logf("COUNT %s (%d): without list %v (%d fallbacks), with list %v (%d fallbacks), %.1fx",
			expr, want, mo, fbs["without list"], mn, fbs["with list"], float64(mo)/float64(mn))
	}
}
