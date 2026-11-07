package collections

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// THE FORM READERS NEVER SEE A COMPUTED VALUE.
//
// A computed cell's value is a property of the record, not part of it: the record holds the
// expression, and every reader that rebuilds an ad or ships attribute text, wire or nodes must emit
// that expression. Each read path below is compared, attribute by attribute, against the record as it
// was written; a path that emitted the computed literal in place of the expression (MemUse = 1234 for
// ((Rss + 1023) / 1024)) fails here by name.

// formFixture is a persistent store of computedAd records, columnarized and reopened.
type formFixture struct {
	c       *Collection
	archive bool
	src     map[int64]*classad.ClassAd // the live records as written, by ClusterId
	ever    map[int64]*classad.ClassAd // every record ever written, for the reads of an earlier time
	close   func() error
	reopen  func() *formFixture
	// extraProj are projections checked beside the default ones.
	extraProj [][]string
}

// formAd is computedAd where, beside the uncomputable MemUse, one record in excEvery holds a Label no
// record can compute but each evaluates alone to "rare" -- a string the block's dictionary never sees,
// so a block holding one must not be pruned on the dictionary -- and one record in 499 computes a
// MemUse far wider than the column the rest of them fit. Scaled is computed in every other record, to
// values far above every literal one, so a block's literal range alone would rule out records it holds.
func formAd(i, excEvery int) string {
	s := computedAd(i, excEvery)
	scaled := fmt.Sprint(i % 7)
	if i%2 == 0 {
		scaled = "ProcId * 1000 + 5000"
	}
	s = strings.Replace(s, "ClusterId = ", "Scaled = "+scaled+"; ClusterId = ", 1)
	if excEvery > 0 && i%excEvery == excEvery/2+1 {
		s = labelAttr.ReplaceAllString(s, `Label = ifThenElse(isUndefined(TARGET.Name), "rare", "x");`)
	}
	if i%499 == 250 {
		s = strings.Replace(s, "MemUse = ((Rss + 1023) / 1024)", "MemUse = ((Rss + 1023) / 1024) + 1099511627776", 1)
	}
	return s
}

var labelAttr = regexp.MustCompile(`Label = [^;]+;`)

func buildFormFixture(t *testing.T, dir string, n, excEvery int, archive, interned bool) *formFixture {
	t.Helper()
	f := buildFormFixtureGen(t, dir, n, excEvery, archive, interned, formAd)
	if !columnarHoldsComputed(f.c) {
		t.Fatal("no columnarized segment holds a computed cell")
	}
	return f
}

// buildFormFixtureGen is buildFormFixture over the records gen writes.
func buildFormFixtureGen(t *testing.T, dir string, n, excEvery int, archive, interned bool, gen func(i, excEvery int) string) *formFixture {
	t.Helper()
	return buildFormFixtureOpts(t, dir, n, excEvery, archive, interned, 0, gen)
}

// buildFormFixtureOpts is buildFormFixtureGen with Options.MoveListColumns set to moves, for this open and
// every reopen.
func buildFormFixtureOpts(t *testing.T, dir string, n, excEvery int, archive, interned bool, moves int, gen func(i, excEvery int) string) *formFixture {
	t.Helper()
	src := make(map[int64]*classad.ClassAd, n)
	for i := range n {
		src[int64(i)] = mustAd(t, gen(i, excEvery))
	}
	var open func(fresh bool) *formFixture
	open = func(fresh bool) *formFixture {
		f := &formFixture{archive: archive, src: src, ever: maps.Clone(src)}
		if archive {
			opts := ArchiveOptions{Dir: dir, SegmentSize: 1 << 16, GroupSchemaCount: -1, InternAtSeal: interned, MoveListColumns: moves}
			var a *Archive
			var err error
			if fresh {
				a, err = CreateArchive(opts)
			} else {
				a, err = OpenArchive(opts)
			}
			if err != nil {
				t.Fatal(err)
			}
			f.c, f.close = a.c, a.Close
			if fresh {
				for i := range n {
					if err := a.Append(src[int64(i)]); err != nil {
						t.Fatal(err)
					}
				}
			}
		} else {
			c, err := Open(Options{Dir: dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1, WatchHistory: 64, MoveListColumns: moves,
				TimeTravel: &TimeTravelOptions{MaxDistance: time.Hour, CheckpointInterval: time.Millisecond}})
			if err != nil {
				t.Fatal(err)
			}
			f.c, f.close = c, c.Close
			if fresh {
				for i := range n {
					if err := c.Put([]byte(fmt.Sprintf("%d.0", i)), src[int64(i)]); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
		f.reopen = func() *formFixture {
			if err := f.close(); err != nil {
				t.Fatal(err)
			}
			g := open(false)
			g.extraProj = f.extraProj
			g.c.BuildAndEnableSchemaScan(4096, 8)
			return g
		}
		return f
	}
	f := open(true)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("no schema derived")
	}
	f = f.reopen()
	t.Cleanup(func() { f.close() })
	return f
}

// columnarHoldsComputed reports whether any columnarized segment of c keeps a computed value.
func columnarHoldsComputed(c *Collection) bool {
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			if seg == nil {
				continue
			}
			if cn := seg.colNative.Load(); cn != nil && cn.seg.hasComputed() {
				return true
			}
		}
	}
	return false
}

