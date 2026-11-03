package collections

import (
	"fmt"
	"sync"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// recordValueAttrs are the fixture attributes the per-record evaluation is checked on. They cover
// every shape it has to get right: an expression over another attribute (MemUse, whose own source Rss
// is itself an expression in some records and absent in others), a string expression (Label), a real
// (Ratio), a boolean (Flag), integers past 2^53 (Big), literal-valued attributes (Owner, QDate,
// RssBase), one it must refuse (Age, on the clock), one that evaluates to ERROR (Loop, cyclic in a few
// records) and one most records do not have (Rss).
var recordValueAttrs = []string{"MemUse", "Label", "Ratio", "Flag", "Big", "Loop", "Age",
	"Owner", "QDate", "RssBase", "Rss", "ClusterId", "Absent"}

// recordValueNodeStr renders a value node as a comparable string.
func recordValueNodeStr(node []byte) string {
	lit, ok := wire.LiteralValue(node)
	if !ok {
		return "<not a literal>"
	}
	switch lit.Kind {
	case wire.LitUndef:
		return "undefined"
	case wire.LitError:
		return "error"
	case wire.LitBool:
		return fmt.Sprintf("bool:%v", lit.Bool)
	case wire.LitInt:
		return fmt.Sprintf("int:%d", lit.Int)
	case wire.LitReal:
		return fmt.Sprintf("real:%v", lit.Real)
	case wire.LitString:
		return fmt.Sprintf("str:%q", lit.Str)
	}
	return "<unknown>"
}

// evaluatedStr renders the value the WHOLE ad gives for name the same way, so the two can be compared
// directly. A list, a record or an unrepresentable value renders as something no node can equal, which
// is correct: recordValueNode must refuse those rather than return them.
func evaluatedStr(ad *classad.ClassAd, name string) string {
	v := ad.EvaluateAttr(name)
	switch {
	case v.IsUndefined():
		return "undefined"
	case v.IsError():
		return "error"
	case v.IsBool():
		b, err := v.BoolValue()
		if err != nil {
			return "<bool?>"
		}
		return fmt.Sprintf("bool:%v", b)
	case v.IsInteger():
		i, err := v.IntValue()
		if err != nil {
			return "<int?>"
		}
		return fmt.Sprintf("int:%d", i)
	case v.IsReal():
		f, err := v.RealValue()
		if err != nil {
			return "<real?>"
		}
		return fmt.Sprintf("real:%v", f)
	case v.IsString():
		s, err := v.StringValue()
		if err != nil {
			return "<str?>"
		}
		return fmt.Sprintf("str:%q", s)
	}
	return "<other>"
}

// recordValuePremise counts what a differential run actually exercised, so a fixture that stopped
// reaching the path under test fails instead of passing vacuously.
type recordValuePremise struct {
	answered  int // recordValueNode returned a value
	computed  int // ...for an attribute stored as an expression
	literal   int // ...for one stored as a literal
	refused   int // recordValueNode declined
	undefined int // ...and answered UNDEFINED for an attribute the record lacks
}

func (p *recordValuePremise) check(t *testing.T, when string) {
	t.Helper()
	if p.computed == 0 {
		t.Fatalf("%s: no attribute under test was stored as an expression -- the per-record evaluation never ran", when)
	}
	if p.literal == 0 {
		t.Fatalf("%s: no attribute under test was stored as a literal", when)
	}
	if p.refused == 0 {
		t.Fatalf("%s: nothing was refused -- the fixture no longer holds a value this path must decline", when)
	}
	if p.undefined == 0 {
		t.Fatalf("%s: no record was missing an attribute under test", when)
	}
}

// diffRecord compares recordValueNode's answer for every attribute of one record against evaluating
// the whole ad, and records what the comparison exercised.
func diffRecord(t *testing.T, c *Collection, d *segDictHandle, a wire.Ad, ad *classad.ClassAd, p *recordValuePremise, when string) {
	t.Helper()
	for _, name := range recordValueAttrs {
		node, ok := c.recordValueNode(d, a, name)
		stored, found := c.wireLookupDict(d, a, name)
		switch {
		case !ok:
			p.refused++
			continue
		case !found:
			p.undefined++
		case isExprNode(stored):
			p.computed++
		default:
			p.literal++
		}
		p.answered++
		got, want := recordValueNodeStr(node), evaluatedStr(ad, name)
		if got != want {
			t.Fatalf("%s: %s of %v: per-record evaluation gave %s, evaluating the whole ad gives %s",
				when, name, ad.EvaluateAttr("ClusterId"), got, want)
		}
	}
}

// recordValueFixtureAds are the ads the direct differential runs over: the computed fixture's shapes,
// plus a couple that pin the closure walk itself (a chain, and a reference to an absent attribute).
func recordValueFixtureAds(tb testing.TB, n int) []*classad.ClassAd {
	tb.Helper()
	ads := make([]*classad.ClassAd, 0, n+2)
	for i := range n {
		ads = append(ads, mustAd(tb, computedAd(i, 97)))
	}
	ads = append(ads,
		mustAd(tb, `[ ClusterId = 90001; A = 1; B = A + 1; C = B + 1; MemUse = C * 10; Owner = "chain" ]`),
		mustAd(tb, `[ ClusterId = 90002; MemUse = Missing + 1; Owner = "dangling" ]`),
		// References whose casing differs from the attribute's, and the same attribute referenced
		// twice under two casings: attribute names are case-insensitive, so the closure must resolve
		// both and the value must not depend on which casing was written.
		mustAd(tb, `[ ClusterId = 90003; RSSBASE = 7; MemUse = rssbase + RssBase; Label = toUpper(owner); Owner = "mixed" ]`))
	return ads
}

// TestRecordValueNodeMatchesWholeAd is the differential: for every record of every stored form, the
// value recordValueNode returns for an attribute -- computed or literal -- is the value evaluating the
// whole ad gives for it. That is the contract the per-record exception rests on, and the only thing
// that makes it safe to answer a columnar query from it.
func TestRecordValueNodeMatchesWholeAd(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		open func(t *testing.T) *Collection
	}{
		{"interned-memory", func(t *testing.T) *Collection { return New(Options{}) }},
		{"inline-persistent", func(t *testing.T) *Collection {
			c, err := Open(Options{Dir: t.TempDir(), Shards: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close() })
			return c
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := tc.open(t)
			var p recordValuePremise
			for _, ad := range recordValueFixtureAds(t, 400) {
				w := mustEncodeAd(c.encodeAd(ad.AST()))
				diffRecord(t, c, nil, wire.Ad(w), ad, &p, tc.name)
			}
			p.check(t, tc.name)
		})
	}
}

