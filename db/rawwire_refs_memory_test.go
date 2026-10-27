package db

import (
	"sort"
	"strings"
	"testing"

	"github.com/PelicanPlatform/classad/collections"
)

// attrsOfWireRows renders a wire-row stream to attribute sets, one per ad.
func attrsOfWireRows(t *testing.T, seq func(yield func([]byte) bool)) [][]string {
	t.Helper()
	var out [][]string
	var buf []byte
	var offs []int
	for row := range seq {
		var ok bool
		buf, offs, _, _, ok = collections.RenderRawAdInline(row, buf, offs)
		if !ok {
			t.Fatal("RenderRawAdInline failed on a shipped row")
		}
		names := []string{}
		for i := 0; i+1 < len(offs); i++ {
			name, _, found := strings.Cut(string(buf[offs[i]:offs[i+1]]), " = ")
			if !found {
				t.Fatalf("malformed expr %q", buf[offs[i]:offs[i+1]])
			}
			names = append(names, name)
		}
		sort.Strings(names)
		out = append(out, names)
	}
	return out
}

func memTableWithExpressions(t *testing.T) *DB {
	t.Helper()
	d, err := Open("") // in-memory -> interned encoding
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	if d.Persistent() {
		t.Fatal("Open(\"\") produced a persistent table: this test would not cover the RAM path")
	}
	ads := map[string]string{
		// Requirements is an EXPRESSION over siblings the projection does not name, so a row
		// without the closure is visibly short rather than merely different.
		"a": "MyType = \"Job\"\nClusterId = 1\nRequestMemory = 2048\nRequestDisk = 100\n" +
			"Requirements = (RequestMemory > 1024) && (RequestDisk < 500)",
		// ... and a literal one, so the closure is proven to be per RECORD on this path too.
		"b": "MyType = \"Job\"\nClusterId = 2\nRequestMemory = 512\nRequestDisk = 50\n" +
			"Requirements = true",
	}
	for k, text := range ads {
		if err := d.UpdateOld(k, text); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// TestRefsOnMemoryTableAreRefusedOrServed: a reference-chasing wire query on an IN-MEMORY
// table must either REFUSE or SERVE ROWS THAT CARRY THE CLOSURE. Silently yielding nothing is
// the one unacceptable answer -- a nil error with no rows is indistinguishable from "the query
// matched no ads", so no caller falls back and a short queue is served as a complete one.
//
// This is a seam, not a hypothetical. Wire rows were withheld from in-memory tables by a
// storage-mode test (SupportsRawWire) AND by a separate guard inside the scan; relaxing one
// without the other produced exactly that empty success, which is how this test was written.
// Counting rows is not enough either: serving rows WITHOUT the closure is the other way to
// pass the gate and still be wrong, so the attribute set is asserted, not the row count.
func TestRefsOnMemoryTableAreRefusedOrServed(t *testing.T) {
	d := memTableWithExpressions(t)
	seq, err := d.QueryRawWireRefs("true", []string{"ClusterId", "Requirements"}, false)
	if err != nil {
		t.Logf("SAFE: refused with %v", err)
		return
	}
	got := attrsOfWireRows(t, seq)
	if len(got) == 0 {
		t.Fatal("nil error and ZERO rows: indistinguishable from an empty table, so no caller falls back")
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2: %v", len(got), got)
	}
	sort.Slice(got, func(i, j int) bool { return len(got[i]) < len(got[j]) })
	// The literal Requirements pulls in nothing; the expression pulls in both siblings.
	// MyType/TargetType are absent because both renderers lift them out of the expression
	// list rather than because the projection dropped them.
	wantLiteral := "ClusterId,Requirements"
	wantClosure := "ClusterId,RequestDisk,RequestMemory,Requirements"
	if g := strings.Join(got[0], ","); g != wantLiteral {
		t.Errorf("literal-Requirements row carries %s, want %s", g, wantLiteral)
	}
	if g := strings.Join(got[1], ","); g != wantClosure {
		t.Errorf("expression-Requirements row carries %s, want %s\n"+
			"a row missing RequestMemory/RequestDisk means the closure did not run on the in-memory path", g, wantClosure)
	}
}

// TestRefsOnMemoryTableMatchTextPath holds the in-memory wire rows to the same oracle the
// persistent ones are held to: the text reference-chasing projection, which has always served
// in-memory tables. The two must return the same attributes for the same query.
func TestRefsOnMemoryTableMatchTextPath(t *testing.T) {
	d := memTableWithExpressions(t)
	proj := []string{"ClusterId", "Requirements"}

	seq, err := d.QueryRawWireRefs("true", proj, false)
	if err != nil {
		t.Skipf("wire rows refused for an in-memory table (%v); nothing to compare", err)
	}
	wire := attrsOfWireRows(t, seq)

	textSeq, err := d.QueryRawProjectedRefs("true", proj, false)
	if err != nil {
		t.Fatalf("QueryRawProjectedRefs: %v", err)
	}
	var text [][]string
	for ad := range textSeq {
		names := []string{}
		for _, e := range ad.Exprs {
			if name, _, ok := strings.Cut(string(e), " = "); ok {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		text = append(text, names)
	}
	if len(wire) == 0 || len(text) == 0 {
		t.Fatalf("one path returned nothing: wire %d rows, text %d rows", len(wire), len(text))
	}
	key := func(a [][]string) []string {
		out := []string{}
		for _, n := range a {
			out = append(out, strings.Join(n, ","))
		}
		sort.Strings(out)
		return out
	}
	w, x := key(wire), key(text)
	if len(w) != len(x) {
		t.Fatalf("wire returned %d ads, text %d", len(w), len(x))
	}
	for i := range w {
		if w[i] != x[i] {
			t.Errorf("ad %d:\n wire: %s\n text: %s", i, w[i], x[i])
		}
	}
}
