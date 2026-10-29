package collections

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
)

// The base schema re-derived during maintenance, and every reader agreeing with a plain scan while
// segments of the old and new schema coexist.
//
// The fixture is an epoch-history-shaped table: every ad carries the same couple dozen attributes,
// and one in ten also carries VacateReasonCode / VacateReason, so the first-enable derivation leaves
// them out of the schema (presence 10%, floor 90%). Requiring VacateReasonCode afterwards is the
// production situation: an attribute the workload reads that the schema does not carry.

const reviewEpoch0 = 1_700_000_000

// reviewAd builds ad i. gen distinguishes rewrites of the same key between passes.
func reviewAd(i, gen int, secret bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, `[ EpochWriteDate=%d; ClusterId=%d; ProcId=%d; Owner="user%d"; JobStatus=%d; `,
		reviewEpoch0+i*10, i, i%10, i%7, (i+gen)%7)
	fmt.Fprintf(&b, `RemoteWallClockTime=%d.25; ResidentSetSize=%d; MemoryUsage=((ResidentSetSize + 1023) / 1024); `,
		i%5000, 1000+i%77)
	for j := range 16 {
		fmt.Fprintf(&b, "Pad%02d=%d; ", j, (i*(j+3))%1000)
	}
	if i%10 == 3 {
		fmt.Fprintf(&b, `VacateReasonCode=%d; VacateReason="reason%d"; `, (i/10+gen)%23, i%5)
	}
	if secret {
		fmt.Fprintf(&b, `ClaimId="secret-%d"; `, i)
	}
	b.WriteString("Gen=" + fmt.Sprint(gen) + " ]")
	return b.String()
}

type reviewFixture struct {
	t       *testing.T
	dir     string
	opts    Options
	c       *Collection
	ref     *Collection // plain in-memory copy: no accelerator, the ground truth
	n       int
	secret  bool
	patches bool
	require []string
}

func newReviewFixture(t *testing.T, n int, deltas, secret bool) *reviewFixture {
	t.Helper()
	if !mmapSupported {
		t.Skip("persistence is unix-only")
	}
	f := &reviewFixture{t: t, dir: t.TempDir(), n: n, secret: secret, patches: deltas}
	f.opts = Options{Dir: f.dir, Shards: 1, SegmentSize: 1 << 16, GroupSchemaCount: -1,
		ColumnarSegmentBudget: 1 << 20}
	if deltas {
		f.opts.DeltaMax = 16
	}
	if secret {
		_, key := deriveDataKey(t)
		f.opts.DataKey = key
		f.opts.EncryptedAttrs = []string{"ClaimId"}
	}
	c, err := Open(f.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.c = c
	f.ref = New(Options{Shards: 1})
	t.Cleanup(func() {
		_ = f.c.Close()
		_ = f.ref.Close()
	})
	for i := range n {
		f.put(i, 0)
		// With deltas on, patch an earlier ad as the load goes so delta records land in segments
		// that are sealed -- and later columnarized -- rather than only in the active one.
		if deltas && i%5 == 4 {
			f.patch(i-2, 1)
		}
	}
	return f
}

func (f *reviewFixture) put(i, gen int) {
	f.t.Helper()
	key := []byte(fmt.Sprintf("k%05d", i))
	ad := mustAd(f.t, reviewAd(i, gen, f.secret))
	tx := f.c.Begin()
	tx.SetDurable(false) // a sync per ad is seconds of fsync and tests nothing here; Close flushes
	tx.Put(key, ad)
	if r := tx.Commit(); r.Conflicted() {
		f.t.Fatal("conflict")
	}
	if err := f.ref.Put(key, mustAd(f.t, reviewAd(i, gen, f.secret))); err != nil {
		f.t.Fatal(err)
	}
}

// patch rewrites ad i as generation gen through a delta (only JobStatus/VacateReasonCode/Gen change).
func (f *reviewFixture) patch(i, gen int) {
	f.t.Helper()
	key := []byte(fmt.Sprintf("k%05d", i))
	ad := mustAd(f.t, reviewAd(i, gen, f.secret))
	tx := f.c.Begin()
	tx.SetDurable(false)
	tx.PutPatch(key, ad, []string{"JobStatus", "VacateReasonCode", "Gen"}, false)
	if r := tx.Commit(); r.Conflicted() {
		f.t.Fatal("conflict")
	}
	if err := f.ref.Put(key, mustAd(f.t, reviewAd(i, gen, f.secret))); err != nil {
		f.t.Fatal(err)
	}
}

func (f *reviewFixture) reopen() {
	f.t.Helper()
	if err := f.c.Close(); err != nil {
		f.t.Fatal(err)
	}
	c, err := Open(f.opts)
	if err != nil {
		f.t.Fatal(err)
	}
	f.c = c
	// The required set is the embedding application's to re-assert at every open (the db layer
	// persists and restores it); the collection keeps no copy of its own.
	if len(f.require) > 0 {
		f.c.AddSchemaRequireAttrs(f.require...)
	}
}

// payloads snapshots every sealed segment's columnar payload pointer, by segment id, so a pass's
// rewrites and rebuilds can be counted as the payloads that changed.
func payloads(c *Collection) map[uint32]*colSegment {
	out := map[uint32]*colSegment{}
	for _, sh := range c.shards {
		sh.mu.RLock()
		for _, seg := range sh.segs {
			if seg != nil && seg != sh.act && seg.used > 0 {
				out[seg.id] = seg.colblk.Load()
			}
		}
		sh.mu.RUnlock()
	}
	return out
}

func changedPayloads(before, after map[uint32]*colSegment) int {
	n := 0
	for id, cs := range after {
		// A segment that had no payload before was covered, not rebuilt: first coverage of a newly
		// sealed segment is not what the budget bounds.
		if old, ok := before[id]; ok && old != nil && old != cs {
			n++
		}
	}
	return n
}

// schemaHas reports whether the committed base schema carries attr.
func schemaHas(c *Collection, attr string) bool {
	st := c.schemaScan.Load()
	if st == nil {
		return false
	}
	id, ok := c.intern.LookupID(attr)
	return ok && st.schema.hasField(id)
}

// reviewQueries are the constraints every reader is checked under. The first is the production query.
var reviewQueries = []string{
	fmt.Sprintf("EpochWriteDate >= %d && VacateReasonCode isnt undefined", reviewEpoch0+20000),
	"VacateReasonCode == 5",
	"VacateReasonCode >= 3 && VacateReasonCode < 11",
	"VacateReasonCode is undefined",
	`VacateReason == "reason3"`,
	"JobStatus == 3 && ProcId < 4",
	"Pad03 > 500",
	"MemoryUsage > 1",
}

// canonAd renders an ad as its sorted attribute expressions, so two ads compare equal exactly when
// they hold the same attributes with the same expressions, in any order.
func canonAd(ad *classad.ClassAd) string {
	names := ad.GetAttributes()
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })
	var b strings.Builder
	for _, n := range names {
		e, _ := ad.Lookup(n)
		fmt.Fprintf(&b, "%s=%s;", strings.ToLower(n), e.String())
	}
	return b.String()
}

