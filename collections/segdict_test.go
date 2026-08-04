package collections

import (
	"fmt"
	"strings"
	"testing"
)

// buildDictAt serializes names into a buffer at a non-zero base (a filler prefix) so the test
// exercises base-relative offset handling exactly as an embedded-in-segment dict would.
func buildDictAt(names []string, prefix int) ([]byte, uint32) {
	buf := make([]byte, prefix)
	for i := range buf {
		buf[i] = 0xEE
	}
	base := uint32(len(buf))
	return appendSegDict(buf, names), base
}

func TestSegDictRoundTrip(t *testing.T) {
	names := []string{
		"Memory", "Cpus", "Disk", "Name", "Machine", "Requirements", "Rank",
		"StartdIpAddr", "OpSysAndVer", "TotalSlotMemory", "X", "A_very_long_attribute_name_indeed",
	}
	data, base := buildDictAt(names, 37)

	if got := segDictCount(data, base); int(got) != len(names) {
		t.Fatalf("count=%d want %d", got, len(names))
	}
	for id, want := range names {
		// id -> name
		if got := string(segDictName(data, base, uint32(id))); got != want {
			t.Errorf("segDictName(%d)=%q want %q", id, got, want)
		}
		// name -> id, canonical casing
		if got, ok := segDictLookup(data, base, want); !ok || got != uint32(id) {
			t.Errorf("lookup(%q)=%d,%v want %d,true", want, got, ok, id)
		}
		// name -> id, case-insensitive
		for _, cased := range []string{strings.ToUpper(want), strings.ToLower(want)} {
			if got, ok := segDictLookup(data, base, cased); !ok || got != uint32(id) {
				t.Errorf("lookup(%q)=%d,%v want %d,true (case-insensitive)", cased, got, ok, id)
			}
		}
	}
	// absent names
	for _, absent := range []string{"", "Nope", "Memoryy", "emory", "Zzz"} {
		if got, ok := segDictLookup(data, base, absent); ok {
			t.Errorf("lookup(%q)=%d,true but should be absent", absent, got)
		}
	}
	// out-of-range id
	if segDictName(data, base, uint32(len(names))) != nil {
		t.Errorf("segDictName(out-of-range) should be nil")
	}
}

func TestSegDictEmpty(t *testing.T) {
	data, base := buildDictAt(nil, 8)
	if segDictCount(data, base) != 0 {
		t.Fatalf("empty dict count != 0")
	}
	if _, ok := segDictLookup(data, base, "anything"); ok {
		t.Errorf("empty dict lookup should miss")
	}
	if segDictName(data, base, 0) != nil {
		t.Errorf("empty dict name(0) should be nil")
	}
}

// TestSegDictLarge stresses the MPH (with some keys likely unassigned -> fallback) and the
// authoritative binary-search path over a high-cardinality dict, verifying every member
// resolves and a swath of non-members miss.
func TestSegDictLarge(t *testing.T) {
	const n = 4000
	names := make([]string, n)
	seen := map[string]bool{}
	for i := 0; i < n; i++ {
		nm := fmt.Sprintf("Attr_%d_%x", i, i*2654435761&0xffff)
		if seen[strings.ToLower(nm)] {
			t.Fatalf("duplicate folded name in fixture: %q", nm)
		}
		seen[strings.ToLower(nm)] = true
		names[i] = nm
	}
	data, base := buildDictAt(names, 3)

	for id, want := range names {
		got, ok := segDictLookup(data, base, want)
		if !ok || got != uint32(id) {
			t.Fatalf("member lookup(%q)=%d,%v want %d", want, got, ok, id)
		}
		if got != uint32(id) || string(segDictName(data, base, got)) != want {
			t.Fatalf("id<->name mismatch at id %d", id)
		}
	}
	misses := 0
	for i := 0; i < n; i++ {
		if _, ok := segDictLookup(data, base, fmt.Sprintf("Absent_%d", i)); !ok {
			misses++
		}
	}
	if misses != n {
		t.Errorf("expected all %d non-members to miss, got %d misses", n, misses)
	}
}
