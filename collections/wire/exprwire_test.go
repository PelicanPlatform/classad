package wire

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

// refNodeBytes encodes input's reference-parsed AST to wire node bytes via the
// reference encoder, interning into a fresh table (so ids are assigned in the same
// pre-order the native parser uses).
func refNodeBytes(t *testing.T, input string) []byte {
	t.Helper()
	e, err := parser.ParseExpr(input)
	if err != nil {
		t.Fatalf("reference parse %q: %v", input, err)
	}
	enc := encoder{t: NewInternTable()}
	enc.node(e)
	return enc.buf
}

var handledExprs = []string{
	`1`, `-5`, `3.14`, `1.0e10`, `"hi"`, `"a\"b\n"`, `true`, `false`, `undefined`, `error`,
	`x`, `TARGET.Cpus`, `MY.Rank`,
	`a + b`, `a + b * c`, `(a + b) * c`, `a - b - c`, `a * b / c % d`,
	`a == b`, `a != b`, `a =?= b`, `a =!= b`, `a is b`, `a isnt b`,
	`a && b || c`, `a < b`, `a <= b`, `a > b`, `a >= b`,
	`a | b & c ^ d`, `a << 2 >> 3 >>> 4`,
	`-a`, `!x`, `~y`, `-a * b`, `!a && b`, `-(a + b)`, `+a`,
	`a.b`, `a.b.c`, `L[0]`, `M[i + 1]`, `a.b[0]`,
	`time()`, `strcat("a", "b")`, `ifThenElse(a, b, c)`, `regexp("p", s, "i")`,
	`{1, 2, 3}`, `{}`, `{a, b + 1, "x"}`, `{ {1}, {2, 3} }`,
	`a ? b : c`, `a ? b : c ? d : e`, `a > 0 ? x : y`,
	`a ?: b`, `a ?: b ?: c`,
	`(TARGET.Cpus >= RequestCpus) && (TARGET.Memory >= RequestMemory)`,
	`ifThenElse(State == "Claimed", RemoteOwner, "none")`,
	`(GPUs =?= undefined) || (GPUs == 0) || ((CPUs > 8 * GPUs) && (Memory > 32000 * GPUs))`,
}

// TestParseExprToWireMatchesReference asserts the native wire parser produces
// byte-identical wire to the reference parser+encoder for every handled construct.
func TestParseExprToWireMatchesReference(t *testing.T) {
	for _, in := range handledExprs {
		want := refNodeBytes(t, in)
		got, err := ParseExprToWire(in, NewInternTable(), nil)
		if err != nil {
			t.Errorf("ParseExprToWire(%q): %v", in, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%q:\n native %x\n   ref  %x", in, got, want)
		}
	}
}

// TestParseExprToWireFallsBack confirms unsupported constructs return ErrUnsupported
// (so the caller can fall back) rather than producing wrong wire.
func TestParseExprToWireFallsBack(t *testing.T) {
	for _, in := range []string{`[a = 1]`, `[a = 1; b = 2]`} {
		if _, err := ParseExprToWire(in, NewInternTable(), nil); err == nil {
			t.Errorf("ParseExprToWire(%q) succeeded; expected fallback", in)
		}
	}
}

// BenchmarkParseExprWireVsAST compares the native text->wire parser against the
// reference parser + encoder (parse to ast.Expr, then encode to wire) on a mix of
// realistic computed values -- the head-to-head for the ingest path.
func BenchmarkParseExprWireVsAST(b *testing.B) {
	cases := []string{
		`(TARGET.RequestCpus <= Cpus) && (TARGET.RequestMemory <= Memory) && (TARGET.RequestDisk <= Disk)`,
		`ifThenElse(State =?= "Claimed", RemoteOwner, "none")`,
		`(GPUs =?= undefined) || (GPUs == 0) || ((SlotType == "Partitionable") && (CPUs > 8 * GPUs))`,
		`PelicanPluginVersion =!= undefined`,
	}

	b.Run("native-wire", func(b *testing.B) {
		b.ReportAllocs()
		var buf []byte
		for i := 0; i < b.N; i++ {
			t := NewInternTable()
			for _, s := range cases {
				var err error
				buf, err = ParseExprToWire(s, t, buf[:0])
				if err != nil {
					b.Fatal(err)
				}
			}
		}
	})

	b.Run("reference-ast", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			t := NewInternTable()
			for _, s := range cases {
				e, err := parser.ParseExpr(s)
				if err != nil {
					b.Fatal(err)
				}
				enc := encoder{t: t}
				enc.node(e)
			}
		}
	})
}