func reviewRefAds(t *testing.T, c *Collection, q *vm.Query) map[string]int {
	t.Helper()
	out := map[string]int{}
	for ad := range c.Query(q) {
		out[canonAd(ad)]++
	}
	return out
}

func reviewRefStats(rows []*classad.ClassAd, attr string) NumStats {
	out := NumStats{Min: math.Inf(1), Max: math.Inf(-1)}
	for _, ad := range rows {
		v, ok := ad.EvaluateAttrNumber(attr)
		if !ok {
			continue
		}
		out.N++
		out.Sum += v
		out.Min = math.Min(out.Min, v)
		out.Max = math.Max(out.Max, v)
	}
	return out
}

// checkReaders asserts every columnar reader agrees with the plain in-memory copy, for every query.
// A reader that declines (ok=false) is not a failure -- the caller would scan -- but one that answers
// must answer exactly.
func (f *reviewFixture) checkReaders(label string) {
	t := f.t
	t.Helper()
	c, ref := f.c, f.ref
	served := map[string]int{}
	for _, text := range reviewQueries {
		q, err := vm.Parse(text)
		if err != nil {
			t.Fatal(err)
		}
		// The plain copy's matches, read once and reused for every reader's expectation.
		var refRows []*classad.ClassAd
		for ad := range ref.Query(q) {
			refRows = append(refRows, ad)
		}
		want := map[string]int{}
		for _, ad := range refRows {
			want[canonAd(ad)]++
		}
		wantN := 0
		for _, k := range want {
			wantN += k
		}
		// Whole-ad scan: the prefilter decides from columns before reassembly.
		got := reviewRefAds(t, c, q)
		if len(got) != len(want) {
			t.Errorf("%s: %q: scan returned %d distinct ads, want %d", label, text, len(got), len(want))
		}
		for k, n := range want {
			if got[k] != n {
				t.Errorf("%s: %q: ad missing or wrong (want %d, got %d): %.160s", label, text, n, got[k], k)
				break
			}
		}
		for name, fn := range map[string]func(*vm.Query) (int, bool){
			"CountQuery": c.CountQuery, "VectorEvalCount": c.VectorEvalCount,
			"ColumnarEvalCount": c.ColumnarEvalCount, "PresenceCountQuery": c.PresenceCountQuery,
		} {
			if n, ok := fn(q); ok {
				served[name]++
				if n != wantN {
					t.Errorf("%s: %s(%q) = %d, want %d", label, name, text, n, wantN)
				}
			}
		}
		for _, attr := range []string{"VacateReasonCode", "EpochWriteDate", "RemoteWallClockTime"} {
			if got, ok := c.NumStatsQuery(q, attr); ok {
				served["NumStats"]++
				w := reviewRefStats(refRows, attr)
				if got.N != w.N || (w.N > 0 && (got.Min != w.Min || got.Max != w.Max ||
					math.Abs(got.Sum-w.Sum) > 1e-6*math.Max(1, math.Abs(w.Sum)))) {
					t.Errorf("%s: NumStats(%q, %s) = %+v, want N=%d Sum=%v Min=%v Max=%v",
						label, text, attr, got, w.N, w.Sum, w.Min, w.Max)
				}
			}
		}
		for _, attr := range []string{"VacateReasonCode", "JobStatus", "VacateReason"} {
			got, ok := c.GroupCountQuery(q, attr)
			if !ok {
				continue
			}
			served["GroupCount"]++
			wantG := map[string]int{}
			for _, ad := range refRows {
				wantG[ad.EvaluateAttr(attr).String()]++
			}
			gotG := map[string]int{}
			for _, g := range got {
				gotG[g.Value.String()] += g.Count
			}
			if fmt.Sprint(gotG) != fmt.Sprint(wantG) {
				t.Errorf("%s: GROUP BY %s WHERE %q = %v, want %v", label, attr, text, gotG, wantG)
			}
		}
		if got, ok := c.GroupStatsQuery(q, "JobStatus", []string{"VacateReasonCode", "EpochWriteDate"}); ok {
			served["GroupStats"]++
			type agg struct {
				n      int
				vn     int
				vs, es float64
			}
			want := map[string]*agg{}
			for _, ad := range refRows {
				k := ad.EvaluateAttr("JobStatus").String()
				a := want[k]
				if a == nil {
					a = &agg{}
					want[k] = a
				}
				a.n++
				if v, ok := ad.EvaluateAttrNumber("VacateReasonCode"); ok {
					a.vn++
					a.vs += v
				}
				if v, ok := ad.EvaluateAttrNumber("EpochWriteDate"); ok {
					a.es += v
				}
			}
			gotM := map[string]agg{}
			for _, g := range got {
				gotM[g.Values[0].String()] = agg{n: g.Count, vn: g.Stats[0].N, vs: g.Stats[0].Sum, es: g.Stats[1].Sum}
			}
			if len(gotM) != len(want) {
				t.Errorf("%s: GroupStats(%q) has %d groups, want %d", label, text, len(gotM), len(want))
			}
			for k, w := range want {
				if g := gotM[k]; g.n != w.n || g.vn != w.vn || g.vs != w.vs || g.es != w.es {
					t.Errorf("%s: GroupStats(%q) group %s = %+v, want %+v", label, text, k, g, *w)
				}
			}
		}
		for _, desc := range []bool{true, false} {
			th, seen, ok := c.TopKOrderThreshold(q, "VacateReasonCode", desc, 3, nil)
			if !ok {
				continue
			}
			served["TopK"]++
			var vals []float64
			for _, ad := range refRows {
				if v, ok := ad.EvaluateAttrNumber("VacateReasonCode"); ok {
					vals = append(vals, v)
				}
			}
			sort.Float64s(vals)
			if desc {
				sort.Sort(sort.Reverse(sort.Float64Slice(vals)))
			}
			if seen != len(vals) || (len(vals) >= 3 && th != vals[2]) {
				t.Errorf("%s: TopK(%q, desc=%v) = (%v, %d), want (k-th of %v, %d)", label, text, desc, th, seen, vals[:min(3, len(vals))], len(vals))
			}
		}
		// Projection: the columns-first path for a narrow read.
		proj := []string{"ClusterId", "VacateReasonCode", "VacateReason", "JobStatus"}
		gotP, wantP := map[string]int{}, map[string]int{}
		for row := range c.QueryProject(q, proj) {
			var parts []string
			for _, v := range row {
				parts = append(parts, v.String())
			}
			gotP[strings.Join(parts, "|")]++
		}
		for _, ad := range refRows {
			var parts []string
			for _, a := range proj {
				parts = append(parts, ad.EvaluateAttr(a).String())
			}
			wantP[strings.Join(parts, "|")]++
		}
		if g, w := gotP, wantP; fmt.Sprint(g) != fmt.Sprint(w) {
			t.Errorf("%s: QueryProject(%q) differs: %d rows vs %d", label, text, len(g), len(w))
		}
	}
	// Point reads, including keys whose records sit in segments of either schema.
	for i := 0; i < f.n; i += 97 {
		key := []byte(fmt.Sprintf("k%05d", i))
		got, ok1 := c.Get(key)
		want, ok2 := ref.Get(key)
		if ok1 != ok2 || (ok1 && canonAd(got) != canonAd(want)) {
			t.Errorf("%s: Get(%s) differs", label, key)
			break
		}
	}
	if served["CountQuery"] == 0 || served["NumStats"] == 0 || served["GroupCount"] == 0 {
		t.Errorf("%s: the columnar readers declined everything (%v): nothing was compared", label, served)
	}
}

