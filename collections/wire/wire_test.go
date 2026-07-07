package wire

import (
	"testing"

	"github.com/PelicanPlatform/classad/ast"
	"github.com/PelicanPlatform/classad/classad"
	"github.com/PelicanPlatform/classad/parser"
)

// toClassAd bridges an *ast.ClassAd into a *classad.ClassAd so we can use the
// reference Equal for structural comparison.
func toClassAd(a *ast.ClassAd) *classad.ClassAd {
	c := classad.New()
	if a != nil {
		for _, attr := range a.Attributes {
			c.Insert(attr.Name, attr.Value)
		}
	}
	return c
}

// roundTripCases exercises every ast node type through the codec.
var roundTripCases = []string{
	`[a = 1]`,
	`[a = -42; b = 0]`,
	`[r = 3.14; big = 1.0e10; neg = -2.5]`,
	`[s = "hello"; esc = "a\"b\n\tc"; empty = ""]`,
	`[t = true; f = false; u = undefined; e = error]`,
	`[x = y; my = MY.Cpus; tgt = TARGET.Memory; par = PARENT.Foo]`,
	`[sum = a + b; diff = a - b; prod = a*b; quot = a/b; mod = a%b]`,
	`[lt = a < b; le = a <= b; gt = a > b; ge = a >= b; eq = a == b; ne = a != b]`,
	`[and = a && b; or = a || b; isv = a =?= b; isntv = a =!= b]`,
	`[band = a & b; bor = a | b; bxor = a ^ b; shl = a << 2; shr = a >> 2; ushr = a >>> 2]`,
	`[neg = -a; not = !a; bnot = ~a; pos = +a]`,
	`[list = {1, 2, 3}; nested = {{1}, {2, 3}, {}}; mixed = {1, "two", true, x+1}]`,
	`[rec = [inner = 1; deep = [d = 2]]]`,
	`[f0 = time(); f1 = strcat("a", "b", "c"); f2 = regexp("p", s, "i")]`,
	`[cond = a ? b : c; nested = a ? (b ? c : d) : e]`,
	`[elvis = a ?: b; chain = a ?: b ?: c]`,
	`[sel = x.y; deep = a.b.c]`,
	`[sub = L[0]; expr = M[i + 1]]`,
	`[paren = (a + b) * c; nestp = ((x))]`,
	`[combo = (MY.Rank > TARGET.Rank) && (strcat(Owner, "!") =?= "root!") ? {1,2} : undefined]`,
}

func TestRoundTripShared(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		tbl := NewInternTable()
		enc := Encode(nil, orig, tbl)
		got, err := Decode(enc, tbl)
		if err != nil {
			t.Fatalf("decode %q: %v", src, err)
		}
		if !toClassAd(orig).Equal(toClassAd(got)) {
			t.Errorf("round-trip mismatch for %q:\n orig=%s\n  got=%s", src, orig.String(), got.String())
		}
	}
}

func TestRoundTripStandalone(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatalf("parse %q: %v", src, err)
		}
		enc := EncodeStandalone(orig)
		got, err := DecodeStandalone(enc)
		if err != nil {
			t.Fatalf("decode %q: %v", src, err)
		}
		if !toClassAd(orig).Equal(toClassAd(got)) {
			t.Errorf("standalone round-trip mismatch for %q:\n orig=%s\n  got=%s", src, orig.String(), got.String())
		}
	}
}

// TestEncodeDeterministic checks that encoding is stable and that re-encoding a
// decoded ad reproduces identical bytes (important for content-addressing).
func TestEncodeDeterministic(t *testing.T) {
	for _, src := range roundTripCases {
		orig, err := parser.ParseClassAd(src)
		if err != nil {
			t.Fatal(err)
		}
		a := EncodeStandalone(orig)
		dec, err := DecodeStandalone(a)
		if err != nil {
			t.Fatal(err)
		}
		b := EncodeStandalone(dec)
		if string(a) != string(b) {
			t.Errorf("re-encode not stable for %q: %d vs %d bytes", src, len(a), len(b))
		}
	}
}

func TestInternTableCaseInsensitive(t *testing.T) {
	tbl := NewInternTable()
	id1 := tbl.Intern("Owner")
	id2 := tbl.Intern("owner")
	id3 := tbl.Intern("OWNER")
	if id1 != id2 || id1 != id3 {
		t.Fatalf("case-fold ids differ: %d %d %d", id1, id2, id3)
	}
	name, ok := tbl.Name(id1)
	if !ok || name != "Owner" {
		t.Fatalf("canonical casing not preserved: got %q ok=%v", name, ok)
	}
	if tbl.Len() != 1 {
		t.Fatalf("expected 1 interned name, got %d", tbl.Len())
	}
	other := tbl.Intern("RequestMemory")
	if other == id1 {
		t.Fatalf("distinct name got same id")
	}
}