// TestRecordValueNodeMatchesWholeAdInterned is the same differential over an interned archive's SEALED
// segments, where a record is keyed by its segment's own dictionary and recordValueNode is handed that
// dictionary. The names in the record are segment-local ids, so this is the tier a name-keyed lookup
// would silently miss.
func TestRecordValueNodeMatchesWholeAdInterned(t *testing.T) {
	c := buildComputedFixtureOpts(t, t.TempDir(), 2000, 97, true, true)
	var p recordValuePremise
	segs := 0
	for _, sh := range c.shards {
		for _, seg := range sh.segs {
			cn := seg.colNative.Load()
			dict := seg.dict.Load()
			if cn == nil || dict == nil {
				continue
			}
			segs++
			for i := range cn.seg.offsLen() {
				w, err := c.recordWire(seg, cn.seg.offAt(i))
				if err != nil {
					t.Fatal(err)
				}
				a, err := c.decodeWireDict(dict, w)
				if err != nil {
					t.Fatal(err)
				}
				diffRecord(t, c, dict, wire.Ad(w), classad.FromAST(a), &p, "interned segment")
			}
		}
	}
	if segs == 0 {
		t.Fatal("no sealed segment carries a dictionary: the dictionary-keyed tier was never exercised")
	}
	p.check(t, "interned segment")
}

// TestRecordValueNodeScratchIsNotShared: the state one per-record evaluation reuses must not carry an
// attribute into the next. Two records name the same attributes and give them different values, and
// one of them lacks the attribute the other's expression reads -- so a scratch that kept the previous
// record's attributes would answer 6 where the record itself says UNDEFINED.
func TestRecordValueNodeScratchIsNotShared(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	full := mustAd(t, `[ X = Y + 1; Y = 5 ]`)
	bare := mustAd(t, `[ X = Y + 1 ]`)
	wFull := mustEncodeAd(c.encodeAd(full.AST()))
	wBare := mustEncodeAd(c.encodeAd(bare.AST()))
	for i := range 50 {
		for _, tc := range []struct {
			w    []byte
			want string
		}{{wFull, "int:6"}, {wBare, "undefined"}} {
			node, ok := c.recordValueNode(nil, wire.Ad(tc.w), "X")
			if !ok {
				t.Fatalf("round %d: recordValueNode declined X", i)
			}
			if got := recordValueNodeStr(node); got != tc.want {
				t.Fatalf("round %d: X = %s, want %s -- the previous record's attributes leaked", i, got, tc.want)
			}
		}
	}
}