// convergeUnderRequire drives the production scenario: narrow schema, attribute required, budgeted
// maintenance passes converge -- with every reader checked at every intermediate pass, a restart in
// the middle and one at the end.
func convergeUnderRequire(t *testing.T, deltas, secret bool) {
	f := newReviewFixture(t, 4000, deltas, secret)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	if schemaHas(f.c, "VacateReasonCode") {
		t.Fatal("fixture: VacateReasonCode is already a field at first enable")
	}
	sealed := len(payloads(f.c))
	if sealed < 12 {
		t.Fatalf("fixture: only %d sealed segments", sealed)
	}
	f.checkReaders("before")

	const budget = 4
	f.c.colBudget = budget
	f.opts.ColumnarSegmentBudget = budget
	f.require = []string{"VacateReasonCode", "MemoryUsage"}
	if !f.c.AddSchemaRequireAttrs(f.require...) {
		t.Fatal("require set did not change")
	}
	reopened := false
	passes := 0
	for pass := 1; ; pass++ {
		passes = pass
		if pass > sealed/budget+6 {
			t.Fatalf("did not converge within %d passes (stale=%d)", pass, f.c.staleSegmentCount(f.c.schemaScan.Load()))
		}
		before := payloads(f.c)
		f.c.BuildAndEnableSchemaScan(4096, 8)
		if !schemaHas(f.c, "VacateReasonCode") {
			t.Fatalf("pass %d: the required attribute was not adopted", pass)
		}
		if schemaHas(f.c, "MemoryUsage") {
			t.Fatalf("pass %d: expression-valued MemoryUsage became a field despite the refusal", pass)
		}
		if n := changedPayloads(before, payloads(f.c)); n > 2*budget {
			t.Fatalf("pass %d rewrote/rebuilt %d segments, budget is %d", pass, n, budget)
		}
		stale := f.c.staleSegmentCount(f.c.schemaScan.Load())
		f.checkReaders(fmt.Sprintf("pass %d (stale %d)", pass, stale))
		if deltas {
			// Writes while the schemas coexist, landing on keys whose old versions sit in segments of
			// both shapes.
			for i := pass; i < f.n; i += 173 {
				f.patch(i, pass+1)
			}
		}
		if !reopened && stale > 0 && stale < sealed-budget {
			// Restart mid-convergence: the reopened table must resume toward the NEW schema, not
			// re-adopt the old one from whichever segment it loads last.
			f.reopen()
			reopened = true
			if !schemaHas(f.c, "VacateReasonCode") {
				t.Fatal("reopen mid-convergence reverted to the old schema")
			}
			if got := f.c.staleSegmentCount(f.c.schemaScan.Load()); got != stale {
				t.Fatalf("reopen mid-convergence: %d stale segments, %d before the restart", got, stale)
			}
			f.checkReaders("after mid-convergence reopen")
		}
		if stale == 0 {
			break
		}
	}
	if !reopened {
		t.Fatal("fixture never reached a mid-convergence state to restart in")
	}
	t.Logf("%d sealed segments converged in %d passes at budget %d", sealed, passes, budget)
	// The expression-valued requirement is refused, and the refusal is what reports it.
	info := f.c.SchemaScanInfo()
	var skipped bool
	for _, s := range info.RequireSkipped {
		if strings.EqualFold(s.Name, "MemoryUsage") && s.Reason == requireSkipNoLiteral {
			skipped = true
		}
	}
	if !skipped {
		t.Errorf("MemoryUsage refusal not reported: %+v", info.RequireSkipped)
	}

	f.reopen()
	if !schemaHas(f.c, "VacateReasonCode") || f.c.staleSegmentCount(f.c.schemaScan.Load()) != 0 {
		t.Fatal("reopen after convergence: schema or segments not current")
	}
	f.checkReaders("after converged reopen")
	info = f.c.SchemaScanInfo()
	if len(info.BaseChanges) < 2 || !strings.Contains(info.BaseChanges[len(info.BaseChanges)-1].Reason, "VacateReasonCode") {
		t.Errorf("adoption not logged with its reason: %+v", info.BaseChanges)
	}
	// Steady: another pass adopts nothing and rewrites nothing.
	st := f.c.schemaScan.Load()
	nChanges := len(info.BaseChanges)
	before := payloads(f.c)
	f.c.BuildAndEnableSchemaScan(4096, 8)
	if f.c.schemaScan.Load().schema != st.schema {
		t.Error("a steady pass re-adopted the schema")
	}
	if n := changedPayloads(before, payloads(f.c)); n != 0 {
		t.Errorf("a steady pass rewrote %d segments", n)
	}
	if got := len(f.c.BaseSchemaChanges()); got != nChanges {
		t.Errorf("a steady pass logged %d change(s)", got-nChanges)
	}
}

