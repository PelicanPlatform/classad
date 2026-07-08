package wire

import (
	"bytes"
	"testing"

	"github.com/PelicanPlatform/classad/parser"
)

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
	// records
	`[]`, `[a = 1]`, `[a = 1; b = 2]`, `[x = a + b; y = TARGET.Cpus]`,
	`[a = [b = 1; c = 2]]`, `['a b' = 1]`,
	`{ [p = "primary"; port = 9618; noUDP = true], [p = "IPv4"; port = 41525] }`, // AddressV1-shaped
}

// TestParseExprToWireMatchesReference asserts the native wire parser produces
// byte-identical wire to the reference parser+encoder for every handled construct,
// in both interned and inline modes.
func TestParseExprToWireMatchesReference(t *testing.T) {
	for _, in := range handledExprs {
		e, err := parser.ParseExpr(in)
		if err != nil {
			t.Fatalf("reference parse %q: %v", in, err)
		}

		// interned
		refI := encoder{t: NewInternTable()}
		refI.node(e)
		gotI, err := parseExprToWire(in, NewInternTable(), false, nil)
		if err != nil {
			t.Errorf("interned %q: %v", in, err)
		} else if !bytes.Equal(gotI, refI.buf) {
			t.Errorf("interned %q:\n native %x\n   ref  %x", in, gotI, refI.buf)
		}

		// inline
		refN := encoder{inline: true}
		refN.node(e)
		gotN, err := parseExprToWire(in, nil, true, nil)
		if err != nil {
			t.Errorf("inline %q: %v", in, err)
		} else if !bytes.Equal(gotN, refN.buf) {
			t.Errorf("inline %q:\n native %x\n   ref  %x", in, gotN, refN.buf)
		}
	}
}

// TestParseExprToWireFallsBack confirms unsupported constructs return ErrUnsupported
// (so the caller can fall back) rather than producing wrong wire.
func TestParseExprToWireFallsBack(t *testing.T) {
	for _, in := range []string{
		`-9223372036854775808`, // int64-min magnitude: handled specially by the reference
	} {
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
