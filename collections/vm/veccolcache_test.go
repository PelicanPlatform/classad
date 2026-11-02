package vm

import (
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/ast"
)

// rawSource serves its one numeric column as a SLICE of bytes it already holds, which is what a
// block with no escapes does on a build whose vector engine supports the width. That is the case the cache must stay out of, because
// re-reading such a column is a bounds check and the cache's copy would be the only cost.
type rawSource struct {
	*countingSource
	raw   []byte
	width int
}

// newRawSource builds the column at a width the store actually emits -- RawColumnUseful gates which
// widths exist, and a fixture at a width production never produces would be testing a shape the
// executor never sees.
func newRawSource(t *testing.T, vals []int64) *rawSource {
	t.Helper()
	// The raw form is compiled out entirely without a vector engine: rawSupported is false, every
	// raw branch is deleted, and a vector carrying Raw reads as zeros. So there is no cheap path to
	// stay out of on this build, and the honest thing is to say so rather than to build a fixture
	// the executor cannot read. The GOEXPERIMENT=simd job runs this.
	if !rawSupported {
		t.Skip("no vector engine compiled in, so the raw column form does not exist on this build; " +
			"the GOEXPERIMENT=simd CI job is what runs this")
	}
	const width = 8
	recs := make([]map[string]string, len(vals))
	raw := make([]byte, len(vals)*width)
	for i, v := range vals {
		recs[i] = map[string]string{"A": itoa(v), "S": `"x"`}
		u := uint64(v)
		for b := 0; b < width; b++ {
			raw[i*width+b] = byte(u >> (8 * uint(b)))
		}
	}
	return &rawSource{countingSource: newCountingSource(recs), raw: raw, width: width}
}

func (s *rawSource) LoadColumn(name string, scope ast.AttributeScope, dst *Vec) bool {
	if strings.EqualFold(name, "A") {
		s.loads["a"]++
		dst.Mask, dst.Const, dst.Dict = false, false, nil
		dst.Raw, dst.RawWidth, dst.RawUnsigned, dst.RawReal = s.raw, s.width, false, false
		return true
	}
	return s.countingSource.LoadColumn(name, scope, dst)
}

// TestColCacheStaysOutOfTheCheapPath pins the rule that the measurement forced.
//
// A load is not a decompression on this path: the block is decompressed once and a load extracts a
// column from it. For a fitted-width column with no escapes that extraction is a slice of bytes the
// block already holds, so re-reading it is free and caching it is a copy for nothing. For a string
// column it is a dictionary parse or a walk over every preceding field, which is where the time is.
//
// So the executor decides from the first load: a column that comes back in RAW form is re-read, and
// anything else is cached. This asserts both halves, because asserting only the caching half would
// pass against a cache that never let go.
func TestColCacheStaysOutOfTheCheapPath(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		col        string
		wantLoads  int
	}{
		// Raw: mentioned twice, loaded twice, because re-reading is cheaper than copying.
		{"raw column is re-read", "A > 1 && A < 100", "a", 2},
		{"raw column three times", "A > 1 && A < 100 && A != 50", "a", 3},
		// Mentioned once: untouched either way.
		{"raw column once", "A > 1", "a", 1},
	} {
		src := newRawSource(t, []int64{2, 50, 99})
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, 3, nil)
		if !ok {
			t.Errorf("%s: %q declined", tc.name, tc.expr)
			continue
		}
		if n := src.loads[tc.col]; n != tc.wantLoads {
			t.Errorf("%s: %q loaded %q %d times, want %d", tc.name, tc.expr, tc.col, n, tc.wantLoads)
		}
		// Whatever it did about loading, the answer has to be the scalar one.
		for i := 0; i < 3; i++ {
			if want := q.Eval(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s != scalar %s", tc.name, i, got.value(i), want)
			}
		}
	}
}

// TestColCacheHoldsAMaterializedColumn is the other half, and it runs on every build: a column whose
// load materializes something is read once however many times the program names it.
//
// Separate from the raw half on purpose. That one can only run with a vector engine compiled in, and
// folding them together would have skipped this one too on the build everything else runs on.
func TestColCacheHoldsAMaterializedColumn(t *testing.T) {
	for _, tc := range []struct {
		name, expr string
		wantLoads  int
	}{
		{"named twice", `S == "x" || S == "y"`, 1},
		{"named three times", `S == "x" || S == "y" || S == "z"`, 1},
		{"named once", `S == "x"`, 1},
		{"named twice across a conditional", `ifThenElse(S == "x", S, "other")`, 1},
	} {
		src := newCountingSource([]map[string]string{{"S": `"x"`, "A": "1"}, {"S": `"y"`, "A": "2"}})
		q, err := Parse(tc.expr)
		if err != nil {
			t.Fatal(err)
		}
		got, ok := q.VecEval(src, 2, nil)
		if !ok {
			t.Errorf("%s: %q declined", tc.name, tc.expr)
			continue
		}
		if n := src.loads["s"]; n != tc.wantLoads {
			t.Errorf("%s: %q loaded S %d times, want %d", tc.name, tc.expr, n, tc.wantLoads)
		}
		for i := 0; i < 2; i++ {
			if want := q.Eval(src.ad(i)); !sameValue(got.value(i), want) {
				t.Errorf("%s record %d: vector %s != scalar %s", tc.name, i, got.value(i), want)
			}
		}
	}
}