// TestBaseSchemaConvergesUnderRequire is the production case: a table enabled with a narrow schema,
// then an attribute required. Maintenance adopts it and converges within budget; every reader agrees
// with a plain scan at every pass, across a restart mid-way and one at the end.
func TestBaseSchemaConvergesUnderRequire(t *testing.T) { convergeUnderRequire(t, false, false) }

// TestBaseSchemaConvergesWithDeltas is the same with delta records in the sealed segments and writes
// landing between passes.
func TestBaseSchemaConvergesWithDeltas(t *testing.T) { convergeUnderRequire(t, true, false) }

// TestBaseSchemaConvergesEncrypted is the same over an encrypted-at-rest attribute.
func TestBaseSchemaConvergesEncrypted(t *testing.T) { convergeUnderRequire(t, false, true) }

// reviewWires encodes n ads built by src into the id-keyed wire form the derivation reads.
func reviewWires(t *testing.T, c *Collection, n int, src func(i int) string) [][]byte {
	t.Helper()
	var out [][]byte
	for i := range n {
		ad, err := classad.ParseOld(src(i))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, c.encodeAd(ad.AST()))
	}
	return out
}

// reviewShape is the material-change fixture's ad: A, B, C always; W in wPct% of ads; X in xPct%
// (from a 1-byte range); Y in yPct%; and A outgrowing its width in bigPct% of ads.
func reviewShape(wPct, xPct, yPct, bigPct int) func(i int) string {
	return func(i int) string {
		a := i % 100
		if i%100 < bigPct {
			a = 1 << 40
		}
		s := fmt.Sprintf("A = %d\nB = %d\nC = \"c%d\"\n", a, i%7, i%3)
		if i%100 < wPct {
			s += fmt.Sprintf("W = %d\n", i%9)
		}
		if (i*7)%100 < xPct {
			s += fmt.Sprintf("X = %d\n", i%50)
		}
		if (i*13)%100 < yPct {
			s += fmt.Sprintf("Y = %d\n", i%11)
		}
		return s
	}
}

