package db

import (
	"strconv"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// probeChainFixture builds three indexed archives joined on one key: two large and one
// small. unordered gives up the newest-first promise, which is what lets the planner choose.
//
//	big1, big2   big rows, ClusterId 1..big
//	sel          small rows, ClusterId = stride, 2*stride, ...
func probeChainFixture(tb testing.TB, big, small int, unordered bool) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	// A small segment size on purpose: postings are built when a segment seals, so a
	// fixture whose rows all fit one open segment would have the index configured and
	// none built, and every probe here would be a filter over a full scan.
	cfg := ArchiveConfig{SegmentSize: 4096, ValueAttrs: []string{"ClusterId"}, Unordered: unordered}
	mk := func(name string) *ArchiveTable {
		t, cerr := cat.CreateArchiveTable(name, cfg)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return t
	}
	b1, b2, sel := mk("big1"), mk("big2"), mk("sel")
	for i := 1; i <= big; i++ {
		for _, t := range []*ArchiveTable{b1, b2} {
			ad := classad.New()
			ad.InsertAttr("ClusterId", int64(i))
			ad.InsertAttrString("Pad", strings.Repeat("x", 64))
			if aerr := t.Append(ad); aerr != nil {
				tb.Fatal(aerr)
			}
		}
	}
	for i := big / small; i <= big; i += big / small {
		ad := classad.New()
		ad.InsertAttr("ClusterId", int64(i))
		ad.InsertAttrString("Owner", "alice")
		if aerr := sel.Append(ad); aerr != nil {
			tb.Fatal(aerr)
		}
	}
	return cat
}

// probeChainSpec is `from f, s1 ON f.ClusterId == s1.ClusterId, s2 ON f.ClusterId ==
// s2.ClusterId`: a star, so any of the three may sit at its centre.
func probeChainSpec(from, s1, s2 string) JoinSpec {
	step := func(table string) JoinStep {
		return JoinStep{
			Kind: JoinInner,
			Side: JoinSide{Table: table, Alias: table},
			Cond: []string{from + ".ClusterId == " + table + ".ClusterId"},
			Equi: []JoinEqui{{Left: from + ".ClusterId", Right: "ClusterId"}},
		}
	}
	return JoinSpec{
		Left:  JoinSide{Table: from, Alias: from},
		Steps: []JoinStep{step(s1), step(s2)},
	}
}

func drainChain(tb testing.TB, cat *Catalog, spec JoinSpec) (int, *JoinStats) {
	tb.Helper()
	var st JoinStats
	seq, err := cat.Join(spec, JoinLimits{}, &st)
	if err != nil {
		tb.Fatal(err)
	}
	n := 0
	for range seq {
		n++
	}
	if st.Err != nil {
		tb.Fatal(st.Err)
	}
	return n, &st
}

// What the FROM side is worth.
//
// The probe narrows each step's table to the key values the FROM side holds, so the FROM
// side's key count decides whether any step is read through an index at all. Written FROM a
// large table the chain offers more keys than maxProbeKeys and every step scans; written
// FROM the small one each large table is read through its index instead.
//
// Both arms are the same inner join over the same data and the row counts are compared
// before anything is timed.
func BenchmarkChainFromSide(b *testing.B) {
	cat := probeChainFixture(b, 8192, 32, true)
	spec := probeChainSpec("big1", "big2", "sel")

	var want int
	withoutChainReorder(func() { want, _ = drainChain(b, cat, spec) })
	if got, _ := drainChain(b, cat, spec); got != want {
		b.Fatalf("the re-planned chain joined %d rows, the chain as written %d", got, want)
	}

	// The arms are run in both orders across the two rounds, so a result that is really
	// the machine warming up cannot be read as the plan being faster.
	for _, arm := range []struct {
		name string
		wrap func(func())
	}{
		{"as-written", withoutChainReorder},
		{"planned", func(f func()) { f() }},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var last *JoinStats
			arm.wrap(func() {
				for i := 0; i < b.N; i++ {
					_, last = drainChain(b, cat, spec)
				}
			})
			b.ReportMetric(float64(last.HashRows), "hashRows")
			b.ReportMetric(float64(last.Pairs), "pairs")
		})
	}
}

// What the STEP order is worth, with the FROM side held fixed.
//
// A chain reads every table once whichever order its steps are in, so what moves here is
// only the pair loop -- which is why this is the smaller of the two effects even on a shape
// built to maximize it: a step that doubles the row count folded before a step that drops
// all but a handful of them.
func BenchmarkChainStepOrder(b *testing.B) {
	cat := stepOrderFixture(b, 20000, 50)
	spec := stepOrderSpec()

	var want int
	withoutChainReorder(func() { want, _ = drainChain(b, cat, spec) })
	if got, _ := drainChain(b, cat, spec); got != want {
		b.Fatalf("the re-ordered chain joined %d rows, the chain as written %d", got, want)
	}

	for _, arm := range []struct {
		name string
		wrap func(func())
	}{
		{"as-written", withoutChainReorder},
		{"planned", func(f func()) { f() }},
	} {
		b.Run(arm.name, func(b *testing.B) {
			var last *JoinStats
			arm.wrap(func() {
				for i := 0; i < b.N; i++ {
					_, last = drainChain(b, cat, spec)
				}
			})
			b.ReportMetric(float64(last.Pairs), "pairs")
		})
	}
}

// stepOrderFixture is three mutable tables where the step order is the only thing that can
// move: a FROM side with far too many keys to probe with, a table that doubles whatever
// reaches it, and a table that drops all but nsel of them.
func stepOrderFixture(tb testing.TB, n, nsel int) *Catalog {
	tb.Helper()
	cat, err := OpenCatalog(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { cat.Close() })
	mk := func(name string) *DB {
		d, cerr := cat.CreateTable(name)
		if cerr != nil {
			tb.Fatal(cerr)
		}
		return d
	}
	put := func(d *DB, key string, k, v int) {
		ad := classad.New()
		ad.InsertAttr("K", int64(k))
		ad.InsertAttr("V", int64(v))
		if werr := d.Put(key, ad); werr != nil {
			tb.Fatal(werr)
		}
	}
	a, sel, fan := mk("a"), mk("sel"), mk("fan")
	for i := 0; i < n; i++ {
		put(a, "a"+strconv.Itoa(i), i, i)
	}
	for i := 0; i < nsel; i++ {
		put(sel, "s"+strconv.Itoa(i), i, i)
	}
	for i := 0; i < 2*n; i++ {
		put(fan, "f"+strconv.Itoa(i), i%n, i)
	}
	return cat
}

func stepOrderSpec() JoinSpec {
	step := func(table string) JoinStep {
		return JoinStep{
			Kind: JoinInner,
			Side: JoinSide{Table: table, Alias: table, Project: []string{"K"}},
			Cond: []string{"a.K == " + table + ".K"},
			Equi: []JoinEqui{{Left: "a.K", Right: "K"}},
		}
	}
	// fan first as written, which is the order that carries the most rows between the steps.
	return JoinSpec{
		Left:  JoinSide{Table: "a", Alias: "a", Project: []string{"K"}},
		Steps: []JoinStep{step("fan"), step("sel")},
	}
}