// attrTexts returns ad's attributes as name=expression text, the names folded, restricted to keep when
// it is non-nil. MyType and TargetType are left out: some paths carry them in the header, some as
// attributes.
func attrTexts(ad *classad.ClassAd, keep map[string]bool) map[string]string {
	out := map[string]string{}
	for _, name := range ad.GetAttributes() {
		fold := strings.ToLower(name)
		if fold == "mytype" || fold == "targettype" || (keep != nil && !keep[fold]) {
			continue
		}
		e, ok := ad.Lookup(name)
		if !ok {
			continue
		}
		out[fold] = e.String()
	}
	return out
}

// check compares one emitted ad against the record it came from. With proj nil the emitted ad must hold
// exactly the record's attributes; with a projection, every projected attribute the record holds must
// come back, and anything else the path adds (a reference it chased) must also be the record's own.
func (f *formFixture) check(t *testing.T, path string, got *classad.ClassAd, proj []string) bool {
	t.Helper()
	return f.checkIn(t, path, got, proj, f.src)
}

func (f *formFixture) checkIn(t *testing.T, path string, got *classad.ClassAd, proj []string, recs map[int64]*classad.ClassAd) bool {
	t.Helper()
	gt := attrTexts(got, nil)
	cid, ok := got.EvaluateAttrInt("ClusterId")
	if !ok {
		t.Errorf("%s: an emitted ad has no ClusterId: %v", path, got)
		return false
	}
	src, ok := recs[cid]
	if !ok {
		t.Errorf("%s: ClusterId %d is not a record of the fixture", path, cid)
		return false
	}
	st := attrTexts(src, nil)
	for name, text := range gt {
		want, ok := st[name]
		if !ok {
			t.Errorf("%s: record %d: %s = %s, which the record does not hold", path, cid, name, text)
			return false
		}
		if text != want {
			t.Errorf("%s: record %d: %s = %s, want %s as written", path, cid, name, text, want)
			return false
		}
	}
	if proj == nil {
		for name, want := range st {
			if _, ok := gt[name]; !ok {
				t.Errorf("%s: record %d is missing %s = %s", path, cid, name, want)
				return false
			}
		}
		return true
	}
	for _, p := range proj {
		fold := strings.ToLower(p)
		if want, ok := st[fold]; ok {
			if _, got := gt[fold]; !got {
				t.Errorf("%s: record %d is missing projected %s = %s", path, cid, p, want)
				return false
			}
		}
	}
	return true
}