// reviewState derives a schema from samples and commits it as the collection's state, as a first
// enable would, without any segment.
func reviewState(t *testing.T, c *Collection, samples [][]byte) *schemaScanState {
	t.Helper()
	s, hot, _, ok := c.deriveSchemaFrom(samples, 8)
	if !ok {
		t.Fatal("derivation produced no schema")
	}
	st := &schemaScanState{schema: s, hot: hot}
	c.schemaScan.Store(st)
	return st
}

// TestReviewBaseSchemaMateriality pins down when a re-derivation is adopted: each material change
// on its own, and the near-floor changes the hysteresis exists to ignore.
func TestReviewBaseSchemaMateriality(t *testing.T) {
	const n = 2000
	c := New(Options{Shards: 1})
	defer c.Close()
	st := reviewState(t, c, reviewWires(t, c, n, reviewShape(100, 10, 0, 0)))
	for _, a := range []string{"A", "B", "C", "W"} {
		if !schemaHas(c, a) {
			t.Fatalf("fixture: %s is not a field", a)
		}
	}
	if schemaHas(c, "X") {
		t.Fatal("fixture: X is a field at 10% presence")
	}
	review := func(label string, samples [][]byte, wantAdopt bool, wantReason string) *adSchema {
		t.Helper()
		s, _, reason, ok := c.reviewBaseSchema(st, samples, 8)
		if ok != wantAdopt {
			t.Errorf("%s: adopted=%v (reason %q), want %v", label, ok, reason, wantAdopt)
		}
		if ok && !strings.Contains(reason, wantReason) {
			t.Errorf("%s: reason %q does not mention %q", label, reason, wantReason)
		}
		return s
	}

	review("steady", reviewWires(t, c, n, reviewShape(100, 10, 0, 0)), false, "")

	c.AddSchemaRequireAttrs("X")
	review("required", reviewWires(t, c, n, reviewShape(100, 10, 0, 0)), true, "required attribute X")
	c.DropSchemaRequireAttrs("X")

	// Y clears the floor (90%) without clearing the adoption bar (95%), then clears both.
	review("promote near floor", reviewWires(t, c, n, reviewShape(100, 10, 93, 0)), false, "")
	review("promote", reviewWires(t, c, n, reviewShape(100, 10, 98, 0)), true, "Y present")

	// W falls below the floor without falling below the drop bar (80%), then falls below both.
	review("demote near floor", reviewWires(t, c, n, reviewShape(85, 10, 0, 0)), false, "")
	review("demote", reviewWires(t, c, n, reviewShape(60, 10, 0, 0)), true, "W in its slot")

	// A outgrows its width in a third of the ads; the re-derivation widens it.
	review("escape", reviewWires(t, c, n, reviewShape(100, 10, 0, 30)), true, "A escapes")
}

