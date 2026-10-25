package collections

import (
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/collections/vm"
	"github.com/PelicanPlatform/classad/collections/wire"
)

// nanSchemaFixture builds a schema whose R field is a real column, and returns the collection's
// intern table id for R alongside it. The sample is all ordinary finite reals so the dominant kind
// is unambiguously akReal and the field's slot is 8 bytes -- the point of the tests below is what
// happens to an exceptional value in a column that is otherwise perfectly ordinary.
func nanSchemaFixture(t *testing.T, c *Collection) (*adSchema, uint32, int) {
	t.Helper()
	var sample [][]byte
	for i := range 64 {
		ad, err := classad.Parse(fmt.Sprintf("[ R = %d.5; K = %d ]", i, i))
		if err != nil {
			t.Fatal(err)
		}
		sample = append(sample, wire.Encode(nil, ad.AST(), c.intern))
	}
	s := buildAdSchema(sample, adSchemaOpts{Presence: 0.5, Fit: 0.95})
	id, ok := c.intern.LookupID("R")
	if !ok {
		t.Fatal("R was never interned")
	}
	idx, ok := s.byID[id]
	if !ok {
		t.Fatal("the schema has no R field; the fixture proves nothing")
	}
	if s.fields[idx].kind != akReal {
		t.Fatalf("R field kind = %d, want akReal", s.fields[idx].kind)
	}
	return s, id, idx
}

// nanAdText builds `[ R = <text> ]`, for the values classad text can spell.
func nanAdText(t *testing.T, text string) func() *classad.ClassAd {
	return func() *classad.ClassAd {
		t.Helper()
		ad, err := classad.Parse("[ R = " + text + " ]")
		if err != nil {
			t.Fatal(err)
		}
		return ad
	}
}

// nanAdFloat builds the same ad for a float64 the text form cannot spell -- NaN and +-Inf have no
// literal syntax, so they can only be inserted as values.
func nanAdFloat(f float64) func() *classad.ClassAd {
	return func() *classad.ClassAd {
		ad := classad.New()
		ad.InsertAttrFloat("R", f)
		return ad
	}
}

// nanWire is nanAdFloat straight to the wire form, for the fixtures below.
func nanWire(t *testing.T, c *Collection, f float64) []byte {
	t.Helper()
	return wire.Encode(nil, nanAdFloat(f)().AST(), c.intern)
}

// coldHas reports whether a record's cold tail carries an entry for id (the tail is keyed globally
// here -- encodeExceptLocal was called with a nil toLocal).
func coldHas(s *adSchema, rec []byte, id uint32) bool {
	_, _, cold := s.splitRecord(rec)
	for len(cold) > 0 {
		got, m := binary.Uvarint(cold)
		if m <= 0 {
			return false
		}
		cold = cold[m:]
		nl, ok := wire.NodeLen(cold)
		if !ok || nl > len(cold) {
			return false
		}
		if uint32(got) == id {
			return true
		}
		cold = cold[nl:]
	}
	return false
}