// checkProjected compares a projection that need not carry ClusterId: the multiset of projected
// attribute texts across all emitted ads must equal that of the records.
func (f *formFixture) checkProjected(t *testing.T, path string, got []*classad.ClassAd, proj []string, want int) {
	t.Helper()
	keep := map[string]bool{}
	for _, p := range proj {
		keep[strings.ToLower(p)] = true
	}
	render := func(ad *classad.ClassAd) string {
		m := attrTexts(ad, keep)
		parts := make([]string, 0, len(m))
		for k, v := range m {
			parts = append(parts, k+"="+v)
		}
		sort.Strings(parts)
		return strings.Join(parts, "; ")
	}
	var g, w []string
	for _, ad := range got {
		g = append(g, render(ad))
	}
	for _, ad := range f.src {
		w = append(w, render(ad))
	}
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != want {
		t.Errorf("%s: %d ads, want %d", path, len(g), want)
		return
	}
	if !slices.Equal(g, w) {
		for i := range g {
			if i < len(w) && g[i] != w[i] {
				t.Errorf("%s: emitted %q, the records hold %q", path, g[i], w[i])
				return
			}
		}
		t.Errorf("%s: the emitted projections differ from the records", path)
	}
}

func wireToAd(t *testing.T, c *Collection, w []byte) *classad.ClassAd {
	t.Helper()
	ad, err := c.decodeWireAny(w)
	if err != nil {
		t.Fatalf("an emitted wire ad does not decode: %v", err)
	}
	return ad
}

func rawToAd(t *testing.T, r RawAd) *classad.ClassAd {
	t.Helper()
	parts := make([]string, 0, len(r.Exprs))
	for _, e := range r.Exprs {
		parts = append(parts, string(e))
	}
	ad, err := classad.Parse("[" + strings.Join(parts, "; ") + "]")
	if err != nil {
		t.Fatalf("an emitted text ad does not parse: %v", err)
	}
	return ad
}

