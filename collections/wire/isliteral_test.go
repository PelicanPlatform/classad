package wire

import (
	"encoding/binary"
	"math"
	"math/rand"
	"testing"
)

// oldIsLiteral is the predicate IsLiteralNode replaced: decode the value, discard it, keep the
// ok. It is the reference this must agree with on every input, including malformed ones.
func oldIsLiteral(node []byte) bool {
	if _, ok := LiteralValue(node); ok {
		return true
	}
	_, ok := NumberValue(node)
	return ok
}

// TestIsLiteralNodeMatchesDecoding is the differential test. A predicate that is right about
// well-formed nodes and wrong about truncated ones would turn a malformed record into an
// "expression" or the reverse, silently, on the per-record path.
func TestIsLiteralNodeMatchesDecoding(t *testing.T) {
	var nodes [][]byte
	add := func(b ...byte) { nodes = append(nodes, b) }

	add()                          // empty
	add(nUndefined)                //
	add(nError)                    //
	add(nBoolFalse)                //
	add(nBoolTrue)                 //
	add(nUndefined, 0xff)          // trailing junk
	add(nInt)                      // truncated varint
	add(nReal)                     // truncated real
	add(nReal, 1, 2, 3)            // short real
	add(nString)                   // truncated length
	add(nString, 5, 'a', 'b')      // length longer than the payload
	add(nString, 0)                // empty string
	add(nString, 3, 'a', 'b', 'c') // well-formed
	add(nParen, nInt, 2)           // (1)
	add(nUnOp, 0, nInt, 2)         // signed number, whichever op 0 is
	add(nParen)                    // dangling paren
	add(nUnOp)                     // dangling unop
	add(0x7f)                      // unknown tag
	for _, tag := range []byte{nInt, nReal, nString, nParen, nUnOp, 0x7f, 0xff} {
		add(tag)
		add(tag, 0)
		add(tag, 0xff)
		add(tag, 0x80, 0x80, 0x80)
	}
	// Well-formed ints and reals of every magnitude.
	for _, v := range []int64{0, 1, -1, 127, -128, 1 << 20, -(1 << 20), math.MaxInt64, math.MinInt64} {
		b := make([]byte, 1, 11)
		b[0] = nInt
		b = binary.AppendVarint(b, v)
		nodes = append(nodes, b)
		// ...and the same bytes truncated by one, which must not be a literal.
		if len(b) > 2 {
			nodes = append(nodes, b[:len(b)-1])
		}
	}
	for _, f := range []float64{0, 1.5, -1.5, math.Inf(1), math.NaN()} {
		b := make([]byte, 9)
		b[0] = nReal
		binary.LittleEndian.PutUint64(b[1:], math.Float64bits(f))
		nodes = append(nodes, b)
	}
	// Random bytes: the shapes nobody thinks to write down.
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		b := make([]byte, rng.Intn(12))
		for j := range b {
			b[j] = byte(rng.Intn(8)) // bias to the real tag space
		}
		nodes = append(nodes, b)
	}

	for _, n := range nodes {
		if got, want := IsLiteralNode(n), oldIsLiteral(n); got != want {
			t.Fatalf("IsLiteralNode(%v) = %v, decoding says %v", n, got, want)
		}
	}
	t.Logf("agreed on %d node shapes", len(nodes))
}

// TestStringNodeBytesMatchesLiteralValue pins the other half: the bytes handed back are the ones
// LiteralValue would have copied.
func TestStringNodeBytesMatchesLiteralValue(t *testing.T) {
	for _, s := range []string{"", "a", "owner@example.com", "o999", string(make([]byte, 300))} {
		b := make([]byte, 1, len(s)+11)
		b[0] = nString
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
		got, ok := StringNodeBytes(b)
		if !ok {
			t.Fatalf("StringNodeBytes refused a well-formed %d-byte string", len(s))
		}
		lit, lok := LiteralValue(b)
		if !lok || lit.Kind != LitString {
			t.Fatalf("LiteralValue did not read it back as a string")
		}
		if string(got) != lit.Str {
			t.Errorf("bytes %q, LiteralValue %q", got, lit.Str)
		}
	}
	// A non-string node is refused rather than misread.
	if _, ok := StringNodeBytes([]byte{nInt, 2}); ok {
		t.Error("StringNodeBytes accepted an int node")
	}
}