// TestReviewKeepsRequiredFieldAbsentFromSample: a required field the sample happens to miss is
// neither a reason to adopt nor dropped by an adoption for another reason -- otherwise a rare
// required attribute flips in and out of the schema with the sample, rewriting the table each time.
func TestReviewKeepsRequiredFieldAbsentFromSample(t *testing.T) {
	const n = 2000
	c := New(Options{Shards: 1})
	defer c.Close()
	c.AddSchemaRequireAttrs("X")
	st := reviewState(t, c, reviewWires(t, c, n, reviewShape(100, 10, 0, 0)))
	if !schemaHas(c, "X") {
		t.Fatal("fixture: required X is not a field")
	}
	noX := reviewWires(t, c, n, reviewShape(100, 0, 0, 0))
	if _, _, reason, ok := c.reviewBaseSchema(st, noX, 8); ok {
		t.Errorf("a sample without the required X triggered an adoption: %s", reason)
	}
	xID, _ := c.intern.LookupID("X")
	s, _, _, ok := c.reviewBaseSchema(st, reviewWires(t, c, n, reviewShape(100, 0, 98, 0)), 8)
	if !ok {
		t.Fatal("Y at 98% was not adopted")
	}
	if !s.hasField(xID) {
		t.Error("adopting for Y dropped the required X the sample did not contain")
	}
}