// checkFormReaders runs every form-reading path against f and compares each against the records.
func checkFormReaders(t *testing.T, f *formFixture, label string) {
	t.Helper()
	c := f.c
	n := len(f.src)
	all, _ := vm.Parse("true")
	sel, _ := vm.Parse("ProcId >= 0") // a constraint, so the query plans rather than taking the scan path
	mem, _ := vm.Parse("MemUse > 2047")
	memWant := 0
	for _, ad := range f.src {
		if v, ok := ad.EvaluateAttrNumber("MemUse"); ok && v > 2047 {
			memWant++
		}
	}
	projections := [][]string{{"MemUse"}, {"MemUse", "Label", "Owner"}, {"Big", "Ratio", "Flag", "Rss"}, {"Label"}}
	projections = append(projections, f.extraProj...)

	full := func(path string, seq func(func(*classad.ClassAd) bool), want int) {
		t.Helper()
		got := 0
		for ad := range seq {
			got++
			if !f.check(t, label+": "+path, ad, nil) {
				return
			}
		}
		if got != want {
			t.Errorf("%s: %s returned %d ads, want %d", label, path, got, want)
		}
	}
	full("Scan", c.Scan(), n)
	full("Query", c.Query(sel), n)
	full("QueryRedacted", c.QueryRedacted(sel), n)
	full("Query(MemUse > 2047)", c.Query(mem), memWant)
	full("ForEachAd", func(yield func(*classad.ClassAd) bool) {
		c.ForEachAd(func(_ string, ad *classad.ClassAd) bool { return yield(ad) })
	}, n)
	full("ForEachAdRedacted", func(yield func(*classad.ClassAd) bool) {
		c.ForEachAdRedacted(func(_ string, ad *classad.ClassAd) bool { return yield(ad) })
	}, n)
	for _, redact := range []bool{false, true} {
		rl := fmt.Sprintf("redact=%v", redact)
		full("QueryRawWire "+rl, func(yield func(*classad.ClassAd) bool) {
			for w := range c.QueryRawWire(sel, nil, redact) {
				if !yield(wireToAd(t, c, w)) {
					return
				}
			}
		}, n)
		full("ScanRawWire "+rl, func(yield func(*classad.ClassAd) bool) {
			for w := range c.ScanRawWire(nil, redact) {
				if !yield(wireToAd(t, c, w)) {
					return
				}
			}
		}, n)
		full("QueryRawWire(MemUse > 2047) "+rl, func(yield func(*classad.ClassAd) bool) {
			for w := range c.QueryRawWire(mem, nil, redact) {
				if !yield(wireToAd(t, c, w)) {
					return
				}
			}
		}, memWant)
		raw := c.QueryRaw
		scanRaw := c.ScanRaw
		if redact {
			raw, scanRaw = c.QueryRawRedacted, c.ScanRawRedacted
		}
		full("QueryRaw "+rl, func(yield func(*classad.ClassAd) bool) {
			for r := range raw(sel) {
				if !yield(rawToAd(t, r)) {
					return
				}
			}
		}, n)
		full("ScanRaw "+rl, func(yield func(*classad.ClassAd) bool) {
			for r := range scanRaw() {
				if !yield(rawToAd(t, r)) {
					return
				}
			}
		}, n)
		for _, proj := range projections {
			pl := fmt.Sprintf("%v %s", proj, rl)
			for _, q := range []*vm.Query{all, mem} {
				want := n
				if q == mem {
					want = memWant
					// The constraint's own reads are not part of the projection; compare by record.
					var ads []*classad.ClassAd
					for w := range c.QueryRawWire(q, append(slices.Clone(proj), "ClusterId"), redact) {
						ads = append(ads, wireToAd(t, c, w))
					}
					for _, ad := range ads {
						if !f.check(t, label+": QueryRawWire(MemUse > 2047) "+pl, ad, proj) {
							break
						}
					}
					if len(ads) != want {
						t.Errorf("%s: QueryRawWire(MemUse > 2047) %s returned %d, want %d", label, pl, len(ads), want)
					}
					continue
				}
				var ads []*classad.ClassAd
				for w := range c.QueryRawWire(q, proj, redact) {
					ads = append(ads, wireToAd(t, c, w))
				}
				f.checkProjected(t, label+": QueryRawWire "+pl, ads, proj, want)
				ads = ads[:0]
				for w := range c.QueryRawWireRefs(q, proj, redact) {
					ads = append(ads, wireToAd(t, c, w))
				}
				f.checkProjected(t, label+": QueryRawWireRefs "+pl, ads, proj, want)
				for _, chase := range []bool{false, true} {
					ads = ads[:0]
					for r := range c.QueryRawProjected(q, proj, chase, redact) {
						ads = append(ads, rawToAd(t, r))
					}
					f.checkProjected(t, fmt.Sprintf("%s: QueryRawProjected chase=%v %s", label, chase, pl), ads, proj, want)
				}
			}
			var ads []*classad.ClassAd
			for w := range c.ScanRawWire(proj, redact) {
				ads = append(ads, wireToAd(t, c, w))
			}
			f.checkProjected(t, label+": ScanRawWire "+pl, ads, proj, n)
			ads = ads[:0]
			for r := range c.ScanRawProjected(proj, false, redact) {
				ads = append(ads, rawToAd(t, r))
			}
			f.checkProjected(t, label+": ScanRawProjected "+pl, ads, proj, n)
		}
	}
	if !f.archive {
		for cid := range f.src {
			if cid%211 != 5 {
				continue
			}
			key := []byte(fmt.Sprintf("%d.0", cid))
			ad, ok := c.Get(key)
			if !ok {
				t.Errorf("%s: Get(%s) found nothing", label, key)
				continue
			}
			f.check(t, label+": Get", ad, nil)
			if ad, ok = c.GetRedacted(key); ok {
				f.check(t, label+": GetRedacted", ad, nil)
			}
		}
		for _, redact := range []bool{false, true} {
			asOf := c.QueryAsOf
			if redact {
				asOf = c.QueryAsOfRedacted
			}
			// The time resolves to the nearest checkpoint, so which records it sees is not pinned here;
			// each one it returns must be a record as written.
			seq, err := asOf(sel, time.Now())
			if err != nil {
				t.Errorf("%s: QueryAsOf: %v", label, err)
				continue
			}
			got := 0
			for ad := range seq {
				got++
				if !f.checkIn(t, fmt.Sprintf("%s: QueryAsOf redact=%v", label, redact), ad, nil, f.ever) {
					break
				}
			}
			if got == 0 {
				t.Errorf("%s: QueryAsOf redact=%v returned nothing", label, redact)
			}
		}
		for _, redact := range []bool{false, true} {
			watch := c.Watch
			if redact {
				watch = c.WatchRedacted
			}
			ctx, cancel := context.WithCancel(context.Background())
			seq, err := watch(ctx, nil)
			if err != nil {
				cancel()
				t.Fatalf("%s: Watch: %v", label, err)
			}
			full(fmt.Sprintf("Watch replay redact=%v", redact), func(yield func(*classad.ClassAd) bool) {
				for ev := range seq {
					if ev.Kind == WatchSynced {
						return
					}
					if ev.Ad != nil && !yield(ev.Ad) {
						return
					}
				}
			}, n)
			cancel()
		}
	}
	// QueryProject evaluates: every value it returns is the attribute's VALUE, computed or not, so it is
	// compared against evaluation rather than against the text.
	for _, redact := range []bool{false, true} {
		proj := c.QueryProject
		if redact {
			proj = c.QueryProjectRedacted
		}
		var got, want []string
		for row := range proj(sel, []string{"ClusterId", "MemUse", "Label", "Big", "Ratio", "Flag"}) {
			parts := make([]string, len(row))
			for i, v := range row {
				parts[i] = v.String()
			}
			got = append(got, strings.Join(parts, "|"))
		}
		for _, ad := range f.src {
			var parts []string
			for _, a := range []string{"ClusterId", "MemUse", "Label", "Big", "Ratio", "Flag"} {
				parts = append(parts, ad.EvaluateAttr(a).String())
			}
			want = append(want, strings.Join(parts, "|"))
		}
		sort.Strings(got)
		sort.Strings(want)
		if !slices.Equal(got, want) {
			t.Errorf("%s: QueryProject redact=%v differs from evaluating each record (%d rows vs %d)", label, redact, len(got), len(want))
		}
	}
}