// TestNaNEscapesRealSlot is the storage contract: a NaN never occupies a real slot, and nothing
// else about a real column changes.
//
// A NaN is a number by KIND, so every kind-based guard in the columnar layer admits it; it is a
// member of no order, so every accelerator built on ordering is wrong about it. It therefore has to
// leave the ordered column the same way a wrong-typed or too-wide value does -- by escaping to the
// cold tail, where the cell machinery reads the value node itself.
//
// +-Inf stays in its slot on purpose. It orders against everything and is the correct min/max seed,
// so a column of infinities both accelerates and accumulates correctly; only the bucket-key floor
// mishandles it, and that is a property of the floor, not of the storage.
func TestNaNEscapesRealSlot(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	defer c.Close()
	s, id, idx := nanSchemaFixture(t, c)

	// What each case expects of the record: where R ends up, and -- for a value that round-trips as
	// a real literal -- what comes back out. wantReal is false for the two cases that have no real
	// literal to compare: an expression node and an absent attribute.
	for _, tc := range []struct {
		name     string
		ad       func() *classad.ClassAd
		escape   bool // must be out of the fixed slot
		inCold   bool // ... and carried in the cold tail (false = simply absent)
		wantReal bool
		want     float64
	}{
		{"finite", nanAdText(t, "1.5"), false, false, true, 1.5},
		{"zero", nanAdText(t, "0.0"), false, false, true, 0},
		{"negative", nanAdFloat(-2.5), false, false, true, -2.5},
		{"+Inf", nanAdFloat(math.Inf(1)), false, false, true, math.Inf(1)},
		{"-Inf", nanAdFloat(math.Inf(-1)), false, false, true, math.Inf(-1)},
		{"NaN literal", nanAdFloat(math.NaN()), true, true, true, math.NaN()},
		// A NaN that arrives by arithmetic is a different storage path: 0.0/0.0 is still an
		// expression node at encode time, so it has no literal kind and escapes for the ordinary
		// reason. Covered so a change to the literal rule cannot be mistaken for covering both.
		{"NaN by arithmetic", nanAdText(t, "0.0/0.0"), true, true, false, 0},
		{"absent", func() *classad.ClassAd { return classad.New() }, true, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := wire.Encode(nil, tc.ad().AST(), c.intern)
			rec := s.encode(wire.Ad(w))
			if esc := testBit(rec[:s.escBytes], idx); esc != tc.escape {
				t.Fatalf("escaped = %v, want %v", esc, tc.escape)
			}
			if got := coldHas(s, rec, id); got != tc.inCold {
				t.Fatalf("in cold tail = %v, want %v", got, tc.inCold)
			}
			// Wherever it was stored, the value must read back unchanged.
			var got float64
			gotReal, found := false, false
			s.forEach(rec, func(gotID uint32, node []byte) bool {
				if gotID != id {
					return true
				}
				found = true
				if k, lit := nodeKind(node); k == akReal {
					gotReal, got = true, lit.Real
				}
				return true
			})
			if found != (tc.name != "absent") {
				t.Fatalf("R came back = %v, want %v", found, tc.name != "absent")
			}
			if gotReal != tc.wantReal {
				t.Fatalf("read back a real literal = %v, want %v", gotReal, tc.wantReal)
			}
			if tc.wantReal && (math.IsNaN(tc.want) != math.IsNaN(got) || (!math.IsNaN(tc.want) && got != tc.want)) {
				t.Fatalf("round-tripped %v, want %v", got, tc.want)
			}
		})
	}
}

// TestNaNInSlotStillReadsBack pins the READ side against the fix.
//
// Nothing re-encodes what is already on disk, so every segment sealed before this change keeps its
// NaN in the slot. The decoder must go on returning that value verbatim: the escape is a rule about
// what the ENCODER writes, not a claim that a slot can no longer hold a NaN, and a decoder taught to
// "correct" one would turn stored data into something else.
func TestNaNInSlotStillReadsBack(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	defer c.Close()
	s, id, idx := nanSchemaFixture(t, c)

	// Hand-build the record the old encoder produced: in-slot NaN, escape bit clear.
	rec := make([]byte, s.escBytes+s.fixedLen)
	for i := range s.fields {
		if i != idx {
			setBit(rec[:s.escBytes], i) // every other field missing
		}
	}
	binary.LittleEndian.PutUint64(rec[s.escBytes+s.fields[idx].off:], math.Float64bits(math.NaN()))

	found := false
	s.forEach(rec, func(gotID uint32, node []byte) bool {
		if gotID != id {
			return true
		}
		k, lit := nodeKind(node)
		if k != akReal || !math.IsNaN(lit.Real) {
			t.Fatalf("read back kind %d value %v, want a NaN real", k, lit.Real)
		}
		found = true
		return true
	})
	if !found {
		t.Fatal("the in-slot NaN did not come back out of the record")
	}
}

