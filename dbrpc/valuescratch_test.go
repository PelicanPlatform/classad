package dbrpc

import (
	"fmt"
	"testing"

	"github.com/PelicanPlatform/classad/classad"
)

// roundTripBatch encodes the rows through enc and decodes the frame back, as the server and client
// do. The encoder is deliberately NOT fresh per call where a test passes the same one twice: reuse
// across batches is the thing under test.
func roundTripBatch(t *testing.T, enc *valueEncoder, rows [][]classad.Value) *ValueBatch {
	t.Helper()
	for _, r := range rows {
		enc.append(r)
	}
	frame := enc.encode(respHead(1, stStream))
	enc.reset()
	_, _, body, ok := respHeader(frame)
	if !ok {
		t.Fatal("the frame has no readable header")
	}
	var out ValueBatch
	if !decodeValueBatch(body, &out) {
		t.Fatal("the decoder refused the batch")
	}
	return &out
}

func strCol(t *testing.T, b *ValueBatch, col int) []string {
	t.Helper()
	vals := b.Column(col, nil)
	out := make([]string, len(vals))
	for i, v := range vals {
		s, err := v.StringValue()
		if err != nil {
			t.Fatalf("column %d row %d is %v, want a string", col, i, v.Type())
		}
		out[i] = s
	}
	return out
}

// TestDictionaryColumnsDoNotShareAnIndex: two string columns in ONE batch, where the second
// repeats a value from the first.
//
// The encoder reuses its dictionary scratch between columns, so the index has to be cleared. If it
// is not, the second column finds the shared value already present and takes the FIRST column's
// code for it, while the dictionary it ships was rebuilt from empty -- so the code addresses a
// different entry. The failure is silent and in range: a cell comes back holding another cell's
// string, not an error and not a refused batch.
//
// The existing dictionary fixture cannot see this: it projects one string column, and one column
// has no other column's codes to inherit.
func TestDictionaryColumnsDoNotShareAnIndex(t *testing.T) {
	const n = 120
	first := []string{"alpha", "beta"}
	second := []string{"alpha", "gamma", "delta"} // "alpha" is the overlap that does the damage

	enc := newValueEncoder(2)
	enc.dict = true
	rows := make([][]classad.Value, 0, n)
	var wantFirst, wantSecond []string
	for i := 0; i < n; i++ {
		a, b := first[i%len(first)], second[i%len(second)]
		wantFirst = append(wantFirst, a)
		wantSecond = append(wantSecond, b)
		rows = append(rows, []classad.Value{classad.NewStringValue(a), classad.NewStringValue(b)})
	}
	got := roundTripBatch(t, enc, rows)

	// Premise: both columns must actually take the dictionary form, or the reuse under test
	// never happens and the comparison is of the plain form with itself.
	for col := 0; col < 2; col++ {
		if k := got.Cols[col].Kind; k != KindStringDict {
			t.Fatalf("column %d arrived as %v, want string-dict: the dictionary path did not "+
				"run, so this test is not exercising the shared scratch", col, k)
		}
	}
	for col, want := range [][]string{wantFirst, wantSecond} {
		for i, g := range strCol(t, got, col) {
			if g != want[i] {
				t.Fatalf("column %d row %d: got %q, want %q", col, i, g, want[i])
			}
		}
	}
}

