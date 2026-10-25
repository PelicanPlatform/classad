package collections

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
)

// schemaLagArchive is an archive whose older segments were columnarized under a schema WITHOUT the
// zone-mapped attribute Ts: in the old phase only four records in five carry it, under the schema's
// presence floor, and it joined the schema (as a required attribute) only after those segments were
// rewritten. A columnarized segment keeps the schema it was written with, so a column aggregate over
// `Ts >= T` has no block zone for Ts there and walks the segment by row -- unless the segment's own
// zone map, which does bound Ts, rules it out first.
//
// Old records' Ts lies about a million seconds in the past and new ones' in the future, so `Ts >= now`
// and `Ts >= CurrentTime - 600` select the new phase whatever the clock does while the test runs.
// Two old records are exceptions the pruning must not lose: `straggler` has a future Ts (its segment's
// zone reaches the query range, so the segment is read and the record counted; its Val is positive, so
// it also matches `Val > 0`), and `poison` defines
// CurrentTime itself (the CurrentTime-folded probes do not describe it, so its segment is never pruned
// on them, and it matches `Ts >= CurrentTime - 600`).
type schemaLagArchive struct {
	now               int64
	old, new          int
	straggler, poison int
}

const schemaLagSegSize = 1 << 12

func (f *schemaLagArchive) ts(i int) int64 {
	switch {
	case i == f.straggler:
		return f.now + 500_000
	case i < f.old:
		return f.now - 1_000_000 + int64(i)
	}
	return f.now + 1000 + int64(i)
}

func (f *schemaLagArchive) append(tb testing.TB, a *Archive, from, to int) {
	tb.Helper()
	for i := from; i < to; i++ {
		var b strings.Builder
		fmt.Fprintf(&b, `[ N = %d; Grp = %d; Val = %d; Owner = "u%d"; Pad = %q`, i, i%5, (i*37)%1000-300, i%7, strings.Repeat("p", 16))
		if i >= f.old || i%5 != 0 || i == f.straggler || i == f.poison {
			fmt.Fprintf(&b, `; Ts = %d`, f.ts(i))
		}
		if i == f.poison {
			b.WriteString(`; CurrentTime = 0`)
		}
		b.WriteString(` ]`)
		ad, err := classad.Parse(b.String())
		if err != nil {
			tb.Fatal(err)
		}
		if err := a.Append(ad); err != nil {
			tb.Fatal(err)
		}
	}
}

// newSchemaLagArchive builds the fixture. poison=false leaves out the record defining CurrentTime, so
// every segment is proven free of it and the predicate tiers serve the CurrentTime query too.
func newSchemaLagArchive(tb testing.TB, nOld, nNew int, poison bool) (*schemaLagArchive, *Archive) {
	tb.Helper()
	if !mmapSupported {
		tb.Skip("persistence is unix-only")
	}
	f := &schemaLagArchive{now: time.Now().Unix(), old: nOld, new: nNew, straggler: nOld/3 + 10, poison: -1}
	if poison {
		f.poison = nOld/2 + 1
	}
	a, err := CreateArchive(ArchiveOptions{Dir: tb.TempDir(), SegmentSize: schemaLagSegSize,
		ZoneAttrs: []string{"Ts"}})
	if err != nil {
		tb.Fatal(err)
	}
	f.append(tb, a, 0, f.old)
	if !a.BuildAndEnableSchemaScan(1<<16, 8) {
		tb.Fatal("schema scan did not enable")
	}
	for a.c.ColumnarizeSealed() > 0 {
	}
	f.append(tb, a, f.old, f.old+f.new)
	a.AddSchemaRequireAttrs("Ts")
	if !a.ReschemaScan(1<<16, 8) {
		tb.Fatal("reschema failed")
	}
	a.BuildAndEnableSchemaScan(1<<16, 8)
	for a.c.ColumnarizeSealed() > 0 {
	}

	// The shape the test is about: most sealed segments columnarized without Ts, the current schema
	// with it, and the old segments zone-mapped on Ts.
	tsID, ok := a.c.intern.LookupID("Ts")
	if !ok {
		tb.Fatal("Ts not interned")
	}
	if st := a.c.schemaScan.Load(); st == nil {
		tb.Fatal("no schema")
	} else if _, ok := st.schema.byID[tsID]; !ok {
		tb.Fatal("fixture: the current schema lacks Ts")
	}
	lacking, zoned, withTs := 0, 0, 0
	for _, sh := range a.c.shards {
		_, wins := sh.snapshot()
		for _, w := range wins {
			cs := w.seg.colblk.Load()
			if sch := cs.schema(); sch != nil {
				if _, has := sch.byID[tsID]; has {
					withTs++
				} else {
					lacking++
					if _, z := w.zones[tsID]; z {
						zoned++
					}
				}
			}
		}
		releaseWindows(wins)
	}
	if lacking < 10 || zoned != lacking || withTs == 0 {
		tb.Fatalf("fixture: %d segments lack Ts in their schema (%d zoned on it), %d carry it", lacking, zoned, withTs)
	}
	return f, a
}

// BenchmarkColumnAggregateSchemaLag measures the column aggregates over a recent window of an archive
// whose older segments were columnarized before the filtered field joined the schema: those segments
// are walked by row unless their zone map rules them out.
func BenchmarkColumnAggregateSchemaLag(b *testing.B) {
	f, a := newSchemaLagArchive(b, 40000, 1000, false)
	c := a.c
	q := mustQuery(b, fmt.Sprintf(`Ts >= %d`, f.now))
	want := 0
	for ad := range c.Scan() {
		if q.Matches(ad) {
			want++
		}
	}
	run := func(name string, fn func() bool) {
		b.Run(name, func(b *testing.B) {
			for b.Loop() {
				if !fn() {
					b.Fatal("not served")
				}
			}
		})
	}
	run("count", func() bool { n, ok := c.CountQuery(q); return ok && n == want })
	run("stats", func() bool { st, ok := c.NumStatsQuery(q, "Val"); return ok && st.N == want })
	run("group", func() bool { _, ok := c.GroupStatsQuery(q, "Grp", []string{"Val"}); return ok })
	run("topk", func() bool { _, seen, ok := c.TopKOrderThreshold(q, "Ts", true, 10, nil); return ok && seen == want })
	run("vector", func() bool { n, ok := c.VectorEvalCount(q); return ok && n == want })
	run("eval", func() bool { n, ok := c.ColumnarEvalCount(q); return ok && n == want })
}