// TestNaNEscapeInColumnarBlock follows one escaped NaN into a sealed columnar block: the escape bit
// is set, the block classifies it as an out-of-slot value rather than a missing one, the cold tail
// hands the NaN back, and -- because the field now has an escape -- the block's zone declares itself
// inexact, so no range test can prune a block holding a NaN away.
//
// The zone is the part worth pinning. With the NaN in a slot, numericZones compared it into
// [min,max] and every comparison lost, so the NaN contributed nothing AND the zone still reported
// itself exact: the range provably excluded a value the block contained.
func TestNaNEscapeInColumnarBlock(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 1})
	defer c.Close()
	s, id, idx := nanSchemaFixture(t, c)

	const n = 128
	const nanAt = 7
	var rows [][]byte
	for i := range n {
		var w []byte
		if i == nanAt {
			w = nanWire(t, c, math.NaN())
		} else {
			w = nanWire(t, c, float64(i)+0.5)
		}
		rows = append(rows, s.encode(wire.Ad(w)))
	}
	blk := encodeColumnarBlock(s, rows, resolveColLayout(s, nil), c.regionCodec(), nil)
	bc, err := newBlockCache(1 << 20)
	if err != nil {
		t.Fatal(err)
	}

	for k := range n {
		if got := testBit(blk.escapeAt(k), idx); got != (k == nanAt) {
			t.Fatalf("record %d: escaped = %v, want %v", k, got, k == nanAt)
		}
	}
	if missing, ok := blk.escapeIsMissing(idx, nanAt); !ok || missing {
		t.Fatalf("the NaN record classifies as missing=%v (ok=%v); it is present and exceptional",
			missing, ok)
	}
	nv, cell := blk.escapedNumCell(nanAt, id, bc)
	if cell != cellNumber || !math.IsNaN(nv.f) {
		t.Fatalf("escaped cell = (%v, %d), want a NaN number", nv.f, cell)
	}
	z, ok := blk.zone(idx)
	if !ok {
		t.Fatal("the real field has no block zone")
	}
	if !z.escaped {
		t.Fatal("the zone reports itself exact although the block holds a NaN outside [min,max]")
	}
	// The finite records still produce a usable range; escaping only marks it inexact.
	if z.Min != 0.5 || z.Max != float64(n-1)+0.5 {
		t.Fatalf("zone [%v, %v], want [0.5, %v]", z.Min, z.Max, float64(n-1)+0.5)
	}
}

// nanOracleConstraints are comparison shapes whose answer the columnar paths are expected to serve.
// Deliberately no negation of an ordering comparison: `!(R <= c)` is TRUE of a NaN while `R > c` is
// FALSE of it, and the paths that fold one into the other are being corrected separately. These are
// the forms escaping must not change.
func nanOracleConstraints() []string {
	var out []string
	for _, op := range []string{"<", "<=", ">", ">=", "==", "!=", "=?="} {
		for _, lit := range []string{"0.0", "10.5", "-1.0", "1000000.0"} {
			out = append(out, "R "+op+" "+lit)
		}
	}
	return append(out, "R > 1.0 && R < 50.0", "R >= 0.0 && K < 20", "R == 10.5 || K == 3")
}