// recolumnarizeAll rewrites every columnarized segment again, through the path a group or schema
// change takes: the full ads are rebuilt from the old payload and the remnants, and split again.
func recolumnarizeAll(t *testing.T, c *Collection) int {
	t.Helper()
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("no schema")
	}
	c.maintMu.Lock()
	defer c.maintMu.Unlock()
	done := 0
	for _, sh := range c.shards {
		sh.mu.RLock()
		var srcs []*segment
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.columnarized() {
				srcs = append(srcs, seg)
			}
		}
		sh.mu.RUnlock()
		for _, src := range srcs {
			if c.columnarizeSealedSegment(sh, src, st.schema, st.hot) {
				done++
			}
		}
	}
	if done > 0 {
		c.reindexAfterCompaction()
	}
	return done
}

// TestComputedColumnFormReaders is the guard: no path that rebuilds an ad or ships attribute text, wire
// or nodes emits a computed value where the record holds an expression -- full reads, projections
// (including the computed attribute alone), raw wire with and without reference chasing, text, the
// redacted variants, AS OF, a watch replay -- on a mutable table, an archive and an interned archive,
// after reopening, after re-columnarizing and after a compaction.
func TestComputedColumnFormReaders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		archive, interned bool
		excEvery, records int
	}{
		{"mutable", false, false, 50, 1200},
		{"archive", true, false, 50, 1200},
		{"archive-interned", true, true, 20, 1200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := buildFormFixture(t, t.TempDir(), tc.records, tc.excEvery, tc.archive, tc.interned)
			checkFormReaders(t, f, "reopened")
			if recolumnarizeAll(t, f.c) == 0 {
				t.Fatal("nothing was re-columnarized")
			}
			if !columnarHoldsComputed(f.c) {
				t.Fatal("re-columnarizing dropped the computed cells")
			}
			checkFormReaders(t, f, "re-columnarized")
			if !tc.archive {
				// Delete a few records so compaction has something to reclaim, then reopen.
				for i := 0; i < tc.records; i += 97 {
					if !f.c.Delete([]byte(fmt.Sprintf("%d.0", i))) {
						t.Fatalf("Delete(%d.0) found nothing", i)
					}
					delete(f.src, int64(i))
				}
				f.c.Compact()
				checkFormReaders(t, f, "compacted")
			} else {
				f.c.InternSealed()
				checkFormReaders(t, f, "interned")
			}
			f = f.reopen()
			checkFormReaders(t, f, "reopened again")
		})
	}
}

