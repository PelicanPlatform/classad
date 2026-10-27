package db

import "testing"

// A reference-chasing wire query on an IN-MEMORY table must either REFUSE or SERVE.
// Silently yielding nothing is the one unacceptable answer: a nil error with no rows
// is indistinguishable from "the query matched no ads", so no caller falls back and
// the result is a short queue reported as a complete one.
//
// This is a seam, not a hypothetical. Wire rows are refused for in-memory tables by a
// storage-mode test (SupportsRawWire) and separately by a `!c.inline` guard inside the
// scan. Anything that relaxes one without the other -- teaching the wire path to read
// interned records, say -- turns a clean refusal into an empty success. Pinning the
// invariant rather than either implementation is what makes this survive that change.
func TestRefsOnMemoryTableAreRefusedOrServed(t *testing.T) {
	d, err := Open("") // in-memory -> interned encoding
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.UpdateOld("k1", "MyType = \"Job\"\nClusterId = 1\nRequestMemory = 2048\nRequirements = RequestMemory > 1024"); err != nil {
		t.Fatal(err)
	}
	seq, err := d.QueryRawWireRefs("true", []string{"ClusterId", "Requirements"}, false)
	if err != nil {
		t.Logf("SAFE: refused with %v", err)
		return
	}
	n := 0
	for range seq {
		n++
	}
	if n == 0 {
		t.Fatalf("HAZARD: nil error and ZERO rows -- indistinguishable from an empty table")
	}
	t.Logf("served %d rows", n)
}