// TestNaNColumnarPathsAgreeWithEvaluator holds every count path to per-ad evaluation on a fixture
// that deliberately contains NaN, +-Inf and ordinary reals in one column.
//
// The oracle is the evaluator rather than the record scan: the scan shares the pruning structures
// under test, so an agreement between the two would prove only that they are wrong together.
func TestNaNColumnarPathsAgreeWithEvaluator(t *testing.T) {
	t.Parallel()
	c := New(Options{Shards: 2, SegmentSize: 16 << 10})
	defer c.Close()

	vals := make([]float64, 0, 600)
	for i := range 600 {
		switch {
		case i%97 == 0:
			vals = append(vals, math.NaN())
		case i%197 == 0:
			vals = append(vals, math.Inf(1))
		case i%199 == 0:
			vals = append(vals, math.Inf(-1))
		default:
			vals = append(vals, float64(i%120)+0.5)
		}
	}
	nans := 0
	for i, v := range vals {
		ad := classad.New()
		ad.InsertAttrFloat("R", v)
		ad.InsertAttr("K", int64(i%40))
		if math.IsNaN(v) {
			nans++
		}
		if err := c.Put([]byte(fmt.Sprintf("k%d", i)), ad); err != nil {
			t.Fatal(err)
		}
	}
	if nans == 0 {
		t.Fatal("the fixture holds no NaN, so it proves nothing")
	}

	served := map[string]int{}
	check := func(label string) {
		for _, constraint := range nanOracleConstraints() {
			q, err := vm.Parse(constraint)
			if err != nil {
				t.Fatal(err)
			}
			expr, err := classad.ParseExpr(constraint)
			if err != nil {
				t.Fatal(err)
			}
			all, _ := vm.Parse("true")
			want := 0
			for ad := range c.Query(all) {
				if b, err := ad.EvaluateExprWithTarget(expr, nil).BoolValue(); err == nil && b {
					want++
				}
			}
			cmp := func(path string, got int, ok bool) {
				if !ok {
					return
				}
				served[label+"/"+path]++
				if got != want {
					t.Errorf("%s %s: %s = %d, want %d", label, constraint, path, got, want)
				}
			}
			cmp("Query", countSeq(c.Query(q)), true)
			n, ok := c.CountQuery(q)
			cmp("CountQuery", n, ok)
			n, ok = c.VectorEvalCount(q)
			cmp("VectorEvalCount", n, ok)
			n, ok = c.ColumnarEvalCount(q)
			cmp("ColumnarEvalCount", n, ok)
		}
	}
	check("rows")
	if !c.BuildAndEnableSchemaScan(4000, 4) {
		t.Fatal("the collection did not build its columnar accelerator")
	}
	// R must actually be a real column AND the NaN records must actually have escaped it, or the
	// columnar paths never meet an escaped NaN and the agreement above proves nothing.
	if escaped := nanColumnEscapes(t, c); escaped == 0 {
		t.Fatal("no record escaped the R column; the columnar paths never met an escaped NaN")
	} else {
		t.Logf("escaped R cells across the columnar blocks: %d of %d records", escaped, len(vals))
	}
	check("columnar")
	// Required per PHASE: a columnar path that only ever answered before the accelerator was
	// built would make the whole comparison vacuous.
	requireServed(t, served, "rows/Query",
		"columnar/Query", "columnar/CountQuery", "columnar/VectorEvalCount", "columnar/ColumnarEvalCount")
}

// nanColumnEscapes counts the records that escaped the R column across every columnar block, after
// checking that R is a real field in the committed schema. Zero means the fixture never exercised
// the escape, which is the way this kind of test passes while testing nothing.
func nanColumnEscapes(t *testing.T, c *Collection) int {
	t.Helper()
	id, ok := c.intern.LookupID("R")
	if !ok {
		t.Fatal("R was never interned")
	}
	st := c.schemaScan.Load()
	if st == nil {
		t.Fatal("the columnar accelerator is not enabled")
	}
	idx, ok := st.schema.byID[id]
	if !ok || st.schema.fields[idx].kind != akReal {
		t.Fatal("R is not a real column in the built schema; the test proves nothing")
	}
	escaped := 0
	for _, sh := range c.shards {
		_, ws := sh.snapshot()
		for _, w := range ws {
			cs := w.seg.colblk.Load()
			if cs == nil || cs.schema() == nil {
				continue
			}
			for _, blk := range cs.blocks {
				for k := range blk.n {
					if testBit(blk.escapeAt(k), idx) {
						escaped++
					}
				}
			}
		}
		releaseWindows(ws)
	}
	return escaped
}