// TestComputedColumnValueReaders is the value half: every columnar answer over a field with computed
// cells equals evaluating the records -- counts, presence, the vector and per-record evaluators,
// numeric aggregates, value and bucketed GROUP BY, the TopK cutoff and the string GROUP BY -- including
// integers past 2^53, a NaN result, reals among integers, UNDEFINED, ERROR and uncomputable cells.
func TestComputedColumnValueReaders(t *testing.T) {
	for _, tc := range []struct {
		name              string
		archive, interned bool
		excEvery          int
	}{
		{"mutable/0.1%", false, false, 1000},
		{"mutable/5%", false, false, 20},
		{"archive/5%", true, false, 20},
		{"archive-interned/0.1%", true, true, 1000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := buildFormFixture(t, t.TempDir(), 3000, tc.excEvery, tc.archive, tc.interned)
			checkComputedFixture(t, f.c, tc.name)
			checkComputedExtra(t, f, tc.name)
		})
	}
}

// checkComputedExtra covers the value readers checkComputedFixture does not.
func checkComputedExtra(t *testing.T, f *formFixture, label string) {
	t.Helper()
	c := f.c
	ads := make([]*classad.ClassAd, 0, len(f.src))
	for _, ad := range f.src {
		ads = append(ads, ad)
	}
	all, _ := vm.Parse("ProcId >= 0")
	for _, attr := range []string{"MemUse", "Big", "Ratio"} {
		// TopK cutoff: the k-th largest value, compared against sorting the evaluated values.
		var vals []float64
		for _, ad := range ads {
			if v, ok := ad.EvaluateAttrNumber(attr); ok && !math.IsNaN(v) {
				vals = append(vals, v)
			}
		}
		sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
		for _, k := range []int{1, 10, 100} {
			if th, _, ok := c.TopKOrderThreshold(all, attr, true, k, nil); ok && k <= len(vals) && th > vals[k-1] {
				t.Errorf("%s: TopKOrderThreshold(%s DESC, %d) = %v, above the k-th value %v", label, attr, k, th, vals[k-1])
			}
			if th, _, ok := c.TopKOrderThreshold(all, attr, false, k, nil); ok && k <= len(vals) && th < vals[len(vals)-k] {
				t.Errorf("%s: TopKOrderThreshold(%s ASC, %d) = %v, below the k-th value %v", label, attr, k, th, vals[len(vals)-k])
			}
		}
		// Every value the column yields, as a multiset, against evaluation.
		// One list per worker, each fed by one goroutine, joined once the pass is done.
		var parts []*[]string
		var got []string
		newSink := func() func(NumValue) {
			part := new([]string)
			parts = append(parts, part)
			return func(v NumValue) { *part = append(*part, fmt.Sprint(v)) }
		}
		merge := func() {
			for _, p := range parts {
				got = append(got, *p...)
			}
		}
		if c.NumValuesQuery(all, attr, newSink, merge) {
			var want []string
			for _, ad := range ads {
				v := ad.EvaluateAttr(attr)
				switch {
				case v.IsInteger():
					i, _ := v.IntValue()
					want = append(want, fmt.Sprint(NumValue{F: float64(i), I: i, Kind: NumInt}))
				case v.IsReal():
					r, _ := v.RealValue()
					want = append(want, fmt.Sprint(NumValue{F: r, Kind: NumReal}))
				}
			}
			sort.Strings(got)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("%s: NumValuesQuery(%s) yields %d values, evaluation %d, or they differ", label, attr, len(got), len(want))
			}
		}
	}
	// Bucketed GROUP BY on the computed field: per-bucket counts against evaluation.
	if gs, ok := c.GroupStatsQueryBucketed(all, "MemUse", 1024, nil); ok {
		want := map[string]int{}
		for _, ad := range ads {
			v := ad.EvaluateAttr("MemUse")
			// A bucketed group is a number's bucket; a record with no number is in no group.
			switch {
			case v.IsInteger():
				i, _ := v.IntValue()
				want[fmt.Sprint(int64(math.Floor(float64(i)/1024))*1024)]++
			case v.IsReal():
				r, _ := v.RealValue()
				want[fmt.Sprint(int64(math.Floor(r/1024))*1024)]++
			}
		}
		got := map[string]int{}
		for _, g := range gs {
			key := "undefined"
			if v := g.Values[0]; !v.IsUndefined() {
				if i, err := v.IntValue(); err == nil {
					key = fmt.Sprint(i)
				} else if r, err := v.RealValue(); err == nil {
					key = fmt.Sprint(int64(r))
				}
			}
			got[key] += g.Count
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: GROUP BY MemUse bucket 1024 = %v, want %v", label, got, want)
		}
	}
	// A string only an uncomputed cell holds: the dictionary of its block lacks it. And numbers only
	// computed cells hold, above every literal of their block.
	for _, e := range []string{`Label == "rare"`, `Label =?= "rare"`, `Label > "r"`, "Scaled > 4000", "Scaled >= 5000 && Scaled < 6000"} {
		q, _ := vm.Parse(e)
		want := computedRefCount(t, ads, e)
		if got, ok := c.VectorEvalCount(q); !ok || got != want {
			t.Errorf("%s: VectorEvalCount(%s) = %d (ok=%v), want %d", label, e, got, ok, want)
		}
		if got, ok := c.CountQuery(q); ok && got != want {
			t.Errorf("%s: CountQuery(%s) = %d, want %d", label, e, got, want)
		}
		if got, ok := c.ColumnarEvalCount(q); ok && got != want {
			t.Errorf("%s: ColumnarEvalCount(%s) = %d, want %d", label, e, got, want)
		}
	}
	// String GROUP BY on a computed string field.
	if gs, ok := c.GroupCountQuery(all, "Label"); ok {
		want := map[string]int{}
		for _, ad := range ads {
			want[ad.EvaluateAttr("Label").String()]++
		}
		got := map[string]int{}
		for _, g := range gs {
			got[g.Value.String()] += g.Count
		}
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: GROUP BY Label = %v, want %v", label, got, want)
		}
	}
}