// TestRecordValueNodeConcurrent runs the differential from several goroutines at once, since the state
// a per-record evaluation reuses is shared through a pool. Run under -race.
func TestRecordValueNodeConcurrent(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	ads := recordValueFixtureAds(t, 200)
	wires := make([][]byte, len(ads))
	for i, ad := range ads {
		wires[i] = mustEncodeAd(c.encodeAd(ad.AST()))
	}
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var p recordValuePremise
			for i, ad := range ads {
				diffRecord(t, c, nil, wire.Ad(wires[i]), ad, &p, fmt.Sprintf("goroutine %d", g))
			}
			if p.computed == 0 {
				t.Errorf("goroutine %d evaluated no expression-valued attribute", g)
			}
		}()
	}
	wg.Wait()
}

// TestRecordValueNodeRefusesSealed: a sealed attribute's value is never served by this path, whether
// it is the attribute asked for or one the asked-for attribute reads. Sealing is by policy, so the
// refusal must hold for a record written before the attribute was sealed (its value stored in the
// clear) as much as for a record whose node is encrypted.
func TestRecordValueNodeRefusesSealed(t *testing.T) {
	t.Parallel()
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 11)
	}
	open := func(opts Options) *Collection {
		opts.Dir, opts.Shards = t.TempDir(), 1
		c, err := Open(opts)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	plain := open(Options{})
	sealed := open(Options{DataKey: key, EncryptedAttrs: []string{"Rss"}})
	ad := mustAd(t, `[ ClusterId = 1; Rss = 4096; MemUse = ((Rss + 1023) / 1024); Owner = "u" ]`)

	// The premise: without sealing, both the sealed attribute and the one that reads it answer.
	for _, name := range []string{"Rss", "MemUse"} {
		w := mustEncodeAd(plain.encodeAd(ad.AST()))
		if _, ok := plain.recordValueNode(nil, wire.Ad(w), name); !ok {
			t.Fatalf("unsealed: %s was refused, so refusing it under a key proves nothing", name)
		}
	}
	// Encrypted node: the attribute itself and its reader are both refused.
	encrypted := mustEncodeAd(sealed.encodeAd(ad.AST()))
	if !wire.IsEncryptedNode(mustLookup(t, sealed, encrypted, "Rss")) {
		t.Fatal("fixture: Rss was not sealed in the record")
	}
	for _, name := range []string{"Rss", "MemUse"} {
		if node, ok := sealed.recordValueNode(nil, wire.Ad(encrypted), name); ok {
			t.Fatalf("sealed: %s was served as %s", name, recordValueNodeStr(node))
		}
	}
	// Sealed in the record but NOT named by the reader's policy -- the set was narrowed after the
	// record was written. The reader holds the key, so nothing stops it from opening the node; what
	// must stop it is the node's own encrypted tag. Without that check this path would hand a sealed
	// value to a reader the sealing is there to withhold it from.
	narrowed := open(Options{DataKey: key})
	if narrowed.shouldEncrypt("Rss") {
		t.Fatal("fixture: the narrowed reader still seals Rss by policy")
	}
	if expr, err := narrowed.decodeNodeDict(nil, mustLookup(t, sealed, encrypted, "Rss")); err != nil || expr == nil {
		t.Fatalf("fixture: the narrowed reader cannot open the node (%v), so refusing it proves nothing", err)
	}
	for _, name := range []string{"Rss", "MemUse"} {
		if node, ok := narrowed.recordValueNode(nil, wire.Ad(encrypted), name); ok {
			t.Fatalf("sealed in the record, not named by the reader's policy: %s was served as %s", name, recordValueNodeStr(node))
		}
	}
	// Stored in the clear, sealed by policy only: still refused, both of them.
	legacy := mustEncodeAd(plain.encodeAd(ad.AST()))
	for _, name := range []string{"Rss", "MemUse"} {
		if node, ok := sealed.recordValueNode(nil, wire.Ad(legacy), name); ok {
			t.Fatalf("sealed by policy, stored in the clear: %s was served as %s", name, recordValueNodeStr(node))
		}
	}
}

