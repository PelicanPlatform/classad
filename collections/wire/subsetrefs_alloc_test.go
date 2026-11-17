package wire

import "testing"

// refsAllocAd builds an inline ad with n attributes and no hot header, so the assembler takes
// the entry walk rather than the hot shortcut -- the path a scanned record takes.
func refsAllocAd(n int) []byte {
	entries := make([][]byte, 0, n)
	for i := 0; i < n; i++ {
		name := string(rune('A' + i%26))
		entries = append(entries, inlineEntry(name+string(rune('a'+i/26)), AppendIntNode(nil, int64(i))))
	}
	return rawAd(flagInlineNames, nil, entries...)
}

// TestAppendAdSubsetInlineRefsReusesTheCallersBuffer pins the per-RECORD cost of the
// reference-chasing wire-row assembler at zero allocations.
//
// It is called once per scanned record, by every QueryRawWireRefs consumer, with a scratch
// buffer the scan reuses across the whole iteration (see yieldWireRow). That reuse buys nothing
// if the assembler allocates per call anyway -- which it did: the entry walk built its cursor
// as `c = &cursor{...}` inside the repeat loop, and the compiler could not keep that on the
// stack. One allocation per row is invisible in a unit test and is 100,000 of them on a
// 100,000-row scan.
//
// The number this asserts is 0 and not "fewer", because the thing being pinned is a property --
// the assembler touches the heap not at all per record -- and any nonzero count is a new site
// that will scale with the scan. It is measured with AllocsPerRun over a warmed buffer, so a
// regression fails here rather than quietly costing.
func TestAppendAdSubsetInlineRefsReusesTheCallersBuffer(t *testing.T) {
	ad := Ad(refsAllocAd(40))
	sc := &SubsetScratch{}
	keep := func(_ []byte, _ uint32, _ []byte) bool { return true }
	note := func([]byte) bool { return false }
	pending := func() int { return 1 } // outstanding, so the entry walk runs
	buf := make([]byte, 0, 4096)

	// Warm the buffer to its working size first; the growth is the CALLER's, once, and is not
	// what this measures.
	out, ok := AppendAdSubsetInlineRefs(buf[:0], ad, keep, note, pending, 40, nil, sc)
	if !ok {
		t.Fatal("AppendAdSubsetInlineRefs refused a well-formed inline ad")
	}
	buf = out

	n := testing.AllocsPerRun(200, func() {
		out, ok := AppendAdSubsetInlineRefs(buf[:0], ad, keep, note, pending, 40, nil, sc)
		if !ok {
			t.Fatal("AppendAdSubsetInlineRefs refused a well-formed inline ad")
		}
		buf = out
	})
	if n != 0 {
		t.Errorf("AppendAdSubsetInlineRefs allocated %.1f times per record, want 0", n)
	}
}

// BenchmarkAppendAdSubsetInlineRefs is the same call under the benchmark harness, so the
// allocation count travels with benchstat output as well as with the test above.
func BenchmarkAppendAdSubsetInlineRefs(b *testing.B) {
	ad := Ad(refsAllocAd(40))
	sc := &SubsetScratch{}
	keep := func(_ []byte, _ uint32, _ []byte) bool { return true }
	note := func([]byte) bool { return false }
	pending := func() int { return 1 }
	buf := make([]byte, 0, 4096)
	b.ReportAllocs()
	for b.Loop() {
		out, ok := AppendAdSubsetInlineRefs(buf[:0], ad, keep, note, pending, 40, nil, sc)
		if !ok {
			b.Fatal("refused a well-formed inline ad")
		}
		buf = out
	}
}