// TestComputedColumnVectorizes: the column readers serve a computed field from the column, a block at a
// time. Every block of a field that is an expression in nearly every record vectorizes -- the few cells
// left uncomputed are evaluated one record at a time inside the block -- and a block whose only escapes
// are computed cells prunes on the range of its values, string dictionary included.
func TestComputedColumnVectorizes(t *testing.T) {
	f := buildFormFixture(t, t.TempDir(), 3000, 0, true, false)
	c := f.c
	ads := make([]*classad.ClassAd, 0, len(f.src))
	for _, ad := range f.src {
		ads = append(ads, ad)
	}
	for _, e := range []string{"MemUse > 2047", "MemUse > 2047 && Ratio < 500", "Flag", `Label == "SITE2"`} {
		q, _ := vm.Parse(e)
		got, ok := c.VectorEvalCount(q)
		if want := computedRefCount(t, ads, e); !ok || got != want {
			t.Fatalf("VectorEvalCount(%s) = %d (ok=%v), want %d", e, got, ok, want)
		}
		split := lastVecSplit.Load()
		if split.vecBlocks == 0 || split.scopeBlocks != 0 {
			t.Errorf("VectorEvalCount(%s): %d blocks vectorized, %d declined to the per-record resolver", e, split.vecBlocks, split.scopeBlocks)
		}
	}
	// Every Big is computed or a literal, so its blocks' value ranges are exact, and a value above all of
	// them prunes every block.
	q, _ := vm.Parse("Big > 9007199254700000 && Big < 9007199254700001")
	if got, ok := c.VectorEvalCount(q); !ok || got != 0 {
		t.Fatalf("VectorEvalCount(no Big in range) = %d (ok=%v), want 0", got, ok)
	}
	if split := lastVecSplit.Load(); split.prunedBlocks == 0 || split.vecBlocks+split.scopeBlocks != 0 {
		t.Errorf("a Big range no block holds: %d blocks pruned, %d read", split.prunedBlocks, split.vecBlocks+split.scopeBlocks)
	}
	q, _ = vm.Parse(`Label == "NOSUCHSITE"`)
	if got, ok := c.VectorEvalCount(q); !ok || got != 0 {
		t.Fatalf(`VectorEvalCount(Label == "NOSUCHSITE") = %d (ok=%v), want 0`, got, ok)
	}
	if split := lastVecSplit.Load(); split.prunedBlocks == 0 {
		t.Errorf("a Label no dictionary holds: %d blocks pruned, %d read", split.prunedBlocks, split.vecBlocks+split.scopeBlocks)
	}
}