// TestReviewWaitsForConvergence: while segments are still stale against the committed state, a drift
// signal is not acted on, but a required attribute still is.
func TestReviewWaitsForConvergence(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 12})
	defer c.Close()
	for i := range 3000 {
		ad, err := classad.ParseOld(reviewShape(100, 10, 0, 0)(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no enable")
	}
	st := c.schemaScan.Load()
	if len(st.hot) == 0 {
		t.Fatal("fixture: no hot tier")
	}
	if c.staleSegmentCount(st) != 0 {
		t.Fatal("fixture: stale segments right after enable")
	}
	// Commit a state every segment is stale against.
	st = &schemaScanState{schema: st.schema, hot: st.hot[:len(st.hot)-1], cache: st.cache}
	c.schemaScan.Store(st)
	if c.staleSegmentCount(st) == 0 {
		t.Fatal("fixture: no stale segment")
	}
	drift := reviewWires(t, c, 2000, reviewShape(100, 10, 98, 0))
	if _, _, reason, ok := c.reviewBaseSchema(st, drift, 8); ok {
		t.Errorf("adopted a drift change mid-convergence: %s", reason)
	}
	c.AddSchemaRequireAttrs("X")
	if _, _, _, ok := c.reviewBaseSchema(st, drift, 8); !ok {
		t.Error("a required attribute waited for convergence")
	}
}

// TestReschemaScanConvergesColumnarized: `.schema rebuild` on a columnar-native table adopts the new
// schema AND brings the columnarized segments onto it -- one budget synchronously, the rest through
// maintenance. It used to skip them, leaving every existing segment on the old schema for good.
func TestReschemaScanConvergesColumnarized(t *testing.T) {
	f := newReviewFixture(t, 3000, false, false)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	const budget = 3
	f.c.colBudget = budget
	f.c.AddSchemaRequireAttrs("VacateReasonCode")
	before := payloads(f.c)
	if !f.c.ReschemaScan(4096, 8) {
		t.Fatal("ReschemaScan did not run")
	}
	if !schemaHas(f.c, "VacateReasonCode") {
		t.Fatal("ReschemaScan did not adopt the required attribute")
	}
	if n := changedPayloads(before, payloads(f.c)); n == 0 || n > budget {
		t.Fatalf("ReschemaScan rewrote %d columnarized segments synchronously, want 1..%d", n, budget)
	}
	f.checkReaders("after rebuild")
	for pass := 0; f.c.staleSegmentCount(f.c.schemaScan.Load()) > 0; pass++ {
		if pass > len(before) {
			t.Fatal("maintenance did not converge the rebuild")
		}
		f.c.BuildAndEnableSchemaScan(4096, 8)
	}
	f.checkReaders("after convergence")
}

// TestBaseSchemaCrashMidRewrite: a crash while a segment is being rewritten onto the new schema --
// staged, not yet committed -- recovers to a store that reads correctly, still commits the new
// schema, and converges from there.
func TestBaseSchemaCrashMidRewrite(t *testing.T) {
	f := newReviewFixture(t, 3000, true, false)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	f.c.colBudget = 3
	f.require = []string{"VacateReasonCode"}
	f.c.AddSchemaRequireAttrs(f.require...)
	f.c.BuildAndEnableSchemaScan(4096, 8) // adopt + the first budget
	var img string
	colCommitStallHook = func() {
		if img == "" {
			img = liveImage(t, f.c, f.dir)
		}
	}
	f.c.BuildAndEnableSchemaScan(4096, 8)
	colCommitStallHook = nil
	if img == "" {
		t.Fatal("no rewrite reached its commit")
	}
	live := f.c
	o := f.opts
	o.Dir = img
	o.ColumnarSegmentBudget = 3
	rec, _ := openRecovered(t, o)
	f.c = rec
	defer func() { f.c = live }()
	f.c.AddSchemaRequireAttrs(f.require...)
	if !schemaHas(f.c, "VacateReasonCode") {
		t.Fatal("crash image reopened without the committed schema")
	}
	if f.c.staleSegmentCount(f.c.schemaScan.Load()) == 0 {
		t.Fatal("fixture: crash image is already converged")
	}
	f.checkReaders("crash image")
	for pass := 0; f.c.staleSegmentCount(f.c.schemaScan.Load()) > 0; pass++ {
		if pass > 100 {
			t.Fatal("recovered store did not converge")
		}
		f.c.BuildAndEnableSchemaScan(4096, 8)
	}
	f.checkReaders("crash image converged")
	if err := rec.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestColumnarCountAfterReopenRemnantOnlyAttr: after a restart, an attribute that lives only in the
// remnants of columnarized segments (an expression, which never moves into a column) is not in the
// intern table -- and that is not proof that no record has it. The column readers used to answer
// UNDEFINED for every sealed record, so `MemoryUsage > 1` counted 46 of 4050 after a reopen.
func TestColumnarCountAfterReopenRemnantOnlyAttr(t *testing.T) {
	f := newReviewFixture(t, 3000, false, false)
	if !f.c.BuildAndEnableSchemaScan(4096, 8) {
		t.Fatal("schema scan did not enable")
	}
	f.reopen()
	q, err := vm.Parse("MemoryUsage > 1")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for range f.ref.Query(q) {
		want++
	}
	for name, fn := range map[string]func(*vm.Query) (int, bool){
		"CountQuery": f.c.CountQuery, "VectorEvalCount": f.c.VectorEvalCount, "ColumnarEvalCount": f.c.ColumnarEvalCount,
	} {
		if n, ok := fn(q); ok && n != want {
			t.Errorf("%s = %d after reopen, want %d", name, n, want)
		}
	}
}

// TestColScopeGroupBindingPerSegment: one scope walks every segment of a scan, and segments built
// under different group sets put an attribute in a different group, or in none. The cached binding
// must follow the segment, not the first one the scan met.
func TestColScopeGroupBindingPerSegment(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	x := c.intern.Intern("X")
	h := c.intern.Intern("H")
	g := func(ids ...uint32) *colGroup {
		var fs []adField
		for _, id := range ids {
			fs = append(fs, adField{id: id, kind: akInt, width: 8})
		}
		return &colGroup{schema: layoutSchema(fs), ids: ids}
	}
	segA := &colSegment{groups: []*colGroup{g(h)}}
	segB := &colSegment{groups: []*colGroup{g(h), g(x)}}
	cs := &colScope{c: c}
	cs.setGroups(segA, 0)
	if _, ok := cs.bindGroupField("X"); ok {
		t.Fatal("X bound to a group in a segment where no group carries it")
	}
	cs.setGroups(segB, 0)
	b, ok := cs.bindGroupField("X")
	if !ok || b.gi != 1 {
		t.Fatalf("X in segment B bound to (%v, %+v), want group 1", ok, b)
	}
	cs.setGroups(segA, 0)
	if _, ok := cs.bindGroupField("X"); ok {
		t.Fatal("X kept segment B's group binding in segment A")
	}
}

// TestBuildPairsGroupsWithItsBase: a block is built with the committed group set only when it is
// built under the committed base schema, since a group set is derived against one base.
func TestBuildPairsGroupsWithItsBase(t *testing.T) {
	c := New(Options{Shards: 1})
	defer c.Close()
	base := layoutSchema([]adField{{id: c.intern.Intern("A"), kind: akInt, width: 8}})
	other := layoutSchema([]adField{{id: c.intern.Intern("A"), kind: akInt, width: 8}})
	grp := []*colGroup{{schema: layoutSchema([]adField{{id: c.intern.Intern("Y"), kind: akInt, width: 8}})}}
	c.schemaScan.Store(&schemaScanState{schema: base, groups: grp})
	if got := c.groupsFor(base); len(got) != 1 {
		t.Errorf("the committed base got %d groups, want 1", len(got))
	}
	if got := c.groupsFor(other); got != nil {
		t.Errorf("a base other than the committed one got the committed groups: %d", len(got))
	}
}

// TestSchemaReviewGate: a pass re-derives the base schema only when the required set changed or the
// review interval has passed; otherwise a steady pass samples nothing.
func TestSchemaReviewGate(t *testing.T) {
	c := New(Options{Shards: 1, SegmentSize: 1 << 14})
	defer c.Close()
	for i := range 2000 {
		ad, err := classad.ParseOld(reviewShape(100, 10, 0, 0)(i))
		if err != nil {
			t.Fatal(err)
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no enable")
	}
	reviews := func() int64 {
		n := schemaReviews.Load()
		c.BuildAndEnableSchemaScan(2000, 8)
		return schemaReviews.Load() - n
	}
	for i := range 3 {
		if n := reviews(); n != 0 {
			t.Fatalf("steady pass %d re-derived the schema %d time(s)", i, n)
		}
	}
	c.AddSchemaRequireAttrs("X")
	if n := reviews(); n != 1 {
		t.Fatalf("a changed required set was reviewed %d times on the next pass, want 1", n)
	}
	if !schemaHas(c, "X") {
		t.Fatal("the required X was not adopted")
	}
	if n := reviews(); n != 0 {
		t.Fatalf("the pass after the review re-derived %d times", n)
	}
	defer func(d time.Duration) { schemaReviewInterval = d }(schemaReviewInterval)
	schemaReviewInterval = 0
	if n := reviews(); n != 1 {
		t.Fatalf("an elapsed interval was reviewed %d times, want 1", n)
	}
}

// TestBaseSchemaDriftConvergesSidecars: an in-memory table, whose accelerator is sidecar blocks only,
// takes in an attribute that has become common, rebuilds its blocks onto the new schema a budget at
// a time, and answers exactly at every pass.
func TestBaseSchemaDriftConvergesSidecars(t *testing.T) {
	defer func(d time.Duration) { schemaReviewInterval = d }(schemaReviewInterval)
	schemaReviewInterval = 0
	c := New(Options{Shards: 1, SegmentSize: 1 << 13, ColumnarSegmentBudget: 1})
	ref := New(Options{Shards: 1})
	defer c.Close()
	defer ref.Close()
	put := func(i int, src string) {
		for _, x := range []*Collection{c, ref} {
			ad, err := classad.ParseOld(src)
			if err != nil {
				t.Fatal(err)
			}
			if err := x.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Few old ads and many new ones: the sample spans both, and Y has to clear the adoption bar
	// (95%) across it.
	for i := range 400 {
		put(i, reviewShape(100, 10, 0, 0)(i))
	}
	if !c.BuildAndEnableSchemaScan(2000, 8) {
		t.Fatal("no enable")
	}
	if schemaHas(c, "Y") {
		t.Fatal("fixture: Y is a field")
	}
	for i := 400; i < 14400; i++ {
		put(i, reviewShape(100, 10, 100, 0)(i))
	}
	q, err := vm.Parse("Y > 3")
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for range ref.Query(q) {
		want++
	}
	served := 0
	for pass := 1; ; pass++ {
		if pass > 200 {
			t.Fatal("did not converge")
		}
		before := payloads(c)
		c.BuildAndEnableSchemaScan(2000, 8)
		if n := changedPayloads(before, payloads(c)); n > 1 {
			t.Fatalf("pass %d rebuilt %d sidecar blocks, budget 1", pass, n)
		}
		for name, fn := range map[string]func(*vm.Query) (int, bool){
			"CountQuery": c.CountQuery, "VectorEvalCount": c.VectorEvalCount, "ColumnarEvalCount": c.ColumnarEvalCount,
		} {
			if n, ok := fn(q); ok {
				served++
				if n != want {
					t.Fatalf("pass %d: %s = %d, want %d", pass, name, n, want)
				}
			}
		}
		if schemaHas(c, "Y") && c.staleSegmentCount(c.schemaScan.Load()) == 0 {
			if pass < 2 {
				t.Fatalf("converged in %d pass: the fixture never had stale blocks to rebuild across passes", pass)
			}
			t.Logf("converged in %d passes", pass)
			break
		}
	}
	if served == 0 {
		t.Fatal("no columnar reader answered")
	}
	if ch := c.BaseSchemaChanges(); ch != nil {
		t.Fatalf("an in-memory table has no change log to keep, got %d entries", len(ch))
	}
}