func TestAccessorLookupAndForEach(t *testing.T) {
	src := `[Owner = "alice"; RequestMemory = 2048; Rank = Cpus * 2; Nested = [k = 1]]`
	orig, err := parser.ParseClassAd(src)
	if err != nil {
		t.Fatal(err)
	}
	tbl := NewInternTable()
	enc := Ad(Encode(nil, orig, tbl))

	// Lookup a scalar and an expression by interned id.
	ownerID := tbl.Intern("Owner")
	node, ok := enc.Lookup(ownerID)
	if !ok {
		t.Fatal("Owner not found")
	}
	expr, err := DecodeNode(node, tbl)
	if err != nil {
		t.Fatal(err)
	}
	if s, isStr := expr.(*ast.StringLiteral); !isStr || s.Value != "alice" {
		t.Fatalf("Owner = %v, want string \"alice\"", expr)
	}

	rankID := tbl.Intern("Rank")
	rnode, ok := enc.Lookup(rankID)
	if !ok {
		t.Fatal("Rank not found")
	}
	rexpr, err := DecodeNode(rnode, tbl)
	if err != nil {
		t.Fatal(err)
	}
	if _, isBin := rexpr.(*ast.BinaryOp); !isBin {
		t.Fatalf("Rank = %T, want *ast.BinaryOp", rexpr)
	}

	// Absent id.
	if _, ok := enc.Lookup(999999); ok {
		t.Fatal("lookup of absent id reported found")
	}

	// ForEach visits every attribute exactly once.
	seen := map[uint32]int{}
	if !enc.ForEach(func(id uint32, _ []byte) bool { seen[id]++; return true }) {
		t.Fatal("ForEach reported malformed ad")
	}
	if len(seen) != 4 {
		t.Fatalf("ForEach visited %d attrs, want 4", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			name, _ := tbl.Name(id)
			t.Fatalf("attr %q visited %d times", name, n)
		}
	}
}

// TestDecodeMalformed ensures the decoder rejects (never panics on) bad input.
func TestDecodeMalformed(t *testing.T) {
	orig, err := parser.ParseClassAd(roundTripCases[len(roundTripCases)-1])
	if err != nil {
		t.Fatal(err)
	}
	good := EncodeStandalone(orig)

	// Every prefix of a valid encoding must decode-or-error, never panic.
	for i := 0; i < len(good); i++ {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on prefix len %d: %v", i, r)
				}
			}()
			_, _ = DecodeStandalone(good[:i])
		}()
	}

	// Garbage / wrong magic.
	if _, err := Decode([]byte{0x00, 0x00, 0x00}, NewInternTable()); err == nil {
		t.Error("expected error for bad magic")
	}
	if _, err := Decode(nil, NewInternTable()); err == nil {
		t.Error("expected error for empty input")
	}
}

func FuzzRoundTrip(f *testing.F) {
	for _, src := range roundTripCases {
		f.Add(src)
	}
	f.Fuzz(func(t *testing.T, src string) {
		orig, err := parser.ParseClassAd(src)
		if err != nil || orig == nil {
			return // only well-formed ads are in scope
		}
		enc := EncodeStandalone(orig)
		got, err := DecodeStandalone(enc)
		if err != nil {
			t.Fatalf("decode failed for parseable ad %q: %v", src, err)
		}
		// Re-encoding the freshly-decoded ad must be byte-stable. This must run
		// before the Equal check below, because classad.Equal sorts nested
		// records in place (ensureSorted), which would reorder got's attributes.
		if string(enc) != string(EncodeStandalone(got)) {
			t.Fatalf("re-encode not stable for %q", src)
		}
		if !toClassAd(orig).Equal(toClassAd(got)) {
			t.Fatalf("round-trip mismatch for %q", src)
		}
	})
}

func BenchmarkEncode(b *testing.B) {
	orig, err := parser.ParseClassAd(roundTripCases[len(roundTripCases)-1])
	if err != nil {
		b.Fatal(err)
	}
	tbl := NewInternTable()
	buf := make([]byte, 0, 256)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf = Encode(buf[:0], orig, tbl)
	}
	b.SetBytes(int64(len(buf)))
}

func BenchmarkDecode(b *testing.B) {
	orig, err := parser.ParseClassAd(roundTripCases[len(roundTripCases)-1])
	if err != nil {
		b.Fatal(err)
	}
	tbl := NewInternTable()
	enc := Encode(nil, orig, tbl)
	b.ReportAllocs()
	b.SetBytes(int64(len(enc)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Decode(enc, tbl); err != nil {
			b.Fatal(err)
		}
	}
}
