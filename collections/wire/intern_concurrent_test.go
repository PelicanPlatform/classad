package wire

import (
	"fmt"
	"sync"
	"testing"
)

// TestInternByExactCache verifies the byExact fast path: repeated exact names and
// mixed casings all resolve to one id, and the canonical casing is the first seen.
func TestInternByExactCache(t *testing.T) {
	tbl := NewInternTable()
	id := tbl.Intern("RequestMemory")
	// Repeated exact name -> byExact hit, same id.
	if got := tbl.Intern("RequestMemory"); got != id {
		t.Fatalf("repeat exact: got %d want %d", got, id)
	}
	// Different casings -> byFold, same id, canonical casing preserved.
	for _, c := range []string{"requestmemory", "REQUESTMEMORY", "requestMemory"} {
		if got := tbl.Intern(c); got != id {
			t.Fatalf("casing %q: got %d want %d", c, got, id)
		}
	}
	if name, _ := tbl.Name(id); name != "RequestMemory" {
		t.Fatalf("canonical casing = %q, want RequestMemory", name)
	}
	// LookupID hits via byExact and via fold.
	if got, ok := tbl.LookupID("RequestMemory"); !ok || got != id {
		t.Fatalf("LookupID exact: %d,%v", got, ok)
	}
	if got, ok := tbl.LookupID("REQUESTMEMORY"); !ok || got != id {
		t.Fatalf("LookupID fold: %d,%v", got, ok)
	}
	if _, ok := tbl.LookupID("Absent"); ok {
		t.Fatal("LookupID of absent name reported present")
	}
}

// TestInternConcurrent stresses concurrent Intern of overlapping names; with
// -race it guards the byExact/byFold locking, and it asserts every casing of a
// name resolves to the same id afterward.
func TestInternConcurrent(t *testing.T) {
	tbl := NewInternTable()
	const workers, names = 16, 200
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < names; i++ {
				n := fmt.Sprintf("Attr%d", i)
				tbl.Intern(n)                    // exact
				tbl.Intern("attr" + itoaTest(i)) // fold-equal lower ("attrN")
			}
		}()
	}
	wg.Wait()
	if tbl.Len() != names {
		t.Fatalf("interned %d distinct names, want %d", tbl.Len(), names)
	}
	for i := 0; i < names; i++ {
		a := tbl.Intern(fmt.Sprintf("Attr%d", i))
		b := tbl.Intern("attr" + itoaTest(i))
		if a != b {
			t.Fatalf("casings of Attr%d differ: %d vs %d", i, a, b)
		}
	}
}

func itoaTest(i int) string { return fmt.Sprintf("%d", i) }