// TestComputedColumnOlderBuildView reads a store the way a build that predates computed cells in the
// column reads it: the per-block bitmaps are gone -- such a build parses the computed trailer as blocks
// with no sections and never looks at their bytes -- while every slot still holds what this build wrote.
// Every form reader still emits the expressions and every value reader still answers exactly, so a
// rollback costs the acceleration and nothing else.
func TestComputedColumnOlderBuildView(t *testing.T) {
	f := buildFormFixture(t, t.TempDir(), 1500, 50, true, false)
	dropped := 0
	for _, sh := range f.c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			body, _, err := splitColPayload(recAd(seg.data, cn.rec))
			if err != nil {
				t.Fatal(err)
			}
			if cn.seg.hasComputed() {
				// The trailer as the older reader parses it: per block a byte string and no sections.
				trailer := appendComputedTrailer(nil, cn.seg.blocks)
				if !bytes.HasSuffix(body, trailer) {
					t.Fatal("the computed trailer is not the payload's last bytes")
				}
			}
			for _, b := range cn.seg.blocks {
				if len(b.computed) != 0 {
					t.Fatal("a block written now carries legacy sections")
				}
				dropped += len(b.ccols)
				b.ccols, b.ccBlob = nil, nil
			}
		}
	}
	if dropped == 0 {
		t.Fatal("no block held computed cells")
	}
	checkFormReaders(t, f, "older build")
	checkComputedFixture(t, f.c, "older build")
	checkComputedExtra(t, f, "older build")
}

// TestComputedEveryComputableCell checks every columnarized cell of the computed fields against its
// record: a cell whose expression the builder can compute holds exactly the value the record evaluates
// to -- in the column when it fits the column, beside it when it does not (a real among MemUse's ints,
// a MemUse far wider than its slot) -- and nothing is computed for a cell the builder must leave alone.
func TestComputedEveryComputableCell(t *testing.T) {
	f := buildFormFixture(t, t.TempDir(), 3000, 50, true, false)
	c := f.c
	cidID, _ := c.intern.LookupID("ClusterId")
	inColumn, beside := 0, 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			if cn == nil {
				continue
			}
			for _, b := range cn.seg.blocks {
				cidx, ok := b.schema.byID[cidID]
				if !ok {
					t.Fatal("ClusterId is not a column")
				}
				for _, attr := range []string{"MemUse", "Label", "Big", "Ratio", "Flag", "Scaled"} {
					id, _ := c.intern.LookupID(attr)
					idx, ok := b.schema.byID[id]
					if !ok {
						continue
					}
					for k := 0; k < b.n; k++ {
						cid, ok := b.fieldIntAt(k, cidx, cn.cache)
						if !ok {
							t.Fatal("ClusterId does not read")
						}
						src := f.src[cid]
						e, _ := src.Lookup(attr)
						node, computed := b.computedNode(k, idx, cn.cache)
						want := src.EvaluateAttr(attr)
						// A literal unparses to the value it is; an expression does not.
						isExpr := e != nil && e.String() != want.String()
						switch {
						case computed:
							if !isExpr {
								t.Fatalf("%s of record %d is the literal %s, but a value was computed for it", attr, cid, e)
							}
							lit, _ := wire.LiteralValue(node)
							if got := litToValue(lit); got.String() != want.String() || got.IsInteger() != want.IsInteger() {
								t.Fatalf("%s of record %d computed as %v, evaluates to %v", attr, cid, got, want)
							}
							if testBit(b.computedBits(idx), k) {
								inColumn++
							} else {
								beside++
							}
						case isExpr && strings.Contains(e.String(), "TARGET"):
						case isExpr && attr == "MemUse" && !want.IsUndefined():
							t.Fatalf("%s = %s of record %d (%v) was not computed", attr, e, cid, want)
						}
					}
				}
			}
		}
	}
	if inColumn == 0 || beside == 0 {
		t.Fatalf("%d cells computed into the column, %d beside it: the fixture exercises both", inColumn, beside)
	}
}
