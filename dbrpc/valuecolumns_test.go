package dbrpc

import (
	"bytes"
	"fmt"
	"math"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// TestBatchEncodePathsAgree holds the two encoders to one wire format.
//
// The column source writes a frame straight from the producer's columns; the record source boxes
// each cell into a classad.Value and writes the frame from those. They are two implementations of
// the same bytes, and nothing else in the system would notice them drifting: a client decodes
// whichever it is sent, and both produce a well-formed frame. So compare the BYTES, over batches
// built to exercise every branch that differs between them -- kind choice, the dictionary payoff
// test, exceptions, and the empty column.
func TestBatchEncodePathsAgree(t *testing.T) {
	for _, tc := range []struct {
		name string
		cols []collections.Column
		n    int
	}{
		{"uniform ints", []collections.Column{intCol(64, func(i int) int64 { return int64(i * 7) })}, 64},
		{"uniform reals", []collections.Column{realCol(64, func(i int) float64 { return float64(i) / 3 })}, 64},
		{"uniform bools", []collections.Column{boolCol(64, func(i int) bool { return i%3 == 0 })}, 64},
		{"repeated strings (dictionary pays)", []collections.Column{strCol2(64, func(i int) string {
			return fmt.Sprintf("owner%d", i%4)
		})}, 64},
		{"distinct strings (dictionary declines)", []collections.Column{strCol2(64, func(i int) string {
			return fmt.Sprintf("/a/very/long/distinct/path/number/%d", i)
		})}, 64},
		{"ints with string exceptions", []collections.Column{mixedCol(64)}, 64},
		{"all undefined", []collections.Column{undefCol(32)}, 32},
		{"all error", []collections.Column{errCol(32)}, 32},
		{"one row", []collections.Column{intCol(1, func(i int) int64 { return 5 })}, 1},
		{"ties on kind", []collections.Column{tiedCol(64)}, 64},
		{"exceptions of every kind", []collections.Column{everyExcCol(70)}, 70},
		{"reals with int exceptions", []collections.Column{realWithIntExcCol(64)}, 64},
		{"several columns", []collections.Column{
			intCol(48, func(i int) int64 { return int64(i) }),
			strCol2(48, func(i int) string { return fmt.Sprintf("h%d", i%3) }),
			realCol(48, func(i int) float64 { return float64(i) * 1.5 }),
			mixedCol(48),
		}, 48},
	} {
		for _, dict := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/dict=%v", tc.name, dict), func(t *testing.T) {
				b := &collections.Batch{N: tc.n, Cols: tc.cols}

				direct := newValueEncoder(len(tc.cols))
				direct.dict = dict
				gotDirect := direct.encodeBatchFrame(nil, b, 0, b.N)

				viaValues := newValueEncoder(len(tc.cols))
				viaValues.dict = dict
				for r := 0; r < b.N; r++ {
					viaValues.appendBatch(b, r)
				}
				gotValues := viaValues.encode(nil)

				if !bytes.Equal(gotDirect, gotValues) {
					t.Fatalf("the two encoders disagree\n direct: %d bytes\n values: %d bytes\n"+
						"first difference at %d", len(gotDirect), len(gotValues),
						firstDiff(gotDirect, gotValues))
				}
				// Premise: the frame has to decode, or two identically broken encoders pass.
				var out ValueBatch
				_, _, body, ok := respHeader(append(respHead(1, stStream), gotDirect...))
				if !ok || !decodeValueBatch(body, &out) {
					t.Fatal("the agreed frame does not decode")
				}
				if out.Rows != tc.n {
					t.Fatalf("decoded %d rows, want %d", out.Rows, tc.n)
				}
			})
		}
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

func newCol(n int) collections.Column {
	return collections.Column{Kind: make([]collections.ValueKind, n), Num: make([]int64, n), Str: make([]string, n)}
}

func intCol(n int, f func(int) int64) collections.Column {
	c := newCol(n)
	for i := range n {
		c.Kind[i], c.Num[i] = collections.ValueInt, f(i)
	}
	return c
}

func realCol(n int, f func(int) float64) collections.Column {
	c := newCol(n)
	for i := range n {
		c.Kind[i], c.Num[i] = collections.ValueReal, int64(math.Float64bits(f(i)))
	}
	return c
}

func boolCol(n int, f func(int) bool) collections.Column {
	c := newCol(n)
	for i := range n {
		c.Kind[i] = collections.ValueBool
		if f(i) {
			c.Num[i] = 1
		}
	}
	return c
}

func strCol2(n int, f func(int) string) collections.Column {
	c := newCol(n)
	for i := range n {
		c.Kind[i], c.Str[i] = collections.ValueString, f(i)
	}
	return c
}

func undefCol(n int) collections.Column { return newCol(n) } // ValueUndefined is the zero kind

func errCol(n int) collections.Column {
	c := newCol(n)
	for i := range n {
		c.Kind[i] = collections.ValueError
	}
	return c
}

// mixedCol is mostly ints with every kind of exception among them, so the dense array, the bitmap
// and the exception list all carry something.
func mixedCol(n int) collections.Column {
	c := newCol(n)
	for i := range n {
		switch i % 7 {
		case 3:
			c.Kind[i], c.Str[i] = collections.ValueString, fmt.Sprintf("s%d", i)
		case 5:
			c.Kind[i] = collections.ValueUndefined
		case 6:
			c.Kind[i] = collections.ValueError
		default:
			c.Kind[i], c.Num[i] = collections.ValueInt, int64(i)
		}
	}
	return c
}

// tiedCol has equal counts of two kinds, so the two implementations must break the tie the same
// way -- a difference no other fixture would show.
func tiedCol(n int) collections.Column {
	c := newCol(n)
	for i := range n {
		if i%2 == 0 {
			c.Kind[i], c.Num[i] = collections.ValueInt, int64(i)
		} else {
			c.Kind[i], c.Str[i] = collections.ValueString, fmt.Sprintf("t%d", i%3)
		}
	}
	return c
}

// everyExcCol is dominated by strings, so every OTHER kind present is an exception. Without a
// column shaped like this no int, real or bool cell is ever written through the exception encoding,
// and a path that tags them wrongly agrees with the other encoder anyway.
func everyExcCol(n int) collections.Column {
	c := newCol(n)
	for i := range n {
		switch i % 7 {
		case 1:
			c.Kind[i], c.Num[i] = collections.ValueInt, int64(i*3)
		case 2:
			c.Kind[i], c.Num[i] = collections.ValueReal, int64(math.Float64bits(float64(i)/7))
		case 3:
			c.Kind[i] = collections.ValueBool
			c.Num[i] = int64(i % 2)
		case 4:
			c.Kind[i] = collections.ValueUndefined
		case 5:
			c.Kind[i] = collections.ValueError
		default:
			// Few enough distinct values that the dictionary form pays, so the dict arm of
			// this case exercises KindStringDict with exceptions of every kind beside it.
			c.Kind[i], c.Str[i] = collections.ValueString, fmt.Sprintf("dom%d", i%3)
		}
	}
	return c
}

// realWithIntExcCol is dominated by reals with ints among them: the one pair whose dense encoding
// is identical (both uint64(Num[i])) and whose EXCEPTION tags are not.
func realWithIntExcCol(n int) collections.Column {
	c := newCol(n)
	for i := range n {
		if i%5 == 0 {
			c.Kind[i], c.Num[i] = collections.ValueInt, int64(i)
			continue
		}
		c.Kind[i], c.Num[i] = collections.ValueReal, int64(math.Float64bits(float64(i)*2.5))
	}
	return c
}