// TestEncoderScratchSurvivesSuccessiveBatches: the same encoder, several batches, shapes that
// shrink. A scratch slice reused without being re-sliced would leave the previous batch's tail
// behind, and a validity bitmap reused without being cleared would mark cells valid that this
// batch never packed.
func TestEncoderScratchSurvivesSuccessiveBatches(t *testing.T) {
	enc := newValueEncoder(2)
	enc.dict = true
	// Descending row counts, so every batch after the first reuses scratch longer than it needs.
	//
	// EVERY BATCH USES DIFFERENT VALUES, via base. With the same values in each, scratch left
	// over from the previous batch happens to equal what this one expects, and a reuse bug
	// reads as correct -- which it did: the first version of this test passed with the dense
	// scratch deliberately not re-sliced.
	for b, n := range []int{200, 50, 7, 1, 90} {
		base := int64(b+1) * 1000
		exc := fmt.Sprintf("not-an-int-%d", b)
		t.Run(fmt.Sprintf("rows=%d", n), func(t *testing.T) {
			rows := make([][]classad.Value, 0, n)
			var wantStr []string
			var wantInt []int64
			for i := 0; i < n; i++ {
				// Column 1 is an int column with a string every third row, so it carries
				// exceptions and its validity bitmap is NOT all ones.
				s := fmt.Sprintf("host%d-%d", b, i%5)
				wantStr = append(wantStr, s)
				v := classad.NewIntValue(base + int64(i))
				wantInt = append(wantInt, base+int64(i))
				if i%3 == 0 {
					v = classad.NewStringValue(exc)
					wantInt[i] = -1 // marker: this row is an exception
				}
				rows = append(rows, []classad.Value{classad.NewStringValue(s), v})
			}
			got := roundTripBatch(t, enc, rows)
			if got.Rows != n {
				t.Fatalf("decoded %d rows, want %d", got.Rows, n)
			}
			for i, g := range strCol(t, got, 0) {
				if g != wantStr[i] {
					t.Fatalf("string column row %d: got %q, want %q", i, g, wantStr[i])
				}
			}
			ints := got.Column(1, nil)
			for i, v := range ints {
				if wantInt[i] == -1 {
					s, err := v.StringValue()
					if err != nil || s != exc {
						t.Fatalf("row %d should be the exception %q, got %v", i, exc, v)
					}
					continue
				}
				x, err := v.IntValue()
				if err != nil || x != wantInt[i] {
					t.Fatalf("row %d: got %v, want %d", i, v, wantInt[i])
				}
			}
		})
	}
}

// TestDictionaryHoldsOnlyThisColumnsValues: the dictionary scratch is reused, so a column's
// dictionary must be rebuilt from empty rather than appended to the previous one's.
//
// Stale entries do not corrupt the values -- the codes still address the right strings -- so the
// round-trip tests above cannot see this. What it costs is wire: every column after the first
// ships the earlier columns' strings as dead dictionary entries, and their bytes are not counted
// by the payoff test, so the form can be chosen when the plain one would have been smaller.
func TestDictionaryHoldsOnlyThisColumnsValues(t *testing.T) {
	enc := newValueEncoder(1)
	enc.dict = true
	strs := func(ss ...string) []classad.Value {
		out := make([]classad.Value, 0, len(ss))
		for _, s := range ss {
			out = append(out, classad.NewStringValue(s))
		}
		return out
	}
	// Repeated enough that the dictionary pays for both columns; "alpha" is shared.
	first := strs("alpha", "beta", "alpha", "beta", "alpha", "beta", "alpha", "beta")
	second := strs("alpha", "gamma", "alpha", "gamma", "alpha", "gamma", "alpha", "gamma")

	if _, dict := enc.buildStringDict(first); len(dict) != 2 {
		t.Fatalf("first column's dictionary is %v, want 2 entries", dict)
	}
	codes, dict := enc.buildStringDict(second)
	if dict == nil {
		t.Fatal("the second column declined the dictionary form; this test needs both to take it")
	}
	if len(dict) != 2 {
		t.Fatalf("second column's dictionary is %v, want exactly its own 2 distinct values -- "+
			"anything more is the previous column's, kept by reusing the scratch without "+
			"re-slicing it", dict)
	}
	for i, c := range codes {
		want, _ := second[i].StringValue()
		if int(c) >= len(dict) || dict[c] != want {
			t.Fatalf("row %d: code %d addresses %v, want %q", i, c, dict, want)
		}
	}
}