func mustLookup(t *testing.T, c *Collection, w []byte, name string) []byte {
	t.Helper()
	node, ok := c.wireLookupDict(nil, wire.Ad(w), name)
	if !ok {
		t.Fatalf("%s is not in the record", name)
	}
	return node
}

// TestRecordValueNodeClosureBound: a closure wider than computedClosureMax is refused rather than
// walked, which is what keeps one record's evaluation bounded.
func TestRecordValueNodeClosureBound(t *testing.T) {
	t.Parallel()
	c := New(Options{})
	build := func(n int) *classad.ClassAd {
		src := "[ A0 = 1"
		for i := 1; i <= n; i++ {
			src += fmt.Sprintf("; A%d = A%d + 1", i, i-1)
		}
		return mustAd(t, src+" ]")
	}
	narrow := build(computedClosureMax - 2)
	wNarrow := mustEncodeAd(c.encodeAd(narrow.AST()))
	top := fmt.Sprintf("A%d", computedClosureMax-2)
	node, ok := c.recordValueNode(nil, wire.Ad(wNarrow), top)
	if !ok {
		t.Fatalf("a closure of %d attributes was refused", computedClosureMax-1)
	}
	if got, want := recordValueNodeStr(node), evaluatedStr(narrow, top); got != want {
		t.Fatalf("%s = %s, want %s", top, got, want)
	}
	wide := build(computedClosureMax + 8)
	wWide := mustEncodeAd(c.encodeAd(wide.AST()))
	if _, ok := c.recordValueNode(nil, wire.Ad(wWide), fmt.Sprintf("A%d", computedClosureMax+8)); ok {
		t.Fatal("a closure past computedClosureMax was walked instead of refused")
	}
}

// benchRecordWires encodes the computed fixture's ads into this collection's key space, the wire form
// a row walk hands recordValueNode.
func benchRecordWires(tb testing.TB, c *Collection, n int) [][]byte {
	tb.Helper()
	out := make([][]byte, 0, n)
	for i := range n {
		ad := mustAd(tb, computedAd(i, 0))
		out = append(out, mustEncodeAd(c.encodeAd(ad.AST())))
	}
	return out
}

// BenchmarkRecordValueNode measures the per-record evaluation a row walk falls to for an
// expression-valued attribute: one attribute of one record, its closure decoded and evaluated.
func BenchmarkRecordValueNode(b *testing.B) {
	c := New(Options{})
	wires := benchRecordWires(b, c, 512)
	// The premise: the path must answer, or the benchmark measures a refusal.
	if _, ok := c.recordValueNode(nil, wire.Ad(wires[0]), "MemUse"); !ok {
		b.Fatal("recordValueNode declined the fixture's MemUse: the path under test never runs")
	}
	for _, name := range []string{"MemUse", "Label", "Ratio", "Flag"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; b.Loop(); i++ {
				c.recordValueNode(nil, wire.Ad(wires[i%len(wires)]), name)
			}
		})
	}
}

// BenchmarkScanColumnsComputed is the end-to-end read the per-record evaluation is a tier of: a
// columnar scan projecting attributes most records compute. Most rows come from the columns; the rows
// of the segment still being written, and the cells no block could compute, fall to recordValueNode.
func BenchmarkScanColumnsComputed(b *testing.B) {
	c := buildComputedFixture(b, b.TempDir(), 20000, 97, false)
	q, err := vm.Parse("ProcId >= 0")
	if err != nil {
		b.Fatal(err)
	}
	attrs := []string{"ClusterId", "MemUse", "Label", "Ratio", "Flag"}
	// The premise: the scan must be servable AND must fall to the per-record tier, or this measures a
	// pure column read that the change cannot touch.
	evals := heldExceptionEvals.Load()
	rows := 0
	if _, ok := c.ScanColumns(q, attrs, func(bt *Batch) bool { rows += bt.N; return true }); !ok {
		b.Fatal("ScanColumns gave the query back")
	}
	if rows == 0 {
		b.Fatal("the scan produced no rows")
	}
	if heldExceptionEvals.Load() == evals {
		b.Fatal("no row fell to the per-record evaluation: the tier under test never runs")
	}
	b.ReportAllocs()
	for b.Loop() {
		c.ScanColumns(q, attrs, func(bt *Batch) bool { return true })
	}
}
